//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/csigw"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// TestCSIGatewayRegistry: the registry on Postgres — credential hash only, revoke, a name taken once, listings, the task
// TTL and a task result claimed once.
func TestCSIGatewayRegistry(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	st := csigw.NewStore(db)

	tok, secret, err := st.MintCredential(ctx, csigw.Credential{ID: "pc-c-1", ProjectID: "p1", ComputeServiceID: "svc",
		Pool: "pc-p1", Storages: []string{"local-zfs"}, ControllerVMID: 9999})
	if err != nil || tok != "csi@proxcloud!pc-c-1" || len(secret) != 64 {
		t.Fatalf("mint: %q %d %v", tok, len(secret), err)
	}
	var raw pgdoc.M
	if _, err := db.C("csiGatewayCredential").Get(ctx, "pc-c-1", &raw); err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(raw); strings.Contains(string(b), secret) || raw["secretSHA256"] == "" {
		t.Fatalf("stored credential holds the secret or no hash: %s", b)
	}
	if _, _, err := st.MintCredential(ctx, csigw.Credential{ID: "pc-c-2"}); err == nil {
		t.Fatal("a credential without scope was minted")
	}

	// The handler accepts the minted secret and refuses it once revoked.
	h := &csigw.Handler{Reg: st, PVE: nil, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := func(auth string) int {
		r := httptest.NewRequest("GET", "/api2/json/version", nil)
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if code := req("PVEAPIToken=" + tok + "=" + secret); code != 403 {
		t.Fatalf("minted credential: %d, want 403 (authenticated, call not allowed)", code)
	}
	if err := st.RevokeCredential(ctx, "pc-c-1"); err != nil {
		t.Fatal(err)
	}
	if code := req("PVEAPIToken=" + tok + "=" + secret); code != 401 {
		t.Fatalf("revoked credential: %d", code)
	}
	if err := st.RevokeCredential(ctx, "nope"); err != nil {
		t.Fatalf("revoke of no row: %v", err)
	}

	now := time.Now().UTC()
	vol := func(id, cluster, project, state string, size int64) *csigw.Volume {
		return &csigw.Volume{ID: csigw.VolumeID("svc", "local-zfs", id), ClusterID: cluster, ProjectID: project,
			ComputeServiceID: "svc", Storage: "local-zfs", Node: "pve1", VolID: "local-zfs:" + id, SizeBytes: size, State: state, CreatedAt: &now}
	}
	for _, v := range []*csigw.Volume{vol("a", "c1", "p1", csigw.StateReady, 1<<30), vol("b", "c1", "p1", csigw.StateDeleted, 4<<30),
		vol("c", "c2", "p1", csigw.StateCreating, 2<<30), vol("d", "c1", "p1", csigw.StateDeleting, 1<<30)} {
		if err := st.InsertVolume(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.InsertVolume(ctx, vol("a", "c2", "p2", csigw.StateCreating, 1)); err != csigw.ErrExists {
		t.Fatalf("second insert of a name: %v", err)
	}
	rows, err := st.ClusterVolumes(ctx, "c1", "svc", "local-zfs")
	if err != nil || len(rows) != 2 {
		t.Fatalf("cluster volumes: %d %v", len(rows), err)
	}
	if n, err := st.ProjectBytes(ctx, "p1", "svc"); err != nil || n != 4<<30 {
		t.Fatalf("project bytes %d %v, want 4 GiB (deleted not counted)", n, err)
	}
	if err := st.UpdateVolume(ctx, csigw.VolumeID("svc", "local-zfs", "a"), pgdoc.M{"state": csigw.StateDeleted, "deletedAt": now}); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.Volume(ctx, csigw.VolumeID("svc", "local-zfs", "a")); v.State != csigw.StateDeleted || v.DeletedAt == nil {
		t.Fatalf("update: %+v", v)
	}

	if err := st.PutTask(ctx, &csigw.Task{ID: "UPID:old", ClusterID: "c1", ExpiresAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTask(ctx, &csigw.Task{ID: "UPID:new", ClusterID: "c1", Kind: "delete", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if old, _ := st.Task(ctx, "UPID:old"); old != nil {
		t.Fatal("an expired task outlived the next insert")
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := st.FinishTask(ctx, "UPID:new"); err != nil {
				t.Error(err)
			} else if ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("task claimed %d times", wins.Load())
	}
}

// TestCSIGatewayConcurrentCreate: many creates of one disk name from two clusters at once reach PVE once, and the row
// belongs to whoever reached it.
func TestCSIGatewayConcurrentCreate(t *testing.T) {
	ctx := context.Background()
	st := csigw.NewStore(freshPG(t))
	var creates atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch {
		case r.URL.Path == "/api2/json/cluster/resources":
			fmt.Fprint(w, `{"data":[{"type":"storage","storage":"local-zfs","node":"pve1","status":"available","shared":0}]}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/content"):
			creates.Add(1)
			time.Sleep(50 * time.Millisecond)
			fmt.Fprintf(w, `{"data":"local-zfs:%s"}`, r.Form.Get("filename"))
		default:
			w.WriteHeader(501)
		}
	}))
	defer fake.Close()
	client := func(context.Context, string) (*pve.Client, error) {
		return pve.New(pve.Config{APIURL: fake.URL, TokenID: "csi@pve!gateway", Secret: "x"})
	}
	secrets := map[string]string{}
	for _, id := range []string{"c1", "c2"} {
		_, s, err := st.MintCredential(ctx, csigw.Credential{ID: id, ProjectID: "p-" + id, ComputeServiceID: "svc",
			Pool: "pc-" + id, Storages: []string{"local-zfs"}, ControllerVMID: 9999})
		if err != nil {
			t.Fatal(err)
		}
		secrets[id] = s
	}
	h := &csigw.Handler{Reg: st, PVE: client, Portal: client, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	name := "vm-9999-pvc-0b0b0b0b-1111-4111-8111-111111111111"
	target := "/api2/json/nodes/pve1/storage/local-zfs/content?" + url.Values{"filename": {name}, "vmid": {"9999"}, "size": {"1G"}}.Encode()

	var wg sync.WaitGroup
	codes := make(chan [2]string, 16)
	for i := range 16 {
		cluster := []string{"c1", "c2"}[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("POST", target, nil)
			r.Header.Set("Authorization", "PVEAPIToken=csi@proxcloud!"+cluster+"="+secrets[cluster])
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			codes <- [2]string{cluster, fmt.Sprint(w.Code)}
		}()
	}
	wg.Wait()
	close(codes)
	if n := creates.Load(); n != 1 {
		t.Fatalf("PVE asked to create %d times", n)
	}
	v, err := st.Volume(ctx, csigw.VolumeID("svc", "local-zfs", name))
	if err != nil || v == nil || v.State != csigw.StateReady {
		t.Fatalf("row %+v %v", v, err)
	}
	for c := range codes {
		// The winner, and its own retries once the row is ready, answer 200; the other cluster only ever 409.
		if c[0] != v.ClusterID && c[1] != "409" {
			t.Fatalf("cluster %s got %s on a name %s holds", c[0], c[1], v.ClusterID)
		}
		if c[0] == v.ClusterID && c[1] != "200" && c[1] != "409" {
			t.Fatalf("winner got %s", c[1])
		}
	}
}

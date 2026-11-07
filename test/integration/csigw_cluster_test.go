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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/csigw"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// fakeStore is a PVE store holding disks by volid, with a delete that can be made to fail (a disk still in use).
type fakeStore struct {
	mu      sync.Mutex
	disks   map[string]int64
	busy    bool
	deletes []string
}

func (f *fakeStore) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		switch {
		case r.Method == "GET" && strings.HasSuffix(p, "/content"):
			store := strings.Split(p, "/")[6]
			list := []map[string]any{}
			for v, size := range f.disks {
				if strings.HasPrefix(v, store+":") {
					list = append(list, map[string]any{"volid": v, "size": size, "content": "images"})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": list})
		case r.Method == "DELETE" && strings.Contains(p, "/content/"):
			if f.busy {
				w.WriteHeader(500)
				fmt.Fprint(w, `{"data":null,"message":"volume is in use"}`)
				return
			}
			seg := strings.Split(p, "/")
			volid := seg[6] + ":" + strings.Join(seg[8:], "/")
			f.deletes = append(f.deletes, volid)
			delete(f.disks, volid)
			fmt.Fprint(w, `{"data":"UPID:pve1:1:2:3:imgdel::csi@pve!gateway:"}`)
		case strings.HasSuffix(p, "/status") && strings.Contains(p, "/tasks/"):
			fmt.Fprint(w, `{"data":{"status":"stopped","exitstatus":"OK"}}`)
		default:
			w.WriteHeader(501)
		}
	}))
}

func csiVol(cluster, project, name, state string, size int64, at time.Time) *csigw.Volume {
	return &csigw.Volume{ID: csigw.VolumeID("svc", "local-zfs", name), ClusterID: cluster, ProjectID: project,
		ComputeServiceID: "svc", Storage: "local-zfs", Node: "pve1", VolID: "local-zfs:" + name, SizeBytes: size,
		State: state, CreatedAt: &at}
}

// TestCSIGatewayClusterRelease: the controller Secret carries a working credential; the release revokes it at once,
// keeps every row while PVE refuses a delete, then deletes the cluster's disks (and only its own) and closes their rows.
// A provider that is gone closes the rows with no call.
func TestCSIGatewayClusterRelease(t *testing.T) {
	ctx := context.Background()
	st := csigw.NewStore(freshPG(t))
	sec, err := st.ControllerSecret(ctx, csigw.Credential{ID: "c1", ProjectID: "p1", ComputeServiceID: "svc", Region: "r",
		Pool: "pc-p1", Storages: []string{"local-zfs"}}, csigw.Endpoint{URL: "https://gw.example:8443/", CA: "PEM"})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Features map[string]any `json:"features"`
		Clusters []struct {
			URL         string `json:"url"`
			CAFile      string `json:"ca_file"`
			TokenID     string `json:"token_id"`
			TokenSecret string `json:"token_secret"`
			Region      string `json:"region"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal([]byte(sec["config.yaml"]), &cfg); err != nil {
		t.Fatal(err)
	}
	c := cfg.Clusters[0]
	if c.URL != "https://gw.example:8443/api2/json" || c.CAFile != "/etc/proxmox/ca.crt" || c.TokenID != "csi@proxcloud!c1" ||
		c.Region != "r" || sec["ca.crt"] != "PEM" || cfg.Features["provider"] != "capmox" {
		t.Fatalf("controller config %s", sec["config.yaml"])
	}
	h := &csigw.Handler{Reg: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	call := func() int {
		r := httptest.NewRequest("GET", "/api2/json/version", nil)
		r.Header.Set("Authorization", "PVEAPIToken "+c.TokenID+"="+c.TokenSecret)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if code := call(); code != 403 {
		t.Fatalf("minted credential: %d, want 403 (authenticated, call not allowed)", code)
	}

	now := time.Now().UTC()
	names := map[string]string{}
	for _, v := range []*csigw.Volume{
		csiVol("c1", "p1", "vm-9999-pvc-a", csigw.StateReady, 1<<30, now),
		csiVol("c1", "p1", "vm-9999-pvc-b", csigw.StateCreating, 1<<30, now),
		csiVol("c1", "p1", "vm-9999-pvc-c", csigw.StateDeleting, 1<<30, now),
		csiVol("c2", "p2", "vm-9999-pvc-e", csigw.StateReady, 1<<30, now),
	} {
		if err := st.InsertVolume(ctx, v); err != nil {
			t.Fatal(err)
		}
		names[v.VolID] = v.ID
	}
	fs := &fakeStore{disks: map[string]int64{"local-zfs:vm-9999-pvc-a": 1 << 30, "local-zfs:vm-9999-pvc-c": 1 << 30,
		"local-zfs:vm-9999-pvc-e": 1 << 30}, busy: true}
	srv := fs.server()
	defer srv.Close()
	gw := func(context.Context, string) (*pve.Client, error) {
		return pve.New(pve.Config{APIURL: srv.URL, TokenID: "csi@pve!gateway", Secret: "x"})
	}
	state := func(volid string) string {
		v, err := st.Volume(ctx, names[volid])
		if err != nil || v == nil {
			t.Fatalf("row %s: %v", volid, err)
		}
		return v.State
	}

	if err := st.ReleaseCluster(ctx, "c1", gw); err == nil {
		t.Fatal("a disk PVE refused to delete: want an error, so the sweep retries")
	}
	if code := call(); code != 401 {
		t.Fatalf("the credential must be revoked before any disk goes: %d", code)
	}
	if s := state("local-zfs:vm-9999-pvc-a"); s != csigw.StateReady {
		t.Fatalf("refused delete changed the row: %s", s)
	}

	fs.busy = false
	if err := st.ReleaseCluster(ctx, "c1", gw); err != nil {
		t.Fatal(err)
	}
	for _, volid := range []string{"local-zfs:vm-9999-pvc-a", "local-zfs:vm-9999-pvc-b", "local-zfs:vm-9999-pvc-c"} {
		if s := state(volid); s != csigw.StateDeleted {
			t.Errorf("%s: %s after the release", volid, s)
		}
	}
	if s := state("local-zfs:vm-9999-pvc-e"); s != csigw.StateReady {
		t.Errorf("another cluster's row: %s", s)
	}
	if _, ok := fs.disks["local-zfs:vm-9999-pvc-e"]; !ok || len(fs.deletes) != 2 {
		t.Errorf("deletes %v: want only c1's two disks on the store", fs.deletes)
	}

	if err := st.InsertVolume(ctx, csiVol("c3", "p3", "vm-9999-pvc-f", csigw.StateReady, 1<<30, now)); err != nil {
		t.Fatal(err)
	}
	gone := func(context.Context, string) (*pve.Client, error) { return nil, externalservice.ErrNotFound }
	if err := st.ReleaseCluster(ctx, "c3", gone); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.Volume(ctx, csigw.VolumeID("svc", "local-zfs", "vm-9999-pvc-f")); v.State != csigw.StateDeleted {
		t.Fatalf("provider gone: %s", v.State)
	}
}

// TestCSIGatewayVolumeRows: the registry bills as VOLUME rows of the project, settled against PVE, and its reconcile
// and the PVE volume sync never archive each other's rows.
func TestCSIGatewayVolumeRows(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	st := csigw.NewStore(db)
	repo := cloud.NewRepo(db)
	now := time.Now().UTC()
	old := now.Add(-time.Hour)
	for _, v := range []*csigw.Volume{
		csiVol("c1", "p1", "vm-9999-pvc-grown", csigw.StateReady, 1<<30, old),
		csiVol("c1", "p1", "vm-9999-pvc-lost", csigw.StateCreating, 1<<30, old),
		csiVol("c1", "p1", "vm-9999-pvc-new", csigw.StateCreating, 1<<30, now),
		csiVol("c1", "p1", "vm-9999-pvc-gone", csigw.StateReady, 1<<30, old),
		csiVol("c9", "p9", "vm-9999-pvc-other", csigw.StateReady, 1<<30, old),
	} {
		if err := st.InsertVolume(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	fs := &fakeStore{disks: map[string]int64{"local-zfs:vm-9999-pvc-grown": 2 << 30, "local-zfs:vm-9999-pvc-other": 1 << 30}}
	srv := fs.server()
	defer srv.Close()
	gw := func(context.Context, string) (*pve.Client, error) {
		return pve.New(pve.Config{APIURL: srv.URL, TokenID: "csi@pve!gateway", Secret: "x"})
	}
	// A server's disk the PVE volume sync made: never this provider's to archive.
	vmDisk := &cloud.CloudResource{ServiceID: "svc", ProjectID: "p1", Type: cloud.TypeVolume, ExternalID: "local-zfs:vm-101-disk-0",
		Data: map[string]any{"volume": map[string]any{"vmid": 101, "size": 10}}, CreatedAt: &old, UpdatedAt: &old}
	if _, err := repo.Insert(ctx, vmDisk); err != nil {
		t.Fatal(err)
	}
	rows := &csigw.VolumeRows{Reg: st, ServiceID: "svc", Project: "p1", Region: "r", GW: gw,
		ProfileOf: func(node, storage, _ string) (string, string) { return "std", "performance" }}
	if _, err := providers.Reconcile(ctx, rows, repo, "svc", now); err != nil {
		t.Fatal(err)
	}
	cached, err := repo.FindByServiceProjectAndType(ctx, "svc", "p1", cloud.TypeVolume)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]cloud.CloudResource{}
	for _, r := range cached {
		byID[r.ExternalID] = r
	}
	if len(byID) != 2 {
		t.Fatalf("rows %v: want the server disk and the one live cluster disk", byID)
	}
	grown := byID["local-zfs:vm-9999-pvc-grown"]
	vol, _ := grown.Data["volume"].(map[string]any)
	if pve.CSIClusterOf(grown.Data) != "c1" || fmt.Sprint(vol["size"]) != "2" || vol["volume_type"] != "std" ||
		vol["name"] != "pvc-grown" || grown.CreatedAt == nil || !grown.CreatedAt.Equal(old) {
		t.Fatalf("cluster disk row %+v %v", vol, grown.CreatedAt)
	}
	if _, ok := byID["local-zfs:vm-101-disk-0"]; !ok {
		t.Fatal("the server's disk row was archived")
	}
	reg := func(name string) *csigw.Volume {
		v, err := st.Volume(ctx, csigw.VolumeID("svc", "local-zfs", name))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := reg("vm-9999-pvc-grown"); v.SizeBytes != 2<<30 {
		t.Errorf("size not taken from PVE: %d", v.SizeBytes)
	}
	if v := reg("vm-9999-pvc-lost"); v != nil {
		t.Errorf("a creating row past the grace with no disk stayed: %+v", v)
	}
	if v := reg("vm-9999-pvc-new"); v == nil || v.State != csigw.StateCreating {
		t.Errorf("a fresh creating row was settled: %+v", v)
	}
	if v := reg("vm-9999-pvc-gone"); v.State != csigw.StateDeleted {
		t.Errorf("a ready row whose disk is gone: %s", v.State)
	}
	if v := reg("vm-9999-pvc-other"); v.State != csigw.StateReady {
		t.Errorf("another project's row: %s", v.State)
	}

	// The disk goes: its row closes and its VOLUME row is archived (billing stops), the server's disk stays.
	if _, err := db.C("csiVolume").SetByID(ctx, csigw.VolumeID("svc", "local-zfs", "vm-9999-pvc-grown"),
		pgdoc.M{"state": csigw.StateDeleted}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := providers.Reconcile(ctx, rows, repo, "svc", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	cached, _ = repo.FindByServiceProjectAndType(ctx, "svc", "p1", cloud.TypeVolume)
	if len(cached) != 1 || cached[0].ExternalID != "local-zfs:vm-101-disk-0" {
		t.Fatalf("after the delete: %v", cached)
	}
}

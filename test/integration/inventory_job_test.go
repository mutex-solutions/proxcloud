//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/cloud/syncjob"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

func TestUntrustedNetworkSyncSkipsDependentMutations(t *testing.T) {
	ctx := context.Background()
	f := newShareAccessFixture(t)
	mutations, seedCalls, shareCalls := 0, 0, 0
	lost := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			mutations++
			w.WriteHeader(500)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")
		write := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
		switch path {
		case "/nodes":
			write([]any{map[string]any{"node": "n1", "status": "online"}})
		case "/pools/pc-" + f.projectID:
			write(map[string]any{"poolid": "pc-" + f.projectID, "members": []any{}})
		case "/cluster/resources", "/storage", "/cluster/firewall/groups":
			write([]any{})
		case "/access/acl":
			write([]any{})
		case "/access/permissions":
			if r.URL.Query().Get("path") == "/access" {
				write(map[string]any{"/access": map[string]int{"Sys.Audit": 0}})
				return
			}
			privs := map[string]int{}
			if !lost {
				privs["SDN.Audit"] = 1
			}
			write(map[string]any{r.URL.Query().Get("path"): privs})
		case "/cluster/sdn/vnets":
			write([]any{})
		case "/cluster/sdn/vnets/v1":
			w.WriteHeader(404)
		case "/cluster/firewall/rules":
			write([]any{})
		default:
			fmt.Fprintf(w, `{"data":[]}`)
		}
	}))
	t.Cleanup(srv.Close)
	if _, err := f.db.C("externalService").SetByID(ctx, "svc-pve", pgdoc.M{"config": pgdoc.M{
		"provider": "proxmox", "apiUrl": srv.URL, "region": "r", "poolPrefix": "pc-", "network": pgdoc.M{"cidrPool": "10.100.0.0/16"},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	row, err := cloud.NewRepo(f.db).FindByServiceIDAndExternalID(ctx, "svc-pve", "v1")
	if err != nil {
		t.Fatal(err)
	}
	row.Data["network"].(map[string]any)["zone"] = "overlay"
	if _, err := cloud.NewRepo(f.db).Insert(ctx, row); err != nil {
		t.Fatal(err)
	}
	es := externalservice.NewService(externalservice.NewRepo(f.db), textcrypt.New("k"))
	c, err := pve.New(pve.Config{APIURL: srv.URL, TokenID: "p@pve!t", Secret: "x"})
	if err != nil {
		t.Fatal(err)
	}
	j := syncjob.New(project.NewRepo(f.db), es, cloud.NewRepo(f.db), nil).
		WithProxmoxFactory(func(es *externalservice.ExternalService) (*pve.Service, error) {
			return &pve.Service{C: c, PoolPrefix: "pc-"}, nil
		}).
		WithSDNLock(lock.New(f.db)).
		WithShareAccess(func(ctx context.Context, pid string) error {
			shareCalls++
			return f.handler.ReconcileShareAccess(ctx, pid)
		}).
		WithStaleSeeds(func(context.Context, *pve.Service, *externalservice.ExternalService) error { seedCalls++; return nil })
	// Both the scheduled walk and explicit single-project sync share the safety
	// boundary; there is no sticky failure map when a later invocation recovers.
	for round := 0; round < 2; round++ {
		if _, err := j.Run(ctx); err != nil {
			t.Fatal(err)
		}
		if err := j.SyncOne(ctx, f.projectID, ""); err != nil {
			t.Fatal(err)
		}
	}
	if mutations != 0 || seedCalls != 0 || shareCalls != 0 || f.nas.updates != 0 {
		t.Fatalf("unknown inventory caused side effects: HTTP=%d seed=%d share=%d NAS=%d", mutations, seedCalls, shareCalls, f.nas.updates)
	}
	if got, _ := cloud.NewRepo(f.db).FindByServiceIDAndExternalID(ctx, "svc-pve", "v1"); got == nil {
		t.Fatal("network lost")
	}
	lost = false
	// Recovery allows a genuine by-id404 and cleanup in a new invocation.
	if _, err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if seedCalls != 1 || shareCalls != 1 {
		t.Fatalf("failure state persisted: seed=%d share=%d", seedCalls, shareCalls)
	}
}

// A provider with no project still gets its isolation DROPs from the sync (lab M0: on a Proxmox cluster another portal
// shares, the managed-services preflight waits for them and nothing else writes them before the first tenant network).
// Without the SDN lock, and for a cidrPool over a management network, the sync writes nothing.
func TestSyncWritesProviderIsolationWithoutProjects(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	var comments []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")
		switch {
		case r.Method == "POST" && path == "/cluster/firewall/rules":
			_ = r.ParseForm()
			comments = append(comments, r.PostFormValue("action")+" "+r.PostFormValue("source")+" "+r.PostFormValue("comment"))
			fmt.Fprint(w, `{"data":null}`)
		case r.Method != "GET":
			t.Errorf("unexpected write %s %s", r.Method, path)
			w.WriteHeader(500)
		case path == "/cluster/status":
			fmt.Fprint(w, `{"data":[{"type":"node","name":"n1","ip":"192.0.2.11"}]}`)
		case path == "/nodes/n1/network":
			fmt.Fprint(w, `{"data":[{"iface":"vmbr0","address":"192.0.2.11","gateway":"192.0.2.1","cidr":"192.0.2.11/24"}]}`)
		default:
			fmt.Fprint(w, `{"data":[]}`)
		}
	}))
	t.Cleanup(srv.Close)
	provider := func(pool string) {
		if _, err := db.C("externalService").SetByID(ctx, "svc-pve", pgdoc.M{"type": externalservice.TypeCloud, "config": pgdoc.M{
			"provider": "proxmox", "apiUrl": srv.URL, "region": "r", "poolPrefix": "pcm-",
			"network": pgdoc.M{"cidrPool": pool, "blockedCidrs": []any{"10.90.0.0/26"}},
		}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-pve", "type": externalservice.TypeCloud, "config": pgdoc.M{"provider": "proxmox"}})
	provider("10.121.0.0/16")
	c, err := pve.New(pve.Config{APIURL: srv.URL, TokenID: "p@pve!t", Secret: "x"})
	if err != nil {
		t.Fatal(err)
	}
	es := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("k"))
	j := syncjob.New(project.NewRepo(db), es, cloud.NewRepo(db), nil).
		WithProxmoxFactory(func(*externalservice.ExternalService) (*pve.Service, error) {
			return &pve.Service{C: c, PoolPrefix: "pcm-"}, nil
		})
	if _, err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 0 {
		t.Fatalf("no SDN lock, yet the sync wrote rules: %v", comments)
	}
	j.WithSDNLock(lock.New(db))
	provider("192.0.0.0/16")
	if _, err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 0 {
		t.Fatalf("cidrPool over the management network, yet the sync wrote rules: %v", comments)
	}
	provider("10.121.0.0/16")
	if _, err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 4 {
		t.Fatalf("want the provider's 4 DROPs, got %v", comments)
	}
	for _, c := range comments {
		if c != "DROP 10.121.0.0/16 proxcloud:isolation/pcm-" {
			t.Errorf("unexpected rule %q", c)
		}
	}
}

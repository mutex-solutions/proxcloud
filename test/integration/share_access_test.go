//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// exportNAS is a fake TrueNAS holding one NFS share: enough of the JSON-RPC surface for the export reconcile.
type exportNAS struct {
	mu       sync.Mutex
	networks []string
	failing  bool
	updates  int
	onUpdate func() // runs at the moment the export changes
}

func (n *exportNAS) current() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.networks
}

func (n *exportNAS) serve(t *testing.T, projectID string) string {
	t.Helper()
	dataset := "tank/shares/" + projectID + "-data"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			var req struct {
				ID     int               `json:"id"`
				Method string            `json:"method"`
				Params []json.RawMessage `json:"params"`
			}
			if err := wsjson.Read(r.Context(), conn, &req); err != nil {
				return
			}
			var result any = true
			resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
			n.mu.Lock()
			switch req.Method {
			case "auth.login_ex":
				result = map[string]any{"response_type": "SUCCESS"}
			case "pool.dataset.query":
				result = []any{map[string]any{"id": dataset, "user_properties": map[string]any{
					"proxcloud:project": map[string]any{"rawvalue": projectID}, "proxcloud:protocol": map[string]any{"rawvalue": "NFS"},
				}}}
			case "sharing.nfs.query":
				result = []any{map[string]any{"id": 11, "path": "/mnt/" + dataset, "networks": n.networks}}
			case "sharing.smb.query":
				result = []any{}
			case "sharing.nfs.update":
				if n.failing {
					resp["error"] = map[string]any{"code": 1, "message": "NAS says no"}
					result = nil
					break
				}
				var body struct {
					Networks []string `json:"networks"`
				}
				_ = json.Unmarshal(req.Params[1], &body)
				n.networks = body.Networks
				n.updates++
				if n.onUpdate != nil {
					n.onUpdate()
				}
			}
			n.mu.Unlock()
			if result != nil {
				resp["result"] = result
			}
			if err := wsjson.Write(r.Context(), conn, resp); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

type shareAccessFixture struct {
	db        *pgdoc.DB
	handler   *project.Handler
	ips       *project.PublicIPRepo
	nas       *exportNAS
	projectID string
}

func newShareAccessFixture(t *testing.T) *shareAccessFixture {
	t.Helper()
	ctx := context.Background()
	db := freshPG(t)
	repo := project.NewRepo(db)
	p, err := repo.Insert(ctx, &project.Project{Name: "a", Status: "ENABLED", OrganizationID: "o", Services: []any{
		map[string]any{"serviceId": "svc-pve"}, map[string]any{"serviceId": "svc-nas"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	nas := &exportNAS{networks: []string{"10.100.0.0/24", "10.89.0.10/32"}}
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-pve", "type": externalservice.TypeCloud,
		"config": pgdoc.M{"provider": "proxmox"}})
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-nas", "type": externalservice.TypeCloud,
		"config": pgdoc.M{"provider": "truenas", "apiUrl": nas.serve(t, p.ID), "username": "u", "insecureTls": true, "parentDataset": "tank/shares"},
		"secret": pgdoc.M{"apiKey": "k"}})
	mustInsert(t, db, "pvePublicIp", pgdoc.M{"_id": "svc-pve/vmbr1/10.89.0.10", "serviceId": "svc-pve", "networkId": "vmbr1",
		"ip": "10.89.0.10", "prefix": 24, "projectId": p.ID, "serverId": "100", "nic": "net1"})
	cloudRepo := cloud.NewRepo(db)
	if _, err := cloudRepo.Insert(ctx, &cloud.CloudResource{ServiceID: "svc-pve", ProjectID: p.ID, Type: cloud.TypeNetwork, ExternalID: "v1",
		Data: map[string]any{"network": map[string]any{"cidr": "10.100.0.0/24", "subnets": []any{"10.100.0.0/24", "10.100.1.0/24"}}}}); err != nil {
		t.Fatal(err)
	}
	svc := project.NewService(repo, nil, nil, nil, nil)
	es := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("k"))
	h := project.NewHandler(svc, nil, nil, nil, nil, nil, cloudRepo, es, nil, nil, "")
	ips := project.NewPublicIPRepo(db)
	h.SetPublicIPs(ips)
	return &shareAccessFixture{db: db, handler: h, ips: ips, nas: nas, projectID: p.ID}
}

func (f *shareAccessFixture) held(t *testing.T) int {
	t.Helper()
	n, err := f.ips.CountForProject(context.Background(), "svc-pve", f.projectID)
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

// A public address released while a share's export lists it: it comes off the export before the row goes, because the
// row is what keeps it from being reallocated to another tenant.
func TestReleasedAddressLeavesExportsFirst(t *testing.T) {
	ctx := context.Background()
	f := newShareAccessFixture(t)
	heldAtUpdate := -1
	f.nas.onUpdate = func() { heldAtUpdate = f.held(t) }

	if err := f.ips.ReleaseNIC(ctx, "svc-pve", "100", "net1"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.nas.current(), ","); strings.Contains(got, "10.89.0.10") {
		t.Fatalf("export still lists the released address: %s", got)
	}
	if heldAtUpdate != 1 {
		t.Fatalf("address was held by %d rows when the export changed, want 1", heldAtUpdate)
	}
	if n := f.held(t); n != 0 {
		t.Fatalf("%d rows left", n)
	}
}

// A NAS that refuses: the address stays allocated, so no other tenant can get it while the export lists it.
func TestReleaseFailsClosedWhenTheExportCannotBeUpdated(t *testing.T) {
	ctx := context.Background()
	f := newShareAccessFixture(t)
	f.nas.failing = true
	if err := f.ips.ReleaseServer(ctx, "svc-pve", "100"); err == nil {
		t.Fatal("release succeeded with a stale export")
	}
	if n := f.held(t); n != 1 {
		t.Fatalf("address was freed: %d rows", n)
	}
	f.nas.failing = false
	if err := f.ips.ReleaseServer(ctx, "svc-pve", "100"); err != nil {
		t.Fatal(err)
	}
	if n := f.held(t); n != 0 {
		t.Fatalf("retry left %d rows", n)
	}
}

// The sync's backstop rewrites a stale export, every subnet of a network included, and leaves a correct one alone.
func TestReconcileShareAccessRewritesStaleExports(t *testing.T) {
	ctx := context.Background()
	f := newShareAccessFixture(t)
	f.nas.networks = []string{"10.100.0.0/24", "10.89.0.10/32", "10.200.7.7/32"}
	if err := f.handler.ReconcileShareAccess(ctx, f.projectID); err != nil {
		t.Fatal(err)
	}
	want := "10.100.0.0/24,10.100.1.0/24,10.89.0.10/32"
	if got := strings.Join(f.nas.current(), ","); got != want {
		t.Fatalf("export = %s, want %s", got, want)
	}
	if err := f.handler.ReconcileShareAccess(ctx, f.projectID); err != nil {
		t.Fatal(err)
	}
	if f.nas.updates != 1 {
		t.Fatalf("%d writes, want 1: an unchanged export must not be rewritten", f.nas.updates)
	}
}

// A provider's NAT address stays on the export through the sync and through a release, and leaves with the setting. A
// provider without the setting holds only the project's addresses, as the other tests here show.
func TestSNATSourceFollowsTheProviderSetting(t *testing.T) {
	ctx := context.Background()
	f := newShareAccessFixture(t)
	setSNAT := func(v any) {
		t.Helper()
		if _, err := f.db.Pool.Exec(ctx, `UPDATE "externalService" SET doc = jsonb_set(doc, '{config,snatSources}', $1::jsonb) WHERE id = 'svc-nas'`, v); err != nil {
			t.Fatal(err)
		}
	}
	const own = "10.100.0.0/24,10.100.1.0/24,10.89.0.10/32"

	setSNAT(`["192.0.2.1"]`)
	f.nas.networks = []string{"10.100.0.0/24", "10.89.0.10/32"}
	if err := f.handler.ReconcileShareAccess(ctx, f.projectID); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.nas.current(), ","); got != own+",192.0.2.1/32" {
		t.Fatalf("with the setting: %s", got)
	}
	writes := f.nas.updates
	if err := f.handler.ReconcileShareAccess(ctx, f.projectID); err != nil {
		t.Fatal(err)
	}
	if f.nas.updates != writes {
		t.Fatal("a current export was rewritten")
	}

	if err := f.ips.ReleaseServer(ctx, "svc-pve", "100"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.nas.current(), ","); got != "10.100.0.0/24,10.100.1.0/24,192.0.2.1/32" {
		t.Fatalf("after a release: %s", got)
	}

	setSNAT(`[]`)
	if err := f.handler.ReconcileShareAccess(ctx, f.projectID); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.nas.current(), ","); got != "10.100.0.0/24,10.100.1.0/24" {
		t.Fatalf("setting removed: %s", got)
	}
}

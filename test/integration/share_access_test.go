//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
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
	maproot  string
	setperms int
	failing  bool
	updates  int
	onUpdate func() // runs at the moment the export changes
	name     string // the share's name; empty = "data"
	deleted  []string
}

func (n *exportNAS) current() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.networks
}

func (n *exportNAS) serve(t *testing.T, projectID string) string {
	t.Helper()
	if n.name == "" {
		n.name = "data"
	}
	dataset := "tank/shares/" + projectID + "-" + n.name
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
				row := map[string]any{"id": 11, "path": "/mnt/" + dataset, "networks": n.networks, "maproot_user": nil, "maproot_group": nil}
				if n.maproot != "" {
					row["maproot_user"] = n.maproot
				}
				result = []any{row}
			case "filesystem.setperm":
				n.setperms++
				result = 7
			case "core.get_jobs":
				result = []any{map[string]any{"state": "SUCCESS"}}
			case "sharing.smb.query":
				result = []any{}
			case "pool.dataset.delete":
				var id string
				_ = json.Unmarshal(req.Params[0], &id)
				n.deleted = append(n.deleted, id)
			case "sharing.nfs.update":
				if n.failing {
					resp["error"] = map[string]any{"code": 1, "message": "NAS says no"}
					result = nil
					break
				}
				var body struct {
					Networks    []string `json:"networks"`
					MaprootUser *string  `json:"maproot_user"`
				}
				_ = json.Unmarshal(req.Params[1], &body)
				n.networks = body.Networks
				if body.MaprootUser == nil {
					n.maproot = ""
				}
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
		map[string]any{"serviceId": "svc-pve"}, map[string]any{"serviceId": "svc-nas"}, map[string]any{"serviceId": "svc-k8s"},
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
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-k8s", "type": externalservice.TypeCloud,
		"config": pgdoc.M{"provider": "kamaji", "cluster": pgdoc.M{"sharedWorkers": pgdoc.M{"bridge": "vmbrw"}}}})
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

// An export from before root squash, with the right addresses: the sync still brings it in line, once.
func TestReconcileShareAccessSquashesAnOldExport(t *testing.T) {
	ctx := context.Background()
	f := newShareAccessFixture(t)
	f.nas.networks = []string{"10.100.0.0/24", "10.100.1.0/24", "10.89.0.10/32"}
	f.nas.maproot = "root"
	if err := f.handler.ReconcileShareAccess(ctx, f.projectID); err != nil {
		t.Fatal(err)
	}
	if f.nas.maproot != "" || f.nas.updates != 1 || f.nas.setperms != 1 {
		t.Fatalf("maproot=%q updates=%d setperms=%d", f.nas.maproot, f.nas.updates, f.nas.setperms)
	}
	if err := f.handler.ReconcileShareAccess(ctx, f.projectID); err != nil {
		t.Fatal(err)
	}
	if f.nas.updates != 1 || f.nas.setperms != 1 {
		t.Fatalf("a second pass changed it again: updates=%d setperms=%d", f.nas.updates, f.nas.setperms)
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

func (f *shareAccessFixture) reserve(t *testing.T, network, ip, cluster string) {
	t.Helper()
	mustInsert(t, f.db, "pvePublicIp", pgdoc.M{"_id": "svc-pve/" + network + "/" + ip, "serviceId": "svc-pve", "networkId": network,
		"ip": ip, "prefix": 24, "projectId": f.projectID, "serverId": "k8s:" + cluster, "nic": project.ReservedNIC})
}

func (f *shareAccessFixture) clusterRows(t *testing.T, network string) int {
	t.Helper()
	rows, err := f.ips.InNetwork(context.Background(), "svc-pve", network)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

// A cluster on a project network is covered by the subnet, so its addresses never reach an export on their own: a row
// left on a deleted vnet would keep a freed CIDR's addresses open to the tenant it goes to next.
func TestProjectNetworkClusterAddressesStayOffExports(t *testing.T) {
	ctx := context.Background()
	f := newShareAccessFixture(t)
	f.reserve(t, "v1", "10.100.0.250", "pc-c-live")
	f.reserve(t, "v-deleted", "10.100.9.250", "pc-c-stale")
	f.nas.networks = []string{"10.100.0.0/24", "10.100.9.250/32"} // written before this rule
	if err := f.handler.ReconcileShareAccess(ctx, f.projectID); err != nil {
		t.Fatal(err)
	}
	want := "10.100.0.0/24,10.100.1.0/24,10.89.0.10/32"
	if got := strings.Join(f.nas.current(), ","); got != want {
		t.Fatalf("export = %s, want %s", got, want)
	}
}

// A shared worker pool sits on no project network, so its addresses are listed, and go through the same guard as a
// public address: off the exports first, and held if the NAS refuses.
func TestSharedPoolClusterAddressLeavesExportsFirst(t *testing.T) {
	ctx := context.Background()
	f := newShareAccessFixture(t)
	f.reserve(t, "vmbrw", "10.89.0.99", "pc-c-a")
	if err := f.handler.ReconcileShareAccess(ctx, f.projectID); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.nas.current(), ","); !strings.Contains(got, "10.89.0.99/32") {
		t.Fatalf("a shared worker is off the export: %s", got)
	}

	f.nas.failing = true
	if err := f.ips.ReleaseServer(ctx, "svc-pve", "k8s:pc-c-a"); err == nil {
		t.Fatal("release succeeded with a stale export")
	}
	if n := f.clusterRows(t, "vmbrw"); n != 1 {
		t.Fatalf("address was freed while exported: %d rows", n)
	}

	f.nas.failing = false
	heldAtUpdate := -1
	f.nas.onUpdate = func() { heldAtUpdate = f.clusterRows(t, "vmbrw") }
	if err := f.ips.ReleaseServer(ctx, "svc-pve", "k8s:pc-c-a"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.nas.current(), ","); strings.Contains(got, "10.89.0.99") {
		t.Fatalf("export still lists the released address: %s", got)
	}
	if heldAtUpdate != 1 || f.clusterRows(t, "vmbrw") != 0 {
		t.Fatalf("held at update = %d, rows left = %d", heldAtUpdate, f.clusterRows(t, "vmbrw"))
	}
}

type failingServices struct{ err error }

func (s failingServices) Get(context.Context, string) (*externalservice.ExternalService, error) {
	return nil, s.err
}

// A failed read of the compute provider says nothing about it: the sweep must keep the marker and retry. A provider
// that is really gone has no tokens left to revoke, and everything else is still released.
func TestClusterReleaseKeepsItsRecordUntilDone(t *testing.T) {
	ctx := context.Background()
	f := newShareAccessFixture(t)
	f.reserve(t, "vmbrw", "10.89.0.99", "pc-c-a")
	cloudRepo := cloud.NewRepo(f.db)
	if err := project.KamajiReleaseCluster(failingServices{errors.New("db down")}, cloudRepo, nil, f.ips, false)(ctx, "svc-pve", f.projectID, "pc-c-a"); err == nil {
		t.Fatal("release reported success after a failed provider read")
	}
	if n := f.clusterRows(t, "vmbrw"); n != 1 {
		t.Fatalf("%d rows after a failed read", n)
	}

	es := externalservice.NewService(externalservice.NewRepo(f.db), textcrypt.New("k"))
	if err := project.KamajiReleaseCluster(es, cloudRepo, nil, f.ips, false)(ctx, "no-such-provider", f.projectID, "pc-c-a"); err != nil {
		t.Fatal(err)
	}
}

// The dataset goes from the NAS its share row names. A broken provider holding nothing of this cluster is not asked.
func TestClusterReleaseOnlyTouchesTheNASHoldingItsDataset(t *testing.T) {
	ctx := context.Background()
	f := newShareAccessFixture(t)
	mustInsert(t, f.db, "externalService", pgdoc.M{"_id": "svc-dead", "type": externalservice.TypeCloud,
		"config": pgdoc.M{"provider": "truenas", "apiUrl": "https://127.0.0.1:1", "username": "u", "insecureTls": true, "parentDataset": "tank/shares"},
		"secret": pgdoc.M{"apiKey": "k"}})
	es := externalservice.NewService(externalservice.NewRepo(f.db), textcrypt.New("k"))
	cloudRepo := cloud.NewRepo(f.db)
	release := project.KamajiReleaseCluster(es, cloudRepo, nil, f.ips, true)

	// No share row: nothing on any NAS to reach.
	f.reserve(t, "vmbrw", "10.89.0.99", "pc-c-a")
	if err := release(ctx, "svc-pve", f.projectID, "pc-c-a"); err != nil {
		t.Fatal(err)
	}
	if n := f.clusterRows(t, "vmbrw"); n != 0 {
		t.Fatalf("%d rows left", n)
	}

	// A row on the fake NAS: its dataset goes, even though the dead provider is still registered.
	nas := &exportNAS{name: "k8s-pc-c-b"}
	url := nas.serve(t, f.projectID)
	mustInsert(t, f.db, "externalService", pgdoc.M{"_id": "svc-nas2", "type": externalservice.TypeCloud,
		"config": pgdoc.M{"provider": "truenas", "apiUrl": url, "username": "u", "insecureTls": true, "parentDataset": "tank/shares"},
		"secret": pgdoc.M{"apiKey": "k"}})
	dataset := "tank/shares/" + f.projectID + "-k8s-pc-c-b"
	if _, err := cloudRepo.Insert(ctx, &cloud.CloudResource{ServiceID: "svc-nas2", ProjectID: f.projectID, Type: cloud.TypeShare, ExternalID: dataset}); err != nil {
		t.Fatal(err)
	}
	if err := release(ctx, "svc-pve", f.projectID, "pc-c-b"); err != nil {
		t.Fatal(err)
	}
	if len(nas.deleted) != 1 || nas.deleted[0] != dataset {
		t.Fatalf("deleted = %v, want %s", nas.deleted, dataset)
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

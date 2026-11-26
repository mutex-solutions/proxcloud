//go:build integration

package project

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
)

// The public address of a tenant's own balancer has its FLOATING_IP row from the moment it is allocated, and loses
// it with the release, with no provider sync in between (lab drill of v0.11.1: a pair's row came only with the sync,
// 44 minutes after its allocation, and a pair made and deleted between two syncs never had one). The row is the one
// the sync makes of the allocation (FloatingIPRow), written only where there is none.
//
// Proxmox is a fake that lists no VM and no template, so a create gets as far as its address and no further; the
// pass runs against the pair of cloud_lb_wire_test.go. No real build of an appliance runs here.
func TestLBTenantAddressRows(t *testing.T) {
	ctx := context.Background()
	pgc, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("lb"), tcpostgres.WithUsername("u"), tcpostgres.WithPassword("p"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = testcontainers.TerminateContainer(pgc) }()
	dsn, _ := pgc.ConnectionString(ctx, "sslmode=disable")
	db, err := pgdoc.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)

	// A Proxmox with nothing in it, which nothing may write to.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("wrote to Proxmox: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer empty.Close()
	wire := newPairWire(t, "")
	pair := httptest.NewServer(wire)
	defer pair.Close()
	service := func(url string) *pve.Service {
		ps, err := pve.NewService(pve.Config{APIURL: url, TokenID: "t", Secret: "s"}, "svc", "pc-")
		if err != nil {
			t.Fatal(err)
		}
		return ps
	}
	ps := service(empty.URL)
	flavors := pveflavor.New(db)
	if err := flavors.Save(ctx, &pveflavor.Flavor{Slug: "lb-small", Name: "lb-small", VCPUs: 1, RAMMb: 1024, DiskGb: 10, Active: true}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cloud: cloud.NewRepo(db), sdnLock: lock.New(db), publicIPs: NewPublicIPRepo(db), pveFlavors: flavors,
		pveFor: func(*externalservice.ExternalService) (*pve.Service, error) { return ps, nil }}
	es := &externalservice.ExternalService{ID: "svc", Config: map[string]any{"network": map[string]any{"publicNetworks": []any{
		map[string]any{"id": "vmbr1", "cidr": "203.0.113.0/24", "gateway": "203.0.113.1", "ranges": []any{"203.0.113.10-203.0.113.20"}}}}}}
	proj := &Project{ID: "p1"}
	pn := pveAllowedPublicNetwork(es, proj, "vmbr1")
	if pn == nil {
		t.Fatal("no public network")
	}
	start := time.Now().UTC().Add(-time.Second)

	live := func() []cloud.CloudResource {
		t.Helper()
		rows, err := h.cloud.FindByServiceProjectAndType(ctx, "svc", "p1", cloud.TypeFloatingIP)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	archived := func() (out []cloud.History) {
		t.Helper()
		all, err := h.cloud.FindArchivedSince(ctx, "p1", "svc", start)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range all {
			if r.Type == cloud.TypeFloatingIP {
				out = append(out, r)
			}
		}
		return out
	}
	// same: row is the row the provider sync makes of allocation a, billed from the allocation's time.
	same := func(row cloud.CloudResource, a PublicIP) bool {
		want := FloatingIPRow(a, es.PVERegion())
		return row.ExternalID == want.ExternalID && row.CreatedAt != nil && row.CreatedAt.Equal(*a.CreatedAt) && reflect.DeepEqual(row.Data, want.Data)
	}
	held := func(owner string) []PublicIP {
		t.Helper()
		out, err := h.publicIPs.ForServer(ctx, "svc", owner)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// 1. A pair's create: the address is allocated, the build of its first appliance fails (the site has no image for
	// it), and the create gives everything back. The address was the project's for that time: its row was written
	// with the allocation, and is archived with the release, from the allocation's time to the release's. No pass and
	// no sync ran.
	w := httptest.NewRecorder()
	h.pveCreateLBPair(w, httptest.NewRequest(http.MethodPost, "/", nil), &user.User{}, proj, es,
		map[string]any{"name": "web", "flavorId": "lb-small", "networkId": "net-1"}, pn, "v1")
	if w.Code < 400 {
		t.Fatalf("the create without an image answered %d %s", w.Code, w.Body)
	}
	gone := archived()
	if len(live()) != 0 || len(gone) != 1 {
		t.Fatalf("after a create that took an address and gave it back: %d live rows, %d archived, want 0 and 1\n%s", len(live()), len(gone), w.Body)
	}
	if fip, _ := gone[0].Data["floatingIp"].(map[string]any); !strings.HasPrefix(strAny(fip["server_id"]), lbOwnerPrefix) ||
		gone[0].CreatedAt == nil || gone[0].DeletedAt == nil || gone[0].CreatedAt.Before(start) || gone[0].DeletedAt.Before(*gone[0].CreatedAt) {
		t.Fatalf("the archived row is not the pair's address from its allocation to its release: %+v", gone[0])
	}
	if n, _ := h.publicIPs.CountForProject(ctx, "svc", "p1"); n != 0 {
		t.Fatalf("%d addresses still held", n)
	}
	if lbs, _ := h.cloud.FindByServiceProjectAndType(ctx, "svc", "p1", cloud.TypeLoadBalancer); len(lbs) != 0 {
		t.Fatalf("the failed create left its balancer row: %+v", lbs)
	}

	// 2. A single VM's balancer: lbPlugPublic takes the address before the VM's first boot and writes its row then.
	// It stops after that here (this Proxmox has no VM 401), which is what a build that fails there does; the
	// create then gives the address back (lbGiveBack) and the row goes with it.
	single := &loadBalancer{ID: "s1", Name: "one", PublicNetworkID: "vmbr1"}
	if err := h.lbPlugPublic(ctx, ps, es, proj, pn, single, "n1", 401); err == nil {
		t.Fatal("a public NIC was plugged into a VM that does not exist")
	}
	if a, rows := held("401"), live(); len(a) != 1 || len(rows) != 1 || !same(rows[0], a[0]) {
		t.Fatalf("the single VM's address %+v has rows %+v, want the one the sync makes of it", a, rows)
	}
	if err := h.lbGiveBack(ctx, es, proj, "401"); err != nil {
		t.Fatal(err)
	}
	if len(live()) != 0 || len(archived()) != 2 || len(held("401")) != 0 {
		t.Fatalf("after the give-back: %d live rows, %d archived, %d addresses", len(live()), len(archived()), len(held("401")))
	}

	// 3. A pair that an earlier release made: it serves, holds its address, and has no row until a provider sync. Its
	// next pass writes the row, billed from the allocation and not from the pass, and a second pass writes nothing.
	ps = service(pair.URL)
	owner := LBAddressOwner("abc")
	vip, err := h.publicIPs.Allocate(ctx, "svc", "p1", owner, publicNIC, *pn)
	if err != nil {
		t.Fatal(err)
	}
	lb := &loadBalancer{ID: "abc", Name: "web", Status: lbStatusBuild, NetworkID: "v1", PublicNetworkID: "vmbr1", HA: true,
		ServerID: "301", PrivateIP: "10.0.0.31", PrivatePrefix: 24, PublicIP: vip.IP, VIPs: []string{vip.IP},
		Appliances: []lbAppliance{{ServerID: "301", PrivateIP: "10.0.0.31"}, {ServerID: "302", PrivateIP: "10.0.0.32", HoldsVIP: true}},
		LBSpec:     pve.LBSpec{Listeners: []pve.LBListener{{Port: 80, Pool: "web"}}, Pools: []pve.LBPool{{Name: "web", Protocol: "http", Algorithm: "roundrobin"}}}}
	row := &cloud.CloudResource{ExternalID: "lb-abc", ServiceID: "svc", ProjectID: "p1", Type: cloud.TypeLoadBalancer}
	if err := setLBRow(row, lb); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cloud.Insert(ctx, row); err != nil {
		t.Fatal(err)
	}
	if len(live()) != 0 {
		t.Fatalf("the allocation alone wrote a row: %+v", live())
	}
	time.Sleep(1100 * time.Millisecond) // the pass is later than the allocation by more than the row's resolution
	for pass := 1; pass <= 2; pass++ {
		if status, err := h.pveLBApply(ctx, es, proj, "lb-abc"); err != nil || status != lbStatusActive {
			t.Fatalf("pass %d: %s %v", pass, status, err)
		}
		if a, rows := held(owner), live(); len(a) != 1 || len(rows) != 1 || !same(rows[0], a[0]) {
			t.Fatalf("pass %d: the pair's address %+v has rows %+v, want the one the sync makes of it", pass, a, rows)
		}
	}
	// A provider sync after it finds its own row and changes nothing: one row per address, never two.
	if st, err := providers.Reconcile(ctx, syncedFloatingIPs{h.publicIPs, es.PVERegion()}, h.cloud, "svc", time.Now().UTC()); err != nil || st.Created+st.Updated+st.Deleted != 0 {
		t.Fatalf("a sync after the pass changed the rows: %+v %v", st, err)
	}
	if a, rows := held(owner), live(); len(rows) != 1 || !same(rows[0], a[0]) {
		t.Fatalf("after the sync: rows %+v", rows)
	}
	// The delete's release (lbDestroy's, for a pair) archives it.
	if err := h.lbGiveBack(ctx, es, proj, owner); err != nil {
		t.Fatal(err)
	}
	if len(live()) != 0 || len(archived()) != 3 {
		t.Fatalf("after the pair's release: %d live rows, %d archived", len(live()), len(archived()))
	}
}

// syncedFloatingIPs lists what the provider sync's floating IP provider lists (syncjob.floatingIPProvider, which
// this package cannot import): the FloatingIPRow of every public allocation of project p1 on service svc.
type syncedFloatingIPs struct {
	ips    *PublicIPRepo
	region string
}

func (syncedFloatingIPs) Type() string          { return cloud.TypeFloatingIP }
func (syncedFloatingIPs) ProjectID() string     { return "p1" }
func (syncedFloatingIPs) CompareKeys() []string { return []string{} }
func (p syncedFloatingIPs) List(ctx context.Context) ([]cloud.CloudResource, error) {
	allocs, err := p.ips.ForProject(ctx, "svc", "p1")
	if err != nil {
		return nil, err
	}
	var out []cloud.CloudResource
	for _, a := range allocs {
		if a.IsPublic() {
			row := FloatingIPRow(a, p.region)
			row.ServiceID, row.ProjectID = "", "p1"
			out = append(out, row)
		}
	}
	return out, nil
}

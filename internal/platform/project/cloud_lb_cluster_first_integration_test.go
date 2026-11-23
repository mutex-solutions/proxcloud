//go:build integration

package project

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/lbgw"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// A cluster's pair while it has one appliance (drill Q0b-1 of v0.11.1: the second appliance was refused for want of
// room and tried again five minutes later; meanwhile the Services showed their address and nothing answered, and the
// row read ERROR for a pass). From the controller's state to the appliance, against a real PostgreSQL and the pair's
// wire fake, whose guest refuses a bind without keepalived's files and elects as keepalived does. The fake builds
// no VM: the row stands as lbClusterBuild leaves it after the first appliance and a second build that just failed
// (buildFailedAt), and the second appliance is put into the row the way that function does when its build succeeds.
//
// Shown: with one appliance a TCP and a UDP Service get their address and are ready, served by that appliance as
// its own master; the row reads DEGRADED and says the platform builds the second one, and never ERROR; a Service
// added meanwhile is served too; one removed gives its address back (the appliance is settled); and when the
// second appliance comes the first learns of it before the new one's keepalived starts, keeps the addresses, the
// Services stay ready, and the row is ACTIVE.
func testLBClusterFirstAppliance(t *testing.T) {
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

	ext := lbClusterExternalID("c1")
	id := ext[len("lb-"):]
	f := newPairWire(t, pve.LBTag(id))
	delete(f.guests, "302")
	*f.guests["301"] = lbGuest{packages: "installed", haproxy: true} // fresh from its image: no keepalived, no role
	srv := httptest.NewServer(f)
	defer srv.Close()
	ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	for coll, docs := range map[string][]pgdoc.M{
		"externalService": {
			{"_id": "svc", "type": "CLOUD", "config": pgdoc.M{"provider": "proxmox", "network": pgdoc.M{"publicNetworks": []any{
				pgdoc.M{"id": "vmbr1", "cidr": "203.0.113.0/24", "gateway": "203.0.113.1", "ranges": []any{"203.0.113.10-203.0.113.20"}}}}}},
			{"_id": "k8s", "type": "CLOUD", "config": pgdoc.M{"provider": "kamaji", "cluster": pgdoc.M{"loadBalancer": pgdoc.M{"publicNetworkId": "vmbr1", "flavorId": "lb-small"}}}},
		},
		"project": {{"_id": "p1", "name": "p1", "status": StatusEnabled, "services": []any{}, "quota": pgdoc.M{"floatingIps": 5}}},
	} {
		for _, d := range docs {
			if _, err := db.C(coll).InsertOne(ctx, d); err != nil {
				t.Fatal(err)
			}
		}
	}
	flavors := pveflavor.New(db)
	if err := flavors.Save(ctx, &pveflavor.Flavor{Slug: "lb-small", Name: "lb-small", VCPUs: 1, RAMMb: 1024, DiskGb: 10, Active: true}); err != nil {
		t.Fatal(err)
	}
	store := lbgw.NewStore(db)
	h := &Handler{svc: NewService(NewRepo(db), nil, nil, nil, nil), cloud: cloud.NewRepo(db), sdnLock: lock.New(db), publicIPs: NewPublicIPRepo(db),
		esSvc: externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("k")), pveFlavors: flavors, lbGW: store,
		pveFor: func(*externalservice.ExternalService) (*pve.Service, error) { return ps, nil }}
	cred := lbgw.Credential{ID: "c1", ProjectID: "p1", ComputeServiceID: "svc", KubernetesServiceID: "k8s", NetworkID: "net-1", Vnet: "v1"}
	if _, _, err := store.MintCredential(ctx, cred); err != nil {
		t.Fatal(err)
	}
	vnet := externalservice.PVEPublicNetwork{ID: "v1", CIDR: "10.0.0.0/24", Gateway: "10.0.0.1", Ranges: []string{"10.0.0.2-10.0.0.254"}}
	reserved, err := h.publicIPs.Reserve(ctx, "svc", "p1", kamajiReservation("c1"), vnet, kamajiWorkerAddrs+kamajiLBAddrs)
	if err != nil {
		t.Fatal(err)
	}
	nodes := []lbgw.Node{{Name: "w1", Address: reserved[0]}, {Name: "w2", Address: reserved[1]}}
	row := &cloud.CloudResource{ExternalID: ext, ServiceID: "svc", ProjectID: "p1", Type: cloud.TypeLoadBalancer}
	if err := setLBRow(row, &loadBalancer{ID: id, Name: "k8s-" + id, Status: lbStatusBuild, NetworkID: "v1", NetworkRef: "net-1",
		PublicNetworkID: "vmbr1", HA: true, FlavorID: "lb-small", ServerID: "301", PrivateIP: "10.0.0.31", PrivatePrefix: 24,
		Appliances: []lbAppliance{{ServerID: "301", PrivateIP: "10.0.0.31"}},
		Cluster:    &lbCluster{ID: "c1", BuildFailedAt: time.Now().UTC().Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cloud.Insert(ctx, row); err != nil {
		t.Fatal(err)
	}

	read := func() *loadBalancer {
		t.Helper()
		cr, err := h.cloud.FindByServiceIDAndExternalID(ctx, "svc", ext)
		if err != nil || cr == nil {
			t.Fatalf("row: %v", err)
		}
		lb, err := lbOfRow(cr)
		if err != nil {
			t.Fatal(err)
		}
		return lb
	}
	var statuses []string // every status the row was read with
	converge := func(what string, done func(*loadBalancer) bool) *loadBalancer {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			h.lbClusterConverge(ctx, "c1", true)
			lb := read()
			statuses = append(statuses, lb.Status)
			if done(lb) {
				return lb
			}
		}
		lb := read()
		t.Fatalf("%s: %s %q, %+v %+v\n%s", what, lb.Status, lb.StatusMessage, lb.Cluster, lb.Appliances, strings.Join(f.log, "\n"))
		return nil
	}
	ready := func(n int) func(*loadBalancer) bool {
		return func(lb *loadBalancer) bool {
			return len(lb.Cluster.Services) == n && len(lb.Cluster.Releasing) == 0 &&
				!slices.ContainsFunc(lb.Cluster.Services, func(s lbClusterSvc) bool { return s.State != lbgw.StateReady })
		}
	}
	owned := func() int {
		t.Helper()
		rows, err := h.publicIPs.ForServer(ctx, "svc", LBAddressOwner(id))
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	svc := func(uid, proto string, port, nodePort int) lbgw.Service {
		return lbgw.Service{UID: uid, Namespace: "default", Name: uid, Ports: []lbgw.Port{{Protocol: proto, Port: port, NodePort: nodePort}},
			ExternalTrafficPolicy: "Cluster"}
	}
	state := func(services ...lbgw.Service) {
		t.Helper()
		if _, err := h.LBClusterState(ctx, &cred, services, nodes, nil); err != nil {
			t.Fatal(err)
		}
	}
	guest := func(vm string) lbGuest {
		f.mu.Lock()
		defer f.mu.Unlock()
		g := *f.guests[vm]
		g.disk = map[string]string{lbGuestKeepalived: f.guests[vm].disk[lbGuestKeepalived]}
		return g
	}

	// One appliance: a TCP and a UDP Service are served by it.
	web, dns := svc("web", "TCP", 80, 30080), svc("dns", "UDP", 53, 30053)
	state(web, dns)
	lb := converge("two Services on one appliance", ready(2))
	a := lb.Appliances[0]
	if lb.Status != lbStatusDegraded || !strings.Contains(lb.StatusMessage, "The platform builds the second one itself") || strings.Contains(lb.StatusMessage, "Replace appliances") ||
		len(lb.Appliances) != 1 || a.State != lbAppUp || a.Role != "MASTER" || !a.HoldsVIP || a.KeepalivedHash == "" || a.LVSHash == "" || lb.AppliedHash != lbAppliedHash(lb) {
		t.Fatalf("one appliance: %s %q, %+v", lb.Status, lb.StatusMessage, a)
	}
	g := guest("301")
	if !g.serving() || !g.lvs || !strings.Contains(g.disk[lbGuestKeepalived], "unicast_peer {\n    127.0.0.1\n") || len(lb.VIPs) != 2 ||
		!strings.Contains(g.disk[lbGuestKeepalived], lb.VIPs[0]+"/24 dev ens19") || !strings.Contains(g.disk[lbGuestKeepalived], lb.VIPs[1]+"/24 dev ens19") {
		t.Fatalf("the appliance: serving %v, UDP %v, addresses %v\n%s", g.serving(), g.lvs, lb.VIPs, g.disk[lbGuestKeepalived])
	}
	if slices.ContainsFunc(f.rules["301"], func(r pve.FirewallRule) bool { return r.Proto == "vrrp" }) {
		t.Fatalf("an appliance with no peer lets VRRP in: %+v", f.rules["301"])
	}

	// A Service added while it is alone is served, and one removed gives its address back.
	state(web, dns, svc("api", "TCP", 81, 30081))
	lb = converge("a third Service on one appliance", ready(3))
	if g := guest("301"); owned() != 3 || !strings.Contains(g.disk[lbGuestKeepalived], lb.VIPs[2]+"/24 dev ens19") || !g.serving() {
		t.Fatalf("the third Service: %d addresses\n%s", owned(), g.disk[lbGuestKeepalived])
	}
	state(web, dns)
	lb = converge("a Service removed on one appliance", func(lb *loadBalancer) bool { return ready(2)(lb) && owned() == 2 })
	if lb.Status != lbStatusDegraded {
		t.Fatalf("after the removal: %s %q", lb.Status, lb.StatusMessage)
	}
	if slices.Contains(statuses, lbStatusError) {
		t.Fatalf("the row read ERROR while the pair had one appliance: %v", statuses)
	}

	// The second appliance, as lbClusterBuild puts it into the row.
	es, err := h.esSvc.Get(ctx, "svc")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.guests["302"] = &lbGuest{packages: "installed", haproxy: true}
	mark := len(f.log)
	f.mu.Unlock()
	if _, err := h.lbEdit(ctx, es, ext, func(lb *loadBalancer) {
		lb.Appliances = append(lb.Appliances, lbAppliance{ServerID: "302", PrivateIP: "10.0.0.32"})
		lb.Cluster.BuildFailedAt, lb.StatusMessage = "", ""
		lbPairLegacyFields(lb)
	}); err != nil {
		t.Fatal(err)
	}
	lb = converge("the second appliance", func(lb *loadBalancer) bool { return lb.Status == lbStatusActive && ready(2)(lb) })
	a, b := lb.Appliances[0], lb.Appliances[1]
	if a.Role != "MASTER" || !a.HoldsVIP || a.State != lbAppUp || b.Role != "BACKUP" || b.HoldsVIP || b.State != lbAppUp || b.LVSHash != a.LVSHash || b.HAProxyHash != a.HAProxyHash {
		t.Fatalf("the pair: %+v %+v", a, b)
	}
	f.mu.Lock()
	joined := slices.Clone(f.log[mark:])
	f.mu.Unlock()
	if first, second := slices.Index(joined, "301 keepalived reload"), slices.Index(joined, "302 keepalived reload"); first < 0 || first > second {
		t.Fatalf("the appliance that served alone learns of its peer before the peer's keepalived starts:\n%s", strings.Join(joined, "\n"))
	}
	if slices.Contains(joined, "301 haproxy reload") || slices.Contains(joined, "301 lvs reload") {
		t.Fatalf("the second appliance's arrival reloaded HAProxy or the UDP service on the one that serves:\n%s", strings.Join(joined, "\n"))
	}
	if g := guest("301"); !strings.Contains(g.disk[lbGuestKeepalived], "unicast_peer {\n    10.0.0.32\n") {
		t.Fatalf("the first appliance's keepalived after the second came:\n%s", g.disk[lbGuestKeepalived])
	}
	if slices.Contains(statuses, lbStatusError) {
		t.Fatalf("the row read ERROR on the way to a pair: %v", statuses)
	}
}

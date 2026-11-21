//go:build integration

package project

import (
	"context"
	"errors"
	"net/http"
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

// A cluster's balancer from the gateway's state to its release, against Postgres and a fake Proxmox with the pair's
// two appliances already built: members only from the cluster's worker addresses, one address per Service that a
// repeated PUT does not double, a Service removed gives its address back once the appliances let go of it, the
// floatingIps quota, a suspended or deleting project changes nothing, a site without the provider setting refuses,
// and the release revokes first and leaves no row, address or state.
func TestLBClusterStateConverges(t *testing.T) {
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
	f := &pairWire{t: t, tag: pve.LBTag(id), rules: map[string][]pve.FirewallRule{}, sets: map[string][]string{},
		vrrp: map[string]string{"301": "BACKUP 1 0", "302": "MASTER 1 1"}, runs: map[int]string{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range []pgdoc.M{
		{"_id": "svc", "type": "CLOUD", "config": pgdoc.M{"provider": "proxmox", "network": pgdoc.M{"publicNetworks": []any{
			pgdoc.M{"id": "vmbr1", "cidr": "203.0.113.0/24", "gateway": "203.0.113.1", "ranges": []any{"203.0.113.10-203.0.113.20"}}}}}},
		{"_id": "k8s", "type": "CLOUD", "config": pgdoc.M{"provider": "kamaji", "cluster": pgdoc.M{"loadBalancer": pgdoc.M{"publicNetworkId": "vmbr1", "flavorId": "lb-small"}}}},
		{"_id": "k8s-off", "type": "CLOUD", "config": pgdoc.M{"provider": "kamaji", "cluster": pgdoc.M{}}},
	} {
		if _, err := db.C("externalService").InsertOne(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	flavors := pveflavor.New(db)
	if err := flavors.Save(ctx, &pveflavor.Flavor{Slug: "lb-small", Name: "lb-small", VCPUs: 1, RAMMb: 1024, DiskGb: 10, Active: true}); err != nil {
		t.Fatal(err)
	}
	projects := NewRepo(db)
	if _, err := db.C("project").InsertOne(ctx, pgdoc.M{"_id": "p1", "name": "p1", "status": StatusEnabled, "services": []any{},
		"quota": pgdoc.M{"floatingIps": 2}}); err != nil {
		t.Fatal(err)
	}
	store := lbgw.NewStore(db)
	h := &Handler{svc: NewService(projects, nil, nil, nil, nil), cloud: cloud.NewRepo(db), sdnLock: lock.New(db), publicIPs: NewPublicIPRepo(db),
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
	worker1, worker2, metallb := reserved[0], reserved[1], reserved[kamajiWorkerAddrs]

	// The pair as a create would have left it, before the first Service: two appliances, no address.
	row := &cloud.CloudResource{ExternalID: ext, ServiceID: "svc", ProjectID: "p1", Type: cloud.TypeLoadBalancer}
	if err := setLBRow(row, &loadBalancer{ID: id, Name: "k8s-" + id, Status: lbStatusBuild, NetworkID: "v1", NetworkRef: "net-1",
		PublicNetworkID: "vmbr1", HA: true, FlavorID: "lb-small", ServerID: "301", PrivateIP: "10.0.0.31", PrivatePrefix: 24,
		Appliances: []lbAppliance{{ServerID: "301", PrivateIP: "10.0.0.31"}, {ServerID: "302", PrivateIP: "10.0.0.32"}},
		Cluster:    &lbCluster{ID: "c1"}}); err != nil {
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
	// The PUT starts a converge of its own; this one runs whenever that one is done, until the row says so.
	converge := func(what string, done func(*loadBalancer) bool) *loadBalancer {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			h.lbClusterConverge(ctx, "c1")
			if lb := read(); done(lb) {
				return lb
			}
		}
		lb := read()
		t.Fatalf("%s: %+v %+v\n%s", what, lb.Cluster, lb.Appliances, strings.Join(f.log, "\n"))
		return nil
	}
	owned := func() []string {
		t.Helper()
		rows, err := h.publicIPs.ForServer(ctx, "svc", LBAddressOwner(id))
		if err != nil {
			t.Fatal(err)
		}
		var ips []string
		for _, r := range rows {
			ips = append(ips, r.IP)
		}
		slices.Sort(ips)
		return ips
	}
	svc := func(uid string, port, nodePort int) lbgw.Service {
		return lbgw.Service{UID: uid, Namespace: "default", Name: uid, Ports: []lbgw.Port{{Protocol: "TCP", Port: port, NodePort: nodePort}},
			ExternalTrafficPolicy: "Cluster"}
	}
	nodes := []lbgw.Node{{Name: "w1", Address: worker1}, {Name: "w2", Address: worker2}, {Name: "metallb", Address: metallb}, {Name: "x", Address: "10.9.9.9"}}
	state := func(services ...lbgw.Service) *lbgw.Answer {
		t.Helper()
		ans, err := h.LBClusterState(ctx, &cred, services, nodes)
		if err != nil {
			t.Fatal(err)
		}
		return ans
	}

	// The first Service: an address, members from the workers' part of the reservation only.
	ans := state(svc("a", 80, 30080))
	if len(ans.Services) != 1 || ans.Services[0].State != lbgw.StateBuilding || len(ans.Nodes) != 2 ||
		ans.Nodes[0].Address != metallb || ans.Nodes[1].Address != "10.9.9.9" || ans.Nodes[0].Reason != lbgw.ReasonNotAWorker {
		t.Fatalf("first answer: %+v", ans)
	}
	lb := converge("first Service ready", func(lb *loadBalancer) bool {
		return len(lb.Cluster.Services) == 1 && lb.Cluster.Services[0].State == lbgw.StateReady
	})
	vipA := lb.Cluster.Services[0].VIP
	if vipA == "" || !slices.Equal(lb.VIPs, []string{vipA}) || !slices.Equal(owned(), []string{vipA}) || lb.Status != lbStatusActive {
		t.Fatalf("address: %q %v %v %s", vipA, lb.VIPs, owned(), lb.Status)
	}
	var members []string
	for _, m := range lb.Pools[0].Members {
		members = append(members, m.Address)
	}
	if !slices.Equal(members, []string{worker1, worker2}) {
		t.Fatalf("members %v", members)
	}
	cfg := f.files[len(f.files)-1]
	if !strings.Contains(cfg, "timeout client 1h") || !strings.Contains(cfg, "bind "+vipA+":80") || strings.Contains(cfg, metallb) || !strings.Contains(cfg, worker1+":30080 check") {
		t.Fatalf("config:\n%s", cfg)
	}
	if !slices.ContainsFunc(f.rules["301"], func(r pve.FirewallRule) bool { return r.Dest == vipA && r.DPort == "80" }) || !slices.Contains(f.sets["302"], vipA) {
		t.Fatalf("firewall: %+v %v", f.rules["301"], f.sets["302"])
	}

	// The same state again: the same address, no second one.
	if ans := state(svc("a", 80, 30080)); ans.Services[0].State != lbgw.StateReady || ans.Services[0].Address != vipA {
		t.Fatalf("repeated PUT: %+v", ans.Services)
	}
	converge("repeated PUT", func(lb *loadBalancer) bool { return true })
	if got := owned(); !slices.Equal(got, []string{vipA}) {
		t.Fatalf("a repeated PUT took %v", got)
	}

	// a goes, b comes: b gets an address of its own, and a's goes back to the pool once neither appliance has it.
	f.mu.Lock()
	mark := len(f.log)
	f.mu.Unlock()
	state(svc("b", 443, 30443))
	lb = converge("a released, b ready", func(lb *loadBalancer) bool {
		return len(lb.Cluster.Services) == 1 && lb.Cluster.Services[0].UID == "b" && lb.Cluster.Services[0].State == lbgw.StateReady && len(lb.Cluster.Releasing) == 0
	})
	vipB := lb.Cluster.Services[0].VIP
	if vipB == vipA || !slices.Equal(owned(), []string{vipB}) || slices.Contains(f.sets["301"], vipA) || slices.Contains(f.sets["302"], vipA) ||
		slices.ContainsFunc(f.rules["301"], func(r pve.FirewallRule) bool { return r.Dest == vipA }) {
		t.Fatalf("after a's removal: b %s, owned %v, sets %v %v, rules %+v", vipB, owned(), f.sets["301"], f.sets["302"], f.rules["301"])
	}
	if i, j := slices.Index(f.log[mark:], "301 ipset delete "+vipA), slices.Index(f.log[mark:], "301 keepalived reload"); i < 0 || j < 0 || i < j {
		t.Errorf("a's address left the set before keepalived let go of it:\n%s", strings.Join(f.log, "\n"))
	}

	// The quota allows two addresses: the third Service is refused, the others are served.
	state(svc("b", 443, 30443), svc("c", 5432, 30432), svc("d", 6379, 30379))
	lb = converge("quota", func(lb *loadBalancer) bool {
		return len(lb.Cluster.Services) == 3 && lb.Cluster.Services[1].State == lbgw.StateReady
	})
	if d := lb.Cluster.Services[2]; d.State != lbgw.StateRefused || d.Reason != lbgw.ReasonQuotaExceeded || d.VIP != "" || len(owned()) != 2 {
		t.Fatalf("over the quota: %+v, owned %v", d, owned())
	}
	if ans := state(svc("b", 443, 30443), svc("c", 5432, 30432), svc("d", 6379, 30379)); ans.Services[2].Reason != lbgw.ReasonQuotaExceeded {
		t.Fatalf("answer over the quota: %+v", ans.Services)
	}

	// A suspended project, and one being deleted: refused, and nothing stored.
	before, _ := store.State(ctx, "c1")
	for _, status := range []string{StatusDisabled, StatusScheduledForDeletion} {
		if _, err := db.C("project").SetByID(ctx, "p1", pgdoc.M{"status": status}, nil); err != nil {
			t.Fatal(err)
		}
		_, err := h.LBClusterState(ctx, &cred, []lbgw.Service{svc("e", 80, 30080)}, nodes)
		var rf *lbgw.Refusal
		if !errors.As(err, &rf) || rf.Reason != lbgw.ReasonProjectNotEnabled || rf.Status != http.StatusConflict {
			t.Fatalf("%s project: %v", status, err)
		}
		if after, _ := store.State(ctx, "c1"); !after.UpdatedAt.Equal(*before.UpdatedAt) || len(after.Services) != 3 {
			t.Fatalf("%s project: the state changed", status)
		}
		if err := h.lbClusterPass(ctx, "c1"); err != nil || len(read().Cluster.Services) != 3 {
			t.Fatalf("%s project: converge %v", status, err)
		}
	}
	if _, err := db.C("project").SetByID(ctx, "p1", pgdoc.M{"status": StatusEnabled}, nil); err != nil {
		t.Fatal(err)
	}

	// A site whose Kubernetes provider offers no load balancer: refused before anything is built, nothing stored.
	off := lbgw.Credential{ID: "c2", ProjectID: "p1", ComputeServiceID: "svc", KubernetesServiceID: "k8s-off", NetworkID: "net-1", Vnet: "v1"}
	if _, _, err := store.MintCredential(ctx, off); err != nil {
		t.Fatal(err)
	}
	if ans, err := h.LBClusterState(ctx, &off, []lbgw.Service{svc("a", 80, 30080)}, nil); err != nil || ans.Services[0].Reason != lbgw.ReasonPublicNetworkUnavailable {
		t.Fatalf("no provider setting: %+v %v", ans, err)
	}
	if st, _ := store.State(ctx, "c2"); st != nil {
		t.Fatalf("state stored without the provider setting: %+v", st)
	}

	// The release revokes the credential before anything else: while a converge holds the cluster, the rest waits for
	// the next sweep, but no PUT can build anything any more.
	held, ok, err := h.sdnLock.Wait(ctx, "lbc-c1", time.Minute, 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	if err := h.LBReleaseCluster(ctx, "svc", "p1", "c1"); err == nil {
		t.Fatal("the release went on while a converge ran")
	}
	held()
	if c, _ := store.Credential(ctx, "c1"); c.RevokedAt == nil {
		t.Fatal("the credential was not revoked first")
	}
	// The appliances are gone (the fake lists no VM); the addresses, their FLOATING_IP rows, the row and the state go.
	now := time.Now().UTC()
	if _, err := h.cloud.Insert(ctx, &cloud.CloudResource{ExternalID: "fip-" + vipB, ServiceID: "svc", ProjectID: "p1", Type: cloud.TypeFloatingIP,
		CreatedAt: &now, Data: map[string]any{"floatingIp": map[string]any{"server_id": LBAddressOwner(id), "floating_ip_address": vipB}}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.gone = true
	f.mu.Unlock()
	if err := h.LBReleaseCluster(ctx, "svc", "p1", "c1"); err != nil {
		t.Fatal(err)
	}
	fips, _ := h.cloud.FindByServiceProjectAndType(ctx, "svc", "p1", cloud.TypeFloatingIP)
	st, _ := store.State(ctx, "c1")
	if cr, _ := h.cloud.FindByServiceIDAndExternalID(ctx, "svc", ext); cr != nil || len(owned()) != 0 || len(fips) != 0 || st != nil {
		t.Fatalf("left after the release: row %v, addresses %v, floating rows %d, state %v", cr != nil, owned(), len(fips), st)
	}
	// A converge after the release builds nothing, even with a state stored again.
	if err := store.PutState(ctx, &lbgw.State{ID: "c1", Services: []lbgw.Service{svc("a", 80, 30080)}}); err != nil {
		t.Fatal(err)
	}
	if err := h.lbClusterPass(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if cr, _ := h.cloud.FindByServiceIDAndExternalID(ctx, "svc", ext); cr != nil {
		t.Fatal("a revoked cluster's converge made a row")
	}
	// The worker reservation is the cluster release's, not the balancer's: it is untouched.
	if rows, _ := h.publicIPs.ForServer(ctx, "svc", kamajiReservation("c1")); len(rows) != kamajiWorkerAddrs+kamajiLBAddrs {
		t.Fatalf("reservation: %d rows", len(rows))
	}
}

//go:build integration

package project

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/csigw"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/lbgw"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// lbClusterPassed runs a pass over a cluster's state under the cluster's lock, as lbClusterConverge does, and says
// whether it ran. A state the controller sends starts a pass of its own in the background, and while that one holds
// the lock lbClusterConverge does nothing: a test that called it and then read the row could read the row of before
// that state, and took it for the answer whenever what it waited for was already true of that row.
// TestLBClusterStateConverges/KeepAddress failed so in CI ("d on a's address" read d as still refused) once the pair
// fake ran the state read's script, which makes a pass take longer. So a test's converge reads the row only after a
// pass of its own.
func lbClusterPassed(ctx context.Context, h *Handler, clusterID string) bool {
	release, ok, err := h.sdnLock.Hold(ctx, "lbc-"+clusterID, lbcLease, 0)
	if err != nil || !ok {
		return false
	}
	defer release()
	_ = h.lbClusterPass(ctx, clusterID, true)
	return true
}

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
	f := newPairWire(t, pve.LBTag(id))
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
			if !lbClusterPassed(ctx, h, "c1") {
				continue // another pass is at work: the row may still be the one of before
			}
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
		ans, err := h.LBClusterState(ctx, &cred, services, nodes, nil, nil)
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

	// What the consoles show next to the cluster: the balancer row and the controller's last call, never the secret;
	// the operator reads it through the cluster's own Kubernetes provider only.
	if view, err := h.lbClusterView(ctx, "c1"); err != nil || view == nil || view.ControllerProtocol != 0 || view.ControllerLastSeen != nil {
		t.Fatalf("view before the controller's first call: %+v, %v", view, err)
	}
	if err := store.Seen(ctx, "c1", time.Now().UTC(), 1); err != nil {
		t.Fatal(err)
	}
	view, err := h.lbClusterView(ctx, "c1")
	if err != nil || view == nil || view.ControllerLastSeen == nil || view.ControllerSince == nil || view.projectID != "p1" {
		t.Fatalf("view: %+v, %v", view, err)
	}
	// The controller's protocol next to the portal's: the consoles name an older controller by it.
	if view.ControllerProtocol != 1 || view.Protocol != lbgw.Protocol || len(view.Certificates) != 0 {
		t.Fatalf("view says protocol %d of %d and certificates %+v", view.ControllerProtocol, view.Protocol, view.Certificates)
	}
	if cr, _ := h.cloud.FindByServiceIDAndExternalID(ctx, "svc", ext); view.BalancerID != cr.ID {
		t.Fatalf("view names row %q, want %q", view.BalancerID, cr.ID)
	}
	if b, _ := json.Marshal(view); !strings.Contains(string(b), `"vip":"`+vipA+`"`) || strings.Contains(string(b), "secret") {
		t.Fatalf("view JSON: %s", b)
	}
	if v, _ := h.ClusterLBView(ctx, &externalservice.ExternalService{ID: "k8s-off"}, "c1"); v != nil {
		t.Fatalf("another provider reads the cluster's balancer: %+v", v)
	}
	if v, _ := h.ClusterLBView(ctx, &externalservice.ExternalService{ID: "k8s"}, "c1"); v == nil {
		t.Fatal("the cluster's provider reads no balancer")
	}

	// The same state again: the same address, no second one.
	if ans := state(svc("a", 80, 30080)); ans.Services[0].State != lbgw.StateReady || ans.Services[0].Address != vipA {
		t.Fatalf("repeated PUT: %+v", ans.Services)
	}
	converge("repeated PUT", func(lb *loadBalancer) bool { return true })
	if got := owned(); !slices.Equal(got, []string{vipA}) {
		t.Fatalf("a repeated PUT took %v", got)
	}

	// Protocol 2: the Service gains UDP on its port's number (the same node port, as Kubernetes allocates it), an
	// affinity and a method. It keeps its one address; the UDP port goes to the appliances' LVS
	// and opens in the firewall beside the TCP one; nothing of it reaches HAProxy. When the UDP port goes, so does
	// the UDP service of both appliances, and the config is the one it was.
	tcpOnly := f.files[len(f.files)-1]
	mixed := svc("a", 80, 30080)
	mixed.Ports = append(mixed.Ports, lbgw.Port{Protocol: "UDP", Port: 80, NodePort: 30080})
	mixed.SessionAffinity, mixed.Annotations = "ClientIP", map[string]string{"proxcloud.io/lb-method": "LEAST_CONNECTIONS"}
	if _, reason, msg := lbgw.CheckService(&mixed, 0, lbgw.Protocol); reason != "" {
		t.Fatalf("the gateway refuses the mixed Service: %s %s", reason, msg)
	}
	state(mixed)
	lb = converge("TCP and UDP on one port", func(lb *loadBalancer) bool {
		return len(lb.Listeners) == 2 && lb.Cluster.Services[0].State == lbgw.StateReady && lb.Status == lbStatusActive
	})
	if lb.Cluster.Services[0].VIP != vipA || !slices.Equal(owned(), []string{vipA}) {
		t.Fatalf("the Service's address moved, or a second one was taken: %q %v", lb.Cluster.Services[0].VIP, owned())
	}
	if l, p := lb.Listeners[1], lb.Pools[1]; l.Protocol != "udp" || l.VIP != vipA || l.Port != 80 || p.Protocol != "udp" || p.Algorithm != "leastconn" ||
		p.SessionPersistence == nil || len(p.Members) != 2 || p.Members[0].Port != 30080 || p.Name == lb.Pools[0].Name {
		t.Fatalf("UDP listener and pool: %+v %+v", l, p)
	}
	for _, vm := range []string{"301", "302"} {
		if !slices.Contains(f.log, vm+" lvs reload") {
			t.Fatalf("appliance %s got no UDP service:\n%s", vm, strings.Join(f.log, "\n"))
		}
		for _, proto := range []string{"tcp", "udp"} {
			if !slices.ContainsFunc(f.rules[vm], func(r pve.FirewallRule) bool { return r.Proto == proto && r.Dest == vipA && r.DPort == "80" }) {
				t.Fatalf("appliance %s: no %s rule for %s:80 in %+v", vm, proto, vipA, f.rules[vm])
			}
		}
	}
	if cfg := f.files[len(f.files)-1]; strings.Contains(cfg, "udp") || !strings.Contains(cfg, "balance leastconn") || !strings.Contains(cfg, "stick on src") {
		t.Fatalf("the HAProxy config of the mixed Service:\n%s", cfg)
	}
	// Drill Q1-2 (v0.11.1-rc.1): while the pair served a UDP listener the state read lost which appliance holds the
	// address, the row stayed DEGRADED, and no Service made or changed afterwards got its address in its status,
	// though it answered. The fake runs the read's script (pairWire.stateRead): the holder is known, and a Service
	// made now is ready with its address in the answer the controller writes into its status.
	if holders := slices.DeleteFunc(slices.Clone(lb.Appliances), func(a lbAppliance) bool { return !a.HoldsVIP }); len(holders) != 1 || holders[0].ServerID != "302" {
		t.Fatalf("with a UDP listener the row does not know its holder: %+v", lb.Appliances)
	}
	late := svc("late", 81, 30081)
	state(mixed, late)
	lb = converge("a Service made while a UDP listener is served", func(lb *loadBalancer) bool {
		return len(lb.Cluster.Services) == 2 && lb.Cluster.Services[1].State == lbgw.StateReady && lb.Status == lbStatusActive
	})
	if ans := state(mixed, late); ans.Services[1].State != lbgw.StateReady || ans.Services[1].Address == "" || ans.Services[1].Address == vipA ||
		ans.Services[0].State != lbgw.StateReady || ans.Services[0].Address != vipA {
		t.Fatalf("the answer for a Service made while a UDP listener is served: %+v", ans.Services)
	}
	if holds := func(vm string) bool { // the standby got each change first
		return slices.ContainsFunc(lb.Appliances, func(a lbAppliance) bool { return a.ServerID == vm && a.HoldsVIP })
	}; !holds("302") || holds("301") || f.first("301 lvs reload") > f.first("302 lvs reload") {
		t.Fatalf("holder and push order with a UDP listener: %+v\n%s", lb.Appliances, strings.Join(f.log, "\n"))
	}
	state(svc("a", 80, 30080))
	lb = converge("the UDP port went", func(lb *loadBalancer) bool {
		return len(lb.Listeners) == 1 && len(lb.Cluster.Services) == 1 && lb.Cluster.Services[0].State == lbgw.StateReady && lb.Status == lbStatusActive &&
			slices.Contains(f.log, "301 lvs remove") && slices.Contains(f.log, "302 lvs remove") && len(owned()) == 1
	})
	if cfg := f.files[len(f.files)-1]; cfg != tcpOnly || lb.Cluster.Services[0].VIP != vipA {
		t.Fatalf("back to the TCP Service: address %q, config:\n%s\nwas:\n%s", lb.Cluster.Services[0].VIP, cfg, tcpOnly)
	}
	if slices.ContainsFunc(f.rules["301"], func(r pve.FirewallRule) bool { return r.Proto == "udp" }) {
		t.Fatalf("a UDP rule outlived the UDP port: %+v", f.rules["301"])
	}

	// N2-4 (v0.11.0-rc.1 drill): the controller sends its state every ten seconds, and every PUT started a full pass,
	// about 16 Proxmox calls each. A PUT that changes nothing on a settled balancer starts none; once the last look is
	// a minute old, six of them make one; a changed state is applied by its own PUT; and a balancer that is not
	// settled (here an appliance someone stopped) is passed over at every PUT until it is.
	passes := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.pages
	}
	lockUntil := func(name string) time.Time {
		var row struct {
			LockUntil time.Time `json:"lockUntil"`
		}
		if _, err := db.C("shedLock").Get(ctx, name, &row); err != nil {
			t.Fatal(err)
		}
		return row.LockUntil
	}
	lockedUntil := func() time.Time { return lockUntil("lbc-c1") }
	// quiet waits for the passes the PUTs started in the background to end, and returns the count.
	quiet := func() int {
		t.Helper()
		for last, same := passes(), 0; ; time.Sleep(100 * time.Millisecond) {
			if now := passes(); now != last || lockedUntil().After(time.Now()) {
				last, same = now, 0
			} else if same++; same == 5 {
				return now
			}
		}
	}
	until := func(what string, done func() bool) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); !done(); time.Sleep(100 * time.Millisecond) {
			if time.Now().After(deadline) {
				lb := read()
				t.Fatalf("%s: %s %q %+v\n%s", what, lb.Status, lb.StatusMessage, lb.Cluster, strings.Join(f.log, "\n"))
			}
		}
	}
	a := svc("a", 80, 30080)
	base := quiet()
	h.lbClusterConverge(ctx, "c1", true)
	perPass := passes() - base
	if perPass < 1 {
		t.Fatalf("a pass read the VM list %d times", perPass)
	}
	base = quiet()
	for i := 0; i < 6; i++ {
		if ans := state(a); ans.Services[0].State != lbgw.StateReady || ans.Services[0].Address != vipA {
			t.Fatalf("an equal PUT is still answered with the Service's state: %+v", ans.Services)
		}
	}
	if got := quiet() - base; got != 0 {
		t.Fatalf("six equal PUTs on a settled balancer started %d passes, want none", got/perPass)
	}
	aged := func() {
		v, _ := h.lbPasses.Load("c1")
		last := v.(lbPass)
		last.at = time.Now().Add(-2 * lbPassEvery)
		h.lbPasses.Store("c1", last)
	}
	aged()
	for i := 0; i < 6; i++ {
		state(a)
	}
	if got := quiet() - base; got != perPass {
		t.Fatalf("six equal PUTs a minute after the last look started %d passes, want one", got/perPass)
	}
	// A changed state: its own PUT applies it, with no sync and no second PUT.
	state(svc("a", 80, 30081))
	until("a changed state applied by its PUT", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return strings.Contains(f.files[len(f.files)-1], worker1+":30081 check")
	})
	state(a)
	until("the state put back", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return strings.Contains(f.files[len(f.files)-1], worker1+":30080 check")
	})
	base = quiet()
	// An appliance stopped behind a settled balancer: the next look starts it, and the PUTs after it pass over the
	// balancer again without waiting a minute, until it is whole.
	f.set("301", func(g *lbGuest) { g.stopped = true })
	state(a)
	if got := quiet() - base; got != 0 {
		t.Fatalf("an equal PUT inside the minute started %d passes", got/perPass)
	}
	aged()
	state(a)
	until("the stopped appliance started by the next look", func() bool { return f.count("301 start") == 1 })
	quiet()
	if lb := read(); lb.Status != lbStatusDegraded || !strings.Contains(lb.StatusMessage, "appliance 301: its VM is stopped; starting it") {
		t.Fatalf("after the look that found it: %s %q", lb.Status, lb.StatusMessage)
	}
	state(a) // no minute has passed: the balancer is not settled, so this PUT passes over it
	until("whole again one PUT later", func() bool { return read().Status == lbStatusActive })
	quiet()

	// N2-3 (v0.11.0-rc.1 drill): the converge lock was taken for 20 minutes, the longest a pass can take, and an API
	// restarted in the middle of a pass left it held: for 24.7 minutes every PUT was stored and none applied. A pass
	// holds it for a lease it extends while it works. What a pass leaves behind when its API dies is what it holds at
	// that moment: one lease.
	//
	// N3-9 (v0.11.0-rc.2 drill): the balancer's own lock (lb-<id>), which the pass takes inside the cluster's, was
	// still taken for three minutes whatever happened to its holder, so a change the API was applying when it stopped
	// went live 190 to 209 s later. It is a lease now, like the cluster's.
	lease, rowLease := lbcLease, lbLease
	lbcLease, lbLease = 1500*time.Millisecond, 1500*time.Millisecond
	defer func() { lbcLease, lbLease = lease, rowLease }()
	gate := make(chan struct{})
	var openGate sync.Once
	open := func() {
		openGate.Do(func() {
			f.mu.Lock()
			f.gate = nil
			f.mu.Unlock()
			close(gate)
		})
	}
	defer open() // a failed check must not leave the fake holding a request
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()
	passDone := make(chan struct{})
	go func() { h.lbClusterConverge(ctx, "c1", true); close(passDone) }()
	until("a pass in the middle of its work", func() bool { return lockedUntil().After(time.Now()) && lockUntil("lb-"+ext).After(time.Now()) })
	first, firstRow := lockedUntil(), lockUntil("lb-"+ext)
	if left := time.Until(first); left > lbcLease {
		t.Fatalf("an API that dies now leaves the cluster locked for %s, want a lease (%s) at most", left, lbcLease)
	}
	if left := time.Until(firstRow); left > lbLease {
		t.Fatalf("an API that dies now leaves the balancer locked for %s, want a lease (%s) at most", left, lbLease)
	}
	time.Sleep(2 * lbcLease) // the pass works on, longer than a lease
	if now := lockedUntil(); !now.After(time.Now()) || !now.After(first) {
		t.Fatalf("a pass still at work lost its lock: until %s, first %s", now, first)
	}
	if now := lockUntil("lb-" + ext); !now.After(time.Now()) || !now.After(firstRow) {
		t.Fatalf("a pass still at work lost the balancer's lock: until %s, first %s", now, firstRow)
	}
	open()
	<-passDone
	if lockedUntil().After(time.Now()) || lockUntil("lb-"+ext).After(time.Now()) {
		t.Fatal("a lock outlived its pass")
	}
	// The locks of a pass whose API died in its apply: held, extended by nobody. The state the controller sends
	// meanwhile is stored and not applied; it is applied by a PUT a lease later.
	for _, name := range []string{"lbc-c1", "lb-" + ext} {
		if ok, err := h.sdnLock.Lock(ctx, name, lbcLease, time.Now().UTC()); err != nil || !ok {
			t.Fatalf("lock %s: %v %v", name, ok, err)
		}
	}
	died := time.Now()
	changed := svc("a", 80, 30082)
	state(changed)
	time.Sleep(300 * time.Millisecond)
	f.mu.Lock()
	early := strings.Contains(f.files[len(f.files)-1], ":30082 check")
	f.mu.Unlock()
	if early {
		t.Fatal("a state was applied while another holder had the cluster")
	}
	until("the state applied once the dead holder's lease ran out", func() bool {
		state(changed) // the controller comes back
		f.mu.Lock()
		defer f.mu.Unlock()
		return strings.Contains(f.files[len(f.files)-1], worker1+":30082 check")
	})
	if waited := time.Since(died); waited > 4*lbcLease {
		t.Fatalf("the cluster's balancer stood still for %s after its API died, want about a lease (%s)", waited, lbcLease)
	}
	state(a)
	until("the state put back", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return strings.Contains(f.files[len(f.files)-1], worker1+":30080 check")
	})
	quiet()

	// Two changes 2 to 3 s apart (review of #439): the second arrives while the pass of the first is at work. Its PUT
	// finds the cluster's lock held and starts no pass, and the state was applied only when the controller sent it
	// again, about 11 s later. The converge that ran looks at the stored state once more before it lets go of the
	// lock: no PUT comes after the second one here, and it is applied.
	hold := make(chan struct{})
	base = passes()
	f.mu.Lock()
	f.gate = hold
	f.mu.Unlock()
	state(svc("a", 80, 30085))
	until("the first change's pass in the middle of its work", func() bool { return passes() > base && lockedUntil().After(time.Now()) })
	state(svc("a", 80, 30086)) // stored; the pass at work read the state before it
	f.mu.Lock()
	f.gate = nil
	f.mu.Unlock()
	close(hold)
	until("a state stored during a pass applied by the converge that ran, with no PUT after it", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return strings.Contains(f.files[len(f.files)-1], worker1+":30086 check")
	})
	quiet()
	// Three changes that follow each other (drill R2a-1 of v0.11.1-rc.2: the later ones were live after up to 44 s).
	// The third arrives while the converge is at its second pass, which is its last: the look at the stored state
	// comes before that pass, so the third state was nobody's until the controller sent it again. Its PUT finds the
	// lock held and leaves a mark; the converge that lets go of the lock starts the next one at once. No PUT comes
	// after the third here, and it is applied.
	gate1, gate2 := make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.gate = gate1
	f.mu.Unlock()
	base = passes()
	state(svc("a", 80, 30087))
	until("the first change's pass in the middle of its work", func() bool { return passes() > base && lockedUntil().After(time.Now()) })
	state(svc("a", 80, 30088)) // stored; the look after the first pass finds it
	f.mu.Lock()
	f.gate, f.gateOn = gate2, worker1+":30087 check" // the passes after the one that applies the first change
	f.mu.Unlock()
	close(gate1)
	until("the first pass done and the second in the middle of its work", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return strings.Contains(f.files[len(f.files)-1], worker1+":30087 check") && f.gated > 0
	})
	state(svc("a", 80, 30089)) // stored during the converge's last pass: nothing looks at the state after it
	f.mu.Lock()
	f.gate, f.gateOn = nil, ""
	f.mu.Unlock()
	close(gate2)
	until("a state stored during a converge's last pass applied by the converge it started, with no PUT after it", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return strings.Contains(f.files[len(f.files)-1], worker1+":30089 check")
	})
	quiet()
	// Once more, not until the state stands still: under a Service that keeps changing a converge still ends, so
	// its lock is free for the next one and no pass runs without a PUT behind it.
	stop, flapped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(flapped)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			if st, err := store.State(ctx, "c1"); err == nil && st != nil {
				st.Services = []lbgw.Service{svc("a", 80, 30100+i%2000)} // never the state a pass just applied
				_ = store.PutState(ctx, st)
			}
		}
	}()
	ended := make(chan struct{})
	go func() { h.lbClusterConverge(ctx, "c1", true); close(ended) }()
	select {
	case <-ended:
	case <-time.After(20 * time.Second):
		close(stop)
		t.Fatal("a Service that keeps changing kept one converge at work")
	}
	close(stop)
	<-flapped
	if lockedUntil().After(time.Now()) {
		t.Fatal("the converge of a changing state kept its lock")
	}
	state(a)
	until("the state put back", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return strings.Contains(f.files[len(f.files)-1], worker1+":30080 check")
	})
	quiet()

	// N3-7 (v0.11.0-rc.2 drill): the controller writes a Service's address into its status when the Service is
	// answered "ready", so ready means that the appliance holding the address serves the Service's config. Here the
	// holder cannot take a change yet (dpkg is at work on its HAProxy package) and the standby has it: the Service
	// that changed is not ready and keeps its address. Both appliances serve, so the row is ACTIVE and says which
	// appliance waits and why (it read DEGRADED, or DOWN with both waiting, until v0.11.1-rc.2: drill R2b-1).
	loaded := func(vm string) string {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.guests[vm].loaded
	}
	f.set("302", func(g *lbGuest) { g.upgrading = true })
	moved := svc("a", 80, 30084)
	state(moved)
	lb = converge("the standby serves the change, the holder does not", func(lb *loadBalancer) bool {
		return lb.Status == lbStatusActive && lb.StatusMessage != "" && strings.Contains(loaded("301"), worker1+":30084 check")
	})
	if s := lb.Cluster.Services[0]; s.State != lbgw.StateBuilding || s.VIP != vipA || strings.Contains(loaded("302"), ":30084") ||
		!strings.Contains(lb.StatusMessage, "A change waits (appliance 302: a change is not loaded on it yet: its package manager is at work on a package the load balancer runs)") {
		t.Fatalf("ready before the holder serves it: %+v, %s %q", s, lb.Status, lb.StatusMessage)
	}
	if ans := state(moved); ans.Services[0].State != lbgw.StateBuilding || ans.Services[0].Address != vipA {
		t.Fatalf("the controller is answered %+v", ans.Services[0])
	}
	f.set("302", func(g *lbGuest) { g.upgrading = false })
	converge("ready once the holder serves it", func(lb *loadBalancer) bool {
		return lb.Status == lbStatusActive && lb.StatusMessage == "" && lb.Cluster.Services[0].State == lbgw.StateReady && strings.Contains(loaded("302"), worker1+":30084 check")
	})

	// R2b-1 (v0.11.1-rc.2 drill): a worker leaves while both appliances wait. Nothing of the Service changed, and the
	// appliance that holds the address serves it as it is: the Service stays ready, and the row is never DOWN
	// (it read DOWN "No appliance answers" for 29 s, and every Service of the cluster got "building"). The change is
	// then loaded by the platform's own retry, with no other call: not the controller's next send, not a sync.
	every, tries := lbRetryEvery, lbRetryTries
	lbRetryEvery, lbRetryTries = 200*time.Millisecond, 100
	workers := nodes
	nodes = slices.Delete(slices.Clone(workers), 1, 2) // w2 leaves
	for _, vm := range []string{"301", "302"} {
		f.set(vm, func(g *lbGuest) { g.upgrading = true })
	}
	if ans := state(moved); ans.Services[0].State != lbgw.StateReady {
		t.Fatalf("a Service is not ready before the pass of a worker's leaving: %+v", ans.Services[0])
	}
	until("the pass that both appliances hold", func() bool {
		lb := read()
		return lb.HeldSince != "" && strings.Contains(lb.StatusMessage, "A change waits (appliance 301: ")
	})
	if lb = read(); lb.Status != lbStatusActive || lb.Cluster.Services[0].State != lbgw.StateReady ||
		!strings.Contains(loaded("301"), worker2+":30084") || !strings.Contains(loaded("302"), worker2+":30084") {
		t.Fatalf("while both appliances wait: %s %q, Service %+v", lb.Status, lb.StatusMessage, lb.Cluster.Services[0])
	}
	if ans := state(moved); ans.Services[0].State != lbgw.StateReady || ans.Services[0].Address != vipA {
		t.Fatalf("while both appliances wait the controller is answered %+v", ans.Services[0])
	}
	if !strings.Contains(lb.StatusMessage, "appliance 302: a change is not loaded on it yet: appliance 301 takes every change first") {
		t.Fatalf("the row does not say that the holder waits for the standby: %q", lb.StatusMessage)
	}
	for _, vm := range []string{"301", "302"} {
		f.set(vm, func(g *lbGuest) { g.upgrading = false })
	}
	until("the platform's own retry loads the change on both", func() bool {
		return !strings.Contains(loaded("301"), worker2+":30084") && !strings.Contains(loaded("302"), worker2+":30084")
	})
	until("the row after the retry", func() bool {
		lb := read()
		return lb.Status == lbStatusActive && lb.StatusMessage == "" && lb.HeldSince == "" && lb.Cluster.Services[0].State == lbgw.StateReady
	})
	f.mu.Lock()
	lastStandby, lastHolder := 0, 0
	for i, l := range f.log {
		switch l {
		case "301 haproxy reload":
			lastStandby = i
		case "302 haproxy reload":
			lastHolder = i
		}
	}
	f.mu.Unlock()
	if lastHolder < lastStandby {
		t.Fatal("the retry pushed the holder before the standby")
	}
	until("the end of the retries", func() bool {
		_, running := h.lbRetries.Load("lbc-c1")
		return !running
	})
	lbRetryEvery, lbRetryTries = every, tries
	nodes = workers
	state(a)
	until("the state put back", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return strings.Contains(f.files[len(f.files)-1], worker1+":30080 check") && strings.Contains(f.guests["302"].loaded, worker1+":30080 check") &&
			strings.Contains(f.guests["302"].loaded, worker2+":30080 check")
	})
	quiet()

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

	// Another project's cluster naming c1's workers: none is a member of its own balancer, and c1's balancer is
	// untouched by its converge (everything comes from the credential, never from the request).
	if _, err := db.C("project").InsertOne(ctx, pgdoc.M{"_id": "p2", "name": "p2", "status": StatusEnabled, "services": []any{}}); err != nil {
		t.Fatal(err)
	}
	other := lbgw.Credential{ID: "c3", ProjectID: "p2", ComputeServiceID: "svc", KubernetesServiceID: "k8s", NetworkID: "net-2", Vnet: "v2"}
	if _, _, err := store.MintCredential(ctx, other); err != nil {
		t.Fatal(err)
	}
	vipsBefore := slices.Clone(read().VIPs)
	ans, err = h.LBClusterState(ctx, &other, nil, nodes, nil, nil)
	if err != nil || len(ans.Nodes) != len(nodes) {
		t.Fatalf("another project's cluster: %+v %v", ans, err)
	}
	for _, n := range ans.Nodes {
		if n.Reason != lbgw.ReasonNotAWorker {
			t.Fatalf("another project's cluster accepted %s: %+v", n.Address, n)
		}
	}
	if st, _ := store.State(ctx, "c3"); st == nil || len(st.Nodes) != 0 {
		t.Fatalf("another project's cluster stored nodes: %+v", st)
	}
	if err := h.lbClusterPass(ctx, "c3", true); err != nil {
		t.Fatal(err)
	}
	if got := read().VIPs; !slices.Equal(got, vipsBefore) || len(owned()) != 2 {
		t.Fatalf("c3's converge changed c1's balancer: %v -> %v, owned %v", vipsBefore, got, owned())
	}
	if cr, _ := h.cloud.FindByServiceIDAndExternalID(ctx, "svc", lbClusterExternalID("c3")); cr != nil {
		t.Fatal("a cluster with no Service got a balancer row")
	}

	// A suspended project, and one being deleted: refused, and nothing stored.
	before, _ := store.State(ctx, "c1")
	for _, status := range []string{StatusDisabled, StatusScheduledForDeletion} {
		if _, err := db.C("project").SetByID(ctx, "p1", pgdoc.M{"status": status}, nil); err != nil {
			t.Fatal(err)
		}
		_, err := h.LBClusterState(ctx, &cred, []lbgw.Service{svc("e", 80, 30080)}, nodes, nil, nil)
		var rf *lbgw.Refusal
		if !errors.As(err, &rf) || rf.Reason != lbgw.ReasonProjectNotEnabled || rf.Status != http.StatusConflict {
			t.Fatalf("%s project: %v", status, err)
		}
		if after, _ := store.State(ctx, "c1"); !after.UpdatedAt.Equal(*before.UpdatedAt) || len(after.Services) != 3 {
			t.Fatalf("%s project: the state changed", status)
		}
		if err := h.lbClusterPass(ctx, "c1", true); err != nil || len(read().Cluster.Services) != 3 {
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
	if ans, err := h.LBClusterState(ctx, &off, []lbgw.Service{svc("a", 80, 30080)}, nil, nil, nil); err != nil || ans.Services[0].Reason != lbgw.ReasonPublicNetworkUnavailable {
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
	if v, _ := h.lbClusterView(ctx, "c1"); v != nil {
		t.Fatalf("a released cluster still shows a controller: %+v", v)
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
	if err := h.lbClusterPass(ctx, "c1", true); err != nil {
		t.Fatal(err)
	}
	if cr, _ := h.cloud.FindByServiceIDAndExternalID(ctx, "svc", ext); cr != nil {
		t.Fatal("a revoked cluster's converge made a row")
	}
	// The worker reservation is the cluster release's, not the balancer's: it is untouched.
	if rows, _ := h.publicIPs.ForServer(ctx, "svc", kamajiReservation("c1")); len(rows) != kamajiWorkerAddrs+kamajiLBAddrs {
		t.Fatalf("reservation: %d rows", len(rows))
	}

	// The Services that end TLS with a Secret, on a database and a pair of their own
	// (cloud_lb_cluster_tls_integration_test.go).
	t.Run("TLSFromSecret", testLBClusterTLSFromSecret)
	// A Service's address kept for its cluster, and spec.loadBalancerIP (cloud_lb_cluster_keep_integration_test.go).
	t.Run("KeepAddress", testLBClusterKeepAddress)
	// The pair while it has one appliance, and the second one's arrival (cloud_lb_cluster_first_integration_test.go).
	t.Run("FirstAppliance", testLBClusterFirstAppliance)
}

// lbKube is a management cluster holding one tenant cluster's Application and the Secrets written next to it.
type lbKube struct {
	kamaji.K8sAPI
	apps    map[string]map[string]any
	secrets map[string]map[string]string
}

func (k *lbKube) GetApplication(_ context.Context, _, name string) (map[string]any, error) {
	app, ok := k.apps[name]
	if !ok {
		return nil, nil
	}
	b, _ := json.Marshal(app) // a copy, as an API answer is
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out, nil
}

func (k *lbKube) ApplyApplication(_ context.Context, app map[string]any) error {
	b, _ := json.Marshal(app)
	var stored map[string]any
	_ = json.Unmarshal(b, &stored)
	k.apps[stored["metadata"].(map[string]any)["name"].(string)] = stored
	return nil
}

func (k *lbKube) ApplySecret(_ context.Context, ns, name string, data map[string]string, _, _ map[string]string) error {
	k.secrets[ns+"/"+name] = data
	return nil
}

type lbNoPortal struct{}

func (lbNoPortal) LBClusterState(context.Context, *lbgw.Credential, []lbgw.Service, []lbgw.Node, []lbgw.CertificateData, []lbgw.Gone) (*lbgw.Answer, error) {
	return &lbgw.Answer{Services: []lbgw.ServiceStatus{}}, nil
}

// "Enable load balancer controller" on an existing cluster, against Postgres: the credential names the cluster's own
// project, providers and network; its Secret's token passes the gateway; the cluster goes class-only on chart 0.1.17
// with nothing else changed; again rotates (the old token stops) and keeps the mode; a missing gate is named.
func TestLBControllerEnable(t *testing.T) {
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
	for coll, docs := range map[string][]pgdoc.M{
		"externalService": {
			{"_id": "pve", "type": "CLOUD", "config": pgdoc.M{"provider": "proxmox", "network": pgdoc.M{"publicNetworks": []any{
				pgdoc.M{"id": "vmbr1", "cidr": "203.0.113.0/24", "gateway": "203.0.113.1", "ranges": []any{"203.0.113.10-203.0.113.20"}}}}}},
			{"_id": "k8s", "type": "CLOUD", "config": pgdoc.M{"provider": "kamaji", "argocd": pgdoc.M{"chartVersion": "0.1.17"},
				"cluster": pgdoc.M{"loadBalancer": pgdoc.M{"publicNetworkId": "vmbr1", "flavorId": "lb-small"}}}},
			{"_id": "k8s-off", "type": "CLOUD", "config": pgdoc.M{"provider": "kamaji", "argocd": pgdoc.M{"chartVersion": "0.1.17"}}},
		},
		"project": {{"_id": "p1", "name": "p1", "status": StatusEnabled, "services": []any{
			pgdoc.M{"serviceId": "pve", "provider": "proxmox", "pool": "pc-p1"}, pgdoc.M{"serviceId": "k8s", "provider": "kamaji"}}}},
		"cloudResource": {{"_id": "net1", "serviceId": "pve", "projectId": "p1", "type": cloud.TypeNetwork, "externalId": "net-ext",
			"data": pgdoc.M{"network": pgdoc.M{"vnet": "v1", "zone": "pc"}}}},
	} {
		for _, d := range docs {
			if _, err := db.C(coll).InsertOne(ctx, d); err != nil {
				t.Fatal(err)
			}
		}
	}
	store := lbgw.NewStore(db)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("k"))
	h := &Handler{svc: NewService(NewRepo(db), nil, nil, nil, nil), cloud: cloud.NewRepo(db), esSvc: esSvc, lbGW: store,
		csiGW: &CSIGateway{Endpoint: csigw.Endpoint{URL: "https://192.0.2.7:8444", CA: "CA"}}}
	values := pgdoc.M{"kubernetesVersion": "1.35.10",
		"proxmox": pgdoc.M{"pool": "pc-p1", "network": pgdoc.M{"bridge": "v1", "addresses": []any{"10.0.0.250"}},
			"metallb": pgdoc.M{"addresses": []any{"10.0.0.240/32"}}}}
	kube := &lbKube{secrets: map[string]map[string]string{}, apps: map[string]map[string]any{"pc-c-1": {
		"metadata": map[string]any{"name": "pc-c-1", "namespace": "argocd", "labels": map[string]any{
			kamaji.LabelProject: "p1", kamaji.LabelService: "k8s", kamaji.LabelManagedBy: kamaji.ManagedByValue}},
		"spec": map[string]any{"project": "proxcloud-k8s", "destination": map[string]any{"server": "https://kubernetes.default.svc"},
			"source": map[string]any{"targetRevision": "0.1.16", "helm": map[string]any{"valuesObject": values}}},
	}}}
	kamajiFor := func(id, image string) (*externalservice.ExternalService, *kamaji.Service) {
		es, err := esSvc.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		cfg := es.KamajiConfig()
		cfg.LBControllerImage = image
		return es, kamaji.NewWithAPI(kube, cfg, id)
	}
	gateway := &lbgw.Handler{Reg: store, Portal: lbNoPortal{}}
	put := func(token string) int {
		r := httptest.NewRequest(http.MethodPut, lbgw.StatePath, strings.NewReader(`{"services":[],"nodes":[]}`))
		r.Header.Set("Authorization", "PVEAPIToken="+token)
		r.Header.Set(lbgw.ProtocolHeader, "1")
		w := httptest.NewRecorder()
		gateway.ServeHTTP(w, r)
		return w.Code
	}
	lbValues := func() map[string]any {
		v, _ := kube.apps["pc-c-1"]["spec"].(map[string]any)["source"].(map[string]any)["helm"].(map[string]any)["valuesObject"].(map[string]any)
		lb, _ := v["loadBalancer"].(map[string]any)
		return lb
	}
	secretKey := "pc-p1/" + kamaji.LBGatewaySecretName("pc-c-1")

	es, ks := kamajiFor("k8s", "ghcr.io/mutex-solutions/proxcloud-api:v0.11.0")
	if _, err := h.EnableClusterLB(ctx, es, ks, "pc-c-1"); err != nil {
		t.Fatal(err)
	}
	first := kube.secrets[secretKey]
	if !strings.HasPrefix(first["token"], "lb@proxcloud!pc-c-1=") || first["ca.crt"] != "CA" {
		t.Fatalf("secret = %v", first)
	}
	cred, err := store.Credential(ctx, "pc-c-1")
	if err != nil || cred == nil {
		t.Fatalf("credential %v %v", cred, err)
	}
	if cred.ProjectID != "p1" || cred.ComputeServiceID != "pve" || cred.KubernetesServiceID != "k8s" || cred.NetworkID != "net-ext" || cred.Vnet != "v1" {
		t.Fatalf("credential = %+v", cred)
	}
	if code := put(first["token"]); code != http.StatusOK {
		t.Fatalf("the Secret's token: %d", code)
	}
	if rev := kube.apps["pc-c-1"]["spec"].(map[string]any)["source"].(map[string]any)["targetRevision"]; rev != "0.1.17" {
		t.Fatalf("targetRevision = %v", rev)
	}
	lb := lbValues()
	if lb["default"] != false || lb["image"] != "ghcr.io/mutex-solutions/proxcloud-api:v0.11.0" || lb["gatewayURL"] != "https://192.0.2.7:8444" ||
		lb["secretName"] != kamaji.LBGatewaySecretName("pc-c-1") {
		t.Fatalf("loadBalancer = %v", lb)
	}
	if raw, _ := json.Marshal(kube.apps); strings.Contains(string(raw), strings.SplitN(first["token"], "=", 2)[1]) {
		t.Fatal("the Application carries the secret")
	}

	// Again, on a cluster in default mode with a newer portal: a new token, the old one refused, the mode kept.
	lbValues()["default"] = true
	es, ks = kamajiFor("k8s", "ghcr.io/mutex-solutions/proxcloud-api:v0.11.1")
	if _, err := h.EnableClusterLB(ctx, es, ks, "pc-c-1"); err != nil {
		t.Fatal(err)
	}
	second := kube.secrets[secretKey]
	if second["token"] == first["token"] {
		t.Fatal("a second enable kept the token")
	}
	if put(first["token"]) != http.StatusUnauthorized || put(second["token"]) != http.StatusOK {
		t.Fatal("rotation: the old token must stop and the new one pass")
	}
	if lb := lbValues(); lb["default"] != true || lb["image"] != "ghcr.io/mutex-solutions/proxcloud-api:v0.11.1" {
		t.Fatalf("rotation: %v", lb)
	}

	// A gate that does not hold is named and changes nothing.
	refused := func(id, image, want string) {
		t.Helper()
		es, ks := kamajiFor(id, image)
		_, err := h.EnableClusterLB(ctx, es, ks, "pc-c-1")
		var he *httpx.HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusConflict || !strings.Contains(he.Msg, want) {
			t.Fatalf("%s: %v", want, err)
		}
		if kube.secrets[secretKey]["token"] != second["token"] {
			t.Fatalf("%s: a refused enable changed the Secret", want)
		}
	}
	refused("k8s", "", "PROXCLOUD_LB_CONTROLLER_IMAGE")
	kube.apps["pc-c-1"]["metadata"].(map[string]any)["labels"].(map[string]any)[kamaji.LabelService] = "k8s-off"
	refused("k8s-off", "img", "config.cluster.loadBalancer")
	kube.apps["pc-c-1"]["metadata"].(map[string]any)["labels"].(map[string]any)[kamaji.LabelService] = "k8s"
	live := kube.apps["pc-c-1"]["spec"].(map[string]any)["source"].(map[string]any)["helm"].(map[string]any)["valuesObject"].(map[string]any)
	live["proxmox"].(map[string]any)["network"].(map[string]any)["bridge"] = "v9"
	refused("k8s", "img", "not a network of the project")
	live["proxmox"].(map[string]any)["pool"] = "shared"
	refused("k8s", "img", "not the project's pool")
	es, ks = kamajiFor("k8s", "img")
	if _, err := h.EnableClusterLB(ctx, es, ks, "pc-c-nothere"); err == nil {
		t.Fatal("an unknown cluster is an error")
	}
}

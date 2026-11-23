//go:build integration

package project

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/lbgw"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// A Service's address kept for its cluster (proxcloud.io/lb-keep-address) and spec.loadBalancerIP, from the
// controller's request to both appliances: the gateway's handler itself (protocol 2, the requests as the controller
// sends them), the portal's side behind it, a real PostgreSQL and the pair's wire fake (which models the Proxmox API
// and what a guest answers).
//
// Shown: the address of a Service that goes with the annotation leaves both appliances and their firewall and stays
// the balancer's, allocated (so in the floatingIps quota, and billed: the meter makes a FLOATING_IP row of every
// public allocation, syncjob/pve_meter.go) with its FLOATING_IP row left alone, while one that goes without it is
// given back; an annotation set and a Service deleted with no pass in between still keeps; a kept address counts in
// the quota, and a Service that names it gets it without the quota or the pool being asked; a Service that names
// another cluster's kept address, another project's address, a free one, one in use or no address at all gets an
// address of the pool and one Event, the same whichever it was, and nothing of the other cluster moves; a Service
// that has an address keeps it whatever it names afterwards; the sixth kept address is given back, and its Service
// was told so while it stood; the release action gives back a kept address and no other, for the cluster's own
// project only; and the cluster's release gives back what it kept.
func testLBClusterKeepAddress(t *testing.T) {
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
	public := externalservice.PVEPublicNetwork{ID: "vmbr1", CIDR: "203.0.113.0/24", Gateway: "203.0.113.1", Ranges: []string{"203.0.113.10-203.0.113.60"}}
	for coll, docs := range map[string][]pgdoc.M{
		"externalService": {
			{"_id": "svc", "type": "CLOUD", "config": pgdoc.M{"provider": "proxmox", "network": pgdoc.M{"publicNetworks": []any{
				pgdoc.M{"id": public.ID, "cidr": public.CIDR, "gateway": public.Gateway, "ranges": []any{public.Ranges[0]}}}}}},
			{"_id": "k8s", "type": "CLOUD", "config": pgdoc.M{"provider": "kamaji", "cluster": pgdoc.M{"loadBalancer": pgdoc.M{"publicNetworkId": "vmbr1", "flavorId": "lb-small"}}}},
		},
		"project": {
			{"_id": "p1", "name": "p1", "status": StatusEnabled, "services": []any{}, "quota": pgdoc.M{"floatingIps": 20}},
			{"_id": "p2", "name": "p2", "status": StatusEnabled, "services": []any{}, "quota": pgdoc.M{"floatingIps": 20}},
		},
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
	h.kamajiFor = func(es *externalservice.ExternalService) (*kamaji.Service, error) {
		return kamaji.NewWithAPI(&lbKube{}, es.KamajiConfig(), es.ID), nil
	}
	_, secret, err := store.MintCredential(ctx, lbgw.Credential{ID: "c1", ProjectID: "p1", ComputeServiceID: "svc", KubernetesServiceID: "k8s", NetworkID: "net-1", Vnet: "v1"})
	if err != nil {
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
		Appliances: []lbAppliance{{ServerID: "301", PrivateIP: "10.0.0.31"}, {ServerID: "302", PrivateIP: "10.0.0.32"}},
		Cluster:    &lbCluster{ID: "c1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cloud.Insert(ctx, row); err != nil {
		t.Fatal(err)
	}

	gw := &lbgw.Handler{Reg: store, Portal: h}
	put := func(services ...lbgw.Service) *lbgw.Answer {
		t.Helper()
		if services == nil {
			services = []lbgw.Service{}
		}
		raw, _ := json.Marshal(lbgw.Request{Services: services, Nodes: nodes})
		r := httptest.NewRequest(http.MethodPut, lbgw.StatePath, bytes.NewReader(raw))
		r.Header.Set("Authorization", "PVEAPIToken="+lbgw.TokenID("c1")+"="+secret)
		r.Header.Set(lbgw.ProtocolHeader, "2")
		w := httptest.NewRecorder()
		gw.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", w.Code, w.Body)
		}
		var ans lbgw.Answer
		if err := json.Unmarshal(w.Body.Bytes(), &ans); err != nil {
			t.Fatal(err)
		}
		return &ans
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
	converge := func(what string, done func(*loadBalancer) bool) *loadBalancer {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			h.lbClusterConverge(ctx, "c1", true)
			if lb := read(); done(lb) {
				return lb
			}
		}
		lb := read()
		t.Fatalf("%s: %+v %s\n%s", what, lb.Cluster, lb.StatusMessage, strings.Join(f.log, "\n"))
		return nil
	}
	// settled: every Service of the row has its answer, as many as n, and no address waits to be given back.
	settled := func(n int) func(*loadBalancer) bool {
		return func(lb *loadBalancer) bool {
			return len(lb.Cluster.Services) == n && len(lb.Cluster.Releasing) == 0 &&
				!slices.ContainsFunc(lb.Cluster.Services, func(s lbClusterSvc) bool { return s.State == lbgw.StateBuilding })
		}
	}
	owner := LBAddressOwner(id)
	owned := func() []string {
		t.Helper()
		rows, err := h.publicIPs.ForServer(ctx, "svc", owner)
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
	vipOf := func(lb *loadBalancer, uid string) string {
		t.Helper()
		i := slices.IndexFunc(lb.Cluster.Services, func(s lbClusterSvc) bool { return s.UID == uid })
		if i < 0 {
			t.Fatalf("no Service %s in %+v", uid, lb.Cluster.Services)
		}
		return lb.Cluster.Services[i].VIP
	}
	served := func(ip string) bool { // an appliance has the address, or a firewall rule for it
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Contains(f.sets["301"], ip) || slices.Contains(f.sets["302"], ip) ||
			slices.ContainsFunc(f.rules["301"], func(r pve.FirewallRule) bool { return r.Dest == ip }) ||
			slices.ContainsFunc(f.rules["302"], func(r pve.FirewallRule) bool { return r.Dest == ip })
	}
	// What the meter makes of a public allocation, and the address's release archives.
	bill := func(ip string) {
		t.Helper()
		now := time.Now().UTC()
		if _, err := h.cloud.Insert(ctx, &cloud.CloudResource{ExternalID: "fip-" + ip, ServiceID: "svc", ProjectID: "p1", Type: cloud.TypeFloatingIP,
			CreatedAt: &now, Data: map[string]any{"floatingIp": map[string]any{"server_id": owner, "floating_ip_address": ip}}}); err != nil {
			t.Fatal(err)
		}
	}
	billed := func() []string {
		t.Helper()
		fips, err := h.cloud.FindByServiceProjectAndType(ctx, "svc", "p1", cloud.TypeFloatingIP)
		if err != nil {
			t.Fatal(err)
		}
		var ips []string
		for i := range fips {
			fip, _ := fips[i].Data["floatingIp"].(map[string]any)
			ips = append(ips, strAny(fip["floating_ip_address"]))
		}
		slices.Sort(ips)
		return ips
	}
	quota := func(n int) {
		t.Helper()
		if _, err := db.C("project").SetByID(ctx, "p1", pgdoc.M{"quota": pgdoc.M{"floatingIps": n}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	svc := func(uid string, port int, keep, ip string) lbgw.Service {
		s := lbgw.Service{UID: uid, Namespace: "default", Name: uid, Ports: []lbgw.Port{{Protocol: "TCP", Port: port, NodePort: 30000 + port}},
			ExternalTrafficPolicy: "Cluster", LoadBalancerIP: ip}
		if keep != "" {
			s.Annotations = map[string]string{lbgw.OwnPrefix + lbgw.KnobKeepAddress: keep}
		}
		return s
	}
	event := func(ans *lbgw.Answer, uid, reason string) string {
		t.Helper()
		i := slices.IndexFunc(ans.Services, func(s lbgw.ServiceStatus) bool { return s.UID == uid })
		if i < 0 {
			t.Fatalf("no answer for %s: %+v", uid, ans.Services)
		}
		for _, ev := range ans.Services[i].Events {
			if ev.Reason == reason {
				return ev.Message
			}
		}
		return ""
	}
	state := func() *lbgw.State {
		t.Helper()
		st, err := store.State(ctx, "c1")
		if err != nil || st == nil {
			t.Fatalf("state: %v", err)
		}
		return st
	}

	// a asks to keep its address, b does not. Both go: a's address leaves the appliances and stays the balancer's,
	// b's goes back to the pool.
	a, b := svc("a", 80, "true", ""), svc("b", 81, "", "")
	put(a, b)
	lb := converge("a and b ready", settled(2))
	vipA, vipB := vipOf(lb, "a"), vipOf(lb, "b")
	if vipA == "" || vipB == "" || !served(vipA) || !served(vipB) {
		t.Fatalf("addresses %q %q, served %v %v", vipA, vipB, served(vipA), served(vipB))
	}
	if ans := put(a, b); event(ans, "a", lbgw.ReasonKeepAddressLimit) != "" || len(ans.Services[0].Events) != 0 {
		t.Fatalf("a Service that keeps its address is told nothing: %+v", ans.Services[0])
	}
	bill(vipA)
	bill(vipB)
	put()
	if got := state().KeepGone; !slices.Equal(got, []string{"a"}) {
		t.Fatalf("the state without a and b carries %v, want a alone", got)
	}
	lb = converge("a kept, b given back", settled(0))
	if !slices.Equal(lb.Cluster.Kept, []string{vipA}) || !slices.Equal(owned(), []string{vipA}) || len(lb.VIPs) != 0 || served(vipA) || served(vipB) {
		t.Fatalf("kept %v, owned %v, vips %v, a served %v, b served %v", lb.Cluster.Kept, owned(), lb.VIPs, served(vipA), served(vipB))
	}
	if !slices.Equal(billed(), []string{vipA}) {
		t.Fatalf("floating rows %v: the kept address keeps its row, the other's is archived", billed())
	}
	if n, _ := h.publicIPs.CountForProject(ctx, "svc", "p1"); n != 1 {
		t.Fatalf("quota usage %d, want the kept address", n)
	}
	// The converge has seen it: the next state carries it no more, and a pass over that state keeps nothing twice.
	put()
	if got := state().KeepGone; len(got) != 0 {
		t.Fatalf("the state still carries %v", got)
	}
	if lb = converge("again", settled(0)); !slices.Equal(lb.Cluster.Kept, []string{vipA}) {
		t.Fatalf("kept %v", lb.Cluster.Kept)
	}
	// What the consoles read next to the cluster names it.
	view, err := h.lbClusterView(ctx, "c1")
	if raw, _ := json.Marshal(view); err != nil || !strings.Contains(string(raw), `"kept":["`+vipA+`"]`) {
		t.Fatalf("view: %s, %v", raw, err)
	}

	// The annotation is set and the Service deleted before any pass ran in between (kubectl annotate, then kubectl
	// delete): the PUT without the Service is the last word of it, and its address is kept all the same.
	c := svc("c", 82, "", "")
	put(c)
	vipC := vipOf(converge("c ready", settled(1)), "c")
	held, ok, err := h.sdnLock.Wait(ctx, "lbc-c1", time.Minute, 10*time.Second)
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	put(svc("c", 82, "true", ""))
	put()
	put() // and a resync on top, before the pass
	held()
	if got := state().KeepGone; !slices.Equal(got, []string{"c"}) {
		t.Fatalf("the state carries %v, want c", got)
	}
	lb = converge("c kept", settled(0))
	if !slices.Equal(lb.Cluster.Kept, []string{vipA, vipC}) || !slices.Equal(owned(), []string{vipA, vipC}) || served(vipC) {
		t.Fatalf("kept %v, owned %v, c served %v", lb.Cluster.Kept, owned(), served(vipC))
	}

	// The two kept addresses fill a quota of two: a Service that names none of them gets no address. One that names
	// a kept address gets it: nothing new is allocated, and the quota is not asked.
	quota(2)
	put(svc("d", 83, "true", ""))
	lb = converge("d over the quota", settled(1))
	if d := lb.Cluster.Services[0]; d.State != lbgw.StateRefused || d.Reason != lbgw.ReasonQuotaExceeded || d.VIP != "" {
		t.Fatalf("kept addresses do not count in the quota: %+v", d)
	}
	d := svc("d", 83, "true", vipA)
	if ans := put(d); event(ans, "d", lbgw.ReasonLoadBalancerIPIgnored) != "" {
		t.Fatalf("a kept address that is named: %+v", ans.Services[0])
	}
	lb = converge("d on a's address", settled(1))
	if vipOf(lb, "d") != vipA || lb.Cluster.Services[0].State != lbgw.StateReady || !slices.Equal(lb.Cluster.Kept, []string{vipC}) ||
		!slices.Equal(owned(), []string{vipA, vipC}) || !served(vipA) {
		t.Fatalf("d: %+v, kept %v, owned %v, served %v", lb.Cluster.Services[0], lb.Cluster.Kept, owned(), served(vipA))
	}
	if ans := put(d); len(ans.Services[0].Events) != 0 || ans.Services[0].Address != vipA {
		t.Fatalf("d's answer: %+v", ans.Services[0])
	}
	quota(20)

	// Addresses that are not this cluster's to give: one another cluster keeps, one of another project's server, a
	// free one of the pool, one a Service of this cluster is on, and texts that are no IPv4 address. Each Service
	// gets an address of the pool and is told so in the same words, and nothing of the other cluster moves.
	ext2 := lbClusterExternalID("c2")
	id2 := ext2[len("lb-"):]
	other, err := h.publicIPs.Allocate(ctx, "svc", "p2", LBAddressOwner(id2), publicNIC, public)
	if err != nil {
		t.Fatal(err)
	}
	server, err := h.publicIPs.Allocate(ctx, "svc", "p2", "900", publicNIC, public)
	if err != nil {
		t.Fatal(err)
	}
	row2 := &cloud.CloudResource{ExternalID: ext2, ServiceID: "svc", ProjectID: "p2", Type: cloud.TypeLoadBalancer}
	if err := setLBRow(row2, &loadBalancer{ID: id2, Name: "k8s-" + id2, Status: lbStatusActive, NetworkID: "v2", PublicNetworkID: "vmbr1", HA: true,
		Cluster: &lbCluster{ID: "c2", Kept: []string{other.IP}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cloud.Insert(ctx, row2); err != nil {
		t.Fatal(err)
	}
	const free = "203.0.113.60"
	named := map[string]string{"e1": other.IP, "e2": server.IP, "e3": free, "e4": vipA}
	asked := []lbgw.Service{d, svc("e1", 91, "", other.IP), svc("e2", 92, "", server.IP), svc("e3", 93, "", free), svc("e4", 94, "", vipA),
		svc("e5", 95, "", "not-an-address"), svc("e6", 96, "", "2001:db8::1")}
	first := put(asked...)
	lb = converge("the named addresses", settled(7))
	ans := put(asked...)
	var said []string
	for uid, ip := range named {
		got := vipOf(lb, uid)
		if got == "" || got == ip || slices.Contains([]string{other.IP, server.IP, free}, got) {
			t.Fatalf("%s named %s and got %q", uid, ip, got)
		}
		msg := event(ans, uid, lbgw.ReasonLoadBalancerIPIgnored)
		if !strings.Contains(msg, ip) || !strings.Contains(msg, "not an address this cluster keeps") {
			t.Fatalf("%s: event %q, want the address named and that the cluster does not keep it", uid, msg)
		}
		// One Warning for one refusal: the words are the same before the Service has its address and after
		// (drill Q1-3 of v0.11.1: the second text read as if the address had been named too late).
		if was := event(first, uid, lbgw.ReasonLoadBalancerIPIgnored); was != msg {
			t.Fatalf("%s heard %q before it had its address and %q after", uid, was, msg)
		}
		said = append(said, strings.ReplaceAll(msg, ip, "<named>"))
	}
	if uniq := slices.Compact(slices.Sorted(slices.Values(said))); len(uniq) != 1 {
		t.Fatalf("the answer differs with whose the named address is: %q", said)
	}
	for _, uid := range []string{"e5", "e6"} {
		if msg := event(ans, uid, lbgw.ReasonLoadBalancerIPIgnored); vipOf(lb, uid) == "" || !strings.Contains(msg, "not an IPv4 address") ||
			strings.Contains(msg, "not-an-address") || strings.Contains(msg, "2001:db8") {
			t.Fatalf("%s: %q, event %q", uid, vipOf(lb, uid), msg)
		}
	}
	if vipOf(lb, "d") != vipA {
		t.Fatalf("d lost its address to a Service that named it: %+v", lb.Cluster.Services)
	}
	cr2, _ := h.cloud.FindByServiceIDAndExternalID(ctx, "svc", ext2)
	lb2, _ := lbOfRow(cr2)
	rows2, _ := h.publicIPs.ForServer(ctx, "svc", LBAddressOwner(id2))
	if !slices.Equal(lb2.Cluster.Kept, []string{other.IP}) || len(rows2) != 1 || rows2[0].IP != other.IP || rows2[0].ProjectID != "p2" {
		t.Fatalf("the other cluster: kept %v, allocations %+v", lb2.Cluster.Kept, rows2)
	}
	if rows, _ := h.publicIPs.ForServer(ctx, "svc", "900"); len(rows) != 1 || rows[0].IP != server.IP {
		t.Fatalf("the other project's server: %+v", rows)
	}
	// A Service that has an address keeps it, whatever it names afterwards: a kept address is taken by a Service that
	// has none yet.
	e1 := vipOf(lb, "e1")
	asked[1] = svc("e1", 91, "", vipC)
	put(asked...)
	lb = converge("e1 names a kept address late", settled(7))
	if msg := event(put(asked...), "e1", lbgw.ReasonLoadBalancerIPIgnored); vipOf(lb, "e1") != e1 || !slices.Equal(lb.Cluster.Kept, []string{vipC}) ||
		!strings.Contains(msg, "has "+e1) {
		t.Fatalf("e1 has %s (was %s), kept %v, event %q", vipOf(lb, "e1"), e1, lb.Cluster.Kept, msg)
	}

	// The cluster keeps five addresses at most. With five kept, a Service that asks to keep its own is told while it
	// stands that it will not be, and when it goes its address is given back.
	more := []string{vipC}
	for len(more) < lbgw.MaxKeptAddresses {
		ip, err := h.publicIPs.Allocate(ctx, "svc", "p1", owner, publicNIC, public)
		if err != nil {
			t.Fatal(err)
		}
		more = append(more, ip.IP)
	}
	es, err := h.esSvc.Get(ctx, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.lbEdit(ctx, es, ext, func(lb *loadBalancer) { lb.Cluster.Kept = slices.Clone(more) }); err != nil {
		t.Fatal(err)
	}
	if msg := event(put(d), "d", lbgw.ReasonKeepAddressLimit); !strings.Contains(msg, "given back") {
		t.Fatalf("d was not told that the cluster keeps five already: %q", msg)
	}
	put()
	if got := state().KeepGone; !slices.Equal(got, []string{"d"}) {
		t.Fatalf("the state carries %v", got)
	}
	lb = converge("the sixth is given back", settled(0))
	want := slices.Sorted(slices.Values(more))
	if !slices.Equal(lb.Cluster.Kept, more) || !slices.Equal(owned(), want) || served(vipA) {
		t.Fatalf("kept %v, owned %v, want %v", lb.Cluster.Kept, owned(), want)
	}

	// The release action: RELEASE_LOAD_BALANCER_ADDRESS on the cluster. It gives back an address the cluster keeps
	// and no other: not one a Service is on, not a free one, and not for another project.
	g := svc("g", 97, "", "")
	put(g)
	vipG := vipOf(converge("g ready", settled(1)), "g")
	k8s, err := h.esSvc.Get(ctx, "k8s")
	if err != nil {
		t.Fatal(err)
	}
	cluster := &cloud.CloudResource{ExternalID: "c1", ServiceID: "k8s", ProjectID: "p1", Type: cloud.TypeKubernetesCluster}
	release := func(projectID, ip string) int {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		if !h.kamajiAction(w, r, &Project{ID: projectID}, k8s, cluster, "RELEASE_LOAD_BALANCER_ADDRESS", map[string]any{"address": ip}) {
			t.Fatal("the action is not the cluster's")
		}
		return w.Code
	}
	for what, code := range map[string]int{
		"an address a Service is on":      release("p1", vipG),
		"an address the pool has":         release("p1", vipA),
		"no address":                      release("p1", ""),
		"another cluster's kept one":      release("p1", other.IP),
		"a kept one, for another project": release("p2", vipC),
	} {
		if code != http.StatusNotFound {
			t.Errorf("release of %s: %d, want 404", what, code)
		}
	}
	if lb = read(); !slices.Equal(lb.Cluster.Kept, more) || len(lb.Cluster.Releasing) != 0 || vipOf(lb, "g") != vipG {
		t.Fatalf("a refused release changed the row: %+v", lb.Cluster)
	}
	bill(vipC)
	if code := release("p1", vipC); code != http.StatusOK {
		t.Fatalf("release of a kept address: %d", code)
	}
	lb = converge("the kept address released", settled(1))
	if slices.Contains(lb.Cluster.Kept, vipC) || len(lb.Cluster.Kept) != lbgw.MaxKeptAddresses-1 || slices.Contains(owned(), vipC) ||
		slices.Contains(billed(), vipC) || vipOf(lb, "g") != vipG || !served(vipG) {
		t.Fatalf("after the release: kept %v, owned %v, floating rows %v, g %s", lb.Cluster.Kept, owned(), billed(), vipOf(lb, "g"))
	}
	if code := release("p1", vipC); code != http.StatusNotFound {
		t.Fatalf("a second release of the same address: %d", code)
	}

	// The cluster's release gives back what it kept, with the rest.
	f.mu.Lock()
	f.gone = true
	f.mu.Unlock()
	deadline := time.Now().Add(30 * time.Second)
	for err = h.LBReleaseCluster(ctx, "svc", "p1", "c1"); err != nil && time.Now().Before(deadline); err = h.LBReleaseCluster(ctx, "svc", "p1", "c1") {
		time.Sleep(200 * time.Millisecond) // a pass a PUT started still holds the cluster
	}
	if err != nil {
		t.Fatal(err)
	}
	if cr, _ := h.cloud.FindByServiceIDAndExternalID(ctx, "svc", ext); cr != nil || len(owned()) != 0 || len(billed()) != 0 {
		t.Fatalf("left after the cluster's release: row %v, addresses %v, floating rows %v", cr != nil, owned(), billed())
	}
	if rows2, _ := h.publicIPs.ForServer(ctx, "svc", LBAddressOwner(id2)); len(rows2) != 1 {
		t.Fatalf("the other cluster's kept address went with this one: %+v", rows2)
	}
}

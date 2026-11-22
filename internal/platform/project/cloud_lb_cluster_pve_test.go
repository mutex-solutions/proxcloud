package project

import (
	"crypto/sha256"
	"encoding/hex"
	"math/rand/v2"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/csigw"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/lbgw"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// The workers' part of a reservation is what ClusterInfra.Addresses got at create: the first kamajiWorkerAddrs that
// Reserve handed out. The MetalLB part is never a member (C11), in whatever order the rows come back.
func TestKamajiWorkerPart(t *testing.T) {
	pn := externalservice.PVEPublicNetwork{ID: "v1", CIDR: "10.0.0.0/24", Gateway: "10.0.0.1", Ranges: []string{"10.0.0.2-10.0.0.254"}}
	reserved, _, herr := freeAddresses(pn, []string{"10.0.0.254", "10.0.0.200"}, kamajiWorkerAddrs+kamajiLBAddrs)
	if herr != nil {
		t.Fatal(herr)
	}
	var rows []PublicIP
	for _, ip := range reserved {
		rows = append(rows, PublicIP{IP: ip, NIC: ReservedNIC, ServerID: "k8s:c1"})
	}
	rand.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
	got := kamajiWorkerPart(rows)
	if !slices.Equal(got, reserved[:kamajiWorkerAddrs]) {
		t.Fatalf("workers %v, want %v", got, reserved[:kamajiWorkerAddrs])
	}
	for _, metallb := range reserved[kamajiWorkerAddrs:] {
		if slices.Contains(got, metallb) {
			t.Fatalf("MetalLB address %s counted as a worker", metallb)
		}
	}
}

// A tenant's edit or delete of a cluster's balancer answers 409; its own balancers are untouched by the guard.
func TestLBClusterGuard(t *testing.T) {
	row := func(lb *loadBalancer) *cloud.CloudResource {
		cr := &cloud.CloudResource{Type: cloud.TypeLoadBalancer}
		if err := setLBRow(cr, lb); err != nil {
			t.Fatal(err)
		}
		return cr
	}
	if herr := lbClusterGuard(row(&loadBalancer{ID: "abc", HA: true, Cluster: &lbCluster{ID: "c1"}})); herr == nil || herr.Status != http.StatusConflict {
		t.Fatalf("cluster balancer: %v", herr)
	}
	if herr := lbClusterGuard(row(&loadBalancer{ID: "abc", HA: true})); herr != nil {
		t.Fatalf("tenant balancer: %v", herr)
	}
	id := lbClusterExternalID("c1")
	if id != lbClusterExternalID("c1") || id == lbClusterExternalID("c2") || len(id) != len("lb-")+12 || !pve.LBNameOK("k8s-"+id[3:]) {
		t.Fatalf("external id %q", id)
	}
	if _, err := lbOfRow(row(&loadBalancer{ID: id[3:]})); err != nil {
		t.Fatal(err)
	}
}

// One pool per Service port of the workers' node port in mode tcp, one listener on the Service's own address; policy
// Local is checked over HTTP on healthCheckNodePort.
func TestLBClusterServiceSpec(t *testing.T) {
	w := lbgw.Service{UID: "u", Ports: []lbgw.Port{{Port: 80, NodePort: 30080}, {Port: 443, NodePort: 30443}},
		ExternalTrafficPolicy: "Local", HealthCheckNodePort: 32000, ProxyProtocol: true, SourceRanges: []string{"198.51.100.0/24"}}
	spec := lbClusterServiceSpec(pve.LBSpec{Pools: []pve.LBPool{{Name: "s0-0"}}}, w, "203.0.113.10", []string{"10.0.0.250", "10.0.0.249"})
	if len(spec.Pools) != 3 || len(spec.Listeners) != 2 {
		t.Fatalf("%+v", spec)
	}
	p := spec.Pools[2]
	if !pve.LBNameOK(p.Name) || !strings.HasSuffix(p.Name, "-30443") || p.Name == spec.Pools[1].Name || p.Protocol != "tcp" || !p.ProxyProtocol || p.HealthCheck == nil || p.HealthCheck.Port != 32000 ||
		len(p.Members) != 2 || !p.Members[0].Node || p.Members[0].Port != 30443 || p.Members[1].Address != "10.0.0.249" {
		t.Fatalf("pool %+v", p)
	}
	if l := spec.Listeners[1]; l.VIP != "203.0.113.10" || l.Port != 443 || l.Pool != p.Name || l.SourceRanges[0] != "198.51.100.0/24" {
		t.Fatalf("listener %+v", l)
	}
	w.ExternalTrafficPolicy = "Cluster"
	if spec := lbClusterServiceSpec(pve.LBSpec{}, w, "203.0.113.10", nil); spec.Pools[0].HealthCheck != nil {
		t.Fatal("policy Cluster takes the TCP check on the node port")
	}
	// A pool is named by its Service and its node port, not by its place: the same pool whatever comes before it and
	// whichever of the Service's ports it is, and another Service's pool on another name (drill N3-8).
	other := lbgw.Service{UID: "v", Ports: []lbgw.Port{{Port: 80, NodePort: 31080}}}
	alone := lbClusterServiceSpec(pve.LBSpec{}, lbgw.Service{UID: "u", Ports: w.Ports[1:]}, "203.0.113.10", nil).Pools[0].Name
	after := lbClusterServiceSpec(lbClusterServiceSpec(pve.LBSpec{}, other, "203.0.113.11", nil), w, "203.0.113.10", nil)
	if alone != p.Name || after.Pools[2].Name != p.Name || after.Pools[0].Name == after.Pools[1].Name {
		t.Fatalf("pool names move: %q alone, %q after another Service, was %q", alone, after.Pools[2].Name, p.Name)
	}
}

// A Service as a controller of v0.11.0 sends it makes the spec it made before protocol 2, field for field: its
// balancer's config, and so the hash every appliance is compared with, does not change with the portal's upgrade.
func TestLBClusterServiceSpecOfProtocol1IsUnchanged(t *testing.T) {
	w := lbgw.Service{UID: "u", Namespace: "web", Name: "shop", Ports: []lbgw.Port{{Name: "http", Protocol: "TCP", Port: 80, NodePort: 30080}},
		ExternalTrafficPolicy: "Local", HealthCheckNodePort: 32000, ProxyProtocol: true, SourceRanges: []string{"198.51.100.0/24"}, IPFamilies: []string{"IPv4"}}
	nodes := []string{"10.0.0.250", "10.0.0.249"}
	uid := sha256.Sum256([]byte("u"))
	// What v0.11.0's lbClusterServiceSpec wrote, copied from it.
	was := func(w lbgw.Service) pve.LBSpec {
		var out pve.LBSpec
		for _, p := range w.Ports {
			pool := pve.LBPool{Name: "s" + hex.EncodeToString(uid[:4]) + "-" + strconv.Itoa(p.NodePort), Protocol: "tcp", Algorithm: "roundrobin", ProxyProtocol: w.ProxyProtocol}
			if w.ExternalTrafficPolicy == "Local" {
				pool.HealthCheck = &pve.LBHealthCheck{Type: "http", Port: w.HealthCheckNodePort, Path: "/"}
			}
			for _, n := range nodes {
				pool.Members = append(pool.Members, pve.LBMember{Node: true, Address: n, Port: p.NodePort, Weight: 1})
			}
			out.Pools = append(out.Pools, pool)
			out.Listeners = append(out.Listeners, pve.LBListener{Port: p.Port, Pool: pool.Name, SourceRanges: w.SourceRanges, VIP: "203.0.113.10"})
		}
		return out
	}
	for name, change := range map[string]func(*lbgw.Service){
		"local with PROXY": func(*lbgw.Service) {},
		"cluster, plain": func(w *lbgw.Service) {
			w.ExternalTrafficPolicy, w.HealthCheckNodePort, w.ProxyProtocol = "Cluster", 0, false
		},
	} {
		w := w
		change(&w)
		if got, want := lbClusterServiceSpec(pve.LBSpec{}, w, "203.0.113.10", nodes), was(w); !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, want)
		}
	}
}

// k8sServices are Services as a protocol 2 controller sends them, after the gateway's checks: a DNS server on TCP 53
// and UDP 53 (one node port for both, as Kubernetes allocates it) with affinity, a selector and policy Local; a game
// server on UDP with every figure an annotation can set; a web Service with the forwarded-for header, a method, a
// limit and the three timeouts by OCCM's names; and one with PROXY version 1 and no health check.
func k8sServices() []lbgw.Service {
	return []lbgw.Service{
		{UID: "dns", Ports: []lbgw.Port{{Protocol: "TCP", Port: 53, NodePort: 30053}, {Protocol: "UDP", Port: 53, NodePort: 30053}},
			ExternalTrafficPolicy: "Local", HealthCheckNodePort: 32000, SessionAffinity: "ClientIP", SessionAffinityTimeout: 600,
			Nodes: &[]string{"10.0.0.249", "10.9.9.9"}, SourceRanges: []string{"198.51.100.0/24"}},
		{UID: "game", Ports: []lbgw.Port{{Protocol: "UDP", Port: 27015, NodePort: 31015}}, ExternalTrafficPolicy: "Cluster",
			Annotations: map[string]string{"proxcloud.io/lb-method": "source", "proxcloud.io/lb-connection-limit": "1000",
				"proxcloud.io/lb-health-monitor-delay": "10", "proxcloud.io/lb-health-monitor-timeout": "4", "proxcloud.io/lb-health-monitor-max-retries": "3",
				"proxcloud.io/lb-health-monitor-max-retries-down": "1", "proxcloud.io/lb-proxy-protocol": "v2", "proxcloud.io/lb-timeout-client-data": "30000"}},
		{UID: "web", Ports: []lbgw.Port{{Protocol: "TCP", Port: 80, NodePort: 30080}}, ExternalTrafficPolicy: "Cluster",
			Annotations: map[string]string{"loadbalancer.openstack.org/x-forwarded-for": "true", "loadbalancer.openstack.org/lb-method": "LEAST_CONNECTIONS",
				"loadbalancer.openstack.org/connection-limit": "2000", "loadbalancer.openstack.org/timeout-client-data": "50000",
				"loadbalancer.openstack.org/timeout-member-connect": "5000", "loadbalancer.openstack.org/timeout-member-data": "50000",
				"loadbalancer.openstack.org/health-monitor-delay": "5", "loadbalancer.openstack.org/health-monitor-timeout": "3",
				"loadbalancer.openstack.org/health-monitor-max-retries": "1", "loadbalancer.openstack.org/health-monitor-max-retries-down": "3"}},
		{UID: "mail", Ports: []lbgw.Port{{Protocol: "TCP", Port: 25, NodePort: 30025}}, ExternalTrafficPolicy: "Cluster",
			Annotations: map[string]string{"loadbalancer.openstack.org/proxy-protocol": "true", "proxcloud.io/lb-enable-health-monitor": "false"}},
	}
}

// What the Services of k8sServices make of a cluster's balancer, as the two files an appliance gets. The files are
// goldens beside the renderers' own (internal/cloud/pve/testdata), where the container tests hand every one of them
// to the HAProxy and the keepalived of both appliance images (test/integration/lb_keepalived_test.go).
func TestLBClusterServicesOfProtocol2(t *testing.T) {
	vips := []string{"203.0.113.10", "203.0.113.11", "203.0.113.12", "203.0.113.13"}
	var spec pve.LBSpec
	for i, w := range k8sServices() {
		if _, reason, msg := lbgw.CheckService(&w, i, lbgw.Protocol); reason != "" {
			t.Fatalf("%s is refused: %s %s", w.UID, reason, msg)
		}
		spec = lbClusterServiceSpec(spec, w, vips[i], []string{"10.0.0.250", "10.0.0.249"})
	}
	for name, got := range map[string]string{"lb-k8s-services.cfg": pve.RenderClusterHAProxy(spec, vips), "lvs-k8s-services.conf": pve.RenderLVS(spec, vips)} {
		path := "../../cloud/pve/testdata/" + name
		if os.Getenv("UPDATE_GOLDEN") != "" {
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s differs:\n--- got\n%s\n--- want\n%s", name, got, want)
		}
	}
	// A spec the tenant API would take too: the mapping writes nothing a validation refuses. Validate clears what
	// only the platform sets, so it runs on a copy with members a tenant may name.
	check := pve.LBSpec{Listeners: slices.Clone(spec.Listeners)}
	for _, p := range spec.Pools {
		p.Members = nil
		if p.HealthCheck != nil {
			hc := *p.HealthCheck
			p.HealthCheck = &hc
		}
		check.Pools = append(check.Pools, p)
	}
	for i := range check.Listeners {
		check.Listeners[i].Port = 1000 + i // two Services may share a port, each on its own address
	}
	if msg := check.Validate(); msg != "" {
		t.Fatalf("the spec of the Services does not pass the balancer's own validation: %s", msg)
	}

	pool := func(suffix string) pve.LBPool {
		t.Helper()
		i := slices.IndexFunc(spec.Pools, func(p pve.LBPool) bool { return strings.HasSuffix(p.Name, suffix) })
		if i < 0 {
			t.Fatalf("no pool %s in %+v", suffix, spec.Pools)
		}
		return spec.Pools[i]
	}
	// TCP 53 and UDP 53: two listeners on one address and port, two pools on one node port, both checked on the
	// health check node port, both with the affinity, and only the selected node that is a worker behind them.
	tcp, udp := pool("-30053"), pool("-30053-udp")
	for _, p := range []pve.LBPool{tcp, udp} {
		if p.HealthCheck == nil || p.HealthCheck.Type != "http" || p.HealthCheck.Port != 32000 || p.SessionPersistence == nil || p.SessionPersistence.Timeout != 600 ||
			len(p.Members) != 1 || p.Members[0].Address != "10.0.0.249" {
			t.Errorf("pool %s: %+v", p.Name, p)
		}
	}
	if tcp.Protocol != "tcp" || udp.Protocol != "udp" || spec.Listeners[0].Protocol != "" || spec.Listeners[1].Protocol != "udp" ||
		spec.Listeners[0].VIP != spec.Listeners[1].VIP || spec.Listeners[0].Port != spec.Listeners[1].Port {
		t.Errorf("the two listeners of port 53: %+v %+v", spec.Listeners[0], spec.Listeners[1])
	}
	rules := pve.LBListenerRules(spec.Listeners[:2], vips)
	if len(rules) != 2 || rules[0].Proto != "tcp" || rules[1].Proto != "udp" || rules[0].Dest != "203.0.113.10" || rules[1].DPort != "53" || rules[1].Source != "198.51.100.0/24" {
		t.Errorf("firewall rules of port 53: %+v", rules)
	}
	// A UDP port takes what UDP has: no PROXY header, no timeout, no rise, a fall of 2 at least.
	game := pool("-31015-udp")
	if game.ProxyProtocol || game.TimeoutMemberData != 0 || game.Algorithm != "source" || game.HealthCheck == nil ||
		*game.HealthCheck != (pve.LBHealthCheck{Type: "udp-connect", Interval: 10, Timeout: 4, Fall: 2}) {
		t.Errorf("game: %+v %+v", game, game.HealthCheck)
	}
	web, mail := pool("-30080"), pool("-30025")
	if web.Protocol != "http" || web.Algorithm != "leastconn" || *web.HealthCheck != (pve.LBHealthCheck{Type: "tcp", Interval: 5, Timeout: 3, Rise: 1, Fall: 3}) ||
		web.TimeoutMemberConnect != 5000 || web.TimeoutMemberData != 50000 {
		t.Errorf("web: %+v %+v", web, web.HealthCheck)
	}
	if !mail.ProxyProtocol || mail.ProxyVersion != 1 || mail.HealthCheck == nil || mail.HealthCheck.Type != "none" {
		t.Errorf("mail: %+v %+v", mail, mail.HealthCheck)
	}
	// A selector that matches no worker leaves a Service without a member, and still with its listener.
	none := lbClusterServiceSpec(pve.LBSpec{}, lbgw.Service{UID: "x", Ports: []lbgw.Port{{Port: 80, NodePort: 30080}}, Nodes: &[]string{}}, "203.0.113.10", []string{"10.0.0.250"})
	if len(none.Listeners) != 1 || len(none.Pools[0].Members) != 0 {
		t.Errorf("an empty selection: %+v", none)
	}
}

// A Compose site with the gateway listener (an endpoint) but no gateway token on its compute provider: a new cluster
// gets no CSI gateway, and so no StorageClass.
func TestComposeListenerWithoutTokenGetsNoStorageClass(t *testing.T) {
	h := &Handler{csiGW: &CSIGateway{Endpoint: csigw.Endpoint{URL: "https://192.0.2.7:8444", CA: "CA"}}}
	compute := &externalservice.ExternalService{Config: map[string]any{}}
	gateway, err := h.kamajiGatewayCSI(kamaji.Config{ChartVersion: "0.1.16"}, compute)
	if err != nil || gateway {
		t.Fatalf("gateway %v %v", gateway, err)
	}
	if classes := kamajiTenantClasses(gateway, kamaji.ClusterDefaults{}, []kamaji.StorageClass{{Name: "local", Storage: "local-lvm"}}); len(classes) != 0 {
		t.Fatalf("classes %v", classes)
	}
}

// A cluster gets the load balancer controller only when every gate holds, and the first one that does not is named:
// that text is what the operator's "Enable load balancer controller" answers, and a create without one is as before.
func TestKamajiLBGate(t *testing.T) {
	k8s := &externalservice.ExternalService{ID: "k8s", Config: map[string]any{"cluster": map[string]any{
		"loadBalancer": map[string]any{"publicNetworkId": "vmbr1", "flavorId": "lb-small"}}}}
	noLB := &externalservice.ExternalService{ID: "k8s-off", Config: map[string]any{"cluster": map[string]any{}}}
	compute := &externalservice.ExternalService{ID: "pve", Config: map[string]any{"network": map[string]any{"publicNetworks": []any{
		map[string]any{"id": "vmbr1", "cidr": "203.0.113.0/24", "gateway": "203.0.113.1", "ranges": []any{"203.0.113.10-203.0.113.20"}}}}}}
	gw := &CSIGateway{Endpoint: csigw.Endpoint{URL: "https://192.0.2.7:8444", CA: "CA"}}
	cfg := func(chart, image string, d kamaji.ClusterDefaults) kamaji.Config {
		return kamaji.Config{ChartVersion: chart, LBControllerImage: image, Defaults: d}
	}
	ok := cfg("0.1.17", "ghcr.io/mutex-solutions/proxcloud-api:v0.11.0", kamaji.ClusterDefaults{})
	for name, tc := range map[string]struct {
		h    *Handler
		cfg  kamaji.Config
		k8s  *externalservice.ExternalService
		proj *Project
		want string // a part of the reason; "" = every gate holds
	}{
		"every gate":       {&Handler{lbGW: &lbgw.Store{}, csiGW: gw}, ok, k8s, &Project{ID: "p1"}, ""},
		"project allows":   {&Handler{lbGW: &lbgw.Store{}, csiGW: gw}, ok, k8s, &Project{ID: "p1", PublicNetworkIds: []string{"vmbr1"}}, ""},
		"no store":         {&Handler{csiGW: gw}, ok, k8s, &Project{ID: "p1"}, "store"},
		"no gateway URL":   {&Handler{lbGW: &lbgw.Store{}, csiGW: &CSIGateway{}}, ok, k8s, &Project{ID: "p1"}, "PROXCLOUD_CSI_GATEWAY_URL"},
		"no image":         {&Handler{lbGW: &lbgw.Store{}, csiGW: gw}, cfg("0.1.17", "", kamaji.ClusterDefaults{}), k8s, &Project{ID: "p1"}, "PROXCLOUD_LB_CONTROLLER_IMAGE"},
		"shared workers":   {&Handler{lbGW: &lbgw.Store{}, csiGW: gw}, cfg("0.1.17", "i", kamaji.ClusterDefaults{Shared: &kamaji.SharedWorkers{Pool: "p"}}), k8s, &Project{ID: "p1"}, "sharedWorkers"},
		"chart 0.1.16":     {&Handler{lbGW: &lbgw.Store{}, csiGW: gw}, cfg("0.1.16", "i", kamaji.ClusterDefaults{}), k8s, &Project{ID: "p1"}, "0.1.17 or later"},
		"no provider lb":   {&Handler{lbGW: &lbgw.Store{}, csiGW: gw}, ok, noLB, &Project{ID: "p1"}, "config.cluster.loadBalancer"},
		"network refused":  {&Handler{lbGW: &lbgw.Store{}, csiGW: gw}, ok, k8s, &Project{ID: "p1", PublicNetworkIds: []string{}}, "vmbr1 is not available"},
		"drill chart tag":  {&Handler{lbGW: &lbgw.Store{}, csiGW: gw}, cfg("0.1.17-lb.1", "i", kamaji.ClusterDefaults{}), k8s, &Project{ID: "p1"}, ""},
		"malformed pin":    {&Handler{lbGW: &lbgw.Store{}, csiGW: gw}, cfg("latest", "i", kamaji.ClusterDefaults{}), k8s, &Project{ID: "p1"}, "or later"},
		"no CSI component": {&Handler{lbGW: &lbgw.Store{}, csiGW: nil}, ok, k8s, &Project{ID: "p1"}, "PROXCLOUD_CSI_GATEWAY_URL"},
	} {
		got := tc.h.kamajiLBGate(tc.cfg, tc.k8s, compute, tc.proj)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: gate = %q, want %q", name, got, tc.want)
		}
	}
}

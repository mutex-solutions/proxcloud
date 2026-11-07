package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestOwnershipAlias(t *testing.T) {
	a := OwnershipAlias("p1", "web-net")
	pid, name, ok := aliasProject(a)
	if !ok || pid != "p1" || name != "web-net" {
		t.Fatalf("round trip: %v %v %v", pid, name, ok)
	}
	if _, _, ok := aliasProject("vmbr0"); ok {
		t.Fatal("foreign alias must not parse")
	}
	if _, _, ok := aliasProject("st:broken"); ok {
		t.Fatal("malformed alias must not parse")
	}
}

func TestFirstHostAndDHCPRange(t *testing.T) {
	_, ipnet, _ := net.ParseCIDR("10.100.5.0/24")
	if gw := firstHost(ipnet); gw != "10.100.5.1" {
		t.Fatalf("gw = %s", gw)
	}
	start, end := dhcpRange(ipnet, "10.100.5.1")
	if start != "10.100.5.10" || end != "10.100.5.254" {
		t.Fatalf("range = %s..%s", start, end)
	}
}

func TestCreateNetworkStagesVnetAndSubnet(t *testing.T) {
	var gotVnet map[string]string
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/sdn/vnets": func(w http.ResponseWriter, r *http.Request) {
			// The VNI max spans zones: a per-project EVPN zone's vnet counts too.
			fmt.Fprint(w, `{"data":[{"vnet":"vabc12","zone":"overlay","tag":100005,"alias":"st:p2:other"},{"vnet":"vdef34","zone":"pzzzzzz1","tag":100009,"alias":"st:p3:x"}]}`)
		},
		"POST /api2/json/cluster/sdn/vnets": func(w http.ResponseWriter, r *http.Request) {
			r.ParseForm()
			gotVnet = map[string]string{}
			for k := range r.PostForm {
				gotVnet[k] = r.PostFormValue(k)
			}
			fmt.Fprint(w, `{"data":null}`)
		},
	})
	srv := &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}
	// Register subnet handler via a wrapping fake is awkward — instead assert via
	// the vnet form and let subnet POST 500 (fake has no route) to exercise rollback.
	_, err := srv.CreateNetwork(context.Background(), CreateNetworkReq{
		ProjectID: "p1", Name: "web", Zone: ZoneConfig{Key: "overlay", Kind: "evpn"},
		CIDR: "10.100.0.0/24", DHCP: true, SNAT: true,
	})
	if err == nil {
		t.Fatal("expected subnet-create failure from the fake")
	}
	if gotVnet["zone"] != "overlay" || gotVnet["alias"] != "st:p1:web" {
		t.Fatalf("vnet form = %v", gotVnet)
	}
	if gotVnet["tag"] != "100010" { // cluster-wide maxTag 100009 + 1
		t.Fatalf("tag = %v", gotVnet["tag"])
	}
}

func TestNetworkSyncProviderFiltersByAlias(t *testing.T) {
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/sdn/vnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[
				{"vnet":"vaaa11","zone":"overlay","alias":"st:p1:web"},
				{"vnet":"vbbb22","zone":"overlay","alias":"st:p2:other"},
				{"vnet":"vmgmt","zone":"overlay","alias":"ops managed"}
			]}`)
		},
		"GET /api2/json/cluster/sdn/vnets/vaaa11/subnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"subnet":"overlay-10.100.0.0-24","cidr":"10.100.0.0/24","gateway":"10.100.0.1","snat":1,"dhcp-range":[{"start-address":"10.100.0.10","end-address":"10.100.0.254"}]}]}`)
		},
	})
	srv := &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}
	rs, err := srv.NetworkSyncProvider("pve", "p1").List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].ExternalID != "vaaa11" {
		t.Fatalf("rows = %+v", rs)
	}
	netMap := rs[0].Data["network"].(map[string]any)
	if netMap["name"] != "web" || netMap["cidr"] != "10.100.0.0/24" || netMap["snat"] != true || netMap["dhcp"] != true {
		t.Fatalf("network = %v", netMap)
	}
}

func TestEnsureZoneIdempotent(t *testing.T) {
	created := 0
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/sdn/zones": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"zone":"overlay","type":"evpn"}]}`)
		},
		"POST /api2/json/cluster/sdn/zones": func(w http.ResponseWriter, r *http.Request) {
			created++
			fmt.Fprint(w, `{"data":null}`)
		},
	})
	srv := &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}
	if err := srv.EnsureZone(context.Background(), ZoneConfig{Key: "overlay", Kind: "evpn"}); err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatal("existing zone must not be re-created")
	}
}

func TestZoneIDFor(t *testing.T) {
	re := regexp.MustCompile(`^p[0-9a-z]{7}$`)
	seen := map[string]string{}
	for _, pid := range []string{"6a46b1464d44db697643c5f7", "6a46b1464d44db697643c5f8", "p1", ""} {
		id := ZoneIDFor(pid)
		if !re.MatchString(id) {
			t.Fatalf("ZoneIDFor(%q) = %q, want p+7 base36 (8 chars)", pid, id)
		}
		if ZoneIDFor(pid) != id {
			t.Fatalf("ZoneIDFor(%q) not deterministic", pid)
		}
		if prev, dup := seen[id]; dup {
			t.Fatalf("ZoneIDFor collision %q/%q -> %s", prev, pid, id)
		}
		seen[id] = pid
	}
}

func TestIsolationRules(t *testing.T) {
	const pool, comment = "10.100.0.0/16", isolationComment
	if got, want := isolationRules(pool, nil, nil, comment), []FirewallRule{
		{Type: "forward", Action: "DROP", Source: pool, Dest: pool, Enable: 1, Comment: comment},
		{Type: "in", Action: "DROP", Source: pool, Dest: pool, Enable: 1, Comment: comment},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("no blocked: got %+v", got)
	}
	got := isolationRules(pool, []string{"192.0.2.0/24", "198.51.100.0/24"}, nil, comment)
	want := []FirewallRule{
		{Type: "forward", Action: "DROP", Source: pool, Dest: pool, Enable: 1, Comment: comment},
		{Type: "in", Action: "DROP", Source: pool, Dest: pool, Enable: 1, Comment: comment},
		{Type: "forward", Action: "DROP", Source: pool, Dest: "192.0.2.0/24,198.51.100.0/24", Enable: 1, Comment: comment},
		{Type: "in", Action: "DROP", Source: pool, Dest: "192.0.2.0/24,198.51.100.0/24", Enable: 1, Comment: comment},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// The sandbox networks are dropped both ways: tenants to the workers (kubelet, VXLAN), and the
// workers to the tenants and the management networks they would otherwise reach through an exit
// node's SNAT.
func TestIsolationRulesSandbox(t *testing.T) {
	const pool, comment = "10.100.0.0/16", isolationComment
	got := isolationRules(pool, []string{"192.0.2.0/24"}, []string{"10.110.0.0/24", "10.111.0.0/24"}, comment)
	want := []FirewallRule{
		{Type: "forward", Action: "DROP", Source: pool, Dest: pool, Enable: 1, Comment: comment},
		{Type: "in", Action: "DROP", Source: pool, Dest: pool, Enable: 1, Comment: comment},
		{Type: "forward", Action: "DROP", Source: pool, Dest: "192.0.2.0/24,10.110.0.0/24,10.111.0.0/24", Enable: 1, Comment: comment},
		{Type: "in", Action: "DROP", Source: pool, Dest: "192.0.2.0/24,10.110.0.0/24,10.111.0.0/24", Enable: 1, Comment: comment},
		{Type: "forward", Action: "DROP", Source: "10.110.0.0/24,10.111.0.0/24", Dest: pool + ",192.0.2.0/24", Enable: 1, Comment: comment},
		{Type: "in", Action: "DROP", Source: "10.110.0.0/24,10.111.0.0/24", Dest: pool + ",192.0.2.0/24", Enable: 1, Comment: comment},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	// Nothing blocked but a sandbox: still both directions.
	got = isolationRules(pool, nil, []string{"10.110.0.0/24"}, comment)
	if len(got) != 6 || got[2].Dest != "10.110.0.0/24" || got[4].Source != "10.110.0.0/24" || got[4].Dest != pool {
		t.Fatalf("sandbox without blocked: %+v", got)
	}
}

func TestReconcileIsolationSandbox(t *testing.T) {
	srv, rules, _ := isolationFake(t, nil)
	if err := srv.ReconcileIsolation(context.Background(), "p1", "10.100.0.0/16", nil, []string{"10.110.0.5/24", "10.110.0.0/24"}); err != nil {
		t.Fatal(err)
	}
	var drops []string
	for _, r := range *rules {
		if r.Action == "DROP" {
			drops = append(drops, r.Type+" "+r.Source+" "+r.Dest)
		}
	}
	want := []string{
		"forward 10.100.0.0/16 10.100.0.0/16", "in 10.100.0.0/16 10.100.0.0/16",
		"forward 10.100.0.0/16 192.0.2.0/24,10.110.0.0/24", "in 10.100.0.0/16 192.0.2.0/24,10.110.0.0/24",
		"forward 10.110.0.0/24 10.100.0.0/16,192.0.2.0/24", "in 10.110.0.0/24 10.100.0.0/16,192.0.2.0/24",
	}
	if !reflect.DeepEqual(drops, want) {
		t.Fatalf("drops:\n%s\nwant:\n%s", strings.Join(drops, "\n"), strings.Join(want, "\n"))
	}
	// The fake's management network is 192.0.2.0/24; 198.51.100.0/24 is an operator-blocked one.
	for _, bad := range [][]string{{"10.100.7.0/24"}, {"10.0.0.0/8"}, {"not-a-cidr"}, {"fd00::/64"},
		{"192.0.0.0/16"}, {"192.0.2.128/25"}, {"198.51.100.0/24"}} {
		srv, rules, writes := isolationFake(t, nil)
		if err := srv.ReconcileIsolation(context.Background(), "p1", "10.100.0.0/16", []string{"198.51.100.0/24"}, bad); err == nil {
			t.Fatalf("sandboxCidrs %v accepted", bad)
		}
		if *writes != 0 || len(*rules) != 0 {
			t.Fatalf("sandboxCidrs %v: %d writes before the refusal", bad, *writes)
		}
	}
}

// isolationFake is a cluster with two vnets of p1, one of p2 and a firewall rule list
// behaving like PVE's (a new rule lands on top, positions renumber); the management
// network comes from the node's bridge.
func isolationFake(t *testing.T, rules []FirewallRule) (*Service, *[]FirewallRule, *int) {
	return isolationFakeNodes(t, rules, nil)
}

// isolationFakeNodes adds cluster members and overrides routes.
func isolationFakeNodes(t *testing.T, rules []FirewallRule, extra map[string]func(http.ResponseWriter, *http.Request), members ...string) (*Service, *[]FirewallRule, *int) {
	writes := 0
	renumber := func() {
		for i := range rules {
			rules[i].Pos = i
		}
	}
	renumber()
	routes := map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/status": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[`+strings.Join(append([]string{`{"type":"node","name":"pve1","ip":"192.0.2.11"}`}, members...), ",")+`]}`)
		},
		"GET /api2/json/nodes/pve1/network": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"iface":"vmbr0","address":"192.0.2.11","gateway":"192.0.2.1","cidr":"192.0.2.11/24"},
				{"iface":"vmbr1","cidr":"203.0.113.5/24"}]}`)
		},
		"GET /api2/json/cluster/sdn/vnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[
				{"vnet":"vaaa11","zone":"overlay","alias":"st:p1:web"},
				{"vnet":"vbbb22","zone":"overlay","alias":"st:p1:db"},
				{"vnet":"vccc33","zone":"overlay","alias":"st:p2:other"}
			]}`)
		},
		"GET /api2/json/cluster/sdn/vnets/vaaa11/subnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"cidr":"10.100.1.0/24"}]}`)
		},
		"GET /api2/json/cluster/sdn/vnets/vbbb22/subnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"cidr":"10.100.0.0/24"}]}`)
		},
		"GET /api2/json/cluster/firewall/rules": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"data": rules})
		},
		"POST /api2/json/cluster/firewall/rules": func(w http.ResponseWriter, r *http.Request) {
			r.ParseForm()
			writes++
			rules = append([]FirewallRule{{
				Type: r.PostFormValue("type"), Action: r.PostFormValue("action"), Source: r.PostFormValue("source"),
				Dest: r.PostFormValue("dest"), Comment: r.PostFormValue("comment"), Enable: map[string]int{"1": 1}[r.PostFormValue("enable")],
			}}, rules...)
			renumber()
			fmt.Fprint(w, `{"data":null}`)
		},
	}
	for pos := 0; pos < 12; pos++ {
		routes["DELETE /api2/json/cluster/firewall/rules/"+strconv.Itoa(pos)] = func(w http.ResponseWriter, r *http.Request) {
			writes++
			rules = slices.Delete(rules, pos, pos+1)
			renumber()
			fmt.Fprint(w, `{"data":null}`)
		}
	}
	maps.Copy(routes, extra)
	_, c := fakePVE(t, routes)
	return &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}, &rules, &writes
}

func TestReconcileIsolationBuildsAndKeepsOrder(t *testing.T) {
	ops := FirewallRule{Type: "in", Action: "ACCEPT", Dest: "10.9.9.9", Comment: "ops"}
	other := projectIsolationRules("p2", []string{"10.100.5.0/24"})[0]
	srv, rules, _ := isolationFake(t, []FirewallRule{ops, other})
	if err := srv.ReconcileIsolation(context.Background(), "p1", "10.100.0.0/16", []string{"198.51.100.0/24"}, nil); err != nil {
		t.Fatal(err)
	}
	const blocked = "192.0.2.0/24,198.51.100.0/24"
	var got []string
	for _, r := range *rules {
		got = append(got, r.Type+" "+r.Action+" "+r.Source+" "+r.Dest+" "+r.Comment)
	}
	own := "10.100.0.0/24,10.100.1.0/24"
	want := []string{
		"in ACCEPT " + own + " " + own + " proxcloud:isolation:p1",
		"forward ACCEPT " + own + " " + own + " proxcloud:isolation:p1",
		"forward ACCEPT 10.100.5.0/24 10.100.5.0/24 proxcloud:isolation:p2",
		"forward DROP 10.100.0.0/16 10.100.0.0/16 proxcloud:isolation",
		"in DROP 10.100.0.0/16 10.100.0.0/16 proxcloud:isolation",
		"forward DROP 10.100.0.0/16 " + blocked + " proxcloud:isolation",
		"in DROP 10.100.0.0/16 " + blocked + " proxcloud:isolation",
		"in ACCEPT  10.9.9.9 ops",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestReconcileIsolationIdempotent(t *testing.T) {
	own := "10.100.0.0/24,10.100.1.0/24"
	current := append(projectIsolationRules("p1", strings.Split(own, ",")),
		isolationRules("10.100.0.0/16", []string{"192.0.2.0/24"}, nil, isolationComment)...)
	srv, _, writes := isolationFake(t, current)
	if err := srv.ReconcileIsolation(context.Background(), "p1", "10.100.0.0/16", nil, nil); err != nil {
		t.Fatal(err)
	}
	if *writes != 0 {
		t.Fatalf("%d writes on an up-to-date rule set", *writes)
	}
}

// A rule disabled by hand (enable 0) counts as missing: the next reconcile puts every rule back enabled, in order.
func TestReconcileIsolationRepairsDisabledRules(t *testing.T) {
	own := "10.100.0.0/24,10.100.1.0/24"
	want := append(projectIsolationRules("p1", strings.Split(own, ",")),
		isolationRules("10.100.0.0/16", []string{"192.0.2.0/24"}, nil, isolationComment)...)
	for _, off := range []int{0, 3} { // a project ACCEPT, a provider DROP
		current := slices.Clone(want)
		current[off].Enable = 0
		srv, rules, writes := isolationFake(t, current)
		if err := srv.ReconcileIsolation(context.Background(), "p1", "10.100.0.0/16", nil, nil); err != nil {
			t.Fatal(err)
		}
		if *writes == 0 {
			t.Fatalf("rule %d disabled: no write", off)
		}
		if !slices.EqualFunc(*rules, want, sameLiveRule) {
			t.Fatalf("rule %d disabled: rules after the reconcile:\n%+v\nwant:\n%+v", off, *rules, want)
		}
	}
}

// One portal: a changed pool replaces its own DROPs and leaves no stale ones behind.
func TestReconcileIsolationReplacesOwnRules(t *testing.T) {
	own := "10.100.0.0/24,10.100.1.0/24"
	current := append(projectIsolationRules("p1", strings.Split(own, ",")),
		isolationRules("10.100.0.0/16", []string{"192.0.2.0/24", "198.51.100.0/24"}, nil, isolationComment)...)
	srv, rules, _ := isolationFake(t, current)
	if err := srv.ReconcileIsolation(context.Background(), "p1", "10.100.0.0/16", nil, nil); err != nil {
		t.Fatal(err)
	}
	var drops []string
	for _, r := range *rules {
		if r.Action == "DROP" {
			drops = append(drops, r.Type+" "+r.Dest)
		}
	}
	want := []string{"forward 10.100.0.0/16", "in 10.100.0.0/16", "forward 192.0.2.0/24", "in 192.0.2.0/24"}
	if !reflect.DeepEqual(drops, want) {
		t.Fatalf("drops = %v, want %v", drops, want)
	}
	if (*rules)[0].Action != "ACCEPT" || (*rules)[1].Action != "ACCEPT" {
		t.Fatalf("ACCEPTs fell below the DROPs: %+v", *rules)
	}
}

// Two portals: neither may remove the other's DROPs, whatever their pools.
func TestReconcileIsolationLeavesOtherPortals(t *testing.T) {
	theirs := isolationRules("10.200.0.0/16", []string{"192.0.2.0/24"}, nil, isolationComment+"/other-")
	theirAccept := projectIsolationRules("p9", []string{"10.200.7.0/24"})[0]
	srv, rules, _ := isolationFake(t, append([]FirewallRule{theirAccept}, theirs...))
	if err := srv.ReconcileIsolation(context.Background(), "p1", "10.100.0.0/16", nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, r := range append(theirs, theirAccept) {
		if !slices.ContainsFunc(*rules, func(g FirewallRule) bool { return sameRule(g, r) }) {
			t.Fatalf("rule of another portal is gone: %+v", r)
		}
	}
	if (*rules)[len(*rules)-1].Comment != isolationComment+"/other-" {
		t.Fatalf("rules of the other portal were moved: %+v", *rules)
	}
	srv.PoolPrefix = "pcx-"
	if got := srv.isolationOwner(); got != isolationComment+"/pcx-" {
		t.Fatalf("owner = %q", got)
	}
}

// A dead node must not block a network change, and what only it knew must not lose its DROP.
func TestReconcileIsolationWithNodeDown(t *testing.T) {
	nodeDown := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "595 No route to host", 595)
	}
	stale := isolationRules("10.100.0.0/16", []string{"192.0.2.0/24", "198.51.100.0/24"}, nil, isolationComment)
	srv, rules, _ := isolationFakeNodes(t, stale, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/nodes/pve2/network": nodeDown,
	}, `{"type":"node","name":"pve2","ip":"192.0.2.12"}`)
	if err := srv.ReconcileIsolation(context.Background(), "p1", "10.100.0.0/16", nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, r := range stale {
		if !slices.ContainsFunc(*rules, func(g FirewallRule) bool { return sameRule(g, r) }) {
			t.Fatalf("DROP went while a node was down: %+v", r)
		}
	}
	if (*rules)[0].Comment != isolationComment+":p1" {
		t.Fatalf("the project's ACCEPTs must still be set: %+v", *rules)
	}

	// No DROPs at all: a partial list beats none.
	srv, rules, _ = isolationFakeNodes(t, nil, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/nodes/pve2/network": nodeDown,
	}, `{"type":"node","name":"pve2","ip":"192.0.2.12"}`)
	if err := srv.ReconcileIsolation(context.Background(), "p1", "10.100.0.0/16", nil, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(*rules, func(g FirewallRule) bool { return g.Action == "DROP" && g.Dest == "192.0.2.0/24" }) {
		t.Fatalf("no DROP for the networks the live nodes know: %+v", *rules)
	}
}

// A rule list that shifts between the read and the delete must not cost another rule.
func TestDeleteClusterRuleByIdentity(t *testing.T) {
	ops := FirewallRule{Type: "in", Action: "ACCEPT", Dest: "10.9.9.9", Comment: "ops"}
	stale := projectIsolationRules("p1", []string{"10.100.1.0/24"})
	srv, rules, _ := isolationFake(t, append([]FirewallRule{ops}, stale...))
	if err := srv.setProjectIsolation(context.Background(), "p1", nil); err != nil {
		t.Fatal(err)
	}
	if len(*rules) != 1 || (*rules)[0].Comment != "ops" {
		t.Fatalf("rules = %+v", *rules)
	}
}

func TestEnsureProjectZonePayload(t *testing.T) {
	var got map[string]string
	ctrlCreated := false
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/sdn/zones": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"zone":"overlay","type":"evpn","vrf-vxlan":10000},{"zone":"pother01","type":"evpn","vrf-vxlan":10007}]}`)
		},
		"GET /api2/json/cluster/sdn/controllers": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"controller":"ctl","type":"evpn"}]}`)
		},
		"POST /api2/json/cluster/sdn/controllers": func(w http.ResponseWriter, r *http.Request) {
			ctrlCreated = true
			fmt.Fprint(w, `{"data":null}`)
		},
		"POST /api2/json/cluster/sdn/zones": func(w http.ResponseWriter, r *http.Request) {
			r.ParseForm()
			got = map[string]string{}
			for k := range r.PostForm {
				got[k] = r.PostFormValue(k)
			}
			fmt.Fprint(w, `{"data":null}`)
		},
	})
	srv := &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}
	tmpl := ZoneConfig{Key: "overlay", Kind: "evpn", Controller: "tmplctl", ExitNodes: []string{"pve1"}, MTU: 1450}
	z, err := srv.EnsureProjectZone(context.Background(), "p1", tmpl, "ctl")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"zone": ZoneIDFor("p1"), "type": "evpn", "controller": "ctl", "vrf-vxlan": "10008",
		"mtu": "1450", "exitnodes": "pve1", "ipam": "pve",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("zone form = %v\nwant %v", got, want)
	}
	if z.Key != ZoneIDFor("p1") || ctrlCreated {
		t.Fatalf("zone = %+v, ctrlCreated = %v", z, ctrlCreated)
	}
}

func TestEnsureProjectZoneCollision(t *testing.T) {
	id := ZoneIDFor("p1")
	tests := []struct{ name, zones, vnets string }{
		{"non-evpn zone with our id", `[{"zone":"` + id + `","type":"vlan"}]`, `[]`},
		{"another owner's vnet in it", `[{"zone":"` + id + `","type":"evpn"}]`, `[{"vnet":"vx","zone":"` + id + `","alias":"st:p2:web"}]`},
		{"foreign vnet in it", `[{"zone":"` + id + `","type":"evpn"}]`, `[{"vnet":"vx","zone":"` + id + `","alias":"ops"}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
				"GET /api2/json/cluster/sdn/zones": func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, `{"data":`+tt.zones+`}`)
				},
				"GET /api2/json/cluster/sdn/vnets": func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, `{"data":`+tt.vnets+`}`)
				},
			})
			srv := &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}
			if _, err := srv.EnsureProjectZone(context.Background(), "p1", ZoneConfig{Key: "overlay", Kind: "evpn"}, "ctl"); err == nil || !strings.Contains(err.Error(), "collides") {
				t.Fatalf("err = %v, want a collision", err)
			}
		})
	}
}

func TestDeleteProjectZoneIfEmpty(t *testing.T) {
	id := ZoneIDFor("p1")
	for _, tt := range []struct {
		name, vnets string
		wantGone    bool
	}{
		{"last vnet gone", `[{"vnet":"vx","zone":"overlay","alias":"st:p2:x"}]`, true},
		{"vnet still there", `[{"vnet":"vx","zone":"` + id + `","alias":"st:p1:web"}]`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			deleted := false
			_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
				"GET /api2/json/cluster/sdn/vnets": func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, `{"data":`+tt.vnets+`}`)
				},
				"GET /api2/json/cluster/sdn/zones": func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, `{"data":[{"zone":"`+id+`","type":"evpn"}]}`)
				},
				"DELETE /api2/json/cluster/sdn/zones/" + id: func(w http.ResponseWriter, r *http.Request) {
					deleted = true
					fmt.Fprint(w, `{"data":null}`)
				},
			})
			srv := &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}
			gone, err := srv.DeleteProjectZoneIfEmpty(context.Background(), "p1")
			if err != nil || gone != tt.wantGone || deleted != tt.wantGone {
				t.Fatalf("gone=%v deleted=%v err=%v, want %v", gone, deleted, err, tt.wantGone)
			}
		})
	}
}

// TestTeardownProjectNetworks: teardown stages the delete of every vnet the
// project owns (subnet, then vnet), drops its isolation rule and never touches
// another project's or an operator's vnet.
func TestTeardownProjectNetworks(t *testing.T) {
	var calls []string
	rec := func(s string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, s)
			fmt.Fprint(w, `{"data":null}`)
		}
	}
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/sdn/vnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[
				{"vnet":"vaaa11","zone":"overlay","alias":"st:p1:web"},
				{"vnet":"vccc33","zone":"overlay","alias":"st:p2:other"},
				{"vnet":"opsnet","zone":"overlay","alias":"ops"}
			]}`)
		},
		"GET /api2/json/cluster/firewall/rules": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"pos":0,"type":"forward","action":"ACCEPT","comment":"proxcloud:isolation:p1"},
				{"pos":1,"type":"forward","action":"DROP","comment":"proxcloud:isolation"}]}`)
		},
		"DELETE /api2/json/cluster/firewall/rules/0": rec("del rule 0"),
		"GET /api2/json/cluster/sdn/vnets/vaaa11/subnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"subnet":"overlay-10.100.0.0-24","cidr":"10.100.0.0/24"}]}`)
		},
		"DELETE /api2/json/cluster/sdn/vnets/vaaa11/subnets/overlay-10.100.0.0-24": rec("del subnet vaaa11"),
		"DELETE /api2/json/cluster/sdn/vnets/vaaa11":                               rec("del vnet vaaa11"),
	})
	srv := &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}
	staged, err := srv.TeardownProjectNetworks(context.Background(), "p1")
	if err != nil || !staged {
		t.Fatalf("staged=%v err=%v", staged, err)
	}
	if want := []string{"del subnet vaaa11", "del vnet vaaa11", "del rule 0"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	if staged, err := srv.TeardownProjectNetworks(context.Background(), "p9"); err != nil || staged {
		t.Fatalf("no vnets: staged=%v err=%v, want nothing staged", staged, err)
	}
}

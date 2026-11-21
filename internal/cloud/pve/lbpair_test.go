package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// golden compares got with testdata/name; UPDATE_GOLDEN=1 writes it.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := "testdata/" + name
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

func legacyGoldenSpec() LBSpec {
	return LBSpec{
		Listeners: []LBListener{{Port: 80, Pool: "web"}, {Port: 5432, Pool: "db"}},
		Pools: []LBPool{
			{Name: "web", Protocol: "http", Algorithm: "roundrobin", Members: []LBMember{
				{ServerID: "101", Port: 8080, Weight: 1, Address: "10.0.0.11"},
				{ServerID: "102", Port: 8080, Weight: 3, Address: "10.0.0.12"},
				{ServerID: "103", Port: 8080, Weight: 1, Address: "10.0.0.13", Missing: true},
			}},
			{Name: "db", Protocol: "tcp", Algorithm: "leastconn", Members: []LBMember{{ServerID: "104", Port: 5432, Weight: 1, Address: "10.0.0.14"}}},
		},
	}
}

// testdata/lb-legacy.cfg was rendered by origin/main (f0a1afe8) before the pair: a single appliance's config must
// not change by a byte, or every existing balancer would be pushed again at the next sync.
func TestRenderHAProxyLegacyUnchanged(t *testing.T) {
	want, err := os.ReadFile("testdata/lb-legacy.cfg")
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range []string{RenderHAProxy(legacyGoldenSpec()), RenderHAProxyOn(legacyGoldenSpec(), nil)} {
		if got != string(want) {
			t.Fatalf("the single appliance's config changed:\n%s", got)
		}
	}
}

func TestRenderHAProxyPair(t *testing.T) {
	s := legacyGoldenSpec()
	s.Listeners[0].SourceRanges = []string{"198.51.100.0/24"} // firewall only: nothing in the config
	s.Pools[0].HealthCheck = &LBHealthCheck{Type: "http", Port: 10256, Path: "/healthz"}
	s.Pools[1].HealthCheck = &LBHealthCheck{Type: "tcp"}
	s.Pools[1].ProxyProtocol = true
	golden(t, "lb-pair.cfg", RenderHAProxyOn(s, []string{"203.0.113.10", "203.0.113.11", "not-an-address", "2001:db8::1"}))
}

func TestLBSpecValidateHealthAndSources(t *testing.T) {
	base := func() LBSpec {
		return LBSpec{
			Listeners: []LBListener{{Port: 80, Pool: "web"}},
			Pools:     []LBPool{{Name: "web", Members: []LBMember{{ServerID: "101", Port: 8080}}}},
		}
	}
	s := base()
	s.Pools[0].HealthCheck = &LBHealthCheck{Type: "http", Path: "/healthz"}
	s.Listeners[0].SourceRanges = []string{"198.51.100.7", " 203.0.113.9/24", "198.51.100.7/32"}
	if msg := s.Validate(); msg != "" {
		t.Fatal(msg)
	}
	if got := s.Listeners[0].SourceRanges; !slices.Equal(got, []string{"198.51.100.7/32", "203.0.113.0/24"}) {
		t.Fatalf("source ranges = %v", got)
	}
	var many []string
	for i := range 40 { // 40 x 15 characters: over the 512 a rule's source takes (C6)
		many = append(many, fmt.Sprintf("198.51.%d.0/24", 100+i))
	}
	for name, breakIt := range map[string]func(*LBSpec){
		"check type":      func(s *LBSpec) { s.Pools[0].HealthCheck = &LBHealthCheck{Type: "udp"} },
		"check port":      func(s *LBSpec) { s.Pools[0].HealthCheck = &LBHealthCheck{Type: "tcp", Port: 70000} },
		"tcp with a path": func(s *LBSpec) { s.Pools[0].HealthCheck = &LBHealthCheck{Type: "tcp", Path: "/"} },
		"path injection": func(s *LBSpec) {
			s.Pools[0].HealthCheck = &LBHealthCheck{Type: "http", Path: "/x\n  server evil 1.1.1.1:1"}
		},
		"path with space":  func(s *LBSpec) { s.Pools[0].HealthCheck = &LBHealthCheck{Type: "http", Path: "/a b"} },
		"ipv6 source":      func(s *LBSpec) { s.Listeners[0].SourceRanges = []string{"2001:db8::/32"} },
		"any as a range":   func(s *LBSpec) { s.Listeners[0].SourceRanges = []string{"0.0.0.0/0"} },
		"not an address":   func(s *LBSpec) { s.Listeners[0].SourceRanges = []string{"example.com"} },
		"over 512 chars":   func(s *LBSpec) { s.Listeners[0].SourceRanges = many },
		"source injection": func(s *LBSpec) { s.Listeners[0].SourceRanges = []string{"10.0.0.0/8,0.0.0.0/0"} },
	} {
		s := base()
		breakIt(&s)
		if s.Validate() == "" {
			t.Errorf("%s: accepted", name)
		}
	}
}

func keepalivedFor(n int) LBKeepalived {
	var vips []string
	for i := range n {
		vips = append(vips, fmt.Sprintf("203.0.113.%d", 10+i))
	}
	return LBKeepalived{RouterID: 42, Self: "10.0.0.5", Peer: "10.0.0.6", TenantIface: "ens18", PublicIface: "ens19",
		VIPs: vips, Prefix: 24, Gateway: "203.0.113.1"}
}

// S1 and C8: VRRPv3, unicast with check_unicast_src, both BACKUP at one priority, nopreempt, garp refresh, a track
// script; one VIP in virtual_ipaddress (a placeholder with none), the rest excluded; rules with a preference.
// test/integration/lb_keepalived_test.go runs `keepalived -t` on the same renders.
func TestRenderKeepalived(t *testing.T) {
	for _, n := range []int{0, 1, 2} {
		conf, err := RenderKeepalived(keepalivedFor(n))
		if err != nil {
			t.Fatal(err)
		}
		golden(t, fmt.Sprintf("keepalived-%d.conf", n), conf)
	}
	conf, err := RenderKeepalived(keepalivedFor(50))
	if err != nil {
		t.Fatal(err)
	}
	in, ex, _ := strings.Cut(conf, "virtual_ipaddress_excluded {")
	if strings.Count(in, "/24 dev ens19") != 1 || strings.Count(ex, "/24 dev ens19") != 49+1 /* the subnet route */ {
		t.Errorf("50 VIPs: one in virtual_ipaddress, 49 excluded:\n%s", conf)
	}
	if strings.Count(conf, "table 100 preference 100") != 50 {
		t.Errorf("50 VIPs need 50 rules with a preference:\n%s", conf)
	}
	for name, k := range map[string]LBKeepalived{
		"self is peer":     {RouterID: 1, Self: "10.0.0.5", Peer: "10.0.0.5", TenantIface: "a", PublicIface: "b"},
		"iface injection":  {RouterID: 1, Self: "10.0.0.5", Peer: "10.0.0.6", TenantIface: "a\n}\nglobal_defs {", PublicIface: "b"},
		"same iface":       {RouterID: 1, Self: "10.0.0.5", Peer: "10.0.0.6", TenantIface: "a", PublicIface: "a"},
		"router id":        {RouterID: 256, Self: "10.0.0.5", Peer: "10.0.0.6", TenantIface: "a", PublicIface: "b"},
		"vip injection":    {RouterID: 1, Self: "10.0.0.5", Peer: "10.0.0.6", TenantIface: "a", PublicIface: "b", VIPs: []string{"1.2.3.4 dev lo"}, Prefix: 24, Gateway: "1.2.3.1"},
		"vip, no gateway":  {RouterID: 1, Self: "10.0.0.5", Peer: "10.0.0.6", TenantIface: "a", PublicIface: "b", VIPs: []string{"1.2.3.4"}, Prefix: 24},
		"ipv6 peer":        {RouterID: 1, Self: "10.0.0.5", Peer: "2001:db8::1", TenantIface: "a", PublicIface: "b"},
		"prefix too large": {RouterID: 1, Self: "10.0.0.5", Peer: "10.0.0.6", TenantIface: "a", PublicIface: "b", VIPs: []string{"1.2.3.4"}, Prefix: 33, Gateway: "1.2.3.1"},
	} {
		if _, err := RenderKeepalived(k); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
}

func TestParseVRRPState(t *testing.T) {
	for out, want := range map[string]VRRPState{
		"MASTER 1 1\n":  {State: "MASTER", HAProxy: true, HoldsVIP: true},
		"BACKUP 1 0":    {State: "BACKUP", HAProxy: true},
		"FAULT 0 0":     {State: "FAULT"},
		"STOP 1 0":      {State: "STOPPED", HAProxy: true},
		"STOPPED 1 1":   {State: "STOPPED", HAProxy: true, HoldsVIP: true},
		"UNKNOWN 1 0":   {State: "UNKNOWN", HAProxy: true},
		"weird 1 1":     {State: "UNKNOWN", HAProxy: true, HoldsVIP: true},
		"":              {State: "UNKNOWN"},
		"MASTER 1 1 x":  {State: "UNKNOWN"},
		"MASTER\n1\n1":  {State: "MASTER", HAProxy: true, HoldsVIP: true},
		"BACKUP 0 1\n ": {State: "BACKUP", HoldsVIP: true},
	} {
		if got := ParseVRRPState(out); got != want {
			t.Errorf("%q = %+v, want %+v", out, got, want)
		}
	}
}

// fwFake is the firewall API of a few VMs as the spike measured it: a rule's source and dest take at most 512
// characters and its dport list at most 15 numbers (pve-firewall Firewall.pm:1241-1245, :1716-1729); a new rule goes
// on top; and an address answers only while it is in the ipfilter-net1 set of every VM of the pair.
type fwFake struct {
	mu      sync.Mutex
	rules   map[int][]FirewallRule
	sets    map[int]map[string][]string // vmid -> set -> entries
	onWrite func()
	dropped []string // sets deleted whole
}

func (f *fwFake) answers(vip string) bool {
	for _, sets := range f.sets {
		if !slices.Contains(sets["ipfilter-net1"], vip) {
			return false
		}
	}
	return len(f.sets) > 0
}

func dportCount(d string) int {
	n := 0
	for p := range strings.SplitSeq(d, ",") {
		if strings.Contains(p, ":") {
			n += 2
		} else if p != "" {
			n++
		}
	}
	return n
}

func (f *fwFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api2/json/nodes/"), "/")
	// <node>/qemu/<vmid>/firewall/<rules|ipset>[/<x>[/<y>]]
	if len(parts) < 5 || parts[1] != "qemu" || parts[3] != "firewall" {
		http.Error(w, `{"errors":{"path":"no fake"}}`, http.StatusInternalServerError)
		return
	}
	vmid, _ := strconv.Atoi(parts[2])
	_ = r.ParseForm()
	write := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	bad := func(msg string) { w.WriteHeader(http.StatusBadRequest); fmt.Fprintf(w, `{"errors":{"x":%q}}`, msg) }
	defer func() {
		if r.Method != http.MethodGet && f.onWrite != nil {
			f.onWrite()
		}
	}()
	switch key := r.Method + " " + strings.Join(parts[4:min(len(parts), 5)], "/"); {
	case key == "GET rules" && len(parts) == 5:
		out := slices.Clone(f.rules[vmid])
		for i := range out {
			out[i].Pos = i
		}
		write(out)
	case key == "POST rules" && len(parts) == 5:
		rule := FirewallRule{Type: r.Form.Get("type"), Action: r.Form.Get("action"), Proto: r.Form.Get("proto"),
			Source: r.Form.Get("source"), Dest: r.Form.Get("dest"), DPort: r.Form.Get("dport"), Comment: r.Form.Get("comment")}
		switch {
		case len(rule.Source) > 512 || len(rule.Dest) > 512:
			bad("value may only be 512 characters long")
			return
		case dportCount(rule.DPort) > 15:
			bad("too many entries in dport list")
			return
		}
		f.rules[vmid] = append([]FirewallRule{rule}, f.rules[vmid]...)
		write(nil)
	case key == "DELETE rules" && len(parts) == 6:
		pos, _ := strconv.Atoi(parts[5])
		f.rules[vmid] = slices.Delete(f.rules[vmid], pos, pos+1)
		write(nil)
	case key == "GET ipset" && len(parts) == 5:
		var out []IPSet
		for name := range f.sets[vmid] {
			out = append(out, IPSet{Name: name})
		}
		write(out)
	case key == "POST ipset" && len(parts) == 5:
		if f.sets[vmid] == nil {
			f.sets[vmid] = map[string][]string{}
		}
		f.sets[vmid][r.Form.Get("name")] = []string{}
		write(nil)
	case key == "GET ipset" && len(parts) == 6:
		var out []IPSet
		for _, c := range f.sets[vmid][parts[5]] {
			out = append(out, IPSet{CIDR: c})
		}
		write(out)
	case key == "POST ipset" && len(parts) == 6:
		f.sets[vmid][parts[5]] = append(f.sets[vmid][parts[5]], strings.TrimSuffix(r.Form.Get("cidr"), "/32"))
		write(nil)
	case key == "DELETE ipset" && len(parts) == 7:
		f.sets[vmid][parts[5]] = slices.DeleteFunc(f.sets[vmid][parts[5]], func(c string) bool { return c == parts[6] })
		write(nil)
	case key == "DELETE ipset" && len(parts) == 6:
		delete(f.sets[vmid], parts[5])
		f.dropped = append(f.dropped, fmt.Sprintf("%d/%s", vmid, parts[5]))
		write(nil)
	default:
		http.Error(w, `{"errors":{"path":"no fake"}}`, http.StatusInternalServerError)
	}
}

func fwService(t *testing.T, f *fwFake) *Service {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(Config{APIURL: srv.URL, TokenID: "proxcloud@pve!t1", Secret: "sec"})
	if err != nil {
		t.Fatal(err)
	}
	return &Service{C: c}
}

// A new address goes into both appliances' sets entry by entry, and the one already served never stops answering.
// The set rebuild single appliances use would cut it for a moment: the spike's reason for C14's order.
func TestSetIPFilterEntriesKeepsServedAddresses(t *testing.T) {
	ctx := context.Background()
	f := &fwFake{rules: map[int][]FirewallRule{}, sets: map[int]map[string][]string{
		201: {"ipfilter-net1": {"203.0.113.10"}}, 202: {"ipfilter-net1": {"203.0.113.10"}},
	}}
	s := fwService(t, f)
	cut := 0
	f.onWrite = func() {
		if !f.answers("203.0.113.10") {
			cut++
		}
	}
	for _, vmid := range []int{201, 202} {
		if err := s.SetIPFilterEntries(ctx, "n1", vmid, "net1", []string{"203.0.113.10", "203.0.113.11/32"}); err != nil {
			t.Fatal(err)
		}
	}
	if cut != 0 || len(f.dropped) != 0 || !f.answers("203.0.113.11") {
		t.Fatalf("adding: served address cut %d times, sets dropped %v, new one answers %v", cut, f.dropped, f.answers("203.0.113.11"))
	}
	for _, vmid := range []int{201, 202} {
		if err := s.SetIPFilterEntries(ctx, "n1", vmid, "net1", []string{"203.0.113.10"}); err != nil {
			t.Fatal(err)
		}
	}
	if cut != 0 || len(f.dropped) != 0 || f.answers("203.0.113.11") {
		t.Fatalf("removing: cut %d, dropped %v, old one still answers %v", cut, f.dropped, f.answers("203.0.113.11"))
	}
	// The same change through SetIPFilter drops and rebuilds the set: the served address stops answering.
	if err := s.SetIPFilter(ctx, "n1", 201, "net1", []string{"203.0.113.10/32", "203.0.113.12/32"}); err != nil {
		t.Fatal(err)
	}
	if cut == 0 || len(f.dropped) == 0 {
		t.Fatalf("the fake does not see a rebuild cut the address (cut %d, dropped %v)", cut, f.dropped)
	}
	// A new appliance's set does not exist yet: it is made, then filled.
	f.sets[203] = nil
	delete(f.sets, 203)
	if err := s.SetIPFilterEntries(ctx, "n1", 203, "net1", []string{"203.0.113.10"}); err != nil {
		t.Fatal(err)
	}
	if got := f.sets[203]["ipfilter-net1"]; !slices.Equal(got, []string{"203.0.113.10"}) {
		t.Fatalf("new set = %v", got)
	}
}

// C6: 50 listener ports with two source lists on two VIPs fit Proxmox's limits as rules of at most 15 ports; the
// writer adds before it deletes and leaves every rule it did not write.
func TestSetLBRulesWithinFirewallLimits(t *testing.T) {
	ctx := context.Background()
	foreign := []FirewallRule{
		{Type: "group", Action: "pc-sg-default"},
		{Type: "in", Action: "ACCEPT", Proto: "tcp", DPort: "22", Comment: "someone's own"},
	}
	legacy := []FirewallRule{
		{Type: "in", Action: "ACCEPT", Proto: "tcp", DPort: "80", Comment: lbListenerComment},
		{Type: "in", Action: "ACCEPT", Proto: "tcp", DPort: "9999", Comment: lbListenerComment},
	}
	f := &fwFake{rules: map[int][]FirewallRule{201: append(slices.Clone(foreign), legacy...)}, sets: map[int]map[string][]string{}}
	s := fwService(t, f)
	open80 := 0
	f.onWrite = func() {
		if !slices.ContainsFunc(f.rules[201], func(r FirewallRule) bool { return slices.Contains(strings.Split(r.DPort, ","), "80") }) {
			open80++
		}
	}
	var ls []LBListener
	for i := range 50 {
		l := LBListener{Port: 80 + i, Pool: "p"}
		if i%2 == 1 {
			l.SourceRanges = []string{"198.51.100.0/24", "203.0.113.0/25"}
		}
		ls = append(ls, l)
	}
	want := append(LBListenerRules(ls, []string{"203.0.113.10", "203.0.113.11"}), LBVRRPRule("10.0.0.6"))
	// 25 ports per source list: 2 rules each, per VIP, and the vrrp rule.
	if len(want) != 2*2*2+1 {
		t.Fatalf("rules = %d: %+v", len(want), want)
	}
	if err := s.SetLBRules(ctx, "n1", 201, want); err != nil {
		t.Fatal(err)
	}
	var ours, others []FirewallRule
	for _, r := range f.rules[201] {
		if r.Comment == lbListenerComment || r.Comment == lbVRRPComment {
			ours = append(ours, r)
		} else {
			others = append(others, r)
		}
	}
	key := func(rs []FirewallRule) []string {
		var out []string
		for _, r := range rs {
			r.Pos = 0
			out = append(out, lbRuleKey(r))
		}
		slices.Sort(out)
		return out
	}
	if !slices.Equal(key(ours), key(want)) || !slices.Equal(key(others), key(foreign)) || open80 != 0 {
		t.Fatalf("rules after:\nours %v\nothers %v\nport 80 closed %d times", ours, others, open80)
	}
	// Again: nothing to do, nothing written.
	writes := 0
	f.onWrite = func() { writes++ }
	if err := s.SetLBRules(ctx, "n1", 201, want); err != nil || writes != 0 {
		t.Fatalf("second pass: %v, %d writes", err, writes)
	}
	// One rule per port, as a naive writer would have it, is refused past 15 numbers by the fake.
	var ports []string
	for i := range 16 {
		ports = append(ports, strconv.Itoa(1000+i))
	}
	if err := s.SetLBRules(ctx, "n1", 201, []FirewallRule{{Type: "in", Action: "ACCEPT", Proto: "tcp", DPort: strings.Join(ports, ","), Comment: lbListenerComment}}); err == nil {
		t.Fatal("the fake took a dport list of 16 numbers")
	}
}

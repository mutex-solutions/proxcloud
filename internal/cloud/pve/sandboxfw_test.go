package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func testSandbox() SandboxFirewall {
	return SandboxFirewall{
		Pool: "proxcloud-sandbox", Subnet: "10.110.0.0/24",
		Nodes: []string{"10.110.0.10-10.110.0.19"}, LoadBalancers: []string{"10.110.0.200-10.110.0.209"},
		ControlPlane: "10.20.0.214", APIPort: 6443, DNS: []string{"1.1.1.1"},
		KubeletFrom: []string{"10.20.0.201-10.20.0.209"}, Deny: []string{"10.30.0.0/24", "203.0.113.0/24"},
	}
}

func TestRangeCIDRs(t *testing.T) {
	for in, want := range map[string]string{
		"10.110.0.10-10.110.0.19":   "10.110.0.10/31 10.110.0.12/30 10.110.0.16/30",
		"10.0.0.0-10.0.0.255":       "10.0.0.0/24",
		"10.0.0.5-10.0.0.5":         "10.0.0.5/32",
		"192.0.2.7":                 "192.0.2.7/32",
		"0.0.0.0-255.255.255.255":   "0.0.0.0/0",
		"10.0.0.0/8,10.30.0.0/24":   "10.0.0.0/8", // collapsed: nftables refuses overlapping intervals
		"10.30.0.0/24,10.30.0.0/16": "10.30.0.0/16",
	} {
		ps, err := toCIDRs(strings.Split(in, ","))
		var got []string
		for _, p := range ps {
			got = append(got, p.String())
		}
		if err != nil || strings.Join(got, " ") != want {
			t.Errorf("%s: got %v %v, want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"10.0.0.9-10.0.0.1", "fd00::/64", "x", "10.0.0.1-y"} {
		if _, err := toCIDRs([]string{bad}); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}

func TestSandboxGeneration(t *testing.T) {
	g, err := testSandbox().generation()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(g.Group, sandboxPrefix("proxcloud-sandbox")) || len(g.Group) > 18 {
		t.Fatalf("group %q: needs the pool prefix and PVE's 18 characters", g.Group)
	}
	for _, r := range g.Rules {
		if r.Action != "ACCEPT" || r.Enable != 1 {
			t.Fatalf("every rule ACCEPTs (order-free, fail closed): %+v", r)
		}
		if strings.Contains(r.Source+r.Dest, "@") {
			t.Fatalf("placeholder left in %+v", r)
		}
	}
	has := func(dir, proto, addr, port string) bool {
		return slices.ContainsFunc(g.Rules, func(r FirewallRule) bool {
			return r.Type == dir && r.Proto == proto && r.Source+r.Dest == addr && r.DPort == port
		})
	}
	nodes := "+dc/" + g.NodeSet
	for _, want := range [][4]string{
		{"out", "tcp", "10.20.0.214", "6443,8132"},
		{"out", "udp", "1.1.1.1", "53"}, {"out", "tcp", "1.1.1.1", "53"},
		{"out", "udp", nodes, "8472"}, {"in", "udp", nodes, "8472"},
		{"in", "tcp", nodes, "10250"}, {"in", "tcp", "10.20.0.201,10.20.0.202/31,10.20.0.204/30,10.20.0.208/31", "10250"},
		{"out", "", "+dc/" + g.OutSet, ""},
	} {
		if !has(want[0], want[1], want[2], want[3]) {
			t.Errorf("missing rule %v", want)
		}
	}
	if has("in", "tcp", "10.20.0.214", "") || slices.ContainsFunc(g.Rules, func(r FirewallRule) bool { return r.Type == "in" && r.Source == "" }) {
		t.Fatal("nothing comes in from anywhere")
	}
	var nomatch []string
	for _, e := range g.Out {
		if e.NoMatch == 1 {
			nomatch = append(nomatch, e.CIDR)
		}
	}
	for _, want := range []string{"10.0.0.0/8", "203.0.113.0/24", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"} {
		if !slices.Contains(nomatch, want) {
			t.Errorf("internet set does not exclude %s: %v", want, nomatch)
		}
	}
	if slices.Contains(nomatch, "10.30.0.0/24") || slices.Contains(nomatch, "10.110.0.0/24") {
		t.Errorf("covered entries must collapse: %v", nomatch)
	}

	// Public sandbox subnet, control plane and DNS outside the private ranges: they still stay out of the internet set.
	f := testSandbox()
	f.Subnet, f.Nodes, f.LoadBalancers = "198.51.100.0/24", []string{"198.51.100.10-198.51.100.19"}, nil
	f.ControlPlane = "192.0.2.10"
	g2, err := f.generation()
	if err != nil {
		t.Fatal(err)
	}
	var excl []string
	for _, e := range g2.Out {
		if e.NoMatch == 1 {
			excl = append(excl, e.CIDR)
		}
	}
	if !slices.Contains(excl, "198.51.100.0/24") || !slices.Contains(excl, "192.0.2.10") {
		t.Errorf("the subnet (its gateway is a PVE node) and the control plane must be excluded: %v", excl)
	}
	if g2.Group == g.Group {
		t.Fatal("other content, other generation")
	}
	again, _ := testSandbox().generation()
	if again.Group != g.Group {
		t.Fatal("the generation name must be stable")
	}

	for name, mod := range map[string]func(*SandboxFirewall){
		"no workers":           func(f *SandboxFirewall) { f.Nodes = nil },
		"worker outside":       func(f *SandboxFirewall) { f.Nodes = []string{"10.111.0.10"} },
		"lb overlaps workers":  func(f *SandboxFirewall) { f.LoadBalancers = []string{"10.110.0.15"} },
		"cp inside subnet":     func(f *SandboxFirewall) { f.ControlPlane = "10.110.0.250" },
		"cp is a name":         func(f *SandboxFirewall) { f.ControlPlane = "cp.example.org" },
		"subnet IPv6":          func(f *SandboxFirewall) { f.Subnet = "fd00::/64" },
		"dns is a range in ok": func(f *SandboxFirewall) { f.DNS = []string{"nope"} },
	} {
		f := testSandbox()
		mod(&f)
		if _, err := f.generation(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// sandboxFake is a stateful PVE: the sandbox vnet, VMs in pools, VM firewall and cluster groups and IP sets.
type sandboxFake struct {
	mu      sync.Mutex
	vms     map[int]*fakeVM // all on node pve1
	sets    map[string][]IPSet
	setNote map[string]string
	groups  map[string][]FirewallRule
	grpNote map[string]string
	writes  []string
}

type fakeVM struct {
	name, pool string
	cfg        VMConfig
	opts       map[string]any
	rules      []FirewallRule
	sets       map[string][]IPSet
}

func newSandboxFake(t *testing.T) (*sandboxFake, *Service) {
	f := &sandboxFake{vms: map[int]*fakeVM{}, sets: map[string][]IPSet{}, setNote: map[string]string{},
		groups: map[string][]FirewallRule{}, grpNote: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	c, err := New(Config{APIURL: srv.URL, TokenID: "proxcloud@pve!t1", Secret: "sec"})
	if err != nil {
		t.Fatal(err)
	}
	return f, &Service{C: c, PoolPrefix: "pc-"}
}

func (f *sandboxFake) addVM(vmid int, name, pool string, nets ...string) *fakeVM {
	vm := &fakeVM{name: name, pool: pool, cfg: VMConfig{}, opts: map[string]any{}, sets: map[string][]IPSet{}}
	for i, n := range nets {
		vm.cfg["net"+strconv.Itoa(i)] = n
	}
	f.vms[vmid] = vm
	return vm
}

func reply(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }

func (f *sandboxFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	p := strings.TrimPrefix(r.URL.Path, "/api2/json")
	seg := strings.Split(strings.Trim(p, "/"), "/")
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+p)
	}
	rule := func() FirewallRule {
		en, _ := strconv.Atoi(r.Form.Get("enable"))
		return FirewallRule{Type: r.Form.Get("type"), Action: r.Form.Get("action"), Proto: r.Form.Get("proto"),
			Source: r.Form.Get("source"), Dest: r.Form.Get("dest"), DPort: r.Form.Get("dport"), Enable: en, Comment: r.Form.Get("comment")}
	}
	listed := func(rs []FirewallRule) []FirewallRule {
		out := slices.Clone(rs)
		for i := range out {
			out[i].Pos, out[i].Digest = i, fmt.Sprint(len(rs))
		}
		return out
	}
	entry := func() IPSet {
		nm, _ := strconv.Atoi(r.Form.Get("nomatch"))
		return IPSet{CIDR: strings.TrimSuffix(r.Form.Get("cidr"), "/32"), NoMatch: nm}
	}
	switch {
	case p == "/cluster/sdn/vnets":
		reply(w, []map[string]any{{"vnet": "vtenant", "zone": "overlay"}, {"vnet": "pcsbx", "zone": "overlay"}})
	case strings.HasPrefix(p, "/cluster/sdn/vnets/") && seg[len(seg)-1] == "subnets":
		cidr := map[string]string{"pcsbx": "10.110.0.0/24", "vtenant": "10.100.0.0/24"}[seg[3]]
		reply(w, []map[string]any{{"cidr": cidr}})
	case p == "/cluster/resources":
		var out []map[string]any
		for id, vm := range f.vms {
			out = append(out, map[string]any{"type": "qemu", "vmid": id, "node": "pve1", "name": vm.name, "pool": vm.pool, "status": "running"})
		}
		reply(w, out)
	case p == "/nodes":
		reply(w, []map[string]any{{"node": "pve1", "status": "online"}})
	case p == "/cluster/firewall/ipset" && r.Method == http.MethodGet:
		var out []IPSet
		for n := range f.sets {
			out = append(out, IPSet{Name: n, Comment: f.setNote[n]})
		}
		reply(w, out)
	case p == "/cluster/firewall/ipset":
		f.sets[r.Form.Get("name")], f.setNote[r.Form.Get("name")] = []IPSet{}, r.Form.Get("comment")
		reply(w, nil)
	case strings.HasPrefix(p, "/cluster/firewall/ipset/"):
		switch r.Method {
		case http.MethodGet:
			reply(w, f.sets[seg[3]])
		case http.MethodPost:
			f.sets[seg[3]] = append(f.sets[seg[3]], entry())
			reply(w, nil)
		case http.MethodDelete:
			delete(f.sets, seg[3])
			reply(w, nil)
		}
	case p == "/cluster/firewall/groups" && r.Method == http.MethodGet:
		var out []FirewallGroup
		for g := range f.groups {
			out = append(out, FirewallGroup{Group: g, Comment: f.grpNote[g]})
		}
		reply(w, out)
	case p == "/cluster/firewall/groups":
		f.groups[r.Form.Get("group")], f.grpNote[r.Form.Get("group")] = []FirewallRule{}, r.Form.Get("comment")
		reply(w, nil)
	case strings.HasPrefix(p, "/cluster/firewall/groups/"):
		g := seg[3]
		switch {
		case r.Method == http.MethodGet:
			reply(w, listed(f.groups[g]))
		case r.Method == http.MethodPost:
			f.groups[g] = append([]FirewallRule{rule()}, f.groups[g]...)
			reply(w, nil)
		case r.Method == http.MethodDelete && len(seg) == 5:
			pos, _ := strconv.Atoi(seg[4])
			f.groups[g] = slices.Delete(f.groups[g], pos, pos+1)
			reply(w, nil)
		case r.Method == http.MethodDelete:
			delete(f.groups, g)
			reply(w, nil)
		}
	case strings.HasPrefix(p, "/nodes/pve1/tasks/"):
		reply(w, map[string]any{"status": "stopped", "exitstatus": "OK"})
	case strings.HasPrefix(p, "/nodes/pve1/qemu/"):
		id, _ := strconv.Atoi(seg[3])
		vm := f.vms[id]
		rest := strings.Join(seg[4:], "/")
		switch {
		case rest == "config" && r.Method == http.MethodGet:
			reply(w, vm.cfg)
		case rest == "config":
			for k := range r.Form {
				vm.cfg[k] = r.Form.Get(k)
			}
			reply(w, "UPID:pve1:1:1:1:qmconfig:"+seg[3]+":x:")
		case rest == "firewall/options" && r.Method == http.MethodGet:
			reply(w, vm.opts)
		case rest == "firewall/options":
			for k := range r.Form {
				vm.opts[k] = r.Form.Get(k)
			}
			reply(w, nil)
		case rest == "firewall/rules" && r.Method == http.MethodGet:
			reply(w, listed(vm.rules))
		case rest == "firewall/rules":
			vm.rules = append([]FirewallRule{rule()}, vm.rules...)
			reply(w, nil)
		case strings.HasPrefix(rest, "firewall/rules/"):
			pos, _ := strconv.Atoi(seg[len(seg)-1])
			if r.Form.Get("digest") != fmt.Sprint(len(vm.rules)) {
				w.WriteHeader(500)
				fmt.Fprint(w, `{"errors":{"digest":"detected modified configuration"}}`)
				return
			}
			vm.rules = slices.Delete(vm.rules, pos, pos+1)
			reply(w, nil)
		case rest == "firewall/ipset" && r.Method == http.MethodGet:
			var out []IPSet
			for n := range vm.sets {
				out = append(out, IPSet{Name: n})
			}
			reply(w, out)
		case rest == "firewall/ipset":
			vm.sets[r.Form.Get("name")] = []IPSet{}
			reply(w, nil)
		case strings.HasPrefix(rest, "firewall/ipset/"):
			n := seg[len(seg)-1]
			switch r.Method {
			case http.MethodGet:
				reply(w, vm.sets[n])
			case http.MethodPost:
				vm.sets[n] = append(vm.sets[n], entry())
				reply(w, nil)
			case http.MethodDelete:
				delete(vm.sets, n)
				reply(w, nil)
			}
		default:
			w.WriteHeader(500)
		}
	default:
		w.WriteHeader(500)
		fmt.Fprintf(w, `{"errors":{"path":"no fake for %s %s"}}`, r.Method, p)
	}
}

func cidrsOf(es []IPSet) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.CIDR)
	}
	slices.Sort(out)
	return out
}

func TestSandboxSecure(t *testing.T) {
	ctx := context.Background()
	f, s := newSandboxFake(t)
	vm := f.addVM(121, "w1", "proxcloud-sandbox", "virtio=BC:24:11:00:00:01,bridge=pcsbx", "virtio=BC:24:11:00:00:02,bridge=vmbr1")
	old, foreign := sandboxPrefix("proxcloud-sandbox")+"deadbeef", sandboxPrefix("other-pool")+"deadbeef"
	vm.rules = []FirewallRule{{Type: "group", Action: old, Enable: 1}, {Type: "in", Action: "ACCEPT", Enable: 1}}
	tenant := f.addVM(130, "t1", "pc-p1", "virtio=BC:24:11:00:00:03,bridge=vtenant")
	for _, g := range []string{old, foreign} {
		f.groups[g], f.grpNote[g] = []FirewallRule{}, sandboxComment
		f.sets[g+"-o"], f.setNote[g+"-o"] = []IPSet{}, sandboxComment
	}

	p, err := s.StartSandboxPass(ctx, testSandbox())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Workers) != 1 || p.Workers[0].VMID != 121 {
		t.Fatalf("workers %+v: only the sandbox pool", p.Workers)
	}
	st, err := p.Secure(ctx, p.Workers[0], "")
	if err != nil || !st.Secured || st.Pinned != "" {
		t.Fatalf("first secure: %+v %v", st, err)
	}
	if got := cidrsOf(vm.sets["ipfilter-net0"]); !slices.Equal(got, []string{"10.110.0.10/31", "10.110.0.12/30", "10.110.0.16/30", "10.110.0.200/29", "10.110.0.208/31"}) {
		t.Fatalf("before its Node joins, the filter admits the worker range and the load balancers: %v", got)
	}
	if got, ok := vm.sets["ipfilter-net1"]; !ok || len(got) != 0 {
		t.Fatalf("a NIC off the sandbox vnet gets an empty filter (no IPv4 out): %v %v", got, ok)
	}
	if len(vm.rules) != 1 || vm.rules[0].Type != "group" || vm.rules[0].Action != p.Group() {
		t.Fatalf("the current group must be the VM's only rule: %+v", vm.rules)
	}
	for k, v := range sandboxFirewallOptions {
		if fmt.Sprint(vm.opts[k]) != v {
			t.Fatalf("option %s = %v", k, vm.opts[k])
		}
	}
	for _, nic := range []string{"net0", "net1"} {
		if !strings.Contains(vm.cfg.Str(nic), "firewall=1") {
			t.Fatalf("%s: %s", nic, vm.cfg.Str(nic))
		}
	}
	lastFW := slices.IndexFunc(f.writes, func(w string) bool { return strings.HasSuffix(w, "/qemu/121/config") })
	for _, w := range f.writes[lastFW+1:] {
		if strings.Contains(w, "/qemu/121/") {
			t.Fatalf("the NIC flag goes last, %s came after it: %v", w, f.writes)
		}
	}
	if len(f.sets[p.g.NodeSet]) == 0 || len(f.groups[p.Group()]) != len(p.g.Rules) {
		t.Fatal("generation not built")
	}

	st, err = p.Secure(ctx, p.Workers[0], "10.110.0.11")
	if err != nil || !st.Secured || st.Pinned != "10.110.0.11" {
		t.Fatalf("pin: %+v %v", st, err)
	}
	if got := cidrsOf(vm.sets["ipfilter-net0"]); !slices.Equal(got, []string{"10.110.0.11", "10.110.0.200/29", "10.110.0.208/31"}) {
		t.Fatalf("pinned filter %v", got)
	}
	// The kubelet writes its Node: once pinned, a later address is not followed, nor a load balancer address taken.
	for _, claim := range []string{"10.110.0.12", "10.110.0.200", ""} {
		if st, _ := p.Secure(ctx, p.Workers[0], claim); st.Pinned != "10.110.0.11" || !st.Secured {
			t.Fatalf("claim %q moved the pin: %+v", claim, st)
		}
	}

	// Drift is repaired: someone turns the filter off and adds a rule.
	vm.opts["ipfilter"] = "0"
	vm.rules = append(vm.rules, FirewallRule{Type: "out", Action: "ACCEPT", Enable: 1})
	vm.cfg["net0"] = "virtio=BC:24:11:00:00:01,bridge=pcsbx,firewall=0"
	if st, err := p.Secure(ctx, p.Workers[0], "10.110.0.11"); err != nil || !st.Secured {
		t.Fatalf("drift: %+v %v", st, err)
	}
	if vm.opts["ipfilter"] != "1" || len(vm.rules) != 1 || !strings.Contains(vm.cfg.Str("net0"), "firewall=1") {
		t.Fatalf("drift not repaired: %v %+v %s", vm.opts, vm.rules, vm.cfg.Str("net0"))
	}

	if err := p.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.groups[old]; ok || f.sets[old+"-o"] != nil {
		t.Fatal("an older generation no worker uses goes, its sets too")
	}
	if _, ok := f.groups[foreign]; !ok || f.sets[foreign+"-o"] == nil {
		t.Fatal("another sandbox pool's generation stays")
	}
	if len(tenant.rules) != 0 || len(tenant.opts) != 0 {
		t.Fatal("a tenant VM is never touched")
	}

	// A worker range not starting at a node: a pin outside the range is not taken.
	vm2 := f.addVM(122, "w2", "proxcloud-sandbox", "virtio=BC:24:11:00:00:04,bridge=pcsbx")
	if st, _ := p.Secure(ctx, SandboxWorker{Node: "pve1", VMID: 122}, "10.30.0.5"); st.Pinned != "" {
		t.Fatalf("an address outside the worker range was pinned: %+v %v", st, cidrsOf(vm2.sets["ipfilter-net0"]))
	}
}

func TestSandboxGenerationDrift(t *testing.T) {
	ctx := context.Background()
	f, s := newSandboxFake(t)
	f.addVM(121, "w1", "proxcloud-sandbox", "virtio=BC:24:11:00:00:01,bridge=pcsbx")
	p, err := s.StartSandboxPass(ctx, testSandbox())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Secure(ctx, p.Workers[0], ""); err != nil {
		t.Fatal(err)
	}
	// Someone widens the IP set a worker uses: never rewritten in place, reported.
	f.sets[p.g.OutSet] = append(f.sets[p.g.OutSet][:2:2], f.sets[p.g.OutSet][3:]...)
	if _, err := s.StartSandboxPass(ctx, testSandbox()); !errors.Is(err, ErrSandboxDrift) {
		t.Fatalf("want ErrSandboxDrift, got %v", err)
	}
	// A half-built generation nobody uses yet is rebuilt.
	f2, s2 := newSandboxFake(t)
	g, _ := testSandbox().generation()
	f2.groups[g.Group], f2.grpNote[g.Group] = g.Rules[:3], sandboxComment
	if _, err := s2.StartSandboxPass(ctx, testSandbox()); err != nil || len(f2.groups[g.Group]) != len(g.Rules) {
		t.Fatalf("rebuild: %v, %d rules", err, len(f2.groups[g.Group]))
	}
}

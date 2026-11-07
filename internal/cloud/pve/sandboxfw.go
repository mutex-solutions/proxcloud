package pve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// Layer 1 of the sandbox network (docs/managed-sandbox.md §2): the VM firewall on every NIC of the platform's sandbox
// workers. Layer 0, the cluster isolation rules, sees routed traffic only. This sees every frame a worker sends or
// receives, frames switched inside the sandbox vnet included, and pins the worker's source address.
//
// The rules live in one cluster security group per generation (its name carries a hash of its content) plus two
// cluster IP sets, and every worker references that group as its only rule. All of them ACCEPT; the VM policies DROP
// both ways. So no rule order and no partial state can open anything: a missing rule drops more, and a worker moving
// to a new generation holds both groups for a moment, which allows the union of two complete sets.

const (
	// SandboxUnsecuredTaint keeps tenant pods off a worker until its firewall is in place (set at join by the chart).
	SandboxUnsecuredTaint = "proxcloud.io/unsecured"
	sandboxComment        = "proxcloud:sandbox"
	// Kamaji's konnectivity server port on the tenant control plane's address.
	// ponytail: Kamaji's default, not read from the TenantControlPlane; read it there if a site ever changes it.
	sandboxKonnectivityPort = 8132
)

// ErrSandboxDrift: a group or IP set of the current generation exists with other content while a worker uses it. It
// is not rewritten in place (that could open a gap); the pass reports it and no worker is cleared for tenants.
var ErrSandboxDrift = errors.New("pve sandbox: the firewall group or IP set in use was changed outside the portal")

// ErrSandboxUnenforced: the datacenter firewall is off, so PVE enforces no VM rule anywhere. The pass still writes
// every worker's firewall, and Secure reports this for each one so that none is cleared for tenants.
var ErrSandboxUnenforced = errors.New("pve sandbox: the datacenter firewall is off, so PVE enforces no VM firewall")

// sandboxPrivate are the destinations a worker never reaches except through an explicit rule (EE's guardrail set).
var sandboxPrivate = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
}

// SandboxFirewall is what a sandbox worker may reach and be reached from; everything else is dropped.
type SandboxFirewall struct {
	Pool          string   // the workers' PVE pool
	Subnet        string   // the sandbox vnet's subnet
	Nodes         []string // worker addresses inside Subnet: CIDRs, addresses or a-b ranges
	LoadBalancers []string // MetalLB addresses inside Subnet: any worker may answer for them
	ControlPlane  string   // the sandbox control plane's address: API on APIPort, konnectivity on 8132
	APIPort       int
	DNS           []string // resolvers, port 53 only
	KubeletFrom   []string // sources allowed to the kubelet (10250), e.g. the services network
	Deny          []string // never reachable on top of the private ranges: management networks, blocked, tenant pool
	// The public entry (docs/managed-sandbox.md §4): EntryFrom, the entry appliance's address on the sandbox vnet, is
	// the one source allowed to sandbox-proxy's MetalLB address ProxyAddress on ProxyPort (default 443). Unset: no entry.
	ProxyAddress string
	ProxyPort    int
	EntryFrom    string
	// The sandbox registry (docs/managed-sandbox.md §6): the one address and port outside the subnet the workers'
	// containerd pulls templates and runtime images from (a MetalLB address on the services cluster). Unset: none.
	RegistryAddress string
	RegistryPort    int
	// The template build bucket (docs/managed-sandbox.md §7): build pods fetch their contexts from it. Unset: none.
	BuildStorageAddress string
	BuildStoragePort    int
}

// sandboxGen is one generation of the shared rules: the group and its two IP sets.
type sandboxGen struct {
	Group   string
	NodeSet string
	OutSet  string
	Nodes   []IPSet // the workers' addresses
	Out     []IPSet // the internet: 0.0.0.0/1 and 128.0.0.0/1 minus every denied network
	Rules   []FirewallRule
}

// sandboxPrefix names the generations of one pool: "sbx" + 4 hex of the pool, so portals sharing a PVE cluster with
// another sandbox pool leave each other's groups alone (a group name is at most 18 characters).
func sandboxPrefix(pool string) string {
	sum := sha256.Sum256([]byte(pool))
	return "sbx" + hex.EncodeToString(sum[:2])
}

// toCIDRs turns addresses, CIDRs and a-b ranges into the CIDRs PVE takes in an IP set.
func toCIDRs(in []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, raw := range in {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if a, b, ok := strings.Cut(raw, "-"); ok {
			lo, err1 := netip.ParseAddr(strings.TrimSpace(a))
			hi, err2 := netip.ParseAddr(strings.TrimSpace(b))
			if err1 != nil || err2 != nil || !lo.Is4() || !hi.Is4() || hi.Less(lo) {
				return nil, fmt.Errorf("pve sandbox: invalid range %q", raw)
			}
			out = append(out, rangeCIDRs(lo, hi)...)
			continue
		}
		if !strings.Contains(raw, "/") {
			raw += "/32"
		}
		p, err := netip.ParsePrefix(raw)
		if err != nil || !p.Addr().Is4() {
			return nil, fmt.Errorf("pve sandbox: invalid IPv4 address or CIDR %q", raw)
		}
		out = append(out, p.Masked())
	}
	return collapse(out), nil
}

// rangeCIDRs covers lo..hi with the fewest CIDRs.
func rangeCIDRs(lo, hi netip.Addr) []netip.Prefix {
	var out []netip.Prefix
	for {
		bits := 32
		for bits > 0 {
			p := netip.PrefixFrom(lo, bits-1).Masked()
			last := lastAddr(p)
			if p.Addr() != lo || hi.Less(last) {
				break
			}
			bits--
		}
		p := netip.PrefixFrom(lo, bits)
		out = append(out, p)
		last := lastAddr(p)
		if last == hi || last == netip.MustParseAddr("255.255.255.255") {
			return out
		}
		lo = last.Next()
	}
}

func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	n := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	n |= (1 << (32 - p.Bits())) - 1
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}

// collapse sorts and drops a CIDR another one covers: nftables refuses overlapping intervals in one set.
func collapse(in []netip.Prefix) []netip.Prefix {
	slices.SortFunc(in, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits()
	})
	var out []netip.Prefix
	for _, p := range in {
		if len(out) > 0 && out[len(out)-1].Contains(p.Addr()) && out[len(out)-1].Bits() <= p.Bits() {
			continue
		}
		out = append(out, p)
	}
	return out
}

// pveCIDR is how PVE stores a set entry: a host without /32.
func pveCIDR(p netip.Prefix) string {
	if p.Bits() == 32 {
		return p.Addr().String()
	}
	return p.String()
}

func cidrList(ps []netip.Prefix) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, pveCIDR(p))
	}
	return out
}

func within(outer, inner []netip.Prefix) bool {
	for _, i := range inner {
		if !slices.ContainsFunc(outer, func(o netip.Prefix) bool { return o.Contains(i.Addr()) && o.Bits() <= i.Bits() }) {
			return false
		}
	}
	return true
}

func overlap(a, b []netip.Prefix) bool {
	for _, x := range a {
		for _, y := range b {
			if x.Overlaps(y) {
				return true
			}
		}
	}
	return false
}

// generation computes the group and IP sets for f.
func (f SandboxFirewall) generation() (sandboxGen, error) {
	var g sandboxGen
	subnet, err := netip.ParsePrefix(strings.TrimSpace(f.Subnet))
	if err != nil || !subnet.Addr().Is4() {
		return g, fmt.Errorf("pve sandbox: invalid subnet %q", f.Subnet)
	}
	nodes, err := toCIDRs(f.Nodes)
	if err != nil {
		return g, err
	}
	lbs, err := toCIDRs(f.LoadBalancers)
	if err != nil {
		return g, err
	}
	sub := []netip.Prefix{subnet.Masked()}
	switch {
	case len(nodes) == 0:
		return g, errors.New("pve sandbox: no worker addresses")
	case !within(sub, nodes) || !within(sub, lbs):
		return g, fmt.Errorf("pve sandbox: worker and load balancer addresses must be inside %s", subnet)
	case overlap(nodes, lbs):
		return g, errors.New("pve sandbox: worker and load balancer addresses overlap")
	}
	cp, err := netip.ParseAddr(strings.TrimSpace(f.ControlPlane))
	if err != nil || !cp.Is4() || subnet.Contains(cp) {
		return g, fmt.Errorf("pve sandbox: invalid control plane address %q (IPv4, outside the sandbox subnet)", f.ControlPlane)
	}
	apiPort := f.APIPort
	if apiPort <= 0 {
		apiPort = 6443
	}
	dns, err := toCIDRs(f.DNS)
	if err != nil {
		return g, err
	}
	kubelet, err := toCIDRs(f.KubeletFrom)
	if err != nil {
		return g, err
	}
	denyIn := append(append(slices.Clone(sandboxPrivate), f.Deny...), subnet.String(), cp.String()+"/32")
	deny, err := toCIDRs(denyIn)
	if err != nil {
		return g, err
	}

	g.Nodes = make([]IPSet, 0, len(nodes))
	for _, n := range nodes {
		g.Nodes = append(g.Nodes, IPSet{CIDR: pveCIDR(n)})
	}
	g.Out = []IPSet{{CIDR: "0.0.0.0/1"}, {CIDR: "128.0.0.0/1"}}
	for _, d := range deny {
		g.Out = append(g.Out, IPSet{CIDR: pveCIDR(d), NoMatch: 1})
	}
	const nodeRef, outRef = "+dc/@n", "+dc/@o"
	rule := func(dir, proto, src, dst, dport string) FirewallRule {
		r := FirewallRule{Type: dir, Action: "ACCEPT", Proto: proto, DPort: dport, Enable: 1, Comment: sandboxComment}
		if dir == "out" {
			r.Dest = dst
		} else {
			r.Source = src
		}
		return r
	}
	rules := []FirewallRule{
		// its control plane: API and konnectivity (exec and logs ride konnectivity, so nothing comes in for them)
		rule("out", "tcp", "", cp.String(), strconv.Itoa(apiPort)+","+strconv.Itoa(sandboxKonnectivityPort)),
	}
	if len(dns) > 0 {
		d := strings.Join(cidrList(dns), ",")
		rules = append(rules, rule("out", "udp", "", d, "53"), rule("out", "tcp", "", d, "53"))
	}
	// Between sandbox workers only: Cilium VXLAN and health, MetalLB memberlist, and the kubelet. The kubelet one is
	// konnectivity's: Kamaji runs its server with the default proxy strategy, so exec and logs for a pod on one worker
	// can leave through the agent on another (the kubelet still wants an API server credential).
	for _, dir := range []string{"out", "in"} {
		rules = append(rules,
			rule(dir, "udp", nodeRef, nodeRef, "8472"),
			rule(dir, "tcp", nodeRef, nodeRef, "4240"),
			rule(dir, "icmp", nodeRef, nodeRef, ""),
			rule(dir, "tcp", nodeRef, nodeRef, "7946"),
			rule(dir, "udp", nodeRef, nodeRef, "7946"),
			rule(dir, "tcp", nodeRef, nodeRef, "10250"),
		)
	}
	if len(kubelet) > 0 {
		rules = append(rules, rule("in", "tcp", strings.Join(cidrList(kubelet), ","), "", "10250"))
	}
	if f.ProxyAddress != "" || f.EntryFrom != "" {
		r, err := f.entryRule(subnet, nodes, lbs)
		if err != nil {
			return g, err
		}
		rules = append(rules, r)
	}
	for _, ep := range []struct {
		what, addr string
		port, def  int
	}{{"registry", f.RegistryAddress, f.RegistryPort, 443}, {"build storage", f.BuildStorageAddress, f.BuildStoragePort, 443}} {
		if ep.addr == "" {
			continue
		}
		a, err := netip.ParseAddr(strings.TrimSpace(ep.addr))
		port := ep.port
		if port == 0 {
			port = ep.def
		}
		if err != nil || !a.Is4() || subnet.Contains(a) || a == cp || port < 1 || port > 65535 {
			return g, fmt.Errorf("pve sandbox: invalid %s %s:%d (IPv4 outside the sandbox subnet, not the control plane)", ep.what, ep.addr, port)
		}
		rules = append(rules, rule("out", "tcp", "", a.String(), strconv.Itoa(port)))
	}
	rules = append(rules, rule("out", "", "", outRef, "")) // the internet, through the sandbox SNAT address

	content, _ := json.Marshal([]any{g.Nodes, g.Out, rules})
	sum := sha256.Sum256(content)
	g.Group = sandboxPrefix(f.Pool) + hex.EncodeToString(sum[:4])
	g.NodeSet, g.OutSet = g.Group+"-n", g.Group+"-o"
	for _, r := range rules {
		r.Source = strings.NewReplacer("@n", g.NodeSet).Replace(r.Source)
		r.Dest = strings.NewReplacer("@n", g.NodeSet, "@o", g.OutSet).Replace(r.Dest)
		g.Rules = append(g.Rules, r)
	}
	return g, nil
}

// entryRule lets the entry appliance, and nothing else, reach sandbox-proxy. Whichever worker answers for the
// address (MetalLB L2) receives it; kube-proxy delivers it to the proxy pod there.
func (f SandboxFirewall) entryRule(subnet netip.Prefix, nodes, lbs []netip.Prefix) (FirewallRule, error) {
	proxy, err1 := netip.ParseAddr(strings.TrimSpace(f.ProxyAddress))
	from, err2 := netip.ParseAddr(strings.TrimSpace(f.EntryFrom))
	port := f.ProxyPort
	if port == 0 {
		port = 443
	}
	host := func(a netip.Addr) []netip.Prefix { return []netip.Prefix{netip.PrefixFrom(a, 32)} }
	switch {
	case err1 != nil || !proxy.Is4() || !within(lbs, host(proxy)):
		return FirewallRule{}, fmt.Errorf("pve sandbox: proxy address %q must be one of the load balancer addresses", f.ProxyAddress)
	case err2 != nil || !from.Is4() || !subnet.Contains(from) || overlap(host(from), nodes) || overlap(host(from), lbs):
		return FirewallRule{}, fmt.Errorf("pve sandbox: entry address %q must be inside %s, off the worker and load balancer addresses", f.EntryFrom, subnet)
	case port < 1 || port > 65535:
		return FirewallRule{}, fmt.Errorf("pve sandbox: invalid proxy port %d", port)
	}
	return FirewallRule{Type: "in", Action: "ACCEPT", Proto: "tcp", Source: from.String(), Dest: proxy.String(),
		DPort: strconv.Itoa(port), Enable: 1, Comment: sandboxComment}, nil
}

// sandboxFirewallOptions: anti-spoofing on, both policies DROP, no DHCP (CAPMOX gives static addresses) and no IPv6.
var sandboxFirewallOptions = map[string]string{
	"enable": "1", "ipfilter": "1", "macfilter": "1", "dhcp": "0", "ndp": "0", "radv": "0",
	"policy_in": "DROP", "policy_out": "DROP",
}

// vmFirewallDefaults is what PVE means by an option it leaves out of GET .../firewall/options.
var vmFirewallDefaults = map[string]any{"enable": 0, "ipfilter": 0, "macfilter": 1, "dhcp": 0, "ndp": 0, "radv": 0}

// SandboxWorker is one VM of the sandbox pool.
type SandboxWorker struct {
	Node string
	VMID int
	Name string
}

// SandboxPass is one reconcile of the sandbox workers' firewall (StartSandboxPass, then Secure per worker, then Sweep).
type SandboxPass struct {
	s       *Service
	g       sandboxGen
	vnet    string
	nodes   []netip.Prefix
	lbs     []netip.Prefix
	on      bool // the datacenter firewall is on
	Workers []SandboxWorker
}

// StartSandboxPass lists the pool's workers and makes the current generation exist.
func (s *Service) StartSandboxPass(ctx context.Context, f SandboxFirewall) (*SandboxPass, error) {
	p, err := s.newSandboxPass(ctx, f)
	if err != nil {
		return nil, err
	}
	g := p.g
	inUse := false
	for _, w := range p.Workers {
		groups, err := p.groupsOf(ctx, w)
		if err != nil {
			return nil, err
		}
		inUse = inUse || slices.Contains(groups, g.Group)
	}
	if err := s.ensureSandboxGeneration(ctx, g, inUse); err != nil {
		return nil, err
	}
	return p, nil
}

// newSandboxPass reads what a pass works on (the generation, the datacenter firewall switch, the vnet and the pool's
// workers) and writes nothing.
func (s *Service) newSandboxPass(ctx context.Context, f SandboxFirewall) (*SandboxPass, error) {
	g, err := f.generation()
	if err != nil {
		return nil, err
	}
	p := &SandboxPass{s: s, g: g}
	if p.on, err = s.C.DatacenterFirewallEnabled(ctx); err != nil {
		return nil, err
	}
	p.nodes, _ = toCIDRs(f.Nodes)
	p.lbs, _ = toCIDRs(f.LoadBalancers)
	if p.vnet, err = s.vnetOfSubnet(ctx, f.Subnet); err != nil {
		return nil, err
	}
	vms, err := s.C.ClusterResources(ctx, "vm")
	if err != nil {
		return nil, err
	}
	for _, vm := range vms {
		if vm.Pool == f.Pool && vm.Template == 0 && !vm.NodeDown && vm.Type == "qemu" {
			p.Workers = append(p.Workers, SandboxWorker{Node: vm.Node, VMID: vm.VMID, Name: vm.Name})
		}
	}
	return p, nil
}

// CheckSandboxFence is the worker-fence release gate (docs/managed-sandbox.md §3): read-only, with the portal's token, it
// checks that PVE enforces VM firewalls, that the current generation's IP sets and group hold exactly what the pass
// would write, and that every worker open to tenant pods (open: Node name → the InternalIP its Node reports, for Nodes
// without the unsecured taint) is a VM of the pool whose firewall reads back as the pass leaves it, with its filter
// pinned to that address. A tainted worker takes no tenant pod (NodeRestriction keeps the kubelet from dropping the
// taint), so it is not asked for. At least one worker must be open.
func (s *Service) CheckSandboxFence(ctx context.Context, f SandboxFirewall, open map[string]string) error {
	p, err := s.newSandboxPass(ctx, f)
	if err != nil {
		return err
	}
	if !p.on {
		return ErrSandboxUnenforced
	}
	if err := s.checkSandboxGeneration(ctx, p.g); err != nil {
		return err
	}
	if len(open) == 0 {
		return errors.New("pve sandbox: no worker is open to sandboxes yet (every Node still has the unsecured taint)")
	}
	byName := map[string]SandboxWorker{}
	for _, w := range p.Workers {
		byName[w.Name] = w
	}
	for _, name := range slices.Sorted(maps.Keys(open)) {
		w, ok := byName[name]
		if !ok {
			return fmt.Errorf("pve sandbox: Node %s takes tenant pods but is no VM of pool %s", name, f.Pool)
		}
		st, changed, err := p.secure(ctx, w, "", false)
		if err != nil {
			return fmt.Errorf("pve sandbox: worker %s (VM %d): %w", name, w.VMID, err)
		}
		if changed {
			return fmt.Errorf("pve sandbox: worker %s (VM %d) does not carry the current firewall (group %s)", name, w.VMID, p.g.Group)
		}
		if st.Pinned == "" || st.Pinned != open[name] {
			return fmt.Errorf("pve sandbox: worker %s (VM %d) filter admits %q, its Node reports %q", name, w.VMID, st.Pinned, open[name])
		}
	}
	return nil
}

// checkSandboxGeneration is the read-only half of ensureSandboxGeneration: anything missing or different is an error.
func (s *Service) checkSandboxGeneration(ctx context.Context, g sandboxGen) error {
	sets, err := s.C.ListClusterIPSets(ctx)
	if err != nil {
		return err
	}
	for _, set := range []struct {
		name    string
		entries []IPSet
	}{{g.NodeSet, g.Nodes}, {g.OutSet, g.Out}} {
		if !slices.ContainsFunc(sets, func(x IPSet) bool { return x.Name == set.name }) {
			return fmt.Errorf("pve sandbox: ip set %s does not exist", set.name)
		}
		have, err := s.C.ListClusterIPSetEntries(ctx, set.name)
		if err != nil {
			return err
		}
		if !sameEntries(have, set.entries) {
			return fmt.Errorf("%w: ip set %s", ErrSandboxDrift, set.name)
		}
	}
	have, err := s.C.ListFirewallRules(ctx, g.Group)
	if err != nil {
		return err
	}
	if !sameGroupRules(have, g.Rules) {
		return fmt.Errorf("%w: group %s", ErrSandboxDrift, g.Group)
	}
	return nil
}

// Group is the current generation's security group.
func (p *SandboxPass) Group() string { return p.g.Group }

func (s *Service) vnetOfSubnet(ctx context.Context, subnet string) (string, error) {
	want, err := netip.ParsePrefix(subnet)
	if err != nil {
		return "", fmt.Errorf("pve sandbox: invalid subnet %q", subnet)
	}
	vnets, err := s.C.ListSDNVnets(ctx)
	if err != nil {
		return "", err
	}
	for _, v := range vnets {
		subs, err := s.C.ListSDNSubnets(ctx, v.Vnet)
		if err != nil {
			return "", err
		}
		for _, sub := range subs {
			if p, err := netip.ParsePrefix(sub.CIDR); err == nil && p.Masked() == want.Masked() {
				return v.Vnet, nil
			}
		}
	}
	return "", fmt.Errorf("pve sandbox: no vnet holds subnet %s", subnet)
}

func (p *SandboxPass) groupsOf(ctx context.Context, w SandboxWorker) ([]string, error) {
	rules, err := p.s.C.ListVMFirewallRules(ctx, w.Node, w.VMID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rules {
		if r.Type == "group" {
			out = append(out, r.Action)
		}
	}
	return out, nil
}

func sameEntries(have, want []IPSet) bool {
	key := func(e IPSet) string { return strings.TrimSuffix(e.CIDR, "/32") + "!" + strconv.Itoa(e.NoMatch) }
	a, b := make([]string, 0, len(have)), make([]string, 0, len(want))
	for _, e := range have {
		a = append(a, key(e))
	}
	for _, e := range want {
		b = append(b, key(e))
	}
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func sameGroupRules(have, want []FirewallRule) bool {
	if len(have) != len(want) {
		return false
	}
	for i := range want { // PVE lists a group top down, the order the rules were added in reverse
		h, w := have[i], want[i]
		if h.Type != w.Type || h.Action != w.Action || h.Proto != w.Proto || h.Source != w.Source || h.Dest != w.Dest ||
			h.DPort != w.DPort || h.Enable != w.Enable || h.Comment != w.Comment {
			return false
		}
	}
	return true
}

// ensureSandboxGeneration makes the generation's sets and group exist with exactly its content. A copy that differs
// is rebuilt while no worker uses it; one in use is ErrSandboxDrift.
func (s *Service) ensureSandboxGeneration(ctx context.Context, g sandboxGen, inUse bool) error {
	sets, err := s.C.ListClusterIPSets(ctx)
	if err != nil {
		return err
	}
	for _, set := range []struct {
		name    string
		entries []IPSet
	}{{g.NodeSet, g.Nodes}, {g.OutSet, g.Out}} {
		if slices.ContainsFunc(sets, func(x IPSet) bool { return x.Name == set.name }) {
			have, err := s.C.ListClusterIPSetEntries(ctx, set.name)
			if err != nil {
				return err
			}
			if sameEntries(have, set.entries) {
				continue
			}
			if inUse {
				return fmt.Errorf("%w: ip set %s", ErrSandboxDrift, set.name)
			}
			if err := s.C.DeleteClusterIPSet(ctx, set.name); err != nil {
				return err
			}
		}
		if err := s.C.CreateClusterIPSet(ctx, set.name, sandboxComment); err != nil {
			return err
		}
		for _, e := range set.entries {
			if err := s.C.AddClusterIPSetEntry(ctx, set.name, e.CIDR, e.NoMatch == 1); err != nil {
				return err
			}
		}
	}
	groups, err := s.C.ListFirewallGroups(ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(groups, func(x FirewallGroup) bool { return x.Group == g.Group }) {
		have, err := s.C.ListFirewallRules(ctx, g.Group)
		if err != nil {
			return err
		}
		if sameGroupRules(have, g.Rules) {
			return nil
		}
		if inUse {
			return fmt.Errorf("%w: group %s", ErrSandboxDrift, g.Group)
		}
		if err := s.deleteGroup(ctx, g.Group, len(have)); err != nil {
			return err
		}
	}
	if err := s.C.CreateFirewallGroup(ctx, g.Group, sandboxComment); err != nil {
		return err
	}
	for i := len(g.Rules) - 1; i >= 0; i-- { // each new rule lands on top
		if err := s.C.AddFirewallRule(ctx, g.Group, g.Rules[i]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) deleteGroup(ctx context.Context, group string, rules int) error {
	for range rules {
		if err := s.C.DeleteFirewallRule(ctx, group, 0); err != nil {
			return err
		}
	}
	return s.C.DeleteFirewallGroup(ctx, group)
}

// SandboxWorkerState is a worker after Secure.
type SandboxWorkerState struct {
	// Pinned is the one worker address its filter admits; "" while the filter still admits the whole worker range.
	Pinned string
	// Secured: read back after the changes, every NIC has firewall=1, the options and filters are as wanted and the
	// current group is the VM's only rule.
	Secured bool
}

// Secure brings one worker to the current generation: IP filters, the group as its only rule, the options, then
// firewall=1 on every NIC (until that flag is on nothing of the rest applies, so it goes last). nodeAddr is the
// address the worker's Node reports. The caller passes it only while the worker still has its unsecured taint (the
// kubelet writes its Node, and once tenant code runs there it may lie), "" otherwise; see ipfilterFor.
func (p *SandboxPass) Secure(ctx context.Context, w SandboxWorker, nodeAddr string) (SandboxWorkerState, error) {
	st, changed, err := p.secure(ctx, w, nodeAddr, true)
	if err == nil && changed { // read it all back after the changes
		st, changed, err = p.secure(ctx, w, nodeAddr, false)
	}
	if err == nil && !p.on {
		err = ErrSandboxUnenforced
	}
	st.Secured = err == nil && !changed
	return st, err
}

// secure checks the worker against the generation and, with apply, fixes what differs. changed reports a difference.
func (p *SandboxPass) secure(ctx context.Context, w SandboxWorker, nodeAddr string, apply bool) (st SandboxWorkerState, changed bool, err error) {
	c := p.s.C
	cfg, err := c.GetVMConfig(ctx, w.Node, w.VMID)
	if err != nil {
		return st, false, err
	}
	nics := nicKeys(cfg)
	slices.Sort(nics)
	sets, err := c.ListVMIPSets(ctx, w.Node, w.VMID)
	if err != nil {
		return st, false, err
	}
	for _, nic := range nics {
		name := "ipfilter-" + nic
		var have []IPSet
		exists := slices.ContainsFunc(sets, func(x IPSet) bool { return x.Name == name })
		if exists {
			if have, err = c.ListVMIPSetEntries(ctx, w.Node, w.VMID, name); err != nil {
				return st, false, err
			}
		}
		var want []string // a NIC off the sandbox vnet gets an empty set: PVE then drops every IPv4 frame it sends
		if nicBridge(cfg.Str(nic)) == p.vnet {
			want, st.Pinned = p.ipfilterFor(have, nodeAddr)
		}
		wantSet := make([]IPSet, 0, len(want))
		for _, cidr := range want {
			wantSet = append(wantSet, IPSet{CIDR: cidr})
		}
		if exists && sameEntries(have, wantSet) {
			continue
		}
		changed = true
		if apply {
			if err := p.s.SetIPFilter(ctx, w.Node, w.VMID, nic, want); err != nil {
				return st, changed, err
			}
		}
	}

	isOurs := func(r FirewallRule) bool { return r.Type == "group" && r.Action == p.g.Group && r.Enable == 1 }
	// extra is the last rule that is not the one reference to the current group (the old generation's included).
	extra := func(rules []FirewallRule) int {
		for i := len(rules) - 1; i >= 0; i-- {
			if !isOurs(rules[i]) || slices.ContainsFunc(rules[:i], isOurs) {
				return i
			}
		}
		return -1
	}
	rules, err := c.ListVMFirewallRules(ctx, w.Node, w.VMID)
	if err != nil {
		return st, changed, err
	}
	if !slices.ContainsFunc(rules, isOurs) {
		changed = true
		if !apply {
			return st, changed, nil
		}
		if err := c.AddVMGroupRule(ctx, w.Node, w.VMID, p.g.Group); err != nil {
			return st, changed, err
		}
		if rules, err = c.ListVMFirewallRules(ctx, w.Node, w.VMID); err != nil {
			return st, changed, err
		}
	}
	for i := extra(rules); i >= 0; i = extra(rules) { // one at a time: each delete changes the digest
		changed = true
		if !apply {
			break
		}
		if err := c.DeleteVMFirewallRule(ctx, w.Node, w.VMID, rules[i].Pos, rules[i].Digest); err != nil {
			return st, changed, err
		}
		if rules, err = c.ListVMFirewallRules(ctx, w.Node, w.VMID); err != nil {
			return st, changed, err
		}
	}

	opts, err := c.GetVMFirewallOptions(ctx, w.Node, w.VMID)
	if err != nil {
		return st, changed, err
	}
	for k, v := range sandboxFirewallOptions {
		have, ok := opts[k]
		if !ok {
			have = vmFirewallDefaults[k]
		}
		if fmt.Sprint(have) != v {
			changed = true
			if apply {
				if err := c.SetVMFirewallOptions(ctx, w.Node, w.VMID, sandboxFirewallOptions); err != nil {
					return st, changed, err
				}
			}
			break
		}
	}

	if params := nicFirewallParams(cfg); len(params) > 0 {
		changed = true
		if apply {
			upid, err := c.SetVMConfig(ctx, w.Node, w.VMID, params)
			if err == nil {
				err = c.WaitTask(ctx, upid)
			}
			if err != nil {
				return st, changed, err
			}
		}
	}
	return st, changed, nil
}

// ipfilterFor is the filter of a worker's sandbox NIC. The Node object is the kubelet's to write, so its address is
// trusted once, before the worker takes tenant pods (the caller passes it only while the unsecured taint is on), and
// then pinned: a host address inside the worker range that the set already holds stays, whatever the Node says later. Before that the whole worker range is
// admitted. The load balancer addresses are always in, since any worker may answer for them.
func (p *SandboxPass) ipfilterFor(have []IPSet, nodeAddr string) (want []string, pinned string) {
	inRange := func(a netip.Addr) bool {
		in := func(ps []netip.Prefix) bool {
			return slices.ContainsFunc(ps, func(x netip.Prefix) bool { return x.Contains(a) })
		}
		return in(p.nodes) && !in(p.lbs)
	}
	// Pinned means the set holds exactly one worker-range entry and it is a host. The unpinned set can end in a host
	// too (a range .10-.20 is .10/31 .12/30 .16/30 .20), so a lone host among other range entries is not a pin.
	var held []netip.Prefix
	for _, e := range have {
		raw := e.CIDR
		if !strings.Contains(raw, "/") {
			raw += "/32"
		}
		if pf, err := netip.ParsePrefix(raw); err == nil && inRange(pf.Addr()) {
			held = append(held, pf)
		}
	}
	if len(held) == 1 && held[0].Bits() == 32 {
		pinned = held[0].Addr().String()
	} else if a, err := netip.ParseAddr(nodeAddr); err == nil && a.Is4() && inRange(a) {
		pinned = a.String()
	}
	if pinned != "" {
		want = append(want, pinned)
	} else {
		want = append(want, cidrList(p.nodes)...)
	}
	return append(want, cidrList(p.lbs)...), pinned
}

// Sweep removes the older generations of this pool once no worker references them.
func (p *SandboxPass) Sweep(ctx context.Context) error {
	used := map[string]bool{p.g.Group: true}
	for _, w := range p.Workers {
		groups, err := p.groupsOf(ctx, w)
		if err != nil {
			return err
		}
		for _, g := range groups {
			used[g] = true
		}
	}
	prefix := p.g.Group[:len(sandboxPrefix(""))]
	groups, err := p.s.C.ListFirewallGroups(ctx)
	if err != nil {
		return err
	}
	for _, g := range groups {
		if !strings.HasPrefix(g.Group, prefix) || g.Comment != sandboxComment || used[g.Group] {
			continue
		}
		rules, err := p.s.C.ListFirewallRules(ctx, g.Group)
		if err != nil {
			return err
		}
		if err := p.s.deleteGroup(ctx, g.Group, len(rules)); err != nil {
			return err
		}
	}
	sets, err := p.s.C.ListClusterIPSets(ctx)
	if err != nil {
		return err
	}
	for _, set := range sets {
		group, _, _ := strings.Cut(set.Name, "-")
		if strings.HasPrefix(set.Name, prefix) && set.Comment == sandboxComment && !used[group] {
			if err := p.s.C.DeleteClusterIPSet(ctx, set.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

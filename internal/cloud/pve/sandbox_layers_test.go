package pve

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The two layers around a managed platform's sandbox machine, held against the packet list of the drill of
// 2026-11-26 (v0120lab/z2/d/probe-machine.txt), on the lab's addresses:
//
//	tenant pool 10.121.0.0/16, management network 10.88.0.0/24 (the Proxmox nodes .11 to .13), platform network
//	10.90.0.0/24 with the blocked network 10.90.0.0/26 (gateway .1, portal VM .2, seed .5, API address .10, node range
//	.20 to .60), the MetalLB range from .100, sandbox machines 10.90.0.64/28.
//
// Layer 1 is the machine's VM firewall (the generation's ACCEPTs under policy DROP both ways). Layer 0 is the cluster
// firewall's forward chain as a node with net.bridge.bridge-nf-call-iptables = 1 applies it: to every routed packet
// AND to every frame bridged between two guests of one bridge, which is how pve3 cut VM 119 off its cluster. The two
// evaluators below are the test's own (the firewall's first match on one packet); they are a model of Proxmox, not
// Proxmox: what a node really does with these rules is the lab drill's to show.

const (
	labPool     = "10.121.0.0/16"
	labSandbox  = "10.90.0.64/28"
	labAPI      = "10.90.0.10"
	labPeers    = "10.90.0.20-10.90.0.60"
	labOwner    = "proxcloud:isolation/pcm-"
	labSeparate = "10.90.0.100" // the control plane of pc-sandbox: a MetalLB address of the platform cluster
)

var labBlocked = []string{"10.88.0.0/24", "10.90.0.0/26"}

// labDeny is what the sync's sandboxDeny adds for this site: the management network, each node's own address, the
// blocked networks, the tenant pool, and the platform prefix of config.sandbox.deny.
var labDeny = []string{"10.88.0.0/24", "10.88.0.11", "10.88.0.12", "10.88.0.13", "10.90.0.0/26", labPool, "10.90.0.0/24"}

func labShared() SandboxFirewall {
	return SandboxFirewall{Pool: SandboxPool, Subnet: labSandbox, Nodes: []string{labSandbox}, ControlPlane: labAPI, APIPort: 6443,
		DNS: []string{"1.1.1.1"}, KubeletFrom: []string{labPeers}, Peers: []string{labPeers}, Deny: labDeny, Bridge: "vmbr1"}
}

func labSeparateFW() SandboxFirewall {
	return SandboxFirewall{Pool: SandboxPool, Subnet: labSandbox, Nodes: []string{labSandbox}, LoadBalancers: []string{"10.90.0.90"},
		ControlPlane: labSeparate, APIPort: 6443, DNS: []string{"1.1.1.1"}, Deny: labDeny, ProxyAddress: "10.90.0.90", ProxyPort: 443, Bridge: "vmbr1"}
}

// labClusterRules is the cluster firewall of the site: what another stack and an operator hold there (never this
// portal's to touch), with this portal's provider-wide isolation rules on top, as a rebuild leaves them.
func labClusterRules(allow ...string) []FirewallRule {
	rules := isolationRules(labPool, labBlocked, []string{labSandbox}, labOwner, allow...)
	rules = append(rules, foreignRules()...)
	for i := range rules {
		rules[i].Pos = i
	}
	return rules
}

// foreignRules are rules of other owners, after the lab's: the platform's own PVE API rule, another stack's
// isolation set under its own prefix (with a sandbox network of its own), and an operator's rule.
func foreignRules() []FirewallRule {
	const other = "proxcloud:isolation/sb8-"
	return []FirewallRule{
		{Type: "in", Action: "ACCEPT", Proto: "tcp", Source: "10.90.0.5,10.90.0.20/30", Dest: "10.88.0.0/24", DPort: "8006", Enable: 1, Comment: "proxcloud:pve-api:pi-6cef1244"},
		{Type: "forward", Action: "DROP", Source: "10.120.0.0/16", Dest: "10.120.0.0/16", Enable: 1, Comment: other},
		{Type: "in", Action: "DROP", Source: "10.120.0.0/16", Dest: "10.120.0.0/16", Enable: 1, Comment: other},
		{Type: "forward", Action: "DROP", Source: "10.120.0.0/16", Dest: "10.88.0.0/24,10.110.0.0/24", Enable: 1, Comment: other},
		{Type: "forward", Action: "ACCEPT", Proto: "tcp", Source: "10.110.0.0/24", Dest: "10.88.0.41", DPort: "9000", Enable: 1, Comment: other},
		{Type: "forward", Action: "DROP", Source: "10.110.0.0/24", Dest: "10.120.0.0/16,10.88.0.0/24", Enable: 1, Comment: other},
		{Type: "in", Action: "ACCEPT", Proto: "tcp", DPort: "22", Source: "10.200.101.0/24", Enable: 1, Comment: "ops ssh"},
	}
}

type packet struct {
	what, src, dst, proto string
	port                  int
}

func inList(list string, a netip.Addr) bool {
	if list == "" {
		return true
	}
	ps, _ := toCIDRs(strings.Split(list, ","))
	return slices.ContainsFunc(ps, func(p netip.Prefix) bool { return p.Contains(a) })
}

func portIn(list string, port int) bool {
	rs, ok := portRanges(list)
	return list == "" || ok && slices.ContainsFunc(rs, func(r [2]int) bool { return r[0] <= port && port <= r[1] })
}

// forwardChain is the node's forward chain on one packet: the first enabled forward rule that matches decides, and a
// packet no rule matches passes.
func forwardChain(rules []FirewallRule, p packet) string {
	src, dst := netip.MustParseAddr(p.src), netip.MustParseAddr(p.dst)
	for _, r := range rules {
		if r.Enable == 1 && r.Type == "forward" && (r.Proto == "" || r.Proto == p.proto) && inList(r.Source, src) && inList(r.Dest, dst) && portIn(r.DPort, p.port) {
			return map[bool]string{true: "allow", false: "drop"}[r.Action == "ACCEPT"]
		}
	}
	return "pass"
}

// vmFirewall is the worker's VM firewall on one packet: out when the worker sends it, in when it receives it. Every
// rule of the group is an ACCEPT and both policies are DROP.
func vmFirewall(t *testing.T, g sandboxGen, p packet) string {
	t.Helper()
	range_ := netip.MustParsePrefix(labSandbox)
	src, dst := netip.MustParseAddr(p.src), netip.MustParseAddr(p.dst)
	dir, peer := "in", src
	if range_.Contains(src) {
		dir, peer = "out", dst
	}
	inSet := func(set []IPSet) bool {
		hit := false
		for _, e := range set {
			if inList(e.CIDR, peer) {
				if e.NoMatch == 1 {
					return false
				}
				hit = true
			}
		}
		return hit
	}
	for _, r := range g.Rules {
		if r.Action != "ACCEPT" {
			t.Fatalf("the group holds a rule that is no ACCEPT: %+v", r)
		}
		ref := r.Source + r.Dest
		var match bool
		switch ref {
		case "+dc/" + g.NodeSet:
			match = inSet(g.Nodes)
		case "+dc/" + g.OutSet:
			match = inSet(g.Out)
		default:
			match = inList(ref, peer)
			if dir == "in" && r.Source != "" && r.Dest != "" { // the entry rule: a source and the proxy's address
				match = inList(r.Source, src) && inList(r.Dest, dst)
			} else if dir == "in" && r.Dest != "" {
				match = inList(r.Dest, dst)
			}
		}
		if r.Type == dir && match && (r.Proto == "" || r.Proto == p.proto) && portIn(r.DPort, p.port) {
			return "allow"
		}
	}
	return "drop"
}

func TestSandboxLayersOnTheDrillsPackets(t *testing.T) {
	const worker, worker2, tenant = "10.90.0.64", "10.90.0.65", "10.121.0.5"
	out := func(what, dst, proto string, port int) packet { return packet{what, worker, dst, proto, port} }
	type row struct {
		packet
		vm, forward string
	}
	// What the machine must reach of its cluster, and what stays dropped at both layers: every other port of the API
	// address and of the cluster's machines, the seed, the portal VM, the gateway, the Proxmox nodes, the management
	// network, the tenant pool. A private destination that is no blocked network of the provider (the MetalLB range
	// tenants must reach, another stack, the pod and service ranges, the metadata address) is held by layer 1 alone:
	// layer 0 never dropped it, and passes it.
	shared := []row{
		{out("API address", labAPI, "tcp", 6443), "allow", "allow"},
		{out("API address, kubelet port", labAPI, "tcp", 10250), "drop", "drop"},
		{out("API address, ssh", labAPI, "tcp", 22), "drop", "drop"},
		{out("API address, etcd", labAPI, "tcp", 2379), "drop", "drop"},
		{out("API address, health port", labAPI, "tcp", 4240), "drop", "drop"},
		{out("API address, ping", labAPI, "icmp", 0), "drop", "drop"},
		{out("API address, konnectivity port", labAPI, "tcp", 8132), "drop", "drop"},
		{out("seed API", "10.90.0.5", "tcp", 6443), "drop", "drop"},
		{out("seed ssh", "10.90.0.5", "tcp", 22), "drop", "drop"},
		{out("portal VM", "10.90.0.2", "tcp", 443), "drop", "drop"},
		{out("portal VM ssh", "10.90.0.2", "tcp", 22), "drop", "drop"},
		{out("gateway ssh", "10.90.0.1", "tcp", 22), "drop", "drop"},
		{out("a free address of the blocked network", "10.90.0.15", "tcp", 6443), "drop", "drop"},
		{out("the address after the node range", "10.90.0.61", "tcp", 6443), "drop", "drop"},
		{out("pve1 API", "10.88.0.11", "tcp", 8006), "drop", "drop"},
		{out("pve1 ssh", "10.88.0.11", "tcp", 22), "drop", "drop"},
		{out("pve2 API", "10.88.0.12", "tcp", 8006), "drop", "drop"},
		{out("pve3 API", "10.88.0.13", "tcp", 8006), "drop", "drop"},
		{out("pve3 ssh", "10.88.0.13", "tcp", 22), "drop", "drop"},
		{out("another stack's bucket on the management network", "10.88.0.41", "tcp", 9000), "drop", "drop"},
		{out("tenant pool", "10.121.0.1", "tcp", 22), "drop", "drop"},
		{out("tenant pool, any port", "10.121.44.7", "udp", 53), "drop", "drop"},
		{out("MetalLB, free address", "10.90.0.100", "tcp", 443), "drop", "pass"},
		{out("MetalLB, sandbox-proxy", "10.90.0.140", "tcp", 443), "drop", "pass"},
		{out("management host outside the node networks", "10.200.101.211", "tcp", 22), "drop", "pass"},
		{out("old services cluster API", "10.89.0.200", "tcp", 6443), "drop", "pass"},
		{out("old services cluster kubelet", "10.89.0.201", "tcp", 10250), "drop", "pass"},
		{out("another stack's sandbox control plane", "10.89.0.214", "tcp", 6443), "drop", "pass"},
		{out("another stack's sandbox network", "10.110.0.10", "tcp", 22), "drop", "pass"},
		{out("metadata address", "169.254.169.254", "tcp", 80), "drop", "pass"},
		{out("ArgoCD service address", "10.96.5.139", "tcp", 443), "drop", "pass"},
		{out("kamaji-etcd service address", "10.96.227.1", "tcp", 2379), "drop", "pass"},
		{out("kamaji-etcd pod address", "10.244.3.203", "tcp", 2379), "drop", "pass"},
		{out("internet https", "1.1.1.1", "tcp", 443), "allow", "pass"},
		{out("internet submission", "142.250.4.108", "tcp", 587), "allow", "pass"},
		{out("internet ping", "1.1.1.1", "icmp", 0), "allow", "pass"},
		{out("internet smtp", "142.250.4.27", "tcp", 25), "allow", "pass"},
		{out("another sandbox machine, tunnel", worker2, "udp", 8472), "allow", "pass"},
		{packet{"kubelet from a cluster machine", "10.90.0.24", worker, "tcp", 10250}, "allow", "pass"},
		{packet{"tunnel from a cluster machine", "10.90.0.21", worker, "udp", 8472}, "allow", "pass"},
		{packet{"health from a cluster machine", "10.90.0.21", worker, "tcp", 4240}, "allow", "pass"},
		{packet{"ping from a cluster machine", "10.90.0.21", worker, "icmp", 0}, "allow", "pass"},
		{packet{"ssh from a cluster machine", "10.90.0.21", worker, "tcp", 22}, "drop", "pass"},
		{packet{"kubelet from the portal VM", "10.90.0.2", worker, "tcp", 10250}, "drop", "pass"},
		{packet{"kubelet from another sandbox machine", worker2, worker, "tcp", 10250}, "drop", "pass"},
		{packet{"a tenant to the machine", tenant, worker, "tcp", 10250}, "drop", "drop"},
		{packet{"a tenant to the machine, tunnel", tenant, worker, "udp", 8472}, "drop", "drop"},
		{packet{"a tenant to the seed", tenant, "10.90.0.5", "tcp", 6443}, "", "drop"},
		{packet{"a tenant to the API address", tenant, labAPI, "tcp", 6443}, "", "drop"},
		{packet{"a tenant to a cluster machine", tenant, "10.90.0.24", "tcp", 6443}, "", "drop"},
		{packet{"a tenant to another tenant", tenant, "10.121.9.9", "tcp", 22}, "", "drop"},
		{packet{"a tenant to MetalLB", tenant, "10.90.0.140", "tcp", 443}, "", "pass"},
	}
	// Each machine of the cluster, on each port the drill tried.
	for _, m := range []string{"10.90.0.20", "10.90.0.21", "10.90.0.23", "10.90.0.24", "10.90.0.60"} {
		shared = append(shared,
			row{out("cluster machine API", m, "tcp", 6443), "allow", "allow"},
			row{out("cluster machine tunnel", m, "udp", 8472), "allow", "allow"},
			row{out("cluster machine health", m, "tcp", 4240), "allow", "allow"},
			row{out("cluster machine ping", m, "icmp", 0), "allow", "allow"},
			row{out("cluster machine kubelet", m, "tcp", 10250), "drop", "drop"},
			row{out("cluster machine ssh", m, "tcp", 22), "drop", "drop"},
			row{out("cluster machine etcd", m, "tcp", 2379), "drop", "drop"},
			row{out("cluster machine hubble", m, "tcp", 4244), "drop", "drop"},
			row{out("cluster machine, tunnel port over tcp", m, "tcp", 8472), "drop", "drop"},
			row{out("cluster machine, API port over udp", m, "udp", 6443), "drop", "drop"},
		)
	}
	// pc-sandbox: its control plane is a Kamaji tenant control plane on the platform cluster, published as a
	// LoadBalancer Service at an address of the platform cluster's MetalLB range (10.90.0.100 on the lab), API on
	// 6443 and konnectivity on 8132. That range is no blocked network (tenants reach it), so layer 0 has no rule for
	// it and needs none. The platform cluster itself is not this machine's cluster: all of it stays dropped.
	separate := []row{
		{out("its control plane, API", labSeparate, "tcp", 6443), "allow", "pass"},
		{out("its control plane, konnectivity", labSeparate, "tcp", 8132), "allow", "pass"},
		{out("its control plane, another port", labSeparate, "tcp", 2379), "drop", "pass"},
		{out("its control plane, https", labSeparate, "tcp", 443), "drop", "pass"},
		{out("the platform cluster's API address", labAPI, "tcp", 6443), "drop", "drop"},
		{out("a platform machine, API", "10.90.0.24", "tcp", 6443), "drop", "drop"},
		{out("a platform machine, tunnel", "10.90.0.24", "udp", 8472), "drop", "drop"},
		{out("a platform machine, health", "10.90.0.24", "tcp", 4240), "drop", "drop"},
		{out("a platform machine, ping", "10.90.0.24", "icmp", 0), "drop", "drop"},
		{out("the kamaji machine, kubelet", "10.90.0.23", "tcp", 10250), "drop", "drop"},
		{out("seed API", "10.90.0.5", "tcp", 6443), "drop", "drop"},
		{out("portal VM", "10.90.0.2", "tcp", 443), "drop", "drop"},
		{out("pve3 API", "10.88.0.13", "tcp", 8006), "drop", "drop"},
		{out("tenant pool", "10.121.0.1", "tcp", 22), "drop", "drop"},
		{out("another MetalLB address", "10.90.0.140", "tcp", 443), "drop", "pass"},
		{out("internet https", "1.1.1.1", "tcp", 443), "allow", "pass"},
		{out("internet smtp", "142.250.4.27", "tcp", 25), "allow", "pass"},
		{out("another sandbox machine, tunnel", worker2, "udp", 8472), "allow", "pass"},
		{out("another sandbox machine, kubelet", worker2, "tcp", 10250), "allow", "pass"},
		{out("another sandbox machine, memberlist", worker2, "tcp", 7946), "allow", "pass"},
		{packet{"a client to sandbox-proxy", "198.51.100.7", "10.90.0.90", "tcp", 443}, "allow", "pass"},
		{packet{"a platform machine to the kubelet", "10.90.0.24", worker, "tcp", 10250}, "drop", "pass"},
		{packet{"a tenant to the machine", tenant, worker, "tcp", 10250}, "drop", "drop"},
	}

	for name, c := range map[string]struct {
		fw    SandboxFirewall
		allow []string
		rows  []row
	}{
		"shared":   {labShared(), SandboxClusterAllow(labAPI+":6443", []string{labPeers}), shared},
		"separate": {labSeparateFW(), SandboxClusterAllow("", nil), separate},
	} {
		t.Run(name, func(t *testing.T) {
			g, err := c.fw.generation()
			if err != nil {
				t.Fatal(err)
			}
			rules := labClusterRules(c.allow...)
			for _, r := range c.rows {
				if got := forwardChain(rules, r.packet); got != r.forward {
					t.Errorf("layer 0, %s (%s %d, %s to %s): %s, want %s", r.what, r.proto, r.port, r.src, r.dst, got, r.forward)
				}
				if r.vm != "" {
					if got := vmFirewall(t, g, r.packet); got != r.vm {
						t.Errorf("layer 1, %s (%s %d, %s to %s): %s, want %s", r.what, r.proto, r.port, r.src, r.dst, got, r.vm)
					}
				}
				// The gate's own reading of the rule list says the same of every packet.
				port := ""
				if r.port > 0 {
					port = strconv.Itoa(r.port)
				}
				verdict, _ := forwardVerdict(rules, netip.MustParsePrefix(r.src+"/32"), netip.MustParsePrefix(r.dst+"/32"), r.proto, port)
				if got := map[string]string{"": "pass", "ACCEPT": "allow", "DROP": "drop"}[verdict]; got != r.forward {
					t.Errorf("forwardVerdict, %s: %s, want %s", r.what, got, r.forward)
				}
			}
			// What layer 0 lets a sandbox machine open inside a blocked network is exactly what layer 1 opens there:
			// every ACCEPT of the sandbox range is one flow of the list the VM firewall is made from, and no other.
			var accepts []string
			for _, r := range rules {
				if r.Comment == labOwner && r.Action == "ACCEPT" {
					if r.Type != "forward" || r.Source != labSandbox {
						t.Errorf("an ACCEPT that is not FORWARD from the sandbox range: %+v", r)
					}
					accepts = append(accepts, strings.TrimSpace(r.Proto+" "+r.Dest+" "+r.DPort))
				}
			}
			if !slices.Equal(accepts, c.allow) {
				t.Errorf("layer 0 accepts %q, the list says %q", accepts, c.allow)
			}
			if err := sandboxReach(rules, g.workers, g.flows); err != nil {
				t.Errorf("the gate's reach over the rules as written: %v", err)
			}
		})
	}

	// The control, as the lab had it: the rules v0.12.0-rc.1 wrote, with no ACCEPT of the machine's own cluster. The
	// forward chain drops the API and every cluster machine, and the gate says which rule does it.
	g, _ := labShared().generation()
	old := labClusterRules()
	for _, p := range []packet{out("API address", labAPI, "tcp", 6443).packet(), out("cluster machine API", "10.90.0.24", "tcp", 6443).packet(), out("tunnel", "10.90.0.24", "udp", 8472).packet()} {
		if got := forwardChain(old, p); got != "drop" {
			t.Fatalf("rc.1's rules on %s: %s; the lab saw every one of them time out", p.what, got)
		}
	}
	err := sandboxReach(old, g.workers, g.flows)
	if err == nil || !strings.Contains(err.Error(), "rule 4") || !strings.Contains(err.Error(), "tcp 6443 from "+labSandbox+" to "+labAPI) || !strings.Contains(err.Error(), "bridge-nf-call-iptables") {
		t.Fatalf("the gate on rc.1's rules: %v", err)
	}
	// Separate, on a site whose blocked networks hold the MetalLB address of its control plane: cut off as well, and said.
	sg, _ := labSeparateFW().generation()
	wide := isolationRules(labPool, []string{"10.88.0.0/24", "10.90.0.0/24"}, []string{labSandbox}, labOwner)
	if err := sandboxReach(wide, sg.workers, sg.flows); err == nil || !strings.Contains(err.Error(), "to "+labSeparate) {
		t.Fatalf("separate with its control plane inside a blocked network: %v", err)
	}
}

func (p packet) packet() packet { return p }

// What forwardVerdict must not take for an answer: a disabled rule, a rule of another direction, an ACCEPT that
// covers only part of a flow, and a rule whose match is not in the list.
func TestForwardVerdictReadsOnlyWhatARuleSays(t *testing.T) {
	src, dst := netip.MustParsePrefix(labSandbox), netip.MustParsePrefix("10.90.0.24/29")
	drop := FirewallRule{Type: "forward", Action: "DROP", Source: labSandbox, Dest: "10.90.0.0/26", Enable: 1}
	accept := FirewallRule{Type: "forward", Action: "ACCEPT", Proto: "tcp", Source: labSandbox, Dest: "10.90.0.24/29", DPort: "6443", Enable: 1}
	for name, c := range map[string]struct {
		rules []FirewallRule
		want  string
	}{
		"the ACCEPT above the DROP":             {[]FirewallRule{accept, drop}, "ACCEPT"},
		"the DROP above the ACCEPT":             {[]FirewallRule{drop, accept}, "DROP"},
		"a disabled ACCEPT":                     {[]FirewallRule{with(accept, func(r *FirewallRule) { r.Enable = 0 }), drop}, "DROP"},
		"an ACCEPT of the IN direction":         {[]FirewallRule{with(accept, func(r *FirewallRule) { r.Type = "in" }), drop}, "DROP"},
		"an ACCEPT of half the destination":     {[]FirewallRule{with(accept, func(r *FirewallRule) { r.Dest = "10.90.0.24/30" }), drop}, "DROP"},
		"an ACCEPT of one address of the range": {[]FirewallRule{with(accept, func(r *FirewallRule) { r.Source = "10.90.0.64" }), drop}, "DROP"},
		"an ACCEPT of another port":             {[]FirewallRule{with(accept, func(r *FirewallRule) { r.DPort = "443" }), drop}, "DROP"},
		"an ACCEPT of a port range":             {[]FirewallRule{with(accept, func(r *FirewallRule) { r.DPort = "6000:7000" }), drop}, "ACCEPT"},
		"an ACCEPT bound to a source port":      {[]FirewallRule{with(accept, func(r *FirewallRule) { r.SPort = "1024" }), drop}, "DROP"},
		"an ACCEPT of udp":                      {[]FirewallRule{with(accept, func(r *FirewallRule) { r.Proto = "udp" }), drop}, "DROP"},
		"a REJECT":                              {[]FirewallRule{with(drop, func(r *FirewallRule) { r.Action = "REJECT" })}, "REJECT"},
		"a DROP of part of the destination":     {[]FirewallRule{with(drop, func(r *FirewallRule) { r.Dest = "10.90.0.28" })}, "DROP"},
		"a DROP of another network":             {[]FirewallRule{with(drop, func(r *FirewallRule) { r.Dest = "10.88.0.0/24" })}, ""},
		"a DROP of another port":                {[]FirewallRule{with(drop, func(r *FirewallRule) { r.Proto, r.DPort = "tcp", "22" })}, ""},
		"a disabled DROP":                       {[]FirewallRule{with(drop, func(r *FirewallRule) { r.Enable = 0 })}, ""},
		"a DROP on an IP set (not read)":        {[]FirewallRule{with(drop, func(r *FirewallRule) { r.Dest = "+dc/somewhere" })}, ""},
		"a DROP with a macro (not read)":        {[]FirewallRule{with(drop, func(r *FirewallRule) { r.Macro = "SSH" })}, ""},
		"no rule":                               {nil, ""},
	} {
		if got, _ := forwardVerdict(c.rules, src, dst, "tcp", "6443"); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

func with(r FirewallRule, mod func(*FirewallRule)) FirewallRule {
	mod(&r)
	return r
}

// The fence gate against the fake PVE with the lab's cluster firewall: with the rules rc.1 wrote it fails and names
// the rule; with the rules of this release it holds; the rules of other owners are in the list both times.
func TestFenceGateSaysWhenTheClusterFirewallCutsTheMachineOff(t *testing.T) {
	ctx := context.Background()
	f, s := newSandboxFake(t)
	f.addVM(119, "pc-platform-sandbox-pve3-991934eb", SandboxPool, "virtio=BC:24:11:00:01:01,bridge=vmbr1")
	fw := labShared()
	p, err := s.StartSandboxPass(ctx, fw)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := p.Secure(ctx, p.Workers[0], "10.90.0.64"); err != nil || !st.Secured {
		t.Fatalf("secure: %+v %v", st, err)
	}
	open := map[string]string{"pc-platform-sandbox-pve3-991934eb": "10.90.0.64"}
	f.cluster = labClusterRules()
	err = s.CheckSandboxFence(ctx, fw, open)
	if err == nil || !strings.Contains(err.Error(), "cuts the sandbox machines off their own cluster") || !strings.Contains(err.Error(), "FORWARD DROP, "+labSandbox+" to "+labPool+",10.88.0.0/24,10.90.0.0/26") {
		t.Fatalf("the fence gate on a machine the cluster firewall cuts off: %v", err)
	}
	f.cluster = labClusterRules(SandboxClusterAllow(labAPI+":6443", []string{labPeers})...)
	if err := s.CheckSandboxFence(ctx, fw, open); err != nil {
		t.Fatalf("the fence gate with the cluster's ACCEPTs in place: %v", err)
	}
	// An operator's DROP above them cuts the machine off just the same.
	f.cluster = append([]FirewallRule{{Type: "forward", Action: "DROP", Source: "10.90.0.0/24", Dest: "10.90.0.0/24", Enable: 1, Comment: "ops"}}, f.cluster...)
	if err := s.CheckSandboxFence(ctx, fw, open); err == nil || !strings.Contains(err.Error(), `"ops"`) {
		t.Fatalf("an operator's DROP above the ACCEPTs: %v", err)
	}
}

// The provider-wide set of the lab's shared site, line by line as cluster.fw holds it: the five ACCEPTs of the
// machines' own cluster sit between the tenant DROPs and the sandbox DROPs, FORWARD only, and the DROPs are the ones
// v0.12.0-rc.1 wrote. An entry that would open the tenant pool, or that is no flow, adds nothing.
func TestIsolationRulesOfASharedSandboxGroup(t *testing.T) {
	peers := "10.90.0.20/30,10.90.0.24/29,10.90.0.32/28,10.90.0.48/29,10.90.0.56/30,10.90.0.60"
	allow := append(SandboxClusterAllow(labAPI+":6443", []string{labPeers}), "tcp 10.121.0.9 6443", "sctp 10.90.0.10 6443", "tcp 10.90.0.10", "icmp 10.90.0.10 8", "tcp +dc/set 22")
	line := func(r FirewallRule) string {
		return strings.Join(strings.Fields(strings.ToUpper(r.Type)+" "+r.Action+" "+r.Proto+" "+r.Source+" > "+r.Dest+" "+r.DPort), " ")
	}
	var got []string
	for _, r := range isolationRules(labPool, labBlocked, []string{labSandbox}, labOwner, allow...) {
		if r.Comment != labOwner || r.Enable != 1 {
			t.Fatalf("rule %+v", r)
		}
		got = append(got, line(r))
	}
	want := []string{
		"FORWARD DROP 10.121.0.0/16 > 10.121.0.0/16",
		"IN DROP 10.121.0.0/16 > 10.121.0.0/16",
		"FORWARD DROP 10.121.0.0/16 > 10.88.0.0/24,10.90.0.0/26,10.90.0.64/28",
		"IN DROP 10.121.0.0/16 > 10.88.0.0/24,10.90.0.0/26,10.90.0.64/28",
		"FORWARD ACCEPT tcp 10.90.0.64/28 > 10.90.0.10 6443",
		"FORWARD ACCEPT tcp 10.90.0.64/28 > " + peers + " 6443",
		"FORWARD ACCEPT udp 10.90.0.64/28 > " + peers + " 8472",
		"FORWARD ACCEPT tcp 10.90.0.64/28 > " + peers + " 4240",
		"FORWARD ACCEPT icmp 10.90.0.64/28 > " + peers,
		"FORWARD DROP 10.90.0.64/28 > 10.121.0.0/16,10.88.0.0/24,10.90.0.0/26",
		"IN DROP 10.90.0.64/28 > 10.121.0.0/16,10.88.0.0/24,10.90.0.0/26",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A config that names no control plane (the installer's sandbox cluster, pc-sandbox) adds no ACCEPT.
	if a := SandboxClusterAllow("", []string{labPeers}); a != nil {
		t.Fatalf("no control plane named: %q", a)
	}
	// With no sandbox network nothing of the list is written.
	if rules := isolationRules(labPool, labBlocked, nil, labOwner, allow...); len(rules) != 4 {
		t.Fatalf("no sandbox network: %d rules", len(rules))
	}
}

// WriteProviderIsolation against the fake PVE holding the lab's other rules: it brings the set to the config whatever
// order the rules were in (the drill saw cluster.fw rewritten in another order by a sweep), touches no rule of another
// owner, keeps a project's ACCEPTs on top, and a second pass makes no request but reads. With a node that does not
// report its networks it changes nothing and says that the rules are not in place.
func TestWriteProviderIsolation(t *testing.T) {
	ctx := context.Background()
	f, s := newSandboxFake(t)
	s.PoolPrefix = "pcm-"
	blocked := []string{"10.90.0.0/26"} // the fake's nodes add 192.0.2.0/24, 198.51.100.0/24 and 203.0.113.0/28
	allow := SandboxClusterAllow(labAPI+":6443", []string{labPeers})
	project := []FirewallRule{
		{Type: "forward", Action: "ACCEPT", Source: "10.121.1.0/24", Dest: "10.121.1.0/24", Enable: 1, Comment: isolationComment + ":proj1"},
		{Type: "in", Action: "ACCEPT", Source: "10.121.1.0/24", Dest: "10.121.1.0/24", Enable: 1, Comment: isolationComment + ":proj1"},
	}
	owned := func() (own, others []FirewallRule) {
		for _, r := range f.cluster {
			if r.Comment == labOwner {
				own = append(own, r)
			} else {
				others = append(others, r)
			}
		}
		return own, others
	}
	writes := func() int {
		return len(slices.DeleteFunc(slices.Clone(f.writes), func(w string) bool { return !strings.Contains(w, "/cluster/firewall/rules") }))
	}
	want := func(sandbox []string, allow ...string) []FirewallRule {
		w, _, _, _, _, err := s.isolationWant(ctx, labPool, blocked, sandbox, allow)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	// The set of before sandboxes, in another order than isolationRules gives, between the others' rules.
	base := want(nil)
	f.cluster = slices.Concat(project, []FirewallRule{base[3], base[0]}, foreignRules()[:3], []FirewallRule{base[2], base[1]}, foreignRules()[3:])

	if err := s.WriteProviderIsolation(ctx, labPool, blocked, []string{labSandbox}, allow...); err != nil {
		t.Fatal(err)
	}
	own, others := owned()
	if w := want([]string{labSandbox}, allow...); !slices.EqualFunc(own, w, sameLiveRule) || len(w) != 11 {
		t.Fatalf("the set after the write:\n%+v\nwant\n%+v", own, w)
	}
	if !slices.EqualFunc(others, slices.Concat(project, foreignRules()), sameLiveRule) {
		t.Fatalf("a project's ACCEPTs or the rules of another owner were changed or moved among themselves:\n%+v", others)
	}
	if !isProjectIsolationRule(f.cluster[0]) || !isProjectIsolationRule(f.cluster[1]) || f.cluster[2].Comment != labOwner {
		t.Fatalf("the project's ACCEPTs must stay above the DROPs: %+v", f.cluster[:3])
	}
	n := writes()
	if err := s.WriteProviderIsolation(ctx, labPool, blocked, []string{labSandbox}, allow...); err != nil || writes() != n {
		t.Fatalf("a pass with nothing to change: %v, %d requests that are no read", err, writes()-n)
	}

	// The range taken away again: back to the four DROPs, in the order of isolationRules.
	if err := s.WriteProviderIsolation(ctx, labPool, blocked, nil); err != nil {
		t.Fatal(err)
	}
	if own, others = owned(); !slices.EqualFunc(own, base, sameLiveRule) || !slices.EqualFunc(others, slices.Concat(project, foreignRules()), sameLiveRule) {
		t.Fatalf("after the range went:\n%+v\n%+v", own, others)
	}

	// IsolationDiffersOnlyIn: the four DROPs against a config that names the range, and the mirror.
	own1 := []string{labSandbox}
	for name, c := range map[string]struct {
		own     []string
		sandbox []string
		mod     func()
		want    bool
	}{
		"the range's rules missing":           {own1, own1, func() {}, true},
		"no own range":                        {nil, own1, func() {}, false},
		"another range is the own one":        {[]string{"10.90.0.80/28"}, own1, func() {}, false},
		"a rule of the set disabled as well":  {own1, own1, func() { f.cluster[2].Enable = 0 }, false},
		"the datacenter firewall off as well": {own1, own1, func() { f.dcOff = true }, false},
		"no rule at all":                      {own1, own1, func() { f.cluster = foreignRules() }, false},
	} {
		keep, off := slices.Clone(f.cluster), f.dcOff
		c.mod()
		if got, err := s.IsolationDiffersOnlyIn(ctx, c.own, labPool, blocked, c.sandbox, allow...); err != nil || got != c.want {
			t.Errorf("%s: %v %v, want %v", name, got, err, c.want)
		}
		f.cluster, f.dcOff = keep, off
	}
	if err := s.WriteProviderIsolation(ctx, labPool, blocked, []string{labSandbox}, allow...); err != nil {
		t.Fatal(err)
	}
	if got, err := s.IsolationDiffersOnlyIn(ctx, own1, labPool, blocked, nil); err != nil || !got {
		t.Fatalf("the range's rules left over: %v %v", got, err)
	}
	if got, _ := s.IsolationDiffersOnlyIn(ctx, own1, labPool, []string{"10.90.0.0/26", "10.77.0.0/24"}, nil); got {
		t.Fatal("a blocked network the rules do not drop passed as a difference of the own range")
	}

	// A node that does not report its networks: the DROPs in place stay as they are, and the write says so.
	f.netDown = true
	n = writes()
	err := s.WriteProviderIsolation(ctx, labPool, blocked, nil)
	if !errors.Is(err, ErrIsolationNotInPlace) || !strings.Contains(err.Error(), "did not report its networks") || writes() != n {
		t.Fatalf("with a node down: %v, %d requests that are no read", err, writes()-n)
	}
}

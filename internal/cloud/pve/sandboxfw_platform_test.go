package pve

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// A managed platform's sandbox machines (docs/managed-services.md, Sandboxes): on the platform network 192.168.140.0/24,
// in a range of their own. The platform cluster's machines are .20-.39, its MetalLB range .200-.219, the endpoint .10.

// sharedSandbox is the fence of a node group of the platform cluster.
func sharedSandbox() SandboxFirewall {
	return SandboxFirewall{
		Pool: SandboxPool, Subnet: "192.168.140.64/28", Nodes: []string{"192.168.140.64/28"},
		ControlPlane: "192.168.140.10", APIPort: 6443, DNS: []string{"192.168.140.1"},
		KubeletFrom: []string{"192.168.140.20-192.168.140.39"}, Peers: []string{"192.168.140.20-192.168.140.39"},
		Deny: []string{"192.168.140.0/24", "10.30.0.0/24"}, Bridge: "vmbr0", VLAN: 140,
	}
}

// separateSandbox is the fence of the cluster pc-sandbox: its control plane is a MetalLB address of the platform
// cluster, its own MetalLB announces sandbox-proxy at .250, an address outside every range.
func separateSandbox() SandboxFirewall {
	return SandboxFirewall{
		Pool: SandboxPool, Subnet: "192.168.140.64/28", Nodes: []string{"192.168.140.64/28"}, LoadBalancers: []string{"192.168.140.250"},
		ControlPlane: "192.168.140.201", APIPort: 6443, DNS: []string{"192.168.140.1"},
		Deny: []string{"192.168.140.0/24"}, ProxyAddress: "192.168.140.250", ProxyPort: 443, Bridge: "vmbr0", VLAN: 140,
	}
}

func ruleLines(g sandboxGen) []string {
	var out []string
	for _, r := range g.Rules {
		addr := strings.NewReplacer("+dc/"+g.NodeSet, "NODES", "+dc/"+g.OutSet, "INTERNET").Replace(r.Source + r.Dest)
		out = append(out, strings.Join(strings.Fields(r.Type+" "+r.Proto+" "+addr+" "+r.DPort), " "))
	}
	slices.Sort(out)
	return out
}

// The whole shared rule set, rule by rule: a rule added here opens a machine that sits beside every tenant's control
// plane, so the list is exact.
func TestSandboxSharedRuleSet(t *testing.T) {
	g, err := sharedSandbox().generation()
	if err != nil {
		t.Fatal(err)
	}
	peers := "192.168.140.20/30,192.168.140.24/29,192.168.140.32/29"
	want := []string{
		"in icmp NODES", "in tcp " + peers + " 10250", "in tcp NODES 4240", "in udp NODES 8472",
		"out icmp INTERNET", "out icmp NODES", "out tcp 192.168.140.1 53", "out tcp 192.168.140.10 6443", "out tcp " + peers + " 6443",
		"out tcp INTERNET 1:24,26:65535", "out tcp NODES 4240", "out udp 192.168.140.1 53", "out udp INTERNET", "out udp NODES 8472",
	}
	if got := ruleLines(g); !slices.Equal(got, want) {
		t.Fatalf("shared rules:\n got %q\nwant %q", got, want)
	}
	// The rules' node set is the sandbox machines and the cluster's other machines; nothing else of the network.
	if got := cidrsOf(g.Nodes); !slices.Equal(got, []string{"192.168.140.20/30", "192.168.140.24/29", "192.168.140.32/29", "192.168.140.64/28"}) {
		t.Fatalf("node set %v", got)
	}
	// The internet set leaves out the platform network (the provider's deny list), the peers and the sandbox range.
	for _, d := range []string{"192.168.0.0/16", "10.0.0.0/8"} {
		if !slices.ContainsFunc(g.Out, func(e IPSet) bool { return e.CIDR == d && e.NoMatch == 1 }) {
			t.Fatalf("the internet set does not leave out %s: %+v", d, g.Out)
		}
	}
	// A platform network that is not RFC 1918: only the deny list and the peers keep it out.
	f := sharedSandbox()
	f.Subnet, f.Nodes, f.ControlPlane, f.DNS = "203.0.113.64/28", []string{"203.0.113.64/28"}, "203.0.113.10", []string{"1.1.1.1"}
	f.Peers, f.KubeletFrom, f.Deny = []string{"203.0.113.20-203.0.113.39"}, []string{"203.0.113.20-203.0.113.39"}, []string{"203.0.113.0/24"}
	if g, err = f.generation(); err != nil || !slices.ContainsFunc(g.Out, func(e IPSet) bool { return e.CIDR == "203.0.113.0/24" && e.NoMatch == 1 }) {
		t.Fatalf("a public platform prefix stays reachable from a sandbox machine: %v %+v", err, g.Out)
	}
	// The peers are left out by themselves as well: a provider row that lost its deny list must not turn "the
	// internet" into every port of the cluster's own machines.
	f.Deny = nil
	if g, err = f.generation(); err != nil {
		t.Fatal(err)
	}
	for _, peer := range []string{"203.0.113.20/30", "203.0.113.24/29", "203.0.113.32/29"} {
		if !slices.ContainsFunc(g.Out, func(e IPSet) bool { return e.CIDR == peer && e.NoMatch == 1 }) {
			t.Fatalf("without a deny list the internet set admits the cluster's machines at %s: %+v", peer, g.Out)
		}
	}
	for name, mod := range map[string]func(*SandboxFirewall){
		"peers without the platform bridge": func(f *SandboxFirewall) { f.Bridge = "" },
		"peers inside the sandbox range":    func(f *SandboxFirewall) { f.Peers = []string{"192.168.140.60-192.168.140.70"} },
		"a load balancer address":           func(f *SandboxFirewall) { f.LoadBalancers = []string{"192.168.140.210"} },
		"a proxy address":                   func(f *SandboxFirewall) { f.ProxyAddress = "192.168.140.210" },
		"the endpoint in the sandbox range": func(f *SandboxFirewall) { f.ControlPlane = "192.168.140.70" },
		"a worker outside the range":        func(f *SandboxFirewall) { f.Nodes = []string{"192.168.140.64/27"} },
	} {
		f := sharedSandbox()
		mod(&f)
		if _, err := f.generation(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// The separate cluster keeps the installer's rule set; what differs is the place: no vnet, a proxy address outside the
// machines' range, and clients that reach sandbox-proxy with their own address.
func TestSandboxSeparateRuleSet(t *testing.T) {
	g, err := separateSandbox().generation()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"in icmp NODES", "in tcp 192.168.140.250 443", "in tcp NODES 10250", "in tcp NODES 4240", "in tcp NODES 7946", "in udp NODES 7946", "in udp NODES 8472",
		"out icmp INTERNET", "out icmp NODES", "out tcp 192.168.140.1 53", "out tcp 192.168.140.201 6443,8132",
		"out tcp INTERNET 1:24,26:65535", "out tcp NODES 10250", "out tcp NODES 4240", "out tcp NODES 7946", "out udp 192.168.140.1 53", "out udp INTERNET", "out udp NODES 7946", "out udp NODES 8472",
	}
	if got := ruleLines(g); !slices.Equal(got, want) {
		t.Fatalf("separate rules:\n got %q\nwant %q", got, want)
	}
	if got := cidrsOf(g.Nodes); !slices.Equal(got, []string{"192.168.140.64/28"}) {
		t.Fatalf("node set %v: the sandbox cluster's machines alone", got)
	}
	// On a vnet of its own (the installer) the same addresses are still refused: nothing there changed.
	f := separateSandbox()
	f.Bridge, f.VLAN = "", 0
	if _, err := f.generation(); err == nil {
		t.Fatal("a load balancer address outside the subnet passed without the platform bridge")
	}
}

// The fence on the platform network against the fake PVE: only the pool selects a VM, no vnet is looked up (the fake
// has none for this range), the NIC is known by bridge and tag, and the filter pins a machine to its own address.
func TestSandboxFenceOnThePlatformNetwork(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		fw       SandboxFirewall
		nic, off string
		lbs      []string // what the filter admits beside the machine's own address
	}{
		"shared, tagged bridge": {sharedSandbox(), "bridge=vmbr0,tag=140", "bridge=vmbr0,tag=141", nil},
		"separate":              {separateSandbox(), "bridge=vmbr0,tag=140", "bridge=vmbr0", []string{"192.168.140.250"}},
		"routed vnet, no tag": {func() SandboxFirewall {
			f := sharedSandbox()
			f.Bridge, f.VLAN = "pcplat", 0
			return f
		}(), "bridge=pcplat", "bridge=pcplat,tag=7", nil},
	} {
		t.Run(name, func(t *testing.T) {
			f, s := newSandboxFake(t)
			sbx := f.addVM(301, "pc-platform-sandbox-a", SandboxPool, "virtio=BC:24:11:00:01:01,"+c.nic, "virtio=BC:24:11:00:01:02,"+c.off)
			// The platform cluster's own machines: the same bridge and tag, another pool. And a tenant's VM.
			system := f.addVM(201, "pc-platform-system-a", SystemPool, "virtio=BC:24:11:00:02:01,"+c.nic)
			tenant := f.addVM(130, "t1", "pc-p1", "virtio=BC:24:11:00:00:03,bridge=vtenant")
			p, err := s.StartSandboxPass(ctx, c.fw)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Workers) != 1 || p.Workers[0].VMID != 301 {
				t.Fatalf("workers %+v: the sandbox pool's VMs and no other", p.Workers)
			}
			if st, err := p.Secure(ctx, p.Workers[0], ""); err != nil || !st.Secured || st.Pinned != "" {
				t.Fatalf("first secure: %+v %v", st, err)
			}
			if got := cidrsOf(sbx.sets["ipfilter-net0"]); !slices.Equal(got, slices.Sorted(slices.Values(append([]string{"192.168.140.64/28"}, c.lbs...)))) {
				t.Fatalf("before its Node joins the filter admits %v", got)
			}
			if got, ok := sbx.sets["ipfilter-net1"]; !ok || len(got) != 0 {
				t.Fatalf("a NIC on another tag of the bridge gets an empty filter: %v %v", got, ok)
			}
			// A peer's address, the endpoint and a MetalLB address are not this machine's, whatever its Node says.
			for _, claim := range []string{"192.168.140.20", "192.168.140.10", "192.168.140.201", "192.168.140.250"} {
				if st, _ := p.Secure(ctx, p.Workers[0], claim); st.Pinned != "" {
					t.Fatalf("the filter was pinned to %s, an address outside the sandbox range", claim)
				}
			}
			st, err := p.Secure(ctx, p.Workers[0], "192.168.140.66")
			if err != nil || !st.Secured || st.Pinned != "192.168.140.66" {
				t.Fatalf("pin: %+v %v", st, err)
			}
			if got := cidrsOf(sbx.sets["ipfilter-net0"]); !slices.Equal(got, slices.Sorted(slices.Values(append([]string{"192.168.140.66"}, c.lbs...)))) {
				t.Fatalf("pinned filter %v", got)
			}
			if len(sbx.rules) != 1 || sbx.rules[0].Action != p.Group() || sbx.opts["policy_in"] != "DROP" || sbx.opts["policy_out"] != "DROP" {
				t.Fatalf("the sandbox VM: %+v %v", sbx.rules, sbx.opts)
			}
			for what, vm := range map[string]*fakeVM{"a platform machine": system, "a tenant VM": tenant} {
				if len(vm.rules) != 0 || len(vm.opts) != 0 || len(vm.sets) != 0 || strings.Contains(vm.cfg.Str("net0"), "firewall=") {
					t.Fatalf("%s was fenced: %+v %v %v %s", what, vm.rules, vm.opts, vm.sets, vm.cfg.Str("net0"))
				}
			}
			// The gate: the sandbox machine alone passes; with a platform machine among the open Nodes it does not.
			open := map[string]string{"pc-platform-sandbox-a": "192.168.140.66"}
			if err := s.CheckSandboxFence(ctx, c.fw, open); err != nil {
				t.Fatalf("fenced and pinned: %v", err)
			}
			open["pc-platform-system-a"] = "192.168.140.21"
			if err := s.CheckSandboxFence(ctx, c.fw, open); err == nil || !strings.Contains(err.Error(), "is no VM of pool") {
				t.Fatalf("the gate passed on a node set with a platform machine: %v", err)
			}
			// Sandboxes switched off: the machines go, then the generations.
			if err := s.RemoveSandboxFirewall(ctx, SandboxPool); err != nil || f.groups[p.Group()] == nil {
				t.Fatalf("a generation a VM still refers to was removed: %v", err)
			}
			delete(f.vms, 301)
			if err := s.RemoveSandboxFirewall(ctx, SandboxPool); err != nil || len(f.groups) != 0 || len(f.sets) != 0 {
				t.Fatalf("generations left after the pool is empty: %v %v %v", err, f.groups, f.sets)
			}
		})
	}
}

package platformlife

import (
	"slices"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// routed puts the whole lab profile on pve1 with a routed network on the vnet pcplat0.
func routed(in *Installation) {
	in.Spec.Network.Mode, in.Spec.Network.Bridge = NetworkRouted, "pcplat0"
	for i := range in.Spec.Clusters[0].NodeGroups {
		in.Spec.Clusters[0].NodeGroups[i].Nodes = []string{"pve1"}
	}
}

// The network modes: a spec without a mode is a bridge, as before the field; a VLAN tag goes with a bridge; a routed
// network names a vnet and keeps every machine on one node.
func TestValidateNetworkModes(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(*Installation)
		want string // "" = valid
	}{
		"no mode":                {func(*Installation) {}, ""},
		"bridge":                 {func(in *Installation) { in.Spec.Network.Mode = NetworkBridge }, ""},
		"bridge with a VLAN":     {func(in *Installation) { in.Spec.Network.VLAN = 140 }, ""},
		"VLAN 4095":              {func(in *Installation) { in.Spec.Network.VLAN = 4095 }, "spec.network.vlan 4095"},
		"negative VLAN":          {func(in *Installation) { in.Spec.Network.VLAN = -1 }, "spec.network.vlan -1"},
		"unknown mode":           {func(in *Installation) { in.Spec.Network.Mode = "vlan" }, `spec.network.mode "vlan"`},
		"routed":                 {routed, ""},
		"routed on two nodes":    {func(in *Installation) { routed(in); in.Spec.Clusters[0].ControlPlane.Node = "pve2" }, "one Proxmox node, and the settings place machines on pve1, pve2"},
		"routed, seed elsewhere": {func(in *Installation) { routed(in); in.Spec.Seed.Node = "pve3" }, "pve1, pve3"},
		"routed with a VLAN":     {func(in *Installation) { routed(in); in.Spec.Network.VLAN = 5 }, "spec.network.vlan goes with mode bridge"},
		"routed, a bridge name":  {func(in *Installation) { routed(in); in.Spec.Network.Bridge = "vmbr1.140" }, "names the vnet ProxCloud makes"},
		"routed, nine letters":   {func(in *Installation) { routed(in); in.Spec.Network.Bridge = "platform0" }, "at most 8"},
	} {
		in := validInstallation(t)
		tc.mut(in)
		err := in.Validate()
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// The network stays fixed after the first install, the mode and the tag with it; a client that names the mode of an
// installation stored without one changes nothing.
func TestFixedNetworkMode(t *testing.T) {
	old := validInstallation(t)
	old.Status.BOM = &BOMRef{Release: "0.8.1", SHA256: "x"}
	next := func(mut func(*Network)) *Installation {
		n := *old
		mut(&n.Spec.Network)
		return &n
	}
	if f := FixedChanges(old, next(func(n *Network) { n.Mode = NetworkBridge })); len(f) != 0 {
		t.Errorf("naming the mode bridge of a stored spec without one: %v", f)
	}
	for name, mut := range map[string]func(*Network){
		"mode": func(n *Network) { n.Mode = NetworkRouted },
		"vlan": func(n *Network) { n.VLAN = 140 },
	} {
		if f := FixedChanges(old, next(mut)); !slices.Equal(f, []string{"spec.network"}) {
			t.Errorf("%s changed after the install: %v", name, f)
		}
	}
}

// The seed's NIC carries the tag next to the bridge, and an older tag of the clone never survives.
func TestWithNet0VLAN(t *testing.T) {
	if got := withNet0("virtio=BC:24:11:00:00:01,bridge=vmbr0,tag=7", "vmbr1", 140, 1450); got != "virtio=BC:24:11:00:00:01,bridge=vmbr1,firewall=1,tag=140,mtu=1450" {
		t.Errorf("tagged: %s", got)
	}
	if got := withNet0("virtio=BC:24:11:00:00:01,bridge=vmbr0,tag=7", "vmbr1", 0, 0); got != "virtio=BC:24:11:00:00:01,bridge=vmbr1,firewall=1" {
		t.Errorf("untagged: %s", got)
	}
}

// The cluster values name the VLAN only with a tag and only for a chart that reads it: an older chart would leave
// the machines untagged on the bridge, so the render is refused. Without a tag the values hold no such key, under
// this BOM and under a previous one.
func TestClusterValuesVLAN(t *testing.T) {
	network := func(b *BOM, vlan int) (map[string]any, error) {
		in := renderInput()
		in.Network.VLAN = vlan
		v, err := ClusterValues(b, in)
		if err != nil {
			return nil, err
		}
		return v["proxmox"].(map[string]any)["network"].(map[string]any), nil
	}
	withChart := func(version string) *BOM {
		b := *embedded(t)
		b.Components = slices.Clone(b.Components)
		for i := range b.Components {
			if b.Components[i].Name == ClusterName {
				b.Components[i].Chart.Version = version
			}
		}
		return &b
	}
	for _, version := range []string{"0.1.13", "0.1.14"} {
		n, err := network(withChart(version), 0)
		if _, has := n["vlan"]; err != nil || has {
			t.Errorf("chart %s without a tag: %v %v", version, n, err)
		}
	}
	if n, err := network(withChart("0.1.14"), 140); err != nil || n["vlan"] != 140 || n["bridge"] != "vmbr1" {
		t.Errorf("chart 0.1.14 with a tag: %v %v", n, err)
	}
	if _, err := network(withChart("0.1.13"), 140); err == nil || !strings.Contains(err.Error(), "names cluster chart 0.1.13, which cannot tag a NIC") {
		t.Errorf("chart 0.1.13 with a tag must be refused: %v", err)
	}
}

// Stage 3 with a routed network: the zone limited to the node, the vnet with the installation's alias and the subnet
// with the platform gateway and SNAT, applied once, before the seed is cloned onto the vnet.
func TestSeedVMMakesTheRoutedNetwork(t *testing.T) {
	e := newEnv(t, RunApply)
	e.only(StageSeedVM)
	e.mutate(func(_ *Run, in *Installation, _ *Secrets) { routed(in) })
	var order []string
	e.pve.onWrite = func(w string) {
		if w == "PUT /cluster/sdn" || strings.HasSuffix(w, "/clone") {
			order = append(order, w)
		}
	}
	if end := e.until(5); end != "complete" {
		t.Fatalf("end %s; logs %s", end, e.logs)
	}
	f := e.pve
	if len(f.zones) != 1 || f.zones[0].Zone != RoutedZone || f.zones[0].Type != "simple" || f.zones[0].Nodes != "pve1" || f.zones[0].State != "" {
		t.Errorf("zones %+v", f.zones)
	}
	if len(f.vnets) != 1 || f.vnets[0].Vnet != "pcplat0" || f.vnets[0].Zone != RoutedZone || f.vnets[0].Alias != "proxcloud-platform:"+inID {
		t.Errorf("vnets %+v", f.vnets)
	}
	if subs := f.subnets["pcplat0"]; len(subs) != 1 || subs[0].CIDR != "10.200.200.0/24" || subs[0].Gateway != "10.200.200.1" || subs[0].SNAT != 1 {
		t.Errorf("subnets %+v", subs)
	}
	if !slices.Equal(order, []string{"PUT /cluster/sdn", "POST /nodes/pve1/qemu/9100/clone"}) {
		t.Errorf("the network is applied once, before the clone: %v", order)
	}
	if net0, _ := f.vms[120].cfg["net0"].(string); !strings.Contains(net0, "bridge=pcplat0,firewall=1") {
		t.Errorf("the seed's net0 %q", net0)
	}
}

// A vnet of that name that the installation did not make is never used or changed, and a bridge network makes none.
func TestSeedVMRoutedNetworkRefusals(t *testing.T) {
	e := newEnv(t, RunApply)
	e.only(StageSeedVM)
	e.mutate(func(_ *Run, in *Installation, _ *Secrets) { routed(in) })
	e.pve.vnets = []pve.SDNVnet{{Vnet: "pcplat0", Zone: "lan", Alias: "the owner's"}}
	if end := e.until(3); !strings.HasPrefix(end, "fail") {
		t.Fatalf("end %s", end)
	}
	if run, _, _ := e.state.snapshot(); run.Code != CodePlatformNetwork || !strings.Contains(run.Message, "this installation did not make") {
		t.Errorf("run %s %q", run.Code, run.Message)
	}
	if n := e.pve.writeCount(""); n != 0 {
		t.Errorf("writes to Proxmox: %v", e.pve.writes)
	}

	e = newEnv(t, RunApply)
	e.only(StageSeedVM)
	if end := e.until(5); end != "complete" || e.pve.writeCount("POST /cluster/sdn") != 0 || e.pve.writeCount("PUT /cluster/sdn") != 0 {
		t.Errorf("a bridge network wrote to SDN: end %s, %v", end, e.pve.writes)
	}
}

// Remove deletes the routed network after the seed: subnet, vnet and the zone it alone was in, applied. A vnet with
// another alias stays.
func TestRemoveSeedDeletesTheRoutedNetwork(t *testing.T) {
	for _, alias := range []string{"proxcloud-platform:" + inID, "someone else's"} {
		e := newEnv(t, RunRemove)
		e.only(StageRemoveSeed)
		e.mutate(func(_ *Run, in *Installation, _ *Secrets) { routed(in) })
		e.pve.zones = []pve.SDNZone{{Zone: RoutedZone, Type: "simple"}}
		e.pve.vnets = []pve.SDNVnet{{Vnet: "pcplat0", Zone: RoutedZone, Alias: alias}}
		e.pve.subnets = map[string][]pve.SDNSubnet{"pcplat0": {{Subnet: "z-10.200.200.0-24", CIDR: "10.200.200.0/24", Gateway: "10.200.200.1"}}}
		if end := e.until(5); end != "complete" {
			t.Fatalf("%s: end %s; logs %s", alias, end, e.logs)
		}
		it := item(e.stage(StageRemoveSeed), "network")
		if ours := strings.HasPrefix(alias, "proxcloud-platform:"); ours {
			if len(e.pve.vnets) != 0 || len(e.pve.zones) != 0 || len(e.pve.subnets["pcplat0"]) != 0 || e.pve.writeCount("PUT /cluster/sdn") != 1 || it == nil || it.State != "gone" {
				t.Errorf("ours: vnets %v zones %v item %+v writes %v", e.pve.vnets, e.pve.zones, it, e.pve.writes)
			}
		} else if len(e.pve.vnets) != 1 || e.pve.writeCount("PUT /cluster/sdn") != 0 || it == nil || it.State != "left" {
			t.Errorf("foreign: vnets %v item %+v writes %v", e.pve.vnets, it, e.pve.writes)
		}
	}
}

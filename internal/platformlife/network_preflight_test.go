package platformlife

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// The rows that know the network mode: bridge (the bridge on every node used, or the routed vnet's name), spec (a
// VLAN tag against the BOM's cluster chart), network (the route a routed network needs), privileges:capi (SDN.Use on
// the path of the NIC) and tenant-path (ok for a routed network the install has yet to make).
func TestPreflightNetworkModes(t *testing.T) {
	grant := func(checks []pve.PrivCheck, more ...string) map[string][]string {
		out := map[string][]string{}
		for _, c := range checks {
			out[c.Path] = append(out[c.Path], c.Privs...)
		}
		for _, p := range more {
			out[p] = []string{"SDN.Use"}
		}
		return out
	}
	run := func(mut func(*Installation), f *preflightPVE, capiPaths ...string) map[string]Check {
		in := validInstallation(t)
		in.ID = "pi-0a1b2c3d"
		for i := range in.Spec.Clusters[0].NodeGroups {
			in.Spec.Clusters[0].NodeGroups[i].Nodes = []string{"pve1"}
		}
		mut(in)
		f.perms = map[string]map[string][]string{pve.CAPITokenID: grant(pve.CAPIPrivileges(in.Spec.Storage.Boot, in.Spec.Storage.ISO), capiPaths...)}
		srv := httptest.NewServer(http.HandlerFunc(f.serve))
		defer srv.Close()
		pf := Preflight{Installation: in, Site: Site{CIDRPool: "10.100.0.0/16", Blocked: []string{"10.200.200.0/25"}, DefaultZone: "pcm0", DefaultZoneKind: "simple"},
			Portal: &pve.Service{C: f.client(t, srv.URL, pve.DefaultPlatformTokenID), ServiceID: "svc-pve"}, CAPI: f.client(t, srv.URL, pve.CAPITokenID)}
		rows := checkMap(pf.Run(context.Background()))
		if len(f.writes) != 0 {
			t.Errorf("the Check wrote to PVE: %v", f.writes)
		}
		return rows
	}
	want := func(name string, c Check, status string, parts ...string) {
		t.Helper()
		if c.Status != status {
			t.Errorf("%s: %s %q, want %s", name, c.ID, c.Status, status)
		}
		for _, p := range parts {
			if !strings.Contains(c.Message, p) {
				t.Errorf("%s: no %q in %s: %s", name, p, c.ID, c.Message)
			}
		}
	}

	// Bridge mode, as every installation before the field.
	rows := run(func(*Installation) {}, &preflightPVE{})
	want("bridge", rows["bridge"], CheckOK, "The bridge vmbr1 exists on pve1.")
	want("bridge", rows["privileges:capi"], CheckOK)
	rows = run(func(in *Installation) { in.Spec.Network.Bridge = "vmbr9" }, &preflightPVE{})
	want("a bridge no node has", rows["bridge"], CheckFail, "No bridge or vnet vmbr9 was found on pve1")
	// An installed platform runs on its bridge: a miss is a read that went wrong, and no apply stops on it.
	rows = run(func(in *Installation) {
		in.Spec.Network.Bridge, in.Status.BOM = "vmbr9", &BOMRef{Release: embedded(t).Release, SHA256: "x"}
	}, &preflightPVE{})
	want("a bridge not found after the install", rows["bridge"], CheckWarn, "vmbr9")
	rows = run(func(in *Installation) { in.Spec.Clusters[0].ControlPlane.Node = "pve2" }, &preflightPVE{})
	want("a node that did not answer", rows["bridge"], CheckFail, "was found on pve2")

	// A VLAN tag: the token's right is read on the tag's own path, and the BOM's chart must be able to tag.
	tagged := func(in *Installation) { in.Spec.Network.VLAN = 140 }
	rows = run(tagged, &preflightPVE{})
	want("VLAN without the right on the tag", rows["privileges:capi"], CheckFail, "SDN.Use on /sdn/zones/localnetwork/vmbr1/140")
	want("VLAN", rows["bridge"], CheckOK, "tagged with VLAN 140")
	rows = run(tagged, &preflightPVE{}, "/sdn/zones/localnetwork/vmbr1/140")
	want("VLAN with the right", rows["privileges:capi"], CheckOK)
	rows = run(func(in *Installation) { tagged(in); in.Spec.Network.Bridge = "plat" },
		&preflightPVE{vnets: `[{"vnet":"plat","zone":"lan"}]`, extra: map[string]string{"/nodes/pve1/network": `[{"iface":"plat","type":"vnet"}]`}}, "/sdn/zones/lan/plat/140")
	want("VLAN on a vnet reads the vnet's zone", rows["privileges:capi"], CheckOK)
	if kamajiChartTagsNICs(t) {
		want("VLAN with a chart that tags", rows["spec"], CheckOK)
	} else {
		want("VLAN with a chart that cannot tag", rows["spec"], CheckFail, "spec.network.vlan 140", "which cannot tag a NIC", "0.1.14")
	}

	// A routed network the install has yet to make.
	rows = run(routed, &preflightPVE{zones: `[{"zone":"pcm0","type":"simple"}]`}, "/sdn/zones/pcplat/pcplat0")
	want("routed", rows["spec"], CheckOK)
	want("routed", rows["bridge"], CheckOK, "No vnet pcplat0 exists yet", "simple zone pcplat on pve1")
	want("routed", rows["network"], CheckOK, `needs the route "10.200.200.0/24 via the address of pve1"`)
	want("routed", rows["privileges:capi"], CheckOK)
	want("routed", rows["tenant-path"], CheckOK, "is of mode routed", "the node pve1 holds", "Test the tenant path")
	// A name an interface of a node holds is not free, whatever the vnet list says.
	rows = run(func(in *Installation) { routed(in); in.Spec.Network.Bridge = "vmbr1" }, &preflightPVE{})
	want("routed, an interface's name", rows["bridge"], CheckFail, "The name vmbr1 is an interface of pve1")
	rows = run(routed, &preflightPVE{extra: map[string]string{"/nodes/pve1/network": `[{"iface":"vmbr0","address":"192.0.2.11","gateway":"192.0.2.1","cidr":"192.0.2.11/24"},{"iface":"bond0","address":"10.200.200.7"}]`}})
	want("routed, a range a node is in", rows["network"], CheckFail, "overlaps the node address 10.200.200.7")
	rows = run(routed, &preflightPVE{})
	want("routed without the right on the vnet", rows["privileges:capi"], CheckFail, "SDN.Use on /sdn/zones/pcplat/pcplat0")
	// The vnet once made, and a vnet of that name that is someone else's.
	made := &preflightPVE{zones: `[{"zone":"pcm0","type":"simple"},{"zone":"pcplat","type":"simple"}]`, vnets: `[{"vnet":"pcplat0","zone":"pcplat","alias":"proxcloud-platform:pi-0a1b2c3d"}]`,
		extra: map[string]string{"/cluster/sdn/vnets/pcplat0/subnets": `[{"cidr":"10.200.200.0/24","gateway":"10.200.200.1","snat":1}]`}}
	rows = run(routed, made)
	want("routed, made", rows["bridge"], CheckOK, "is this installation's")
	want("routed, made", rows["tenant-path"], CheckOK, "routed by the node itself")
	rows = run(routed, &preflightPVE{vnets: `[{"vnet":"pcplat0","zone":"lan","alias":"st:p1:net"}]`})
	want("routed, the name taken", rows["bridge"], CheckFail, "this installation did not make")
}

// kamajiChartTagsNICs: the embedded BOM's cluster chart reads proxmox.network.vlan. True since the BOM pins chart
// 0.1.14; under an older pin the spec row refuses a tag, and this test follows the pin.
func kamajiChartTagsNICs(t *testing.T) bool {
	t.Helper()
	_, err := ClusterValues(embedded(t), func() RenderInput { in := renderInput(); in.Network.VLAN = 2; return in }())
	return err == nil
}

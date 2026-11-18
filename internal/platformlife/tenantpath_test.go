package platformlife

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// The tenant-path row, for each layout of the rule in docs/managed-services.md ("The path from tenants to the
// platform"): it warns when a node both translates tenant traffic and hosts a platform machine without routing the
// platform network itself, is ok otherwise, never fails, and is absent when the provider names no zone.
func TestPreflightTenantPath(t *testing.T) {
	const (
		simple   = `{"zone":"pcm0","type":"simple"}`
		lan      = `{"zone":"lan","type":"vlan","bridge":"vmbr0"}`
		platVlan = `[{"vnet":"plat140","zone":"lan","tag":140}]`
		platSDN  = `[{"vnet":"plat140","zone":"platz"}]`
		platZone = `{"zone":"platz","type":"simple"}`
		subnets  = "/cluster/sdn/vnets/plat140/subnets"
	)
	simpleSite := Site{DefaultZone: "pcm0", DefaultZoneKind: "simple"}
	evpnSite := Site{DefaultZone: "ev", DefaultZoneKind: "evpn"}
	for _, tc := range []struct {
		name          string
		site          Site
		zones, vnets  string
		bridge        string            // the platform bridge; vmbr1 when empty
		nodes         []string          // the platform machines' nodes; pve1 when nil
		cp            string            // the control plane's node, when the node groups (nodes) are elsewhere
		extra         map[string]string // further answers of the fake
		status, wants string            // wants: parts of the message, split at "|"
	}{
		// Production, 2026-11-18: one node, a simple tenant zone, the platform network a VLAN behind the site's router.
		{name: "simple zone, VLAN platform vnet on the same node", site: simpleSite,
			zones: "[" + simple + "," + lan + "]", vnets: platVlan, bridge: "plat140", status: CheckWarn,
			wants: "from pve1. The provider's default zone pcm0 is a simple zone|a vnet of the VLAN zone lan|never join|managed databases|one the node routes|a leg on the platform bridge|" + tenantPathGuide},
		{name: "simple zone, platform vnet routed by the node", site: simpleSite,
			zones: "[" + simple + "," + platZone + "]", vnets: platSDN, bridge: "plat140", status: CheckOK,
			extra: map[string]string{subnets: `[{"subnet":"platz-10.200.200.0-24","cidr":"10.200.200.0/24","gateway":"10.200.200.1","snat":1}]`},
			wants: "routed by the node itself"},
		{name: "simple zone, platform vnet of a simple zone without the gateway", site: simpleSite,
			zones: "[" + simple + "," + platZone + "]", vnets: platSDN, bridge: "plat140", status: CheckWarn,
			extra: map[string]string{subnets: `[{"cidr":"10.200.200.0/24","gateway":"10.200.200.254"}]`},
			wants: "with no subnet whose gateway is 10.200.200.1"},
		{name: "simple zone, plain bridge the node holds no address on", site: simpleSite,
			zones: "[" + simple + "]", status: CheckWarn, wants: "its bridge vmbr1 is no vnet of a simple zone"},
		{name: "simple zone, plain bridge with the gateway on the node", site: simpleSite,
			zones: "[" + simple + "]", status: CheckOK, wants: "routed by the node itself",
			extra: map[string]string{"/nodes/pve1/network": `[{"iface":"vmbr0","address":"192.0.2.11","gateway":"192.0.2.1","cidr":"192.0.2.11/24"},{"iface":"vmbr1","address":"10.200.200.1"}]`}},
		// An address that is not the gateway routes nothing: the platform machines answer through their gateway.
		{name: "simple zone, plain bridge with another address on the node", site: simpleSite,
			zones: "[" + simple + "]", status: CheckWarn, wants: "does not hold the gateway 10.200.200.1",
			extra: map[string]string{"/nodes/pve1/network": `[{"iface":"vmbr1","address":"10.200.200.2"}]`}},
		// Each node that shares is read: one that holds the gateway does not clear the other.
		{name: "simple zone, plain bridge, the gateway on one of two nodes", site: simpleSite,
			zones: "[" + simple + "]", nodes: []string{"pve1", "pve2"}, status: CheckWarn, wants: "from pve2. |Platform machines are placed on pve2 too",
			extra: map[string]string{"/nodes/pve1/network": `[{"iface":"vmbr1","address":"10.200.200.1"}]`, "/nodes/pve2/network": `[{"iface":"vmbr1","address":"10.200.200.3"}]`}},
		// An upgraded site: the zone was made by hand as simple and its config entry names no kind, which reads evpn.
		{name: "a live simple zone the provider's config reads as evpn", site: Site{DefaultZone: "pcm0", DefaultZoneKind: "evpn"},
			zones: "[" + simple + "]", status: CheckWarn, wants: "pcm0 is a simple zone"},
		{name: "a simple zone that does not exist yet", site: simpleSite, zones: "[]", status: CheckWarn, wants: "from pve1"},
		// The lab: the node translates nothing, the site's router does.
		{name: "VLAN tenant zone", site: Site{DefaultZone: "lan", DefaultZoneKind: "vlan"},
			zones: "[" + lan + "]", vnets: platVlan, bridge: "plat140", status: CheckOK, wants: "no Proxmox node that translates"},
		// Several nodes: it breaks only where a platform machine shares a node with what translates.
		{name: "simple zone on other nodes than the platform machines", site: simpleSite,
			zones: `[{"zone":"pcm0","type":"simple","nodes":"pve2,pve3"}]`, status: CheckOK, wants: "platform machines are on other nodes (pve1)"},
		{name: "several nodes, the control plane alone on one a tenant runs on", site: simpleSite,
			zones: `[{"zone":"pcm0","type":"simple","nodes":"pve2, pve3"},` + lan + `]`, vnets: platVlan, bridge: "plat140", cp: "pve3",
			status: CheckWarn, wants: "from pve3. |Platform machines are placed on pve3 too"},
		{name: "EVPN, a platform machine on an exit node", site: evpnSite,
			zones: `[{"zone":"ev","type":"evpn","exitnodes":"pve1"},` + lan + `]`, vnets: platVlan, bridge: "plat140", status: CheckWarn,
			wants: "its exit nodes (pve1) translate"},
		{name: "EVPN, the exit node holds no platform machine", site: evpnSite,
			zones: `[{"zone":"ev","type":"evpn","exitnodes":"pve2"}]`, status: CheckOK, wants: "platform machines are on other nodes"},
		{name: "EVPN not made yet, exit nodes from the provider's config", site: Site{DefaultZone: "ev", DefaultZoneKind: "evpn", DefaultZoneExitNodes: []string{"pve1"}},
			zones: "[]", status: CheckWarn, wants: "its exit nodes (pve1) translate"},
		{name: "EVPN without exit nodes", site: evpnSite,
			zones: `[{"zone":"ev","type":"evpn"}]`, status: CheckOK, wants: "no Proxmox node that translates"},
		{name: "zones not readable", site: simpleSite, zones: "!", status: CheckWarn,
			wants: "Could not read the SDN zones|is not known|" + tenantPathGuide},
		{name: "vnets not readable", site: simpleSite, zones: "[" + simple + "]", vnets: "!", status: CheckWarn, wants: "Could not read the SDN vnets"},
		{name: "no zone named", site: Site{}, zones: "[" + simple + "]"},
	} {
		in := validInstallation(t)
		if tc.bridge != "" {
			in.Spec.Network.Bridge = tc.bridge
		}
		nodes := tc.nodes
		if nodes == nil {
			nodes = []string{"pve1"}
		}
		in.Spec.Clusters[0].ControlPlane.Node = nodes[0]
		if tc.cp != "" {
			in.Spec.Clusters[0].ControlPlane.Node = tc.cp
		}
		for i := range in.Spec.Clusters[0].NodeGroups {
			in.Spec.Clusters[0].NodeGroups[i].Nodes = nodes
		}
		f := &preflightPVE{zones: tc.zones, vnets: tc.vnets, extra: tc.extra}
		srv := httptest.NewServer(http.HandlerFunc(f.serve))
		tc.site.CIDRPool = "10.100.0.0/16"
		pf := Preflight{Installation: in, Site: tc.site, Portal: &pve.Service{C: f.client(t, srv.URL, pve.DefaultPlatformTokenID), ServiceID: "svc-pve"}}
		c, found := checkMap(pf.Run(context.Background()))["tenant-path"]
		srv.Close()
		if tc.status == "" {
			if found {
				t.Errorf("%s: a row without a zone: %+v", tc.name, c)
			}
			continue
		}
		if c.Status != tc.status {
			t.Errorf("%s: status %q, want %q: %s", tc.name, c.Status, tc.status, c.Message)
		}
		for _, w := range strings.Split(tc.wants, "|") {
			if !strings.Contains(c.Message, w) {
				t.Errorf("%s: no %q in: %s", tc.name, w, c.Message)
			}
		}
		// It cannot prove the path, and says so in every state.
		if !strings.Contains(c.Message, "reads the configuration only") && !strings.Contains(c.Message, "is not known") {
			t.Errorf("%s: the row does not say that it proves nothing: %s", tc.name, c.Message)
		}
		if len(f.writes) != 0 {
			t.Errorf("%s: the row wrote to PVE: %v", tc.name, f.writes)
		}
	}
}

// SiteOf carries the exit nodes of an EVPN default zone: the tenant-path row reads them for a zone not made yet.
func TestSiteOfExitNodes(t *testing.T) {
	s := SiteOf(&externalservice.ExternalService{Config: map[string]any{"network": map[string]any{"zones": map[string]any{"ev": map[string]any{"exitNodes": []any{"pve1", "pve2"}}}}}})
	if len(s.DefaultZoneExitNodes) != 2 || s.DefaultZoneExitNodes[0] != "pve1" {
		t.Errorf("exit nodes: %+v", s)
	}
}

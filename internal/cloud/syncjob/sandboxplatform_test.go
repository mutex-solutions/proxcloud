package syncjob

import (
	"slices"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platformlife"
)

// What a managed platform's run writes on its Proxmox provider (platformlife.SandboxSiteConfig) is what this sync
// reads: the two must agree key for key, or the machines stay tainted for good. Each placement's settings become a
// firewall the pve package accepts, on the platform's bridge, with the platform network denied.
func TestPlatformSandboxSettingsMakeAFence(t *testing.T) {
	render := func(mode, proxy string) platformlife.RenderInput {
		return platformlife.RenderInput{ID: "pi-0123abcd", Network: platformlife.RenderNetwork{Bridge: "vmbr0", VLAN: 140},
			Cluster: platformlife.RenderCluster{Endpoint: "192.168.140.10", NodeRange: []string{"192.168.140.20-192.168.140.39"}},
			Sandbox: &platformlife.RenderSandbox{Mode: mode, NodeRange: "192.168.140.64/28", PlatformPrefix: "192.168.140.0/24", DNS: []string{"192.168.140.1"}, ProxyAddress: proxy}}
	}
	site := []string{"10.30.0.0/24"} // what sandboxDeny reads from the site
	for mode, c := range map[string]struct {
		proxy, server string
	}{
		platformlife.SandboxShared:   {"192.168.140.210", "https://192.168.140.10:6443"},  // the platform cluster's endpoint
		platformlife.SandboxSeparate: {"192.168.140.250", "https://192.168.140.201:6443"}, // pc-sandbox's control plane, a MetalLB address
	} {
		es := &externalservice.ExternalService{Config: map[string]any{
			"network": map[string]any{"cidrPool": "10.100.0.0/16", "sandboxCidrs": []any{"192.168.140.64/28"}},
			"sandbox": platformlife.SandboxSiteConfig(render(mode, c.proxy)),
		}}
		cfg, ok := es.PVESandboxConfig()
		if !ok || cfg.Owner != "pi-0123abcd" || cfg.Mode != mode || cfg.UnsecuredMinAge.Minutes() != 20 {
			t.Fatalf("%s: the provider reads %+v", mode, cfg)
		}
		f, err := sandboxFirewallOf(es.PVENetworkConfig(), cfg, c.server, site)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if err := f.Validate(); err != nil {
			t.Fatalf("%s: the settings make no firewall: %v", mode, err)
		}
		if f.Pool != pve.SandboxPool || f.Subnet != "192.168.140.64/28" || f.Bridge != "vmbr0" || f.VLAN != 140 || !slices.Equal(f.Nodes, []string{"192.168.140.64/28"}) ||
			!slices.Contains(f.Deny, "192.168.140.0/24") || !slices.Contains(f.Deny, "10.30.0.0/24") || !slices.Equal(f.DNS, []string{"192.168.140.1"}) {
			t.Fatalf("%s: firewall %+v", mode, f)
		}
		shared := mode == platformlife.SandboxShared
		if shared != (len(f.Peers) > 0) || shared != (len(f.KubeletFrom) > 0) || shared == (f.ProxyAddress != "") || shared == (len(f.LoadBalancers) > 0) {
			t.Fatalf("%s: peers %v, kubelet from %v, proxy %q, load balancers %v", mode, f.Peers, f.KubeletFrom, f.ProxyAddress, f.LoadBalancers)
		}
	}
	// The installer's block reads as before: no owner, no mode, a vnet looked up by its subnet.
	es := &externalservice.ExternalService{Config: map[string]any{"sandbox": map[string]any{"pool": "proxcloud-sandbox", "nodes": []any{"10.110.0.10-10.110.0.19"}}}}
	if cfg, ok := es.PVESandboxConfig(); !ok || cfg.Owner != "" || cfg.Mode != "" || cfg.Bridge != "" || len(cfg.Peers)+len(cfg.Deny) != 0 {
		t.Fatalf("the installer's block: %+v", cfg)
	}
}

package platformlife

// sandbox_provider.go — the portal side of spec.sandbox: what a run writes on the installation's Proxmox provider so
// that its sync fences the sandbox machines (config.network.sandboxCidrs, config.sandbox, secret.sandboxKubeconfig),
// and the sandbox provider it registers. Both follow installer stage 60, key for key, with what the platform network
// adds: the machines sit on the platform's bridge and not on a vnet of their own, and with placement shared they are
// a node group of the platform cluster.
//
// The provider is registered DISABLED and a run never sets it live: the release gates decide that, on the provider's
// page, as for the installer's sandbox cluster.

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/mutex-solutions/proxcloud/deploy"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

const (
	// The two token Secrets of the sandbox chart's identities (templates/identities.yaml), in its namespace on the
	// cluster that runs sandboxes.
	sandboxPortalToken   = "proxcloud-sandbox-token"
	sandboxFirewallToken = "proxcloud-sandbox-firewall-token"
	// sandboxUnsecuredMinutes: a new sandbox machine takes sandboxes no earlier (the join token on its disk lives 15).
	sandboxUnsecuredMinutes = 20
	// sandboxControlPlaneMiB is what the control plane of pc-sandbox takes on the kamaji group (measured on the spike
	// of 2026-11-21: 0.57 to 0.63 GiB resident for one Kamaji tenant control plane).
	sandboxControlPlaneMiB = 640
)

// SandboxSite is what a run writes on the installation's Proxmox provider for its sandbox machines. Range is the one
// entry of config.network.sandboxCidrs, Config the block config.sandbox, Kubeconfig secret.sandboxKubeconfig ("" leaves
// the stored one: the identity exists only once the sandbox chart is deployed). A nil Config takes all three away.
type SandboxSite struct {
	Range      string
	Config     map[string]any
	Kubeconfig string
}

var sandboxPins = sync.OnceValues(func() (map[string]string, error) {
	var f struct {
		Images map[string]string `yaml:"images"`
	}
	if err := yaml.Unmarshal(deploy.SandboxImages, &f); err != nil {
		return nil, fmt.Errorf("deploy/templates/images.yaml: %w", err)
	}
	return f.Images, nil
})

// sandboxPin is the image of deploy/templates/images.yaml named name, by digest: what the installer writes for a
// template's "pin:<name>" on a site without a sandbox registry.
func sandboxPin(name string) (string, error) {
	pins, err := sandboxPins()
	if err != nil {
		return "", err
	}
	ref := pins[name]
	if !strings.Contains(ref, "@sha256:") {
		return "", fmt.Errorf("deploy/templates/images.yaml pins no image %q by digest", name)
	}
	return ref, nil
}

// sandboxPrepull are the images every sandbox machine pulls ahead (the sandbox chart's prepull): the two templates the
// E2B SDKs start by default. The others are pulled at their first start.
var sandboxPrepull = []string{"base", "code-interpreter"}

// SandboxTemplates are the templates of a sandbox provider a run registers: base and code-interpreter-v1, which the
// E2B SDKs ask for, python and mcp-gateway, each with the image this release pins (docs/managed-sandbox.md 5.4, 9).
func SandboxTemplates() (map[string]any, error) {
	user := func(t map[string]any) map[string]any {
		t["user"], t["workdir"] = "user", "/home/user"
		return t
	}
	out := map[string]any{
		"base": user(map[string]any{"image": "base", "cpu": 1, "memoryGiB": 1, "description": "E2B base image"}),
		"code-interpreter-v1": user(map[string]any{"image": "code-interpreter", "cpu": 2, "memoryGiB": 2, "description": "Code interpreter (Python, JavaScript, Bash)",
			"startCmd": "/root/.jupyter/start-up.sh", "startAsRoot": true, "readyCmd": "curl -fsS localhost:49999/health"}),
		"python":      map[string]any{"image": "python", "cpu": 1, "memoryGiB": 2, "description": "Python 3.12"},
		"mcp-gateway": user(map[string]any{"image": "mcp-gateway", "cpu": 2, "memoryGiB": 4, "description": "MCP gateway", "startCmd": "mcp-gateway boot", "startAsRoot": true}),
	}
	for name, t := range out {
		ref, err := sandboxPin(t.(map[string]any)["image"].(string))
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		t.(map[string]any)["image"] = ref
	}
	return out, nil
}

// SandboxSiteConfig is config.sandbox of the installation's Proxmox provider: the keys installer stage 60 writes (pool,
// nodes, loadBalancers, dns, kubeletSources, unsecuredMinAgeMinutes, proxy), and what the platform network adds: the
// owner, the mode, the bridge and tag of the machines' NIC, the platform prefix as a network no sandbox machine
// reaches, and with shared the platform cluster's own machines (peers).
//
// Shared names no proxy and no load balancer: sandbox-proxy runs on the system group, so no sandbox machine answers
// for its address, and none may. Separate names the proxy address as the one load balancer address: the sandbox
// cluster's own MetalLB announces it from a machine of the pool.
func SandboxSiteConfig(in RenderInput) map[string]any {
	sb := in.Sandbox
	cfg := map[string]any{
		ProviderMarker: in.ID, "mode": sb.Mode, "pool": pve.SandboxPool,
		"nodes": []any{sb.NodeRange}, "dns": anyList(sb.DNS), "unsecuredMinAgeMinutes": sandboxUnsecuredMinutes,
		"bridge": in.Network.Bridge, "vlan": in.Network.VLAN, "deny": []any{sb.PlatformPrefix},
	}
	if sb.Mode == SandboxShared {
		// The API server of the platform cluster runs on its control plane machines and calls the kubelet itself.
		cfg["peers"], cfg["kubeletSources"] = anyList(in.Cluster.NodeRange), anyList(in.Cluster.NodeRange)
	} else {
		cfg["loadBalancers"] = []any{sb.ProxyAddress}
		cfg["proxy"] = map[string]any{"address": sb.ProxyAddress, "port": 443}
	}
	return cfg
}

// SandboxProviderBody is the POST /admin/service body of the sandbox provider, after installer stage 60: DISABLED,
// envd on with the sandbox domain, the templates of this release by digest, no limits (the portal's defaults), the
// installer's egress cap and abuse defaults, rotation off, no registry. mode shared tells the portal that its identity
// holds only what it binds in each project namespace. kubeconfig is the chart's proxcloud-sandbox identity.
func SandboxProviderBody(in RenderInput, proxmoxServiceID, kubeconfig string) (map[string]any, error) {
	templates, err := SandboxTemplates()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"name": in.Name + " Sandboxes", "type": "CLOUD", "status": "DISABLED",
		"config": map[string]any{
			"provider": "sandbox", "region": in.Region,
			"regions":  map[string]any{in.Region: map[string]any{"displayName": in.RegionName}},
			"services": map[string]any{"sandbox": map[string]any{in.Region: true}},
			"sandbox": map[string]any{
				"proxmoxServiceId": proxmoxServiceID, "mode": in.Sandbox.Mode,
				"envd": true, "domain": in.Sandbox.Domain, "templates": templates, "limits": map[string]any{}, "egressMbps": 100,
				"abuse":    map[string]any{"disabled": false, "cpuPercent": 90, "windowSeconds": 900, "maxGapSeconds": 90, "trafficBytes": 65536, "action": "alert", "notifyEmails": []any{}},
				"rotation": map[string]any{"enabled": false},
			},
			"attachToNewProjects": false,
			ProviderMarker:        in.ID,
		},
		"secret": map[string]any{"kubeconfig": kubeconfig},
	}, nil
}

// sandboxSite writes the Proxmox provider's sandbox settings (Runner.SandboxSite). kubeconfig "" keeps the stored one.
func (x *acq) sandboxSite(ctx context.Context, kubeconfig string) error {
	if x.r.SandboxSite == nil { // tests that register nothing
		return nil
	}
	in := x.renderInput()
	err := x.r.SandboxSite(ctx, x.in, &SandboxSite{Range: in.Sandbox.NodeRange, Config: SandboxSiteConfig(in), Kubeconfig: kubeconfig})
	return sandboxSiteErr(err)
}

func sandboxSiteErr(err error) error {
	switch {
	case err == nil:
		return nil
	case strings.Contains(err.Error(), CodeProviderConflict):
		return permanent(CodeSandboxRefused, "%s", clip(err.Error()))
	}
	return classify(CodeSandboxAccessFailed, err)
}

// sandboxOffAllowed refuses to take sandboxes away while their provider is live: tenants would lose running sandboxes
// with no notice. The operator disables the provider and lets them end first.
func (x *acq) sandboxOffAllowed(ctx context.Context) error {
	ids := x.in.Status.Providers
	if ids == nil || ids.Sandbox == "" || x.r.Provider == nil {
		return nil
	}
	es, err := x.r.Provider(ctx, ids.Sandbox)
	if err != nil {
		return err
	}
	if why := SandboxOffBlocked(es); why != "" && es.IsSandbox() {
		return permanent(CodeSandboxRefused, "%s Nothing was changed.", why)
	}
	return nil
}

// SandboxSwitchedOff reports that next takes away the sandboxes a run built for old (placement off, or the block's
// placement emptied), and names the sandbox provider a run registered for them ("" = none).
func SandboxSwitchedOff(old, next *Installation) (providerID string, off bool) {
	if old.Status.Sandbox == nil || next.Spec.sandboxMode() != "" {
		return "", false
	}
	if p := old.Status.Providers; p != nil {
		providerID = p.Sandbox
	}
	return providerID, true
}

// SandboxOffBlocked is why sandboxes cannot be switched off (placement off, or a Remove is not asked here) while es,
// the installation's sandbox provider, is live; "" when it is disabled or gone.
func SandboxOffBlocked(es *externalservice.ExternalService) string {
	if es == nil || es.IsDisabled() {
		return ""
	}
	return fmt.Sprintf("The sandbox provider %q is %s: switching sandboxes off deletes every sandbox machine and with it every running sandbox. "+
		"Set the provider DISABLED under Cloud providers and let its sandboxes end, then set spec.sandbox.placement off.", es.Name, es.Status)
}

// sandboxKubeconfigs reads the tokens of the sandbox chart's two identities from the cluster that runs sandboxes and
// answers a kubeconfig for each: the portal's (the sandbox provider) and the firewall's (the Proxmox provider's sync
// and the release gates). Shared: the platform cluster, at its endpoint. Separate: pc-sandbox, reached with the admin
// kubeconfig Kamaji keeps on the platform cluster, which is used for this read and stored nowhere.
func (x *acq) sandboxKubeconfigs(ctx context.Context, wl *kamajik8s.Client) (portal, firewall string, err error) {
	target, server, name := wl, WorkloadServer(x.in.Spec.platformCluster().Endpoint), ClusterName
	if x.in.Spec.sandboxMode() == SandboxSeparate {
		if target, err = x.sandboxClusterClient(ctx, wl); err != nil {
			return "", "", err
		}
		server, name = target.Server(), SandboxClusterName
	}
	read := func(secret string) (string, error) {
		data, err := target.GetSecretData(ctx, sandboxNamespace, secret)
		if err != nil && !kamajik8s.NotFound(err) {
			return "", classify(CodeWorkloadUnreachable, err)
		}
		if len(data["token"]) == 0 || len(data["ca.crt"]) == 0 {
			return "", waiting(CodeComponentUnhealthy, "The sandbox chart has not filled %s/%s on %s with a token yet.", sandboxNamespace, secret, name)
		}
		x.hide(string(data["token"]))
		kc, err := kamajik8s.BuildKubeconfig(name, server, data["ca.crt"], string(data["token"]))
		if err != nil {
			return "", permanent(CodeWorkloadUnreachable, "The CA of %s is not usable for the sandbox identities' kubeconfig.", name)
		}
		return kc, nil
	}
	if portal, err = read(sandboxPortalToken); err != nil {
		return "", "", err
	}
	firewall, err = read(sandboxFirewallToken)
	return portal, firewall, err
}

// sandboxClusterClient is a client of pc-sandbox from the admin kubeconfig Kamaji writes beside its control plane.
func (x *acq) sandboxClusterClient(ctx context.Context, wl *kamajik8s.Client) (*kamajik8s.Client, error) {
	secret := kamaji.AdminKubeconfigSecretName(SandboxClusterName)
	data, err := wl.GetSecretData(ctx, SandboxClusterName, secret)
	if err != nil && !kamajik8s.NotFound(err) {
		return nil, classify(CodeWorkloadUnreachable, err)
	}
	raw := data["admin.conf"]
	if len(raw) == 0 {
		return nil, waiting(CodeControlPlaneTimeout, "The control plane of %s is not up yet: Kamaji has not written %s/%s.", SandboxClusterName, SandboxClusterName, secret)
	}
	x.hide(string(raw))
	c, err := kamajik8s.New(string(raw))
	if err != nil {
		return nil, permanent(CodeWorkloadUnreachable, "The admin kubeconfig of %s is not usable.", SandboxClusterName)
	}
	return c, nil
}

// registerSandbox is the sandbox part of stage register: the firewall identity's kubeconfig to the Proxmox provider,
// so its sync fences the machines and takes their unsecured taint off, then the sandbox provider, created DISABLED
// or adopted. An adopted row keeps everything the operator owns (status, templates, limits) and gets the kubeconfig
// again: after a rebuild of the sandbox cluster the old one names a cluster that is gone. "" when the settings ask
// for no sandboxes.
func (x *acq) registerSandbox(ctx context.Context, wl *kamajik8s.Client) (id string, err error) {
	if x.in.Spec.sandboxMode() == "" {
		return "", nil
	}
	portal, firewall, err := x.sandboxKubeconfigs(ctx, wl)
	if err != nil {
		return "", err
	}
	if err := x.check(ctx); err != nil {
		return "", err
	}
	if err := x.sandboxSite(ctx, firewall); err != nil {
		return "", err
	}
	body, err := SandboxProviderBody(x.renderInput(), x.in.ServiceID, portal)
	if err != nil {
		return "", permanent(CodeSandboxRefused, "The sandbox provider could not be described: %s", clip(err.Error()))
	}
	id, err = x.r.Register(ctx, body)
	if err != nil && strings.Contains(err.Error(), CodeProviderConflict) {
		return "", permanent(CodeProviderConflict, "%s", clip(err.Error()))
	}
	return id, err
}

// ownSandboxRanges are the sandbox networks of the provider that are this installation's own: the range of the
// settings, and the one a run built and wrote into config.network.sandboxCidrs, which stays there until the apply that
// switches sandboxes off has deleted the machines. Without the second, saving placement off would be refused because
// "the platform network overlaps sandboxCidrs".
func (in *Installation) ownSandboxRanges() []netip.Prefix {
	var out []netip.Prefix
	if p := in.Spec.sandboxRange(); p.IsValid() {
		out = append(out, p)
	}
	if st := in.Status.Sandbox; st != nil {
		if p, err := netip.ParsePrefix(st.NodeRange); err == nil && !slices.Contains(out, p.Masked()) {
			out = append(out, p.Masked())
		}
	}
	return out
}

package platformlife

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// sandboxBOM is this build's BOM, which offers sandboxes from 0.12.0 on (TestSandboxIsOffered): a cluster chart that
// gives a node group a pool and a range for the platform and for tenants, and the sandbox chart as a component marked
// with the feature. Every test of a sandbox that is built runs against the real pins; it stops here when a BOM change
// takes the offer away.
func sandboxBOM(t *testing.T) Embedded {
	t.Helper()
	cur, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	saved := *cur.BOM
	t.Cleanup(func() { *cur.BOM = saved })
	cur.Components = slices.Clone(cur.Components) // a test may edit a pin
	if cur.Feature(FeatureSandbox) == nil || !kamaji.ChartHasGroupPool(ClusterChartVersion(cur.BOM)) || !kamaji.ChartHasGroupPool(cur.TenantCharts.Kubernetes.Version) {
		t.Fatalf("release %s offers no sandboxes: its BOM needs the sandbox chart and cluster charts %s or newer", cur.Release, kamaji.GroupPoolChartVersion)
	}
	// Placement separate needs a tenant cluster chart under which Cilium's operator starts on the sandbox cluster's
	// tainted nodes (kamaji.OperatorTolerationsChartVersion). main's BOM cannot name that chart before its tag has
	// published it; a release must name it, or it ships a placement that is refused at the save.
	if v := cur.TenantCharts.Kubernetes.Version; !kamaji.ChartHasOperatorTolerations(v) && !strings.HasSuffix(cur.Release, "-dev") {
		t.Fatalf("release %s names tenant cluster chart %s, under which placement separate is refused: publish chart %s (tag proxmox-kamaji-cluster-v%s) and pin it in tenantCharts.kubernetes",
			cur.Release, v, kamaji.OperatorTolerationsChartVersion, kamaji.OperatorTolerationsChartVersion)
	}
	return cur
}

// separateChart is sandboxBOM for a test of placement separate: until main's BOM names the tenant cluster chart that
// placement needs (see sandboxBOM), the test runs against that version. Nothing once the pin has moved.
func separateChart(t *testing.T) Embedded {
	t.Helper()
	cur := sandboxBOM(t)
	if !kamaji.ChartHasOperatorTolerations(cur.TenantCharts.Kubernetes.Version) {
		cur.TenantCharts.Kubernetes.Version, cur.TenantCharts.Kubernetes.Digest = kamaji.OperatorTolerationsChartVersion, "sha256:"+strings.Repeat("a", 64)
	}
	return cur
}

// fixedSandboxPins replaces the pins of deploy/templates/images.yaml for a test that compares a render byte for byte:
// the template workflow moves the real ones, and such a move must not need a golden update.
func fixedSandboxPins(t *testing.T) {
	t.Helper()
	saved := sandboxPins
	t.Cleanup(func() { sandboxPins = saved })
	sandboxPins = func() (map[string]string, error) {
		out := map[string]string{}
		for i, name := range []string{"base", "code-interpreter", "python", "mcp-gateway"} {
			out[name] = fmt.Sprintf("registry.example/sandbox-%s:test@sha256:%064d", name, i+1)
		}
		return out, nil
	}
}

// addSandbox makes b a BOM that offers sandboxes with pins that never move: for a BOM of before the offer (nextRelease
// starts from 0.8.1's), and for the files of TestSandboxRenderGolden, which must not change with main's chart pins.
func addSandbox(b *BOM) {
	wave := 0
	for i := range b.Components {
		c := &b.Components[i]
		if c.Chart.Name == "proxmox-kamaji-cluster" {
			c.Chart.Version, c.Chart.Digest = kamaji.GroupPoolChartVersion, "sha256:"+strings.Repeat("a", 64)
		}
		if c.Cluster == ClusterWorkload {
			wave = max(wave, c.Wave)
		}
	}
	b.TenantCharts.Kubernetes.Version, b.TenantCharts.Kubernetes.Digest = kamaji.OperatorTolerationsChartVersion, "sha256:"+strings.Repeat("a", 64)
	chart := Chart{Repo: "ghcr.io/mutex-solutions/charts", Name: "proxcloud-sandbox", Version: "0.1.0", Digest: "sha256:" + strings.Repeat("b", 64)}
	if c := b.Feature(FeatureSandbox); c != nil {
		c.Chart = chart
		return
	}
	b.Components = append(b.Components, Component{Name: "sandbox", Cluster: ClusterWorkload, Wave: wave + 1, Install: InstallApplication, Namespace: sandboxNamespace, Chart: chart, Feature: FeatureSandbox})
}

// shared and separate are the two blocks the tests start from, on validInstallation's network (10.200.200.0/24, nodes
// .30-.49, MetalLB .200-.220).
func sharedSandbox() *SandboxSpec {
	return &SandboxSpec{Placement: SandboxShared, SandboxMachines: SandboxMachines{Nodes: []string{"pve1"}, Count: 1, Cores: 2, MemoryMiB: 4096, DiskGiB: 40},
		NodeRange: "10.200.200.64/28", ProxyAddress: "10.200.200.210", Domain: "sbx.example.org", TLS: &SandboxTLS{Mode: TLSSelfSigned}}
}

func separateSandbox() *SandboxSpec {
	sb := sharedSandbox()
	sb.Placement, sb.ProxyAddress = SandboxSeparate, "10.200.200.150"
	sb.System = &SandboxMachines{Nodes: []string{"pve2"}, Count: 1, Cores: 2, MemoryMiB: 4096, DiskGiB: 30}
	return sb
}

// This build's BOM offers sandboxes: it names the sandbox chart, published and marked with the feature, after every
// other component of the platform cluster, and cluster charts that give a node group a pool and a range of its own.
// An installation on it is taken the block in both placements, and so is one on a kept BOM that offers them too
// (0.12.0-rc.1, the first, and 0.12.0-rc.2). No BOM of a release before 0.12.0 names the chart, so a platform one of
// them installed is refused the block at the save and in the Check's spec row until an upgrade has moved it to this
// one, and the sentence names that upgrade. A build whose BOM takes the offer away fails here: its release notes must
// then say so.
//
// Placement separate is taken only on a BOM whose tenant cluster chart lets Cilium's operator start on the sandbox
// cluster's tainted nodes (0.1.20): a platform that 0.12.0-rc.1 or 0.12.0-rc.2 installed (chart 0.1.19) is refused it
// at the save, with the order that works (off or shared, the upgrade, then separate), where a run of rc.2 built the
// machines and then waited for a cluster that could not come up (drill E4-A).
func TestSandboxIsOffered(t *testing.T) {
	cur := separateChart(t)
	c := cur.Feature(FeatureSandbox)
	if c.Name != "sandbox" || c.Chart.Name != "proxcloud-sandbox" || c.Chart.Version != "0.1.0" || !strings.HasPrefix(c.Chart.Digest, "sha256:") || c.Namespace != sandboxNamespace || len(c.UpgradeFrom) != 0 {
		t.Fatalf("the sandbox component: %+v", c)
	}
	for _, o := range cur.Components {
		if o.Cluster == ClusterWorkload && o.Feature == "" && o.Wave >= c.Wave {
			t.Errorf("workload/%s is in wave %d, the sandbox chart in %d: it goes after everything else of the cluster", o.Name, o.Wave, c.Wave)
		}
	}
	if err := cur.Validate(); err != nil {
		t.Fatal(err)
	}
	prev, err := Previous()
	if err != nil {
		t.Fatal(err)
	}
	// A new installation, one this build's BOM installed, and one a kept BOM from 0.12.0-rc.1 on installed.
	offering, before := []*BOMRef{nil, {Release: cur.Release, SHA256: cur.SHA256}}, []Embedded(nil)
	tenantChart := map[string]string{cur.Release: cur.TenantCharts.Kubernetes.Version}
	for _, b := range prev {
		old := slices.ContainsFunc([]string{"0.8.", "0.9.", "0.10.", "0.11."}, func(p string) bool { return strings.HasPrefix(b.Release, p) })
		if offers := b.Feature(FeatureSandbox) != nil; offers == old {
			t.Fatalf("release %s names a sandbox chart = %v: no release before 0.12.0 offered sandboxes, and every kept one since does", b.Release, offers)
		}
		if old {
			before = append(before, b)
		} else {
			offering = append(offering, &BOMRef{Release: b.Release, SHA256: b.SHA256})
			tenantChart[b.Release] = b.TenantCharts.Kubernetes.Version
		}
	}
	if len(offering) < 3 || len(before) == 0 {
		t.Fatalf("%d kept BOMs from before sandboxes, %d references that offer them: want 0.12.0-rc.1 among the kept", len(before), len(offering))
	}
	refusedSeparate := 0
	for _, sb := range []*SandboxSpec{sharedSandbox(), separateSandbox()} {
		for _, ref := range offering {
			in := validInstallation(t)
			in.Spec.Sandbox, in.Status.BOM = sb, ref
			chart := cur.TenantCharts.Kubernetes.Version
			if ref != nil {
				chart = tenantChart[ref.Release]
			}
			if sb.Placement == SandboxSeparate && !kamaji.ChartHasOperatorTolerations(chart) {
				p := in.SandboxProblems()
				if len(p) != 1 || !strings.Contains(p[0], "spec.sandbox.placement separate") || !strings.Contains(p[0], "(release "+ref.Release+") names cluster chart "+chart) ||
					!strings.Contains(p[0], "chart "+kamaji.OperatorTolerationsChartVersion+" or newer") || !strings.Contains(p[0], "Set placement off, or shared, which") ||
					!strings.Contains(p[0], "; then upgrade the platform to a release") || !strings.Contains(p[0], "; then set placement separate. In that order") {
					t.Errorf("placement separate on %+v (tenant chart %s): %v", ref, chart, p)
				}
				refusedSeparate++
				continue
			}
			if err := in.Validate(); err != nil || in.SandboxProblems() != nil {
				t.Errorf("placement %s on %+v: %v %v", sb.Placement, ref, err, in.SandboxProblems())
			}
		}
		for _, b := range before {
			in := validInstallation(t)
			in.Spec.Sandbox, in.Status.BOM = sb, &BOMRef{Release: b.Release, SHA256: b.SHA256}
			if p := in.SandboxProblems(); len(p) != 1 || !strings.Contains(p[0], "(release "+b.Release+") has no sandbox chart") || !strings.Contains(p[0], "Upgrade the platform to release "+cur.Release+" first") {
				t.Errorf("installed by %s, placement %s: %v", b.Release, sb.Placement, p)
			}
		}
	}
	// 0.12.0-rc.1 and 0.12.0-rc.2 are kept and pin chart 0.1.19: both were refused separate above.
	if refusedSeparate < 2 {
		t.Errorf("placement separate was refused on %d kept BOMs, want the two candidates that pin tenant chart 0.1.19", refusedSeparate)
	}
	// Off and no block are never refused: an installation on any BOM says "no sandboxes".
	for _, b := range append(prev, cur) {
		for _, sb := range []*SandboxSpec{nil, {Placement: SandboxOff}} {
			in := validInstallation(t)
			in.Spec.Sandbox, in.Status.BOM = sb, &BOMRef{Release: b.Release, SHA256: b.SHA256}
			if err := in.Validate(); err != nil || in.SandboxProblems() != nil {
				t.Errorf("release %s, block %+v: %v %v", b.Release, sb, err, in.SandboxProblems())
			}
		}
	}
	// An operator's certificate has no route that stores it: refused at the save, before a run builds a machine.
	in := validInstallation(t)
	for _, sb := range []*SandboxSpec{sharedSandbox(), separateSandbox()} {
		sb.TLS = &SandboxTLS{Mode: TLSSecret}
		in.Spec.Sandbox = sb
		if p := in.SandboxProblems(); len(p) != 1 || !strings.Contains(p[0], "tls.mode secret") {
			t.Errorf("tls.mode secret, placement %s: %v", sb.Placement, p)
		}
	}
	// A cluster chart that cannot give a group a pool is named, for the chart each placement is built with.
	in.Spec.Sandbox = sharedSandbox()
	comp(t, cur.BOM, ClusterSeed, ClusterName).Chart.Version = "0.1.17"
	if p := in.SandboxProblems(); len(p) != 1 || !strings.Contains(p[0], "cluster chart 0.1.17") {
		t.Errorf("an older cluster chart: %v", p)
	}
	in.Spec.Sandbox = separateSandbox()
	if p := in.SandboxProblems(); p != nil {
		t.Errorf("separate is built with the tenant chart: %v", p)
	}
	cur.TenantCharts.Kubernetes.Version = "0.1.17"
	if p := in.SandboxProblems(); len(p) != 1 || !strings.Contains(p[0], "cluster chart 0.1.17") {
		t.Errorf("an older tenant chart: %v", p)
	}
	// The chart of rc.2: it gives a group its pool, and still no sandbox cluster comes up under it. Shared, which is
	// built with the platform's own chart, is not held back by the tenant chart.
	cur.TenantCharts.Kubernetes.Version = "0.1.19"
	if p := in.SandboxProblems(); len(p) != 1 || !strings.Contains(p[0], "cluster chart 0.1.19") || !strings.Contains(p[0], "never comes up") {
		t.Errorf("the tenant chart of 0.12.0-rc.2: %v", p)
	}
	comp(t, cur.BOM, ClusterSeed, ClusterName).Chart.Version = "0.1.19"
	in.Spec.Sandbox = sharedSandbox()
	if p := in.SandboxProblems(); p != nil {
		t.Errorf("shared under tenant chart 0.1.19: %v", p)
	}
}

// The rules of the block: the range is one aligned CIDR apart from everything else on the platform network, the proxy
// address is where its mode can announce it, and each mode has its own machines.
func TestValidateSandbox(t *testing.T) {
	for _, base := range []*SandboxSpec{sharedSandbox(), separateSandbox()} {
		in := validInstallation(t)
		in.Spec.Sandbox = base
		if err := in.Validate(); err != nil {
			t.Fatalf("%s: %v", base.Placement, err)
		}
	}
	for _, tc := range []struct {
		name, want string
		edit       func(*Installation)
	}{
		{"first-last", "is not one aligned IPv4 CIDR", func(in *Installation) { in.Spec.Sandbox.NodeRange = "10.200.200.64-10.200.200.79" }},
		{"not aligned", "is not one aligned IPv4 CIDR", func(in *Installation) { in.Spec.Sandbox.NodeRange = "10.200.200.65/28" }},
		{"outside the prefix", "is not one aligned IPv4 CIDR", func(in *Installation) { in.Spec.Sandbox.NodeRange = "10.200.201.64/28" }},
		{"the whole prefix", "is not one aligned IPv4 CIDR", func(in *Installation) { in.Spec.Sandbox.NodeRange = "10.200.200.0/24" }},
		{"IPv6", "is not one aligned IPv4 CIDR", func(in *Installation) { in.Spec.Sandbox.NodeRange = "fd00::/64" }},
		{"the network address", "holds the network or the broadcast address", func(in *Installation) { in.Spec.Sandbox.NodeRange = "10.200.200.0/28" }},
		{"the broadcast address", "holds the network or the broadcast address", func(in *Installation) { in.Spec.Sandbox.NodeRange = "10.200.200.240/28" }},
		{"holds the seed", "spec.seed.address 10.200.200.10 is inside spec.sandbox.nodeRange", func(in *Installation) { in.Spec.Sandbox.NodeRange = "10.200.200.8/29" }},
		{"holds the endpoint", "endpoint 10.200.200.20 is inside spec.sandbox.nodeRange", func(in *Installation) { in.Spec.Sandbox.NodeRange = "10.200.200.16/29" }},
		{"overlaps the node range", "overlaps the cluster's nodeRange", func(in *Installation) { in.Spec.Sandbox.NodeRange = "10.200.200.48/28" }},
		{"overlaps MetalLB", "overlaps the cluster's metallbRange", func(in *Installation) { in.Spec.Sandbox.NodeRange = "10.200.200.192/28" }},
		{"too small", "holds 4 addresses; the sandbox machines and one rolling surge per node need 6", func(in *Installation) {
			in.Spec.Sandbox.NodeRange, in.Spec.Sandbox.Count = "10.200.200.64/30", 5
		}},
		{"proxy outside MetalLB", "is outside the cluster's metallbRange", func(in *Installation) { in.Spec.Sandbox.ProxyAddress = "10.200.200.150" }},
		{"proxy is ArgoCD's", "is spec.access.argocd.address", func(in *Installation) {
			in.Spec.Access.ArgoCD = &ArgoCDAccess{Address: "10.200.200.210", Sources: []string{"192.168.33.0/24"}}
		}},
		{"no domain", "is not a DNS name", func(in *Installation) { in.Spec.Sandbox.Domain = "" }},
		{"a domain with a wildcard", "is not a DNS name", func(in *Installation) { in.Spec.Sandbox.Domain = "*.sbx.example.org" }},
		{"four resolvers", "at most three servers", func(in *Installation) { in.Spec.Sandbox.DNS = []string{"1.1.1.1", "8.8.8.8", "9.9.9.9", "1.0.0.1"} }},
		{"a resolver's name", "is not an IPv4 address", func(in *Installation) { in.Spec.Sandbox.DNS = []string{"dns.example"} }},
		{"tls mode", "is neither secret nor selfsigned", func(in *Installation) { in.Spec.Sandbox.TLS.Mode = "acme" }},
		{"a system group with shared", "goes with placement separate", func(in *Installation) { in.Spec.Sandbox.System = &SandboxMachines{} }},
		{"placement", `spec.sandbox.placement "own" is not shared, separate or off`, func(in *Installation) { in.Spec.Sandbox.Placement = "own" }},
		{"no node", "spec.sandbox.nodes: one to 16 PVE nodes", func(in *Installation) { in.Spec.Sandbox.Nodes = nil }},
		{"fewer machines than nodes", "at least one machine per chosen node", func(in *Installation) { in.Spec.Sandbox.Nodes = []string{"pve1", "pve2"} }},
		{"a machine of one core", "spec.sandbox: cores 2 to 64", func(in *Installation) { in.Spec.Sandbox.Cores = 1 }},
		{"a machine of 2 GiB", "spec.sandbox: cores 2 to 64, memoryMiB 4096", func(in *Installation) { in.Spec.Sandbox.MemoryMiB = 2048 }},
		{"separate without system", "placement separate needs the group", func(in *Installation) {
			in.Spec.Sandbox = separateSandbox()
			in.Spec.Sandbox.System = nil
		}},
		{"separate, proxy in MetalLB", "is inside a range of the installation", func(in *Installation) {
			in.Spec.Sandbox = separateSandbox()
			in.Spec.Sandbox.ProxyAddress = "10.200.200.210"
		}},
		{"separate, proxy in its own range", "is inside a range of the installation", func(in *Installation) {
			in.Spec.Sandbox = separateSandbox()
			in.Spec.Sandbox.ProxyAddress = "10.200.200.70"
		}},
		{"separate, proxy is the gateway", "spec.sandbox.proxyAddress and spec.network.gateway are the same address", func(in *Installation) {
			in.Spec.Sandbox = separateSandbox()
			in.Spec.Sandbox.ProxyAddress = "10.200.200.1"
		}},
		{"separate, a small system machine", "spec.sandbox.system: cores 2 to 64", func(in *Installation) {
			in.Spec.Sandbox = separateSandbox()
			in.Spec.Sandbox.System.DiskGiB = 10
		}},
		// A routed platform network is one Proxmox node's: a sandbox machine on another node would be on another network.
		{"routed, a sandbox machine on another node", "spec.network.mode routed is a layout for one Proxmox node", func(in *Installation) {
			routed(in)
			in.Spec.Sandbox.Nodes = []string{"pve2"}
		}},
		// With node templates named by the operator, a sandbox node needs one too.
		{"templates, none on the sandbox node", "no template VMID for node pve4", func(in *Installation) {
			in.Spec.NodeImage = NodeImage{Mode: ImageTemplates, Templates: map[string]int{"pve1": 9001, "pve2": 9002, "pve3": 9003}}
			in.Spec.Sandbox.Nodes = []string{"pve4"}
		}},
	} {
		in := validInstallation(t)
		in.Spec.Sandbox = sharedSandbox()
		tc.edit(in)
		if err := in.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
	// Off reads nothing else of the block, and a routed network on one node takes a sandbox machine there.
	in := validInstallation(t)
	in.Spec.Sandbox = &SandboxSpec{Placement: SandboxOff, NodeRange: "nonsense"}
	if err := in.Validate(); err != nil {
		t.Errorf("off: %v", err)
	}
	in.Spec.Sandbox = sharedSandbox()
	if routed(in); in.Validate() != nil {
		t.Errorf("routed, everything on one node: %v", in.Validate())
	}
}

// The range against the site: it must not overlap what the provider's isolation would refuse (that refusal reaches
// every tenant's network create), the provider must not name another sandbox network, and the installation's own
// range is neither "the platform network overlaps sandboxCidrs" nor ever one of the blocked networks a save adds.
func TestSandboxSite(t *testing.T) {
	in := validInstallation(t)
	in.Spec.Sandbox = sharedSandbox()
	site := Site{CIDRPool: "10.100.0.0/16", Blocked: []string{"10.200.200.0/26"}, Sandbox: []string{"10.200.200.64/28"}}
	if p := in.SiteProblems(site); len(p) != 0 {
		t.Fatalf("a range clear of everything, already the provider's sandbox network: %v", p)
	}
	for _, tc := range []struct {
		name, want string
		edit       func(*Site)
	}{
		{"the tenant pool", "overlaps the tenant pool (cidrPool) 10.200.200.0/24", func(s *Site) { s.CIDRPool = "10.200.200.0/24" }},
		{"a blocked network", "overlaps the blocked network (blockedCidrs) 10.200.200.64/26", func(s *Site) { s.Blocked = []string{"10.200.200.64/26"} }},
		{"a management network", "overlaps the management network 10.200.200.0/25", func(s *Site) { s.Mgmt = []string{"10.200.200.5/25"} }},
		{"a public network", "overlaps the public network 10.200.200.72/29", func(s *Site) { s.Public = []string{"10.200.200.72/29"} }},
		{"another sandbox network", "the provider already names the sandbox network 10.90.0.0/24", func(s *Site) { s.Sandbox = []string{"10.90.0.0/24"} }},
	} {
		s := site
		tc.edit(&s)
		if p := strings.Join(in.SiteProblems(s), "\n"); !strings.Contains(p, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, p, tc.want)
		}
	}
	// What a save adds to blockedCidrs (Unfenced) and what may open the Proxmox API (APIAccess) are the seed, the API
	// address and the platform's node range: never an address of the sandbox range.
	own := in.Spec.sandboxRange()
	sources, _, _ := in.APIAccess()
	for _, c := range append(in.Unfenced(nil), sources...) {
		if p, err := netip.ParsePrefix(c); err != nil || p.Overlaps(own) {
			t.Errorf("%s overlaps the sandbox range %s", c, own)
		}
	}
	// Without sandboxes the old rule stands: a sandbox network of the provider's must not overlap the platform network.
	in.Spec.Sandbox = nil
	if p := strings.Join(in.SiteProblems(site), "\n"); !strings.Contains(p, "overlaps sandboxCidrs") {
		t.Errorf("no sandboxes, a sandbox network inside the platform network: %q", p)
	}
}

// A PUT that leaves the block out keeps the stored one (an older console must not switch sandboxes off), and what a
// run built is fixed: the placement goes only to off, the range and the proxy address stay.
func TestSandboxKeepAndFixed(t *testing.T) {
	old := validInstallation(t)
	old.Spec.Sandbox = sharedSandbox()
	next := validInstallation(t)
	if KeepSandbox(&old.Spec, &next.Spec); next.Spec.Sandbox == nil || next.Spec.Sandbox.Placement != SandboxShared {
		t.Fatalf("a spec without the block: %+v", next.Spec.Sandbox)
	}
	next.Spec.Sandbox = &SandboxSpec{Placement: SandboxOff}
	if KeepSandbox(&old.Spec, &next.Spec); next.Spec.Sandbox.Placement != SandboxOff {
		t.Fatal("off is explicit and stays")
	}
	old.Status.BOM = &BOMRef{Release: "x"}
	change := func(edit func(*SandboxSpec)) []string {
		n := validInstallation(t)
		n.Spec.Sandbox = sharedSandbox()
		edit(n.Spec.Sandbox)
		return FixedChanges(old, n)
	}
	// Saved and never built: everything may still change.
	if got := change(func(sb *SandboxSpec) { sb.Placement, sb.NodeRange = SandboxSeparate, "10.200.200.96/28" }); len(got) != 0 {
		t.Errorf("not built yet: %v", got)
	}
	old.Status.Sandbox = &SandboxStatus{Placement: SandboxShared, NodeRange: "10.200.200.64/28", ProxyAddress: "10.200.200.210", Count: 1}
	for want, edit := range map[string]func(*SandboxSpec){
		"spec.sandbox.placement":    func(sb *SandboxSpec) { sb.Placement = SandboxSeparate },
		"spec.sandbox.nodeRange":    func(sb *SandboxSpec) { sb.NodeRange = "10.200.200.96/28" },
		"spec.sandbox.proxyAddress": func(sb *SandboxSpec) { sb.ProxyAddress = "10.200.200.211" },
	} {
		if got := change(edit); !slices.Equal(got, []string{want}) {
			t.Errorf("built: %v, want %s", got, want)
		}
	}
	for name, edit := range map[string]func(*SandboxSpec){
		"off":     func(sb *SandboxSpec) { *sb = SandboxSpec{Placement: SandboxOff} },
		"sizes":   func(sb *SandboxSpec) { sb.Count, sb.Cores, sb.Nodes = 3, 4, []string{"pve1", "pve2"} },
		"domain":  func(sb *SandboxSpec) { sb.Domain, sb.DNS = "other.example.org", []string{"9.9.9.9"} },
		"nothing": func(*SandboxSpec) {},
	} {
		if got := change(edit); len(got) != 0 {
			t.Errorf("%s may change on a built sandbox: %v", name, got)
		}
	}
}

// One helper adds the shared group, and everything that counts or orders machines reads through it: the roll takes the
// group last, the machine deployments and the memory include it, and without the block the cluster is the stored one.
func TestSandboxGroupThroughOneHelper(t *testing.T) {
	in := validInstallation(t)
	if got := in.Spec.platformCluster(); !reflect.DeepEqual(got, in.Spec.Clusters[0]) {
		t.Fatalf("without the block the platform cluster is spec.clusters[0]: %+v", got)
	}
	before, _ := in.PlannedMemoryMiB()
	in.Spec.Sandbox = sharedSandbox()
	in.Spec.Sandbox.Nodes, in.Spec.Sandbox.Count, in.Spec.Sandbox.MemoryMiB = []string{"pve3", "pve1"}, 3, 8192
	if len(in.Spec.Clusters[0].NodeGroups) != 3 {
		t.Fatal("the helper changed the stored spec")
	}
	units := rollUnits(in.Spec.platformCluster())
	var names []string
	for _, u := range units {
		names = append(names, u.Name)
	}
	if want := []string{"controlPlane", "system@pve1", "kamaji@pve2", "dbaas@pve3", "sandbox@pve1", "sandbox@pve3"}; !slices.Equal(names, want) {
		t.Fatalf("units %v, want %v", names, want)
	}
	// As the chart splits machineCount: the first node listed takes the remainder.
	if u := units[4]; u.Machines != 1 || u.Owner != "pc-platform-sandbox-pve1" || u.MemoryMiB != 8192 {
		t.Errorf("sandbox@pve1: %+v", u)
	}
	if u := units[5]; u.Machines != 2 {
		t.Errorf("sandbox@pve3: %+v", u)
	}
	if mds := machineDeployments(in.Spec.platformCluster()); !slices.Contains(mds, "pc-platform-sandbox-pve3") || len(mds) != 5 {
		t.Errorf("machine deployments %v", mds)
	}
	need, spare := in.PlannedMemoryMiB()
	if need["pve1"]-before["pve1"] != 8192 || need["pve3"]-before["pve3"] != 2*8192 || spare["pve3"] != 4096+8192 {
		t.Errorf("memory: need %v (was %v), spare %v", need, before, spare)
	}
	if nodes := in.ClusterNodes(); !slices.Equal(nodes, []string{"pve1", "pve2", "pve3"}) {
		t.Errorf("nodes %v", nodes)
	}
	// Separate: the machines are not the platform cluster's (no unit, no machine deployment of it), and they still
	// take memory and need the node image on their nodes.
	in.Spec.Sandbox = separateSandbox()
	in.Spec.Sandbox.System.Nodes = []string{"pve4"}
	if got := in.Spec.platformCluster(); !reflect.DeepEqual(got, in.Spec.Clusters[0]) {
		t.Errorf("separate adds no group to the platform cluster: %+v", got.NodeGroups)
	}
	if need, _ := in.PlannedMemoryMiB(); need["pve1"]-before["pve1"] != 4096 || need["pve4"] != 4096 {
		t.Errorf("separate, memory %v", need)
	}
	if nodes := in.ClusterNodes(); !slices.Contains(nodes, "pve4") {
		t.Errorf("separate, nodes %v", nodes)
	}
}

// The BOM's part: a marked component must be a stateless chart of our own, an installation renders it only with the
// shared placement, and it is the one component an upgrade may add.
func TestSandboxFeatureComponent(t *testing.T) {
	b := embedded(t)
	addSandbox(b)
	if err := b.Validate(); err != nil {
		t.Fatalf("a BOM with the sandbox component: %v", err)
	}
	for name, edit := range map[string]func(*Component){
		"another feature":     func(c *Component) { c.Feature = "functions" },
		"a third-party chart": func(c *Component) { c.Chart.Repo = "ghcr.io/mutex-solutions/charts-mirror" },
		"on the seed":         func(c *Component) { c.Cluster, c.Wave = ClusterSeed, 9 },
		"a node group":        func(c *Component) { c.Group = "system" },
		"stateful":            func(c *Component) { c.Stateful = true },
	} {
		bad := embedded(t)
		addSandbox(bad)
		edit(&bad.Components[len(bad.Components)-1])
		if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "feature") {
			t.Errorf("%s: %v", name, err)
		}
	}
	none, shared, separate := validInstallation(t).Spec, validInstallation(t).Spec, validInstallation(t).Spec
	shared.Sandbox, separate.Sandbox = sharedSandbox(), separateSandbox()
	if got := b.For(shared); got != b {
		t.Error("shared renders every component")
	}
	for name, s := range map[string]Spec{"no block": none, "separate": separate} {
		got := b.For(s)
		if got.Feature(FeatureSandbox) != nil || len(got.Components) != len(b.Components)-1 || b.Feature(FeatureSandbox) == nil {
			t.Errorf("%s: %d components of %d", name, len(got.Components), len(b.Components))
		}
	}
	if plain := plain(t); plain.For(none) != plain || plain.For(shared) != plain {
		t.Error("a BOM without a marked component is returned as it is")
	}

	// Classify: from a BOM without the component to one with it is class added and not refused; any other component
	// that is new, and the marked one when it is taken away, stay refused.
	from, to := plain(t), embedded(t)
	if last := to.Components[len(to.Components)-1]; last.Feature != FeatureSandbox {
		t.Fatalf("the sandbox component is the BOM's last: %+v", last)
	}
	changes := Classify(from, to)
	if len(changes) != 1 || changes[0].Class != ClassAdded || changes[0].Blocking != "" || changes[0].Name != "sandbox" || !strings.Contains(changes[0].Impact, "only when the settings ask for it") {
		t.Fatalf("the feature component added: %+v", changes)
	}
	if back := Classify(to, from); len(back) != 1 || back[0].Class != ClassRefused || !strings.Contains(back[0].Blocking, "removed") {
		t.Errorf("the feature component removed: %+v", back)
	}
	to.Components[len(to.Components)-1].Feature = ""
	if c := Classify(from, to); len(c) != 1 || c[0].Class != ClassRefused || !strings.Contains(c[0].Blocking, "added to an installed platform only by a later release") {
		t.Errorf("an unmarked component added: %+v", c)
	}
	to.Components[len(to.Components)-1].Feature, to.Components[len(to.Components)-1].Chart.Repo = FeatureSandbox, "ghcr.io/mutex-solutions/charts-mirror"
	if c := Classify(from, to); len(c) != 1 || c[0].Class != ClassRefused {
		t.Errorf("a marked third-party chart added: %+v", c)
	}
}

// What an apply shows before it builds, switches off or refuses sandboxes on an installed platform.
func TestSandboxPlanRows(t *testing.T) {
	b := sandboxBOM(t)
	in := validInstallation(t)
	in.Status.BOM = &BOMRef{Release: b.Release, SHA256: b.SHA256}
	in.Status.Clusters = []ClusterStatus{{Name: ClusterName}}
	rows := func() map[string]PlanChange {
		p, err := PlanFor(in, "")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]PlanChange{}
		for _, c := range p.Changes {
			out[c.Name] = c
		}
		return out
	}
	if r := rows(); len(r) != 0 {
		t.Fatalf("no block, nothing built: %+v", r)
	}
	in.Spec.Sandbox = sharedSandbox()
	r := rows()
	if c := r["sandbox"]; c.Class != ClassMachines || c.Stage != stageCluster || !strings.Contains(c.To, "sandbox: 1 x 2 cores, 4 GiB, 40 GiB disk on pve1") ||
		!strings.Contains(c.Impact, "holds ONE sandbox") || !strings.Contains(c.Impact, "proxmox-csi node plugin keeps off") {
		t.Errorf("adding shared: %+v", c)
	}
	if c := r["sandbox-access"]; c.Class != ClassSite || c.To != "ACL row: capmox@pve!capi holds ProxCloudCAPI on /pool/proxcloud-sbx" {
		t.Errorf("the ACL row: %+v", c)
	}
	if c := r["sandbox-pool"]; c.Class != ClassSite || c.To != "pool proxcloud-sbx" {
		t.Errorf("the pool: %+v", c)
	}
	in.Spec.Sandbox.Cores = 8
	if c := rows()["sandbox"]; !strings.Contains(c.Impact, "about 7 sandboxes of one vCPU") {
		t.Errorf("8 cores: %s", c.Impact)
	}
	in.Spec.Sandbox = separateSandbox()
	r = rows()
	if c := r["sandbox"]; !strings.Contains(c.To, "sandbox-system: 1 x 2 cores") || !strings.Contains(c.Impact, "not upgraded in place") {
		t.Errorf("adding separate: %+v", c)
	}
	if c := r["sandbox-access"]; c.To != "token capmox@pve!pc-sandbox with ProxCloudCAPI on /pool/proxcloud-sbx" {
		t.Errorf("the token: %+v", c)
	}
	// Built and unchanged: no row. More machines: the row again. Off: what is deleted. Another placement: refused.
	now := in.CreatedAt
	in.Status.Sandbox = &SandboxStatus{Placement: SandboxSeparate, NodeRange: "10.200.200.64/28", ProxyAddress: "10.200.200.150", Count: 1, Nodes: []string{"pve1"},
		SystemCount: 1, SystemNodes: []string{"pve2"}, BuiltAt: &now}
	if r := rows(); len(r) != 0 {
		t.Errorf("built: %+v", r)
	}
	in.Spec.Sandbox.Count = 2
	if _, ok := rows()["sandbox"]; !ok {
		t.Error("a larger group has no row")
	}
	in.Spec.Sandbox = &SandboxSpec{Placement: SandboxOff}
	r = rows()
	if c := r["sandbox"]; c.From != SandboxSeparate || c.To != SandboxOff || !strings.Contains(c.Impact, "Every running sandbox ends") || !strings.HasPrefix(r["sandbox-pool"].To, "deleted:") {
		t.Errorf("off: %+v", r)
	}
	in.Spec.Sandbox = sharedSandbox()
	p, _ := PlanFor(in, "")
	if c := rows()["sandbox"]; c.Class != ClassRefused || len(p.Blocking) != 1 || !strings.Contains(p.Blocking[0], "set placement off, apply, then set shared") {
		t.Errorf("another placement than the built one: %+v, blocking %v", c, p.Blocking)
	}
	// A new installation lists the sandbox chart among its components only with placement shared.
	fresh := validInstallation(t)
	count := func() int { p, _ := PlanFor(fresh, ""); return len(p.Changes) }
	base := count()
	fresh.Spec.Sandbox = sharedSandbox()
	if count() != base+1 {
		t.Errorf("a new installation with shared: %d rows, without %d", count(), base)
	}
}

// A move that replaces the platform's machines is refused while the separate cluster exists or is asked for (no run
// upgrades it in place), and while a shared group is asked for and not built (a roll renders a unit it has not reached
// at the templates the platform left, and a new group has none).
func TestSandboxRefusesAMachineMove(t *testing.T) {
	old := nextRelease(t, func(b *BOM) { newerK8s(b); addSandbox(b) })
	ref := refOf(old)
	cur, _ := Current()
	blocking := func(edit func(*Installation)) string {
		in := rollInstallation(t, &ref)
		edit(in)
		_, up, err := move(in, cur.SHA256)
		if err != nil || up == nil {
			t.Fatalf("move: %+v %v", up, err)
		}
		return strings.Join(up.Blocking, "\n")
	}
	now := validInstallation(t).CreatedAt
	if got := blocking(func(*Installation) {}); strings.Contains(got, "spec.sandbox") {
		t.Fatalf("no sandboxes: %q", got)
	}
	for name, edit := range map[string]func(*Installation){
		"separate in the settings": func(in *Installation) { in.Spec.Sandbox = separateSandbox() },
		"separate built, settings off": func(in *Installation) {
			in.Spec.Sandbox, in.Status.Sandbox = &SandboxSpec{Placement: SandboxOff}, &SandboxStatus{Placement: SandboxSeparate}
		},
	} {
		if got := blocking(edit); !strings.Contains(got, "the sandbox cluster pc-sandbox is not upgraded in place") {
			t.Errorf("%s: %q", name, got)
		}
	}
	if got := blocking(func(in *Installation) { in.Spec.Sandbox = sharedSandbox() }); !strings.Contains(got, "the node group sandbox is not built yet") {
		t.Errorf("shared, not built: %q", got)
	}
	// A built shared group rolls with the others: its unit is the last, and its row says that sandboxes end.
	in := rollInstallation(t, &ref)
	in.Spec.Sandbox, in.Status.Sandbox = sharedSandbox(), &SandboxStatus{Placement: SandboxShared, Count: 1, BuiltAt: &now}
	_, up, err := move(in, cur.SHA256)
	if err != nil || strings.Contains(strings.Join(up.Blocking, "\n"), "spec.sandbox") {
		t.Fatalf("shared, built: %+v %v", up, err)
	}
	last := PlanChange{}
	for _, c := range up.Changes {
		if strings.HasPrefix(c.Name, "roll/") {
			last = c
		}
	}
	if last.Name != "roll/sandbox@pve1" || !strings.Contains(last.Impact, "Every sandbox running on this machine ENDS") || !strings.Contains(last.Impact, "20 minutes old") {
		t.Errorf("the last unit of the roll: %+v", last)
	}
}

func sandboxRender(sb *SandboxSpec) RenderInput {
	in := renderInput()
	in.Sandbox = &RenderSandbox{Mode: sb.Placement, NodeRange: "192.0.2.64/28", PlatformPrefix: "192.0.2.0/24", DNS: []string{"192.0.2.53"}, Domain: sb.Domain}
	if sb.Placement == SandboxShared {
		in.Sandbox.ProxyAddress = "192.0.2.245"
		in.Cluster.NodeGroups = append(in.Cluster.NodeGroups, RenderNodeGroup{Name: SandboxGroup, Nodes: []string{"pve2"}, Count: 1, Cores: 2, MemoryMiB: 4096, DiskGiB: 40})
		return in
	}
	in.Sandbox.ProxyAddress = "192.0.2.150"
	in.Sandbox.Groups = []RenderNodeGroup{{Name: SandboxGroup, Nodes: []string{"pve2", "pve3"}, Count: 2, Cores: 4, MemoryMiB: 8192, DiskGiB: 40},
		{Name: SandboxSystemGroup, Nodes: []string{"pve1"}, Count: 1, Cores: 2, MemoryMiB: 4096, DiskGiB: 30}}
	return in
}

// What the renders give ArgoCD for each placement, as files to review, and what they must not change: with the sandbox
// chart in the BOM, an installation without the block renders exactly what the BOM without the chart renders.
// `go test ./internal/platformlife -run TestSandboxRenderGolden -update` rewrites the files.
func TestSandboxRenderGolden(t *testing.T) {
	fixedSandboxPins(t)
	plain := plain(t)
	b := embedded(t)
	addSandbox(b)
	// The bump of the two chart versions is the synthetic BOM's, not the sandbox's: both sides get it.
	for i := range plain.Components {
		if c := &plain.Components[i]; c.Chart.Name == "proxmox-kamaji-cluster" {
			c.Chart = comp(t, b, ClusterSeed, ClusterName).Chart
		}
	}
	plain.TenantCharts = b.TenantCharts
	plain.Release, b.Release = "0.0.0-synthetic", "0.0.0-synthetic" // the files do not change with main's release line
	none := validInstallation(t).Spec
	want, got := renderAll(t, plain, renderInput()), renderAll(t, b.For(none), renderInput())
	if !sameJSON(want.cluster, got.cluster) || !sameJSON(want.apps, got.apps) || !sameJSON(want.projects, got.projects) || !sameJSON([]any{want.k8sBody, want.dbBd}, []any{got.k8sBody, got.dbBd}) {
		t.Fatal("an installation without spec.sandbox renders differently under a BOM with the sandbox chart")
	}

	shared := sharedSandbox()
	in := sandboxRender(shared)
	spec := none
	spec.Sandbox = shared
	r := renderAll(t, b.For(spec), in)
	var app, csi map[string]any
	for _, a := range r.apps {
		switch str(a, "metadata", "name") {
		case "pc-platform-sandbox":
			app = a
		case "pc-platform-proxmox-csi":
			csi = a
		}
	}
	if app == nil || dig(app, "spec", "syncPolicy", "managedNamespaceMetadata") != nil {
		t.Fatalf("the sandbox chart's Application, without a namespace placement: %v", app)
	}
	groups := r.cluster["nodeGroups"].([]any)
	golden(t, "render-sandbox-shared.golden.yaml", yamlDoc(t, map[string]any{"nodeGroup": groups[len(groups)-1], "application": app,
		"csiNode": dig(csi, "spec", "source", "helm", "valuesObject", "node")}))
	// Every other group of the cluster renders as without the block: adding the group replaces no machine.
	if !sameJSON(groups[:len(groups)-1], want.cluster["nodeGroups"]) {
		t.Error("the sandbox group changed another group's values")
	}

	separate := separateSandbox()
	in = sandboxRender(separate)
	sapp, err := SandboxApplication(b, in)
	if err != nil {
		t.Fatal(err)
	}
	secret := SandboxCredentials(b, in, PVEToken{ID: "capmox@pve!pc-sandbox", Secret: "PLANT-sandbox-secret-0009"})
	golden(t, "render-sandbox-separate.golden.yaml", yamlDoc(t, map[string]any{"application": sapp, "credentials": secret}))
	if raw, _ := json.Marshal(sapp); strings.Contains(string(raw), "PLANT-") {
		t.Error("the Application carries the token")
	}
	// The provider sync's orphan sweep reaps the Secrets it selects by these two labels (kamaji.Service.FinalizeAllOrphans)
	// once their cluster's Application is gone from the platform cluster's ArgoCD, which never held this one.
	l, _ := dig(secret, "metadata", "labels").(map[string]any)
	if l[kamaji.LabelService] != nil || l[kamaji.LabelManagedBy] == kamaji.ManagedByValue || l["app.kubernetes.io/managed-by"] != ManagedBy {
		t.Errorf("the sandbox cluster's Secret must not look like a tenant cluster's: %v", l)
	}
	// Built with one chart version, the cluster keeps it whatever the BOM names later.
	in.Sandbox.ChartVersion = "0.1.20"
	b.TenantCharts.Kubernetes.Version = "0.1.21"
	if sapp, _ = SandboxApplication(b, in); str(sapp, "spec", "source", "targetRevision") != "0.1.20" {
		t.Errorf("chart version %s", str(sapp, "spec", "source", "targetRevision"))
	}
	// A cluster that a run of 0.12.0-rc.2 began keeps chart 0.1.19, under which it never comes up: the render is
	// refused and says the way out, where the run would wait its 20 minutes again.
	in.Sandbox.ChartVersion = "0.1.19"
	if _, err := SandboxApplication(b, in); err == nil || !strings.Contains(err.Error(), "under cluster chart 0.1.19") || !strings.Contains(err.Error(), "set placement off, apply, then set separate again") {
		t.Errorf("a sandbox cluster kept on chart 0.1.19: %v", err)
	}
	// An older cluster chart ignores a group's pool and would put sandbox machines into proxcloud-system: refused.
	old := embedded(t)
	comp(t, old, ClusterSeed, ClusterName).Chart.Version = "0.1.17"
	if _, err := ClusterValues(old, sandboxRender(shared)); err == nil || !strings.Contains(err.Error(), "cannot give a node group a pool") {
		t.Errorf("cluster chart %s with a sandbox group: %v", ClusterChartVersion(old), err)
	}
}

// The charts take what the renders write. The cluster chart (this repository's, the version the synthetic BOM names):
// the sandbox group's machines go to the sandbox pool with an address pool of their own and no address from the
// cluster's, and every template of the three other groups keeps its name, so adding the group replaces no machine.
// The sandbox chart renders with the run's values in mode shared; the separate cluster's values render a Kamaji
// control plane, both groups and the sandbox chart as an add-on.
func TestSandboxChartsTakeTheValues(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	b := embedded(t)
	addSandbox(b)
	render := func(release, chart string, values map[string]any) []map[string]any {
		t.Helper()
		f := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(f, yamlDoc(t, values), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(helm, "template", release, "../../deploy/charts/"+chart, "-n", release, "-f", f).CombinedOutput()
		if err != nil {
			t.Fatalf("helm template %s: %v\n%s", chart, err, out)
		}
		return yamlObjects(t, out)
	}
	names := func(objs []map[string]any) (templates []string, byKind map[string][]map[string]any) {
		byKind = map[string][]map[string]any{}
		for _, o := range objs {
			kind, _ := o["kind"].(string)
			byKind[kind] = append(byKind[kind], o)
			if name := str(o, "metadata", "name"); (kind == "ProxmoxMachineTemplate" || kind == "KubeadmConfigTemplate") && !strings.Contains(name, "-"+SandboxGroup+"-") {
				templates = append(templates, kind+"/"+name)
			}
		}
		slices.Sort(templates)
		return templates, byKind
	}
	without, err := ClusterValues(b, renderInput())
	if err != nil {
		t.Fatal(err)
	}
	with, err := ClusterValues(b, sandboxRender(sharedSandbox()))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := names(render(ClusterName, "proxmox-kamaji-cluster", without))
	after, kinds := names(render(ClusterName, "proxmox-kamaji-cluster", with))
	if !slices.Equal(before, after) || len(before) == 0 {
		t.Fatalf("adding the sandbox group renames a template of another group:\n%v\n%v", before, after)
	}
	var pool, ipam map[string]any
	for _, o := range kinds["ProxmoxMachineTemplate"] {
		if strings.HasPrefix(str(o, "metadata", "name"), "pc-platform-sandbox-pve2-") {
			pool = o
		}
	}
	for _, o := range kinds["InClusterIPPool"] {
		if str(o, "metadata", "name") == "pc-platform-sandbox" {
			ipam = o
		}
	}
	spec, _ := dig(pool, "spec", "template", "spec").(map[string]any)
	raw, _ := json.Marshal(spec)
	if spec["pool"] != pve.SandboxPool || !strings.Contains(string(raw), `"defaultIPv4":false`) || !strings.Contains(string(raw), `"name":"pc-platform-sandbox"`) || !strings.Contains(string(raw), "192.0.2.53") {
		t.Errorf("the sandbox group's machine template: %s", raw)
	}
	if got, _ := json.Marshal(dig(ipam, "spec", "addresses")); string(got) != `["192.0.2.64/28"]` {
		t.Errorf("the group's address pool: %s", got)
	}
	for _, o := range kinds["ProxmoxMachineTemplate"] {
		if s, _ := dig(o, "spec", "template", "spec").(map[string]any); str(o, "metadata", "name") != str(pool, "metadata", "name") && s["pool"] != pve.SystemPool {
			t.Errorf("%s is in pool %v", str(o, "metadata", "name"), s["pool"])
		}
	}
	var taints, labels string
	for _, o := range kinds["KubeadmConfigTemplate"] {
		if strings.HasPrefix(str(o, "metadata", "name"), "pc-platform-sandbox-") {
			raw, _ := json.Marshal(o)
			taints, labels = string(raw), string(raw)
		}
	}
	if !strings.Contains(taints, SandboxTaintKey) || !strings.Contains(taints, UnsecuredTaintKey) || strings.Contains(taints, `"key":"`+GroupTaintKey+`"`) ||
		!strings.Contains(labels, "proxcloud.io/node-group=sandbox") {
		t.Errorf("the sandbox group's kubelet registration: %s", taints)
	}

	// The sandbox chart, with what componentPart writes for the shared placement.
	spec2 := validInstallation(t).Spec
	spec2.Sandbox = sharedSandbox()
	apps, err := Applications(b.For(spec2), sandboxRender(sharedSandbox()))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range apps {
		if str(a, "metadata", "name") != "pc-platform-sandbox" {
			continue
		}
		values, _ := dig(a, "spec", "source", "helm", "valuesObject").(map[string]any)
		_, kinds := names(render("sandbox", "proxcloud-sandbox", values))
		if len(kinds["DaemonSet"]) == 0 || len(kinds["RuntimeClass"]) != 1 || len(kinds["CiliumNodeConfig"]) != 1 {
			t.Errorf("the sandbox chart in mode shared: %d DaemonSets, %d RuntimeClasses, %d CiliumNodeConfigs", len(kinds["DaemonSet"]), len(kinds["RuntimeClass"]), len(kinds["CiliumNodeConfig"]))
		}
		// One toleration on the RuntimeClass: never the unsecured taint, which keeps sandboxes off an unfenced machine.
		if raw, _ := json.Marshal(kinds["RuntimeClass"]); strings.Contains(string(raw), UnsecuredTaintKey) || !strings.Contains(string(raw), SandboxTaintKey) {
			t.Errorf("RuntimeClass: %s", raw)
		}
	}

	values, err := SandboxClusterValues(b, sandboxRender(separateSandbox()))
	if err != nil {
		t.Fatal(err)
	}
	_, kinds = names(render(SandboxClusterName, "proxmox-kamaji-cluster", values))
	if len(kinds["KamajiControlPlane"]) != 1 || len(kinds["MachineDeployment"]) != 3 || len(kinds["KubeadmControlPlane"]) != 0 {
		t.Fatalf("the separate cluster: %d KamajiControlPlanes, %d MachineDeployments", len(kinds["KamajiControlPlane"]), len(kinds["MachineDeployment"]))
	}
	for _, o := range kinds["ProxmoxMachineTemplate"] {
		if s, _ := dig(o, "spec", "template", "spec").(map[string]any); s["pool"] != pve.SandboxPool {
			t.Errorf("%s is in pool %v", str(o, "metadata", "name"), s["pool"])
		}
	}
	found := false
	for _, o := range kinds["HelmChartProxy"] {
		if raw, _ := json.Marshal(o); strings.Contains(string(raw), "proxcloud-sandbox") {
			tmpl := str(o, "spec", "valuesTemplate")
			found = strings.Contains(tmpl, `"mode":"separate"`) && strings.Contains(tmpl, `"address":"192.0.2.150"`) && str(o, "spec", "version") == "0.1.0" &&
				str(o, "spec", "repoURL") == "oci://ghcr.io/mutex-solutions/charts"
		}
	}
	if !found {
		t.Error("the separate cluster does not install the sandbox chart in mode separate at the BOM's version with the proxy address")
	}
}

// The separate sandbox cluster comes up on nodes that all join tainted (drill E4-A: under v0.12.0-rc.2 it never did).
// Its control plane is hosted, so its two node groups are all the nodes it has, and both join with a taint. Cilium's
// operator registers Cilium's CRDs: no agent starts and no node becomes Ready before it has, and the sandbox chart,
// whose identities the run waits for, is refused without them. So the operator tolerates every taint a group of the
// cluster registers with, in the values the run writes and in what the cluster chart renders from them. The same holds
// for the installer's template, which writes the same cluster.
func TestSandboxClusterComesUpOnTaintedNodes(t *testing.T) {
	b := embedded(t)
	addSandbox(b)
	values, err := SandboxClusterValues(b, sandboxRender(separateSandbox()))
	if err != nil {
		t.Fatal(err)
	}
	tolerated := map[string]bool{}
	tols, _ := values["ciliumOperatorTolerations"].([]any)
	for _, tol := range tols {
		m := tol.(map[string]any)
		if m["operator"] != "Exists" || m["effect"] != "NoSchedule" {
			t.Errorf("toleration %v", m)
		}
		tolerated[m["key"].(string)] = true
	}
	groups, _ := values["nodeGroups"].([]any)
	if len(groups) != 2 {
		t.Fatalf("%d node groups", len(groups))
	}
	for _, g := range groups {
		taints, _ := g.(map[string]any)["taints"].([]any)
		if len(taints) == 0 {
			t.Errorf("group %v joins without a taint: a sandbox could start on a machine with no VM firewall", g.(map[string]any)["name"])
		}
		for _, tn := range taints {
			if key := str(tn.(map[string]any), "key"); !tolerated[key] {
				t.Errorf("group %v joins with taint %s, which Cilium's operator does not tolerate (%v): no node of the cluster becomes Ready", g.(map[string]any)["name"], key, tols)
			}
		}
	}
	// The operator's ClusterRole reads the Secrets of every namespace, and the kubelet of a node can ask for the token
	// of a pod on it: the operator is held to the system group, whose label no kubelet can claim, and never shares a
	// node with code of tenants (docs/managed-sandbox.md §4.5). The tolerations alone would let it onto the sandbox
	// group, which carries no taint but unsecured.
	selector, _ := values["ciliumOperatorNodeSelector"].(map[string]any)
	if len(selector) != 1 || selector[sandboxSystemLabel] != "true" || !strings.HasPrefix(sandboxSystemLabel, "node-restriction.kubernetes.io/") {
		t.Errorf("Cilium's operator is selected onto %v, want the system group's label %s alone", selector, sandboxSystemLabel)
	}
	for _, g := range groups {
		name := g.(map[string]any)["name"]
		if has := dig(g, "machineLabels", sandboxSystemLabel) == "true"; has != (name == SandboxSystemGroup) {
			t.Errorf("group %v carries the system label: %v", name, has)
		}
		if dig(g, "nodeLabels", sandboxSystemLabel) != nil {
			t.Errorf("group %v gets %s from its own kubelet", name, sandboxSystemLabel)
		}
	}
	// Nothing else is let onto an unfenced machine by these values: MetalLB's speaker tolerates the system group's
	// taint alone, as before.
	if raw, _ := json.Marshal(dig(values, "proxmox", "metallb")); strings.Contains(string(raw), UnsecuredTaintKey) {
		t.Errorf("MetalLB tolerates %s: %s", UnsecuredTaintKey, raw)
	}
	// MetalLB's controller is held to the system group as the operator is, and tolerates that group's taint alone: it
	// may rewrite its own ValidatingWebhookConfiguration, whose Service points at its pod, so from its node the API
	// server can be made to send every Secret that is written (docs/managed-sandbox.md §4.5).
	systemToleration := []any{map[string]any{"key": sandboxSystemTaint, "operator": "Exists", "effect": "NoSchedule"}}
	if got := dig(values, "proxmox", "metallb", "controllerNodeSelector"); !reflect.DeepEqual(got, map[string]any{sandboxSystemLabel: "true"}) {
		t.Errorf("MetalLB's controller is selected onto %v, want the system group's label %s alone", got, sandboxSystemLabel)
	}
	if got := dig(values, "proxmox", "metallb", "controllerTolerations"); !reflect.DeepEqual(got, systemToleration) {
		t.Errorf("MetalLB's controller tolerates %v, want %s alone", got, sandboxSystemTaint)
	}

	// The installer writes the same cluster: every taint its template names is tolerated there too.
	raw, err := os.ReadFile("../../installer/playbooks/templates/sandbox-cluster.yml.j2")
	if err != nil {
		t.Fatal(err)
	}
	_, block, ok := strings.Cut(string(raw), "ciliumOperatorTolerations:")
	if !ok {
		t.Fatal("the installer's sandbox cluster gives Cilium's operator no toleration")
	}
	block, _, _ = strings.Cut(block, "kamajiControlPlane:")
	for key := range tolerated {
		if !strings.Contains(block, "{key: "+key+", operator: Exists, effect: NoSchedule}") {
			t.Errorf("the installer's sandbox cluster: Cilium's operator does not tolerate %s", key)
		}
	}
	// And the operator is held to the system group whenever the template renders one (pc_sandbox_edge_on); without a
	// sandbox domain the cluster has the sandbox group alone, and the selector would leave the operator no node.
	if !regexp.MustCompile(`(?s)^.*\{% if pc_sandbox_edge_on \| bool %\}\n(\s*#[^\n]*\n)*\s*ciliumOperatorNodeSelector: \{` + regexp.QuoteMeta(sandboxSystemLabel) + `: "true"\}\n\{% endif %\}`).MatchString(block) {
		t.Errorf("the installer's sandbox cluster does not hold Cilium's operator to the system group when it has one:\n%s", block)
	}
	if !regexp.MustCompile(`(?s)\{% if pc_sandbox_edge_on \| bool %\}[^%]*- name: sandbox-system\n[^%]*machineLabels: \{` + regexp.QuoteMeta(sandboxSystemLabel) + `: "true"\}`).MatchString(string(raw)) {
		t.Errorf("the installer's system group does not carry %s under the same condition", sandboxSystemLabel)
	}
	// MetalLB's controller the same, under the same condition, with the system group's taint alone to tolerate.
	if !regexp.MustCompile(`(?m)^ *speakerTolerations: [^\n]*\n\{% if pc_sandbox_edge_on \| bool %\}\n( *#[^\n]*\n)* *controllerNodeSelector: \{` + regexp.QuoteMeta(sandboxSystemLabel) +
		`: "true"\}\n *controllerTolerations: \[\{key: ` + regexp.QuoteMeta(sandboxSystemTaint) + `, operator: Exists, effect: NoSchedule\}\]\n\{% endif %\}`).MatchString(string(raw)) {
		t.Errorf("the installer's sandbox cluster does not hold MetalLB's controller to the system group when it has one")
	}
	if n := strings.Count(string(raw), "taints:"); n != 2 {
		t.Errorf("the installer's template names %d taint lists, this test knows two (sandbox, sandbox-system)", n)
	}
	for _, m := range regexp.MustCompile(`\{key: (proxcloud\.io/[a-z-]+), value: "true", effect: NoSchedule\}`).FindAllStringSubmatch(string(raw), -1) {
		if !tolerated[m[1]] {
			t.Errorf("the installer's template taints a group with %s, which Cilium's operator does not tolerate", m[1])
		}
	}

	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI: the chart's side of this is not shown without it")
		}
		t.Skip("helm is not installed: the values were checked, the chart's render was not")
	}
	f := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(f, yamlDoc(t, values), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(helm, "template", SandboxClusterName, "../../deploy/charts/proxmox-kamaji-cluster", "-n", SandboxClusterName, "-f", f).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	registered, operator := map[string]bool{}, map[string]bool{}
	var metallb map[string]any // the values of MetalLB's release, as the chart renders them
	type group struct {
		labels map[string]any // on the Node: the Machine's, which Cluster API copies, and the kubelet's own
		taints []string       // as the kubelet registers
	}
	rendered, joins, operatorSelector := map[string]*group{}, map[string][2]string{}, map[string]any(nil)
	for _, o := range yamlObjects(t, out) {
		switch o["kind"] {
		case "MachineDeployment":
			g := &group{labels: map[string]any{"kubernetes.io/os": "linux"}}
			machine, _ := dig(o, "spec", "template", "metadata", "labels").(map[string]any)
			for k, v := range machine {
				if strings.HasPrefix(k, "node-restriction.kubernetes.io/") {
					g.labels[k] = v
				}
			}
			name := str(o, "metadata", "labels", "proxcloud.io/node-group")
			rendered[name] = g
			joins[str(o, "spec", "template", "spec", "bootstrap", "configRef", "name")] = [2]string{name, ""}
		}
	}
	for _, o := range yamlObjects(t, out) {
		switch o["kind"] {
		case "KubeadmConfigTemplate":
			taints, _ := dig(o, "spec", "template", "spec", "joinConfiguration", "nodeRegistration", "taints").([]any)
			g := rendered[joins[str(o, "metadata", "name")][0]]
			if g == nil {
				t.Fatalf("bootstrap template %s belongs to no MachineDeployment", str(o, "metadata", "name"))
			}
			for _, tn := range taints {
				registered[str(tn.(map[string]any), "key")] = true
				g.taints = append(g.taints, str(tn.(map[string]any), "key"))
			}
			args, _ := dig(o, "spec", "template", "spec", "joinConfiguration", "nodeRegistration", "kubeletExtraArgs").([]any)
			for _, a := range args {
				if str(a.(map[string]any), "name") != "node-labels" {
					continue
				}
				for _, kv := range strings.Split(str(a.(map[string]any), "value"), ",") {
					k, v, _ := strings.Cut(kv, "=")
					if strings.HasPrefix(k, "node-restriction.kubernetes.io/") {
						t.Errorf("a kubelet of group %s names %s itself: under NodeRestriction the node would not register", joins[str(o, "metadata", "name")][0], k)
					}
					g.labels[k] = v
				}
			}
		case "HelmChartProxy":
			var v map[string]any
			if err := json.Unmarshal([]byte(str(o, "spec", "valuesTemplate")), &v); err != nil {
				t.Fatal(err)
			}
			if str(o, "spec", "chartName") == "metallb" {
				metallb = v
			}
			if str(o, "spec", "chartName") != "cilium" {
				continue
			}
			ts, _ := dig(v, "operator", "tolerations").([]any)
			for _, tol := range ts {
				operator[str(tol.(map[string]any), "key")] = true
			}
			operatorSelector, _ = dig(v, "operator", "nodeSelector").(map[string]any)
		}
	}
	// Where the scheduler may put the rendered operator: a node whose labels hold its selector and whose taints it
	// tolerates. Before the fence a machine carries the taints it joined with; the fence takes unsecured off and
	// nothing else, so after it the sandbox group has no taint at all. The operator fits the system group at the join,
	// or the cluster does not come up; and it fits no other group at any time, tainted or not.
	fitsPod := func(selector map[string]any, tolerated map[string]bool, g *group, taints []string) bool {
		for k, v := range selector {
			if g.labels[k] != v {
				return false
			}
		}
		for _, key := range taints {
			if !tolerated[key] {
				return false
			}
		}
		return true
	}
	fits := func(g *group, taints []string) bool { return fitsPod(operatorSelector, operator, g, taints) }
	if len(rendered) != 2 || rendered[SandboxSystemGroup] == nil || rendered[SandboxGroup] == nil {
		t.Fatalf("the rendered cluster's node groups: %v", rendered)
	}
	for name, g := range rendered {
		atJoin, fenced := fits(g, g.taints), fits(g, slices.DeleteFunc(slices.Clone(g.taints), func(k string) bool { return k == UnsecuredTaintKey }))
		switch {
		case name == SandboxSystemGroup && !(atJoin && fenced):
			t.Errorf("Cilium's operator (selector %v, tolerations %v) cannot be scheduled on group %s (labels %v, taints %v): at the join %v, once fenced %v",
				operatorSelector, operator, name, g.labels, g.taints, atJoin, fenced)
		case name != SandboxSystemGroup && (atJoin || fenced):
			t.Errorf("Cilium's operator (selector %v, tolerations %v) can be scheduled on group %s (labels %v, taints %v): at the join %v, once fenced %v. "+
				"Its ClusterRole reads every namespace's Secrets, and that group runs code of tenants", operatorSelector, operator, name, g.labels, g.taints, atJoin, fenced)
		}
	}
	if operatorSelector["kubernetes.io/os"] != "linux" {
		t.Errorf("the operator's selector %v lost Cilium's own kubernetes.io/os: linux", operatorSelector)
	}
	// MetalLB's controller, placed the same way: on no group while a machine is unfenced (it does not tolerate
	// unsecured; nothing of the cluster's start waits for it), and once fenced on the system group alone. First by the
	// values the cluster chart hands MetalLB's chart, then by the Deployment MetalLB's own chart renders from them.
	placeController := func(what string, selector map[string]any, tolerations []any) {
		t.Helper()
		tolerated := map[string]bool{}
		for _, tol := range tolerations {
			tolerated[str(tol.(map[string]any), "key")] = true
		}
		if len(selector) == 0 {
			t.Errorf("%s has no node selector: it may run on a sandbox machine, whose kubelet can ask for its token", what)
		}
		for name, g := range rendered {
			atJoin, fenced := fitsPod(selector, tolerated, g, g.taints), fitsPod(selector, tolerated, g, slices.DeleteFunc(slices.Clone(g.taints), func(k string) bool { return k == UnsecuredTaintKey }))
			if want := name == SandboxSystemGroup; atJoin || fenced != want {
				t.Errorf("%s (selector %v, tolerations %v) on group %s (labels %v, taints %v): fits at the join %v, want false; once fenced %v, want %v. "+
					"It may rewrite its webhook's configuration and so be sent every Secret that is written; group %s runs code of tenants",
					what, selector, tolerated, name, g.labels, g.taints, atJoin, fenced, want, SandboxGroup)
			}
		}
	}
	controllerSelector, _ := dig(metallb, "controller", "nodeSelector").(map[string]any)
	controllerTolerations, _ := dig(metallb, "controller", "tolerations").([]any)
	placeController("MetalLB's controller, by the cluster chart's values", controllerSelector, controllerTolerations)
	t.Run("MetalLB's own chart", func(t *testing.T) {
		// The chart the sandbox cluster installs is the cluster chart's addonCharts.metallb; the archive rendered here
		// is the BOM's copy of that version from the chart mirror, pinned by digest (pinnedChart).
		chartValues, err := os.ReadFile("../../deploy/charts/proxmox-kamaji-cluster/values.yaml")
		if err != nil {
			t.Fatal(err)
		}
		installed := str(yamlObjects(t, chartValues)[0], "addonCharts", "metallb", "version")
		var pin Chart
		for _, c := range b.Components {
			if c.Name == "metallb" {
				pin = c.Chart
			}
		}
		if installed == "" || pin.Version != installed {
			t.Fatalf("the cluster chart installs MetalLB %q and the BOM pins %q: this test has no archive of the version a sandbox cluster gets. "+
				"Move the two together, or give this test that version's archive: the controller's placement rests on that chart reading controller.nodeSelector and controller.tolerations",
				installed, pin.Version)
		}
		f := filepath.Join(t.TempDir(), "metallb.yaml")
		if err := os.WriteFile(f, yamlDoc(t, metallb), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(helm, "template", "metallb", pinnedChart(t, pin), "-n", "metallb-system", "-f", f).Output()
		if err != nil {
			t.Fatalf("helm template of MetalLB %s: %v", pin.Version, err)
		}
		found := false
		for _, o := range yamlObjects(t, out) {
			if o["kind"] != "Deployment" || str(o, "metadata", "labels", "app.kubernetes.io/component") != "controller" {
				continue
			}
			found = true
			selector, _ := dig(o, "spec", "template", "spec", "nodeSelector").(map[string]any)
			tolerations, _ := dig(o, "spec", "template", "spec", "tolerations").([]any)
			if dig(o, "spec", "template", "spec", "affinity") != nil {
				t.Errorf("the controller's Deployment has an affinity this test does not place by: %v", dig(o, "spec", "template", "spec", "affinity"))
			}
			placeController("MetalLB's controller, as its chart renders it", selector, tolerations)
		}
		if !found {
			t.Fatalf("MetalLB %s renders no controller Deployment", pin.Version)
		}
	})
	if len(registered) != 2 {
		t.Fatalf("the kubelets of the rendered cluster register with taints %v, want the two of the sandbox cluster", registered)
	}
	for key := range registered {
		if !operator[key] {
			t.Errorf("a node registers with %s and Cilium's operator tolerates only %v", key, operator)
		}
	}
	// A node that is not Ready yet carries this taint too: the operator's own list must survive the addition.
	if !operator["node.kubernetes.io/not-ready"] {
		t.Errorf("Cilium's operator lost its own tolerations: %v", operator)
	}
}

// The capacity row fails for sandbox machines no run has built on a node without the room, as it does for a control
// plane that grows; for machines a run placed it only warns, as for every other machine of an installed platform.
func TestCapacityFailsForUnbuiltSandboxMachines(t *testing.T) {
	sandboxBOM(t)
	in := validInstallation(t)
	for i := range in.Spec.Clusters[0].NodeGroups {
		in.Spec.Clusters[0].NodeGroups[i].Nodes = []string{"pve1"}
	}
	in.Status.BOM = &BOMRef{Release: "x"}
	in.Status.Clusters = []ClusterStatus{{Name: ClusterName}}
	// 24 GiB in the node: the seed, the control plane with its spare and three groups take 24 GiB, so nothing more fits.
	f := &preflightPVE{extra: map[string]string{"/nodes": `[{"node":"pve1","status":"online","maxmem":25769803776,"mem":1073741824}]`}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	p := Preflight{Installation: in, CAPI: f.client(t, srv.URL, pve.CAPITokenID)}
	ctx := context.Background()
	if c := p.capacity(ctx); c.Status == CheckFail {
		t.Fatalf("no sandboxes, an installed platform: %+v", c)
	}
	in.Spec.Sandbox = sharedSandbox()
	c := p.capacity(ctx)
	if c.Status != CheckFail || !strings.Contains(c.Message, "sandbox machines there that no run has built yet") {
		t.Fatalf("a sandbox machine that does not fit and is not built: %+v", c)
	}
	if got := p.GrowthBlocking(ctx); len(got) != 1 {
		t.Errorf("the plan's blocking: %v", got)
	}
	in.Status.Sandbox = &SandboxStatus{Placement: SandboxShared, Count: 1, Nodes: []string{"pve1"}}
	if c := p.capacity(ctx); c.Status != CheckWarn {
		t.Errorf("built: %+v", c)
	}
	in.Spec.Sandbox.Count = 2
	if c := p.capacity(ctx); c.Status != CheckFail {
		t.Errorf("one more machine than built: %+v", c)
	}
	// The same count on a node the last run placed no sandbox machine on: nothing is built there either.
	in.Spec.Sandbox.Count, in.Status.Sandbox.Nodes = 1, []string{"pve2"}
	if c := p.capacity(ctx); c.Status != CheckFail || !slices.Equal(in.sandboxGrowth(), []string{"pve1"}) {
		t.Errorf("the group moved to another node: %+v, growth %v", c, in.sandboxGrowth())
	}
	// Separate: the system group's node counts the same way.
	in.Spec.Sandbox, in.Status.Sandbox = separateSandbox(), nil
	in.Spec.Sandbox.Nodes, in.Spec.Sandbox.System.Nodes = []string{"pve9"}, []string{"pve1"}
	if got := in.sandboxGrowth(); !slices.Equal(got, []string{"pve1", "pve9"}) {
		t.Errorf("growth %v", got)
	}
	if c := p.capacity(ctx); c.Status != CheckFail {
		t.Errorf("separate, the system machine does not fit: %+v", c)
	}
}

// The Check's row: the portal token opens the sandbox pool with rights the setup scripts gave it. A site that lacks
// them is told to run the root script again; a VM of another's in the pool and a lost ACL row are named.
func TestSandboxCheck(t *testing.T) {
	sandboxBOM(t)
	in := validInstallation(t)
	in.ID = "pi-0a1b2c3d"
	in.Spec.Sandbox = sharedSandbox()
	grant := func(checks ...pve.PrivCheck) map[string][]string {
		out := map[string][]string{}
		for _, c := range checks {
			out[c.Path] = append(out[c.Path], c.Privs...)
		}
		return out
	}
	f := &preflightPVE{perms: map[string]map[string][]string{pve.DefaultPlatformTokenID: grant(pve.SandboxPoolPrivileges()), pve.CAPITokenID: {}}, extra: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	p := Preflight{Installation: in, Portal: &pve.Service{C: f.client(t, srv.URL, pve.DefaultPlatformTokenID), ServiceID: "svc-pve"}, CAPI: f.client(t, srv.URL, pve.CAPITokenID)}
	ctx := context.Background()
	if c := p.sandboxCheck(ctx); c == nil || c.Status != CheckOK {
		t.Fatalf("a site the scripts set up: %+v", c)
	}
	f.perms[pve.DefaultPlatformTokenID]["/pool/"+pve.SandboxPool] = []string{"Pool.Audit", "VM.Audit"}
	if c := p.sandboxCheck(ctx); c.Status != CheckFail || !strings.Contains(c.Message, "the portal token lacks Pool.Allocate") ||
		!strings.Contains(c.Message, "Run the managed-services script again as root on a node") || !strings.Contains(c.Message, "bootStorage=local-lvm") {
		t.Errorf("the platform role is not on the pool: %+v", c)
	}
	f.perms[pve.DefaultPlatformTokenID] = grant(pve.SandboxPoolPrivileges())
	// A VM of another's in the pool; this installation's own machines pass.
	f.extra["/cluster/resources"] = `[{"type":"qemu","vmid":310,"node":"pve1","pool":"proxcloud-sbx","tags":"proxcloud;pc-platform-pi-0a1b2c3d"},
		{"type":"qemu","vmid":311,"node":"pve1","pool":"proxcloud-sbx","tags":"mine"}]`
	if c := p.sandboxCheck(ctx); c.Status != CheckFail || !strings.Contains(c.Message, "VM 311 on pve1 is in pool proxcloud-sbx and is not this installation's") || strings.Contains(c.Message, "VM 310") {
		t.Errorf("a foreign VM in the pool: %+v", c)
	}
	delete(f.extra, "/cluster/resources")
	p.Site.PoolPrefix = "proxcloud-"
	if c := p.sandboxCheck(ctx); c.Status != CheckFail || !strings.Contains(c.Message, "poolPrefix") {
		t.Errorf("a pool prefix that takes the pool for a project's: %+v", c)
	}
	p.Site.PoolPrefix = "pc-"
	// Built, and the row of Cluster API's token is gone: a warning, the next apply writes it.
	in.Status.Sandbox = &SandboxStatus{Placement: SandboxShared, Count: 1}
	if c := p.sandboxCheck(ctx); c.Status != CheckWarn || !strings.Contains(c.Message, "An apply writes it again") {
		t.Errorf("the ACL row is gone: %+v", c)
	}
	f.perms[pve.CAPITokenID] = grant(pve.PrivCheck{Path: "/pool/" + pve.SandboxPool, Privs: pve.ManagedRoles[0].Privs})
	if c := p.sandboxCheck(ctx); c.Status != CheckOK {
		t.Errorf("built with its row: %+v", c)
	}
	// Separate on a platform network that is an SDN vnet (zone plat, vnet plat140): the mint needs the mint role on the
	// platform's own stores and on that vnet's path, which the row reads.
	in.Spec.Sandbox, in.Status.Sandbox = separateSandbox(), nil
	in.Spec.Network.Bridge = "plat140"
	f.extra["/cluster/sdn/vnets"] = `[{"vnet":"plat140","zone":"plat"}]`
	c := p.sandboxCheck(ctx)
	if c.Status != CheckFail || !strings.Contains(c.Message, "Permissions.Modify on /sdn/zones/plat/plat140") || !strings.Contains(c.Message, "on /storage/local-lvm") {
		t.Errorf("separate without the mint rights: %+v", c)
	}
	f.perms[pve.DefaultPlatformTokenID] = grant(append(pve.KubeMintPrivileges([]string{"local-lvm", "local"}, []string{"plat"}), pve.SandboxPoolPrivileges(),
		pve.PrivCheck{Path: "/sdn/zones/plat/plat140", Privs: []string{"Permissions.Modify"}})...)
	if c := p.sandboxCheck(ctx); c.Status != CheckOK {
		t.Errorf("separate with the mint rights: %+v", c)
	}
	in.Spec.Sandbox = nil
	if c := p.sandboxCheck(ctx); c != nil {
		t.Errorf("no sandboxes, a row: %+v", c)
	}
}

// The presets fill a block the spec holds and never add one.
func TestSandboxPresets(t *testing.T) {
	for preset, want := range map[string]SandboxSpec{
		PresetHomelab: {Placement: SandboxShared, SandboxMachines: SandboxMachines{Nodes: []string{"pve1"}, Count: 1, Cores: 2, MemoryMiB: 4096, DiskGiB: 40}},
		PresetStandard: {Placement: SandboxSeparate, SandboxMachines: SandboxMachines{Nodes: []string{"pve1"}, Count: 2, Cores: 4, MemoryMiB: 8192, DiskGiB: 40},
			System: &SandboxMachines{Nodes: []string{"pve1"}, Count: 1, Cores: 2, MemoryMiB: 4096, DiskGiB: 30}},
	} {
		s := Spec{Seed: SeedSpec{Node: "pve1"}}
		if err := ApplyPreset(preset, &s, true); err != nil || s.Sandbox != nil {
			t.Fatalf("%s adds a sandbox block: %+v %v", preset, s.Sandbox, err)
		}
		s = Spec{Seed: SeedSpec{Node: "pve1"}, Sandbox: &SandboxSpec{}}
		if err := ApplyPreset(preset, &s, true); err != nil || !reflect.DeepEqual(*s.Sandbox, want) {
			t.Errorf("%s: %+v, want %+v", preset, *s.Sandbox, want)
		}
		// What the spec names stays, and off stays off.
		s = Spec{Seed: SeedSpec{Node: "pve1"}, Sandbox: &SandboxSpec{Placement: SandboxShared, SandboxMachines: SandboxMachines{Nodes: []string{"pve2", "pve3"}, Cores: 8}}}
		if _ = ApplyPreset(preset, &s, true); s.Sandbox.Placement != SandboxShared || s.Sandbox.Cores != 8 || s.Sandbox.Count < 2 || s.Sandbox.System != nil {
			t.Errorf("%s over a spec's own values: %+v", preset, *s.Sandbox)
		}
		s = Spec{Seed: SeedSpec{Node: "pve1"}, Sandbox: &SandboxSpec{Placement: SandboxOff}}
		if _ = ApplyPreset(preset, &s, true); !reflect.DeepEqual(*s.Sandbox, SandboxSpec{Placement: SandboxOff}) {
			t.Errorf("%s fills a block that is off: %+v", preset, *s.Sandbox)
		}
	}
}

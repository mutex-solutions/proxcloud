package platformlife

// sandbox.go — spec.sandbox: where the sandbox service runs on a managed platform, and what a run builds for it.
//
// shared: a node group `sandbox` of the platform cluster. It is not an entry of spec.clusters[].nodeGroups (an older
// console keeps sending exactly system, kamaji and dbaas): Spec.platformCluster adds it, and the render, the roll, the
// capacity row and the machine counts all read the cluster through that one function, so the group is ordered, gated
// and counted like the three others and comes last in a roll.
//
// separate: a cluster of its own, pc-sandbox, with a Kamaji control plane on the platform cluster's kamaji group and
// two worker groups (sandbox, sandbox-system), built from the tenant cluster chart by an Application of the seed's
// ArgoCD. It is not upgraded in place: a move that changes the Kubernetes version or the node image is refused while
// it exists (move), and its chart version stays the one it was built with (SandboxStatus.ChartVersion).
//
// Both put their machines on the platform network, in a range of their own (nodeRange, one CIDR) and in the Proxmox
// pool pve.SandboxPool. This file holds the rules and the renders; sandbox_run.go what a run does on the site.
//
// The BOM of 0.12.0 is the first that pins the sandbox chart. SandboxProblems refuses the block while the installation's
// BOM has no component marked FeatureSandbox: a platform that an earlier release installed, until its upgrade.

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

const (
	// SandboxGroup is the node group of sandbox machines, in both modes; SandboxSystemGroup the separate cluster's
	// group for sandbox-proxy and the agent-sandbox controller, which must never share a node with a sandbox.
	SandboxGroup       = "sandbox"
	SandboxSystemGroup = "sandbox-system"
	// SandboxClusterName is the separate cluster: its Application on the seed, its namespace on the platform cluster,
	// its CAPI Cluster and the name of its Proxmox token (capmox@pve!pc-sandbox).
	SandboxClusterName = "pc-sandbox"
	// SandboxCredentialsSecret is the Secret CAPMOX clones the sandbox cluster's VMs with, in SandboxClusterName on
	// the platform cluster. It carries ManagedBy, not the tenant labels the provider sync's orphan sweep selects.
	SandboxCredentialsSecret = "pc-sandbox-pve"
	// SandboxTaintKey keeps every pod but a gVisor one off a sandbox node. Not GroupTaintKey: MetalLB's speaker
	// tolerates that key and must stay off the sandbox nodes.
	SandboxTaintKey = "proxcloud.io/sandbox"
	// UnsecuredTaintKey is on every new sandbox machine until the provider sync has written its VM firewall. Nothing
	// here tolerates it; the RuntimeClass of the sandbox chart must not either.
	UnsecuredTaintKey  = "proxcloud.io/unsecured"
	sandboxSystemTaint = "proxcloud.io/sandbox-system"
	// sandboxSystemLabel is on the nodes of the separate cluster's system group and on no other. Cluster API sets it
	// from the Machine; under NodeRestriction no kubelet can give it to its own node, so what must never share a node
	// with code of tenants selects it: the sandbox chart's proxy and controller, Cilium's operator and MetalLB's
	// controller.
	sandboxSystemLabel = "node-restriction.kubernetes.io/proxcloud-system"
	sandboxNamespace   = "sandbox-system"
	sandboxTLSSecret   = "sandbox-proxy-tls"

	TLSSecret     = "secret"
	TLSSelfSigned = "selfsigned"
)

// SandboxMachines are the machines of one sandbox group: Count split across Nodes, as a node group's.
type SandboxMachines struct {
	Nodes     []string `json:"nodes,omitempty"`
	Count     int      `json:"count,omitempty"`
	Cores     int      `json:"cores,omitempty"`
	MemoryMiB int      `json:"memoryMiB,omitempty"`
	DiskGiB   int      `json:"diskGiB,omitempty"`
}

// SandboxSpec is spec.sandbox. With placement off nothing else is read. Fixed once a run built it: the placement
// (it may only go to off), nodeRange and proxyAddress. Nodes, count, sizes, dns and domain change with an apply.
type SandboxSpec struct {
	Placement string `json:"placement"` // shared | separate | off
	// The machines that run sandboxes. By CPU a machine of n cores holds n-1 sandboxes of one vCPU: the node's own
	// pods ask for about one core (measured on the spike of 2026-11-21: a 2-core node held one).
	SandboxMachines
	// NodeRange is the machines' own address range: one aligned IPv4 CIDR inside the platform network, apart from
	// every other range of the installation. A CIDR and not first-last: the provider fences it as a sandbox network
	// (config.network.sandboxCidrs), which takes CIDRs only.
	NodeRange string `json:"nodeRange,omitempty"`
	// DNS are the resolvers of the sandbox machines; empty = the platform network's.
	DNS []string `json:"dns,omitempty"`
	// ProxyAddress is where sandbox-proxy answers. Shared: an address of the platform cluster's MetalLB range.
	// Separate: an address of the platform network outside every range, which the sandbox cluster's MetalLB announces.
	ProxyAddress string `json:"proxyAddress,omitempty"`
	// Domain: a sandbox answers at https://<port>-<id>.<domain>.
	Domain string      `json:"domain,omitempty"`
	TLS    *SandboxTLS `json:"tls,omitempty"`
	// System is the separate cluster's second group (sandbox-proxy, the agent-sandbox controller). Separate only.
	System *SandboxMachines `json:"system,omitempty"`
}

// SandboxTLS says where sandbox-proxy's wildcard certificate comes from: selfsigned (the run makes one; also what an
// absent block means) or secret (the operator's, kept in the installation's sealed secrets).
type SandboxTLS struct {
	Mode string `json:"mode"`
}

// SandboxStatus is what a run built for spec.sandbox, written before anything is built so that a later run and a
// Remove know what to take away. Count and SystemCount are the machine counts the last run handed to Cluster API,
// Nodes and SystemNodes the Proxmox nodes it placed them on: the capacity row fails for the machines the settings ask
// for beyond them or on another node, which nothing has placed yet.
type SandboxStatus struct {
	Placement    string   `json:"placement"`
	NodeRange    string   `json:"nodeRange"`
	ProxyAddress string   `json:"proxyAddress"`
	Count        int      `json:"count"`
	Nodes        []string `json:"nodes,omitempty"`
	SystemCount  int      `json:"systemCount,omitempty"`
	SystemNodes  []string `json:"systemNodes,omitempty"`
	// ChartVersion is the cluster chart the separate cluster was built with: its Application keeps that version on
	// every later apply, whatever tenant chart the installation's BOM names by then.
	ChartVersion string `json:"chartVersion,omitempty"`
	// Certificate is the self-signed certificate of sandbox-proxy (PEM, public), for clients to trust; its key is sealed.
	Certificate string `json:"certificate,omitempty"`
	// BuiltAt: every sandbox machine ran once. Until then an upgrade run lets new machine templates of the group through.
	BuiltAt *time.Time `json:"builtAt,omitempty"`
}

var domainRE = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// sandboxMode is the placement the settings ask a run to build: shared, separate, or "" (no block, or off).
func (s Spec) sandboxMode() string {
	if s.Sandbox == nil || (s.Sandbox.Placement != SandboxShared && s.Sandbox.Placement != SandboxSeparate) {
		return ""
	}
	return s.Sandbox.Placement
}

// sandboxGroups are the machines spec.sandbox places, as node groups: sandbox, and with separate sandbox-system.
func (s Spec) sandboxGroups() []NodeGroup {
	group := func(name string, m SandboxMachines) NodeGroup {
		return NodeGroup{Name: name, Nodes: m.Nodes, Count: m.Count, Cores: m.Cores, MemoryMiB: m.MemoryMiB, DiskGiB: m.DiskGiB}
	}
	switch sb := s.Sandbox; {
	case s.sandboxMode() == SandboxShared:
		return []NodeGroup{group(SandboxGroup, sb.SandboxMachines)}
	case s.sandboxMode() == SandboxSeparate && sb.System != nil:
		return []NodeGroup{group(SandboxGroup, sb.SandboxMachines), group(SandboxSystemGroup, *sb.System)}
	case s.sandboxMode() == SandboxSeparate:
		return []NodeGroup{group(SandboxGroup, sb.SandboxMachines)}
	}
	return nil
}

// platformCluster is the platform cluster as a run builds it: spec.clusters[0], with the sandbox group after its node
// groups when the placement is shared. The render, the roll's units and room, the capacity row and every machine
// count read the cluster through it; nothing else may add a group.
func (s Spec) platformCluster() ClusterSpec {
	c := s.Clusters[0]
	if s.sandboxMode() == SandboxShared {
		c.NodeGroups = append(slices.Clone(c.NodeGroups), s.sandboxGroups()...)
	}
	return c
}

// machineGroups are all node groups whose machines the installation places on Proxmox nodes: the platform cluster's,
// and the separate sandbox cluster's.
func (s Spec) machineGroups() []NodeGroup {
	if len(s.Clusters) == 0 {
		return s.sandboxGroups()
	}
	if s.sandboxMode() == SandboxSeparate {
		return append(slices.Clone(s.Clusters[0].NodeGroups), s.sandboxGroups()...)
	}
	return s.platformCluster().NodeGroups
}

// sandboxRange is spec.sandbox.nodeRange of an active block; the zero Prefix when there is none or it does not parse.
func (s Spec) sandboxRange() netip.Prefix {
	if s.sandboxMode() == "" {
		return netip.Prefix{}
	}
	p, err := netip.ParsePrefix(s.Sandbox.NodeRange)
	if err != nil || p != p.Masked() {
		return netip.Prefix{}
	}
	return p
}

// validateSandbox checks spec.sandbox against the record: the machines as a node group's, nodeRange one aligned CIDR
// inside the platform prefix that holds none of points, overlaps neither range of the cluster and has an address for
// every machine and one surge per node, the proxy address where its mode can announce it, the domain, and the mode
// rules. What needs the site (the tenant pool, blocked and management networks) is sandboxSiteProblems.
func (s Spec) validateSandbox(bad func(string, ...any), prefix netip.Prefix, points map[string]netip.Addr, c ClusterSpec, nodes, lbs addrRange, rangesOK bool) {
	sb := s.Sandbox
	if sb == nil || sb.Placement == SandboxOff {
		return
	}
	if sb.Placement != SandboxShared && sb.Placement != SandboxSeparate {
		bad("spec.sandbox.placement %q is not shared, separate or off", sb.Placement)
		return
	}
	need := 0
	machines := func(f string, m SandboxMachines) {
		if len(m.Nodes) == 0 || len(m.Nodes) > maxGroupNodes {
			bad("%s.nodes: one to %d PVE nodes", f, maxGroupNodes)
		}
		for _, nd := range m.Nodes {
			if !nodeRE.MatchString(nd) {
				bad("%s.nodes %q is not a node name", f, nd)
			}
		}
		if len(slices.Compact(slices.Sorted(slices.Values(m.Nodes)))) != len(m.Nodes) {
			bad("%s.nodes: a node is listed twice", f)
		}
		if m.Count < max(len(m.Nodes), 1) || m.Count > maxGroupCount {
			bad("%s.count %d: at least one machine per chosen node, at most %d", f, m.Count, maxGroupCount)
		}
		if m.Cores < minCores || m.Cores > maxCores || m.MemoryMiB < minMemMiB || m.MemoryMiB > maxMemMiB || m.DiskGiB < minDiskGiB || m.DiskGiB > maxDisk {
			bad("%s: cores %d to %d, memoryMiB %d to %d, diskGiB %d to %d", f, minCores, maxCores, minMemMiB, maxMemMiB, minDiskGiB, maxDisk)
		}
		need += m.Count + len(m.Nodes) // each MachineDeployment surges by one in a rollout
	}
	machines("spec.sandbox", sb.SandboxMachines)
	switch {
	case sb.Placement == SandboxSeparate && sb.System == nil:
		bad("spec.sandbox.system: placement separate needs the group for sandbox-proxy and the sandbox controller, which never share a machine with a sandbox")
	case sb.Placement == SandboxSeparate:
		machines("spec.sandbox.system", *sb.System)
	case sb.System != nil:
		bad("spec.sandbox.system goes with placement separate: with shared, sandbox-proxy runs on the platform cluster's system group")
	}

	r, err := netip.ParsePrefix(sb.NodeRange)
	rangeOK := err == nil && r.Addr().Is4() && r == r.Masked() && r.Bits() > prefix.Bits() && r.Bits() <= 30 && prefix.Contains(r.Addr())
	switch {
	case !rangeOK:
		bad("spec.sandbox.nodeRange %q is not one aligned IPv4 CIDR inside %s, from /%d to /30 (192.0.2.64/28, not first-last: the provider fences the range as a sandbox network, which takes a CIDR)",
			sb.NodeRange, prefix, prefix.Bits()+1)
	case r.Contains(prefix.Addr()) || r.Contains(lastAddr(prefix)):
		bad("spec.sandbox.nodeRange %s holds the network or the broadcast address of %s", r, prefix)
	default:
		for field, a := range points {
			if a.IsValid() && r.Contains(a) {
				bad("%s %s is inside spec.sandbox.nodeRange %s", field, a, r)
			}
		}
		if rangesOK && nodes.overlapsPrefix(r) {
			bad("spec.sandbox.nodeRange %s overlaps the cluster's nodeRange %s: sandbox machines take their addresses from a range no platform machine uses", r, c.NodeRange)
		}
		if rangesOK && lbs.overlapsPrefix(r) {
			bad("spec.sandbox.nodeRange %s overlaps the cluster's metallbRange %s", r, c.MetalLBRange)
		}
		if size := 1 << (32 - r.Bits()); size < need {
			bad("spec.sandbox.nodeRange %s holds %d addresses; the sandbox machines and one rolling surge per node need %d", r, size, need)
		}
	}

	proxy, err := netip.ParseAddr(sb.ProxyAddress)
	switch {
	case err != nil || !proxy.Is4():
		bad("spec.sandbox.proxyAddress %q is not an IPv4 address", sb.ProxyAddress)
	case sb.Placement == SandboxShared:
		if rangesOK && !lbs.contains(proxy) {
			bad("spec.sandbox.proxyAddress %s is outside the cluster's metallbRange %s: with placement shared the platform cluster's MetalLB announces it", proxy, c.MetalLBRange)
		}
		if a := s.Access.ArgoCD; a != nil && a.Address == sb.ProxyAddress {
			bad("spec.sandbox.proxyAddress %s is spec.access.argocd.address: each takes an address of its own", proxy)
		}
	default:
		if !prefix.Contains(proxy) || proxy == prefix.Addr() || proxy == lastAddr(prefix) {
			bad("spec.sandbox.proxyAddress %s is not a host address in %s", proxy, prefix)
		}
		for field, a := range points {
			if a == proxy {
				bad("spec.sandbox.proxyAddress and %s are the same address", field)
			}
		}
		if (rangesOK && (nodes.contains(proxy) || lbs.contains(proxy))) || (rangeOK && r.Contains(proxy)) {
			bad("spec.sandbox.proxyAddress %s is inside a range of the installation: with placement separate the sandbox cluster's own MetalLB announces it, so it is an address no range holds", proxy)
		}
	}
	if !domainRE.MatchString(sb.Domain) || len(sb.Domain) > 200 {
		bad("spec.sandbox.domain %q is not a DNS name: sandboxes answer at <port>-<id>.<domain>", sb.Domain)
	}
	if len(sb.DNS) > 3 {
		bad("spec.sandbox.dns: at most three servers (none takes the platform network's)")
	}
	for _, d := range sb.DNS {
		if a, err := netip.ParseAddr(d); err != nil || !a.Is4() {
			bad("spec.sandbox.dns %q is not an IPv4 address", d)
		}
	}
	if sb.TLS != nil && sb.TLS.Mode != TLSSecret && sb.TLS.Mode != TLSSelfSigned {
		bad("spec.sandbox.tls.mode %q is neither secret nor selfsigned", sb.TLS.Mode)
	}
}

// sandboxSiteProblems checks the sandbox range against the site. The provider's isolation refuses a sandbox CIDR that
// overlaps the tenant pool or a blocked network, and it answers that refusal to every network create of every tenant:
// one bad range would break tenant networks provider-wide. So the range is apart from the tenant pool, the public
// networks and every blocked and management network, and the provider names no other sandbox network (the fence of
// the sandbox VMs takes exactly one).
func (in *Installation) sandboxSiteProblems(site Site) []string {
	own := in.Spec.sandboxRange()
	if !own.IsValid() {
		return nil
	}
	var out []string
	// One provider fences one sandbox pool: the block of the installer's sandbox cluster, or of another installation,
	// is not overwritten.
	if site.SandboxPool != "" && (site.SandboxOwner == "" || site.SandboxOwner != in.ID) {
		whose := "the installer's sandbox cluster (docs/managed-sandbox.md)"
		if site.SandboxOwner != "" {
			whose = "installation " + site.SandboxOwner
		}
		out = append(out, fmt.Sprintf("the provider already fences a sandbox pool, %s (config.sandbox), which belongs to %s: one provider fences one sandbox pool. Remove that sandbox cluster and its config.sandbox first, or leave spec.sandbox out",
			site.SandboxPool, whose))
	}
	for _, set := range []struct {
		what string
		nets []string
	}{{"the tenant pool (cidrPool)", []string{site.CIDRPool}}, {"the public network", site.Public}, {"the blocked network (blockedCidrs)", site.Blocked}, {"the management network", site.Mgmt}} {
		for _, c := range set.nets {
			if p, err := netip.ParsePrefix(strings.TrimSpace(c)); err == nil && p.Overlaps(own) {
				out = append(out, fmt.Sprintf("spec.sandbox.nodeRange %s overlaps %s %s: the provider would refuse it as a sandbox network, and with it every tenant network", own, set.what, p.Masked()))
			}
		}
	}
	for _, c := range site.Sandbox {
		if p, err := netip.ParsePrefix(strings.TrimSpace(c)); err != nil || !slices.Contains(in.ownSandboxRanges(), p.Masked()) {
			out = append(out, fmt.Sprintf("the provider already names the sandbox network %s (config.network.sandboxCidrs): one provider fences one sandbox network. Remove that entry, or the sandbox cluster it belongs to, first",
				strings.TrimSpace(c)))
		}
	}
	return out
}

// SandboxProblems is why the installation's BOM, or this release, cannot build spec.sandbox, or nil. The record alone cannot say it, so
// Validate does not: the save and the Check's spec row both ask here, as for ControlPlaneCountProblems.
func (in *Installation) SandboxProblems() []string {
	if in.Spec.sandboxMode() == "" {
		return nil
	}
	bom := (*BOM)(nil)
	if ref := in.Status.BOM; ref != nil {
		if b, ok := Lookup(ref.SHA256); ok {
			bom = b.BOM
		}
	}
	if bom == nil {
		cur, err := Current()
		if err != nil {
			return nil
		}
		bom = cur.BOM
	}
	if bom.Feature(FeatureSandbox) == nil {
		// A platform that an earlier release installed, under a build that offers sandboxes: the upgrade comes first.
		if cur, err := Current(); err == nil && cur.Feature(FeatureSandbox) != nil {
			return []string{fmt.Sprintf("spec.sandbox: the platform's BOM (release %s) has no sandbox chart. Upgrade the platform to release %s first, then save spec.sandbox; until then leave it out, or set placement off", bom.Release, cur.Release)}
		}
		return []string{fmt.Sprintf("spec.sandbox: the platform's BOM (release %s) has no sandbox chart, so this release builds no sandboxes on the platform: leave spec.sandbox out, or set placement off", bom.Release)}
	}
	chart := ClusterChartVersion(bom)
	if in.Spec.sandboxMode() == SandboxSeparate {
		chart = bom.TenantCharts.Kubernetes.Version
	}
	if !kamaji.ChartHasGroupPool(chart) {
		return []string{fmt.Sprintf("spec.sandbox: the platform's BOM (release %s) names cluster chart %s, which cannot give a node group a pool and an address range of its own: sandboxes need chart %s or newer",
			bom.Release, chart, kamaji.GroupPoolChartVersion)}
	}
	// Refused here and not by the run: the run would build the machines and then wait in vain for a cluster that
	// cannot come up. The sentence names the one order that works. The upgrade is an apply, and stage 1 of every apply
	// fails on this row while separate is stored, so off comes first; and once a run has begun the cluster (a build of
	// 0.12.0-rc.2 that failed), shared is no way on either: the save refuses a change of a begun placement
	// (sandboxFixed), and only off removes what that run made.
	if in.Spec.sandboxMode() == SandboxSeparate && !kamaji.ChartHasOperatorTolerations(chart) {
		first := "Set placement off, or shared, which is built with the platform's own chart"
		if in.Status.Sandbox != nil {
			first = "A run has begun one, and it keeps that chart. Set placement off and apply, which deletes its machines, its token and the pool (shared is refused once a run has begun a cluster)"
		}
		return []string{fmt.Sprintf("spec.sandbox.placement separate: the platform's BOM (release %s) names cluster chart %s, under which a sandbox cluster never comes up (%s). "+
			"%s; then upgrade the platform to a release whose BOM names chart %s or newer; then set placement separate. In that order: the upgrade is an apply, and every apply stops at this row while separate is stored",
			bom.Release, chart, noOperatorTolerations, first, kamaji.OperatorTolerationsChartVersion)}
	}
	// Refused here and not by the run: a run would have built the machines before it met the missing certificate.
	if t := in.Spec.Sandbox.TLS; t != nil && t.Mode == TLSSecret {
		return []string{"spec.sandbox.tls.mode secret: this release has no route that stores an operator's certificate. Set tls.mode selfsigned: the run makes a certificate for *." + in.Spec.Sandbox.Domain}
	}
	return nil
}

// KeepSandbox makes a spec sent for an installed platform keep the stored sandbox block when it names none: a console
// from before the block must not switch sandboxes off by leaving it out. Off is the explicit placement off.
func KeepSandbox(old, next *Spec) {
	if next.Sandbox == nil {
		next.Sandbox = old.Sandbox
	}
}

// sandboxFixed names what next may not change of a sandbox a run built (old.Status.Sandbox): the placement, which
// only goes to off (from shared to separate is off, an apply, then separate: each builds other machines in the same
// pool and range), the node range and the proxy address.
func sandboxFixed(old, next *Installation) []string {
	built, sb := old.Status.Sandbox, next.Spec.Sandbox
	if built == nil || next.Spec.sandboxMode() == "" {
		return nil
	}
	var out []string
	if sb.Placement != built.Placement {
		out = append(out, "spec.sandbox.placement")
	}
	if sb.NodeRange != built.NodeRange {
		out = append(out, "spec.sandbox.nodeRange")
	}
	if sb.ProxyAddress != built.ProxyAddress {
		out = append(out, "spec.sandbox.proxyAddress")
	}
	return out
}

// sandboxGrowth are the Proxmox nodes on which the settings place sandbox machines no run has handed to Cluster API
// yet: all of the block's nodes before the first build, the nodes of a group whose count went up after it, and a node
// the last run placed no machine of the group on.
func (in *Installation) sandboxGrowth() []string {
	if !in.Installed() || in.Spec.sandboxMode() == "" {
		return nil
	}
	built, sb := in.Status.Sandbox, in.Spec.Sandbox
	if built == nil {
		built = &SandboxStatus{}
	}
	unbuilt := func(m SandboxMachines, count int, on []string) []string {
		if m.Count > count {
			return m.Nodes
		}
		return slices.DeleteFunc(slices.Clone(m.Nodes), func(n string) bool { return slices.Contains(on, n) })
	}
	nodes := unbuilt(sb.SandboxMachines, built.Count, built.Nodes)
	if sb.System != nil && in.Spec.sandboxMode() == SandboxSeparate {
		nodes = append(nodes, unbuilt(*sb.System, built.SystemCount, built.SystemNodes)...)
	}
	return slices.Compact(slices.Sorted(slices.Values(nodes)))
}

// sandboxRoom says in a plan row how many sandboxes a machine of cores holds by CPU.
func sandboxRoom(cores int) string {
	if cores <= minCores {
		return fmt.Sprintf(" A machine of %d cores holds ONE sandbox of one vCPU (the node's own pods ask for about one core): take 4 cores or more for several.", cores)
	}
	return fmt.Sprintf(" By CPU each machine holds about %d sandboxes of one vCPU.", cores-1)
}

// sandboxCSIRoll is what a plan row says of the storage plugin when the shared group comes or goes. The node plugin's
// DaemonSet tolerates every taint, so only its own node affinity keeps it off the sandbox machines, and the affinity is
// written with the group alone (render.go, workload/proxmox-csi): an installation without sandboxes renders the
// component as every release before did. Adding or removing the group therefore changes the DaemonSet's pod template,
// and Kubernetes replaces the plugin's pod on every other node, one node at a time (16 seconds for four nodes on the
// lab, both ways). Separate changes nothing of the platform cluster's plugin.
const sandboxCSIRoll = "its DaemonSet changes for that, so Kubernetes replaces the plugin's pod on every other machine of " + ClusterName +
	" once, one machine at a time (some seconds each): a volume that is mounted stays mounted, and a pod that mounts or unmounts one in those seconds waits for the plugin."

// sandboxChanges are the plan rows of spec.sandbox on an installed platform, against what a run built: adding lists
// the machines, the pool and the access a run writes on Proxmox, and what is deployed; off lists what is deleted. A
// placement that differs from the built one is refused (the save refuses it too: sandboxFixed).
func (in *Installation) sandboxChanges() []PlanChange {
	want, built, sb := in.Spec.sandboxMode(), in.Status.Sandbox, in.Spec.Sandbox
	site := func(name, to, impact string) PlanChange {
		return PlanChange{Cluster: ClusterName, Name: name, To: to, Class: ClassSite, Stage: stageCluster, Impact: impact}
	}
	// The row or the token that opens the pool, with what a plan says of it when a run writes it and when a run takes it
	// away.
	access := func(placement string) (row, added, removed string) {
		if placement == SandboxShared {
			return "ACL row: " + pve.CAPITokenID + " holds ProxCloudCAPI on /pool/" + pve.SandboxPool,
				"The portal token writes this one row, so the platform's own Cluster API may clone and delete VMs in the sandbox pool. No other pool and no other token.",
				"The portal token deletes this one row once the sandbox pool holds no VM: the platform's own Cluster API can then no longer clone or delete a VM there. " +
					"No other row of that token and no other token is touched."
		}
		return "token capmox@pve!" + SandboxClusterName + " with ProxCloudCAPI on /pool/" + pve.SandboxPool,
			"The portal token mints it as it mints a tenant cluster's token: the sandbox pool, the template pool (clone only), the boot and ISO stores, /nodes and the " +
				"platform network's bridge or vnet. Its secret is kept sealed and reaches the platform cluster as one Secret.",
			"The portal token deletes the token and every row it holds (the sandbox pool, the template pool, the boot and ISO stores, /nodes, the platform network's " +
				"bridge or vnet) once the sandbox pool holds no VM. Its sealed secret leaves the record, and the Secret that held it on the platform cluster goes with the " +
				"namespace " + SandboxClusterName + ". No other token is touched."
	}
	switch {
	case want == "" && built == nil:
		return nil
	case want == "":
		what := "the node group sandbox of " + ClusterName + " and its machines, the sandbox chart's Application"
		storage := " The proxmox-csi node plugin no longer has to keep off the group: " + sandboxCSIRoll
		if built.Placement == SandboxSeparate {
			what = "the cluster " + SandboxClusterName + " with its machines and its control plane on the kamaji group"
			storage = ""
		}
		row, _, impact := access(built.Placement)
		return []PlanChange{
			{Cluster: ClusterName, Name: "sandbox", From: built.Placement, To: SandboxOff, Class: ClassMachines, Stage: stageCluster,
				Impact: "Sandboxes are switched off: the run deletes " + what + ". Every running sandbox ends. Disable the sandbox provider and let its sandboxes end before the apply." + storage},
			site("sandbox-access", "deleted: "+row, impact),
			site("sandbox-pool", "deleted: pool "+pve.SandboxPool, "Deleted once it holds no VM."),
		}
	case built != nil && built.Placement != want:
		return []PlanChange{{Cluster: ClusterName, Name: "sandbox", From: built.Placement, To: want, Class: ClassRefused,
			Blocking: later + "a run built placement " + built.Placement + ", and it does not become " + want + " in one apply: set placement off, apply, then set " + want}}
	case built != nil && built.BuiltAt != nil && len(in.sandboxGrowth()) == 0:
		return nil
	}
	var machines []string
	for _, g := range in.Spec.sandboxGroups() {
		machines = append(machines, fmt.Sprintf("%s: %d x %d cores, %s, %d GiB disk on %s", g.Name, g.Count, g.Cores, gib(g.MemoryMiB), g.DiskGiB, strings.Join(g.Nodes, ", ")))
	}
	impact := "A node group of " + ClusterName + " in pool " + pve.SandboxPool + ", with addresses from " + sb.NodeRange + ". Only gVisor sandboxes run on it; the sandbox chart " +
		"(Application " + ClusterName + "-sandbox) installs runsc there and puts sandbox-proxy on the system group at " + sb.ProxyAddress + ". The proxmox-csi node plugin keeps off these machines: " + sandboxCSIRoll + " " +
		"Code that escapes a sandbox is root on a machine of the cluster that runs every tenant's control plane and database: docs/managed-services.md says what that means."
	if want == SandboxSeparate {
		impact = "The cluster " + SandboxClusterName + ", a cluster of its own: its control plane runs as pods on the kamaji group (about 0.6 GiB), its machines are in pool " + pve.SandboxPool +
			" with addresses from " + sb.NodeRange + ", and its own MetalLB announces sandbox-proxy at " + sb.ProxyAddress + ". It is not upgraded in place: an upgrade that changes the Kubernetes " +
			"version or the node image is refused while it exists (placement off, the upgrade, then separate again builds it new)."
	}
	impact += sandboxRoom(sb.Cores) + " A new machine takes sandboxes only once the provider sync has written its VM firewall (about 20 minutes)."
	from := ""
	if built != nil {
		from = built.Placement
	}
	row, why, _ := access(want)
	return []PlanChange{
		{Cluster: ClusterName, Name: "sandbox", From: from, To: want + " (" + strings.Join(machines, "; ") + ")", Class: ClassMachines, Stage: stageCluster, Impact: impact},
		site("sandbox-pool", "pool "+pve.SandboxPool, "The portal token creates the pool. The portal token holds its platform role there (through /pool), which is what lets the provider sync write the VM firewall of a sandbox machine; on "+pve.SystemPool+" it holds NoAccess."),
		site("sandbox-access", row, why),
	}
}

// RenderSandbox is spec.sandbox as the renderers read it (nil on RenderInput: no sandboxes).
type RenderSandbox struct {
	Mode           string
	NodeRange      string   // the machines' CIDR
	PlatformPrefix string   // the platform network's CIDR, denied to every sandbox
	DNS            []string // of the sandbox machines
	ProxyAddress   string
	Domain         string
	// Groups are the separate cluster's node groups (sandbox, sandbox-system); with shared the group is in
	// RenderCluster.NodeGroups. ChartVersion is the cluster chart the separate cluster was built with ("" = the BOM's).
	Groups       []RenderNodeGroup
	ChartVersion string
}

func anyList(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func taint(key string) map[string]any {
	return map[string]any{"key": key, "value": "true", "effect": "NoSchedule"}
}

// sandboxGroupValues are the keys ClusterValues adds to the shared sandbox group: the pool, the address range and the
// resolvers of its own (cluster chart 0.1.18), the kubelet label the sandbox chart's node-side pods select, and the
// two taints in place of the group taint.
func sandboxGroupValues(b *BOM, in RenderInput, ng map[string]any) error {
	if v := ClusterChartVersion(b); !kamaji.ChartHasGroupPool(v) {
		return fmt.Errorf("platform: the BOM of release %s names cluster chart %s, which cannot give a node group a pool and an address range: the sandbox group needs chart %s or newer", b.Release, v, kamaji.GroupPoolChartVersion)
	}
	ng["pool"], ng["addresses"], ng["dnsServers"] = pve.SandboxPool, []any{in.Sandbox.NodeRange}, anyList(in.Sandbox.DNS)
	ng["nodeLabels"] = map[string]any{SandboxTaintKey: "cpu"}
	ng["taints"] = []any{taint(SandboxTaintKey), taint(UnsecuredTaintKey)}
	ng["machineTags"] = append(machineTags(in.ID), SandboxClusterName)
	return nil
}

// sandboxChartValues are the sandbox chart's values the run adds in both modes: where sandbox-proxy answers and what
// every sandbox is denied on top of the private ranges (a platform network need not be RFC 1918).
func sandboxChartValues(in RenderInput) map[string]any {
	return map[string]any{
		"proxy":     map[string]any{"address": in.Sandbox.ProxyAddress, "domain": in.Sandbox.Domain},
		"guardrail": map[string]any{"extraDeny": []any{in.Sandbox.PlatformPrefix}},
		"prepull":   sandboxPrepullRefs(),
	}
}

// sandboxPrepullRefs are the images of sandboxPrepull by digest, for the chart's prepull DaemonSet. A release whose
// images.yaml lacks one pulls nothing ahead (TestSandboxTemplatesArePinned keeps that from shipping).
func sandboxPrepullRefs() []any {
	out := []any{}
	for _, name := range sandboxPrepull {
		if ref, err := sandboxPin(name); err == nil {
			out = append(out, ref)
		}
	}
	return out
}

// SandboxClusterValues are the proxmox-kamaji-cluster values of the separate sandbox cluster, after the installer's
// sandbox-cluster.yml.j2: a Kamaji control plane of one replica, the workers in pve.SandboxPool on the platform
// network with addresses from the sandbox range, no CSI, MetalLB with the proxy address alone, Cilium with the network
// guard's chain on every node, and the sandbox chart in mode separate (addons.sandbox, chart 0.1.18).
func SandboxClusterValues(b *BOM, in RenderInput) (map[string]any, error) {
	sb := in.Sandbox
	if sb == nil || sb.Mode != SandboxSeparate {
		return nil, fmt.Errorf("platform: no separate sandbox cluster in the settings")
	}
	chart := b.Feature(FeatureSandbox)
	if chart == nil {
		return nil, fmt.Errorf("platform: the BOM of release %s has no sandbox chart", b.Release)
	}
	if v := sb.chartVersion(b); !kamaji.ChartHasGroupPool(v) {
		return nil, fmt.Errorf("platform: cluster chart %s cannot install the sandbox chart: the sandbox cluster needs chart %s or newer", v, kamaji.GroupPoolChartVersion)
	} else if !kamaji.ChartHasOperatorTolerations(v) {
		// Also the chart a cluster keeps from the build that began it (SandboxStatus.ChartVersion).
		return nil, fmt.Errorf("platform: under cluster chart %s the sandbox cluster never comes up (%s): it needs chart %s or newer. A cluster that a run began under an older chart keeps that chart: set placement off, apply, then set separate again",
			v, noOperatorTolerations, kamaji.OperatorTolerationsChartVersion)
	}
	groups := []any{}
	for _, g := range sb.Groups {
		pins := make([]string, 0, len(g.Nodes))
		for _, n := range g.Nodes {
			vmid, ok := in.Templates[n]
			if !ok {
				return nil, fmt.Errorf("platform: no node image template on %s", n)
			}
			pins = append(pins, fmt.Sprintf("%s=%d", n, vmid))
		}
		ng := map[string]any{
			"name": g.Name, "machineImageId": strings.Join(pins, ","), "machineCount": g.Count,
			"machineCores": g.Cores, "machineMemoryMiB": g.MemoryMiB, "machineRootVolume": map[string]any{"diskSize": g.DiskGiB},
			"machineTags": append(machineTags(in.ID), SandboxClusterName),
		}
		if g.Name == SandboxSystemGroup {
			ng["nodeLabels"] = map[string]any{SandboxTaintKey: "system"}
			ng["machineLabels"] = map[string]any{sandboxSystemLabel: "true"}
			ng["taints"] = []any{taint(UnsecuredTaintKey), taint(sandboxSystemTaint)}
		} else {
			ng["nodeLabels"] = map[string]any{SandboxTaintKey: "cpu"}
			ng["machineLabels"] = map[string]any{"node-restriction.kubernetes.io/proxcloud-sandbox-worker": "true"}
			ng["taints"] = []any{taint(UnsecuredTaintKey)}
		}
		groups = append(groups, ng)
	}
	network := map[string]any{
		"bridge": in.Network.Bridge, "gateway": in.Network.Gateway, "prefix": in.Network.Prefix,
		"addresses": []any{sb.NodeRange}, "dnsServers": anyList(sb.DNS), "mtu": in.Network.MTU,
	}
	if in.Network.VLAN > 0 {
		network["vlan"] = in.Network.VLAN
	}
	return map[string]any{
		"kubernetesVersion": b.Kubernetes.Version,
		"sandboxBandwidth":  true,
		// Every node of this cluster joins with a taint, and its control plane is hosted, so there is no other node.
		// Cilium's operator must start on them all the same: it registers Cilium's CRDs, the agents wait for those, and
		// the sandbox chart, whose identities the run waits for, is refused without them. Nothing else gets a
		// toleration of unsecured: CoreDNS, MetalLB and konnectivity's agent start once the provider sync has fenced a
		// machine.
		"ciliumOperatorTolerations": sandboxJoinTolerations(),
		// On the system group alone, before the fence and after it: the operator's ClusterRole reads the Secrets of
		// every namespace, and the kubelet of a sandbox machine could ask for the token of an operator on its node.
		// The tolerations name unsecured, the one taint a sandbox machine carries, so without this selector the
		// operator could be scheduled there.
		"ciliumOperatorNodeSelector": map[string]any{sandboxSystemLabel: "true"},
		"cloudCredentialsSecretName": SandboxCredentialsSecret,
		"kamajiControlPlane":         map[string]any{"replicas": 1, "dataStoreName": defaultDatastore, "network": map[string]any{"serviceType": "LoadBalancer"}},
		"clusterNetwork":             map[string]any{"pods": "10.244.0.0/16", "services": "10.96.0.0/12"},
		"proxmox": map[string]any{
			"region": in.Region, "pool": pve.SandboxPool, "storage": in.BootStorage, "network": network,
			"csi": map[string]any{"configSecretName": "", "storageClasses": []any{}},
			// MetalLB's controller on the system group alone, once that group is fenced (it does not tolerate unsecured,
			// and the run does not wait for it). It may rewrite its own ValidatingWebhookConfiguration, whose Service
			// points at its pod: with its token, a sandbox machine could have the API server send it every Secret
			// that is written, the wildcard key at each apply among them.
			"metallb": map[string]any{"addresses": []any{sb.ProxyAddress + "-" + sb.ProxyAddress},
				"speakerTolerations":     []any{map[string]any{"key": sandboxSystemTaint, "operator": "Exists", "effect": "NoSchedule"}},
				"controllerNodeSelector": map[string]any{sandboxSystemLabel: "true"},
				"controllerTolerations":  []any{map[string]any{"key": sandboxSystemTaint, "operator": "Exists", "effect": "NoSchedule"}}},
		},
		"nodeGroups":  groups,
		"addonCharts": map[string]any{"sandbox": map[string]any{"repoURL": "oci://" + chart.Chart.Repo, "chart": chart.Chart.Name, "version": chart.Chart.Version}},
		"addons": map[string]any{
			"metricsServer": map[string]any{"enabled": false}, "certManager": map[string]any{"enabled": false}, "ingress": map[string]any{"enabled": false},
			"sandbox": map[string]any{"enabled": true, "values": sandboxChartValues(in)},
		},
	}, nil
}

// noOperatorTolerations is why a cluster chart before kamaji.OperatorTolerationsChartVersion builds no sandbox
// cluster, for the two places that refuse it.
const noOperatorTolerations = "every node of it joins with a taint, Cilium's operator does not tolerate one, and no node becomes Ready"

// sandboxJoinTolerations tolerate the taints a machine of the separate sandbox cluster joins with: one per key
// SandboxClusterValues gives a node group (TestSandboxClusterComesUpOnTaintedNodes holds the two together).
func sandboxJoinTolerations() []any {
	out := []any{}
	for _, key := range []string{UnsecuredTaintKey, sandboxSystemTaint} {
		out = append(out, map[string]any{"key": key, "operator": "Exists", "effect": "NoSchedule"})
	}
	return out
}

func (sb *RenderSandbox) chartVersion(b *BOM) string {
	if sb.ChartVersion != "" {
		return sb.ChartVersion
	}
	return b.TenantCharts.Kubernetes.Version
}

// SandboxApplication is the seed ArgoCD's Application of the separate sandbox cluster: the tenant cluster chart with
// SandboxClusterValues, synced to namespace pc-sandbox of the platform cluster, whose pods (the Kamaji control plane)
// go to the kamaji group. Unlike the platform's other Applications it carries the resources finalizer: deleting it
// (placement off, Remove) deletes the cluster, and Cluster API its machines.
func SandboxApplication(b *BOM, in RenderInput) (map[string]any, error) {
	values, err := SandboxClusterValues(b, in)
	if err != nil {
		return nil, err
	}
	annotations := map[string]any{}
	for k, v := range groupPlacement("kamaji").Annotations() {
		annotations[k] = v
	}
	tc := b.TenantCharts.Kubernetes
	app := object("argoproj.io/v1alpha1", "Application", argoNamespace, SandboxClusterName)
	meta := app["metadata"].(map[string]any)
	meta["labels"], meta["finalizers"] = labels(b, in, SandboxClusterName), []any{"resources-finalizer.argocd.argoproj.io"}
	app["spec"] = map[string]any{
		"project":     argoProject,
		"source":      map[string]any{"repoURL": tc.Repo, "chart": tc.Name, "targetRevision": in.Sandbox.chartVersion(b), "helm": map[string]any{"releaseName": SandboxClusterName, "valuesObject": values}},
		"destination": map[string]any{"server": WorkloadServer(in.Cluster.Endpoint), "namespace": SandboxClusterName},
		"syncPolicy": map[string]any{
			"automated":                map[string]any{"prune": true, "selfHeal": true},
			"syncOptions":              []any{"CreateNamespace=true", "ServerSideApply=true"},
			"managedNamespaceMetadata": map[string]any{"annotations": annotations},
			"retry":                    map[string]any{"limit": 10, "backoff": map[string]any{"duration": "30s", "factor": 2, "maxDuration": "5m"}},
		},
	}
	return app, nil
}

// SandboxCredentials is the Secret CAPMOX clones the sandbox cluster's machines with, in the tenant chart's shape.
func SandboxCredentials(b *BOM, in RenderInput, token PVEToken) map[string]any {
	s := object("v1", "Secret", SandboxClusterName, SandboxCredentialsSecret)
	s["metadata"].(map[string]any)["labels"] = labels(b, in, SandboxClusterName)
	s["type"] = "Opaque"
	s["stringData"] = map[string]any{"url": in.PVEAPIURL, "token": token.ID, "secret": token.Secret}
	return s
}

// sandboxCheck is the Check's row sandbox, present while the settings ask for sandboxes: whether the portal token can
// open the sandbox pool, and whether the pool is free for this installation.
//
// The portal token makes the pool and writes the access of the sandbox machines itself, with rights the two setup
// scripts gave it for tenant pools: its platform role on /pool (which holds Pool.Allocate and every privilege of
// ProxCloudCAPI, what Proxmox asks for an ACL row below /pool) and, for the separate cluster's token, the mint role on
// the platform's stores, /nodes and the platform network's zone. No script names the sandbox pool. A site whose rows
// were changed since, or which holds a row of its own on one of these paths, fails here with the command that writes
// the scripts' rows again; an apply would stop at sandbox_access_failed.
func (p Preflight) sandboxCheck(ctx context.Context) *Check {
	in := p.Installation
	mode, s := in.Spec.sandboxMode(), in.Spec
	if mode == "" || p.Portal == nil {
		return nil
	}
	c := &Check{ID: "sandbox", Status: CheckFail}
	checks := []pve.PrivCheck{pve.SandboxPoolPrivileges()}
	if mode == SandboxSeparate {
		var zones []string
		zone, vnet := "", ""
		if s.Network.Routed() {
			zone, vnet = RoutedZone, s.Network.Bridge
		} else if vnets, err := p.Portal.C.ListSDNVnets(ctx); err == nil {
			for _, v := range vnets {
				if v.Vnet == s.Network.Bridge {
					zone, vnet = v.Zone, v.Vnet
				}
			}
		}
		if zone != "" {
			zones = []string{zone}
		}
		checks = append(checks, pve.KubeMintPrivileges([]string{s.Storage.Boot, s.Storage.ISO}, zones)...)
		if zone != "" {
			checks = append(checks, pve.PrivCheck{Path: "/sdn/zones/" + zone + "/" + vnet, Privs: []string{"Permissions.Modify"}})
		}
	}
	missing, err := p.Portal.C.MissingPrivileges(ctx, checks)
	if err != nil {
		c.Message = "Could not read the permissions of the portal token: " + err.Error()
		return c
	}
	var problems []string
	for _, m := range missing {
		problems = append(problems, "the portal token lacks "+m)
	}
	if len(problems) > 0 {
		problems = append(problems, "The portal token creates pool "+pve.SandboxPool+" and writes the access of the sandbox machines with these rights, which the setup scripts grant for tenant pools "+
			"(no script names the sandbox pool). Run the managed-services script again as root on a node (GET /api/v1/admin/setup/proxmox-script?managedServices=true&bootStorage="+s.Storage.Boot+
			", or step 2 of the Managed services page), then run the Check again. A line that stays is a row of the portal's own on that path: docs/proxmox-permissions.md, \"Sandboxes on the managed platform\".")
	}
	if strings.HasPrefix(pve.SandboxPool, p.Site.PoolPrefix) && p.Site.PoolPrefix != "" {
		problems = append(problems, fmt.Sprintf("the provider's poolPrefix %q is the start of the sandbox pool's name %s: the provider sync would take the pool for a project's", p.Site.PoolPrefix, pve.SandboxPool))
	}
	// Proxmox enforces a VM firewall only while the datacenter firewall is on. With it off the sync still writes every
	// sandbox machine's rules, none of them holds, and no machine is ever opened to sandboxes (pve.ErrSandboxUnenforced).
	if on, err := p.Portal.C.DatacenterFirewallEnabled(ctx); err != nil {
		problems = append(problems, "Could not read the datacenter firewall's options: "+err.Error())
	} else if !on {
		problems = append(problems, "The datacenter firewall is off (Datacenter, Firewall, Options, Firewall: No), so Proxmox enforces no VM firewall: the fence of a sandbox machine would hold nothing, "+
			"its unsecured taint would never come off and the fence gate would never pass. Tenant security groups and the isolation rules need it on as well. Before switching it on, read what else it puts in force: the nodes' own input policy.")
	}
	problems = append(problems, p.proxyAddressTaken(ctx)...)
	rows, err := p.Portal.C.ClusterResources(ctx, "vm")
	if err != nil {
		problems = append(problems, "Could not read the VMs of pool "+pve.SandboxPool+": "+err.Error())
	}
	for _, r := range rows {
		if r.Pool == pve.SandboxPool && !slices.Contains(strings.FieldsFunc(r.Tags, func(c rune) bool { return c == ';' || c == ',' || c == ' ' }), "pc-platform-"+in.ID) {
			problems = append(problems, fmt.Sprintf("VM %d on %s is in pool %s and is not this installation's (no tag pc-platform-%s): the pool is for the sandbox machines of one installation; move the VM to another pool",
				r.VMID, r.Node, pve.SandboxPool, in.ID))
		}
	}
	// An installed shared group: the one row of Cluster API's token, which every apply writes again.
	if st := in.Status.Sandbox; len(problems) == 0 && mode == SandboxShared && st != nil && p.CAPI != nil {
		if lacks, err := p.CAPI.MissingPrivileges(ctx, []pve.PrivCheck{{Path: "/pool/" + pve.SandboxPool, Privs: pve.ManagedRoles[0].Privs}}); err == nil && len(lacks) > 0 {
			return &Check{ID: "sandbox", Status: CheckWarn, Message: pve.CAPITokenID + " lacks " + strings.Join(lacks, "; ") +
				": the ACL row a run wrote on the sandbox pool is gone, so Cluster API cannot replace or repair a sandbox machine. An apply writes it again."}
		}
	}
	if len(problems) > 0 {
		c.Message = strings.Join(problems, "\n")
		return c
	}
	return &Check{ID: "sandbox", Status: CheckOK, Message: "The portal token can make pool " + pve.SandboxPool + " and open it for the sandbox machines, the pool holds no VM of another's, " +
		"the datacenter firewall is on, and no Service of the platform cluster holds the proxy address."}
}

// proxyAddressTaken names a LoadBalancer Service of the platform cluster that holds spec.sandbox.proxyAddress and is
// not sandbox-proxy: MetalLB gives an address once, so sandbox-proxy would stay without one. Read only on a platform
// that runs (the Workload client); before the install nothing can hold an address of its MetalLB range. With placement
// separate the address is outside that range and this read finds nothing: what else on the platform network uses the
// address (a router, a guest without an agent) no check can read.
func (p Preflight) proxyAddressTaken(ctx context.Context) []string {
	addr := p.Installation.Spec.Sandbox.ProxyAddress
	if p.Workload == nil || addr == "" {
		return nil
	}
	svcs, err := p.Workload.ListServicesAllNamespaces(ctx)
	if err != nil {
		return nil // rows nodes and certificates say that the cluster does not answer
	}
	var out []string
	for _, s := range svcs {
		ns, name := str(s, "metadata", "namespace"), str(s, "metadata", "name")
		if ns == sandboxNamespace && name == "sandbox-proxy" {
			continue
		}
		holds := str(s, "spec", "loadBalancerIP") == addr || slices.Contains(strings.Split(str(s, "metadata", "annotations", "metallb.io/loadBalancerIPs"), ","), addr)
		ingress, _ := dig(s, "status", "loadBalancer", "ingress").([]any)
		for _, i := range ingress {
			if m, _ := i.(map[string]any); m["ip"] == addr {
				holds = true
			}
		}
		if holds {
			out = append(out, fmt.Sprintf("spec.sandbox.proxyAddress %s is taken: Service %s/%s of the platform cluster holds it, so sandbox-proxy would get no address. Choose a free address of the MetalLB range", addr, ns, name))
		}
	}
	return out
}

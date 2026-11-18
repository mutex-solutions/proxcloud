package platformlife

// installation.go — the PlatformInstallation record (design §3.2): what the operator asked for (spec) and what a run
// built (status). The rules on a spec are pure functions here, so the API, the preflight and the job share them.

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// Installation is one document of collection platformInstallation: one per Proxmox provider.
type Installation struct {
	ID            string    `json:"id"`   // pi-<8hex>
	Name          string    `json:"name"` // prefixes the providers: "<name> Kubernetes", "<name> Databases"
	ServiceID     string    `json:"serviceId"`
	ProvisionedBy string    `json:"provisionedBy"` // portal | installer (M2: refused by Validate)
	Layout        string    `json:"layout"`        // shared (separate is refused in M1)
	Spec          Spec      `json:"spec"`
	Status        Status    `json:"status"`
	LastRunID     string    `json:"lastRunId,omitempty"`
	CreatedBy     string    `json:"createdBy"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
	Rev           int64     `json:"rev"` // optimistic concurrency: a PUT names the rev it read
}

// Spec is what the operator asks for. Fields marked fixed cannot change once a run succeeded (FixedChanges).
type Spec struct {
	PVEAPIURL string        `json:"pveApiUrl"` // the PVE API as the platform network reaches it
	Storage   Storage       `json:"storage"`
	Network   Network       `json:"network"` // fixed
	Seed      SeedSpec      `json:"seed"`
	Clusters  []ClusterSpec `json:"clusters"`
	NodeImage NodeImage     `json:"nodeImage"`
}

// Storage: boot holds every platform disk and is the std StorageClass; iso is the node-local store of bootstrap ISOs;
// import is where Template updates downloads the two images.
type Storage struct {
	Boot   string `json:"boot"`
	ISO    string `json:"iso"`
	Import string `json:"import"`
}

// Network is the platform network, untagged on one bridge (no VLAN in M1, as in the installer).
type Network struct {
	Bridge  string   `json:"bridge"`
	Prefix  string   `json:"prefix"` // IPv4 CIDR
	Gateway string   `json:"gateway"`
	DNS     []string `json:"dns"`
	MTU     int      `json:"mtu,omitempty"`
}

// SeedSpec is the k3s management cluster. Mode, pool, node and address are fixed.
type SeedSpec struct {
	Mode       string   `json:"mode"` // vm (the job clones it) | existing (installer seeds, M2: refused by Validate)
	Pool       string   `json:"pool"`
	Node       string   `json:"node"`
	Address    string   `json:"address"`
	Cores      int      `json:"cores"`
	MemoryMiB  int      `json:"memoryMiB"`
	DiskGiB    int      `json:"diskGiB"`
	SSHKeys    []string `json:"sshKeys,omitempty"` // break-glass only; the portal never uses SSH
	APISources []string `json:"apiSources"`        // CIDRs that may reach the seed on 6443
}

// ClusterSpec is one workload cluster; its VMs go to pool proxcloud-system. Name, endpoint and the set of node groups
// are fixed.
type ClusterSpec struct {
	Name         string      `json:"name"`
	Roles        []string    `json:"roles"`
	Endpoint     string      `json:"endpoint"`     // the API VIP
	NodeRange    string      `json:"nodeRange"`    // first-last, the CAPI IPAM pool
	MetalLBRange string      `json:"metallbRange"` // first-last, tenant-facing load balancers
	ControlPlane Machine     `json:"controlPlane"`
	NodeGroups   []NodeGroup `json:"nodeGroups"`
}

// Machine is one VM's placement and size.
type Machine struct {
	Node      string `json:"node"`
	Cores     int    `json:"cores"`
	MemoryMiB int    `json:"memoryMiB"`
	DiskGiB   int    `json:"diskGiB"`
}

// NodeGroup is one MachineDeployment per PVE node; Count is split across Nodes as the chart splits machineCount.
type NodeGroup struct {
	Name      string   `json:"name"`
	Nodes     []string `json:"nodes"`
	Count     int      `json:"count"`
	Cores     int      `json:"cores"`
	MemoryMiB int      `json:"memoryMiB"`
	DiskGiB   int      `json:"diskGiB"`
}

// NodeImage: bom imports the BOM's node image; templates names a template per node (installer sites).
type NodeImage struct {
	Mode      string         `json:"mode"`
	Templates map[string]int `json:"templates,omitempty"` // node → vmid
}

// Status is what runs recorded. Sync and health are read live, never stored.
type Status struct {
	Phase      string            `json:"phase"`
	BOM        *BOMRef           `json:"bom,omitempty"` // the BOM of the last successful apply; nil = never succeeded
	Seed       *SeedStatus       `json:"seed,omitempty"`
	Clusters   []ClusterStatus   `json:"clusters,omitempty"`
	Components []ComponentStatus `json:"components,omitempty"`
	Volumes    []Volume          `json:"volumes,omitempty"`
	Providers  *ProviderIDs      `json:"providers,omitempty"`
}

// BOMRef names an embedded BOM by release and file sha256.
type BOMRef struct {
	Release string `json:"release"`
	SHA256  string `json:"sha256"`
}

// SeedStatus is the seed as the job built it. CACert is the certificate only; its key is never stored.
type SeedStatus struct {
	VMID        int        `json:"vmid,omitempty"`
	Node        string     `json:"node,omitempty"`
	Generation  int        `json:"generation,omitempty"`
	APIServer   string     `json:"apiServer,omitempty"`
	CACert      string     `json:"caCert,omitempty"`
	CDVolume    string     `json:"cdVolume,omitempty"`    // "" once deleted
	TokenSecret string     `json:"tokenSecret,omitempty"` // the name of the live SA token Secret, not its value
	K3sVersion  string     `json:"k3sVersion,omitempty"`
	ReadyAt     *time.Time `json:"readyAt,omitempty"`
}

// ClusterStatus is one workload cluster as the job built it. KubeconfigSecret is a Secret name on the seed.
type ClusterStatus struct {
	Name                 string         `json:"name"`
	Namespace            string         `json:"namespace,omitempty"`
	Endpoint             string         `json:"endpoint,omitempty"`
	KubernetesVersion    string         `json:"kubernetesVersion,omitempty"`
	KubeconfigSecret     string         `json:"kubeconfigSecret,omitempty"`
	ManagedBy            string         `json:"managedBy,omitempty"` // application | raw
	Templates            map[string]int `json:"templates,omitempty"`
	ArgoCDTokenExpiresAt *time.Time     `json:"argocdTokenExpiresAt,omitempty"`
}

// ComponentStatus is one component as last applied.
type ComponentStatus struct {
	Name      string     `json:"name"`
	Cluster   string     `json:"cluster"`
	Version   string     `json:"version"`
	Digest    string     `json:"digest,omitempty"`
	AppliedAt *time.Time `json:"appliedAt,omitempty"`
	Managed   bool       `json:"managed"`
}

// Volume is a proxmox-csi volume of the platform's own PVCs, deleted by its exact handle at Remove.
type Volume struct {
	Handle    string `json:"handle"`
	Component string `json:"component"`
	PVC       string `json:"pvc"`
}

// ProviderIDs are the service ids stage 10 registered.
type ProviderIDs struct {
	Kubernetes string `json:"kubernetes,omitempty"`
	Database   string `json:"database,omitempty"`
}

// Phases, modes and the fixed names of M1.
const (
	PhaseDraft    = "draft"
	PhaseApplying = "applying"
	PhaseReady    = "ready"
	PhaseFailed   = "failed"
	PhaseRemoving = "removing"
	PhaseRemoved  = "removed"

	ByPortal    = "portal"
	ByInstaller = "installer"

	LayoutShared   = "shared"
	LayoutSeparate = "separate"

	SeedVM       = "vm"
	SeedExisting = "existing"

	ImageBOM       = "bom"
	ImageTemplates = "templates"

	RoleKamaji = "kamaji"
	RoleDBaaS  = "dbaas"

	DefaultClusterName = "pc-platform"
)

// NodeGroupNames is the fixed set of node groups in M1 (no sandbox group until its shape is decided).
var NodeGroupNames = []string{"system", "kamaji", "dbaas"}

// Bounds of a machine: the lab profile (2 cores, 4 GiB) is the smallest; the node image is a 20 GiB disk and a clone
// only grows.
const (
	minCores, maxCores   = 2, 64
	minMemMiB, maxMemMiB = 4096, 256 << 10
	minDiskGiB, maxDisk  = 20, 2048
	maxRangeSize         = 4096 // addresses in a node or MetalLB range
	maxSSHKeys           = 8
	maxAPISources        = 16
	maxGroupNodes        = 16
	maxGroupCount        = 64
)

var (
	nameRE    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$`)
	nodeRE    = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9]{0,61}[A-Za-z0-9])?$`)
	storageRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,63}$`)
	bridgeRE  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,14}$`)
)

// Normalize fills the defaults of a new or updated installation: shared layout, a portal-built seed in pool
// proxcloud-seed, one cluster pc-platform that runs both services, and the BOM's node image.
func (in *Installation) Normalize() {
	if in.ProvisionedBy == "" {
		in.ProvisionedBy = ByPortal
	}
	if in.Layout == "" {
		in.Layout = LayoutShared
	}
	s := &in.Spec
	if s.Seed.Mode == "" {
		s.Seed.Mode = SeedVM
	}
	if s.Seed.Pool == "" && s.Seed.Mode == SeedVM {
		s.Seed.Pool = pve.SeedPool
	}
	if s.NodeImage.Mode == "" {
		s.NodeImage.Mode = ImageBOM
	}
	for i := range s.Clusters {
		if s.Clusters[i].Name == "" {
			s.Clusters[i].Name = DefaultClusterName
		}
		if len(s.Clusters[i].Roles) == 0 {
			s.Clusters[i].Roles = []string{RoleKamaji, RoleDBaaS}
		}
	}
}

// Validate checks an installation on its own, without the site: every rule of design §8.3 to §8.5 that needs nothing
// but the record. It returns every problem it finds.
func (in *Installation) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if !nameRE.MatchString(in.Name) {
		bad("name %q: lower-case letters, digits and dashes, at most 40", in.Name)
	}
	if in.ServiceID == "" {
		bad("serviceId is required")
	}
	switch in.ProvisionedBy {
	case ByPortal:
	case ByInstaller:
		bad("provisionedBy installer comes in a later release; use portal")
	default:
		bad("provisionedBy %q is not portal", in.ProvisionedBy)
	}
	switch in.Layout {
	case LayoutShared:
	case LayoutSeparate:
		bad("layout separate comes in a later release; use shared")
	default:
		bad("layout %q is not shared", in.Layout)
	}
	s := in.Spec
	if u, err := url.Parse(s.PVEAPIURL); err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		bad("spec.pveApiUrl %q is not an https:// URL with a host", s.PVEAPIURL)
	}
	imports := s.NodeImage.Mode == ImageBOM || s.Seed.Mode == SeedVM
	for _, st := range []struct{ field, v string }{{"boot", s.Storage.Boot}, {"iso", s.Storage.ISO}, {"import", s.Storage.Import}} {
		if (st.v != "" || st.field != "import" || imports) && !storageRE.MatchString(st.v) {
			bad("spec.storage.%s %q is not a storage id", st.field, st.v)
		}
	}
	// The portal token holds PVEDatastoreAdmin on the import store, which reaches every disk on it: on the boot store
	// that would undo its NoAccess on the platform pools (docs/proxmox-permissions.md). The API fills an empty import
	// store from the provider, and the preflight checks the mint role on the provider's store: the message names it.
	if s.Storage.Import != "" && s.Storage.Import == s.Storage.Boot {
		bad("spec.storage.import %q is the boot store: use another store with import content, the one the provider's importStorage names (an empty spec.storage.import takes it)", s.Storage.Import)
	}

	n := s.Network
	if !bridgeRE.MatchString(n.Bridge) {
		bad("spec.network.bridge %q is not an interface name", n.Bridge)
	}
	prefix, err := netip.ParsePrefix(n.Prefix)
	if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() || prefix.Bits() < 8 || prefix.Bits() > 29 {
		bad("spec.network.prefix %q is not an IPv4 network from /8 to /29", n.Prefix)
		return errors.Join(errs...) // every address below is checked against it
	}
	host := func(field, v string) (netip.Addr, bool) {
		a, err := netip.ParseAddr(v)
		last := lastAddr(prefix)
		if err != nil || !prefix.Contains(a) || a == prefix.Addr() || a == last {
			bad("%s %q is not a host address in %s", field, v, prefix)
			return netip.Addr{}, false
		}
		return a, true
	}
	gw, _ := host("spec.network.gateway", n.Gateway)
	if len(n.DNS) == 0 || len(n.DNS) > 3 {
		bad("spec.network.dns: one to three servers")
	}
	for _, d := range n.DNS {
		if a, err := netip.ParseAddr(d); err != nil || !a.Is4() {
			bad("spec.network.dns %q is not an IPv4 address", d)
		}
	}
	if n.MTU != 0 && (n.MTU < 1280 || n.MTU > 9000) {
		bad("spec.network.mtu %d is outside 1280 to 9000", n.MTU)
	}

	seed := s.Seed
	switch seed.Mode {
	case SeedVM:
		if seed.Pool != pve.SeedPool {
			bad("spec.seed.pool must be %s for a seed the portal builds", pve.SeedPool)
		}
	case SeedExisting:
		bad("spec.seed.mode existing comes in a later release; use vm")
	default:
		bad("spec.seed.mode %q is not vm", seed.Mode)
	}
	if !nodeRE.MatchString(seed.Node) {
		bad("spec.seed.node %q is not a node name", seed.Node)
	}
	seedAddr, _ := host("spec.seed.address", seed.Address)
	machine := func(field string, cores, mem, disk int) {
		if cores < minCores || cores > maxCores || mem < minMemMiB || mem > maxMemMiB || disk < minDiskGiB || disk > maxDisk {
			bad("%s: cores %d to %d, memoryMiB %d to %d, diskGiB %d to %d", field, minCores, maxCores, minMemMiB, maxMemMiB, minDiskGiB, maxDisk)
		}
	}
	machine("spec.seed", seed.Cores, seed.MemoryMiB, seed.DiskGiB)
	if len(seed.SSHKeys) > maxSSHKeys {
		bad("spec.seed.sshKeys: at most %d", maxSSHKeys)
	}
	for i, k := range seed.SSHKeys {
		if strings.ContainsAny(k, "\r\n") {
			bad("spec.seed.sshKeys[%d]: one key per entry, on one line", i)
		} else if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k)); err != nil {
			bad("spec.seed.sshKeys[%d] is not an SSH public key", i)
		}
	}
	if len(seed.APISources) == 0 || len(seed.APISources) > maxAPISources {
		bad("spec.seed.apiSources: one to %d CIDRs (at least the address the portal reaches the seed from)", maxAPISources)
	}
	for _, c := range seed.APISources {
		if p, err := netip.ParsePrefix(c); err != nil || !p.Addr().Is4() || p.Bits() == 0 {
			bad("spec.seed.apiSources %q is not an IPv4 CIDR narrower than /0", c)
		}
	}

	if len(s.Clusters) != 1 {
		bad("spec.clusters: the shared layout has exactly one cluster")
	}
	for ci, c := range s.Clusters {
		f := fmt.Sprintf("spec.clusters[%d]", ci)
		if !nameRE.MatchString(c.Name) {
			bad("%s.name %q is not a DNS label", f, c.Name)
		}
		if roles := slices.Sorted(slices.Values(c.Roles)); !slices.Equal(roles, []string{RoleDBaaS, RoleKamaji}) {
			bad("%s.roles: the shared cluster runs both kamaji and dbaas", f)
		}
		vip, _ := host(f+".endpoint", c.Endpoint)
		nodes, nodesOK := parseRange(f+".nodeRange", c.NodeRange, prefix, bad)
		lbs, lbsOK := parseRange(f+".metallbRange", c.MetalLBRange, prefix, bad)
		// Disjoint: gateway, seed, VIP, node range and MetalLB range each take their own addresses.
		points := []struct {
			field string
			a     netip.Addr
		}{{"spec.network.gateway", gw}, {"spec.seed.address", seedAddr}, {f + ".endpoint", vip}}
		for i, p := range points {
			for _, q := range points[i+1:] {
				if p.a.IsValid() && p.a == q.a {
					bad("%s and %s are the same address", p.field, q.field)
				}
			}
			for _, r := range []struct {
				field string
				r     addrRange
				ok    bool
			}{{f + ".nodeRange", nodes, nodesOK}, {f + ".metallbRange", lbs, lbsOK}} {
				if p.a.IsValid() && r.ok && r.r.contains(p.a) {
					bad("%s %s is inside %s", p.field, p.a, r.field)
				}
			}
		}
		if nodesOK && lbsOK && nodes.overlaps(lbs) {
			bad("%s.nodeRange and metallbRange overlap", f)
		}

		cp := c.ControlPlane
		if !nodeRE.MatchString(cp.Node) {
			bad("%s.controlPlane.node %q is not a node name", f, cp.Node)
		}
		machine(f+".controlPlane", cp.Cores, cp.MemoryMiB, cp.DiskGiB)
		var names []string
		machines, surge := 2, 0 // the control plane and its rolling surge
		for gi, g := range c.NodeGroups {
			gf := fmt.Sprintf("%s.nodeGroups[%d]", f, gi)
			names = append(names, g.Name)
			if len(g.Nodes) == 0 || len(g.Nodes) > maxGroupNodes {
				bad("%s.nodes: one to %d PVE nodes", gf, maxGroupNodes)
			}
			for _, nd := range g.Nodes {
				if !nodeRE.MatchString(nd) {
					bad("%s.nodes %q is not a node name", gf, nd)
				}
			}
			if len(slices.Compact(slices.Sorted(slices.Values(g.Nodes)))) != len(g.Nodes) {
				bad("%s.nodes: a node is listed twice", gf)
			}
			if g.Count < len(g.Nodes) || g.Count > maxGroupCount {
				bad("%s.count %d: at least one machine per chosen node, at most %d", gf, g.Count, maxGroupCount)
			}
			machine(gf, g.Cores, g.MemoryMiB, g.DiskGiB)
			machines += g.Count
			surge += len(g.Nodes) // one MachineDeployment per node, each surging by one in a rollout
		}
		if !slices.Equal(slices.Sorted(slices.Values(names)), slices.Sorted(slices.Values(NodeGroupNames))) {
			bad("%s.nodeGroups: exactly the groups %s, once each", f, strings.Join(NodeGroupNames, ", "))
		}
		if nodesOK && nodes.size() < uint64(machines+surge) {
			bad("%s.nodeRange holds %d addresses; %d machines and %d rolling surges need %d", f, nodes.size(), machines, surge, machines+surge)
		}

		switch s.NodeImage.Mode {
		case ImageBOM:
			if len(s.NodeImage.Templates) > 0 {
				bad("spec.nodeImage.templates go with mode templates")
			}
		case ImageTemplates:
			want := []string{cp.Node}
			for _, g := range c.NodeGroups {
				want = append(want, g.Nodes...)
			}
			for _, nd := range want {
				if s.NodeImage.Templates[nd] < 100 {
					bad("spec.nodeImage.templates: no template VMID for node %s", nd)
				}
			}
		default:
			bad("spec.nodeImage.mode %q is neither bom nor templates", s.NodeImage.Mode)
		}
	}
	return errors.Join(errs...)
}

// Site is what the Proxmox provider says about the networks the platform must stay apart from
// (externalservice.PVENetwork), plus the nodes' management networks when they were read live.
type Site struct {
	CIDRPool string
	Blocked  []string // config.network.blockedCidrs
	Sandbox  []string
	Public   []string // the public networks' CIDRs
	Mgmt     []string // the nodes' management networks (live; nil when not read)
	// DefaultZone is the zone a tenant network is made in (config.network.defaultZone) and DefaultZoneKind its kind
	// (evpn, vlan or simple); both empty when the provider names no zone. DefaultZoneNoDHCP: the zone's config says
	// dhcp: false (pve.ZoneConfig.NoDHCP).
	DefaultZone, DefaultZoneKind string
	DefaultZoneNoDHCP            bool
}

// SiteProblems checks an installation against its site: the platform network overlaps neither the tenant pool nor a
// sandbox or public network; no API source is a tenant network; and the MetalLB range, which tenants must reach, is
// outside the blocked and management networks.
func (in *Installation) SiteProblems(site Site) []string {
	var out []string
	prefix, err := netip.ParsePrefix(in.Spec.Network.Prefix)
	if err != nil {
		return []string{"spec.network.prefix is not valid"}
	}
	tenant := map[string][]string{"cidrPool": {site.CIDRPool}, "sandboxCidrs": site.Sandbox, "public networks": site.Public}
	for _, what := range []string{"cidrPool", "sandboxCidrs", "public networks"} {
		for _, c := range tenant[what] {
			p, err := netip.ParsePrefix(strings.TrimSpace(c))
			if err != nil {
				continue
			}
			if p.Overlaps(prefix) {
				out = append(out, fmt.Sprintf("the platform network %s overlaps %s %s", prefix, what, p))
			}
			for _, src := range in.Spec.Seed.APISources {
				if s, err := netip.ParsePrefix(src); err == nil && s.Overlaps(p) {
					out = append(out, fmt.Sprintf("spec.seed.apiSources %s overlaps %s %s: tenants must not reach the seed", s, what, p))
				}
			}
		}
	}
	fenced := append(slices.Clone(site.Blocked), site.Mgmt...)
	for _, c := range in.Spec.Clusters {
		lbs, ok := parseRange("", c.MetalLBRange, prefix, func(string, ...any) {})
		if !ok {
			continue
		}
		for _, b := range fenced {
			if p, err := netip.ParsePrefix(strings.TrimSpace(b)); err == nil && lbs.overlapsPrefix(p) {
				out = append(out, fmt.Sprintf("the MetalLB range %s overlaps the blocked or management network %s: tenants could not reach their load balancers", c.MetalLBRange, p))
			}
		}
	}
	return out
}

// Unfenced lists, as CIDRs, the addresses of the seed, the API VIP and the node range that no network of fenced (the
// blocked and the management networks) covers. Layer 0 drops tenant traffic to fenced networks only, so each listed
// CIDR must go into blockedCidrs.
func (in *Installation) Unfenced(fenced []string) []string {
	var nets []netip.Prefix
	for _, c := range fenced {
		if p, err := netip.ParsePrefix(strings.TrimSpace(c)); err == nil && p.Addr().Is4() {
			nets = append(nets, p.Masked())
		}
	}
	covered := func(a netip.Addr) bool {
		return slices.ContainsFunc(nets, func(p netip.Prefix) bool { return p.Contains(a) })
	}
	prefix, _ := netip.ParsePrefix(in.Spec.Network.Prefix)
	var ranges []addrRange
	if a, err := netip.ParseAddr(in.Spec.Seed.Address); err == nil {
		ranges = append(ranges, addrRange{a, a})
	}
	for _, c := range in.Spec.Clusters {
		if a, err := netip.ParseAddr(c.Endpoint); err == nil {
			ranges = append(ranges, addrRange{a, a})
		}
		if r, ok := parseRange("", c.NodeRange, prefix, func(string, ...any) {}); ok {
			ranges = append(ranges, r)
		}
	}
	var out []string
	// ponytail: walks every address, bounded by maxRangeSize per range; CIDR arithmetic if ranges grow past /20.
	for _, r := range ranges {
		var start netip.Addr
		for a := r.first; ; a = a.Next() {
			if !covered(a) && !start.IsValid() {
				start = a
			}
			if (covered(a) || a == r.last) && start.IsValid() {
				end := a
				if covered(a) {
					end = a.Prev()
				}
				for _, p := range rangeCIDRs(start, end) {
					out = append(out, p.String())
				}
				start = netip.Addr{}
			}
			if a == r.last {
				break
			}
		}
	}
	return slices.Compact(out)
}

// Installed reports whether a run ever succeeded, which fixes the fields FixedChanges names.
func (in *Installation) Installed() bool { return in.Status.BOM != nil }

// FixedChanges names the fields next changes that may not change: the provider always; once a run succeeded also the
// path (but for installer, which Validate refuses: such a record can only be corrected), the layout, the name, the network, the seed's mode, pool, node and address, and each cluster's name, endpoint
// and node groups. Before that nothing was built, so a draft can be corrected (no route deletes one).
func FixedChanges(old, next *Installation) []string {
	var out []string
	same := func(field string, a, b any) {
		if !reflect.DeepEqual(a, b) {
			out = append(out, field)
		}
	}
	same("serviceId", old.ServiceID, next.ServiceID)
	if !old.Installed() {
		return out
	}
	if old.ProvisionedBy != ByInstaller {
		same("provisionedBy", old.ProvisionedBy, next.ProvisionedBy)
	}
	same("layout", old.Layout, next.Layout)
	same("name", old.Name, next.Name)
	same("spec.network", old.Spec.Network, next.Spec.Network)
	same("spec.seed.mode", old.Spec.Seed.Mode, next.Spec.Seed.Mode)
	same("spec.seed.pool", old.Spec.Seed.Pool, next.Spec.Seed.Pool)
	same("spec.seed.node", old.Spec.Seed.Node, next.Spec.Seed.Node)
	same("spec.seed.address", old.Spec.Seed.Address, next.Spec.Seed.Address)
	if len(old.Spec.Clusters) != len(next.Spec.Clusters) {
		return append(out, "spec.clusters")
	}
	for i, c := range old.Spec.Clusters {
		n := next.Spec.Clusters[i]
		f := fmt.Sprintf("spec.clusters[%d]", i)
		same(f+".name", c.Name, n.Name)
		same(f+".endpoint", c.Endpoint, n.Endpoint)
		groups := func(gs []NodeGroup) (names []string) {
			for _, g := range gs {
				names = append(names, g.Name)
			}
			return names
		}
		same(f+".nodeGroups", groups(c.NodeGroups), groups(n.NodeGroups))
	}
	return out
}

// addrRange is an inclusive IPv4 range.
type addrRange struct{ first, last netip.Addr }

func (r addrRange) contains(a netip.Addr) bool {
	return r.first.Compare(a) <= 0 && a.Compare(r.last) <= 0
}
func (r addrRange) overlaps(o addrRange) bool {
	return r.first.Compare(o.last) <= 0 && o.first.Compare(r.last) <= 0
}
func (r addrRange) overlapsPrefix(p netip.Prefix) bool {
	p = p.Masked()
	return p.Addr().Is4() && r.overlaps(addrRange{p.Addr(), lastAddr(p)})
}
func (r addrRange) size() uint64 { return uint64(u32(r.last)-u32(r.first)) + 1 }

// parseRange reads "first-last" inside prefix, at most maxRangeSize addresses.
func parseRange(field, v string, prefix netip.Prefix, bad func(string, ...any)) (addrRange, bool) {
	a, b, ok := strings.Cut(v, "-")
	first, err1 := netip.ParseAddr(strings.TrimSpace(a))
	last, err2 := netip.ParseAddr(strings.TrimSpace(b))
	r := addrRange{first, last}
	if !ok || err1 != nil || err2 != nil || !prefix.Contains(first) || !prefix.Contains(last) || first.Compare(last) > 0 ||
		first == prefix.Addr() || last == lastAddr(prefix) {
		bad("%s %q is not first-last host addresses in %s", field, v, prefix)
		return r, false
	}
	if r.size() > maxRangeSize {
		bad("%s %q holds more than %d addresses", field, v, maxRangeSize)
		return r, false
	}
	return r, true
}

func u32(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func addrOf(n uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}

func lastAddr(p netip.Prefix) netip.Addr {
	return addrOf(u32(p.Masked().Addr()) | (1<<(32-p.Bits()) - 1))
}

// rangeCIDRs is the exact CIDR cover of first..last.
func rangeCIDRs(first, last netip.Addr) []netip.Prefix {
	var out []netip.Prefix
	lo, hi := uint64(u32(first)), uint64(u32(last))
	for lo <= hi {
		bits := 32
		for bits > 0 {
			size := uint64(1) << (32 - (bits - 1))
			if lo%size != 0 || lo+size-1 > hi {
				break
			}
			bits--
		}
		out = append(out, netip.PrefixFrom(addrOf(uint32(lo)), bits))
		lo += uint64(1) << (32 - bits)
	}
	return out
}

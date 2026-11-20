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

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// Installation is one document of collection platformInstallation: one per Proxmox provider.
type Installation struct {
	ID            string    `json:"id"`   // pi-<8hex>
	Name          string    `json:"name"` // prefixes the providers: "<name> Kubernetes", "<name> Databases"
	ServiceID     string    `json:"serviceId"`
	ProvisionedBy string    `json:"provisionedBy"` // portal | installer (M2: refused by Validate)
	Layout        string    `json:"layout"`        // of kamaji and dbaas: shared (separate is refused, not built). The sandbox's placement is Spec.Sandbox
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
	Tenants   Tenants       `json:"tenants,omitzero"`
	// Sandbox is where the sandbox service will run, a choice the install offers once that service ships: placement
	// shared (a node group of the platform cluster, the homelab choice) or separate (a cluster of its own). No release
	// builds it yet, so Validate refuses the block; a spec without it means "no sandbox service" and keeps meaning that,
	// which makes the later release an addition for every stored record.
	Sandbox *SandboxSpec `json:"sandbox,omitempty"`
	// Access is what the operator opens to named networks. Not fixed: it may be set, changed and cleared on an
	// installed platform, and the next apply makes it so.
	Access Access `json:"access,omitzero"`
}

// Access holds the addresses the platform answers at beyond its API. ArgoCD nil = none: the platform cluster's ArgoCD
// then stays a ClusterIP Service with plain HTTP, as every release before this field rendered it.
type Access struct {
	ArgoCD *ArgoCDAccess `json:"argocd,omitempty"`
}

// ArgoCDAccess gives the platform cluster's ArgoCD (tenant clusters and databases) an address: an IPv4 of the
// cluster's MetalLB range, answered over HTTPS with ArgoCD's own self-signed certificate, for Sources only (CIDRs).
// The seed's ArgoCD never gets one: its admin controls Cluster API and every machine.
type ArgoCDAccess struct {
	Address string   `json:"address"`
	Sources []string `json:"sources"`
}

// URL is where the block makes ArgoCD answer.
func (a *ArgoCDAccess) URL() string { return "https://" + a.Address }

// SandboxSpec is the placement of the sandbox service (see Spec.Sandbox).
type SandboxSpec struct {
	Placement string `json:"placement"` // shared | separate
}

// Storage: boot holds every platform disk and is the std StorageClass; iso is the node-local store of bootstrap ISOs;
// import is where Template updates downloads the two images.
type Storage struct {
	Boot   string `json:"boot"`
	ISO    string `json:"iso"`
	Import string `json:"import"`
}

// Network is the platform network the seed and the platform machines share. Mode bridge (also what a stored spec
// without a mode means): Bridge exists on every node used, and VLAN, when set, tags every NIC on it (the seed's net0,
// CAPMOX's networkDevices[].vlan), so a VLAN needs no bridge or vnet of its own. Mode routed: Bridge names a vnet
// ProxCloud makes in the simple SDN zone RoutedZone, with one subnet Prefix whose gateway the node holds and SNAT on
// (network.go); every machine is then on one Proxmox node.
type Network struct {
	Mode    string   `json:"mode,omitempty"` // bridge (also "") | routed
	Bridge  string   `json:"bridge"`
	VLAN    int      `json:"vlan,omitempty"` // 1 to 4094, mode bridge only; 0 = untagged
	Prefix  string   `json:"prefix"`         // IPv4 CIDR
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

// Machine is the control plane: the size of one of its machines, their count and their Proxmox node. Replicas is 1 or
// 3 (a stored spec without it means 1) and may grow from 1 to 3 on an installed platform, not shrink. Every machine is
// on Node: the cluster chart pins the control plane's one machine template to a node, and CAPMOX v0.9.2 clones to
// another node only when the template and the new disk are on shared storage. Nodes is the place for that spread; this
// release takes it only empty or as [Node].
type Machine struct {
	Node      string   `json:"node"`
	Nodes     []string `json:"nodes,omitempty"`
	Replicas  int      `json:"replicas,omitempty"`
	Cores     int      `json:"cores"`
	MemoryMiB int      `json:"memoryMiB"`
	DiskGiB   int      `json:"diskGiB"`
}

// Count is the number of control plane machines: Replicas, and 1 for a spec stored before the field.
func (m Machine) Count() int { return max(m.Replicas, 1) }

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
	Phase string  `json:"phase"`
	BOM   *BOMRef `json:"bom,omitempty"` // the BOM of the last successful apply; nil = never succeeded
	// TargetBOM is the BOM an upgrade is moving the platform to: set when its first run starts, cleared by the run that
	// succeeds (which sets BOM to it). While it is set BOM still names the BOM the platform left, Components say per
	// component what has moved, and every apply continues to it.
	TargetBOM *BOMRef `json:"targetBom,omitempty"`
	// TenantsDownBefore are the tenants that were not healthy when the upgrade first reached the workload's waves
	// (non-nil, also when empty: the list was taken). They do not hold its runs; every other tenant does, in every run
	// of the upgrade, a Retry included. Cleared with TargetBOM.
	TenantsDownBefore *[]string         `json:"tenantsDownBefore,omitempty"`
	Seed              *SeedStatus       `json:"seed,omitempty"`
	Clusters          []ClusterStatus   `json:"clusters,omitempty"`
	Components        []ComponentStatus `json:"components,omitempty"`
	Volumes           []Volume          `json:"volumes,omitempty"`
	Providers         *ProviderIDs      `json:"providers,omitempty"`
	// TenantsAttach is the spec.tenants.attach the register stage applied last ("" = none was): a run acts on the
	// choice only when it differs, so an apply never undoes what the operator changed on a provider since.
	TenantsAttach string `json:"tenantsAttach,omitempty"`
	// KubernetesOffered are the Kubernetes versions a run or POST …/kubernetes-version offered on the Kubernetes
	// provider (or found there). A run offers a version once: one the operator removed from the provider since is not
	// added again by an apply, only by the route.
	KubernetesOffered []string `json:"kubernetesOffered,omitempty"`
	// ArgoCDURL is where the platform cluster's ArgoCD answers, written by stage 9 once the Service argocd-server holds
	// the address of spec.access.argocd, with ArgoCDSources the networks let in then; both cleared by the apply that
	// finds the block cleared. The plan compares the block with them. Sources without a URL: a run rendered the block
	// and MetalLB did not give the address (argocd_address_unavailable).
	ArgoCDURL     string   `json:"argocdUrl,omitempty"`
	ArgoCDSources []string `json:"argocdSources,omitempty"`
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
	Name              string `json:"name"`
	Namespace         string `json:"namespace,omitempty"`
	Endpoint          string `json:"endpoint,omitempty"`
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	KubeconfigSecret  string `json:"kubeconfigSecret,omitempty"`
	ManagedBy         string `json:"managedBy,omitempty"` // application | raw
	// ControlPlaneReplicas is the largest control plane count above one a run handed to the cluster's Application,
	// written before that sync; absent means one. ControlPlaneShrink reads it: the stored spec says what is asked for,
	// not what was built.
	ControlPlaneReplicas int            `json:"controlPlaneReplicas,omitempty"`
	Templates            map[string]int `json:"templates,omitempty"`
	ArgoCDTokenExpiresAt *time.Time     `json:"argocdTokenExpiresAt,omitempty"`
	// Roll is set while an upgrade replaces the cluster's machines: from the moment stage 7 begins the roll until the
	// run that succeeds (stage 11 clears it with status.targetBom). While it is set KubernetesVersion and Templates
	// still name what the unreached units run, until the last unit is done.
	Roll *RollStatus `json:"roll,omitempty"`
}

// RollStatus is how far the roll of a cluster's machines has come. Step is the unit being replaced, counted from 1 in
// the order of rollUnits (1 is the control plane); one more than the number of units when all are done. It is written
// before the step's Application is applied and only ever raised: an apply renders from it and never from a read of the
// cluster, so a misread cannot put a replaced unit back on the old version. DeleteFirst are the units ("group/node")
// replaced by deleting the machine first, as the plan the operator confirmed said.
type RollStatus struct {
	Step        int      `json:"step"`
	DeleteFirst []string `json:"deleteFirst,omitempty"`
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

	NetworkBridge = "bridge"
	NetworkRouted = "routed"
	// RoutedZone is the simple SDN zone of a routed platform network. One per Proxmox cluster, as one installation is.
	RoutedZone = "pcplat"

	SandboxShared   = "shared"
	SandboxSeparate = "separate"

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
	vnetRE    = regexp.MustCompile(`^[a-z][a-z0-9]{0,7}$`) // a Proxmox SDN vnet id
)

// Routed reports mode routed: ProxCloud makes the platform network and the node routes it.
func (n Network) Routed() bool { return n.Mode == NetworkRouted }

// Normalize fills the defaults of a new or updated installation: shared layout, a portal-built seed in pool
// proxcloud-seed, one cluster pc-platform that runs both services with one control plane machine, and the BOM's node
// image.
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
	if a := s.Access.ArgoCD; a != nil && a.Address == "" && len(a.Sources) == 0 {
		s.Access.ArgoCD = nil // an empty block says what no block says
	}
	for i := range s.Clusters {
		if s.Clusters[i].Name == "" {
			s.Clusters[i].Name = DefaultClusterName
		}
		if len(s.Clusters[i].Roles) == 0 {
			s.Clusters[i].Roles = []string{RoleKamaji, RoleDBaaS}
		}
		cp := &s.Clusters[i].ControlPlane
		cp.Replicas = cp.Count()
		if slices.Equal(cp.Nodes, []string{cp.Node}) {
			cp.Nodes = nil // says what node says: not stored
		}
	}
}

// ArgoSourceProblems names an entry of spec.access.argocd.sources that Kubernetes 1.36 refuses in the ArgoCD Service's
// loadBalancerSourceRanges (cloud.StrictCIDRs): a save refuses it. An entry stored already has passes, because the API
// server keeps a value the Service holds and the stored spec is never rewritten; the argocd-access row warns about it.
// It is not part of Validate, which also reads stored specs: a platform installed before the rule must still plan.
func (in *Installation) ArgoSourceProblems(stored *Spec) []string {
	a := in.Spec.Access.ArgoCD
	if a == nil {
		return nil
	}
	var had []string
	if stored != nil && stored.Access.ArgoCD != nil {
		had = stored.Access.ArgoCD.Sources
	}
	if err := cloud.StrictCIDRs(a.Sources, had); err != nil {
		return []string{"spec.access.argocd.sources: " + err.Error()}
	}
	return nil
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
	if s.Sandbox != nil {
		switch s.Sandbox.Placement {
		case SandboxShared, SandboxSeparate:
			bad("spec.sandbox: the sandbox service on the platform comes in a later release; leave spec.sandbox out")
		default:
			bad("spec.sandbox.placement %q is neither shared nor separate", s.Sandbox.Placement)
		}
	}
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

	switch s.Tenants.Attach {
	case "", AttachAll, AttachNew, AttachNone:
	default:
		bad("spec.tenants.attach %q is not all, new or none", s.Tenants.Attach)
	}

	n := s.Network
	switch n.Mode {
	case "", NetworkBridge:
		if !bridgeRE.MatchString(n.Bridge) {
			bad("spec.network.bridge %q is not an interface name", n.Bridge)
		}
		if n.VLAN < 0 || n.VLAN > 4094 {
			bad("spec.network.vlan %d is outside 1 to 4094", n.VLAN)
		}
	case NetworkRouted:
		if !vnetRE.MatchString(n.Bridge) {
			bad("spec.network.bridge %q: with mode routed it names the vnet ProxCloud makes: a lower-case letter, then letters and digits, at most 8", n.Bridge)
		}
		if n.VLAN != 0 {
			bad("spec.network.vlan goes with mode bridge: a routed network is a vnet of its own, without a tag")
		}
		// A vnet of a simple zone is a separate network on each node.
		nodes := []string{s.Seed.Node}
		for _, c := range s.Clusters {
			nodes = append(nodes, c.ControlPlane.Node)
			for _, g := range c.NodeGroups {
				nodes = append(nodes, g.Nodes...)
			}
		}
		if nodes = slices.Compact(slices.Sorted(slices.Values(nodes))); len(nodes) > 1 {
			bad("spec.network.mode routed is a layout for one Proxmox node, and the settings place machines on %s: put the seed, the control plane and every node group on one node, or use mode bridge",
				strings.Join(nodes, ", "))
		}
	default:
		bad("spec.network.mode %q is neither bridge nor routed", n.Mode)
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
		if a := s.Access.ArgoCD; a != nil && ci == 0 {
			if addr, err := netip.ParseAddr(a.Address); err != nil || !addr.Is4() {
				bad("spec.access.argocd.address %q is not an IPv4 address (set address and sources together, or leave the block out)", a.Address)
			} else if lbsOK && !lbs.contains(addr) {
				bad("spec.access.argocd.address %s is outside %s.metallbRange %s: MetalLB announces only addresses of that range", addr, f, c.MetalLBRange)
			}
			if len(a.Sources) == 0 || len(a.Sources) > maxAPISources {
				bad("spec.access.argocd.sources: one to %d CIDRs, the networks the operators come from (set address and sources together, or leave the block out)", maxAPISources)
			}
			for _, src := range a.Sources {
				if p, err := netip.ParsePrefix(src); err != nil || !p.Addr().Is4() || p.Bits() == 0 {
					bad("spec.access.argocd.sources %q is not an IPv4 CIDR narrower than /0", src)
				}
			}
		}

		cp := c.ControlPlane
		if !nodeRE.MatchString(cp.Node) {
			bad("%s.controlPlane.node %q is not a node name", f, cp.Node)
		}
		machine(f+".controlPlane", cp.Cores, cp.MemoryMiB, cp.DiskGiB)
		if cp.Replicas != 0 && cp.Replicas != 1 && cp.Replicas != 3 {
			bad("%s.controlPlane.replicas %d: 1 or 3 (etcd needs an odd number of members, and this release builds no more than 3)", f, cp.Replicas)
		}
		if len(cp.Nodes) > 0 && !slices.Equal(cp.Nodes, []string{cp.Node}) {
			bad("%s.controlPlane.nodes: every control plane machine goes to one Proxmox node, controlPlane.node. Machines on several nodes need the node image template and the control plane disks on shared storage (CAPMOX v0.9.2 clones one template, and Proxmox moves a clone to another node only from and to shared storage), and this release does not build that: leave controlPlane.nodes out", f)
		}
		var names []string
		machines, surge := cp.Count()+1, 0 // the control plane machines and the one more a rolling update makes
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
			bad("%s.nodeRange holds %d addresses; %d machines (%d of the control plane and its one more during an update) and %d rolling surges need %d", f, nodes.size(), machines, cp.Count(), surge, machines+surge)
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
	// NodeAddrs: every address the nodes hold, as /32 (live; read for a routed network only).
	NodeAddrs []string
	// DefaultZone is the zone a tenant network is made in (config.network.defaultZone) and DefaultZoneKind its kind
	// (evpn, vlan or simple); both empty when the provider names no zone. DefaultZoneNoDHCP: the zone's config says
	// dhcp: false (pve.ZoneConfig.NoDHCP).
	DefaultZone, DefaultZoneKind string
	DefaultZoneNoDHCP            bool
	// DefaultZoneExitNodes: the exit nodes the config names for an EVPN default zone (pve.ZoneConfig.ExitNodes); the
	// tenant-path row reads them for a zone that does not exist yet.
	DefaultZoneExitNodes []string
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
	if in.Spec.Network.Routed() {
		// The node routes this network and holds its gateway: inside a network the node is already in, the node's own
		// traffic to that network would go to the vnet. The gateway itself is the node's once the vnet is made.
		for _, c := range append(slices.Clone(site.Mgmt), site.NodeAddrs...) {
			p, err := netip.ParsePrefix(strings.TrimSpace(c))
			if err != nil || !p.Overlaps(prefix) || p.IsSingleIP() && p.Addr().String() == in.Spec.Network.Gateway {
				continue
			}
			what := "the node network " + p.Masked().String()
			if p.IsSingleIP() {
				what = "the node address " + p.Addr().String()
			}
			out = append(out, fmt.Sprintf("the routed platform network %s overlaps %s: a routed network must be a range no Proxmox node is in", prefix, what))
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
// path (but for installer, which Validate refuses: such a record can only be corrected), the layout, the name, the network, the seed's mode, pool, node and address, and each cluster's name, endpoint,
// control plane node and node groups. Before that nothing was built, so a draft can be corrected (no route deletes one).
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
	// A spec stored before the mode existed has none, and means bridge.
	mode := func(n Network) Network {
		if n.Mode == "" {
			n.Mode = NetworkBridge
		}
		return n
	}
	same("spec.network", mode(old.Spec.Network), mode(next.Spec.Network))
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
		// Every control plane machine is on this node: another node means each of them made anew there, which no
		// lab has run. A group's nodes are not fixed: a node is added to scale out, and the status does not record
		// which nodes a run built on, so a refusal would also hold a node that was saved by mistake and never built.
		same(f+".controlPlane.node", c.ControlPlane.Node, n.ControlPlane.Node)
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

// KeepMachines makes a spec sent for an installed platform keep the stored machines where it names none: no cluster at
// all keeps the stored clusters, a cluster without controlPlane keeps the stored control plane, a cluster without
// nodeGroups keeps the stored groups. Missing means keep, never a default: on a platform that runs, a default would
// move, resize or recount machines that the caller did not name (lab round of 2026-11-19, finding P1). A machine the
// spec does name is taken as sent, and checked as before.
func KeepMachines(old, next *Spec) {
	if len(next.Clusters) == 0 {
		next.Clusters = slices.Clone(old.Clusters)
		return
	}
	for i := range min(len(old.Clusters), len(next.Clusters)) {
		c, o := &next.Clusters[i], old.Clusters[i]
		if reflect.DeepEqual(c.ControlPlane, Machine{}) {
			c.ControlPlane = o.ControlPlane
		}
		if len(c.NodeGroups) == 0 {
			c.NodeGroups = slices.Clone(o.NodeGroups)
		}
	}
}

// ControlPlaneShrink is why next may not be saved over old, or "": a control plane a run built with 3 machines does
// not go to 1. Cluster API would take two etcd members out one after the other; with two members left, one slow or
// lost member stops etcd, and all three are on one node and one store. No lab has run it, and nothing here repairs a
// control plane that stopped half way. What counts is what a run handed to the cluster (Status.Clusters), not the
// stored spec: a 3 that was saved and never applied goes back to 1. From 1 to 3 is an apply that adds two machines.
func ControlPlaneShrink(old, next *Installation) string {
	for i, c := range old.Status.Clusters {
		built := max(c.ControlPlaneReplicas, 1)
		if i < len(next.Spec.Clusters) && next.Spec.Clusters[i].ControlPlane.Count() < built {
			return fmt.Sprintf("spec.clusters[%d].controlPlane.replicas: a run built this control plane with %d machines, and it does not go from %d machines to %d in this release. "+
				"Taking etcd members out leaves two for a while, where one slow or lost member stops the platform's API, and no lab has run that step. "+
				"Keep %d; to get a platform with one, remove the installation and install again.",
				i, built, built, next.Spec.Clusters[i].ControlPlane.Count(), built)
		}
	}
	return ""
}

// controlPlaneGrowth is, for cluster i of an installed platform whose settings ask for more control plane machines than
// a run built, both counts; 0, 0 otherwise. Built is what ControlPlaneShrink reads. The plan lists it as a change and
// the capacity row fails on it: those machines are not placed yet.
func (in *Installation) controlPlaneGrowth(i int) (from, to int) {
	if !in.Installed() || i >= len(in.Status.Clusters) || i >= len(in.Spec.Clusters) {
		return 0, 0
	}
	from, to = max(in.Status.Clusters[i].ControlPlaneReplicas, 1), in.Spec.Clusters[i].ControlPlane.Count()
	if to <= from {
		return 0, 0
	}
	return from, to
}

// BuildsThree reports whether the cluster chart of the installation's BOM (the installed one, else this release's)
// builds more than one control plane machine.
func (in *Installation) BuildsThree() bool {
	bom, err := bomOf(in)
	return err == nil && kamaji.ChartHasControlPlaneReplicas(ClusterChartVersion(bom))
}

// ControlPlaneCountProblems is why the installation's BOM cannot build the control plane count of the spec, or nil.
// The record alone cannot say it, so Validate does not: the save and the Check's spec row both ask here. Without it a
// saved 3 would fail every apply and upgrade of a platform whose chart builds one.
func (in *Installation) ControlPlaneCountProblems() []string {
	bom, err := bomOf(in)
	if err != nil || kamaji.ChartHasControlPlaneReplicas(ClusterChartVersion(bom)) {
		return nil
	}
	var out []string
	for _, c := range in.Spec.Clusters {
		if n := c.ControlPlane.Count(); n > 1 {
			out = append(out, fmt.Sprintf("spec.clusters: %d control plane machines: the platform's BOM (release %s) names cluster chart %s, which builds one; more need chart %s or newer, so keep 1 until the platform is on a release that has it",
				n, bom.Release, ClusterChartVersion(bom), kamaji.ControlPlaneReplicasChartVersion))
		}
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

// Package kamaji drives Managed Kubernetes clusters on a Kamaji management cluster: one
// ArgoCD Application per customer cluster (the provider's configured cluster chart from our OCI
// registry, pinned targetRevision + full generated values), applied through the management
// cluster's own API (the Application CRD — no ArgoCD API/auth involved). ArgoCD renders,
// syncs and health-checks; proxcloud reads status back off the Application + the Kamaji
// TenantControlPlane + CAPI MachineDeployments.
package kamaji

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
)

// Namespace / naming derivations — the ONE place these are derived (ceph RGWUIDFor precedent).
// Customer-typed names are display-only; every k8s-side identifier is the generated cluster id
// (`pc-c-<8 hex>`, or an older prefix on an older cluster), so duplicate display names, unicode and the RFC1123/63-char limit are
// non-issues, and the API endpoint/DNS never changes on rename.

// NamespaceFor is the management-cluster namespace holding a project's clusters.
func NamespaceFor(projectID string) string { return "pc-" + projectID }

// CloudSecretName is the per-cluster cloud credentials secret (management-cluster side ONLY —
// the customer cluster never sees it; see plan §4/D7).
func CloudSecretName(clusterID string) string { return clusterID + cloudSecretSuffix }

// CSISecretName is the resource-set Secret carrying the tenant-side proxmox-csi config (with its
// scoped token) to the customer cluster.
func CSISecretName(clusterID string) string { return clusterID + "-csi-config" }

// CSIGatewaySecretName is the Secret of the cluster's proxmox-csi controller on the services cluster: its config with
// the cluster's gateway credential, and the CA of the gateway. Management side only, never in the resource set.
func CSIGatewaySecretName(clusterID string) string { return clusterID + "-csi-gateway" }

// GatewayChartVersion is the first cluster chart that renders the services-side proxmox-csi controller.
const GatewayChartVersion = "0.1.7"

// ChartHasGatewayCSI reports whether chart version v ("x.y.z", optionally "-pre"/"+build", which counts as x.y.z so a
// drill can tag one) renders that controller. Anything else, a pinned 0.1.5 or a malformed pin, does not: a cluster on
// it would get the tenant controller at 0 replicas and no controller here.
func ChartHasGatewayCSI(v string) bool { return chartAtLeast(v, GatewayChartVersion) }

// PlacementChartVersion is the first cluster chart that reads a node group's machinePlacement. An older chart ignores
// the key and splits evenly, so a placement written to a cluster pinned below it would take effect, and move running
// workers, only at that cluster's next platform update: the portal writes none there.
const PlacementChartVersion = "0.1.13"

// ChartHasPlacement reports whether chart version v places a node group by machinePlacement.
func ChartHasPlacement(v string) bool { return chartAtLeast(v, PlacementChartVersion) }

// VLANChartVersion is the first cluster chart that reads proxmox.network.vlan (CAPMOX networkDevices[].vlan). An older
// chart ignores the key and leaves the machines untagged on the bridge.
const VLANChartVersion = "0.1.14"

// ChartHasVLAN reports whether chart version v tags the machines' NIC with proxmox.network.vlan.
func ChartHasVLAN(v string) bool { return chartAtLeast(v, VLANChartVersion) }

// RollOrderChartVersion is the first cluster chart that reads a node group's kubernetesVersions and deleteFirstOn: what
// the platform's upgrade run orders the replacement of the platform cluster's machines with, one MachineDeployment at
// a time. An older chart ignores both keys and would replace every machine at once.
const RollOrderChartVersion = "0.1.15"

// ChartHasRollOrder reports whether chart version v can hold a MachineDeployment at another Kubernetes version than
// the cluster's.
func ChartHasRollOrder(v string) bool { return chartAtLeast(v, RollOrderChartVersion) }

// ControlPlaneReplicasChartVersion is the first cluster chart that reads controlPlane.kubeadm.replicas. An older chart
// writes one control plane machine whatever the value says.
const ControlPlaneReplicasChartVersion = "0.1.14"

// ChartHasControlPlaneReplicas reports whether chart version v builds a kubeadm control plane of more than one machine.
func ChartHasControlPlaneReplicas(v string) bool {
	return chartAtLeast(v, ControlPlaneReplicasChartVersion)
}

// MachineHealthCheckChartVersion is the first cluster chart that reads machineHealthCheck: one MachineHealthCheck per
// node group, which replaces a worker that registered no Node within the startup timeout. An older chart ignores
// the key, so none is written for a cluster pinned below it.
const MachineHealthCheckChartVersion = "0.1.14"

// ChartHasMachineHealthCheck reports whether chart version v renders the node groups' MachineHealthChecks.
func ChartHasMachineHealthCheck(v string) bool {
	return chartAtLeast(v, MachineHealthCheckChartVersion)
}

// DefaultNodeStartupTimeoutSeconds is the startup timeout of a provider that names none: the bound after which the
// status sync already calls a single cloning machine stalled (boundProvisioning). Not measured on a lab: Cluster
// API counts from the Machine's creation until its VM is ready, so a site whose clones take longer needs a larger
// config.cluster.nodeStartupTimeoutSeconds, or 0.
const DefaultNodeStartupTimeoutSeconds = 1800

// MachineHealthCheckValues is the chart's machineHealthCheck block for a cluster on chartVersion, nil when the chart
// does not read it or the provider turned the repair off.
func (d ClusterDefaults) MachineHealthCheckValues(chartVersion string) map[string]any {
	if !ChartHasMachineHealthCheck(chartVersion) || d.NodeStartupTimeoutSeconds <= 0 {
		return nil
	}
	return map[string]any{"nodeStartupTimeoutSeconds": d.NodeStartupTimeoutSeconds, "maxUnhealthy": max(d.RepairMaxUnhealthy, 1)}
}

// chartAtLeast compares chart versions "x.y.z" ("-pre" and "+build" count as x.y.z). A malformed v is below any.
func chartAtLeast(v, min string) bool {
	parse := func(s string) (out [3]int, ok bool) {
		s = strings.TrimPrefix(s, "v")
		if i := strings.IndexAny(s, "-+"); i >= 0 {
			s = s[:i]
		}
		parts := strings.Split(s, ".")
		if len(parts) != 3 {
			return out, false
		}
		for i, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 {
				return out, false
			}
			out[i] = n
		}
		return out, true
	}
	have, ok := parse(v)
	want, _ := parse(min)
	return ok && slices.Compare(have[:], want[:]) >= 0
}

// cloudSecretSuffix lets FinalizeOrphans map a secret name back to its cluster id.
const cloudSecretSuffix = "-cloud-config"

// MarkerSecretName is the durable record that a cluster exists. CAPMOX takes ownership of the cloud
// secret, so that one is garbage-collected with the ProxmoxCluster, before the sweep can see the cascade
// finished. Nothing owns the marker: the sweep keys on it, and it carries the compute-service annotation.
func MarkerSecretName(clusterID string) string { return clusterID + markerSuffix }

const markerSuffix = "-marker"

// ControlPlaneName is the KamajiControlPlane the chart renders for a cluster — and therefore the
// TenantControlPlane, since the Kamaji CAPI provider names the TCP after it. The chart builds it as
// `<release>-kamaji-cp` (templates/control-plane/kamaji-control-plane.yaml), and Kamaji in turn
// derives `<tcp>-admin-kubeconfig` from that. Getting this wrong is invisible until the first
// kubeconfig fetch, so it lives here next to the other name derivations.
func ControlPlaneName(clusterID string) string { return clusterID + "-kamaji-cp" }

// AdminKubeconfigSecretName is the Kamaji-issued admin kubeconfig secret for a cluster.
func AdminKubeconfigSecretName(clusterID string) string {
	return ControlPlaneName(clusterID) + "-admin-kubeconfig"
}

// Application labels/annotations stamped by proxcloud and read back by the sync.
//
// LabelManagedBy/ManagedByValue is the OWNERSHIP marker (decision 2026-07-12): clusters that
// pre-date proxcloud on the management cluster (the infra-ops wrappers) must stay invisible and
// untouchable — the sync lists ONLY objects carrying this label, and delete/patch REFUSE any
// object without it. Pre-existing clusters get migrated by hand later (stamp the labels).
const (
	LabelProject          = "proxcloud.io/project"
	LabelService          = "proxcloud.io/service"
	LabelManagedBy        = "app.kubernetes.io/managed-by"
	ManagedByValue        = "proxcloud"
	AnnotationDisplayName = "proxcloud.io/display-name"
	// AnnotationChangedAt is when proxcloud last changed the Application's values (RFC3339), stamped by every
	// values patch.
	AnnotationChangedAt = "proxcloud.io/values-changed-at"
)

// labelNodeGroup is what the chart stamps on a group's MachineDeployments.
const labelNodeGroup = "proxcloud.io/node-group"

// managedBy reports whether obj carries the proxcloud ownership marker.
func managedBy(obj map[string]any) bool {
	labels, _ := dig(obj, "metadata", "labels").(map[string]any)
	v, _ := labels[LabelManagedBy].(string)
	return v == ManagedByValue
}

// Config is the provider-level connection + chart contract, assembled from the kamaji
// externalService document by externalservice.KamajiConfig.
type Config struct {
	Kubeconfig    string // management-cluster kubeconfig (provider secret)
	Region        string // the proxcloud region stamped on cached resources
	ArgoNamespace string // namespace holding Application CRs (default "argocd")
	ArgoProject   string // AppProject constraining sources/destinations (default "proxcloud-k8s")
	ChartRepo     string // OCI helm repo, e.g. "ghcr.io/mutex-solutions/charts" (no oci:// prefix in Argo repoURL)
	ChartName     string // required — no default until the Proxmox cluster chart lands
	ChartVersion  string // pinned default for NEW clusters; existing clusters keep their own pin
	Defaults      ClusterDefaults
	// NamespacePlacement pins the new pods of each project namespace EnsureProjectNamespace applies to a node group
	// (config.namespacePlacement). Empty: the namespaces carry no placement and no platform-role label, as before it
	// existed.
	NamespacePlacement kamajik8s.NamespacePlacement
}

// ClusterDefaults are the per-provider chart value defaults every cluster inherits.
type ClusterDefaults struct {
	DataStoreName string // Kamaji DataStore (default "default")
	DNSZone       string // optional: API FQDN = <clusterID>.<DNSZone> (certSAN + external-dns)
	// Versions maps each curated k8s version → its DEFAULT node image (the ONLY versions offered).
	// A node image is the template of that version per PVE node, "pve1=9210,pve2=9211": a clone to
	// local storage stays on its template's node, so a node group gets one machine pool per entry.
	Versions map[string]string
	// ImageVariants are curated alternative node images per version, keyed variant → version →
	// image id (e.g. "nvidia" → {"1.35.4": "<id>"} for the GPU build of the same release). A node
	// group picks a variant BY NAME; upgrades re-resolve the group onto the target version's image
	// of the SAME variant, so a GPU pool never silently rolls back onto the plain image.
	ImageVariants map[string]map[string]string
	Flavors       []string // optional node-flavor allowlist (empty = every tenant flavor)
	// RootVolumeGiB is the default worker root-disk size. Node images are larger than most
	// flavors' disk, so every node gets an explicitly sized root disk.
	RootVolumeGiB int
	// AllowedCIDRs is the provider-wide default API-server ingress allowlist; a cluster's own
	// AllowedCIDRs overrides it. Empty on both = open.
	AllowedCIDRs []string
	// PVEAPIURL is the PVE API as CAPMOX on the services cluster reaches it (and, with Shared, the workers' proxmox-csi).
	PVEAPIURL string
	// RWXGiB, when set, gives every new cluster a NAS dataset of that size and the shared-file
	// StorageClass on it (csi-driver-nfs). Needs a TrueNAS provider on the project.
	RWXGiB int
	// DNSServers go to the worker nodes (default: public resolvers).
	DNSServers []string
	// NodeGroupMTU is the worker NIC MTU on a tenant vnet (EVPN: the vnet's own, 1450).
	NodeGroupMTU int
	// ProxmoxServiceID names the compute provider whose PVE cluster hosts the workers. Empty: the
	// project's only proxmox service.
	ProxmoxServiceID string
	// NodeStartupTimeoutSeconds is how long a worker may run without registering a Node before Cluster API replaces
	// it (chart machineHealthCheck, from MachineHealthCheckChartVersion). 0: no automatic replacement. The provider
	// parser fills DefaultNodeStartupTimeoutSeconds when config.cluster.nodeStartupTimeoutSeconds is absent.
	NodeStartupTimeoutSeconds int
	// RepairMaxUnhealthy is the guard of that replacement, a number of machines per node group (never a percentage):
	// with more unhealthy machines than this nothing is replaced. 0 counts as 1.
	RepairMaxUnhealthy int
	// WorkerFirewall puts the workers of every NEW cluster behind the Proxmox VM firewall with a security group of
	// their own (config.cluster.workerFirewall, docs/managed-k8s.md "Worker firewall"). Off by default: not verified
	// on a lab. An existing cluster is protected by the operator, one at a time. Nothing with Shared.
	WorkerFirewall bool
	// Shared, when set, puts every cluster's workers on one provider-run placement and credential
	// instead of the project's pool and vnet with tokens minted per cluster. For a site whose portal
	// token may not mint tokens. Workers then neither sync nor bill, and tenants share a network.
	Shared *SharedWorkers
}

// SharedWorkers is the operator-run placement of ClusterDefaults.Shared.
type SharedWorkers struct {
	Pool, Bridge, Gateway string
	Prefix                int
	First, Last           string // address range on the bridge
	CAPITokenID           string
	CAPISecret            string
	CSITokenID            string
	CSISecret             string
}

// ClusterAddons is the curated add-on menu a customer can toggle per cluster (name →
// default-enabled). Keys are the addons subchart's own top-level blocks, passed through as
// `addons.<key>.enabled`; everything else in the addons stack — the CNI, the CSI/credential
// push, the mirror-registry pins — stays operator territory and is never client-writable.
var ClusterAddons = map[string]bool{
	"certManager":      false, // cert-manager (Let's Encrypt & friends)
	GatewayFabricAddon: false, // NGINX Gateway Fabric / Gateway API; legacy ingress is preserved separately
	"metricsServer":    true,  // kubectl top / HPA metrics
}

// ClusterFQDN is the cluster's public API hostname (<clusterID>.<dnsZone>) — the name
// external-dns publishes for the API-server LoadBalancer and the apiserver cert carries as a
// SAN (BuildValues). "" when the provider has no DNS zone.
func (d ClusterDefaults) ClusterFQDN(clusterID string) string {
	if d.DNSZone == "" {
		return ""
	}
	return clusterID + "." + d.DNSZone
}

// ImageFor resolves the node image for (version, variant): the variant's own matrix entry, or
// the default version matrix when no variant is asked for. "" = no image offered for that
// combination — the caller decides whether that is an error (create) or a keep-current (edit).
func (d ClusterDefaults) ImageFor(version, variant string) string {
	if variant != "" {
		return d.ImageVariants[variant][version]
	}
	return d.Versions[version]
}

// VariantsForVersion lists the variant names that carry an image for the version, sorted — the
// client DTO's per-version picker feed.
func (d ClusterDefaults) VariantsForVersion(version string) []string {
	names := make([]string, 0, len(d.ImageVariants))
	for name, m := range d.ImageVariants {
		if m[version] != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// Validate rejects a config that cannot possibly provision (fail at create, not mid-flight).
func (c Config) Validate() error {
	switch {
	case c.Kubeconfig == "":
		return fmt.Errorf("kamaji provider: secret.kubeconfig is required")
	case c.ChartRepo == "":
		return fmt.Errorf("kamaji provider: config.argocd.chartRepo is required")
	case c.ChartName == "":
		return fmt.Errorf("kamaji provider: config.argocd.chartName is required")
	case c.ChartVersion == "":
		return fmt.Errorf("kamaji provider: config.argocd.chartVersion is required (never latest — plan §9)")
	}
	if err := c.NamespacePlacement.Validate(); err != nil {
		return fmt.Errorf("kamaji provider: config.namespacePlacement: %w", err)
	}
	return nil
}

// ClusterInfra is where one cluster's workers live on Proxmox, resolved by the create path from the
// project: its pool, its vnet, addresses reserved on that vnet, the storage classes of the compute
// provider. It rides in the Application values, so the sync and later actions never recompute it.
type ClusterInfra struct {
	Region         string
	Pool           string
	Storage        string // boot disks
	Bridge         string // the vnet
	Gateway        string
	Prefix         int
	Addresses      []string // worker addresses (IPAM)
	LBAddresses    []string // MetalLB inside the tenant cluster
	StorageClasses []StorageClass
	RWXServer      string // NFS server of the cluster's dataset ("" = no RWX class)
	RWXShare       string // its export path
	// CSIGateway runs the cluster's proxmox-csi controller on the services cluster against the CSI gateway, with no PVE
	// credential in the tenant cluster (CSIGatewaySecretName). False: the tenant-side controller of older clusters.
	CSIGateway bool
}

// StorageClass is one tenant-side proxmox-csi class.
type StorageClass struct {
	Name    string
	Storage string
}

// ClusterSpec is the desired state of ONE customer cluster — the values-builder input. The
// full generated values live on the Application (spec.source.helm.valuesObject); this struct
// is what the client API accepts and what actions mutate.
type ClusterSpec struct {
	ID          string // generated pc-c-<8hex>; Application/release/Cluster/TCP name
	DisplayName string // customer-typed, display-only
	ProjectID   string
	Version     string // k8s version, must be a key of Defaults.Versions
	HA          bool   // control-plane replicas 3 (true) or 1
	// OIDC is the customer-supplied issuer config (chart oidc.* block): issuerUrl, clientId,
	// usernameClaim, usernamePrefix, groupsClaim, groupsPrefix, signingAlgs. Empty issuerUrl = disabled.
	OIDC map[string]string
	// AllowedCIDRs restricts API-server LB ingress (LB source ranges — plan Phase 2a). Empty = open.
	AllowedCIDRs []string
	// Addons are the customer's picks off the curated ClusterAddons menu (unknown names are
	// rejected). nil/empty = the chart's own defaults, which match the menu's defaults.
	Addons     map[string]bool
	NodeGroups []NodeGroup
	// NetworkID attaches the cluster to a network the customer already owns; empty gets a dedicated
	// per-cluster network. Customer-supplied, so the create path MUST verify it belongs to the
	// project before building values — a crafted id would otherwise let a customer hang their nodes
	// onto infrastructure networks.
	NetworkID string
	// Infra is filled by the create path, never by the client.
	Infra ClusterInfra
}

// NodeGroup is one CAPI MachineDeployment-backed worker pool.
type NodeGroup struct {
	Name     string `json:"name"`
	FlavorID string `json:"flavorId"`
	ImageID  string `json:"imageId,omitempty"` // explicit override; else resolved from the version matrix
	// ImageVariant picks a curated image variant (Defaults.ImageVariants) for this pool — e.g.
	// "nvidia" for GPU nodes. Sticky across upgrades: the UPGRADE action re-resolves the group
	// onto the target version's image of the same variant. Empty = the default image.
	ImageVariant  string            `json:"imageVariant,omitempty"`
	Count         int               `json:"count"`
	Autoscale     bool              `json:"autoscale"`
	Min           int               `json:"min,omitempty"`
	Max           int               `json:"max,omitempty"`
	RootVolumeGiB int               `json:"rootVolumeGiB,omitempty"` // 0 = ClusterDefaults.RootVolumeGiB
	Labels        map[string]string `json:"labels,omitempty"`
	Taints        []string          `json:"taints,omitempty"` // "key=value:Effect" / "key:Effect"
	// Resolved from the flavor by the portal, never taken from the client: what the worker VMs get,
	// and the VM tags that make them sync and bill as servers of that flavor.
	Cores     int      `json:"-"`
	MemoryMiB int      `json:"-"`
	Tags      []string `json:"-"`
	// Placement is how many replicas each PVE node runs, decided by the portal from the nodes' free capacity. Never
	// taken from a request. Empty: the chart splits evenly.
	Placement map[string]int `json:"-"`
}

// ValidateCreate is Validate for a cluster about to be made: a provider that offers no version has no node image for
// any, and nothing may be reserved or minted for a cluster that could never get a node. (Validate alone lets an empty
// set through: the SET_NODE_GROUPS shape check passes none.)
func (s ClusterSpec) ValidateCreate(d ClusterDefaults) error {
	if len(d.Versions) == 0 {
		return fmt.Errorf("cluster: this provider offers no Kubernetes version yet. An operator must publish a Kubernetes node image " +
			"in Template updates and enter its version on the provider (cluster.versions)")
	}
	return s.Validate(d)
}

// Validate rejects an unbuildable spec.
func (s ClusterSpec) Validate(d ClusterDefaults) error {
	switch {
	case s.ID == "":
		return fmt.Errorf("cluster: id is required")
	case s.ProjectID == "":
		return fmt.Errorf("cluster: projectId is required")
	case s.Version == "":
		return fmt.Errorf("cluster: version is required")
	case len(s.NodeGroups) == 0:
		return fmt.Errorf("cluster: at least one node group is required")
	}
	if len(d.Versions) > 0 {
		if _, ok := d.Versions[s.Version]; !ok {
			return fmt.Errorf("cluster: version %q is not offered by this provider", s.Version)
		}
	}
	for name, enabled := range s.Addons {
		if name == LegacyIngressAddon && !enabled {
			continue // old clients send an explicit false pick; it cannot install the retired addon
		}
		if _, ok := ClusterAddons[name]; !ok {
			return fmt.Errorf("cluster: unknown add-on %q", name)
		}
	}
	// The list becomes loadBalancerSourceRanges of the Service Kamaji makes for the API server on the platform cluster,
	// where Kubernetes 1.36 refuses a CIDR with host bits: the cluster would never get its endpoint.
	if err := cloud.StrictCIDRs(s.AllowedCIDRs, nil); err != nil {
		return fmt.Errorf("cluster: allowedCidrs: %w", err)
	}
	seen := map[string]bool{}
	for _, ng := range s.NodeGroups {
		if ng.Name == "" || ng.FlavorID == "" {
			return fmt.Errorf("cluster: node group name and flavorId are required")
		}
		if len(d.Flavors) > 0 && !slices.Contains(d.Flavors, ng.FlavorID) {
			return fmt.Errorf("cluster: node group %q: flavor %q is not offered by this provider", ng.Name, ng.FlavorID)
		}
		// The chart derives MachineDeployment / template names from the group name and rejects
		// anything else outright (machine-deployment.yaml `fail`), so catch it here with a message
		// the customer can act on.
		if !nodeGroupName.MatchString(ng.Name) {
			return fmt.Errorf("cluster: node group %q: name must be 3+ lowercase letters, digits or dashes, starting with a letter", ng.Name)
		}
		if seen[ng.Name] {
			return fmt.Errorf("cluster: node group %q: duplicate name", ng.Name)
		}
		seen[ng.Name] = true
		// Variant must resolve for the cluster's version (skipped when d has no versions — the
		// SET_NODE_GROUPS shape check — where the values patch resolves images with fallback).
		if ng.ImageVariant != "" && len(d.Versions) > 0 && d.ImageFor(s.Version, ng.ImageVariant) == "" {
			return fmt.Errorf("cluster: node group %q: image variant %q is not offered for version %s", ng.Name, ng.ImageVariant, s.Version)
		}
		// ponytail: no cluster-autoscaler on Proxmox yet. The chart has one machine pool per PVE node,
		// so an autoscale group needs per-pool min/max. Fixed sizes until someone asks.
		if ng.Autoscale {
			return fmt.Errorf("cluster: node group %q: autoscaling is not available on this platform", ng.Name)
		}
		if ng.Count < 1 {
			return fmt.Errorf("cluster: node group %q: count must be >= 1", ng.Name)
		}
		// Every node gets an explicitly sized root disk (the images are bigger than most
		// flavors' disk), so a size has to come from somewhere.
		if ng.RootVolumeGiB < 0 {
			return fmt.Errorf("cluster: node group %q: rootVolumeGiB must not be negative", ng.Name)
		}
		if ng.RootVolumeGiB == 0 && d.RootVolumeGiB == 0 {
			return fmt.Errorf("cluster: node group %q: rootVolumeGiB is required (no provider default configured)", ng.Name)
		}
		for _, t := range ng.Taints {
			obj := taintToObject(t)
			if obj == nil {
				return fmt.Errorf("cluster: node group %q: taint %q must be key[=value]:Effect", ng.Name, t)
			}
			if effect, _ := obj["effect"].(string); !ValidTaintEffects[effect] {
				return fmt.Errorf("cluster: node group %q: taint %q: effect must be NoSchedule, PreferNoSchedule or NoExecute", ng.Name, t)
			}
		}
	}
	return nil
}

// nodeGroupName mirrors the chart's own guard (templates/node-group/machine-deployment.yaml).
var nodeGroupName = regexp.MustCompile(`^[a-z][a-z0-9\-]+[a-z0-9]$`)

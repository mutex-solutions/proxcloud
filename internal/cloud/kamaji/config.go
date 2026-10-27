// Package kamaji drives Managed Kubernetes clusters on a Kamaji management cluster: one
// ArgoCD Application per customer cluster (chart `openstack-kamaji-cluster` from our OCI
// registry, pinned targetRevision + full generated values), applied through the management
// cluster's own API (the Application CRD — no ArgoCD API/auth involved). ArgoCD renders,
// syncs and health-checks; proxcloud reads status back off the Application + the Kamaji
// TenantControlPlane + CAPI MachineDeployments.
package kamaji

import (
	"fmt"
	"regexp"
	"slices"
)

// Namespace / naming derivations — the ONE place these are derived (ceph RGWUIDFor precedent).
// Customer-typed names are display-only; every k8s-side identifier is the generated cluster id
// (`stc-<8 hex>`), so duplicate display names, unicode and the RFC1123/63-char limit are
// non-issues, and the API endpoint/DNS never changes on rename.

// NamespaceFor is the management-cluster namespace holding a project's clusters.
func NamespaceFor(projectID string) string { return "pc-" + projectID }

// CloudSecretName is the per-cluster clouds.yaml secret (management-cluster side ONLY — the
// customer cluster never sees it; see plan §4/D7).
func CloudSecretName(clusterID string) string { return clusterID + cloudSecretSuffix }

// cloudSecretSuffix lets FinalizeOrphans map a secret name back to its cluster id.
const cloudSecretSuffix = "-cloud-config"

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

// ServerGroupName is the nova server group backing one node group. Derived, never stored: the
// delete path finds a cluster's groups by listing and matching this prefix, so cleanup survives a
// lost Application or a half-written cache row (CloudSecretName precedent).
func ServerGroupName(clusterID, nodeGroup string) string { return clusterID + "-" + nodeGroup }

// ServerGroupPrefix matches every server group belonging to a cluster.
func ServerGroupPrefix(clusterID string) string { return clusterID + "-" }

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
	// Appcred annotations on the per-cluster clouds.yaml secret: the ONLY durable record of the
	// keystone application credential minted for the cluster (plan D4). FinalizeOrphans reads them
	// to revoke the credential once the ArgoCD delete cascade has finished with it.
	AnnotationAppCredID   = "proxcloud.io/appcred-id"
	AnnotationAppCredUser = "proxcloud.io/appcred-user"
	// AnnotationAppCredService records WHICH OpenStack externalService minted the credential, so
	// the service-level sweep can revoke it even after the project doc (and its service
	// bindings) is gone.
	AnnotationAppCredService = "proxcloud.io/appcred-service"
)

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
	ArgoProject   string // AppProject constraining sources/destinations (default "default")
	ChartRepo     string // OCI helm repo, e.g. "ghcr.io/mutex-solutions/charts" (no oci:// prefix in Argo repoURL)
	ChartName     string // default "openstack-kamaji-cluster"
	ChartVersion  string // pinned default for NEW clusters; existing clusters keep their own pin
	Defaults      ClusterDefaults
}

// ClusterDefaults are the per-provider chart value defaults every cluster inherits.
type ClusterDefaults struct {
	DataStoreName     string            // Kamaji DataStore (default "default")
	FloatingNetworkID string            // Octavia floating network for the API LB
	ExternalNetworkID string            // CAPO external network (clusterNetworking)
	DNSZone           string            // optional: API FQDN = <clusterID>.<DNSZone> (certSAN + external-dns)
	Versions          map[string]string // curated k8s version → DEFAULT Glance image id (the ONLY versions offered)
	// ImageVariants are curated alternative node images per version, keyed variant → version →
	// image id (e.g. "nvidia" → {"1.35.4": "<id>"} for the GPU build of the same release). A node
	// group picks a variant BY NAME; upgrades re-resolve the group onto the target version's image
	// of the SAME variant, so a GPU pool never silently rolls back onto the plain image.
	ImageVariants map[string]map[string]string
	Flavors       []string // optional node-flavor allowlist (empty = every tenant flavor)
	// RootVolumeGiB is the default worker root-disk size. Node images are larger than most flavors'
	// ephemeral disk, so booting from an explicitly sized Cinder volume is the norm, not an option —
	// without it nova rejects the machine with FlavorDiskSmallerThanImage.
	RootVolumeGiB int
	// SupportKeypairName is a nova keypair injected into EVERY node so support can SSH in when a
	// customer asks for hands-on troubleshooting. Empty = no key injected. Nova keypairs belong to
	// the authenticating user, so one keypair covers the whole provider.
	SupportKeypairName string
	// SupportKeypairPublicKey, when set, lets proxcloud create that keypair if it is missing.
	// Without it the operator owns creating the key and proxcloud only injects the name.
	SupportKeypairPublicKey string
	// AllowedCIDRs is the provider-wide default API-server ingress allowlist; a cluster's own
	// AllowedCIDRs overrides it. Empty on both = open.
	AllowedCIDRs []string
	// StorageVolumeType is the Cinder volume type behind every cluster's default StorageClass
	// (csi-cinder). Empty = the cloud's default volume type.
	StorageVolumeType string
}

// ClusterAddons is the curated add-on menu a customer can toggle per cluster (name →
// default-enabled). Keys are the addons subchart's own top-level blocks, passed through as
// `addons.<key>.enabled`; everything else in the addons stack — the CNI, the CSI/credential
// push, the mirror-registry pins — stays operator territory and is never client-writable.
var ClusterAddons = map[string]bool{
	"certManager":         false, // cert-manager (Let's Encrypt & friends)
	"ingress":             false, // NGINX ingress controller
	"metricsServer":       true,  // kubectl top / HPA metrics
	"monitoring":          false, // kube-prometheus-stack + Loki (heavy)
	"nodeProblemDetector": true,  // node faults (kernel hangs, bad disks) as Events; chart default is on
	"nvidiaGPUOperator":   false, // driver/toolkit for NVIDIA GPU pools
	"reloader":            false, // roll Deployments when watched ConfigMaps/Secrets change
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
	case c.ChartVersion == "":
		return fmt.Errorf("kamaji provider: config.argocd.chartVersion is required (never latest — plan §9)")
	}
	return nil
}

// ClusterSpec is the desired state of ONE customer cluster — the values-builder input. The
// full generated values live on the Application (spec.source.helm.valuesObject); this struct
// is what the client API accepts and what actions mutate.
type ClusterSpec struct {
	ID          string // generated stc-<8hex>; Application/release/Cluster/TCP name
	DisplayName string // customer-typed, display-only
	ProjectID   string
	Version     string // k8s version, must be a key of Defaults.Versions
	HA          bool   // control-plane replicas 3 (true) or 1
	// OIDC is the customer-supplied issuer config (chart oidc.* block): issuerUrl, clientId,
	// usernameClaim, usernamePrefix, groupsClaim, groupsPrefix, signingAlgs. Empty issuerUrl = disabled.
	OIDC map[string]string
	// AllowedCIDRs restricts API-server LB ingress (Octavia ACL — plan Phase 2a). Empty = open.
	AllowedCIDRs []string
	// Addons are the customer's picks off the curated ClusterAddons menu (unknown names are
	// rejected). nil/empty = the chart's own defaults, which match the menu's defaults.
	Addons     map[string]bool
	NodeGroups []NodeGroup
	// NetworkID/SubnetID attach the cluster to a network the customer already owns (EKS-style
	// "pick your VPC subnet") instead of the per-cluster network CAPO otherwise creates. Both or
	// neither: CAPO accepts a lone filter, but a network with the wrong subnet picked — or a
	// subnet resolved off a different network — is exactly the class of half-configured cluster
	// we refuse to build. Customer-supplied, but the create path MUST verify both belong to the
	// project's tenant before building values (kamajiValidateClusterNetwork): CAPO runs with a
	// project-scoped credential, yet shared/external networks are visible cross-tenant, and a
	// crafted id would otherwise let a customer hang their nodes onto infrastructure networks.
	NetworkID string
	SubnetID  string
	// ExternalNetworkID overrides ClusterDefaults.ExternalNetworkID for THIS cluster. ProxCloud-owned,
	// derived at create time (the external network the chosen subnet's router egresses through, via
	// the admin allowlist) — never accepted from the client. CAPO treats spec.externalNetwork as
	// immutable after create (pkg/webhooks/openstackcluster_webhook.go ValidateUpdate), so this is
	// pinned into the Application values once and never rewritten.
	ExternalNetworkID string
	// AppCredID/AppCredUserID/AppCredServiceID record the per-cluster keystone application
	// credential minted at create (plan D4) — stamped as annotations on the clouds.yaml secret
	// so the orphan sweep can revoke it after the delete cascade, even when the project doc is
	// already gone. Empty when minting was skipped (fallback admin auth).
	AppCredID        string
	AppCredUserID    string
	AppCredServiceID string
}

// NodeGroup is one CAPI MachineDeployment-backed worker pool.
type NodeGroup struct {
	Name     string `json:"name"`
	FlavorID string `json:"flavorId"`
	ImageID  string `json:"imageId,omitempty"` // explicit override; else resolved from the version matrix
	// ImageVariant picks a curated image variant (Defaults.ImageVariants) for this pool — e.g.
	// "nvidia" for GPU nodes. Sticky across upgrades: the UPGRADE action re-resolves the group
	// onto the target version's image of the same variant. Empty = the default image.
	ImageVariant string `json:"imageVariant,omitempty"`
	// AvailabilityZone pins the pool's machines to one Nova AZ (the chart's failureDomain).
	// Empty = the cloud scheduler places them (single-AZ clouds today).
	AvailabilityZone string `json:"availabilityZone,omitempty"`
	// PublicIP gives every machine in the pool a floating IP from the cluster's external
	// network (an OpenStackFloatingIPPool per group; released with the machine). The FIPs land
	// in the customer's tenant, so they sync and bill like any other floating IP.
	PublicIP      bool              `json:"publicIp,omitempty"`
	Count         int               `json:"count"`
	Autoscale     bool              `json:"autoscale"`
	Min           int               `json:"min,omitempty"`
	Max           int               `json:"max,omitempty"`
	RootVolumeGiB int               `json:"rootVolumeGiB,omitempty"` // 0 = ClusterDefaults.RootVolumeGiB
	Labels        map[string]string `json:"labels,omitempty"`
	Taints        []string          `json:"taints,omitempty"` // "key=value:Effect" / "key:Effect"
	// ServerGroupID is the nova server group the pool's machines join (soft anti-affinity, one per
	// node group). ProxCloud-owned: minted at create/update, never accepted from the client — hence
	// json:"-", so a crafted request cannot pin machines into somebody else's server group.
	ServerGroupID string `json:"-"`
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
	case (s.NetworkID == "") != (s.SubnetID == ""):
		return fmt.Errorf("cluster: networkId and subnetId must be set together (or both left empty for a dedicated cluster network)")
	case len(s.NodeGroups) == 0:
		return fmt.Errorf("cluster: at least one node group is required")
	}
	if len(d.Versions) > 0 {
		if _, ok := d.Versions[s.Version]; !ok {
			return fmt.Errorf("cluster: version %q is not offered by this provider", s.Version)
		}
	}
	for name := range s.Addons {
		if _, ok := ClusterAddons[name]; !ok {
			return fmt.Errorf("cluster: unknown add-on %q", name)
		}
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
		// Variant must resolve for the cluster's version (skipped when d is empty — the
		// SET_NODE_GROUPS shape check — where the values patch resolves images with fallback).
		if ng.ImageVariant != "" && len(d.Versions) > 0 && d.ImageFor(s.Version, ng.ImageVariant) == "" {
			return fmt.Errorf("cluster: node group %q: image variant %q is not offered for version %s", ng.Name, ng.ImageVariant, s.Version)
		}
		if ng.Autoscale && (ng.Min < 1 || ng.Max < ng.Min) {
			return fmt.Errorf("cluster: node group %q: autoscale needs 1 <= min <= max", ng.Name)
		}
		if !ng.Autoscale && ng.Count < 1 {
			return fmt.Errorf("cluster: node group %q: count must be >= 1", ng.Name)
		}
		// Nodes always boot from a Cinder volume (the images are bigger than most flavors'
		// ephemeral disk), so a size has to come from somewhere.
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

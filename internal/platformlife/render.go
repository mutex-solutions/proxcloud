package platformlife

// render.go: what the platform's clusters are given, as pure functions of the BOM and one installation (P3 design §4.2
// stages 6 to 10, §4.4): the workload cluster's chart values, the seed ArgoCD (its HelmChart, its AppProjects and
// repository entries, one Application per BOM component), the two Secrets those Applications name, and the provider
// bodies stage 10 registers. Values go inline and secrets are referenced by name: no Application carries a secret.

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mutex-solutions/proxcloud/deploy"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
)

// RenderInput is what the renderers read of one installation: the parts of the platform installation model (design
// §3.2) they need, plus what earlier stages found. It holds no secret; the renderers that write one take it apart.
type RenderInput struct {
	ID          string // pi-<8hex>
	Name        string // the providers are "<Name> Kubernetes" and "<Name> Databases"
	Region      string // the Proxmox provider's region, and the nodes' topology region
	RegionName  string
	PVEAPIURL   string // the PVE API as the platform network reaches it, https://host:8006
	BootStorage string // boot disks and the std StorageClass
	Network     RenderNetwork
	SeedAddress string
	SSHKeys     []string // break-glass authorized_keys lines for the seed
	Cluster     RenderCluster
	Templates   map[string]int // the node image's template VMID per PVE node (stage 2)
	WorkerMTU   int            // the tenant vnets' MTU: the Kubernetes provider's workerMtu (0 = the guest default)
	// NodeImageLine is the Kubernetes provider's versions value for the BOM's Kubernetes version (NodeImageLine);
	// "" = no node image is published on every node, and the provider is registered with no version of the run's.
	NodeImageLine string
	// ArgoCD is spec.access.argocd: the address of the platform cluster's ArgoCD (nil = none).
	ArgoCD *ArgoCDAccess
}

// RenderNetwork is the platform network the seed and the workload VMs share. VLAN tags every NIC on Bridge (0 = none).
type RenderNetwork struct {
	Bridge, Gateway string
	VLAN            int
	Prefix          int
	DNS             []string
	MTU             int
}

// RenderCluster is the shared workload cluster.
type RenderCluster struct {
	Endpoint     string   // the API VIP kube-vip holds
	NodeRange    []string // first-last ranges CAPI IPAM gives the VMs
	MetalLBRange string   // first-last, pool services
	ControlPlane RenderMachine
	// ControlPlaneReplicas is the number of control plane machines, all on ControlPlane.Node; 0 and 1 are one machine.
	ControlPlaneReplicas int
	NodeGroups           []RenderNodeGroup
	// Roll orders the replacement of the machines on an upgrade that changes the Kubernetes version or the node image.
	// nil: every machine at the BOM's version and at Templates, as on every other run.
	Roll *Roll
}

// Roll is one step of a roll. The units up to Step (rollOrder; 1 is the control plane) render at the BOM's Kubernetes
// version, kube-vip and Templates; every later unit at FromVersion and FromTemplates, with FromKubeVip for the control
// plane. Step 0 renders everything as it was. DeleteFirst are the units ("group/node") whose MachineDeployment
// deletes a machine before it makes the new one.
type Roll struct {
	Step                     int
	FromVersion, FromKubeVip string
	FromTemplates            map[string]int
	DeleteFirst              []string
}

// rollOrder is the order a roll replaces the MachineDeployments in, as "group/node": the groups system, kamaji, dbaas,
// then the others by name; inside a group its nodes by name. The control plane comes before all of them.
func rollOrder(groups map[string][]string) []string {
	names := slices.SortedFunc(maps.Keys(groups), func(a, b string) int {
		rank := func(n string) int {
			if i := slices.Index(NodeGroupNames, n); i >= 0 {
				return i
			}
			return len(NodeGroupNames)
		}
		return cmp.Or(rank(a)-rank(b), strings.Compare(a, b))
	})
	var out []string
	for _, g := range names {
		for _, n := range slices.Sorted(slices.Values(groups[g])) {
			out = append(out, g+"/"+n)
		}
	}
	return out
}

// RenderMachine is the size of a VM and its PVE node.
type RenderMachine struct {
	Node                      string
	Cores, MemoryMiB, DiskGiB int
}

// RenderNodeGroup is one node group: Count machines split across Nodes in order, as the chart splits machineCount.
type RenderNodeGroup struct {
	Name                      string
	Nodes                     []string
	Count                     int
	Cores, MemoryMiB, DiskGiB int
}

// PVEToken is a pasted PVE API token.
type PVEToken struct{ ID, Secret string }

const (
	// ClusterName is the workload cluster: the seed component that builds it, its CAPI Cluster and its namespace.
	ClusterName = "pc-platform"
	// PVECredentialsSecretName is the seed Secret CAPMOX clones the workload VMs with (the CAPI token), in ClusterName.
	PVECredentialsSecretName = "pc-platform-pve"
	// CSIConfigSecretName is the workload's proxmox-csi config Secret (the CSI token), in the proxmox-csi namespace.
	CSIConfigSecretName = "proxmox-csi-plugin"
	// GroupLabel places a pod on a node group. CAPI copies it from the Machine, and a kubelet cannot set it.
	GroupLabel = "node-restriction.kubernetes.io/proxcloud-node-group"
	// GroupTaintKey keeps other pods off every group but GroupSystem.
	GroupTaintKey = "proxcloud.io/node-group"
	GroupSystem   = "system"
	// ManagedBy marks every object the renderers write.
	ManagedBy     = "proxcloud-platform"
	argoNamespace = "argocd"
	argoServer    = "argocd-server" // the Service of chart argo-cd under release name argocd
	// argoAddressAnnotation asks MetalLB for one address of its pool; a Service that asks for a taken one stays pending.
	argoAddressAnnotation = "metallb.io/loadBalancerIPs"
	argoProject           = "proxcloud-platform"
	inCluster             = "https://kubernetes.default.svc"
	csiNamespace          = "csi-proxmox"
	defaultDatastore      = "default"
)

// WorkloadServer is the workload cluster's API as the seed ArgoCD's cluster Secret names it, and every workload
// Application's destination.
func WorkloadServer(endpoint string) string { return "https://" + endpoint + ":6443" }

func machineTags(id string) []any { return []any{"proxcloud", "pc-platform", "pc-platform-" + id} }

func groupTaint(group string) []any {
	if group == GroupSystem {
		return nil
	}
	return []any{map[string]any{"key": GroupTaintKey, "value": group, "effect": "NoSchedule"}}
}

// groupPlacement is the namespace placement of a group: PodNodeSelector and PodTolerationRestriction, which the kubeadm
// control plane enables, put every pod of the namespace there.
func groupPlacement(group string) kamajik8s.NamespacePlacement {
	p := kamajik8s.NamespacePlacement{NodeSelector: map[string]string{GroupLabel: group}}
	if group != GroupSystem {
		p.Tolerations = []map[string]any{{"key": GroupTaintKey, "operator": "Equal", "value": group, "effect": "NoSchedule"}}
	}
	return p
}

// placementValues is a group's placement as chart values, {nodeSelector, tolerations}: what a pod-creating chart on a
// tainted group sets itself, with the namespace annotations only a backstop (design §4.4), and the providers'
// namespacePlacement.
func placementValues(group string) map[string]any {
	p := groupPlacement(group)
	nodeSelector := map[string]any{}
	for k, v := range p.NodeSelector {
		nodeSelector[k] = v
	}
	tolerations := []any{}
	for _, t := range p.Tolerations {
		tolerations = append(tolerations, t)
	}
	return map[string]any{"nodeSelector": nodeSelector, "tolerations": tolerations}
}

// ClusterChartVersion is the version of the chart b builds the workload cluster from ("" when b names none).
func ClusterChartVersion(b *BOM) string {
	for _, c := range b.Components {
		if c.Cluster == ClusterSeed && c.Name == ClusterName {
			return c.Chart.Version
		}
	}
	return ""
}

// ClusterValues are the proxmox-kamaji-cluster values of the workload cluster: controlPlane.kind kubeadm, its control
// plane VMs on one node, the node groups in pool proxcloud-system, each group labelled and every group but system tainted.
func ClusterValues(b *BOM, in RenderInput) (map[string]any, error) {
	roll := in.Cluster.Roll
	var order []string // the units of a roll after the control plane, "group/node"
	if roll != nil {
		if v := ClusterChartVersion(b); !kamaji.ChartHasRollOrder(v) {
			return nil, fmt.Errorf("platform: the BOM of release %s names cluster chart %s, which cannot order a rollout: it needs chart %s or newer", b.Release, v, kamaji.RollOrderChartVersion)
		}
		by := map[string][]string{}
		for _, g := range in.Cluster.NodeGroups {
			by[g.Name] = g.Nodes
		}
		order = rollOrder(by)
	}
	// reached: the unit renders at the BOM's version and template. Without a roll every unit does.
	reached := func(unit string) bool {
		return roll == nil || (unit == "" && roll.Step >= 1) || (unit != "" && slices.Index(order, unit)+2 <= roll.Step)
	}
	pins := func(group string, nodes []string) (string, error) {
		out := make([]string, 0, len(nodes))
		for _, n := range nodes {
			unit, from := "", in.Templates
			if group != "" {
				unit = group + "/" + n
			}
			if !reached(unit) {
				from = roll.FromTemplates
			}
			vmid, ok := from[n]
			if !ok {
				return "", fmt.Errorf("platform: no node image template on %s", n)
			}
			out = append(out, n+"="+strconv.Itoa(vmid))
		}
		return strings.Join(out, ","), nil
	}
	cp := in.Cluster.ControlPlane
	cpPin, err := pins("", []string{cp.Node})
	if err != nil {
		return nil, err
	}
	version, kubeVip := b.Kubernetes.Version, b.Images.KubeVip
	if !reached("") {
		version, kubeVip = roll.FromVersion, roll.FromKubeVip
	}
	groups := []any{}
	for _, g := range in.Cluster.NodeGroups {
		pin, err := pins(g.Name, g.Nodes)
		if err != nil {
			return nil, err
		}
		old, first := map[string]any{}, []any{}
		for _, n := range g.Nodes {
			if !reached(g.Name+"/"+n) && roll.FromVersion != version {
				old[n] = roll.FromVersion
			}
			if roll != nil && slices.Contains(roll.DeleteFirst, g.Name+"/"+n) {
				first = append(first, n)
			}
		}
		ng := map[string]any{
			"name": g.Name, "machineImageId": pin, "machineCount": g.Count,
			"machineCores": g.Cores, "machineMemoryMiB": g.MemoryMiB, "machineRootVolume": map[string]any{"diskSize": g.DiskGiB},
			"machineTags": machineTags(in.ID), "machineLabels": map[string]any{GroupLabel: g.Name},
		}
		if t := groupTaint(g.Name); t != nil {
			ng["taints"] = t
		}
		// Written only when there is something to say, so a render without a roll is the one it always was.
		if len(old) > 0 {
			ng["kubernetesVersions"] = old
		}
		if len(first) > 0 {
			ng["deleteFirstOn"] = first
		}
		groups = append(groups, ng)
	}
	dns := make([]any, len(in.Network.DNS))
	for i, d := range in.Network.DNS {
		dns[i] = d
	}
	nodeRange := make([]any, len(in.Cluster.NodeRange))
	for i, r := range in.Cluster.NodeRange {
		nodeRange[i] = r
	}
	network := map[string]any{
		"bridge": in.Network.Bridge, "gateway": in.Network.Gateway, "prefix": in.Network.Prefix,
		"addresses": nodeRange, "dnsServers": dns, "mtu": in.Network.MTU,
	}
	// The key is written only with a tag, so a platform without one renders as before under every BOM. An older chart
	// ignores the key and would put the machines on the bridge untagged: refused, not rendered.
	if in.Network.VLAN > 0 {
		if v := ClusterChartVersion(b); !kamaji.ChartHasVLAN(v) {
			return nil, fmt.Errorf("platform: the BOM of release %s names cluster chart %s, which cannot tag a NIC: a VLAN tag needs chart %s or newer", b.Release, v, kamaji.VLANChartVersion)
		}
		network["vlan"] = in.Network.VLAN
	}
	kubeadm := map[string]any{
		"endpoint": in.Cluster.Endpoint, "kubeVipImage": kubeVip, "machineImageId": cpPin,
		"machineCores": cp.Cores, "machineMemoryMiB": cp.MemoryMiB, "machineRootVolume": map[string]any{"diskSize": cp.DiskGiB},
		"machineTags": machineTags(in.ID),
	}
	// As the tag: the key is written only for more than one machine, so a platform with one renders as before under
	// every BOM, and an older chart, which would build one machine and say nothing, is refused.
	if n := in.Cluster.ControlPlaneReplicas; n > 1 {
		if v := ClusterChartVersion(b); !kamaji.ChartHasControlPlaneReplicas(v) {
			return nil, fmt.Errorf("platform: the BOM of release %s names cluster chart %s, which builds one control plane machine: %d need chart %s or newer", b.Release, v, n, kamaji.ControlPlaneReplicasChartVersion)
		}
		kubeadm["replicas"] = n
	}
	return map[string]any{
		"kubernetesVersion":          version,
		"cloudCredentialsSecretName": PVECredentialsSecretName,
		// The installer's services cluster ranges (services-cluster.yml.j2).
		"clusterNetwork": map[string]any{"pods": "10.244.0.0/16", "services": "10.96.0.0/16"},
		"controlPlane":   map[string]any{"kind": "kubeadm", "kubeadm": kubeadm},
		"proxmox": map[string]any{
			"region": in.Region, "pool": "proxcloud-system", "storage": in.BootStorage,
			"network": network,
		},
		"nodeGroups": groups,
	}, nil
}

// SeedArgoCD is the seed's ArgoCD: the one component outside an Application, a k3s HelmChart the run creates through
// the API (never a file in the manifests directory, which k3s reapplies at every start).
func SeedArgoCD(b *BOM, in RenderInput) (map[string]any, error) {
	for _, c := range b.Components {
		if c.Install != InstallHelmChart {
			continue
		}
		values, err := jsonText(c.Values)
		if err != nil {
			return nil, err
		}
		spec := map[string]any{"version": c.Chart.Version, "targetNamespace": c.Namespace, "createNamespace": true, "valuesContent": values}
		if strings.HasPrefix(c.Chart.Repo, "https://") {
			spec["repo"], spec["chart"] = c.Chart.Repo, c.Chart.Name
		} else {
			spec["chart"] = "oci://" + c.Chart.Repo + "/" + c.Chart.Name
		}
		obj := object("helm.cattle.io/v1", "HelmChart", "kube-system", c.Name)
		obj["metadata"].(map[string]any)["labels"] = labels(b, in, c.Name)
		obj["spec"] = spec
		return obj, nil
	}
	return nil, errors.New("platform: the BOM has no helmchart component")
}

// SeedProjects are the seed ArgoCD's project and repositories: AppProject proxcloud-platform, which may source only the
// BOM's charts (design §9) and deploy only to the seed and the workload cluster, in any namespace there: ArgoCD checks
// every object a chart renders, and charts write outside their release namespace (cert-manager's leader-election Roles
// in kube-system, cilium's in cilium-secrets). The default project locked; one repository entry per OCI repository,
// without credentials (the packages are public).
func SeedProjects(b *BOM, in RenderInput) []map[string]any {
	var repos, dests []any
	seen := map[string]bool{}
	for _, c := range b.Components {
		if c.Install != InstallApplication {
			continue
		}
		if !seen["repo "+c.Chart.Repo] {
			seen["repo "+c.Chart.Repo] = true
			repos = append(repos, c.Chart.Repo)
		}
		server := destination(c.Cluster, in)
		if !seen[server] {
			seen[server] = true
			dests = append(dests, map[string]any{"server": server, "namespace": "*"})
		}
	}
	project := object("argoproj.io/v1alpha1", "AppProject", argoNamespace, argoProject)
	project["metadata"].(map[string]any)["labels"] = labels(b, in, "")
	project["spec"] = map[string]any{
		"description":              "The managed-services platform's own components, from the BOM of ProxCloud " + b.Release + ".",
		"sourceRepos":              repos,
		"destinations":             dests,
		"clusterResourceWhitelist": []any{map[string]any{"group": "*", "kind": "*"}},
	}
	locked := object("argoproj.io/v1alpha1", "AppProject", argoNamespace, "default")
	locked["spec"] = map[string]any{"sourceRepos": []any{}, "destinations": []any{}, "clusterResourceWhitelist": []any{}}
	out := []map[string]any{project, locked}
	for _, r := range repos {
		repo := r.(string)
		if strings.Contains(repo, "://") {
			continue // an https repository needs no entry
		}
		name := "pc-repo-" + strings.Trim(nonLabel.ReplaceAllString(repo, "-"), "-")
		l := labels(b, in, "")
		l["argocd.argoproj.io/secret-type"] = "repository"
		s := object("v1", "Secret", argoNamespace, name)
		s["metadata"].(map[string]any)["labels"] = l
		s["type"] = "Opaque"
		s["stringData"] = map[string]any{"type": "helm", "name": name, "url": repo, "enableOCI": "true"}
		out = append(out, s)
	}
	return out
}

var nonLabel = regexp.MustCompile(`[^a-z0-9]+`)

// Applications are the seed ArgoCD's Applications, one per BOM component installed as one, in BOM order. Each carries
// the BOM's values with the run's on top; a key both set differently is an error. Syncs are automated and self-heal;
// a stateful component is never pruned, and no Application has the resources finalizer, so deleting one deletes
// nothing it made. Every Application retries a failed sync: the clusterctl Sync hooks of capi and capi-tenant can fail
// on a young API server, and a hook's outcome shows in neither sync nor health (the run reads the inventory).
func Applications(b *BOM, in RenderInput) ([]map[string]any, error) {
	var apps []map[string]any
	ownsMetadata := map[string]bool{}
	for _, c := range b.Components {
		if c.Install != InstallApplication {
			continue
		}
		p, err := componentPart(b, c, in)
		if err != nil {
			return nil, err
		}
		values, err := mergeValues(c.Values, p.values)
		if err != nil {
			return nil, fmt.Errorf("platform: %s/%s: %w", c.Cluster, c.Name, err)
		}
		helm := map[string]any{"releaseName": c.Name} // the installer's release names, which charts derive object names from
		if len(values) > 0 {
			helm["valuesObject"] = values
		}
		if p.skipCRDs {
			helm["skipCrds"] = true
		}
		server := destination(c.Cluster, in)
		sync := map[string]any{
			"automated": map[string]any{"prune": !c.Stateful, "selfHeal": true},
			// CreateNamespace: capi-tenant syncs into proxcloud-system before the platform component creates it.
			"syncOptions": []any{"CreateNamespace=true", "ServerSideApply=true"},
			"retry":       map[string]any{"limit": 10, "backoff": map[string]any{"duration": "30s", "factor": 2, "maxDuration": "5m"}},
		}
		// One Application owns a namespace's metadata (ArgoCD lets only one): the first in BOM order of a namespace whose
		// pods all go to one group. A backstop: it acts only on pods created after it, so a chart on a tainted group also
		// places its pods through its values.
		if p.pin != "" && !ownsMetadata[server+" "+c.Namespace] {
			ownsMetadata[server+" "+c.Namespace] = true
			annotations := map[string]any{}
			for k, v := range groupPlacement(p.pin).Annotations() {
				annotations[k] = v
			}
			sync["managedNamespaceMetadata"] = map[string]any{"annotations": annotations}
		}
		spec := map[string]any{
			"project":     argoProject,
			"source":      map[string]any{"repoURL": c.Chart.Repo, "chart": c.Chart.Name, "targetRevision": c.Chart.Version, "helm": helm},
			"destination": map[string]any{"server": server, "namespace": c.Namespace},
			"syncPolicy":  sync,
		}
		if p.ignore != nil {
			spec["ignoreDifferences"] = p.ignore
		}
		name := c.Name
		if c.Cluster == ClusterWorkload {
			name = ClusterName + "-" + c.Name
		}
		app := object("argoproj.io/v1alpha1", "Application", argoNamespace, name)
		app["metadata"].(map[string]any)["labels"] = labels(b, in, c.Name)
		app["spec"] = spec
		apps = append(apps, app)
	}
	return apps, nil
}

func destination(cluster string, in RenderInput) string {
	if cluster == ClusterSeed {
		return inCluster
	}
	return WorkloadServer(in.Cluster.Endpoint)
}

func labels(b *BOM, in RenderInput, component string) map[string]any {
	l := map[string]any{
		"app.kubernetes.io/managed-by":       ManagedBy,
		"proxcloud.io/platform-installation": in.ID,
		"proxcloud.io/bom-release":           b.Release,
	}
	if component != "" {
		l["proxcloud.io/component"] = component
	}
	return l
}

// part is what the run adds to a component: values on top of the BOM's, the group a namespace-wide placement pins
// ("" = none: the namespace exists already, another chart owns it, or a DaemonSet in it runs on every group),
// ignoreDifferences, and skipCRDs for a chart whose crds/ directory another component renders (ArgoCD renders crds/
// unless told not to, and two Applications that own one CRD take it from each other at every self-heal).
type part struct {
	values   map[string]any
	pin      string
	ignore   []any
	skipCRDs bool
}

// componentPart holds what each chart needs from a site, after services-platform.sh (installer stage 50). A component
// the BOM gains is an error here until it is given its part.
func componentPart(b *BOM, c Component, in RenderInput) (part, error) {
	group := c.Group
	if group == "" {
		group = GroupSystem
	}
	onGroup := map[string]any{GroupLabel: group}
	allGroups := []any{map[string]any{"key": GroupTaintKey, "operator": "Exists", "effect": "NoSchedule"}}
	switch c.Cluster + "/" + c.Name {
	case "seed/cert-manager":
		return part{}, nil
	case "seed/capi":
		return part{values: clusterctlValues(b, b.CAPI.Seed, nil)}, nil
	case "seed/" + ClusterName:
		v, err := ClusterValues(b, in)
		return part{values: v}, err
	case "workload/cilium":
		return part{values: map[string]any{"operator": map[string]any{"replicas": 1}, "hubble": map[string]any{"enabled": false},
			"ipam": map[string]any{"mode": "kubernetes"}}}, nil
	case "workload/cert-manager":
		return part{values: map[string]any{"crds": map[string]any{"enabled": true}}, pin: group}, nil
	case "workload/metallb":
		return part{values: map[string]any{
			"speaker":    map[string]any{"frr": map[string]any{"enabled": false}, "tolerations": allGroups},
			"frrk8s":     map[string]any{"enabled": false},
			"controller": map[string]any{"nodeSelector": onGroup},
		}}, nil
	case "workload/argocd":
		health, err := tenantHealthChecks()
		if err != nil {
			return part{}, err
		}
		v := map[string]any{
			"dex": map[string]any{"enabled": false}, "notifications": map[string]any{"enabled": false},
			"applicationSet": map[string]any{"replicas": 0}, // the chart has no applicationSet.enabled
			// cm: the health of what the tenant Applications render, which the providers read their status from.
			"configs": map[string]any{"params": map[string]any{"server.insecure": true}, "cm": health},
		}
		// With an address (spec.access.argocd) the Service is MetalLB's, for the operator networks only, and
		// argocd-server serves HTTPS with the certificate it generates: server.insecure is left out, not set false.
		// The three keys are those of chart argo-cd 10.14.0 (server.service.type, .annotations,
		// .loadBalancerSourceRanges); the annotation is MetalLB's request for one address.
		if a := in.ArgoCD; a != nil {
			sources := make([]any, len(a.Sources))
			for i, src := range a.Sources {
				sources[i] = src
			}
			v["configs"] = map[string]any{"cm": health}
			v["server"] = map[string]any{"service": map[string]any{"type": "LoadBalancer",
				"annotations": map[string]any{argoAddressAnnotation: a.Address}, "loadBalancerSourceRanges": sources}}
		}
		return part{values: v, pin: group}, nil
	case "workload/cnpg":
		return part{pin: group}, nil
	case "workload/proxmox-csi":
		return part{values: map[string]any{
			"existingConfigSecret": CSIConfigSecretName,
			"storageClass": []any{map[string]any{"name": "std", "storage": in.BootStorage, "fstype": "ext4", "reclaimPolicy": "Delete",
				"annotations": map[string]any{"storageclass.kubernetes.io/is-default-class": "true"}}},
			"node":         map[string]any{"tolerations": []any{map[string]any{"operator": "Exists"}}},
			"nodeSelector": onGroup,
		}}, nil
	case "workload/capi-tenant":
		return part{values: clusterctlValues(b, b.CAPI.Workload, onGroup)}, nil
	case "workload/kamaji-crds":
		return part{pin: group}, nil
	case "workload/kamaji-etcd":
		// The StatefulSet and the setup and teardown hook Jobs: kamaji-crds, which owns the namespace's metadata, syncs in
		// the same wave and may create kamaji-system after them.
		v := placementValues(group)
		v["replicas"], v["datastore"], v["jobs"] = 1, map[string]any{"enabled": true, "name": defaultDatastore}, placementValues(group)
		return part{values: v, pin: group}, nil
	case "workload/kamaji":
		v := placementValues(group)
		v["kamaji-etcd"], v["defaultDatastoreName"] = map[string]any{"deploy": false}, defaultDatastore
		// The chart's crds/ holds the CRDs kamaji-crds renders; helm install skips existing ones, ArgoCD does not.
		return part{values: v, pin: group, skipCRDs: true}, nil
	case "workload/platform":
		return part{
			values: map[string]any{"platform": map[string]any{"enabled": true, "chartRepository": b.TenantCharts.Kubernetes.Repo,
				"metallb": map[string]any{"addresses": []any{in.Cluster.MetalLBRange}}}},
			// The token controller fills these in.
			ignore: []any{map[string]any{"kind": "Secret", "name": "proxcloud-token", "namespace": "proxcloud-system",
				"jsonPointers": []any{"/data", "/metadata/annotations/kubernetes.io~1service-account.uid"}}},
		}, nil
	}
	return part{}, fmt.Errorf("platform: no values for BOM component %s/%s", c.Cluster, c.Name)
}

// clusterctlValues are the proxcloud-platform chart's clusterctl part: the BOM's image and one cluster's providers.
func clusterctlValues(b *BOM, p Providers, nodeSelector map[string]any) map[string]any {
	providers := map[string]any{}
	for k, v := range map[string]string{"core": p.Core, "bootstrap": p.Bootstrap, "controlPlane": p.ControlPlane,
		"infrastructure": p.Infrastructure, "ipam": p.IPAM, "addon": p.Addon} {
		if v != "" {
			providers[k] = v
		}
	}
	cc := map[string]any{"enabled": true, "image": b.Images.Clusterctl, "providers": providers}
	if p.Addon != "" {
		cc["clusterResourceSet"] = true // the Helm add-on provider (CAAPH) needs the ClusterResourceSet feature
	}
	if nodeSelector != nil {
		cc["nodeSelector"] = nodeSelector
	}
	return map[string]any{"clusterctl": cc}
}

// tenantHealthChecks are the data keys of both argocd-health.yaml files, which installer stage 50 patches into
// argocd-cm. A key in both is an error: one would hide the other.
func tenantHealthChecks() (map[string]any, error) {
	out := map[string]any{}
	for _, raw := range [][]byte{deploy.MgmtArgoCDHealth, deploy.DbaasArgoCDHealth} {
		var cm struct {
			Data map[string]string `yaml:"data"`
		}
		if err := yaml.Unmarshal(raw, &cm); err != nil {
			return nil, err
		}
		for k, v := range cm.Data {
			if _, dup := out[k]; dup {
				return nil, fmt.Errorf("platform: health check %s is in both argocd-health.yaml files", k)
			}
			out[k] = v
		}
	}
	return out, nil
}

// mergeValues lays over on base. A key both set to different leaves is an error: the BOM and the run must not disagree
// silently.
func mergeValues(base, over map[string]any) (map[string]any, error) {
	out := maps.Clone(base)
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range over {
		old, ok := out[k]
		if !ok {
			out[k] = v
			continue
		}
		om, ok1 := old.(map[string]any)
		vm, ok2 := v.(map[string]any)
		if ok1 && ok2 {
			m, err := mergeValues(om, vm)
			if err != nil {
				return nil, fmt.Errorf("%s.%w", k, err)
			}
			out[k] = m
			continue
		}
		if !reflect.DeepEqual(old, v) {
			return nil, fmt.Errorf("%s: the BOM says %v, the run %v", k, old, v)
		}
	}
	return out, nil
}

// PVECredentialsSecret is the seed's pc-platform/pc-platform-pve: the CAPI token CAPMOX clones the workload with, in
// the shape of installer stage 50's pc-services-pve.
func PVECredentialsSecret(b *BOM, in RenderInput, capi PVEToken) map[string]any {
	s := object("v1", "Secret", ClusterName, PVECredentialsSecretName)
	s["metadata"].(map[string]any)["labels"] = labels(b, in, "")
	s["type"] = "Opaque"
	s["stringData"] = map[string]any{"url": in.PVEAPIURL, "token": capi.ID, "secret": capi.Secret}
	return s
}

// CSIConfigSecret is the workload's csi-proxmox/proxmox-csi-plugin: the CSI token's config, as services-platform.sh
// writes it (insecure TLS to PVE, design §9).
func CSIConfigSecret(b *BOM, in RenderInput, csi PVEToken) (map[string]any, error) {
	config, err := jsonText(map[string]any{
		"features": map[string]any{"provider": "capmox"},
		"clusters": []any{map[string]any{
			"url": strings.TrimSuffix(in.PVEAPIURL, "/") + "/api2/json", "insecure": true,
			"token_id": csi.ID, "token_secret": csi.Secret, "region": in.Region,
		}},
	})
	if err != nil {
		return nil, err
	}
	s := object("v1", "Secret", csiNamespace, CSIConfigSecretName)
	s["metadata"].(map[string]any)["labels"] = labels(b, in, "")
	s["type"] = "Opaque"
	s["stringData"] = map[string]any{"config.yaml": config}
	return s, nil
}

// ProviderBodies are the POST /admin/service bodies stage 10 registers, stage 60's loop with the design's changes
// (§4.2): the BOM's tenant charts, the BOM's Kubernetes version when its node image is published (in.NodeImageLine;
// none otherwise), namespace placement on the group, the database egress
// fence, no backup store, PostgreSQL replicas 3 only with three dbaas nodes, held back from new projects, and the
// marker that adopts a row at the next run. kubeconfig is the workload's proxcloud service account (provider secret).
func ProviderBodies(b *BOM, in RenderInput, kubeconfig string) (kubernetes, database map[string]any) {
	regions := map[string]any{in.Region: map[string]any{"displayName": in.RegionName}}
	argo := func(project string, c Chart) map[string]any {
		return map[string]any{"namespace": argoNamespace, "project": project, "chartRepo": c.Repo, "chartName": c.Name, "chartVersion": c.Version}
	}
	dbNodes := map[string]bool{}
	for _, g := range in.Cluster.NodeGroups {
		if g.Name == "dbaas" {
			for _, n := range g.Nodes {
				dbNodes[n] = true
			}
		}
	}
	replicas := []any{1}
	if len(dbNodes) >= 3 {
		replicas = []any{1, 3}
	}
	versions := map[string]any{}
	if in.NodeImageLine != "" {
		versions[b.Kubernetes.Version] = in.NodeImageLine
	}
	kubernetes = map[string]any{
		"name": in.Name + " Kubernetes", "type": "CLOUD", "status": "PUBLIC",
		"config": map[string]any{
			"provider": "kamaji", "region": in.Region, "regions": regions,
			"services": map[string]any{"kubernetes": map[string]any{in.Region: true}},
			"argocd":   argo("proxcloud-k8s", b.TenantCharts.Kubernetes),
			"cluster": map[string]any{
				"dataStoreName": defaultDatastore, "versions": versions, "rootVolumeGiB": 30,
				"workerMtu": in.WorkerMTU, "rwxGiB": 0, "pveApiUrl": in.PVEAPIURL,
			},
			"namespacePlacement":     placementValues("kamaji"),
			"attachToNewProjects":    false,
			"platformInstallationId": in.ID,
		},
		"secret": map[string]any{"kubeconfig": kubeconfig},
	}
	database = map[string]any{
		"name": in.Name + " Databases", "type": "CLOUD", "status": "PUBLIC",
		"config": map[string]any{
			"provider": "dbaas", "region": in.Region, "regions": regions,
			"services": map[string]any{"database": map[string]any{in.Region: true}},
			"argocd":   argo("proxcloud-dbaas", b.TenantCharts.Database),
			"database": map[string]any{
				"lbAddressPool": "services", "storageClasses": []any{"std"}, "egressFence": true,
				"engines": map[string]any{"postgresql": map[string]any{"versions": []any{"17", "18"}, "default": "18", "replicas": replicas}},
			},
			"namespacePlacement":     placementValues("dbaas"),
			"attachToNewProjects":    false,
			"platformInstallationId": in.ID,
		},
		"secret": map[string]any{"kubeconfig": kubeconfig},
	}
	return kubernetes, database
}

package kamaji

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// values.go is the SINGLE place the kamaji cluster chart values contract lives. Node groups use
// the machine* spelling, and a chart `fail`s the render rather than defaulting when a key is
// missing — so a typo here is a create that never provisions. TestBuildValues pins every key;
// keep it that way.
//
// Fix chart drift HERE only; nothing else in proxcloud knows chart keys.

// BuildValues renders the full chart values for a cluster. Full values (not a delta) by design:
// chart-default changes must never silently mutate a customer cluster (plan §9).
func BuildValues(cfg Config, spec ClusterSpec) map[string]any {
	d := cfg.Defaults

	cp := map[string]any{
		"replicas": 1,
	}
	if spec.HA {
		cp["replicas"] = 3
	}
	if d.DataStoreName != "" {
		cp["dataStoreName"] = d.DataStoreName
	}
	network := map[string]any{
		"serviceType": "LoadBalancer",
	}
	// A cluster's own allowlist wins; the provider default applies when it has none.
	cidrs := spec.AllowedCIDRs
	if len(cidrs) == 0 {
		cidrs = d.AllowedCIDRs
	}
	if len(cidrs) > 0 {
		src := make([]any, 0, len(cidrs))
		for _, c := range cidrs {
			src = append(src, c)
		}
		network["loadBalancerSourceRanges"] = src
	}
	ann := map[string]any{}
	if fqdn := d.ClusterFQDN(spec.ID); fqdn != "" {
		ann["external-dns.alpha.kubernetes.io/hostname"] = fqdn
		network["certSANs"] = []any{fqdn}
	}
	if len(ann) > 0 {
		network["serviceAnnotations"] = ann
	}
	cp["network"] = network

	values := map[string]any{
		"kubernetesVersion":  spec.Version,
		"kamajiControlPlane": cp,
		// The chart wires this secret into the cluster's infrastructure identity. It exists ONLY
		// on the management cluster (plan D7).
		"cloudCredentialsSecretName": CloudSecretName(spec.ID),
	}
	values["proxmox"] = infraValues(d, spec)
	if lb := spec.Infra.LoadBalancer; lb != nil {
		values["loadBalancer"] = LBValues(spec.ID, *lb)
	}
	if oidc := OIDCValues(spec.OIDC); oidc != nil {
		values["oidc"] = oidc
	}

	values["nodeGroups"] = NodeGroupValues(d, spec.Version, spec.NodeGroups)
	// A worker that never joins is replaced (kubeadm runs once): only on a chart that reads the block.
	if mhc := d.MachineHealthCheckValues(cfg.ChartVersion); mhc != nil {
		values["machineHealthCheck"] = mhc
	}

	// The addons block: the customer's curated toggles (ClusterAddons, one
	// `addons.<name>.enabled` per pick). No cloud credential ever enters the workload cluster (D7).
	if addons := AddonValues(spec.Addons); len(addons) > 0 {
		values["addons"] = addons
	}
	return values
}

// infraValues renders the chart's proxmox block. Secrets stay out: the two names point at Secrets
// the create path writes next to the Application.
func infraValues(d ClusterDefaults, spec ClusterSpec) map[string]any {
	in := spec.Infra
	classes := make([]any, 0, len(in.StorageClasses))
	for _, c := range in.StorageClasses {
		classes = append(classes, map[string]any{"name": c.Name, "storage": c.Storage})
	}
	dns := d.DNSServers
	if len(dns) == 0 {
		dns = []string{"1.1.1.1", "8.8.8.8"}
	}
	csi := map[string]any{
		"configSecretName": CSISecretName(spec.ID),
		"storageClasses":   classes,
	}
	if in.CSIGateway {
		csi["gateway"] = map[string]any{
			"secretName":           CSIGatewaySecretName(spec.ID),
			"kubeconfigSecretName": AdminKubeconfigSecretName(spec.ID),
		}
	}
	out := map[string]any{
		"region":  in.Region,
		"pool":    in.Pool,
		"storage": in.Storage,
		"network": map[string]any{
			"bridge":     in.Bridge,
			"gateway":    in.Gateway,
			"prefix":     in.Prefix,
			"addresses":  anySlice(in.Addresses),
			"dnsServers": anySlice(dns),
			"mtu":        d.NodeGroupMTU,
		},
		"csi":     csi,
		"metallb": map[string]any{"addresses": anySlice(in.LBAddresses)},
	}
	if in.RWXServer != "" {
		out["rwx"] = map[string]any{"server": in.RWXServer, "share": in.RWXShare}
	}
	return out
}

// LBValues renders the chart's loadBalancer block (from LBControllerChartVersion). The credential stays out: the block
// names the Secret the create path or EnableLBController writes.
func LBValues(clusterID string, lb LBController) map[string]any {
	return map[string]any{
		"secretName":           LBGatewaySecretName(clusterID),
		"kubeconfigSecretName": AdminKubeconfigSecretName(clusterID),
		"image":                lb.Image,
		"gatewayURL":           lb.GatewayURL,
		"default":              lb.Default,
	}
}

func anySlice(in []string) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}

// AddonValues renders the addons block: the customer's picks as `addons.<name>.enabled`.
func AddonValues(picks map[string]bool) map[string]any {
	addons := map[string]any{}
	for name, enabled := range picks {
		if _, curated := ClusterAddons[name]; !curated {
			continue
		}
		addons[name] = map[string]any{"enabled": enabled}
	}
	return addons
}

// OIDCValues renders the chart's oidc block from the customer-supplied config — shared by
// BuildValues and the SET_OIDC action. nil (= OIDC disabled) when issuerUrl is empty.
func OIDCValues(oidc map[string]string) map[string]any {
	issuer := oidc["issuerUrl"]
	if issuer == "" {
		return nil
	}
	out := map[string]any{"issuerUrl": issuer}
	for _, k := range []string{"clientId", "usernameClaim", "usernamePrefix", "groupsClaim", "groupsPrefix", "signingAlgs"} {
		if v := oidc[k]; v != "" {
			out[k] = v
		}
	}
	return out
}

// NodeGroupValues renders the chart's nodeGroups value — shared by BuildValues and the
// SET_NODE_GROUPS action (which swaps ONLY this key on the live Application values). Field
// spellings follow the verified chart contract (wrapper shape): machineCount is always set (the
// autoscaler's initial size for an autoscale group), min==max==count pins a fixed group.
func NodeGroupValues(d ClusterDefaults, version string, groups []NodeGroup) []any {
	out := make([]any, 0, len(groups))
	for _, ng := range groups {
		img := ng.ImageID
		if img == "" {
			img = d.ImageFor(version, ng.ImageVariant)
		}
		disk := ng.RootVolumeGiB
		if disk == 0 {
			disk = d.RootVolumeGiB
		}
		g := map[string]any{
			"name":           ng.Name,
			"machineFlavor":  ng.FlavorID,
			"machineImageId": img,
			// Explicitly sized root disk, always: the node images are bigger than most flavors'
			// disk.
			"machineRootVolume": map[string]any{"diskSize": disk},
		}
		// The variant rides in the values so it survives round-trips: the sync reads it back for
		// the UI, and UPGRADE re-resolves each group onto the target version's image of the same
		// variant. The chart reads only the keys it knows — an extra key is inert there.
		if ng.ImageVariant != "" {
			g["imageVariant"] = ng.ImageVariant
		}
		g["machineCount"] = ng.Count
		g["machineCores"] = ng.Cores
		g["machineMemoryMiB"] = ng.MemoryMiB
		g["machineTags"] = anySlice(ng.Tags)
		if len(ng.Placement) > 0 {
			g["machinePlacement"] = placementValue(ng.Placement)
		}
		if len(ng.Labels) > 0 {
			labels := map[string]any{}
			for k, v := range ng.Labels {
				labels[k] = v
			}
			g["nodeLabels"] = labels
		}
		if len(ng.Taints) > 0 {
			taints := make([]any, 0, len(ng.Taints))
			for _, t := range ng.Taints {
				if obj := taintToObject(t); obj != nil {
					taints = append(taints, obj)
				}
			}
			if len(taints) > 0 {
				g["taints"] = taints
			}
		}
		out = append(out, g)
	}
	return out
}

// NodeGroupsFromValues reads node groups back out of the live Application values — the inverse of
// NodeGroupValues, used by the sync and by any action that has to preserve groups it is not
// changing. Chart keys stay contained in this file on the way in AND on the way out.
func NodeGroupsFromValues(values map[string]any) []map[string]any {
	raw, _ := values["nodeGroups"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		ng, ok := item.(map[string]any)
		if !ok {
			continue
		}
		g := map[string]any{
			"name":      ng["name"],
			"flavor_id": ng["machineFlavor"],
			"image_id":  ng["machineImageId"],
		}
		if rv, ok := ng["machineRootVolume"].(map[string]any); ok {
			g["root_volume_gib"] = rv["diskSize"]
		}
		if v, _ := ng["imageVariant"].(string); v != "" {
			g["image_variant"] = v
		}
		if v, ok := ng["machineCount"]; ok {
			g["count"] = v
		}
		if p, ok := ng["machinePlacement"].(map[string]any); ok && len(p) > 0 {
			g["placement"] = p
		}
		if labels, ok := ng["nodeLabels"].(map[string]any); ok && len(labels) > 0 {
			g["labels"] = labels
		}
		// Back to the "key=value:Effect" string form the API and the UI speak.
		if taints, ok := ng["taints"].([]any); ok && len(taints) > 0 {
			list := []any{}
			for _, item := range taints {
				t, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if s := taintToString(t); s != "" {
					list = append(list, s)
				}
			}
			if len(list) > 0 {
				g["taints"] = list
			}
		}
		out = append(out, g)
	}
	return out
}

// taintToObject converts the kubeadm-style "key=value:Effect" (or "key:Effect") string the API
// accepts into the chart's taint object {key, value, effect}. nil for an unparseable string —
// ClusterSpec.Validate rejects those at the API boundary, so nil here is belt-and-braces.
func taintToObject(s string) map[string]any {
	kv, effect, ok := strings.Cut(s, ":")
	if !ok || effect == "" || kv == "" {
		return nil
	}
	key, value, _ := strings.Cut(kv, "=")
	if key == "" {
		return nil
	}
	obj := map[string]any{"key": key, "effect": effect}
	if value != "" {
		obj["value"] = value
	}
	return obj
}

// taintToString is the inverse mapping (chart taint object → "key=value:Effect"), used by the
// sync to keep the cache/UI representation in the string form the API accepts.
func taintToString(obj map[string]any) string {
	key, _ := obj["key"].(string)
	effect, _ := obj["effect"].(string)
	if key == "" || effect == "" {
		return ""
	}
	if value, _ := obj["value"].(string); value != "" {
		return key + "=" + value + ":" + effect
	}
	return key + ":" + effect
}

// ValidTaintEffects are the effects the kubelet accepts.
var ValidTaintEffects = map[string]bool{"NoSchedule": true, "PreferNoSchedule": true, "NoExecute": true}

// GroupPlacement is what placing a node group needs to know of it, read off rendered values.
type GroupPlacement struct {
	Name      string
	Flavor    string
	Count     int
	Image     string         // "<node>=<vmid>,..."
	Placement map[string]int // stored machinePlacement; nil when the group has none
	Cores     int
	MemoryMiB int
	DiskGiB   int
}

// GroupPlacements reads the node groups of chart values (live Application values, or NodeGroupValues' output under
// "nodeGroups").
func GroupPlacements(values map[string]any) []GroupPlacement {
	raw, _ := values["nodeGroups"].([]any)
	out := make([]GroupPlacement, 0, len(raw))
	for _, item := range raw {
		ng, ok := item.(map[string]any)
		if !ok {
			continue
		}
		g := GroupPlacement{Name: digStr(ng, "name"), Flavor: digStr(ng, "machineFlavor"), Image: digStr(ng, "machineImageId")}
		g.Count, _ = intAt(ng, "machineCount")
		g.Cores, _ = intAt(ng, "machineCores")
		g.MemoryMiB, _ = intAt(ng, "machineMemoryMiB")
		g.DiskGiB, _ = intAt(ng, "machineRootVolume", "diskSize")
		if p, ok := ng["machinePlacement"].(map[string]any); ok && len(p) > 0 {
			g.Placement = map[string]int{}
			for node := range p {
				g.Placement[node], _ = intAt(p, node)
			}
		}
		out = append(out, g)
	}
	return out
}

// Nodes are the PVE nodes that hold the group's image, in the image string's order.
func (g GroupPlacement) Nodes() []string { return ImageNodes(g.Image) }

// Baseline is where the group's machines are now: the stored placement, else the chart's even split.
func (g GroupPlacement) Baseline() map[string]int {
	if len(g.Placement) > 0 {
		return g.Placement
	}
	return EvenSplit(g.Count, g.Nodes())
}

// ImageNodes lists the nodes of an image string "<node>=<vmid>,<node>=<vmid>".
func ImageNodes(image string) []string {
	var out []string
	for _, pin := range strings.Split(image, ",") {
		if node, _, ok := strings.Cut(strings.TrimSpace(pin), "="); ok && node != "" {
			out = append(out, node)
		}
	}
	return out
}

// ImageTemplates reads an image string as node → template VMID. A pin whose VMID is not a number is left out.
func ImageTemplates(image string) map[string]int {
	out := map[string]int{}
	for _, pin := range strings.Split(image, ",") {
		node, id, _ := strings.Cut(strings.TrimSpace(pin), "=")
		if vmid, err := strconv.Atoi(strings.TrimSpace(id)); err == nil && node != "" {
			out[node] = vmid
		}
	}
	return out
}

// EvenSplit is the chart's split of count machines over nodes when a group has no machinePlacement: count/n each, the
// first count%n nodes one more (templates/node-groups.yaml; TestEvenSplitMatchesChart renders both).
func EvenSplit(count int, nodes []string) map[string]int {
	out := map[string]int{}
	for i, node := range nodes {
		out[node] = count / len(nodes)
		if i < count%len(nodes) {
			out[node]++
		}
	}
	return out
}

// SetPlacement writes a group's machinePlacement into its rendered values. It refuses what the chart would: an empty
// map, a node the image string does not name, a sum that is not the count. A bad map must never reach ArgoCD, which
// prunes: it would delete workers.
func SetPlacement(group map[string]any, placement map[string]int) error {
	name := digStr(group, "name")
	count, _ := intAt(group, "machineCount")
	nodes := ImageNodes(digStr(group, "machineImageId"))
	sum := 0
	for node, n := range placement {
		if !slices.Contains(nodes, node) || n < 0 {
			return fmt.Errorf("kamaji: node group %q: placement on %q (%d) does not fit image nodes %v", name, node, n, nodes)
		}
		sum += n
	}
	if len(placement) == 0 || sum != count {
		return fmt.Errorf("kamaji: node group %q: placement %v places %d machines, the group has %d", name, placement, sum, count)
	}
	group["machinePlacement"] = placementValue(placement)
	return nil
}

// placementValue is a placement as chart values. Nodes with no replica are left out: the chart reads an absent node
// as 0.
func placementValue(placement map[string]int) map[string]any {
	out := map[string]any{}
	for node, n := range placement {
		if n > 0 {
			out[node] = n
		}
	}
	return out
}

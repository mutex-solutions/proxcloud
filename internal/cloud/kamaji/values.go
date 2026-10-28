package kamaji

import (
	"fmt"
	"strings"
)

// values.go is the SINGLE place the kamaji cluster chart values contract lives. Node groups use
// the machine* spelling, and a chart `fail`s the render rather than defaulting when a key is
// missing — so a typo here is a create that never provisions. TestBuildValues pins every key;
// keep it that way.
//
// TODO(P7): the Proxmox cluster chart (CAPMOX machines, proxmox-csi default StorageClass, MetalLB
// API-server LoadBalancer) defines the infrastructure keys this builder renders.
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
	// BYO network: attach to the customer's existing network/subnet instead of creating one.
	// Ids only — never name filters, which are ambiguous the moment a customer reuses a name.
	if spec.NetworkID != "" {
		values["clusterNetworking"] = map[string]any{
			"internalNetwork": map[string]any{
				"networkFilter": map[string]any{"id": spec.NetworkID},
				"subnetFilter":  map[string]any{"id": spec.SubnetID},
			},
		}
	}
	if oidc := OIDCValues(spec.OIDC); oidc != nil {
		values["oidc"] = oidc
	}

	values["nodeGroups"] = NodeGroupValues(d, spec.Version, spec.NodeGroups)

	// Cluster-autoscaler minor MUST match the cluster's Kubernetes minor (upstream values
	// comment); the default tag would drift as soon as we offer a different minor.
	if maj, min, _, err := ParseVersion(spec.Version); err == nil {
		values["autoscaler"] = map[string]any{
			"image": map[string]any{"tag": fmt.Sprintf("v%d.%d.0", maj, min)},
		}
	}

	// The addons block: the customer's curated toggles (ClusterAddons, one
	// `addons.<name>.enabled` per pick). No cloud credential ever enters the workload cluster (D7).
	if addons := AddonValues(spec.Addons); len(addons) > 0 {
		values["addons"] = addons
	}
	return values
}

// AddonValues renders the addons block: the customer's picks as `addons.<name>.enabled`.
func AddonValues(picks map[string]bool) map[string]any {
	addons := map[string]any{}
	for name, enabled := range picks {
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
		if ng.Autoscale {
			g["autoscale"] = true
			// machineCount is the MachineDeployment's starting size — the floor, so the pool never
			// has to scale up from zero.
			g["machineCount"] = ng.Min
			g["machineCountMin"] = ng.Min
			g["machineCountMax"] = ng.Max
		} else {
			g["autoscale"] = false
			g["machineCount"] = ng.Count
			g["machineCountMin"] = ng.Count
			g["machineCountMax"] = ng.Count
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
		if v, ok := ng["autoscale"].(bool); ok {
			g["autoscale"] = v
		}
		for src, dst := range map[string]string{"machineCount": "count", "machineCountMin": "min", "machineCountMax": "max"} {
			if v, ok := ng[src]; ok {
				g[dst] = v
			}
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

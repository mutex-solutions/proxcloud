package kamaji

import (
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
	if oidc := OIDCValues(spec.OIDC); oidc != nil {
		values["oidc"] = oidc
	}

	values["nodeGroups"] = NodeGroupValues(d, spec.Version, spec.NodeGroups)

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

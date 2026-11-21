package externalservice

// kamaji.go — typed accessors for the "kamaji" Managed-Kubernetes provider kind
// (config.provider == "kamaji"; ceph-s3 precedent). Document shape:
//
//	{
//	  "name": "Kamaji AZ1", "type": "CLOUD", "status": "PRIVATE",
//	  "config": {
//	    "provider": "kamaji",
//	    "regions":  {"az1": {}},
//	    "services": {"kubernetes": {"az1": true}},
//	    "argocd": {                       // delivery plane (plan D3)
//	      "namespace":    "argocd",
//	      "project":      "proxcloud-k8s",  // AppProject guardrail
//	      "chartRepo":    "ghcr.io/mutex-solutions/charts",
//	      "chartName":    "kamaji-cluster", // required — no default
//	      "chartVersion": "0.2.3"         // pinned — never latest (plan §9)
//	    },
//	    "cluster": {                      // chart-value defaults every cluster inherits
//	      "dataStoreName":     "default",
//	      "dnsZone":           "k8s.example.com",
//	      "versions":          {"1.35.10": "pve1=9210,pve2=9211"},  // version→template per PVE node
//	      "imageVariants":     {"nvidia": {"1.35.4": "<node-image-id>"}},  // optional per-version variants
//	      "pveApiUrl":         "https://192.0.2.11:8006",  // the PVE API as CAPMOX reaches it
//	      "workerMtu":         1450                        // EVPN vnets
//	    },
//	    "namespacePlacement": {           // optional: pins every pod of pc-* (the tenant control planes) to a node
//	                                      // group through the PodNodeSelector/PodTolerationRestriction annotations
//	      "nodeSelector": {"node-restriction.kubernetes.io/proxcloud-node-group": "kamaji"},
//	      "tolerations":  [{"key": "proxcloud.io/node-group", "operator": "Equal", "value": "kamaji", "effect": "NoSchedule"}]
//	    }
//	  },
//	  "secret": {"kubeconfig": "<management-cluster kubeconfig>"}  // encrypted at rest
//	}
//
// Cluster CONTROL PLANES live on the management cluster; worker VMs land in the project's own
// compute service (plan D4) — so a project must be attached to both.

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
)

// IsKamaji reports whether this is a Kamaji Managed-Kubernetes provider (config.provider == "kamaji").
func (e *ExternalService) IsKamaji() bool { return e.Provider() == "kamaji" }

// KamajiRegion is config.region, falling back to the first configured region (ceph precedent).
func (e *ExternalService) KamajiRegion() string {
	if r := str(e.Config["region"]); r != "" {
		return r
	}
	if rs := e.RegionNames(); len(rs) > 0 {
		return rs[0]
	}
	return ""
}

// KamajiConfig assembles the kamaji.Config for kamaji.New: the decrypted management kubeconfig
// (secret.kubeconfig) + the argocd/cluster config blocks with their defaults.
func (e *ExternalService) KamajiConfig() kamaji.Config {
	argo, _ := e.Config["argocd"].(map[string]any)
	cl, _ := e.Config["cluster"].(map[string]any)

	versions := map[string]string{}
	if vs, ok := cl["versions"].(map[string]any); ok {
		for k, v := range vs {
			if s := str(v); s != "" {
				versions[k] = s
			}
		}
	}
	var variants map[string]map[string]string
	if vs, ok := cl["imageVariants"].(map[string]any); ok {
		for name, raw := range vs {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			byVersion := map[string]string{}
			for version, img := range m {
				if s := str(img); s != "" {
					byVersion[version] = s
				}
			}
			if len(byVersion) > 0 {
				if variants == nil {
					variants = map[string]map[string]string{}
				}
				variants[name] = byVersion
			}
		}
	}
	var flavors []string
	if fs, ok := cl["flavors"].([]any); ok {
		for _, f := range fs {
			if s := str(f); s != "" {
				flavors = append(flavors, s)
			}
		}
	}
	cfg := kamaji.Config{
		Kubeconfig:    str(e.secretMap()["kubeconfig"]),
		Region:        e.KamajiRegion(),
		ArgoNamespace: str(argo["namespace"]),
		ArgoProject:   str(argo["project"]),
		ChartRepo:     str(argo["chartRepo"]),
		ChartName:     str(argo["chartName"]),
		ChartVersion:  str(argo["chartVersion"]),
		Defaults: kamaji.ClusterDefaults{
			DataStoreName:  str(cl["dataStoreName"]),
			DNSZone:        str(cl["dnsZone"]),
			Versions:       versions,
			ImageVariants:  variants,
			Flavors:        flavors,
			RootVolumeGiB:  intOf(cl["rootVolumeGiB"]),
			RWXGiB:         intOf(cl["rwxGiB"]),
			AllowedCIDRs:   kamajiDefaultCIDRs(cl),
			PVEAPIURL:      str(cl["pveApiUrl"]),
			DNSServers:     strList(cl["dnsServers"]),
			NodeGroupMTU:   intOf(cl["workerMtu"]),
			WorkerFirewall: cl["workerFirewall"] == true, // anything but a boolean true is off (KamajiClusterProblems names it)

			ProxmoxServiceID: str(cl["proxmoxServiceId"]),
		},
		NamespacePlacement: namespacePlacement(e.Config),
	}
	// The automatic replacement of a worker that never joined: on by default, 0 turns it off.
	// A stored value the save would refuse today (KamajiClusterProblems) reads as the default: 1800 s, a guard of 1.
	cfg.Defaults.NodeStartupTimeoutSeconds = kamaji.DefaultNodeStartupTimeoutSeconds
	if n, ok := wholeNumber(cl["nodeStartupTimeoutSeconds"]); ok && validNodeStartupTimeout(n) {
		cfg.Defaults.NodeStartupTimeoutSeconds = n
	}
	if n, ok := wholeNumber(cl["repairMaxUnhealthy"]); ok && n >= 1 {
		cfg.Defaults.RepairMaxUnhealthy = n
	}
	if sw, ok := cl["sharedWorkers"].(map[string]any); ok && str(sw["bridge"]) != "" {
		sec := e.secretMap()
		cfg.Defaults.Shared = &kamaji.SharedWorkers{
			Pool: str(sw["pool"]), Bridge: str(sw["bridge"]), Gateway: str(sw["gateway"]), Prefix: intOf(sw["prefix"]),
			First: str(sw["first"]), Last: str(sw["last"]),
			CAPITokenID: str(sw["capiTokenId"]), CAPISecret: str(sec["sharedCapiSecret"]),
			CSITokenID: str(sw["csiTokenId"]), CSISecret: str(sec["sharedCsiSecret"]),
		}
	}
	if cfg.ArgoNamespace == "" {
		cfg.ArgoNamespace = "argocd"
	}
	return cfg
}

// KamajiLoadBalancer is config.cluster.loadBalancer: the public network and the flavor of a cluster's load balancer,
// both on the compute provider of the workers. ok is false when the block is absent or incomplete: no cluster gets one.
func (e *ExternalService) KamajiLoadBalancer() (publicNetworkID, flavorID string, ok bool) {
	cl, _ := e.Config["cluster"].(map[string]any)
	lb, _ := cl["loadBalancer"].(map[string]any)
	publicNetworkID, flavorID = str(lb["publicNetworkId"]), str(lb["flavorId"])
	return publicNetworkID, flavorID, publicNetworkID != "" && flavorID != ""
}

// The bounds of config.cluster.nodeStartupTimeoutSeconds. Cluster API deletes a worker that has no Node this long
// after the newest of its creation and its VM being ready, so the value must cover the longer of one clone and one
// first boot with its join. The floor is the 15 minutes the status sync itself gives a running machine to join before
// it asks for attention (kamaji boundDefault); the slowest create-to-provisioned of a worker seen on the lab (nested,
// loaded, 2026-11-19) was 5 min 23 s. Not measured on real slow storage: a site whose clones take longer than the
// floor needs more than the floor. Above a day the repair is as good as off, and 0 says that.
const (
	MinNodeStartupTimeoutSeconds = 900
	MaxNodeStartupTimeoutSeconds = 86400
)

func validNodeStartupTimeout(n int) bool {
	return n == 0 || (n >= MinNodeStartupTimeoutSeconds && n <= MaxNodeStartupTimeoutSeconds)
}

// wholeNumber reads a config value that must be a whole number: a JSON number without a fraction, or its digits as a
// string (Ansible's "{{ x | int }}"). "40%", 1.5, true and null are not one.
func wholeNumber(v any) (int, bool) {
	switch n := v.(type) {
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		return i, err == nil && i > -1e9 && i < 1e9
	case float64:
		return int(n), n == math.Trunc(n) && n > -1e9 && n < 1e9
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), n > -1e9 && n < 1e9
	}
	return 0, false
}

// KamajiClusterProblems names, one sentence each, the values of a kamaji provider's cluster block that the worker
// repair and the worker firewall cannot use. The save refuses a block that has one; a block stored before the check
// is read with the default in its place (KamajiConfig), and KamajiConfigNotes says so on the provider.
func KamajiClusterProblems(cfg map[string]any) []string {
	cl, _ := cfg["cluster"].(map[string]any)
	var out []string
	if v, ok := cl["nodeStartupTimeoutSeconds"]; ok {
		if n, whole := wholeNumber(v); !whole || !validNodeStartupTimeout(n) {
			out = append(out, fmt.Sprintf("config.cluster.nodeStartupTimeoutSeconds must be 0 (no automatic replacement) or a whole number of seconds "+
				"from %d to %d: Cluster API deletes a worker that has no node after this time, and under %d minutes it would delete machines that are still being cloned or are booting",
				MinNodeStartupTimeoutSeconds, MaxNodeStartupTimeoutSeconds, MinNodeStartupTimeoutSeconds/60))
		}
	}
	if v, ok := cl["repairMaxUnhealthy"]; ok {
		if n, whole := wholeNumber(v); !whole || n < 1 {
			out = append(out, "config.cluster.repairMaxUnhealthy must be a whole number of machines, 1 or more, not a percentage: "+
				"Cluster API rounds a percentage down, to 0 for a small node group, and nothing would be replaced")
		}
	}
	if v, ok := cl["workerFirewall"]; ok {
		if _, isBool := v.(bool); !isBool {
			out = append(out, "config.cluster.workerFirewall must be true or false (a JSON boolean): any other value means off")
		}
	}
	if v, ok := cl["loadBalancer"]; ok && v != nil {
		lb, isMap := v.(map[string]any)
		if !isMap || str(lb["publicNetworkId"]) == "" || str(lb["flavorId"]) == "" || len(lb) != 2 {
			out = append(out, "config.cluster.loadBalancer must be {\"publicNetworkId\": ..., \"flavorId\": ...}, both set: "+
				"a public network of the compute provider and a flavor for the two appliances of each cluster's balancer")
		}
	}
	return out
}

// KamajiAllowedCIDRsErr refuses a cluster block whose allowedCidrs, the allow-list a new cluster's API endpoint gets
// when the tenant names none, holds an entry Kubernetes 1.36 refuses in loadBalancerSourceRanges (cloud.StrictCIDRs).
// An entry the stored block already has passes, so a save that leaves the list alone is never refused for it;
// KamajiConfigNotes names it on the provider.
func KamajiAllowedCIDRsErr(cfg map[string]any, storedCluster any) error {
	cl, _ := cfg["cluster"].(map[string]any)
	old, _ := storedCluster.(map[string]any)
	if err := cloud.StrictCIDRs(strList(cl["allowedCidrs"]), strList(old["allowedCidrs"])); err != nil {
		return fmt.Errorf("config.cluster.allowedCidrs: %w", err)
	}
	return nil
}

// kamajiDefaultCIDRs reads config.cluster.allowedCidrs for a cluster about to be made. A stored entry with host bits
// is read as its network: the same range to Kubernetes before 1.36, and the only spelling 1.36 takes on the new
// Service. The stored value is not rewritten, and clusters that exist keep the list in their own values.
func kamajiDefaultCIDRs(cl map[string]any) []string {
	out := strList(cl["allowedCidrs"])
	for i, c := range out {
		out[i] = cloud.CanonicalCIDR(c)
	}
	return out
}

// KamajiConfigNotes is what an operator should know about a stored kamaji config that the config itself does not
// say: a value read as its default because it is not valid, and repair keys that do nothing on the provider's chart.
func KamajiConfigNotes(cfg map[string]any) []string {
	var out []string
	for _, p := range KamajiClusterProblems(cfg) {
		out = append(out, "Stored value ignored, the default applies (1800 s, 1 machine, firewall off) until it is corrected: "+p)
	}
	cl, _ := cfg["cluster"].(map[string]any)
	if err := cloud.StrictCIDRs(strList(cl["allowedCidrs"]), nil); err != nil {
		out = append(out, "config.cluster.allowedCidrs: "+err.Error()+". Kubernetes 1.36 refuses such a value on a new Service. "+
			"A new cluster that takes this default gets the entry without its host bits (one that is not a CIDR at all is passed on and leaves the cluster without an API endpoint); "+
			"clusters that exist keep their list. Correct the stored value")
	}
	argo, _ := cfg["argocd"].(map[string]any)
	_, timeout := cl["nodeStartupTimeoutSeconds"]
	_, guard := cl["repairMaxUnhealthy"]
	if v := str(argo["chartVersion"]); (timeout || guard) && v != "" && !kamaji.ChartHasMachineHealthCheck(v) {
		out = append(out, fmt.Sprintf("config.cluster.nodeStartupTimeoutSeconds and repairMaxUnhealthy are stored but have no effect on cluster chart %s: "+
			"a cluster gets its health check once it is on chart %s or later (config.argocd.chartVersion for new clusters, the chart bump for an existing one)",
			v, kamaji.MachineHealthCheckChartVersion))
	}
	return out
}

// intOf reads a config number, which round-trips through JSON as float64. Ansible's "{{ x | int }}" sends a string, so the
// installer's numbers arrive as "100".
func intOf(v any) int {
	switch n := v.(type) {
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(n))
		return i
	case float64:
		return int(n)
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	}
	return 0
}

// strList reads a config array of strings, skipping blanks.
func strList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s := str(it); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// RemovedKamajiClusterKeys are config.cluster keys of the tenant-side proxmox-csi controller, gone since block volumes
// go through the CSI gateway: a provider that still sends one is refused (KamajiRemovedKeys), not silently ignored.
var RemovedKamajiClusterKeys = []string{"csiSharedStores", "pveCsiUrl"}

// KamajiRemovedKeys names the removed keys a kamaji provider config sets in its cluster block.
func KamajiRemovedKeys(cfg map[string]any) []string {
	cl, _ := cfg["cluster"].(map[string]any)
	var out []string
	for _, k := range RemovedKamajiClusterKeys {
		if _, ok := cl[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

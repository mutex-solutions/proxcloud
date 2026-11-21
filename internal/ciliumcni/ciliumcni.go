// Package ciliumcni answers whether Cilium leaves a node's CNI config file to a chained plugin
// (sandbox-netguard). Stdlib only: the netguard binary on the sandbox nodes imports it.
package ciliumcni

import "sort"

// NodeConfig is the part of a CiliumNodeConfig (cilium.io/v2, in Cilium's namespace) that the
// agent merges over the cilium-config ConfigMap at start-up.
type NodeConfig struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		NodeSelector *Selector         `json:"nodeSelector"`
		Defaults     map[string]string `json:"defaults"`
	} `json:"spec"`
}

// Selector is a Kubernetes LabelSelector.
type Selector struct {
	MatchLabels      map[string]string `json:"matchLabels"`
	MatchExpressions []struct {
		Key      string   `json:"key"`
		Operator string   `json:"operator"`
		Values   []string `json:"values"`
	} `json:"matchExpressions"`
}

// Effective is the agent config of a node with these labels, as Cilium 1.18's build-config makes
// it (pkg/option/resolver): the ConfigMap, then every CiliumNodeConfig whose selector matches and
// whose defaults are not empty, in name order, a later name winning. A nil selector matches
// nothing, an empty one every node.
func Effective(configMap map[string]string, nodeConfigs []NodeConfig, nodeLabels map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range configMap {
		out[k] = v
	}
	ncs := append([]NodeConfig(nil), nodeConfigs...)
	sort.Slice(ncs, func(i, j int) bool { return ncs[i].Metadata.Name < ncs[j].Metadata.Name })
	for _, nc := range ncs {
		if len(nc.Spec.Defaults) == 0 || nc.Spec.NodeSelector == nil || !nc.Spec.NodeSelector.Matches(nodeLabels) {
			continue
		}
		for k, v := range nc.Spec.Defaults {
			out[k] = v
		}
	}
	return out
}

// CustomConf reports whether Cilium on the node leaves 05-cilium.conflist alone: custom-cni-conf
// "true" and no write-cni-conf-when-ready. Set cluster-wide (the sandbox cluster) or by a
// CiliumNodeConfig for the sandbox nodes of a shared cluster, which must then set
// write-cni-conf-when-ready to "": left out, the ConfigMap's path stays and Cilium rewrites the
// file without the netguard chain.
func CustomConf(configMap map[string]string, nodeConfigs []NodeConfig, nodeLabels map[string]string) bool {
	e := Effective(configMap, nodeConfigs, nodeLabels)
	return e["custom-cni-conf"] == "true" && e["write-cni-conf-when-ready"] == ""
}

// Matches evaluates the selector against labels; an unknown operator matches nothing.
func (s *Selector) Matches(labels map[string]string) bool {
	for k, v := range s.MatchLabels {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	for _, e := range s.MatchExpressions {
		v, has := labels[e.Key]
		in := false
		for _, x := range e.Values {
			in = in || (has && v == x)
		}
		switch e.Operator {
		case "In":
			if !in {
				return false
			}
		case "NotIn":
			if in {
				return false
			}
		case "Exists":
			if !has {
				return false
			}
		case "DoesNotExist":
			if has {
				return false
			}
		default:
			return false
		}
	}
	return true
}

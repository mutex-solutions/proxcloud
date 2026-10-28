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
//	      "versions":          {"1.35.4": "<node-image-id>"},  // curated version→DEFAULT-image matrix
//	      "imageVariants":     {"nvidia": {"1.35.4": "<node-image-id>"}}  // optional per-version variants
//	    }
//	  },
//	  "secret": {"kubeconfig": "<management-cluster kubeconfig>"}  // encrypted at rest
//	}
//
// Cluster CONTROL PLANES live on the management cluster; worker VMs land in the project's own
// compute service (plan D4) — so a project must be attached to both.

import "github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"

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
			DataStoreName: str(cl["dataStoreName"]),
			DNSZone:       str(cl["dnsZone"]),
			Versions:      versions,
			ImageVariants: variants,
			Flavors:       flavors,
			RootVolumeGiB: intOf(cl["rootVolumeGiB"]),
			AllowedCIDRs:  strList(cl["allowedCidrs"]),
		},
	}
	if cfg.ArgoNamespace == "" {
		cfg.ArgoNamespace = "argocd"
	}
	return cfg
}

// intOf reads a config number, which round-trips through JSON as float64.
func intOf(v any) int {
	switch n := v.(type) {
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

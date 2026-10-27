package externalservice

// dbaas.go — typed accessors for the "dbaas" Managed-Database provider kind
// (config.provider == "dbaas"; kamaji precedent). Document shape:
//
//	{
//	  "name": "DBaaS AZ1", "type": "CLOUD", "status": "PRIVATE",
//	  "config": {
//	    "provider": "dbaas",
//	    "regions":  {"az1": {}},
//	    "services": {"database": {"az1": true}},
//	    "argocd": {                        // delivery plane (kamaji pattern)
//	      "namespace":    "argocd",
//	      "project":      "proxcloud-dbaas", // AppProject guardrail
//	      "chartRepo":    "ghcr.io/mutex-solutions/charts",
//	      "chartName":    "database-cluster",
//	      "chartVersion": "0.1.0"          // pinned — never latest
//	    },
//	    "database": {
//	      "osServiceId":    "svc-openstack", // the OpenStack service the DB cluster's nodes live on
//	      "osProjectId":    "…",             // dbaas keystone project — neutron RBAC target_tenant
//	      "memberSubnetId": "…",             // DB-cluster node subnet (Octavia member-subnet-id)
//	      "storageClasses": ["nvme"],        // optional customer-facing allowlist
//	      "limits":         {"maxCpu": 32, "maxMemoryGiB": 128, "maxStorageGiB": 2048},
//	      "engines": {                       // curated engine catalog (the ONLY engines offered).
//	        "postgresql": {"versions": ["16","17","18"], "default": "17", "replicas": [1,2,3]},
//	        "valkey":     {"versions": ["8.1"], "default": "8.1", "replicas": [1,3], "beta": true}
//	      }                                  // ⚠ every version MUST have a key in the chart's
//	                                         // engineVersion→image map (_helpers.tpl) — an
//	                                         // unmapped version passes Validate and then hard-
//	                                         // fails the ArgoCD render
//	    }
//	  },
//	  "secret": {"kubeconfig": "<db-cluster kubeconfig>"}  // encrypted at rest
//	}
//
// A dbaas provider is OpenStack-adjacent the OPPOSITE way from kamaji: the databases run on
// OPS-owned nodes; the only customer-tenant artifact is the Octavia LB VIP, which requires the
// tenant network to be shared with the dbaas keystone project (neutron RBAC) — so a project
// must still be attached to the OpenStack service its VPC lives on.

import "github.com/mutex-solutions/proxcloud/internal/cloud/dbaas"

// IsDbaas reports whether this is a Managed-Database provider (config.provider == "dbaas").
func (e *ExternalService) IsDbaas() bool { return e.Provider() == "dbaas" }

// DbaasRegion is config.region, falling back to the first configured region (ceph precedent).
func (e *ExternalService) DbaasRegion() string {
	if r := str(e.Config["region"]); r != "" {
		return r
	}
	if rs := e.RegionNames(); len(rs) > 0 {
		return rs[0]
	}
	return ""
}

// DbaasConfig assembles the dbaas.Config for dbaas.New: the decrypted DB-cluster kubeconfig
// (secret.kubeconfig) + the argocd/database config blocks with their defaults.
func (e *ExternalService) DbaasConfig() dbaas.Config {
	argo, _ := e.Config["argocd"].(map[string]any)
	db, _ := e.Config["database"].(map[string]any)

	engines := map[string]dbaas.EngineOffer{}
	if es, ok := db["engines"].(map[string]any); ok {
		for name, raw := range es {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			offer := dbaas.EngineOffer{
				Versions: strList(m["versions"]),
				Default:  str(m["default"]),
			}
			if b, ok := m["beta"].(bool); ok {
				offer.Beta = b
			}
			if rs, ok := m["replicas"].([]any); ok {
				for _, r := range rs {
					if n := intOf(r); n > 0 {
						offer.Replicas = append(offer.Replicas, n)
					}
				}
			}
			if len(offer.Versions) > 0 {
				engines[name] = offer
			}
		}
	}
	limits, _ := db["limits"].(map[string]any)
	cfg := dbaas.Config{
		Kubeconfig:     str(e.secretMap()["kubeconfig"]),
		Region:         e.DbaasRegion(),
		ArgoNamespace:  str(argo["namespace"]),
		ArgoProject:    str(argo["project"]),
		ChartRepo:      str(argo["chartRepo"]),
		ChartName:      str(argo["chartName"]),
		ChartVersion:   str(argo["chartVersion"]),
		OSServiceID:    str(db["osServiceId"]),
		OSProjectID:    str(db["osProjectId"]),
		MemberSubnetID: str(db["memberSubnetId"]),
		StorageClasses: strList(db["storageClasses"]),
		Limits: dbaas.Limits{
			MaxCPU:        intOf(limits["maxCpu"]),
			MaxMemoryGiB:  intOf(limits["maxMemoryGiB"]),
			MaxStorageGiB: intOf(limits["maxStorageGiB"]),
		},
		Engines: engines,
	}
	if cfg.ArgoNamespace == "" {
		cfg.ArgoNamespace = "argocd"
	}
	if cfg.ChartName == "" {
		cfg.ChartName = "database-cluster"
	}
	return cfg
}

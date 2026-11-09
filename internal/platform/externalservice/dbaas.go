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
//	      "lbAddressPool":  "dbaas-services", // MetalLB pool every tenant-facing LB draws from
//	      "storageClasses": ["nvme"],        // optional customer-facing allowlist; `capacity` and
//	                                         // `capacity-*` (HDD tier) are never offered
//	      "limits":         {"maxCpu": 32, "maxMemoryGiB": 128, "maxStorageGiB": 2048},
//	      "scheduling": {                    // optional
//	        "zoneKey":      "topology.kubernetes.io/zone", // replica spread key (hard anti-affinity)
//	        "nodeSelector": {"node-role": "database"},     // dedicated database node pool
//	        "tolerations":  [{"key": "dedicated", "operator": "Equal", "value": "database", "effect": "NoSchedule"}]
//	      },
//	      "backup": {                        // optional S3 object store; absent = backups off.
//	        "endpoint":     "http://s3.example.com:9000",  // WITH scheme; plain http works
//	        "bucket":       "dbaas-backups",
//	        "prefix":       "backups",
//	        "region":       "us-east-1",
//	        "pathStyle":    true,            // default true
//	        "caCertSecret": ""               // optional Secret (key ca.crt) on the DB cluster
//	      },                                 // credentials: secret.backupAccessKey / backupSecretKey
//	      "engines": {                       // curated engine catalog (the ONLY engines offered).
//	        "postgresql": {"versions": ["16","17","18"], "default": "17", "replicas": [1,2,3]},
//	        "valkey":     {"versions": ["9.0"], "default": "9.0", "replicas": [1,3], "beta": true}
//	      }                                  // ⚠ every version MUST have a key in the chart's
//	                                         // engineVersion→image map (_helpers.tpl) — an
//	                                         // unmapped version passes Validate and then hard-
//	                                         // fails the ArgoCD render
//	    }
//	  },
//	  "secret": {                          // encrypted at rest
//	    "kubeconfig":      "<db-cluster kubeconfig>",
//	    "backupAccessKey": "<s3 access key>",   // only with config.database.backup
//	    "backupSecretKey": "<s3 secret key>"
//	  }
//	}
//
// The databases run on OPS-owned nodes; the only tenant-facing artifact is the LB ingress IP
// (MetalLB, from the lbAddressPool services subnet), restricted by loadBalancerSourceRanges.

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

// dbaasBackupConfig assembles the operator-configured object store. The credentials come from
// the provider SECRET, never from config: they are as sensitive as the kubeconfig and must be
// stripped from admin reads by the same machinery.
func dbaasBackupConfig(db map[string]any, secret map[string]any) dbaas.BackupConfig {
	b, _ := db["backup"].(map[string]any)
	if b == nil {
		return dbaas.BackupConfig{}
	}
	// Path-style defaults ON (RustFS, Ceph RGW); only an explicit false turns it off.
	pathStyle := true
	if v, ok := b["pathStyle"].(bool); ok {
		pathStyle = v
	}
	return dbaas.BackupConfig{
		Endpoint:     str(b["endpoint"]),
		Bucket:       str(b["bucket"]),
		Region:       str(b["region"]),
		Prefix:       str(b["prefix"]),
		PathStyle:    pathStyle,
		CACertSecret: str(b["caCertSecret"]),
		AccessKey:    str(secret["backupAccessKey"]),
		SecretKey:    str(secret["backupSecretKey"]),
	}
}

// dbaasScheduling reads config.database.scheduling: the replica spread key plus the optional
// dedicated-node-pool selector and tolerations (opaque toleration objects, passed to the chart
// unchanged — the API server's admission rejects a bad one).
func dbaasScheduling(db map[string]any) (zoneKey string, nodeSelector map[string]string, tolerations []map[string]any) {
	block, _ := db["scheduling"].(map[string]any)
	if block == nil {
		return "", nil, nil
	}
	if ns, ok := block["nodeSelector"].(map[string]any); ok {
		for k, v := range ns {
			if s := str(v); k != "" && s != "" {
				if nodeSelector == nil {
					nodeSelector = map[string]string{}
				}
				nodeSelector[k] = s
			}
		}
	}
	if ts, ok := block["tolerations"].([]any); ok {
		for _, t := range ts {
			if m, ok := t.(map[string]any); ok && len(m) > 0 {
				tolerations = append(tolerations, m)
			}
		}
	}
	return str(block["zoneKey"]), nodeSelector, tolerations
}

// dbaasStorageClasses reads the customer-facing storage-class allowlist, dropping the capacity
// tier: a class named `capacity` / `capacity-*` is HDD-backed NFS and never offered to databases.
func dbaasStorageClasses(db map[string]any) []string {
	out := []string{}
	for _, c := range strList(db["storageClasses"]) {
		if !dbaas.CapacityClass(c) {
			out = append(out, c)
		}
	}
	return out
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
	zoneKey, nodeSelector, tolerations := dbaasScheduling(db)
	cfg := dbaas.Config{
		Kubeconfig:     str(e.secretMap()["kubeconfig"]),
		Region:         e.DbaasRegion(),
		ArgoNamespace:  str(argo["namespace"]),
		ArgoProject:    str(argo["project"]),
		ChartRepo:      str(argo["chartRepo"]),
		ChartName:      str(argo["chartName"]),
		ChartVersion:   str(argo["chartVersion"]),
		LBAddressPool:  str(db["lbAddressPool"]),
		ZoneKey:        zoneKey,
		Backup:         dbaasBackupConfig(db, e.secretMap()),
		StorageClasses: dbaasStorageClasses(db),
		Limits: dbaas.Limits{
			MaxCPU:        intOf(limits["maxCpu"]),
			MaxMemoryGiB:  intOf(limits["maxMemoryGiB"]),
			MaxStorageGiB: intOf(limits["maxStorageGiB"]),
		},
		Engines:      engines,
		NodeSelector: nodeSelector,
		Tolerations:  tolerations,
	}
	if cfg.ArgoNamespace == "" {
		cfg.ArgoNamespace = "argocd"
	}
	if cfg.ChartName == "" {
		cfg.ChartName = "database-cluster"
	}
	return cfg
}

package dbaas

// values.go — the SINGLE source of truth for the `database-cluster` chart values contract
// (deploy/charts/database-cluster). Every key BuildValues emits is a key the chart reads;
// TestBuildValues pins all of them per engine, so a rename on either side is a test failure,
// not a silently ignored value (the kamaji values.go lesson). No credentials in values, EVER —
// the Application CR is readable by anyone with argocd read; passwords ride in Secrets only.

// BuildValues renders the full chart values for one database. The generated values live inline
// on the Application (spec.source.helm.valuesObject) — the DB cluster is the state store,
// proxcloud stays stateless about desired spec.
func BuildValues(cfg Config, spec DatabaseSpec) map[string]any {
	storage := map[string]any{"sizeGi": spec.StorageGiB}
	if spec.StorageClass != "" {
		storage["storageClassName"] = spec.StorageClass
	}
	cidrs := make([]any, 0, len(spec.AllowedCIDRs))
	for _, c := range spec.AllowedCIDRs {
		cidrs = append(cidrs, c)
	}
	return map[string]any{
		"engine":        spec.Engine,
		"engineVersion": spec.Version,
		"instances":     spec.Replicas,
		"resources":     map[string]any{"cpu": spec.CPU, "memoryGi": spec.MemoryGiB},
		"storage":       storage,
		// TODO(P7): MetalLB services subnet — the LB address pool replaces the tenant-network
		// placement; isolation stays loadBalancerSourceRanges.
		"network": map[string]any{
			"allowedCidrs": cidrs,
		},
		"proxcloud": map[string]any{
			"projectId":   spec.ProjectID,
			"resourceId":  spec.ID,
			"displayName": spec.DisplayName,
		},
	}
}

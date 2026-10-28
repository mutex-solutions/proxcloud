package dbaas

// BuildApplication renders the ArgoCD Application CR for one database: pinned chart revision,
// FULL values inline (helm.valuesObject), destination = the project namespace on the DB cluster
// itself. The resources-finalizer makes an Application delete cascade to everything the chart
// rendered (engine CR, LB Service, NetworkPolicy) — our delete path relies on it.
func BuildApplication(cfg Config, spec DatabaseSpec, serviceID, chartVersion string, values map[string]any) map[string]any {
	if chartVersion == "" {
		chartVersion = cfg.ChartVersion
	}
	// The dedicated AppProject is the guardrail (sourceRepos/destinations constrained —
	// deploy/dbaas-cluster/appproject.yaml). Defaulting to ArgoCD's unrestricted "default"
	// project would silently drop it, so the fallback is the guardrail project's name.
	project := cfg.ArgoProject
	if project == "" {
		project = "proxcloud-dbaas"
	}
	return map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":      spec.ID,
			"namespace": cfg.ArgoNamespace,
			"labels": map[string]any{
				LabelProject:   spec.ProjectID,
				LabelService:   serviceID,
				LabelManagedBy: ManagedByValue,
			},
			"annotations": map[string]any{
				AnnotationDisplayName: spec.DisplayName,
			},
			"finalizers": []any{"resources-finalizer.argocd.argoproj.io"},
		},
		"spec": map[string]any{
			"project": project,
			"source": map[string]any{
				"repoURL":        cfg.ChartRepo,
				"chart":          cfg.ChartName,
				"targetRevision": chartVersion,
				"helm":           map[string]any{"valuesObject": values},
			},
			"destination": map[string]any{
				"server":    "https://kubernetes.default.svc",
				"namespace": NamespaceFor(spec.ProjectID),
			},
			"syncPolicy": map[string]any{
				"automated": map[string]any{"prune": true, "selfHeal": true},
			},
		},
	}
}

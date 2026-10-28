package dbaas

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// DatabaseSyncProvider lists a project's databases → DATABASE_CLUSTER cloud resources. It
// satisfies providers.Provider + ProjectScoped implicitly (no import — keeps the dependency
// arrow pointing dbaas→cloud only). One Application = one cached resource; the LB Service
// enriches it with the tenant-side endpoint.
type DatabaseSyncProvider struct {
	svc       *Service
	region    string
	projectID string
}

// SyncProvider builds the read-sync provider for one project (syncjob leg).
func (s *Service) SyncProvider(region, projectID string) *DatabaseSyncProvider {
	return &DatabaseSyncProvider{svc: s, region: region, projectID: projectID}
}

func (p *DatabaseSyncProvider) Type() string      { return cloud.TypeDatabaseCluster }
func (p *DatabaseSyncProvider) ProjectID() string { return p.projectID }

func (p *DatabaseSyncProvider) List(ctx context.Context) ([]cloud.CloudResource, error) {
	s := p.svc
	// Ownership marker in the selector: anything unlabelled on the DB cluster never enters the
	// cache, so it never surfaces in the UI or billing.
	apps, err := s.api.ListApplications(ctx, s.cfg.ArgoNamespace,
		LabelProject+"="+p.projectID+","+LabelManagedBy+"="+ManagedByValue)
	if err != nil {
		return nil, err
	}
	ns := NamespaceFor(p.projectID)
	out := make([]cloud.CloudResource, 0, len(apps))
	for _, app := range apps {
		id := digStr(app, "metadata", "name")
		if id == "" {
			continue
		}
		// A terminating Application (delete issued, resources-finalizer still cascading) must
		// not re-enter the cache — the row was archived at delete time, and resurrecting it
		// would flash the database back into the UI and billing until the cascade finishes.
		if dig(app, "metadata", "deletionTimestamp") != nil {
			continue
		}
		// Best-effort enrichment: a half-provisioned database still syncs (endpoint stays "").
		engine := digStr(app, "spec", "source", "helm", "valuesObject", "engine")
		host, err := s.lbHost(ctx, ns, LBServiceNameFor(engine, id))
		if err != nil {
			return nil, err
		}
		out = append(out, cloud.CloudResource{
			Type:       cloud.TypeDatabaseCluster,
			ExternalID: id,
			Region:     p.region,
			ProjectID:  p.projectID,
			Data:       databaseDataWithHost(app, host),
		})
	}
	return out, nil
}

// databaseData maps an Application → the cached `data` payload (no endpoint enrichment — the
// create path's initial row). bson-round-trip-stable by construction: strings, bools and JSON
// numbers only, timestamps as the RFC3339 strings Kubernetes already serializes.
func databaseData(app map[string]any, _ []map[string]any) map[string]any {
	return databaseDataWithHost(app, "")
}

func databaseDataWithHost(app map[string]any, host string) map[string]any {
	values, _ := dig(app, "spec", "source", "helm", "valuesObject").(map[string]any)

	status := "PENDING"
	if h := digStr(app, "status", "health", "status"); h != "" {
		// Argo health: Healthy / Progressing / Degraded / Suspended / Missing / Unknown. The LB
		// Service's built-in health keeps the Application Progressing until the LB ingress IP is
		// assigned, so READY implies the endpoint exists.
		status = map[string]string{
			"Healthy":     "READY",
			"Progressing": "PROGRESSING",
			"Degraded":    "DEGRADED",
			"Suspended":   "SUSPENDED",
			"Missing":     "PROGRESSING",
			"Unknown":     "UNKNOWN",
		}[h]
		if status == "" {
			status = "UNKNOWN"
		}
	}

	engine := digStr(values, "engine")
	d := map[string]any{
		"id":            digStr(app, "metadata", "name"),
		"name":          digStr(app, "metadata", "annotations", AnnotationDisplayName),
		"engine":        engine,
		"version":       digStr(values, "engineVersion"),
		"chart_version": digStr(app, "spec", "source", "targetRevision"),
		"status":        status,
		"sync_status":   digStr(app, "status", "sync", "status"),
		"created_at":    digStr(app, "metadata", "creationTimestamp"),
		"endpoint":      host, // "" until the LB ingress IP is assigned — billing keys on this
		"port":          Port(engine),
	}
	for key, path := range map[string][]string{
		"replicas":    {"instances"},
		"cpu":         {"resources", "cpu"},
		"memory_gib":  {"resources", "memoryGi"},
		"storage_gib": {"storage", "sizeGi"},
	} {
		switch n := dig(values, path...).(type) {
		case float64:
			d[key] = n
		case int:
			d[key] = n
		}
	}
	if sc := digStr(values, "storage", "storageClassName"); sc != "" {
		d["storage_class"] = sc
	}
	// Autoscale opt-in: 1/0 as a JSON number — the billing surcharge attribute keys on it.
	if enabled, _ := dig(values, "autoscale", "enabled").(bool); enabled {
		d["autoscale_enabled"] = 1
		auto, _ := values["autoscale"].(map[string]any)
		d["autoscale_max_cpu"] = intAt(auto, "maxCpu")
		d["autoscale_max_memory_gib"] = intAt(auto, "maxMemoryGi")
		d["autoscale_max_storage_gib"] = intAt(auto, "maxStorageGi")
	} else {
		d["autoscale_enabled"] = 0
	}
	if cidrs, ok := dig(values, "network", "allowedCidrs").([]any); ok && len(cidrs) > 0 {
		d["allowed_cidrs"] = cidrs
	}
	return map[string]any{"database": d}
}

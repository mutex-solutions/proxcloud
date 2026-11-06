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
		LabelProject+"="+p.projectID+","+LabelService+"="+s.serviceID+","+LabelManagedBy+"="+ManagedByValue)
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
			Data:       databaseDataWithHost(app, host, s.readHost(ctx, ns, id, engine, host)),
		})
	}
	return out, nil
}

// databaseData maps an Application → the cached `data` payload (no endpoint enrichment — the
// create path's initial row). bson-round-trip-stable by construction: strings, bools and JSON
// numbers only, timestamps as the RFC3339 strings Kubernetes already serializes.
func databaseData(app map[string]any, _ []map[string]any) map[string]any {
	return databaseDataWithHost(app, "", "")
}

func databaseDataWithHost(app map[string]any, host, readHost string) map[string]any {
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

	// Health ALONE is not readiness. ArgoCD computes health over the resources it can actually
	// see, and it has no health check for most operator CRs — so an engine CR the API server
	// REJECTED (never applied, app left OutOfSync: e.g. an ObjectStore whose CRD is not
	// installed) leaves every remaining resource healthy, and the app reads Healthy. OutOfSync
	// is the honest signal there: ArgoCD could not put the desired state on the cluster. A
	// database whose spec never landed is not ready — PROGRESSING rather than DEGRADED, since
	// ArgoCD retries and most OutOfSync moments are a sync still in flight.
	if status == "READY" && digStr(app, "status", "sync", "status") == "OutOfSync" {
		status = "PROGRESSING"
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
		// endpoint is what the customer connects to (their domain > the LB address);
		// endpoint_ip keeps the raw address, which is what a BYO domain's A record must target.
		// Both are "" until the LB ingress IP is assigned — billing keys on endpoint being non-empty.
		"endpoint":    PublicHost(digStr(values, "opensearch", "customDomain"), host),
		"endpoint_ip": host,
		// Read-only endpoint. read_endpoint_lb records whether offering it provisioned a
		// SECOND LoadBalancer (postgresql only) — billing charges that, not the feature, because
		// mysql and mariadb serve reads on another port of the proxy they already have.
		"parameters":    paramValues(values, engine),
		"read_endpoint": dig(values, "network", "readEndpoint") == true,
		// The address customers point readers at. Read from the live Service rather than
		// derived client-side: postgresql's is a different LB with its own address, while mysql
		// and mariadb reuse this one on another port.
		"read_endpoint_host": readHost,
		"read_endpoint_lb":   dig(values, "network", "readEndpoint") == true && ReadEndpointCostsLB(engine),
		// Customer-managed access, echoed from the values so the UI can list it without a
		// second read. Names only — passwords live in per-user Secrets on the DB cluster.
		"databases": accessNames(values, "databases"),
		"users":     accessNames(values, "users"),
		// opensearch-only surfaces; empty slices elsewhere, which stay bson-stable.
		"os_roles":       osList(values, "roles"),
		"index_policies": osList(values, "indexPolicies"),
		"port":           Port(engine),
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
	// Backup posture. The console builds the restore-source list from `backup.enabled`, so
	// without this projection no database is ever offered as a recovery source, and the
	// schedule/retention summary would always render the defaults.
	//
	// Object-store wiring is the operator's, so `enabled` tracks the values block the chart got —
	// which is what "this database archives to the object store" means — and schedule/retention
	// carry the customer's posture on top.
	if b, ok := values["backup"].(map[string]any); ok {
		block := map[string]any{"enabled": b["enabled"] == true}
		if sched := digStr(b, "schedule"); sched != "" {
			block["schedule"] = sched
		}
		if days := intAt(b, "retentionDays"); days > 0 {
			block["retentionDays"] = days
		}
		d["backup"] = block
	}
	return map[string]any{"database": d}
}

// accessNames flattens a values access list (databases/users) to the display shape the client
// renders: [{name, owner|databases|roles}]. Values are already bson-round-trip-stable strings.
func accessNames(values map[string]any, key string) []any {
	list, _ := dig(values, key).([]any)
	out := make([]any, 0, len(list))
	for _, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := digStr(item, "name")
		entry := map[string]any{"name": name}
		// OpenSearch has no username field on its user CR — the CR name IS the login, and it
		// stays prefixed so two databases in one namespace cannot collide. Echo the effective
		// login so the UI shows what to type (see templates/opensearch/users.yaml).
		if key == "users" && digStr(values, "engine") == EngineOpenSearch {
			// K8sName: the CR name is the login, and object names map '_' to '-'.
			entry["login"] = digStr(values, "proxcloud", "resourceId") + "-u-" + K8sName(name)
		}
		for _, f := range []string{"owner"} {
			if v := digStr(item, f); v != "" {
				entry[f] = v
			}
		}
		for _, f := range []string{"databases", "roles"} {
			if v, _ := item[f].([]any); len(v) > 0 {
				entry[f] = v
			}
		}
		out = append(out, entry)
	}
	return out
}

// osList echoes an opensearch sub-list (custom roles, retention policies) from the values so the
// client can render what is deployed without a second read. Plain strings/numbers only.
func osList(values map[string]any, key string) []any {
	block, _ := dig(values, "opensearch").(map[string]any)
	list, _ := dig(block, key).([]any)
	out := make([]any, 0, len(list))
	for _, raw := range list {
		if item, ok := raw.(map[string]any); ok {
			out = append(out, item)
		}
	}
	return out
}

// paramValues echoes a database's applied runtime settings from the values document, so the
// configuration form shows what is deployed rather than what someone last typed.
func paramValues(values map[string]any, engine string) map[string]any {
	block := ParamBlockFor(engine)
	if block == "" {
		return map[string]any{}
	}
	cur, _ := dig(values, block, "parameters").(map[string]any)
	out := make(map[string]any, len(cur))
	for k, v := range cur {
		out[k] = v
	}
	return out
}

// readHost resolves the address of a database's read-only endpoint, or "" when it has none.
// postgresql serves reads from a SECOND LoadBalancer Service with its own address; mysql and
// mariadb serve them on another port of the writer's, so the host is the same and only the port
// differs.
func (s *Service) readHost(ctx context.Context, ns, dbID, engine, writeHost string) string {
	if writeHost == "" {
		return ""
	}
	if !ReadEndpointCostsLB(engine) {
		return writeHost
	}
	vip, err := s.lbHost(ctx, ns, dbID+"-ro-lb")
	if err != nil {
		return ""
	}
	return vip
}

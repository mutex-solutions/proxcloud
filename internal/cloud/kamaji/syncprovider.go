package kamaji

import (
	"context"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// ClusterSyncProvider lists a project's kamaji clusters → KUBERNETES_CLUSTER cloud resources.
// It satisfies providers.Provider + ProjectScoped implicitly (no import — keeps the dependency
// arrow pointing kamaji→cloud only). One Application = one cached resource; the TCP + the CAPI
// MachineDeployments enrich it with live endpoint/health/replica counts.
type ClusterSyncProvider struct {
	svc       *Service
	region    string
	projectID string
}

// SyncProvider builds the read-sync provider for one project (syncjob leg).
func (s *Service) SyncProvider(region, projectID string) *ClusterSyncProvider {
	return &ClusterSyncProvider{svc: s, region: region, projectID: projectID}
}

func (p *ClusterSyncProvider) Type() string      { return cloud.TypeKubernetesCluster }
func (p *ClusterSyncProvider) ProjectID() string { return p.projectID }

func (p *ClusterSyncProvider) List(ctx context.Context) ([]cloud.CloudResource, error) {
	s := p.svc
	// Ownership marker in the selector: pre-proxcloud clusters on the same management cluster
	// (unlabelled) never enter the cache, so they never surface in the UI or billing.
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
		// would flash the cluster back into the UI and billing until the cascade finishes.
		if dig(app, "metadata", "deletionTimestamp") != nil {
			continue
		}
		// Best-effort enrichment: a half-provisioned cluster still syncs (status stays PROGRESSING).
		tcp, err := s.findTCP(ctx, ns, id)
		if err != nil {
			return nil, err
		}
		mds, err := s.api.ListMachineDeployments(ctx, ns, "cluster.x-k8s.io/cluster-name="+id)
		if err != nil {
			return nil, err
		}
		data := clusterData(app, tcp, mds)
		if c := data["cluster"].(map[string]any); settledButProgressing(app, c) {
			// A failed read keeps Argo's Progressing: the next sync tries again.
			if parts, err := s.addonParts(ctx, ns); err == nil && readyPastResourceSet(app, parts, time.Now()) {
				c["status"] = "READY"
			}
		}
		out = append(out, cloud.CloudResource{
			Type:       cloud.TypeKubernetesCluster,
			ExternalID: id,
			Region:     p.region,
			ProjectID:  p.projectID,
			Data:       data,
		})
	}
	return out, nil
}

// addonParts reads the namespace's HelmChartProxies and ClusterResourceSets, keyed Kind/name, for
// readyPastResourceSet.
func (s *Service) addonParts(ctx context.Context, ns string) (map[string]map[string]any, error) {
	parts := map[string]map[string]any{}
	for _, k := range []struct{ group, version, plural, kind string }{
		{"addons.cluster.x-k8s.io", "v1alpha1", "helmchartproxies", "HelmChartProxy"},
		{"addons.cluster.x-k8s.io", "v1beta2", "clusterresourcesets", "ClusterResourceSet"},
	} {
		items, err := s.api.ListCRs(ctx, k.group, k.version, k.plural, ns, "")
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			parts[k.kind+"/"+digStr(it, "metadata", "name")] = it
		}
	}
	return parts, nil
}

// clusterData maps Application (+ optional TCP/MachineDeployments) → the cached `data` payload.
// bson-round-trip-stable by construction: strings, bools and JSON numbers only, timestamps as
// the RFC3339 strings Kubernetes already serializes (never time.Time — the dev-era churn lesson).
func clusterData(app, tcp map[string]any, mds []map[string]any) map[string]any {
	values, _ := dig(app, "spec", "source", "helm", "valuesObject").(map[string]any)

	status := "PENDING"
	h := digStr(app, "status", "health", "status")
	if h != "" {
		// Argo health: Healthy / Progressing / Degraded / Suspended / Missing / Unknown.
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

	c := map[string]any{
		"id":            digStr(app, "metadata", "name"),
		"name":          digStr(app, "metadata", "annotations", AnnotationDisplayName),
		"version":       digStr(values, "kubernetesVersion"),
		"chart_version": digStr(app, "spec", "source", "targetRevision"),
		"status":        status,
		"sync_status":   digStr(app, "status", "sync", "status"),
		"created_at":    digStr(app, "metadata", "creationTimestamp"),
	}
	if replicas, ok := dig(values, "kamajiControlPlane", "replicas").(float64); ok {
		c["cp_replicas"] = replicas
	} else if replicas, ok := dig(values, "kamajiControlPlane", "replicas").(int); ok {
		c["cp_replicas"] = replicas
	}
	// The vnet of the workers, read back off the values. Immutable after create.
	if bridge := digStr(values, "proxmox", "network", "bridge"); bridge != "" {
		c["network_id"] = bridge
	}
	// What storage the cluster was given, so the page can say why a claim stays Pending: the
	// block classes proxmox-csi installs, and whether a NAS dataset backs ReadWriteMany.
	classes := []any{}
	if list, ok := dig(values, "proxmox", "csi", "storageClasses").([]any); ok {
		for _, raw := range list {
			if m, ok := raw.(map[string]any); ok && m["name"] != nil {
				classes = append(classes, m["name"])
			}
		}
	}
	c["storage_classes"] = classes
	c["rwx"] = digStr(values, "proxmox", "rwx", "server") != ""
	// Customer add-on toggles, read back for the UI (absent block = chart defaults). Filtered to
	// the curated menu: a legacy credential-push addon (and any operator hand-edit)
	// must not surface as a customer toggle — it would round-trip into SET_ADDONS and be
	// rejected as an unknown add-on.
	if adds, ok := dig(values, "addons").(map[string]any); ok {
		out := map[string]any{}
		for name, raw := range adds {
			if _, curated := ClusterAddons[name]; !curated {
				continue
			}
			if m, ok := raw.(map[string]any); ok {
				if enabled, ok := m["enabled"].(bool); ok {
					out[name] = enabled
				}
			}
		}
		if len(out) > 0 {
			c["addons"] = out
		}
	}
	if issuer := digStr(values, "oidc", "issuerUrl"); issuer != "" {
		c["oidc_issuer"] = issuer
		// Full block (snake_case like the rest of the payload) — the client OIDC edit form
		// prefills from it; keys mirror OIDCValues.
		oidc := map[string]any{"issuer_url": issuer}
		for src, dst := range map[string]string{
			"clientId": "client_id", "usernameClaim": "username_claim", "usernamePrefix": "username_prefix",
			"groupsClaim": "groups_claim", "groupsPrefix": "groups_prefix",
		} {
			if v := digStr(values, "oidc", src); v != "" {
				oidc[dst] = v
			}
		}
		c["oidc"] = oidc
	}

	// Control-plane endpoint from the TCP status (host:port the kubeconfig points at).
	if tcp != nil {
		if ep := digStr(tcp, "status", "controlPlaneEndpoint"); ep != "" {
			c["endpoint"] = ep
		}
		if v := digStr(tcp, "status", "kubernetesResources", "version", "status"); v != "" {
			c["cp_status"] = v
		}
	}

	// Desired node groups from values, live replica counts from the MachineDeployments. A group has
	// one per PVE node (the chart labels them), so the counts add up.
	live := map[string]map[string]any{}
	for _, md := range mds {
		name := digStr(md, "metadata", "labels", labelNodeGroup)
		if name == "" {
			continue
		}
		entry := live[name]
		if entry == nil {
			entry = map[string]any{}
			live[name] = entry
		}
		for src, dst := range map[string]string{"replicas": "replicas", "readyReplicas": "ready_replicas"} {
			if r, ok := dig(md, "status", src).(float64); ok {
				prev, _ := entry[dst].(float64)
				entry[dst] = prev + r
			}
		}
		// The least settled pool speaks for the group.
		if phase := digStr(md, "status", "phase"); phase != "" && entry["phase"] != "ScalingUp" && entry["phase"] != "ScalingDown" {
			entry["phase"] = phase
		}
	}
	groups := []any{}
	for _, g := range NodeGroupsFromValues(values) {
		name, _ := g["name"].(string)
		for k, v := range live[name] {
			g[k] = v
		}
		groups = append(groups, g)
	}
	c["node_groups"] = groups

	return map[string]any{"cluster": c}
}

// settledButProgressing: Argo still says Progressing, but the control plane, every node group
// and the sync are done. Only then are the add-on objects worth reading (readyPastResourceSet).
func settledButProgressing(app, c map[string]any) bool {
	groups, _ := c["node_groups"].([]any)
	return digStr(app, "status", "health", "status") == "Progressing" && c["sync_status"] == "Synced" &&
		c["cp_status"] == "Ready" && c["endpoint"] != nil && groupsReady(groups)
}

// resourceSetGrace bounds how long a cluster reads READY while its ClusterResourceSet has not
// applied: past it a CRS that never applies leaves the cluster PROGRESSING, as Argo says. Five lab
// creates turned Healthy 9.5 to 16 min after the Application was created. ponytail: a fixed value
// from the Application's creation; a provider setting if a slower site needs one.
const resourceSetGrace = 30 * time.Minute

// readyPastResourceSet reports whether the one part of the Application still not done is its
// ClusterResourceSet (the MetalLB address pool, plus the CSI config). It fails until MetalLB's
// webhook answers, and CAPI's retry backoff then leaves it unapplied for minutes after the
// cluster works: on the lab it applied 15 min after the Application was created, 8 min past
// CAPI's Available. Argo 3 no longer keeps per-resource health in app.status.resources, so the
// HelmChartProxies and the CRS that list names are read off the management cluster
// (parts, keyed Kind/name). Anything else not done, or a CRS past the grace, keeps Argo's answer.
func readyPastResourceSet(app map[string]any, parts map[string]map[string]any, now time.Time) bool {
	created, err := time.Parse(time.RFC3339, digStr(app, "metadata", "creationTimestamp"))
	if err != nil || now.Sub(created) > resourceSetGrace {
		return false
	}
	resources, _ := dig(app, "status", "resources").([]any)
	waiting := 0
	for _, raw := range resources {
		r, _ := raw.(map[string]any)
		kind, _ := r["kind"].(string)
		name, _ := r["name"].(string)
		obj := parts[kind+"/"+name]
		switch kind {
		case "HelmChartProxy":
			if !conditionTrue(obj, "Ready") {
				return false
			}
		case "ClusterResourceSet":
			if !conditionTrue(obj, "ResourcesApplied") {
				waiting++
			}
		}
	}
	return waiting == 1
}

func conditionTrue(obj map[string]any, typ string) bool {
	conds, _ := dig(obj, "status", "conditions").([]any)
	for _, raw := range conds {
		if c, _ := raw.(map[string]any); c["type"] == typ {
			return c["status"] == "True"
		}
	}
	return false
}

// groupsReady: every node group runs its desired count of Ready nodes.
func groupsReady(groups []any) bool {
	for _, raw := range groups {
		g, _ := raw.(map[string]any)
		ready, _ := g["ready_replicas"].(float64)
		if g["phase"] != "Running" || ready < count(g["count"]) {
			return false
		}
	}
	return true
}

// count reads a node count: float64 off JSON, int off Go-built values, 1 when unset.
func count(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 1
}

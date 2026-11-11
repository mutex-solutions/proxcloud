package project

// cloud_kamaji.go — the client write surface for kamaji Managed-Kubernetes clusters. A kamaji
// provider has no compute tenant of its own, so KUBERNETES_CLUSTER writes bypass the generic
// WriteService entirely (ceph precedent) and go through kamaji.Service: Application CR on the
// management cluster in, status via the sync. Worker VMs land in the project's own compute
// service (plan D4) — the create leg (cloud_kamaji_create.go) prepares the mgmt-side cloud
// credentials secret.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// newClusterID mints the platform identifier every k8s-side object is named by (Application,
// helm release, CAPI Cluster, TCP, DNS). Customer names are display-only (plan §9) — this id is
// what keeps duplicate names, unicode input and the RFC1123/63-char limit out of the system. The prefix is for a person
// reading a listing: clusters made earlier keep another one, so nothing may read a cluster from its id's shape.
func newClusterID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "pc-c-" + hex.EncodeToString(b)
}

// kamajiCreate handles the KUBERNETES_CLUSTER leg of cloudCreate for a kamaji provider.
func (h *Handler) kamajiCreate(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, req providers.CreateRequest) {
	if req.Type != cloud.TypeKubernetesCluster {
		h.fail(w, httpx.BadRequest("A Managed Kubernetes service only provisions KUBERNETES_CLUSTER resources"))
		return
	}
	if h.kamajiFor == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "managed kubernetes is not configured")
		return
	}
	ks, err := h.kamajiFor(es)
	if err != nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "managed kubernetes is not available")
		return
	}
	spec, err := kamajiSpecFromData(proj.ID, req.Data)
	if err != nil {
		h.fail(w, httpx.BadRequest(err.Error()))
		return
	}
	if err := spec.Validate(ks.Config().Defaults); err != nil {
		h.fail(w, httpx.BadRequest(err.Error()))
		return
	}
	if err := kamaji.ValidateCreateAddons(ks.Config().ChartVersion, spec.Addons); err != nil {
		h.fail(w, httpx.BadRequest(err.Error()))
		return
	}
	if err := kamaji.ValidateGatewayFabricKubernetes(spec.Version, spec.Addons); err != nil {
		h.fail(w, httpx.BadRequest(err.Error()))
		return
	}
	// GPU worker quota rides the same platform gate as servers (Phase K re-checks
	// against PVE mapping capacity too).
	if err := h.enforceGPUQuotaForNodeGroups(r.Context(), proj, es.ID, spec.NodeGroups, nil); err != nil {
		h.fail(w, err)
		return
	}
	// Detached like the other PVE write paths: a browser leaving mid-create must not strand tokens.
	ctx := context.WithoutCancel(r.Context())
	sec, err := h.kamajiProvisionCluster(ctx, proj, ks, &spec)
	if err != nil {
		h.fail(w, err)
		return
	}
	data, err := ks.CreateCluster(ctx, spec, sec)
	if err != nil {
		if rerr := ks.AbandonCluster(ctx, proj.ID, spec.ID, sec.ComputeServiceID); rerr != nil {
			slog.Warn("kamaji: release after a failed create, the sweep retries", "cluster", spec.ID, "err", rerr)
		}
		h.fail(w, err)
		return
	}
	now := time.Now().UTC()
	region := proj.ServiceRegion(es.ID)
	if region == "" {
		region = es.KamajiRegion()
	}
	cr := &cloud.CloudResource{
		ServiceID: es.ID, Region: region, ProjectID: proj.ID,
		Type: cloud.TypeKubernetesCluster, ExternalID: spec.ID,
		Data: data, CreatedAt: &now, UpdatedAt: &now,
	}
	saved, err := h.cloud.Insert(r.Context(), cr)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_CREATE", "", saved)
	httpx.OK(w, *saved)
}

// kamajiDelete handles the KUBERNETES_CLUSTER leg of cloudDelete: Application delete (ArgoCD's
// resources-finalizer cascades the rendered chart) + cache archive.
func (h *Handler) kamajiDelete(ctx context.Context, es *externalservice.ExternalService, proj *Project, cr *cloud.CloudResource) error {
	if h.kamajiFor == nil {
		return fmt.Errorf("managed kubernetes is not configured")
	}
	ks, err := h.kamajiFor(es)
	if err != nil {
		return err
	}
	if err := ks.DeleteCluster(ctx, proj.ID, cr.ExternalID); err != nil {
		return err
	}
	return h.cloud.DeleteAndArchive(ctx, cr, time.Now().UTC())
}

// kamajiAction serves the per-cluster actions; reports whether it handled the request
// (bucketSettingsAction precedent). Actions:
//   - GET_KUBECONFIG          → {result:{kubeconfig}} — fetched on demand, never stored (plan D5)
//   - UPGRADE {version}       → bumps kubernetesVersion + re-resolves node-group images; the CP
//     rolls first (Kamaji blue/green), node groups rotate off the same values change; guarded to
//     one minor at a time, never backwards
//   - SET_NODE_GROUPS {nodeGroups} → replaces the nodeGroups value (add/remove/resize/labels/taints)
//   - SET_OIDC {oidc}         → replaces the customer OIDC block; empty issuerUrl disables
func (h *Handler) kamajiAction(w http.ResponseWriter, r *http.Request, proj *Project, es *externalservice.ExternalService, cr *cloud.CloudResource, action string, data map[string]any) bool {
	if !es.IsKamaji() || cr.Type != cloud.TypeKubernetesCluster {
		return false
	}
	if h.kamajiFor == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "managed kubernetes is not configured")
		return true
	}
	ks, err := h.kamajiFor(es)
	if err != nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "managed kubernetes is not available")
		return true
	}
	switch action {
	case "GET_KUBECONFIG":
		kc, err := ks.AdminKubeconfig(r.Context(), proj.ID, cr.ExternalID)
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": map[string]any{"kubeconfig": string(kc)}})
		return true

	case "UPGRADE":
		version, _ := data["version"].(string)
		d := ks.Config().Defaults
		if version == "" {
			h.fail(w, httpx.BadRequest("version is required"))
			return true
		}
		if len(d.Versions) > 0 {
			if _, ok := d.Versions[version]; !ok {
				h.fail(w, httpx.BadRequest("version is not offered by this provider"))
				return true
			}
		}
		err := ks.PatchClusterValues(r.Context(), cr.ExternalID, func(values map[string]any) error {
			current, _ := values["kubernetesVersion"].(string)
			// One minor at a time, never backwards (plan §3.4) — validated against the live
			// values, not the cache, so a stale sync can't let a bad jump through.
			if err := kamaji.ValidateUpgradePath(current, version); err != nil {
				return httpx.BadRequest(err.Error())
			}
			values["kubernetesVersion"] = version
			// Node images follow the curated version→image matrix: rotate every group to the new
			// version's image (a MachineDeployment template rotation = CAPI rolling replace). A
			// group with an image VARIANT stays on that variant — resolved against the target
			// version, and the upgrade is refused when the variant has no image there, because
			// silently rolling a GPU pool onto the plain image is a broken pool with extra steps.
			if err := rotateNodeGroupImages(values, d, version); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "UPGRADING"})
		return true

	case "SET_NODE_GROUPS":
		groups, err := kamajiNodeGroups(data["nodeGroups"])
		if err != nil || len(groups) == 0 {
			h.fail(w, httpx.BadRequest("nodeGroups: at least one valid node group is required"))
			return true
		}
		d := ks.Config().Defaults
		if err := validateNodeGroupShapes(groups); err != nil {
			h.fail(w, httpx.BadRequest(err.Error()))
			return true
		}
		compute, err := h.kamajiCompute(r.Context(), proj, d)
		if err != nil {
			h.fail(w, err)
			return true
		}
		if err := h.KamajiCheckNodeGroups(r.Context(), proj, compute, d, cr, groups); err != nil {
			h.fail(w, err)
			return true
		}
		// GPU gate on the DELTA vs the cluster's current groups (their workers are already in
		// the usage count once synced as servers).
		if err := h.enforceGPUQuotaForNodeGroups(r.Context(), proj, es.ID, groups, nodeGroupsFromCache(cr)); err != nil {
			h.fail(w, err)
			return true
		}
		err = ks.PatchClusterValues(r.Context(), cr.ExternalID, func(values map[string]any) error {
			version, _ := values["kubernetesVersion"].(string)
			prevFlavor, prevImage, prevVariant := prevGroupIndex(values)
			// Flavor allowlist applies to NEW or flavor-CHANGED groups only — an admin narrowing
			// the allowlist must not brick resizes of groups that already run a since-removed
			// flavor.
			if len(d.Flavors) > 0 {
				for _, ng := range groups {
					if prevFlavor[ng.Name] == ng.FlavorID {
						continue
					}
					if !slices.Contains(d.Flavors, ng.FlavorID) {
						return httpx.BadRequest(fmt.Sprintf("node group %q: flavor %q is not offered by this provider", ng.Name, ng.FlavorID))
					}
				}
			}
			rendered := kamaji.NodeGroupValues(d, version, groups)
			if err := applyNodeGroupImages(rendered, prevImage, prevVariant, version); err != nil {
				return err
			}
			values["nodeGroups"] = rendered
			return nil
		})
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "UPDATING"})
		return true

	case "SET_OIDC":
		// Post-create OIDC (re)configuration — same chart block the create wizard sets; an empty
		// issuerUrl disables OIDC on the apiserver.
		oidc := map[string]string{}
		if m, ok := data["oidc"].(map[string]any); ok {
			for k, v := range m {
				if s := strAny(v); s != "" {
					oidc[k] = s
				}
			}
		}
		err := ks.PatchClusterValues(r.Context(), cr.ExternalID, func(values map[string]any) error {
			// signingAlgs is operator/advanced config the UI form doesn't carry — preserve the
			// existing value unless the request explicitly sets it.
			if _, sent := oidc["signingAlgs"]; !sent {
				if prev := digAnyStr(values, "oidc", "signingAlgs"); prev != "" {
					oidc["signingAlgs"] = prev
				}
			}
			if block := kamaji.OIDCValues(oidc); block != nil {
				values["oidc"] = block
			} else {
				delete(values, "oidc")
			}
			return nil
		})
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "UPDATING"})
		return true

	case "ROTATE_KUBECONFIG":
		if err := ks.RotateKubeconfig(r.Context(), proj.ID, cr.ExternalID); err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "ROTATING"})
		return true

	case "SET_ADDONS":
		// Post-create curated picks merge with current values, preserving choices omitted by older
		// clients and the separately retained legacy ingress release.
		addons, err := kamajiAddons(data["addons"])
		if err != nil {
			h.fail(w, httpx.BadRequest(err.Error()))
			return true
		}
		// Merge against the live pin and values: older clients must not erase a newer controller,
		// and legacy ingress traffic cannot be removed by an unrelated metrics/certificate edit.
		err = ks.PatchAddons(r.Context(), cr.ExternalID, addons)
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "UPDATING"})
		return true

	case "APPLY_PLATFORM_UPDATE":
		// Re-pins the cluster onto the provider's CURRENT chart version — the customer's opt-in
		// "platform update". The target is server-side authority (never client-supplied): the
		// only version on offer is the one the operator pinned on the provider.
		pin := ks.Config().ChartVersion
		if pin == "" {
			h.fail(w, httpx.BadRequest("no platform version is pinned on this provider"))
			return true
		}
		if err := ks.SetChartVersion(r.Context(), cr.ExternalID, pin); err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "UPDATING"})
		return true
	}
	return false
}

// validateNodeGroupShapes re-runs the create-time node-group SHAPE rules for SET_NODE_GROUPS
// (name/flavor required, count/min/max sanity) — allowlist enforcement happens inside the
// values patch where the cluster's current groups are known.
func validateNodeGroupShapes(groups []kamaji.NodeGroup) error {
	spec := kamaji.ClusterSpec{ID: "x", ProjectID: "x", Version: "x", NodeGroups: groups}
	return spec.Validate(kamaji.ClusterDefaults{})
}

// prevGroupIndex indexes the live values' nodeGroups by name → flavor / imageId / imageVariant.
func prevGroupIndex(values map[string]any) (flavor, image, variant map[string]string) {
	flavor, image, variant = map[string]string{}, map[string]string{}, map[string]string{}
	raw, _ := values["nodeGroups"].([]any)
	for _, r := range raw {
		g, ok := r.(map[string]any)
		if !ok {
			continue
		}
		name, _ := g["name"].(string)
		if name == "" {
			continue
		}
		flavor[name], _ = g["machineFlavor"].(string)
		image[name], _ = g["machineImageId"].(string)
		variant[name], _ = g["imageVariant"].(string)
	}
	return flavor, image, variant
}

// rotateNodeGroupImages re-points every rendered group at the target version's image of its OWN
// variant — the UPGRADE leg. Refused when a variant carries no image for the target; a missing
// DEFAULT image keeps the group's current one (the matrix may have dropped an old version, and
// UPGRADE already validated the target is offered).
func rotateNodeGroupImages(values map[string]any, d kamaji.ClusterDefaults, version string) error {
	groups, _ := values["nodeGroups"].([]any)
	for _, raw := range groups {
		g, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		variant, _ := g["imageVariant"].(string)
		img := d.ImageFor(version, variant)
		if img == "" {
			if variant != "" {
				name, _ := g["name"].(string)
				return httpx.BadRequest(fmt.Sprintf("node group %q: image variant %q is not offered for version %s — update the provider's image variants first", name, variant, version))
			}
			continue
		}
		g["machineImageId"] = img
	}
	return nil
}

// applyNodeGroupImages fills each rendered group's EMPTY machineImageId — the SET_NODE_GROUPS
// leg. A group whose variant is UNCHANGED keeps its previous image (an admin narrowing the
// matrix must not brick resizes of existing pools); a changed-variant or new group must resolve,
// else the request is refused — falling back would stamp a variant the pool doesn't actually
// run, and the values must never lie about what a pool boots.
func applyNodeGroupImages(rendered []any, prevImage, prevVariant map[string]string, version string) error {
	for _, raw := range rendered {
		g, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if img, _ := g["machineImageId"].(string); img != "" {
			continue
		}
		name, _ := g["name"].(string)
		variant, _ := g["imageVariant"].(string)
		if prev := prevImage[name]; prev != "" && variant == prevVariant[name] {
			g["machineImageId"] = prev
			continue
		}
		if variant != "" {
			return httpx.BadRequest(fmt.Sprintf("node group %q: image variant %q is not offered for version %s", name, variant, version))
		}
		return httpx.BadRequest(fmt.Sprintf("node group %q: no image for version %s — set an imageId or update the provider's version matrix", name, version))
	}
	return nil
}

// digAnyStr reads a nested string out of a free-form map (nil-safe).
func digAnyStr(m map[string]any, keys ...string) string {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	s, _ := cur.(string)
	return s
}

// nodeGroupsFromCache rebuilds the cluster's current node groups off the sync cache — the
// baseline the GPU-quota delta subtracts. It deliberately reports each group at its LIVE
// replica count when the sync has one (falling back to the declared count / autoscale min):
// gpuUsage counts actual worker VMs, so subtracting an autoscale group at max would credit
// headroom the project never held and let an edit slip past the limit.
func nodeGroupsFromCache(cr *cloud.CloudResource) []kamaji.NodeGroup {
	cl, _ := cr.Data["cluster"].(map[string]any)
	raw, _ := cl["node_groups"].([]any)
	out := make([]kamaji.NodeGroup, 0, len(raw))
	for _, r := range raw {
		g, ok := r.(map[string]any)
		if !ok {
			continue
		}
		ng := kamaji.NodeGroup{Name: strAny(g["name"]), FlavorID: strAny(g["flavor_id"]), RootVolumeGiB: intAny(g["root_volume_gib"])}
		autoscale, _ := g["autoscale"].(bool)
		switch {
		case g["replicas"] != nil:
			ng.Count = intAny(g["replicas"])
		case autoscale:
			ng.Count = intAny(g["min"])
		default:
			ng.Count = intAny(g["count"])
		}
		out = append(out, ng)
	}
	return out
}

// refreshKamajiClusters live-reconciles the project's KUBERNETES_CLUSTER cache off the
// management cluster — the client list's read-through, so a provisioning cluster's
// status/replica counts are current instead of up to 15 minutes stale (the services-sync
// cadence). Same Reconcile the sync job runs (project-label leak-guard included); best-effort,
// every failure just leaves the cache as the answer.
func (h *Handler) refreshKamajiClusters(ctx context.Context, p *Project) {
	if h.kamajiFor == nil {
		return
	}
	for _, svcID := range p.ServiceIDs() {
		es, err := h.esSvc.Get(ctx, svcID)
		if err != nil || es == nil || !es.IsKamaji() {
			continue
		}
		ks, err := h.kamajiFor(es)
		if err != nil {
			continue
		}
		if _, err := providers.Reconcile(ctx, ks.SyncProvider(es.KamajiRegion(), p.ID), h.cloud, es.ID, time.Now().UTC()); err != nil {
			slog.Warn("kamaji: live cluster list refresh", "project", p.ID, "service", svcID, "err", err)
		}
	}
}

// kamajiSpecFromData maps the client create body → ClusterSpec. Body shape:
//
//	{name, version, ha, oidc:{issuerUrl,clientId,...}, allowedCidrs:[...],
//	 nodeGroups:[{name, flavorId, count|min/max+autoscale, labels, taints}]}
func kamajiSpecFromData(projectID string, d map[string]any) (kamaji.ClusterSpec, error) {
	spec := kamaji.ClusterSpec{
		ID:          newClusterID(),
		ProjectID:   projectID,
		DisplayName: strAny(d["name"]),
		Version:     strAny(d["version"]),
	}
	if spec.DisplayName == "" {
		return spec, fmt.Errorf("name is required")
	}
	if ha, ok := d["ha"].(bool); ok {
		spec.HA = ha
	}
	// The create path verifies the network belongs to this project before anything is built.
	spec.NetworkID = strAny(d["networkId"])
	if oidc, ok := d["oidc"].(map[string]any); ok {
		spec.OIDC = map[string]string{}
		for k, v := range oidc {
			if s := strAny(v); s != "" {
				spec.OIDC[k] = s
			}
		}
	}
	if cidrs, ok := d["allowedCidrs"].([]any); ok {
		for _, c := range cidrs {
			if s := strAny(c); s != "" {
				spec.AllowedCIDRs = append(spec.AllowedCIDRs, s)
			}
		}
	}
	// Curated add-on toggles ({name: bool}); Validate rejects names off the menu.
	if raw, has := d["addons"]; has && raw != nil {
		addons, err := kamajiAddons(raw)
		if err != nil {
			return spec, err
		}
		spec.Addons = addons
	}
	groups, err := kamajiNodeGroups(d["nodeGroups"])
	if err != nil {
		return spec, err
	}
	spec.NodeGroups = groups
	return spec, nil
}

// kamajiAddons decodes the addons toggle map STRICTLY: a non-bool value is a 400, not a silent
// drop — {"metricsServer": "false"} silently ignored would install metrics-server (the chart
// default), the exact opposite of the request.
func kamajiAddons(v any) (map[string]bool, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("addons must be an object of {name: true|false}")
	}
	out := make(map[string]bool, len(m))
	for name, raw := range m {
		b, ok := raw.(bool)
		if !ok {
			return nil, fmt.Errorf("addons: %q must be true or false", name)
		}
		out[name] = b
	}
	return out, nil
}

// kamajiNodeGroups decodes a free-form nodeGroups array via a JSON round-trip into the typed
// slice (kamaji.NodeGroup carries the json tags).
func kamajiNodeGroups(v any) ([]kamaji.NodeGroup, error) {
	if v == nil {
		return nil, fmt.Errorf("nodeGroups is required")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("nodeGroups: %w", err)
	}
	var groups []kamaji.NodeGroup
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil, fmt.Errorf("nodeGroups: %w", err)
	}
	return groups, nil
}

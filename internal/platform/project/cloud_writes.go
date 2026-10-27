package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/client"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/paging"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// cloud_writes.go = the client write surface (client cloud-resource create/delete),
// project-membership + project:cloud_resource:manage gated. The dispatch + OpenStack write +
// cache live in providers.WriteService; this layer resolves the project's service + region and
// builds the per-request WriteService over the live CloudClient.

// resolveServiceID picks the externalService for a cloud write: the FE's `x-service-id` header (or
// `?serviceId=`) when the project is attached to it, else the project's first attached service.
// "" = the project has no cloud service (→ 400, the no-service guard).
func (h *Handler) resolveServiceID(r *http.Request, p *Project) string {
	if hd := r.Header.Get("x-service-id"); hd != "" && p.HasService(hd) {
		return hd
	}
	if q := r.URL.Query().Get("serviceId"); q != "" && p.HasService(q) {
		return q
	}
	ids := p.ServiceIDs()
	if len(ids) == 0 {
		return ""
	}
	// No explicit service → prefer an OpenStack one. A ceph-s3 provider serves ONLY object-store
	// (a kamaji provider ONLY kubernetes clusters, a dbaas provider ONLY databases), so the
	// generic cloud reads that fall through here (images, flavors, ports, security groups) would
	// get ErrNotOpenStack from them. Object-store/k8s/database callers either send x-service-id
	// (from the Location picker) or resolve the service from the resource itself.
	for _, id := range ids {
		if es, err := h.esSvc.Get(r.Context(), id); err == nil && es != nil && !es.IsCephS3() && !es.IsKamaji() && !es.IsDbaas() {
			return id
		}
	}
	return ids[0] // ceph/kamaji/dbaas-only project
}

// tenantWriteService builds the bucket WriteService for a ceph-s3 provider — the
// only kind that still writes through this seam (proxmox/kamaji/dbaas fork before
// reaching it).
func (h *Handler) tenantWriteService(ctx context.Context, w http.ResponseWriter, p *Project, svcID string) (*providers.WriteService, string, bool) {
	es, err := h.esSvc.Get(ctx, svcID)
	if err != nil || es == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "cloud service not available")
		return nil, "", false
	}
	region := p.ServiceRegion(svcID)
	if region == "" {
		region = h.cloudRegion
	}
	if !es.IsCephS3() {
		h.fail(w, httpx.BadRequest("Resource writes are not supported on this provider"))
		return nil, "", false
	}
	cc, err := h.cephClientForProject(ctx, es, p, region)
	if err != nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "cloud client not ready")
		return nil, "", false
	}
	return providers.NewWriteService(cc, h.cloud), region, true
}

// failWebsite maps the static-website surface's errors: asking a Swift bucket for a website is a client
// mistake (400 with the reason), not a server fault. Everything else goes through the normal error path.
func (h *Handler) failWebsite(w http.ResponseWriter, err error) {
	if errors.Is(err, client.ErrWebsiteUnsupported) {
		h.fail(w, httpx.BadRequest("Static website hosting is only available on S3 (Ceph) object storage"))
		return
	}
	h.fail(w, err)
}

// cephClientForProject builds a ceph-s3 CloudClient scoped to the project's RGW tenant-user, using the
// project's stored S3 keys (cephcred) for data I/O + the provider's admin keys for stats. Errors when the
// project has no stored credential (not provisioned onto this ceph service) or the store is unwired.
func (h *Handler) cephClientForProject(ctx context.Context, es *externalservice.ExternalService, p *Project, region string) (*client.Client, error) {
	if h.cephCreds == nil {
		return nil, fmt.Errorf("ceph credential store not configured")
	}
	cred, err := h.cephCreds.Get(ctx, p.ID, es.ID)
	if err != nil {
		return nil, err
	}
	if cred == nil {
		return nil, fmt.Errorf("project is not provisioned on the ceph object-store service")
	}
	return client.NewCephS3(ctx, es.CephConfig(region, cred.AccessKey, cred.SecretKey, cred.RGWUID))
}

// cloudResourceList handles POST /api/v1/project/{id}/resource?type=
// → the project's cached cloud resources, filtered by type. Project-membership-gated (read).
func (h *Handler) cloudResourceList(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, err := h.svc.GetProject(r.Context(), u.Sub, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	typ := r.URL.Query().Get("type")
	// Live-listed types (read straight from the project's tenant, like the synced cache — the
	// os-notification real-time sync is deferred). IMAGE feeds the wizard + the per-server Snapshots tab;
	// PORT/FLOATING_IP/SECURITY_GROUP feed the server-detail Networking + Security-Groups tabs (the
	// cache never holds a server's auto-created port/secgroup, so these must be live).
	switch typ {
	case cloud.TypeKubernetesCluster:
		// Read-through refresh: the services-sync cron runs every 15 minutes, far too stale for
		// a provisioning cluster (ArgoCD already Healthy while the UI still shows Degraded /
		// 0 ready nodes). Best-effort — the cache below is the answer either way.
		h.refreshKamajiClusters(r.Context(), proj)
	case cloud.TypeDatabaseCluster:
		// Same read-through for provisioning databases (endpoint appears minutes after create).
		h.refreshDbaasClusters(r.Context(), proj)
	case cloud.TypeKeypair:
		// KEYPAIR is a USER-scoped identity resource (blank projectId — see providers/write.go),
		// so FindAllByProjectID below never returns it. List by user (same uid the create path
		// stamps on cr.UserID), like resourceStats counts it.
		uid := u.ID
		if uid == "" {
			uid = u.Sub
		}
		pg, ok := paging.FromRequest(w, r)
		if !ok {
			return
		}
		if pg.Active {
			kps, next, prev, err := h.cloud.PageByUserAndType(r.Context(), uid, cloud.TypeKeypair, pg)
			if err != nil {
				h.fail(w, err)
				return
			}
			httpx.CursorList(w, kps, pg.Limit, next, prev)
			return
		}
		kps, err := h.cloud.FindAllByUserID(r.Context(), uid)
		if err != nil {
			h.fail(w, err)
			return
		}
		out := make([]cloud.CloudResource, 0, len(kps))
		for _, cr := range kps {
			if cr.Type == cloud.TypeKeypair {
				out = append(out, cr)
			}
		}
		httpx.List(w, out)
		return
	}
	pg, ok := paging.FromRequest(w, r)
	if !ok {
		return
	}
	if pg.Active {
		out, next, prev, err := h.cloud.PageByProjectAndType(r.Context(), proj.ID, typ, pg)
		if err != nil {
			h.fail(w, err)
			return
		}
		httpx.CursorList(w, out, pg.Limit, next, prev)
		return
	}
	resources, err := h.cloud.FindAllByProjectID(r.Context(), proj.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]cloud.CloudResource, 0, len(resources))
	for _, cr := range resources {
		if typ != "" && cr.Type != typ {
			continue
		}
		out = append(out, cr)
	}
	httpx.List(w, out)
}

// enrichServerFlavors best-effort resolves each cached server's flavor specs live (see
// enrichServerFlavor) — the list otherwise shows "undefined CPU, NaN GB RAM".
func (h *Handler) emitCloudList(w http.ResponseWriter, r *http.Request, out []cloud.CloudResource) {
	pg, err := paging.Parse(r.URL.Query())
	if err != nil {
		h.fail(w, err)
		return
	}
	if !pg.Active {
		httpx.List(w, out)
		return
	}
	page, next, prev := paging.KeysetSlice(out, pg, func(c cloud.CloudResource) string { return c.ID })
	httpx.CursorList(w, page, pg.Limit, next, prev)
}

// listPorts live-lists the project's Neutron ports as PORT CloudResources.
// When `?deviceId=` is set the list is scoped to that server's interfaces (the server-detail
// Networking tab). Empty list if the cloud is unreachable.
func liveCreatedAt(obj map[string]any) *time.Time {
	s, _ := obj["created_at"].(string)
	if s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05"} { // neutron omits the Z on some builds
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}

// resourceOwnedBy is the project-scoped ownership gate (§5/§7): a cloudResource is usable by a
// request only when it exists AND belongs to the acting project. A nil row (a raw/uncached id) or a
// row owned by another project is rejected, so a read/mutate/delete never runs against an id with
// no project binding.
func resourceOwnedBy(cr *cloud.CloudResource, projectID string) bool {
	return cr != nil && cr.ProjectID == projectID
}

// ownedResource loads a cloudResource cache doc by id and confirms it belongs to proj. ok=false for
// a miss, a non-cache raw id, or another project's resource — the caller 404s instead of acting on
// a raw externalId.
func (h *Handler) ownedResource(ctx context.Context, resourceID string, proj *Project) (*cloud.CloudResource, bool) {
	cr, _ := h.cloud.FindByID(ctx, resourceID)
	if !resourceOwnedBy(cr, proj.ID) {
		return nil, false
	}
	return cr, true
}

// ownedKeypair resolves a USER-scoped KEYPAIR (blank projectId — see providers/write.go, where
// create stamps userId instead) and authorizes it by the caller's user id, so a user can only act
// on their OWN keypair. This is the identity-resource analogue of resourceOwnedBy: keypairs never
// carry a projectId, so the project-scoped gate can never match them.
func (h *Handler) ownedKeypair(ctx context.Context, resourceID string, u *user.User) (*cloud.CloudResource, bool) {
	cr, _ := h.cloud.FindByID(ctx, resourceID)
	uid := u.ID
	if uid == "" {
		uid = u.Sub
	}
	if !keypairOwnedByUser(cr, uid) {
		return nil, false
	}
	return cr, true
}

// keypairOwnedByUser is the user-scoped ownership gate for KEYPAIR: a keypair carries no projectId
// and belongs to the user that minted it, so authorize strictly by a non-empty userId match (a
// blank uid must never match, else it would authorize across users).
func keypairOwnedByUser(cr *cloud.CloudResource, uid string) bool {
	return cr != nil && cr.Type == cloud.TypeKeypair && cr.ProjectID == "" &&
		uid != "" && cr.UserID == uid
}

// imageVisibleTo is the §27 glance-image filter, applied in EVERY listImages branch: an image is
// visible only when it is owned by the project's tenant (ownerID) AND — when scoped to a server's
// snapshots (assoc) — stamped with that instance_uuid. The owner check must never be skipped in the
// assoc branch, or a caller could read another tenant's snapshots by naming their instance uuid.
func (h *Handler) regionFor(p *Project, svcID string) string {
	if region := p.ServiceRegion(svcID); region != "" {
		return region
	}
	return h.cloudRegion
}

// requestRegion prefers the Location picker's x-region-id header over
// regionFor, so multi-region providers scope per-region catalogs correctly.
// The header value is client-controlled — consumers validate it against the
// provider's declared regions (ExternalService.CatalogRegion) before use.
func (h *Handler) requestRegion(r *http.Request, p *Project, svcID string) string {
	if hd := r.Header.Get("x-region-id"); hd != "" {
		return hd
	}
	return h.regionFor(p, svcID)
}

// listImages live-lists the project's glance images as IMAGE CloudResources:
// {externalId, type:IMAGE, region, serviceId, projectId, data:{image:{...}}}. The FE shows these
// as the OS options and sends the picked externalId as the create's imageId. Empty list (not an
// error) if the cloud is unreachable.
func (h *Handler) cloudGet(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, err := h.svc.GetProject(r.Context(), u.Sub, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	resourceID := chi.URLParam(r, "resourceId")
	cr, err := h.cloud.FindByID(r.Context(), resourceID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if cr != nil && cr.ProjectID != proj.ID {
		cr = nil
	}
	if cr == nil {
		h.fail(w, httpx.NotFound("Resource not found"))
		return
	}
	// Best-effort live refresh — never fail the read if the cloud is unreachable.
	if es, eerr := h.esSvc.Get(r.Context(), cr.ServiceID); eerr == nil && es != nil {
		switch {
		case es.IsProxmox() && cr.Type == cloud.TypeServer:
			if ps, perr := h.pveFor(es); perr == nil {
				h.pveRefreshServerRow(r.Context(), ps, proj, cr)
			}
		case es.IsCephS3() && cr.Type == cloud.TypeBucket:
			if cc, ok := h.tryTenantClient(r.Context(), proj, cr.ServiceID); ok {
				h.refreshBucketCache(r.Context(), cc, cr)
			}
		}
	}
	// Attach the monthly price (the FE server-detail Pricing tab reads model.vps.pricePlan). Set
	// AFTER the refresh/persist so the computed preview isn't written to the cache.
	h.attachPricePlan(r.Context(), proj, cr)
	httpx.OK(w, *cr)
}

// tryTenantClient builds a tenant-scoped cloud client, returning ok=false (no error written) on any
// failure — for best-effort live reads where the cached resource is the fallback.
func (h *Handler) tryTenantClient(ctx context.Context, p *Project, svcID string) (*client.Client, bool) {
	es, err := h.esSvc.Get(ctx, svcID)
	if err != nil || es == nil {
		return nil, false
	}
	region := p.ServiceRegion(svcID)
	if region == "" {
		region = h.cloudRegion
	}
	// ceph-s3 is the only kind with a client here (proxmox goes through pve.Service).
	if !es.IsCephS3() {
		return nil, false
	}
	cc, err := h.cephClientForProject(ctx, es, p, region)
	if err != nil {
		return nil, false
	}
	return cc, true
}

// refreshResource re-fetches the live cloud object for the supported types and merges it into the
// cached resource's Data, returning true when it updated cr (so the caller persists). Best-effort:
// a fetch error leaves the cache untouched (returns false).
func (h *Handler) cloudCreate(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectCloudResourceManage)
	if !ok {
		return
	}
	svcID := h.resolveServiceID(r, proj)
	if svcID == "" {
		h.fail(w, httpx.BadRequest("Project has no cloud service"))
		return
	}
	// kamaji managed-k8s: no Keystone tenant — KUBERNETES_CLUSTER creates go through the
	// kamaji service (Application CR on the management cluster), not the OpenStack WriteService.
	if es, err := h.esSvc.Get(r.Context(), svcID); err == nil && es != nil && es.IsKamaji() {
		var req providers.CreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			h.fail(w, httpx.BadRequest("invalid request body"))
			return
		}
		h.kamajiCreate(w, r, u, proj, es, req)
		return
	}
	// dbaas managed databases: same no-tenant rule — DATABASE_CLUSTER creates go through the
	// dbaas service (Application CR on the DB cluster), not the OpenStack WriteService.
	if es, err := h.esSvc.Get(r.Context(), svcID); err == nil && es != nil && es.IsDbaas() {
		var req providers.CreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			h.fail(w, httpx.BadRequest("invalid request body"))
			return
		}
		h.dbaasCreate(w, r, u, proj, es, req)
		return
	}
	// proxmox: no Keystone tenant — SERVER/KEYPAIR creates go through the pve service
	// (clone-from-template on the PVE cluster), not the OpenStack WriteService.
	if es, err := h.esSvc.Get(r.Context(), svcID); err == nil && es != nil && es.IsProxmox() {
		var req providers.CreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			h.fail(w, httpx.BadRequest("invalid request body"))
			return
		}
		h.pveCreate(w, r, u, proj, es, req.Type, req.Data)
		return
	}
	ws, region, ok := h.tenantWriteService(r.Context(), w, proj, svcID)
	if !ok {
		return
	}
	var req providers.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.fail(w, httpx.BadRequest("invalid request body"))
		return
	}
	if req.Data == nil {
		req.Data = map[string]any{}
	}
	uid := u.ID
	if uid == "" {
		uid = u.Sub
	}
	cr, err := ws.Create(r.Context(), svcID, region, proj.ID, uid, req)
	if err != nil {
		// The ceph-s3 bucket namespace is GLOBAL (no RGW tenants — see client.CephConfig), so a name may
		// already be owned by this or another project. That is a client-correctable conflict, not a 500.
		if errors.Is(err, client.ErrBucketNameTaken) {
			h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict,
				"Bucket name is already taken — S3 bucket names are globally unique"))
			return
		}
		if quotaErr := cloudQuotaConflict(err); quotaErr != nil {
			h.fail(w, quotaErr)
			return
		}
		h.fail(w, err)
		return
	}
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_CREATE", "", cr)
	httpx.OK(w, *cr)
}

func (h *Handler) cloudDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectCloudResourceManage)
	if !ok {
		return
	}
	svcID := h.resolveServiceID(r, proj)
	if svcID == "" {
		h.fail(w, httpx.BadRequest("Project has no cloud service"))
		return
	}
	// The FE passes the cloudResource CACHE id. §7: require an OWNED cache row and delete by ITS
	// externalId — never fall back to the raw param, or a caller could delete another project's
	// resource (WriteService.Delete resolves by {serviceId, externalId} with no project filter).
	resourceID := chi.URLParam(r, "resourceId")
	// A project-owned cache row, OR a USER-scoped KEYPAIR (blank projectId — see providers/write.go),
	// which resourceOwnedBy can't match; authorize the latter by the caller's user id so a user only
	// ever deletes their own. Either way WriteService.Delete keys on {serviceId, externalId}.
	cr, owned := h.ownedResource(r.Context(), resourceID, proj)
	if !owned {
		cr, owned = h.ownedKeypair(r.Context(), resourceID, u)
	}
	if owned {
		// Delete against the resource's OWN service (see cloudAction) — a header-resolved id could name
		// the other object-store backend, and WriteService.Delete keys on {serviceId, externalId}.
		if cr.ServiceID != "" {
			svcID = cr.ServiceID
		}
		// kamaji managed-k8s cluster: Application delete cascades the chart; archive the cache row.
		if cr.Type == cloud.TypeKubernetesCluster {
			if es, err := h.esSvc.Get(r.Context(), svcID); err == nil && es != nil && es.IsKamaji() {
				if err := h.kamajiDelete(r.Context(), es, proj, cr); err != nil {
					h.fail(w, err)
					return
				}
				h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_DELETE", "", cr)
				httpx.Accepted(w)
				return
			}
		}
		// dbaas managed database: same Application-delete cascade (the LB Service delete tears
		// down the tenant-side Octavia LB); the neutron share is revoked by the orphan sweep.
		if cr.Type == cloud.TypeDatabaseCluster {
			if es, err := h.esSvc.Get(r.Context(), svcID); err == nil && es != nil && es.IsDbaas() {
				if err := h.dbaasDelete(r.Context(), es, proj, cr); err != nil {
					h.fail(w, err)
					return
				}
				h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_DELETE", "", cr)
				httpx.Accepted(w)
				return
			}
		}
		// proxmox: destroy via the pve service, then archive the cache row here.
		if es, err := h.esSvc.Get(r.Context(), svcID); err == nil && es != nil && es.IsProxmox() {
			if cr.Type == cloud.TypeNetwork {
				if !h.pveDeleteNetwork(r.Context(), w, es, proj, cr) {
					return
				}
			} else if err := h.pveDeleteResource(r.Context(), es, proj, cr); err != nil {
				h.fail(w, httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, err.Error()))
				return
			}
			now := time.Now().UTC()
			if err := h.cloud.DeleteAndArchive(r.Context(), cr, now); err != nil {
				h.fail(w, err)
				return
			}
			h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_DELETE", "", cr)
			httpx.Accepted(w)
			return
		}
		ws, _, ok := h.tenantWriteService(r.Context(), w, proj, svcID)
		if !ok {
			return
		}
		if err := ws.Delete(r.Context(), svcID, cr.ExternalID); err != nil {
			h.fail(w, err)
			return
		}
		h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_DELETE", "", cr)
		httpx.Accepted(w)
		return
	}
	// No live-listed types exist on this fork — an unknown cache id is simply gone.
	h.fail(w, httpx.NotFound("Resource not found"))
}

// cloudBulkAction handles the collection-level action (NO resourceId):
// POST /api/v1/project/{id}/cloud/action {type, action, data} with x-service-id/x-region-id →
// CustomActionResponse {result}. Serves the create-server wizard's catalog actions live from the
// project's tenant: PUBLIC_IMAGES (glance images), PUBLIC_IMAGES_AS_CLOUD_RESOURCE, and
// LIST_AVAILABILITY_ZONES (nova AZs). Unknown actions return an empty result (non-fatal).
func (h *Handler) cloudBulkAction(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, err := h.svc.GetProject(r.Context(), u.Sub, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	var req struct {
		Type   string         `json:"type"`
		Action string         `json:"action"`
		Data   map[string]any `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.fail(w, httpx.BadRequest("invalid request body"))
		return
	}
	svcID := h.resolveServiceID(r, proj)
	// proxmox: catalog feeds come from the flavor catalog + template config, never a
	// tenant OpenStack client.
	if es, err := h.esSvc.Get(r.Context(), svcID); err == nil && es != nil && es.IsProxmox() {
		if h.pveBulkAction(w, r, proj, es, req.Action) {
			return
		}
		httpx.OK(w, map[string]any{"result": []any{}})
		return
	}
	// Non-proxmox providers (ceph-s3/kamaji/dbaas) serve no catalog bulk actions.
	var result any = []any{}
	httpx.OK(w, map[string]any{"result": result})
}

// autoPickExternalNetwork selects one external network for a project whose external-network picker
// is hidden (publicNetworksVisible=false). It respects the admin allow-list, and — for floating IPs
// — prefers the network a router already egresses through so the IP is routable. Returns "" when no
// allowed external network exists (the caller then fails the create as it would for a missing pool).
func timeFromAny(v any) *time.Time {
	switch t := v.(type) {
	case time.Time:
		if t.IsZero() {
			return nil
		}
		u := t.UTC()
		return &u
	case *time.Time:
		if t == nil || t.IsZero() {
			return nil
		}
		u := t.UTC()
		return &u
	case string:
		if t == "" {
			return nil
		}
		if parsed, err := time.Parse(time.RFC3339, t); err == nil && !parsed.IsZero() {
			u := parsed.UTC()
			return &u
		}
	}
	return nil
}

// cloudAction handles POST /cloud/{resourceId}/action with
// {action, data}. The FE passes the cloudResource CACHE doc id (not the externalId), so the
// resource is resolved by FindByID (the externalId is the fallback). READ actions (LIST_EVENTS,
// LIST_SECURITY_GROUPS, GET_PORTS) return live data; mutating actions go through WriteService.
func (h *Handler) cloudAction(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectCloudResourceManage)
	if !ok {
		return
	}
	svcID := h.resolveServiceID(r, proj)
	if svcID == "" {
		h.fail(w, httpx.BadRequest("Project has no cloud service"))
		return
	}
	// §5: require an OWNED cache row before acting. A raw external id (not a cache doc) or a row
	// owned by another project must never become the action target — both READ and mutating actions
	// would otherwise run against an id with no project binding.
	resourceID := chi.URLParam(r, "resourceId")
	cr, ok := h.ownedResource(r.Context(), resourceID, proj)
	if !ok {
		h.fail(w, httpx.NotFound("Resource not found"))
		return
	}
	// The action targets THIS resource, so it must run against the service the resource actually lives on
	// — not whatever x-service-id the request happened to carry. Swift and ceph-s3 object stores run side
	// by side permanently, so a header-resolved service can point at the wrong object-store backend (and
	// for a ceph client every OpenStack action would return ErrNotOpenStack).
	if cr.ServiceID != "" {
		svcID = cr.ServiceID
	}
	externalID := cr.ExternalID
	var req struct {
		Action string         `json:"action"`
		Type   string         `json:"type"`
		Data   map[string]any `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.fail(w, httpx.BadRequest("invalid request body"))
		return
	}

	// ceph-s3 bucket configuration + per-key grants (GET_SETTINGS, SET_VERSIONING, SET_QUOTA, …).
	// Handled first: they act on THIS bucket and already run against cr.ServiceID.
	if h.bucketSettingsAction(w, r, proj, svcID, externalID, req.Action, req.Data) {
		return
	}

	// kamaji managed-k8s cluster actions (GET_KUBECONFIG, UPGRADE, SET_NODE_GROUPS) — same
	// act-on-THIS-resource rule, against the resource's own (kamaji) service.
	if cr.Type == cloud.TypeKubernetesCluster {
		if es, err := h.esSvc.Get(r.Context(), svcID); err == nil && es != nil {
			if h.kamajiAction(w, r, proj, es, cr, req.Action, req.Data) {
				return
			}
		}
	}

	// dbaas managed database actions (GET_CONNECTION_INFO, RESIZE, SCALE_REPLICAS, …).
	if cr.Type == cloud.TypeDatabaseCluster {
		if es, err := h.esSvc.Get(r.Context(), svcID); err == nil && es != nil {
			if h.dbaasAction(w, r, proj, es, cr, req.Action, req.Data) {
				return
			}
		}
	}

	// proxmox: power/resize/rename/console — dispatched off the resource's own service.
	if es, err := h.esSvc.Get(r.Context(), svcID); err == nil && es != nil && es.IsProxmox() {
		if h.pveAction(w, r, u, proj, es, cr, req.Action, req.Data) {
			return
		}
		h.fail(w, httpx.BadRequest(fmt.Sprintf("action %q is not supported on a Proxmox provider", req.Action)))
		return
	}

	// READ actions — return live data, no cache mutation.
	switch req.Action {
	case "LIST_OBJECTS":
		// Object-storage explore: the bucket's objects/folders.
		// data.folderName scopes to a sub-path. Empty on error/empty bucket.
		objs := []map[string]any{}
		if cc, ok := h.tryTenantClient(r.Context(), proj, svcID); ok {
			if list, err := cc.ListBucketObjects(r.Context(), externalID, strAny(req.Data["folderName"])); err == nil {
				objs = list
			}
		}
		httpx.OK(w, map[string]any{"result": objs})
		return
	case "IS_BUCKET_PUBLIC":
		// Object-storage: the bucket's read ACL → public?
		pub := false
		if cc, ok := h.tryTenantClient(r.Context(), proj, svcID); ok {
			if v, err := cc.IsBucketPublic(r.Context(), externalID); err == nil {
				pub = v
			}
		}
		httpx.OK(w, map[string]any{"result": pub})
		return
	case "LIST_APIS":
		// Object-storage: the bucket's Swift + S3 access URLs
		// (swiftApis, s3Apis).
		swiftApis, s3Apis := []string{}, []string{}
		if cc, ok := h.tryTenantClient(r.Context(), proj, svcID); ok {
			if sw, s3, err := cc.BucketAPIs(r.Context(), externalID); err == nil {
				swiftApis, s3Apis = sw, s3
			}
		}
		httpx.OK(w, map[string]any{"result": map[string]any{"swiftApis": swiftApis, "s3Apis": s3Apis}})
		return
	case "GET_WEBSITE":
		// Object-storage (ceph-s3 only): the bucket's static-website state
		// {enabled, indexDocument, errorDocument, url, publicObjects}. Swift → 400 (no s3website API).
		cc, ok := h.tryTenantClient(r.Context(), proj, svcID)
		if !ok {
			httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "cloud client not ready")
			return
		}
		site, err := cc.GetBucketWebsite(r.Context(), externalID)
		if err != nil {
			h.failWebsite(w, err)
			return
		}
		httpx.OK(w, map[string]any{"result": site})
		return
	case "DOWNLOAD":
		// Object-storage: mint a short-lived download token (DOWNLOAD) + return its URL. The FE
		// opens result.url → GET /api/v1/download/{token} streams the object.
		if h.downloads == nil {
			h.fail(w, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "download not configured"))
			return
		}
		tok, err := h.downloads.Create(r.Context(), &cloud.CloudDownload{
			Type: cloud.DownloadTypeSwiftObject, ServiceID: svcID, ProjectID: proj.ID, ExternalID: externalID,
			Metadata: map[string]string{"objectName": strAny(req.Data["objectName"])},
		})
		if err != nil {
			h.fail(w, err)
			return
		}
		url := strings.TrimRight(h.apiBaseURL, "/") + "/api/v1/download/" + tok.ID
		httpx.OK(w, map[string]any{"result": map[string]any{"url": url}})
		return
	case "CREATE_FOLDER":
		// Object-storage: create a pseudo-folder marker (createFolder). The
		// FE sends data.folderName. noContent → empty result.
		cc, ok := h.tryTenantClient(r.Context(), proj, svcID)
		if !ok {
			httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "cloud client not ready")
			return
		}
		if err := cc.CreateFolder(r.Context(), externalID, strAny(req.Data["folderName"])); err != nil {
			h.fail(w, err)
			return
		}
		httpx.OK(w, map[string]any{"result": map[string]any{}})
		return
	case "ENABLE_WEBSITE":
		// Object-storage (ceph-s3 only): serve the bucket as a static website.
		// data{indexDocument?, errorDocument?} (index defaults to index.html).
		// ⚠ This ALSO makes every object in the bucket anonymously readable (bucket policy on
		// s3:GetObject) — the result carries publicObjects:true so the UI states it plainly.
		cc, ok := h.tryTenantClient(r.Context(), proj, svcID)
		if !ok {
			httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "cloud client not ready")
			return
		}
		site, err := cc.EnableBucketWebsite(r.Context(), externalID,
			strAny(req.Data["indexDocument"]), strAny(req.Data["errorDocument"]))
		if err != nil {
			h.failWebsite(w, err)
			return
		}
		h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", req.Action, cr)
		httpx.OK(w, map[string]any{"result": site})
		return
	case "DISABLE_WEBSITE":
		// Object-storage (ceph-s3 only): stop serving the website AND remove the public-read policy.
		cc, ok := h.tryTenantClient(r.Context(), proj, svcID)
		if !ok {
			httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "cloud client not ready")
			return
		}
		if err := cc.DisableBucketWebsite(r.Context(), externalID); err != nil {
			h.failWebsite(w, err)
			return
		}
		h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", req.Action, cr)
		httpx.OK(w, map[string]any{"result": &client.BucketWebsite{}})
		return
	case "DELETE_OBJECT":
		// Object-storage: delete an object (or folder + contents) by name (DELETE_OBJECT,
		// startsWith). Then refresh the bucket's size/count cache.
		cc, ok := h.tryTenantClient(r.Context(), proj, svcID)
		if !ok {
			httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "cloud client not ready")
			return
		}
		if err := cc.DeleteBucketObject(r.Context(), externalID, strAny(req.Data["objectName"])); err != nil {
			h.fail(w, err)
			return
		}
		h.refreshBucketCache(r.Context(), cc, cr)
		httpx.OK(w, map[string]any{"result": map[string]any{}})
		return
	case "UPDATE_OBJECT":
		// Object-storage: update an object's metadata (UPDATE_OBJECT). Returns the refreshed cr.
		cc, ok := h.tryTenantClient(r.Context(), proj, svcID)
		if !ok {
			httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "cloud client not ready")
			return
		}
		if err := cc.UpdateBucketObject(r.Context(), externalID, strAny(req.Data["objectName"]), strMap(req.Data["metadata"])); err != nil {
			h.fail(w, err)
			return
		}
		h.refreshBucketCache(r.Context(), cc, cr)
		httpx.OK(w, map[string]any{"result": bucketResult(cr)})
		return
	case "UPDATE_BUCKET_METADATA":
		// Object-storage: update the container's metadata (UPDATE_BUCKET_METADATA). Returns cr.
		cc, ok := h.tryTenantClient(r.Context(), proj, svcID)
		if !ok {
			httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "cloud client not ready")
			return
		}
		if err := cc.UpdateBucketMetadata(r.Context(), externalID, strMap(req.Data["metadata"])); err != nil {
			h.fail(w, err)
			return
		}
		h.refreshBucketCache(r.Context(), cc, cr)
		httpx.OK(w, map[string]any{"result": bucketResult(cr)})
		return
	case "MAKE_BUCKET_PUBLIC":
		// Object-storage: grant public read (.r:*,.rlistings — makeBucketPublic). result=true.
		cc, ok := h.tryTenantClient(r.Context(), proj, svcID)
		if !ok {
			httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "cloud client not ready")
			return
		}
		if err := cc.SetBucketRead(r.Context(), externalID, ".r:*,.rlistings"); err != nil {
			h.fail(w, err)
			return
		}
		httpx.OK(w, map[string]any{"result": true})
		return
	case "MAKE_BUCKET_PRIVATE":
		// Object-storage: remove public read (makeBucketPrivate, ""). result=false.
		cc, ok := h.tryTenantClient(r.Context(), proj, svcID)
		if !ok {
			httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "cloud client not ready")
			return
		}
		if err := cc.SetBucketRead(r.Context(), externalID, ""); err != nil {
			h.fail(w, err)
			return
		}
		httpx.OK(w, map[string]any{"result": false})
		return
	case "GET_OBJECT_ACL", "UPDATE_OBJECT_ACL", "GENERATE_TEMP_URL":
		// Object-storage ACL / presigned-URL actions are S3-only (need the S3 client);
		// this region is Swift → not supported → 501 (throws UnsupportedOperation).
		httpx.Err(w, http.StatusNotImplemented, http.StatusNotImplemented, "object ACL / temp-url is S3-only, not supported on Swift")
		return
	}

	h.fail(w, httpx.BadRequest(fmt.Sprintf("action %q is not supported", req.Action)))
}

func (h *Handler) cloudMetadata(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectCloudResourceManage)
	if !ok {
		return
	}
	cr, _ := h.cloud.FindByID(r.Context(), chi.URLParam(r, "resourceId"))
	if cr == nil || cr.ProjectID != proj.ID {
		h.fail(w, httpx.NotFound("Resource not found"))
		return
	}
	var meta map[string]string
	if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
		h.fail(w, httpx.BadRequest("invalid request body"))
		return
	}
	// Server metadata was a nova surface; nothing equivalent exists on this fork yet.
	_ = meta
	h.fail(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented, "Server metadata is not available on this provider"))
}

// cloudObjectDownload resolves a download token
// (whitelisted — the token IS the auth, no bearer) → streams the cloud object's bytes. Currently the
// Swift-object type (object-store DOWNLOAD action).
func (h *Handler) cloudObjectDownload(w http.ResponseWriter, r *http.Request) {
	if h.downloads == nil {
		h.fail(w, httpx.NotFound("Download not found"))
		return
	}
	tok, err := h.downloads.ByID(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if tok == nil {
		h.fail(w, httpx.NotFound("Download not found"))
		return
	}
	proj, err := h.svc.GetProjectByID(r.Context(), tok.ProjectID)
	if err != nil || proj == nil {
		h.fail(w, httpx.NotFound("Download not found"))
		return
	}
	cc, ok := h.tryTenantClient(r.Context(), proj, tok.ServiceID)
	if !ok {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "cloud client not ready")
		return
	}
	switch tok.Type {
	case cloud.DownloadTypeSwiftObject:
		objectName := tok.Metadata["objectName"]
		data, ct, err := cc.DownloadObject(r.Context(), tok.ExternalID, objectName)
		if err != nil {
			h.fail(w, err)
			return
		}
		fn := objectName
		if i := strings.LastIndex(fn, "/"); i >= 0 {
			fn = fn[i+1:]
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Disposition", "attachment; filename="+fn)
		_, _ = w.Write(data)
	default:
		h.fail(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented, "download type not supported"))
	}
}

// emitProxiedConsole turns a nova remote_console ({protocol,type,url}) into the response the
// client gets: a short-lived console token plus OUR proxied WebSocket endpoint. Nova's console
// host is never surfaced to the browser — that is what keeps it off the public internet.
// Shared by the VNC (REMOTECONTROL) and SERIAL (SERIAL_CONSOLE) actions.
func (h *Handler) emitProxiedConsole(
	w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, cr *cloud.CloudResource,
	svcID, externalID, action string, console map[string]any,
) {
	novaURL := strAny(console["url"])
	if novaURL == "" {
		h.fail(w, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "no console URL returned"))
		return
	}
	if h.downloads == nil {
		h.fail(w, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "console not configured"))
		return
	}
	// Fail fast on a console URL we could never proxy, rather than handing the client a token
	// whose socket would 502 on connect.
	if _, err := consoleBackend(novaURL); err != nil {
		slog.Warn("nova returned an unusable console url", "err", err)
		httpx.Err(w, http.StatusBadGateway, http.StatusBadGateway, "console backend unavailable")
		return
	}
	tok, err := h.downloads.CreateWithTTL(r.Context(), &cloud.CloudDownload{
		Type: cloud.DownloadTypeConsole, ServiceID: svcID, ProjectID: proj.ID, ExternalID: externalID,
		Metadata: map[string]string{"consoleUrl": novaURL},
	}, cloud.ConsoleTokenTTL)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
	httpx.OK(w, map[string]any{"result": map[string]any{
		"protocol": console["protocol"],
		"type":     console["type"],
		"url":      h.consoleWebsocketURL(tok.ID),
	}})
}

// consoleWebsocketURL builds the PUBLIC endpoint the browser's noVNC client connects to.
// apiBaseURL is http(s); noVNC requires a ws(s) scheme.
func (h *Handler) consoleWebsocketURL(token string) string {
	base := strings.TrimRight(h.apiBaseURL, "/")
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	}
	return base + "/api/v1/console/" + token + "/websockify"
}

// consoleBackend derives the novncproxy WebSocket target from the nova console URL. Nova returns
// e.g. https://host:6080/vnc_lite.html?path=%3Ftoken%3D<uuid>, where the `path` query is what
// noVNC appends to that origin for its socket (usually "?token=<uuid>").
func consoleBackend(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("console: unparseable nova url %q", raw)
	}
	// Normalise to the transport's scheme: the VNC console arrives as an http(s) page URL, the
	// SERIAL console as a ws(s) socket URL, and ReverseProxy dials http(s) in both cases (it
	// performs the Upgrade itself). Anything else is a malformed/unexpected console URL and would
	// surface as an opaque "unsupported protocol scheme" from the transport.
	scheme := u.Scheme
	switch scheme {
	case "http", "https":
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	default:
		return nil, fmt.Errorf("console: unsupported nova url scheme %q", u.Scheme)
	}
	// A PVE vncwebsocket URL is already the socket target (port + vncticket query) —
	// pass it through with only the scheme normalized.
	if u.Query().Get("vncticket") != "" {
		return &url.URL{Scheme: scheme, Host: u.Host, Path: u.Path, RawQuery: u.RawQuery}, nil
	}
	p := u.Query().Get("path")
	if p == "" {
		// Some deployments hand back the token directly rather than a `path` blob. Without either
		// there is nothing to authenticate the socket with, so refuse rather than proxy "?token=".
		tok := u.Query().Get("token")
		if tok == "" {
			return nil, fmt.Errorf("console: nova url carries neither a path nor a token")
		}
		p = "?token=" + tok
	}
	path, rawQuery := p, ""
	if i := strings.Index(p, "?"); i >= 0 {
		path, rawQuery = p[:i], p[i+1:]
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return &url.URL{Scheme: scheme, Host: u.Host, Path: path, RawQuery: rawQuery}, nil
}

// cloudConsoleProxy reverse-proxies the VNC WebSocket to the project's novncproxy. Whitelisted —
// the console token IS the auth (same model as the download route: a bearer header cannot ride on
// a WebSocket handshake). This is what keeps the OpenStack console service OFF the public
// internet: the browser only ever talks to ProxCloud. httputil.ReverseProxy carries the HTTP/1.1
// Upgrade through natively.
func (h *Handler) cloudConsoleProxy(w http.ResponseWriter, r *http.Request) {
	if h.downloads == nil {
		h.fail(w, httpx.NotFound("Console not found"))
		return
	}
	tok, err := h.downloads.ByID(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		h.fail(w, err)
		return
	}
	// Type check keeps a download token from being replayed as a console token (and vice versa).
	if tok == nil || tok.Type != cloud.DownloadTypeConsole {
		h.fail(w, httpx.NotFound("Console not found"))
		return
	}
	backend, err := consoleBackend(tok.Metadata["consoleUrl"])
	if err != nil {
		slog.Warn("console token carries an unusable nova url", "err", err)
		httpx.Err(w, http.StatusBadGateway, http.StatusBadGateway, "console backend unavailable")
		return
	}
	// A PVE console backend needs the provider's API-token header on the upgrade
	// request and (usually) its pinned self-signed TLS — resolved fresh here, never
	// persisted in the token.
	backendAuth := ""
	var transport http.RoundTripper
	if tok.Metadata["pve"] == "1" {
		es, eerr := h.esSvc.Get(r.Context(), tok.ServiceID)
		if eerr != nil || es == nil || !es.IsProxmox() {
			httpx.Err(w, http.StatusBadGateway, http.StatusBadGateway, "console backend unavailable")
			return
		}
		cfg := es.PVEConfig()
		backendAuth = pve.AuthHeaderFor(cfg)
		transport = &http.Transport{TLSClientConfig: pve.TLSConfigFor(cfg)}
	}
	// ponytail: default transport (TLS verified). The novncproxy already serves a
	// browser-trusted cert today; add an insecure escape hatch only if a region needs it.
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Director: func(req *http.Request) {
			req.URL.Scheme, req.URL.Host = backend.Scheme, backend.Host
			req.URL.Path, req.URL.RawQuery = backend.Path, backend.RawQuery
			req.Host = backend.Host
			// The console token in the URL is the ONLY intended credential. This endpoint shares an
			// origin with the API, so the browser attaches its ProxCloud cookies to the handshake —
			// never forward the caller's session/bearer down to the OpenStack console service.
			for _, h := range []string{"Authorization", "Cookie", "X-Api-Key"} {
				req.Header.Del(h)
			}
			if backendAuth != "" {
				req.Header.Set("Authorization", backendAuth)
			}
			// novncproxy may enforce [console] allowed_origins; present as same-origin so the
			// operator never has to allow-list the ProxCloud hostname.
			req.Header.Set("Origin", backend.Scheme+"://"+backend.Host)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			slog.Warn("console proxy failed", "err", err)
			httpx.Err(w, http.StatusBadGateway, http.StatusBadGateway, "console backend unavailable")
		},
	}
	proxy.ServeHTTP(w, r)
}

// cloudImageUpload handles POST /{projectId}/image/{imageId}/upload
// with the raw image bytes as the body → streams into the existing glance image.
func (h *Handler) cloudUploadBucketFile(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectCloudResourceManage)
	if !ok {
		return
	}
	cr, _ := h.cloud.FindByID(r.Context(), chi.URLParam(r, "resourceId"))
	if cr == nil || cr.ProjectID != proj.ID {
		h.fail(w, httpx.NotFound("Resource not found"))
		return
	}
	objectName := r.URL.Query().Get("objectName")
	if r.ContentLength < 0 {
		h.fail(w, httpx.BadRequest("Content-Length header is required for bucket object uploads"))
		return
	}
	cc, ok := h.tryTenantClient(r.Context(), proj, cr.ServiceID)
	if !ok {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "cloud client not ready")
		return
	}
	if err := cc.UploadBucketObject(r.Context(), cr.ExternalID, objectName, r.Header.Get("Content-Type"), r.ContentLength, r.Body); err != nil {
		h.fail(w, err)
		return
	}
	h.refreshBucketCache(r.Context(), cc, cr)
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", "UPLOAD_OBJECT", cr)
	w.WriteHeader(http.StatusNoContent)
}

// extID resolves a cloudResource CACHE id to its OpenStack externalId (FindByID), falling back to the
// id itself when it is not a cache doc.
func strAny(v any) string { s, _ := v.(string); return s }

// strMap coerces a free-form JSON object into a map[string]string (stringifying scalar values),
// for the object/bucket metadata maps the FE sends in action data.
func strMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]string{}
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		out[k] = strAny(val)
	}
	return out
}

// refreshBucketCache re-fetches the bucket's live size/count and persists the updated cache doc
// (a resource sync after a mutating object action). Best-effort, no-op for a nil cr.
func (h *Handler) refreshBucketCache(ctx context.Context, cc *client.Client, cr *cloud.CloudResource) {
	if cr == nil {
		return
	}
	b, err := cc.GetBucket(ctx, cr.ExternalID)
	if err != nil || b == nil {
		return
	}
	cr.Data = b
	if saved, err := h.cloud.Insert(ctx, cr); err == nil {
		*cr = *saved
	}
}

// bucketResult returns the cached resource for an action that yields the updated resource;
// empty object when the bucket isn't cached.
func bucketResult(cr *cloud.CloudResource) any {
	if cr == nil {
		return map[string]any{}
	}
	return *cr
}

// intAny coerces a free-form JSON number/string to an int (0 when not numeric).
func intAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// strSlice coerces a free-form JSON array to a []string (nil when not an array of strings).
func strSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// firstStr returns the first present non-empty string value among keys k.
func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// prettyAction humanizes a nova instance-action name (e.g. "reboot_hard" → "Reboot Hard").
func prettyAction(a string) string {
	if a == "" {
		return a
	}
	words := strings.FieldsFunc(a, func(r rune) bool { return r == '_' || r == '-' || r == ' ' })
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
	}
	return strings.Join(words, " ")
}

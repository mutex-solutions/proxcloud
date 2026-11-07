package project

// cloud_bucket_settings.go — the bucket configuration + S3-key surface of the object-store providers.
//
// Bucket configuration rides on the existing cloud-action endpoint
// (POST /project/{id}/cloud/{resourceId}/action) because every one of these acts on a BUCKET resource and
// must therefore run against the service that bucket actually lives on (cloudAction rebinds svcID to
// cr.ServiceID). A backend without the feature gets a 400 (client.ObjectCaps).
//
// S3 keys are project-scoped (not bucket-scoped) — RGW attaches keys to USERS, and a bucket grants access
// to a principal — so they get their own REST endpoints.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/cephcred"
	"github.com/mutex-solutions/proxcloud/internal/cloud/client"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// decodeInto re-decodes a free-form action `data` map into a typed request struct.
func decodeInto(data map[string]any, out any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// failBucketFeature maps the S3-only sentinel to a 400 (asking a non-S3 bucket for it is a client mistake),
// and a bad object-lock mode / quota argument to a 400 too.
func (h *Handler) failBucketFeature(w http.ResponseWriter, err error) {
	if errors.Is(err, client.ErrBucketFeatureUnsupported) {
		h.fail(w, httpx.BadRequest("This object storage does not offer this setting"))
		return
	}
	if errors.Is(err, client.ErrProjectQuotaExceeded) {
		h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, err.Error()))
		return
	}
	if errors.Is(err, client.ErrObjectStoreBusy) {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, err.Error())
		return
	}
	// A malformed hand-edited policy is the CALLER's mistake — surface the parse detail as a 400 so the
	// UI shows why, instead of a 500 (or worse, the old silent "saved nothing" success).
	if errors.Is(err, client.ErrInvalidBucketPolicy) {
		h.fail(w, httpx.BadRequest(err.Error()))
		return
	}
	h.fail(w, err)
}

// bucketSettingsAction handles the BUCKET configuration actions. Returns handled=false when `action` is not
// one of them, so cloudAction can fall through to its own dispatch.
func (h *Handler) bucketSettingsAction(w http.ResponseWriter, r *http.Request, proj *Project, svcID, externalID, action string, data map[string]any) bool {
	needClient := func() (*client.Client, bool) {
		cc, ok := h.tryTenantClient(r.Context(), proj, svcID)
		if !ok {
			httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "cloud client not ready")
			return nil, false
		}
		return cc, true
	}
	ctx := r.Context()

	switch action {
	case "GET_SETTINGS":
		cc, ok := needClient()
		if !ok {
			return true
		}
		s, err := cc.GetBucketSettings(ctx, externalID)
		if err != nil {
			h.failBucketFeature(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": s})
		return true

	case "SET_VERSIONING":
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := decodeInto(data, &req); err != nil {
			h.fail(w, httpx.BadRequest("invalid request body"))
			return true
		}
		cc, ok := needClient()
		if !ok {
			return true
		}
		if err := cc.SetBucketVersioning(ctx, externalID, req.Enabled); err != nil {
			h.failBucketFeature(w, err)
			return true
		}
		return h.bucketSettingsOK(w, r, proj, cc, externalID)

	case "SET_OBJECT_LOCK":
		// Default retention on a bucket that was CREATED with object lock. COMPLIANCE is refused by the
		// client (it would make the project undeletable).
		var req struct {
			Mode string `json:"mode"`
			Days int32  `json:"days"`
		}
		if err := decodeInto(data, &req); err != nil {
			h.fail(w, httpx.BadRequest("invalid request body"))
			return true
		}
		cc, ok := needClient()
		if !ok {
			return true
		}
		if err := cc.SetObjectLockDefaults(ctx, externalID, req.Mode, req.Days); err != nil {
			h.failBucketFeature(w, err)
			return true
		}
		return h.bucketSettingsOK(w, r, proj, cc, externalID)

	case "SET_QUOTA":
		var req struct {
			MaxSizeBytes int64 `json:"maxSizeBytes"`
			MaxObjects   int64 `json:"maxObjects"`
			Enabled      bool  `json:"enabled"`
		}
		if err := decodeInto(data, &req); err != nil {
			h.fail(w, httpx.BadRequest("invalid request body"))
			return true
		}
		cc, ok := needClient()
		if !ok {
			return true
		}
		if err := cc.SetBucketQuota(ctx, externalID, req.MaxSizeBytes, req.MaxObjects, req.Enabled); err != nil {
			h.failBucketFeature(w, err)
			return true
		}
		return h.bucketSettingsOK(w, r, proj, cc, externalID)

	case "SET_LIFECYCLE":
		var req struct {
			Rules []client.LifecycleRule `json:"rules"`
		}
		if err := decodeInto(data, &req); err != nil {
			h.fail(w, httpx.BadRequest("invalid request body"))
			return true
		}
		cc, ok := needClient()
		if !ok {
			return true
		}
		if err := cc.SetBucketLifecycle(ctx, externalID, req.Rules); err != nil {
			h.failBucketFeature(w, err)
			return true
		}
		return h.bucketSettingsOK(w, r, proj, cc, externalID)

	case "SET_CORS":
		var req struct {
			Rules []client.CORSRule `json:"rules"`
		}
		if err := decodeInto(data, &req); err != nil {
			h.fail(w, httpx.BadRequest("invalid request body"))
			return true
		}
		cc, ok := needClient()
		if !ok {
			return true
		}
		if err := cc.SetBucketCORS(ctx, externalID, req.Rules); err != nil {
			h.failBucketFeature(w, err)
			return true
		}
		return h.bucketSettingsOK(w, r, proj, cc, externalID)

	case "SET_TAGS":
		var req struct {
			Tags map[string]string `json:"tags"`
		}
		if err := decodeInto(data, &req); err != nil {
			h.fail(w, httpx.BadRequest("invalid request body"))
			return true
		}
		cc, ok := needClient()
		if !ok {
			return true
		}
		if err := cc.SetBucketTags(ctx, externalID, req.Tags); err != nil {
			h.failBucketFeature(w, err)
			return true
		}
		return h.bucketSettingsOK(w, r, proj, cc, externalID)

	case "GET_POLICY":
		cc, ok := needClient()
		if !ok {
			return true
		}
		pol, err := cc.GetBucketPolicyJSON(ctx, externalID)
		if err != nil {
			h.failBucketFeature(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": map[string]any{"policyJson": pol}})
		return true

	case "SET_POLICY":
		// Raw policy passthrough. ProxCloud-managed statements (website public-read, per-key grants) are
		// preserved regardless of what the caller sends — see client.SetBucketPolicyJSON.
		var req struct {
			PolicyJSON string `json:"policyJson"`
		}
		if err := decodeInto(data, &req); err != nil {
			h.fail(w, httpx.BadRequest("invalid request body"))
			return true
		}
		cc, ok := needClient()
		if !ok {
			return true
		}
		if err := cc.SetBucketPolicyJSON(ctx, externalID, req.PolicyJSON); err != nil {
			h.failBucketFeature(w, err)
			return true
		}
		return h.bucketSettingsOK(w, r, proj, cc, externalID)

	case "GRANT_KEY", "REVOKE_KEY":
		// Granting/revoking a key's access to a bucket IS credential-access management — require the same
		// api_access permission as the key routes (F2), even though the enclosing cloud action only needs
		// manage. Everything else on this dispatch (versioning, quota, lifecycle, …) stays manage-level.
		if err := h.policy.RequireProjectPermission(ctx, httpx.RC(ctx).Sub, proj, rbac.ProjectCloudResourceAPIAcc); err != nil {
			h.fail(w, err)
			return true
		}
		var req struct {
			KeyID      string `json:"keyId"`
			Permission string `json:"permission"`
		}
		if err := decodeInto(data, &req); err != nil {
			h.fail(w, httpx.BadRequest("invalid request body"))
			return true
		}
		if h.cephKeys == nil {
			httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "s3 keys not configured")
			return true
		}
		// Ownership: the key must belong to THIS project on THIS service.
		key, err := h.cephKeys.GetOwned(ctx, req.KeyID, proj.ID, svcID)
		if err != nil {
			h.fail(w, err)
			return true
		}
		if key == nil {
			h.fail(w, httpx.NotFound("S3 key not found"))
			return true
		}
		cc, ok := needClient()
		if !ok {
			return true
		}
		if action == "REVOKE_KEY" {
			err = cc.RevokeBucketAccess(ctx, externalID, key.RGWUID)
		} else {
			err = cc.GrantBucketAccess(ctx, externalID, key.RGWUID, client.BucketPermission(req.Permission))
		}
		if err != nil {
			h.failBucketFeature(w, err)
			return true
		}
		return h.bucketSettingsOK(w, r, proj, cc, externalID)
	}
	return false
}

// bucketSettingsOK re-reads and returns the full settings so the UI always renders live state.
func (h *Handler) bucketSettingsOK(w http.ResponseWriter, r *http.Request, proj *Project, cc *client.Client, bucket string) bool {
	s, err := cc.GetBucketSettings(r.Context(), bucket)
	if err != nil {
		h.failBucketFeature(w, err)
		return true
	}
	httpx.OK(w, map[string]any{"result": s})
	return true
}

// --- project-scoped S3 keys ---

// s3KeyDTO never leaks the secret except on the create response and the explicit credentials read.
// Warning is set only by rotate when the NEW key works but the old one could not be retired.
type s3KeyDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	RGWUID    string `json:"rgwUid"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey,omitempty"`
	CreatedAt any    `json:"createdAt,omitempty"`
	Warning   string `json:"warning,omitempty"`
}

// cephServiceOfProject resolves the project's ceph-s3 service for the S3-key endpoints.
//
// It must NOT go through resolveServiceID: that helper deliberately prefers a Proxmox service for the
// unheadered case (ceph serves object-store only), so on the normal side-by-side setup it would hand back
// the Proxmox service and these endpoints would 400 "not an S3 service". Instead: honour an explicit
// x-service-id ONLY when it names a ceph service the project is attached to, otherwise pick the project's
// ceph-s3 service.
func (h *Handler) cephServiceOfProject(ctx context.Context, w http.ResponseWriter, p *Project, r *http.Request) (*client.Client, string, bool) {
	isCeph := func(id string) bool {
		es, err := h.esSvc.Get(ctx, id)
		return err == nil && es != nil && es.IsObjectStore()
	}
	svcID := ""
	if hd := r.Header.Get("x-service-id"); hd != "" && p.HasService(hd) && isCeph(hd) {
		svcID = hd
	}
	if svcID == "" {
		for _, id := range p.ServiceIDs() {
			if isCeph(id) {
				svcID = id
				break
			}
		}
	}
	if svcID == "" {
		h.fail(w, httpx.BadRequest("Project has no object storage service"))
		return nil, "", false
	}
	cc, ok := h.tryTenantClient(ctx, p, svcID)
	if !ok {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "cloud client not ready")
		return nil, "", false
	}
	return cc, svcID, true
}

// s3Credentials handles GET /project/{id}/s3-credentials → the project's OWN S3 keys + endpoints, i.e. the
// credentials the customer points aws-cli at. Gated on project:cloud_resource:api_access — the credentials
// gate, deliberately NOT :manage (F2) — even though it returns a secret key.
func (h *Handler) s3Credentials(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectCloudResourceAPIAcc) // credentials → api_access, not manage (F2)
	if !ok {
		return
	}
	cc, svcID, ok := h.cephServiceOfProject(r.Context(), w, proj, r)
	if !ok {
		return
	}
	if h.cephCreds == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "ceph credentials not configured")
		return
	}
	cred, err := h.cephCreds.Get(r.Context(), proj.ID, svcID)
	if err != nil || cred == nil {
		h.fail(w, httpx.NotFound("Project is not provisioned on the object storage service"))
		return
	}
	s3Endpoint, websiteEndpoint, region := cc.Endpoints()
	h.projectAudit(u, proj, "CLOUD_RESOURCE_ACTION")
	httpx.OK(w, map[string]any{
		"accessKey": cred.AccessKey, "secretKey": cred.SecretKey, "rgwUid": cred.RGWUID,
		"s3Endpoint": s3Endpoint, "websiteEndpoint": websiteEndpoint, "region": region,
	})
}

// s3KeyList handles GET /project/{id}/s3-keys → the project's EXTRA keys (secrets included: the customer
// owns them, and the gate is project:cloud_resource:api_access, same as s3Credentials).
func (h *Handler) s3KeyList(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectCloudResourceAPIAcc) // credentials → api_access, not manage (F2)
	if !ok {
		return
	}
	_, svcID, ok := h.cephServiceOfProject(r.Context(), w, proj, r)
	if !ok {
		return
	}
	if h.cephKeys == nil {
		httpx.List(w, []any{})
		return
	}
	keys, err := h.cephKeys.List(r.Context(), proj.ID, svcID)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]s3KeyDTO, 0, len(keys))
	for i := range keys {
		out = append(out, s3KeyDTO{
			ID: keys[i].ID, Name: keys[i].Name, RGWUID: keys[i].RGWUID,
			AccessKey: keys[i].AccessKey, SecretKey: keys[i].SecretKey, CreatedAt: keys[i].CreatedAt,
		})
	}
	httpx.List(w, out)
}

// s3KeyCreate handles POST /project/{id}/s3-keys {name} → provisions an extra RGW user and returns its
// access/secret keys. The uid is always "<projectUid>-<name>", enforced by client.assertOwnedUID.
func (h *Handler) s3KeyCreate(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectCloudResourceAPIAcc) // credentials → api_access, not manage (F2)
	if !ok {
		return
	}
	if err := refuseNewUsage(proj); err != nil {
		h.fail(w, err)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.fail(w, httpx.BadRequest("invalid request body"))
		return
	}
	if err := cephcred.ValidateKeyName(req.Name); err != nil {
		h.fail(w, httpx.BadRequest(err.Error()))
		return
	}
	cc, svcID, ok := h.cephServiceOfProject(r.Context(), w, proj, r)
	if !ok {
		return
	}
	if h.cephKeys == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "s3 keys not configured")
		return
	}
	uid := cc.ChildUID(req.Name)
	access, secret, err := cc.CreateChildKey(r.Context(), uid, "proxcloud-"+proj.ID+"-"+req.Name)
	if err != nil {
		h.failBucketFeature(w, err)
		return
	}
	key := &cephcred.S3Key{
		ProjectID: proj.ID, ServiceID: svcID, Name: req.Name, RGWUID: uid,
		AccessKey: access, SecretKey: secret,
	}
	if err := h.cephKeys.Save(r.Context(), key); err != nil {
		h.fail(w, err)
		return
	}
	h.projectAudit(u, proj, "CLOUD_RESOURCE_CREATE")
	httpx.OK(w, s3KeyDTO{ID: key.ID, Name: key.Name, RGWUID: key.RGWUID,
		AccessKey: key.AccessKey, SecretKey: key.SecretKey, CreatedAt: key.CreatedAt})
}

// s3CredentialsRotate handles POST /project/{id}/s3-credentials/rotate: issue a new access/secret pair for
// the PROJECT's own RGW user and retire the old one. Any tool still using the old key breaks immediately —
// that is what rotation means.
func (h *Handler) s3CredentialsRotate(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectCloudResourceAPIAcc) // credentials → api_access, not manage (F2)
	if !ok {
		return
	}
	if err := refuseNewUsage(proj); err != nil {
		h.fail(w, err)
		return
	}
	cc, svcID, ok := h.cephServiceOfProject(r.Context(), w, proj, r)
	if !ok {
		return
	}
	if h.cephCreds == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "ceph credentials not configured")
		return
	}
	cred, err := h.cephCreds.Get(r.Context(), proj.ID, svcID)
	if err != nil || cred == nil {
		h.fail(w, httpx.NotFound("Project is not provisioned on the object storage service"))
		return
	}
	access, secret, rotErr := cc.RotateKey(r.Context(), cred.RGWUID, cred.AccessKey)
	if access == "" {
		h.failBucketFeature(w, rotErr)
		return
	}
	// Persist the NEW key even when retiring the old one failed — otherwise we would lose the working key.
	cred.AccessKey, cred.SecretKey = access, secret
	if err := h.cephCreds.Save(r.Context(), cred); err != nil {
		h.fail(w, err)
		return
	}
	h.projectAudit(u, proj, "CLOUD_RESOURCE_ACTION")
	s3Endpoint, websiteEndpoint, region := cc.Endpoints()
	out := map[string]any{
		"accessKey": access, "secretKey": secret, "rgwUid": cred.RGWUID,
		"s3Endpoint": s3Endpoint, "websiteEndpoint": websiteEndpoint, "region": region,
	}
	if rotErr != nil {
		out["warning"] = rotErr.Error() // new key works; the old one is still live
	}
	httpx.OK(w, out)
}

// s3KeyRotate handles POST /project/{id}/s3-keys/{keyId}/rotate for an EXTRA key. Grants are attached to
// the RGW user (not the key), so bucket access survives a rotation untouched.
func (h *Handler) s3KeyRotate(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectCloudResourceAPIAcc) // credentials → api_access, not manage (F2)
	if !ok {
		return
	}
	if err := refuseNewUsage(proj); err != nil {
		h.fail(w, err)
		return
	}
	cc, svcID, ok := h.cephServiceOfProject(r.Context(), w, proj, r)
	if !ok {
		return
	}
	if h.cephKeys == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "s3 keys not configured")
		return
	}
	key, err := h.cephKeys.GetOwned(r.Context(), chi.URLParam(r, "keyId"), proj.ID, svcID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if key == nil {
		h.fail(w, httpx.NotFound("S3 key not found"))
		return
	}
	access, secret, rotErr := cc.RotateKey(r.Context(), key.RGWUID, key.AccessKey)
	if access == "" {
		h.failBucketFeature(w, rotErr)
		return
	}
	key.AccessKey, key.SecretKey = access, secret
	if err := h.cephKeys.Save(r.Context(), key); err != nil {
		h.fail(w, err)
		return
	}
	h.projectAudit(u, proj, "CLOUD_RESOURCE_ACTION")
	// One stable response shape: the key DTO, with `warning` set only when the old key could not be retired.
	dto := s3KeyDTO{ID: key.ID, Name: key.Name, RGWUID: key.RGWUID,
		AccessKey: key.AccessKey, SecretKey: key.SecretKey, CreatedAt: key.CreatedAt}
	if rotErr != nil {
		dto.Warning = rotErr.Error()
	}
	httpx.OK(w, dto)
}

// s3KeyDelete handles DELETE /project/{id}/s3-keys/{keyId}: revoke the key's grants from every bucket of
// the project FIRST (so no policy is left naming a principal that no longer exists), then purge the RGW
// user and drop the stored document.
func (h *Handler) s3KeyDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectCloudResourceAPIAcc) // credentials → api_access, not manage (F2)
	if !ok {
		return
	}
	cc, svcID, ok := h.cephServiceOfProject(r.Context(), w, proj, r)
	if !ok {
		return
	}
	if h.cephKeys == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "s3 keys not configured")
		return
	}
	key, err := h.cephKeys.GetOwned(r.Context(), chi.URLParam(r, "keyId"), proj.ID, svcID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if key == nil {
		h.fail(w, httpx.NotFound("S3 key not found"))
		return
	}
	// Best-effort grant cleanup across the project's cached buckets on this service.
	if buckets, err := h.cloud.FindByProjectAndType(r.Context(), proj.ID, cloud.TypeBucket); err == nil {
		for i := range buckets {
			if buckets[i].ServiceID != svcID {
				continue
			}
			_ = cc.RevokeBucketAccess(r.Context(), buckets[i].ExternalID, key.RGWUID)
		}
	}
	if err := cc.DeleteChildKey(r.Context(), key.RGWUID); err != nil {
		h.failBucketFeature(w, err)
		return
	}
	if err := h.cephKeys.Delete(r.Context(), key.ID); err != nil {
		h.fail(w, err)
		return
	}
	h.projectAudit(u, proj, "CLOUD_RESOURCE_DELETE")
	httpx.Accepted(w)
}

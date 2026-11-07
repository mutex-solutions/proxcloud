package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// projectmut.go implements the MUTATIONS (+ the two datastore-only reads) of the project surface
// (/api/v1/admin/project) that are not already registered in handler.go. The reads
// GET /project, /project/{id}, /project/by-user, /project/by-organization,
// /project/{billingProfileId}/billing-profile and /project/external-services/{externalServiceId}
// are ALREADY registered there and are intentionally NOT re-registered here.
//
// CLOUD LEGS: the endpoints that hit the cloud run LIVE through h.projectCloud (ProjectCloudOps,
// wired in cmd/api — nil → they degrade to their original 501 responses, which is what the unit tests
// exercise):
//
//   - POST   /project                                  (create; optional bootstrap provision)
//   - POST   /project/{id}/sync                         (syncjob.SyncOne — whole-project / scoped)
//   - POST   /project/{id}/{status}  (ENABLED|DISABLED) (PVE suspend/resume + status flip; resume async + sync)
//   - GET    /project/{id}/external-service/{esid}      (bootstrap onto the explicit service)
//   - GET    /project/{id}/resources/counts            (cache aggregation — already live)
//
//   - DELETE /project/{id}      (schedule deletion → CanDelete pre-check → flip SCHEDULED_FOR_DELETION)
//   - DELETE /project/{id}/now  (delete now → flag DELETE_IN_PROGRESS with a past schedule → the deletion job's
//                                next sweep runs project.Handler.TeardownProject and retries it until it is clean)
//
// IN SCOPE (no cloud, pure datastore): GET /project/{id}/members, PUT /project/{id} (field-set),
// DELETE /project/{id}/cancel (status flip to ENABLED), and the no-cloud branches of
// POST /project/{id}/{status}.
//
// Audit: every mutation also writes an admin audit event — deferred (// TODO(audit)).

const projectCollection = "project"

// project perms (exact permission keys).
const (
	projectReadPerm   = "admin:project:read"
	projectCreatePerm = "admin:project:create"
	projectUpdatePerm = "admin:project:update"
	projectDeletePerm = "admin:project:delete"
)

// routeProjectMut registers ONLY the new project mutation + missing-read routes. The {id}
// param name reuses the one handler.go already uses on /project/{id} (chi requires a single param
// name at a given path position). The static second segments (members / sync / now / cancel /
// external-service) take precedence over the {status} param route at the same position.
func (h *Handler) routeProjectMut(r chi.Router) {
	r.Post("/project", h.projectCreate)
	r.Get("/project/{id}/members", h.projectMembers)
	r.Get("/project/{id}/resources/counts", h.projectResourceCounts)
	r.Get("/project/{id}/gpu-usage", h.projectGPUUsage)
	r.Get("/project/{id}/external-service/{externalServiceId}", h.projectAddExternalService)
	r.Post("/project/{id}/sync", h.projectSync)
	r.Put("/project/{id}", h.projectUpdate)
	r.Put("/project/{id}/quota", h.projectSetQuota)
	r.Put("/project/{id}/public-networks", h.projectSetPublicNetworks)
	r.Put("/project/{id}/gpu-capacity-visible", h.projectSetGPUCapacityVisible)
	r.Delete("/project/{id}", h.projectScheduleDeletion)
	r.Delete("/project/{id}/now", h.projectDeleteNow)
	r.Delete("/project/{id}/cancel", h.projectCancelDeletion)
	r.Post("/project/{id}/{status}", h.projectUpdateStatus)
}

// projectIDNotFound is the exact 404 message
// "Project with id %s not found " (trailing space, interpolated). This is the
// message used by update / updateStatus / schedule-deletion. (Note: the GET /{id} read uses
// a DIFFERENT message and is registered in handler.go.)
func projectIDNotFound(id string) *httpx.HTTPError {
	return httpx.NotFound(fmt.Sprintf("Project with id %s not found ", id))
}

// findProjectOr404 loads a project by id or writes the exact 404; returns (doc, ok).
func (h *Handler) findProjectOr404(w http.ResponseWriter, r *http.Request, id string) (pgdoc.M, bool) {
	doc, err := h.repo.FindDoc(r.Context(), projectCollection, id)
	if httpx.WriteError(w, err) {
		return nil, false
	}
	if doc == nil {
		httpx.WriteError(w, projectIDNotFound(id))
		return nil, false
	}
	return doc, true
}

// ── create ──────────────────────────────────────────────────────────────────────────────────────

// projectCreate validates the request →
// organization (404) + its billingProfileId (400) + userIds exist (404) → then:
// resolve the effective billing profile (validated but NOT stored on the project — the builder
// omits it), build the Project (ENABLED, org-OWNER membership), save, optionally bootstrap the
// external service (create-or-reuse the provider tenant via projectCloud.Bootstrap), add the
// userIds as MEMBERs, save, audit CREATE. Any failure below →
// 500 "Error creating project".
func (h *Handler) projectCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, projectCreatePerm) {
		return
	}
	var req createProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	// Required-field validation → 400. projectName + organizationId are required.
	if req.ProjectName == "" {
		httpx.WriteError(w, httpx.BadRequest("Project name cannot be empty"))
		return
	}
	if req.OrganizationId == "" {
		httpx.WriteError(w, httpx.BadRequest("Organization ID cannot be empty"))
		return
	}
	if req.ExternalServiceId != "" && (h.projectCloud == nil || h.projectCloud.Bootstrap == nil) {
		// Provision requested but the cloud leg is unwired (tests / degraded boot) → 501, BEFORE
		// any persist so the create stays all-or-nothing.
		httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented,
			"project create (external-service provisioning) not implemented"))
		return
	}
	ctx := r.Context()
	org, err := h.repo.FindDoc(ctx, "organization", req.OrganizationId)
	if httpx.WriteError(w, err) {
		return
	}
	if org == nil {
		httpx.WriteError(w, httpx.NotFound("Organization not found"))
		return
	}
	orgBpID, _ := org["billingProfileId"].(string)
	if orgBpID == "" {
		httpx.WriteError(w, httpx.BadRequest("Organization does not have a billing profile configured"))
		return
	}
	// Up-front userIds existence check (looped lookup, 404 before anything persists).
	for _, uid := range req.UserIds {
		u, err := h.repo.FindDoc(ctx, "users", uid)
		if httpx.WriteError(w, err) {
			return
		}
		if u == nil {
			httpx.WriteError(w, httpx.NotFound("User not found: "+uid))
			return
		}
	}
	// ── any failure below maps to 500 "Error creating project" ──
	createFailed := func() {
		httpx.WriteError(w, httpx.NewError(http.StatusInternalServerError, http.StatusInternalServerError,
			"Error creating project"))
	}
	bpID := orgBpID
	if req.BillingProfileId != "" {
		bpID = req.BillingProfileId
	}
	bp, err := h.repo.FindDoc(ctx, "billingProfile", bpID)
	if err != nil || bp == nil {
		createFailed() // missing billing profile → 500
		return
	}
	// Owner membership = the organization's OWNER member (400 when the org has none). Member docs
	// carry `roles: []string` (the role is roles[0]); the array-contains match is the datastore
	// equivalent — live-caught on the dev226 drill (`role` matched nothing → false "no owner").
	ownerDoc, err := h.repo.FindOneBy(ctx, "organization_members",
		pgdoc.M{"organizationId": req.OrganizationId, "roles": pgdoc.M{"$contains": "OWNER"}})
	if httpx.WriteError(w, err) {
		return
	}
	if ownerDoc == nil {
		httpx.WriteError(w, httpx.BadRequest("Organization has no owner"))
		return
	}
	ownerSub, _ := ownerDoc["sub"].(string)
	memberships := pgdoc.A{pgdoc.M{"sub": ownerSub, "role": "OWNER"}}
	// userIds join as MEMBER (skip anyone already a member, i.e. the owner).
	for _, uid := range req.UserIds {
		u, _ := h.repo.FindDoc(ctx, "users", uid)
		sub := ""
		if u != nil {
			if s, ok := u["sub"].(string); ok && s != "" {
				sub = s
			} else if s, ok := u["_id"].(string); ok {
				sub = s
			}
		}
		if sub == "" || sub == ownerSub {
			continue
		}
		dup := false
		for _, m := range memberships {
			if mm, ok := m.(pgdoc.M); ok && mm["sub"] == sub {
				dup = true
				break
			}
		}
		if !dup {
			memberships = append(memberships, pgdoc.M{"sub": sub, "role": "MEMBER"})
		}
	}
	doc := pgdoc.M{
		"name":           req.ProjectName,
		"organizationId": req.OrganizationId,
		"customInfo":     pgdoc.M{},
		"status":         "ENABLED",
		"memberships":    memberships,
		"services":       pgdoc.A{},
	}
	// InsertDoc assigns the id (a pgdoc hex string) and returns the doc carrying it as `_id`.
	created, err := h.repo.InsertDoc(ctx, projectCollection, doc)
	if err != nil {
		createFailed()
		return
	}
	pid, _ := created["_id"].(string)
	if req.ExternalServiceId != "" {
		// Bootstrap with an explicit service.
		if err := h.projectCloud.Bootstrap(ctx, pid, req.ExternalServiceId); err != nil {
			createFailed()
			return
		}
	}
	after, err := h.repo.FindDoc(ctx, projectCollection, pid)
	if err != nil || after == nil {
		createFailed()
		return
	}
	// CREATE PROJECT audit (middleware emits the admin event; snapshot = the created doc).
	audit.RecordSnapshots(ctx, nil, after)
	httpx.OK(w, shapeDoc(after))
}

// createProjectRequest is the create-project request body.
type createProjectRequest struct {
	ProjectName       string   `json:"projectName"`
	BillingProfileId  string   `json:"billingProfileId"`
	OrganizationId    string   `json:"organizationId"`
	ExternalServiceId string   `json:"externalServiceId"`
	UserIds           []string `json:"userIds"`
}

// ── reads (datastore-only / not wired) ──────────────────────────────────────────────────────────────────

// projectMembers lists a project's members: load the project → for each membership, resolve the
// user by sub → list of User. Pure datastore (no cloud). Resolves the project first (404 via the
// "The project with id %s was not found. " message already used by the registered GET /{id},
// which differs from the mutation 404 message).
func (h *Handler) projectMembers(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, projectReadPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	proj, err := h.repo.FindDoc(r.Context(), projectCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if proj == nil {
		// Missing project → the "The project with id %s was not found. " message.
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("The project with id %s was not found. ", id)))
		return
	}
	// Resolve each membership.sub → the User doc. Under greenfield the User lookup subsystem is the
	// users collection; we resolve the members directly from the users collection by sub so the list
	// matches the by-sub user lookup. Missing users are skipped (greenfield projects have valid owner
	// subs).
	subs := membershipSubs(proj)
	members := []pgdoc.M{}
	for _, sub := range subs {
		u, err := h.repo.FindOneBy(r.Context(), "users", pgdoc.M{"_id": sub})
		if httpx.WriteError(w, err) {
			return
		}
		if u == nil {
			u, err = h.repo.FindOneBy(r.Context(), "users", pgdoc.M{"sub": sub})
			if httpx.WriteError(w, err) {
				return
			}
		}
		if u != nil {
			members = append(members, shapeDoc(u))
		}
	}
	httpx.List(w, members)
}

// membershipSubs extracts the membership subs from a project doc (memberships:[{sub,role}]).
func membershipSubs(proj pgdoc.M) []string {
	out := []string{}
	raw, ok := proj["memberships"]
	if !ok {
		return out
	}
	arr, ok := raw.(pgdoc.A)
	if !ok {
		return out
	}
	for _, m := range arr {
		mm, ok := m.(pgdoc.M)
		if !ok {
			continue
		}
		if sub, ok := mm["sub"].(string); ok && sub != "" {
			out = append(out, sub)
		}
	}
	return out
}

// projectResourceCounts counts a project's cloud resources.
// It is a pure datastore aggregation over the cloudResource CACHE (group by
// type+serviceId, SECURITY_GROUP minus the default sg, + TOTAL) — no live cloud call — so this
// is fully portable via cloud.Repo.CountByType.
func (h *Handler) projectResourceCounts(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, projectReadPerm) {
		return
	}
	counts, err := h.cloud.CountByType(r.Context(), chi.URLParam(r, "id"))
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, counts)
}

// externalServiceOr404 resolves + decrypts an external service, or writes the exact
// external-service lookup error — the odd HTTP-400/code-404 "Cloud provider is not found.
// Please contact support." envelope (same as the serviceByID read).
func (h *Handler) externalServiceOr404(w http.ResponseWriter, r *http.Request, esID string) (*externalservice.ExternalService, bool) {
	if h.esSvc == nil {
		httpx.WriteError(w, httpx.NewError(http.StatusBadRequest, http.StatusNotFound,
			"Cloud provider is not found. Please contact support."))
		return nil, false
	}
	es, err := h.esSvc.Get(r.Context(), esID)
	if httpx.WriteError(w, err) {
		return nil, false
	}
	if es == nil {
		httpx.WriteError(w, httpx.NewError(http.StatusBadRequest, http.StatusNotFound,
			"Cloud provider is not found. Please contact support."))
		return nil, false
	}
	return es, true
}

// projectAddExternalService attaches an external service to a project (GET /{id}/external-service/{esid}):
// resolve the project (mutation 404) + external service (the 400/404 envelope), then bootstrap with
// the explicit service — create-or-reuse the provider tenant and attach the ProjectExternalService
// entry. Bootstrap failure = the wrapped 500 "Cannot sync the project with the infrastructure. ".
func (h *Handler) projectAddExternalService(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, projectUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	before, ok := h.findProjectOr404(w, r, id)
	if !ok {
		return
	}
	esID := chi.URLParam(r, "externalServiceId")
	if _, ok := h.externalServiceOr404(w, r, esID); !ok {
		return
	}
	if h.projectCloud == nil || h.projectCloud.Bootstrap == nil {
		httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented,
			"attaching a cloud provider to a project not implemented"))
		return
	}
	if err := h.projectCloud.Bootstrap(r.Context(), id, esID); err != nil {
		// Bootstrap failure → 500 "Cannot sync the project with the infrastructure. ".
		httpx.WriteError(w, httpx.NewError(http.StatusInternalServerError, http.StatusInternalServerError,
			"Cannot sync the project with the infrastructure. "))
		return
	}
	after, err := h.repo.FindDoc(r.Context(), projectCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	// UPDATE PROJECT audit (the middleware diff carries the attached services entry).
	audit.RecordSnapshots(r.Context(), maps.Clone(before), after)
	httpx.OK(w, shapeDoc(after))
}

// ── sync (cloud integration point) ────────────────────────────────────────────────────────────────

// projectSync syncs a project (POST /{id}/sync?serviceId): resolves the project (404 via the
// "The project with id %s was not found. " message), then runs the live sync — whole-project
// (gated on ENABLED) when serviceId is blank, else just that service. Returns the project resolved
// BEFORE the sync.
func (h *Handler) projectSync(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, projectUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	proj, err := h.repo.FindDoc(r.Context(), projectCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if proj == nil {
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("The project with id %s was not found. ", id)))
		return
	}
	if h.projectCloud == nil || h.projectCloud.Sync == nil {
		httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented,
			"project cloud sync not implemented"))
		return
	}
	if err := h.projectCloud.Sync(r.Context(), id, r.URL.Query().Get("serviceId")); err != nil {
		httpx.WriteError(w, httpx.NewError(http.StatusInternalServerError, http.StatusInternalServerError, err.Error()))
		return
	}
	httpx.OK(w, shapeDoc(proj))
}

// ── update (datastore, in scope) ──────────────────────────────────────────────────────────────────────

// projectUpdate updates a project (PUT /{id}): load-or-404, then set
// name / billingProfileId / organizationId (all three set unconditionally, including to null
// when the field is absent), save, return the project. Pure datastore. The three fields are
// overwritten: an absent field becomes null → omitted from the stored doc (nulls omitted).
func (h *Handler) projectUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, projectUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req projectUpdateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, ok := h.findProjectOr404(w, r, id)
	if !ok {
		return
	}
	before := maps.Clone(existing)
	// Overwrite name / billingProfileId / organizationId — drop old values first so an
	// omitted (null) field is cleared. A blank value persists as cleared (omitted on read).
	for _, k := range []string{"name", "billingProfileId", "organizationId"} {
		delete(existing, k)
	}
	if req.Name != "" {
		existing["name"] = req.Name
	}
	if req.BillingProfileId != "" {
		existing["billingProfileId"] = req.BillingProfileId
	}
	if req.OrganizationId != "" {
		existing["organizationId"] = req.OrganizationId
	}
	if err := h.repo.ReplaceDoc(r.Context(), projectCollection, id, existing); httpx.WriteError(w, err) {
		return
	}
	// UPDATE PROJECT: field-level diff (the middleware diffs before vs after).
	after, _ := h.repo.FindDoc(r.Context(), projectCollection, id)
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, shapeDoc(existing))
}

// projectUpdateReq is the project-update request body. `data` + `customInfo` are accepted on the
// wire but never read (only name / billingProfileId / organizationId are applied).
type projectUpdateReq struct {
	Name             string `json:"name"`
	BillingProfileId string `json:"billingProfileId"`
	OrganizationId   string `json:"organizationId"`
}

// projectSetPublicNetworks replaces a project's external-network allow-list
// (PUT /{id}/public-networks): {"publicNetworkIds": ["net-id",...]} sets the list (empty array =
// no external networks allowed); {"publicNetworkIds": null} unsets the field (default: all
// allowed — nulls are dropped from the stored doc, not stored as literal nulls). Pure datastore;
// the client cloud-create path enforces the list. The available networks come from the existing
// GET /cloud-resource/public-networks/{externalServiceId} read.
func (h *Handler) projectSetPublicNetworks(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, projectUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		PublicNetworkIds *[]string `json:"publicNetworkIds"` // pointer: null/absent ≠ empty array
		// PublicNetworksVisible: pointer so absent leaves the flag untouched. false = the client
		// gets no external-network picker and the server auto-selects one; true = the client picks.
		PublicNetworksVisible *bool `json:"publicNetworksVisible"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, ok := h.findProjectOr404(w, r, id)
	if !ok {
		return
	}
	before := maps.Clone(existing)
	set := pgdoc.M{}
	if req.PublicNetworksVisible != nil {
		set["publicNetworksVisible"] = *req.PublicNetworksVisible
	}
	if req.PublicNetworkIds == nil {
		var sf pgdoc.M
		if len(set) > 0 {
			sf = set
		}
		if _, err := h.repo.SetAndUnsetFields(r.Context(), projectCollection, id, sf,
			pgdoc.M{"publicNetworkIds": nil}); httpx.WriteError(w, err) {
			return
		}
	} else {
		set["publicNetworkIds"] = *req.PublicNetworkIds
		if _, err := h.repo.SetFields(r.Context(), projectCollection, id, set); httpx.WriteError(w, err) {
			return
		}
	}
	after, err := h.repo.FindDoc(r.Context(), projectCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	// UPDATE PROJECT audit (the middleware diff carries the publicNetworkIds change).
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, shapeDoc(after))
}

// ── status (datastore flip + cloud suspend/resume integration point) ──────────────────────────────────

// projectUpdateStatus changes a project's status (POST /{id}/{status}): load-or-404, validate the
// status value (invalid → 500), 400 if already in the
// desired status, then:
//   - ENABLED / DISABLED → resume / suspend the project's PVE VMs (projectCloud.PauseServers;
//     unwired → 501 without persisting, since the status is set only after the cloud call).
//   - SCHEDULED_FOR_DELETION / DELETE_IN_PROGRESS → no cloud branch; it falls through and
//     saves the project with its status unchanged → pure datastore no-op save, return the project.
func (h *Handler) projectUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, projectUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	status := chi.URLParam(r, "status")
	if !isValidProjectStatus(status) {
		// Invalid status value → 500.
		httpx.WriteError(w, httpx.NewError(http.StatusInternalServerError, http.StatusInternalServerError,
			fmt.Sprintf("Invalid project status %s", status)))
		return
	}
	existing, ok := h.findProjectOr404(w, r, id)
	if !ok {
		return
	}
	current, _ := existing["status"].(string)
	if current == status {
		// "Project is already in desired status " (trailing space).
		httpx.WriteError(w, httpx.BadRequest("Project is already in desired status "))
		return
	}
	switch status {
	case "ENABLED", "DISABLED":
		action := project.ActionEnable
		if status == "DISABLED" {
			action = project.ActionDisable
		}
		if httpx.WriteError(w, project.CheckTransition(action, current)) {
			return
		}
		if h.projectCloud == nil || h.projectCloud.PauseServers == nil {
			// Cloud leg unwired (tests / degraded boot) → 501; the status is set only after the
			// cloud call, so nothing persists.
			httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented,
				fmt.Sprintf("project %s not implemented", suspendResumeOp(status))))
			return
		}
		before := maps.Clone(existing)
		if status == "DISABLED" {
			// Suspend is SYNCHRONOUS: PVE suspend of every cached server (per-server errors
			// swallowed inside), THEN the status flip persists.
			if err := h.projectCloud.PauseServers(r.Context(), id, true); httpx.WriteError(w, err) {
				return
			}
			if !h.setProjectStatus(w, r, id, current, pgdoc.M{"status": status, "disabledByOperator": true}) {
				return
			}
		} else {
			// Resume runs asynchronously: the datastore flip persists immediately; PVE resume + the
			// follow-up project sync happen on a worker thread. Flip first so the async whole-project
			// sync sees ENABLED.
			if !h.setProjectStatus(w, r, id, current, pgdoc.M{"status": status, "disabledByOperator": false}) {
				return
			}
			ops := h.projectCloud
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				_ = ops.PauseServers(ctx, id, false) // best-effort, swallows per-service errors
				if ops.Sync != nil {
					_ = ops.Sync(ctx, id, "")
				}
			}()
		}
		after, err := h.repo.FindDoc(r.Context(), projectCollection, id)
		if httpx.WriteError(w, err) {
			return
		}
		// UPDATE PROJECT audit with the status diff (resourceMetadata {status}).
		audit.RecordSnapshots(r.Context(), before, after)
		httpx.OK(w, shapeDoc(after))
		return
	default:
		httpx.WriteError(w, httpx.BadRequest("Use the project's delete and cancel endpoints for its deletion"))
	}
}

// setProjectStatus writes a status change decided on a read of status expect. False = the answer went out.
func (h *Handler) setProjectStatus(w http.ResponseWriter, r *http.Request, id, expect string, set pgdoc.M) bool {
	ok, err := h.repo.SetFieldsIfStatus(r.Context(), projectCollection, id, expect, set)
	if err != nil {
		slog.Error("admin project status write", "project", id, "err", err)
		httpx.Err(w, http.StatusInternalServerError, http.StatusInternalServerError, "internal.error")
		return false
	}
	if !ok {
		httpx.WriteError(w, httpx.NewError(http.StatusConflict, http.StatusConflict, "The project changed meanwhile. Try again"))
		return false
	}
	return true
}

func suspendResumeOp(status string) string {
	if status == "ENABLED" {
		return "resume"
	}
	return "suspend"
}

func isValidProjectStatus(s string) bool {
	switch s {
	case "ENABLED", "DISABLED", "SCHEDULED_FOR_DELETION", "DELETE_IN_PROGRESS":
		return true
	default:
		return false
	}
}

// ── deletion (datastore flips + cloud integration point) ───────────────────────────────────────────────

// projectScheduleDeletion schedules a project for deletion (DELETE /{id}?cascade): load-or-404 then
// run a CLOUD pre-check (canDelete) BEFORE flipping the status to
// SCHEDULED_FOR_DELETION. Because the cloud pre-check gates the persisted flip, when unwired we do
// NOT flip the status (it wouldn't if the pre-check failed). The already-scheduled fast path returns
// the project WITHOUT touching the cloud. DELETE_IN_PROGRESS and DELETED are refused.
func (h *Handler) projectScheduleDeletion(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, projectDeletePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	existing, ok := h.findProjectOr404(w, r, id)
	if !ok {
		return
	}
	current, _ := existing["status"].(string)
	if current == project.StatusScheduledForDeletion {
		// Already scheduled → no-op (skip the cloud check + the save) and return the project
		// unchanged. Pure read-back, no cloud.
		// TODO(audit): write a DELETE PROJECT scheduled=true admin audit event.
		httpx.OK(w, shapeDoc(existing))
		return
	}
	if httpx.WriteError(w, project.CheckTransition(project.ActionSchedule, current)) {
		return
	}
	// A live cloud pre-check gating the flip. Unwired (tests / degraded boot) → the original 501,
	// BEFORE any persist.
	if h.projectCloud == nil || h.projectCloud.CanDelete == nil {
		httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented,
			"project deletion scheduling (cloud eligibility check) not implemented"))
		return
	}
	if err := h.projectCloud.CanDelete(r.Context(), id); httpx.WriteError(w, err) {
		return
	}
	// Pre-check passed → flip SCHEDULED_FOR_DELETION + stamp scheduledForDeletionAt.
	now := time.Now().UTC()
	if !h.setProjectStatus(w, r, id, current, pgdoc.M{"status": project.StatusScheduledForDeletion, "statusBeforeDeletion": current, "scheduledForDeletionAt": now}) {
		return
	}
	existing["status"] = project.StatusScheduledForDeletion
	existing["scheduledForDeletionAt"] = now
	// TODO(audit): write a DELETE PROJECT scheduled=true admin audit event.
	httpx.OK(w, shapeDoc(existing))
}

// projectDeleteNow flags a project for immediate deletion (DELETE /{id}/now): load-or-404, then the same flag a client's
// delete-now sets, so the deletion job does the teardown on its next sweep. A restart or a failed pass cannot strand the
// project: what the job selects is the persisted flag, not a worker in this process. Unwired → 501 before any write.
func (h *Handler) projectDeleteNow(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, projectDeletePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	if _, ok := h.findProjectOr404(w, r, id); !ok {
		return
	}
	if h.projectCloud == nil || h.projectCloud.DeleteNow == nil {
		httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented,
			"project deletion (cloud teardown) not implemented"))
		return
	}
	if err := h.projectCloud.DeleteNow(r.Context(), id); httpx.WriteError(w, err) {
		return
	}
	updated, err := h.repo.FindDoc(r.Context(), projectCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write a DELETE PROJECT scheduled=false admin audit event.
	httpx.OK(w, shapeDoc(updated))
}

// projectCancelDeletion cancels a scheduled deletion (DELETE /{id}/cancel): load-or-404,
// 400 unless SCHEDULED_FOR_DELETION, else back to the status it held before + clear
// scheduledForDeletionAt, save, return the project. Pure datastore (no cloud). The 404 here uses
// the "The project with id %s was not found. " message.
func (h *Handler) projectCancelDeletion(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, projectDeletePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.repo.FindDoc(r.Context(), projectCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("The project with id %s was not found. ", id)))
		return
	}
	current, _ := existing["status"].(string)
	if httpx.WriteError(w, project.CheckTransition(project.ActionCancel, current)) {
		return
	}
	// Clear the watermark by setting it to nil (omitted on read).
	held, _ := existing["statusBeforeDeletion"].(string)
	if !h.setProjectStatus(w, r, id, current, pgdoc.M{"status": project.StatusAfterCancel(held), "statusBeforeDeletion": "", "scheduledForDeletionAt": nil}) {
		return
	}
	// Re-read so the response reflects the persisted flip (returns the saved project).
	updated, err := h.repo.FindDoc(r.Context(), projectCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an UPDATE PROJECT deletionCancelled=true admin audit event.
	httpx.OK(w, shapeDoc(updated))
}

package admin

// providerattach.go — attach one cloud provider to many existing projects (docs/cloud-integration.md "Attaching a
// provider to existing projects"). The request only records a run and enqueues one durable job; the
// providerattach.Runner attaches the projects (the per-project attach path, bounded concurrency, fenced, resumable).
//
//	GET  /admin/service/{id}/bulk-attach                    runs (newest first) + the active one   (service:read)
//	GET  /admin/service/{id}/bulk-attach/candidates         projects|organizations for the picker  (service:read + project:read)
//	POST /admin/service/{id}/bulk-attach/preview            counts a scope would give now          (service:read + project:read)
//	POST /admin/service/{id}/bulk-attach                    start {scope, concurrency}             (service:manage + project:update)
//	GET  /admin/service/{id}/bulk-attach/{runId}            one run with live counts               (service:read)
//	GET  /admin/service/{id}/bulk-attach/{runId}/items      its projects (?state=failed …)         (service:read)
//	POST /admin/service/{id}/bulk-attach/{runId}/cancel     stop it                                (service:manage)
//	POST /admin/service/{id}/bulk-attach/{runId}/retry      new run over its failed projects       (service:manage + project:update)
//
// Starting and retrying change projects (they attach a provider to each), so they need the project update key the
// per-project attach needs as well as the provider key; picking projects needs reading them.//
// Each POST that changes something writes an admin audit event naming the actor, the provider, the scope and the counts.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/providerattach"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const (
	bulkAttachReadPerm   = "admin:service:read"
	bulkAttachManagePerm = "admin:service:manage"
)

// SetProviderAttach wires the bulk attach service (cmd/api) and the check of whether a provider can be attached to
// projects (project.Handler.CanAttach; nil = no check). nil service → the surface answers 503.
func (h *Handler) SetProviderAttach(s *providerattach.Service, canAttach func(*externalservice.ExternalService) error) {
	h.providerAttach, h.canAttach = s, canAttach
}

func (h *Handler) routeProviderAttach(r chi.Router) {
	r.Get("/service/{id}/bulk-attach", h.bulkAttachList)
	r.Get("/service/{id}/bulk-attach/candidates", h.bulkAttachCandidates)
	r.Post("/service/{id}/bulk-attach/preview", h.bulkAttachPreview)
	r.Post("/service/{id}/bulk-attach", h.bulkAttachStart)
	r.Get("/service/{id}/bulk-attach/{runId}", h.bulkAttachGet)
	r.Get("/service/{id}/bulk-attach/{runId}/items", h.bulkAttachItems)
	r.Post("/service/{id}/bulk-attach/{runId}/cancel", h.bulkAttachCancel)
	r.Post("/service/{id}/bulk-attach/{runId}/retry", h.bulkAttachRetry)
}

// bulkAttachGate checks every permission, then that the service is wired; false = the answer went out.
func (h *Handler) bulkAttachGate(w http.ResponseWriter, r *http.Request, perms ...string) bool {
	_, granted, ok := h.adminContext(r)
	if missing := missingPerm(granted, perms); !ok || missing != "" {
		if missing == "" {
			missing = perms[0]
		}
		httpx.WriteError(w, httpx.Forbidden("You do not have the required permission: "+missing))
		return false
	}
	if h.providerAttach == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "Bulk attach is not configured")
		return false
	}
	return true
}

// missingPerm is the first of perms the grant does not cover ("" = all covered).
func missingPerm(granted, perms []string) string {
	for _, p := range perms {
		if !rbac.Matches(granted, p) {
			return p
		}
	}
	return ""
}

// bulkAttachProvider loads the provider of the path (404 when absent) as a CLOUD provider.
func (h *Handler) bulkAttachProvider(w http.ResponseWriter, r *http.Request) (*externalservice.ExternalService, bool) {
	es, ok := h.loadServiceOr(w, r, chi.URLParam(r, "id"))
	if !ok {
		return nil, false
	}
	if es.Type != "" && es.Type != externalservice.TypeCloud {
		httpx.WriteError(w, httpx.BadRequest("Only cloud providers are attached to projects"))
		return nil, false
	}
	return es, true
}

// bulkAttachAttachable refuses (400) a provider this API cannot attach projects to (an unknown kind, or one whose client
// is not wired), before anything is queued: every item would fail otherwise.
func (h *Handler) bulkAttachAttachable(w http.ResponseWriter, es *externalservice.ExternalService) bool {
	if h.canAttach == nil {
		return true
	}
	if err := h.canAttach(es); err != nil {
		httpx.WriteError(w, httpx.BadRequest("This provider cannot be attached to projects: "+err.Error()))
		return false
	}
	return true
}

func bulkAttachErr(w http.ResponseWriter, err error) {
	var se *providerattach.ScopeError
	switch {
	case errors.As(err, &se):
		httpx.WriteError(w, httpx.BadRequest(se.Msg))
	case errors.Is(err, providerattach.ErrNotFound):
		httpx.WriteError(w, httpx.NotFound(err.Error()))
	case errors.Is(err, providerattach.ErrActive), errors.Is(err, providerattach.ErrFinished),
		errors.Is(err, providerattach.ErrNoFailed), errors.Is(err, providerattach.ErrNotDone), errors.Is(err, providerattach.ErrDisabled),
		errors.Is(err, providerattach.ErrPruned):
		httpx.Err(w, http.StatusConflict, http.StatusConflict, err.Error())
	case errors.Is(err, providerattach.ErrBadParams):
		httpx.WriteError(w, httpx.BadRequest("Invalid request"))
	default:
		httpx.WriteError(w, err)
	}
}

func adminActor(r *http.Request) string {
	rc := httpx.RC(r.Context())
	if rc == nil {
		return ""
	}
	if rc.Sub != "" {
		return rc.Sub
	}
	return rc.SigV4KeyID
}

func (h *Handler) bulkAttachList(w http.ResponseWriter, r *http.Request) {
	if !h.bulkAttachGate(w, r, bulkAttachReadPerm) {
		return
	}
	es, ok := h.bulkAttachProvider(w, r)
	if !ok {
		return
	}
	runs, active, err := h.providerAttach.List(r.Context(), es.ID, 20)
	if err != nil {
		bulkAttachErr(w, err)
		return
	}
	httpx.OK(w, map[string]any{"runs": runs, "active": active, "attachToNewProjects": es.AttachToNewProjects()})
}

func (h *Handler) bulkAttachGet(w http.ResponseWriter, r *http.Request) {
	if !h.bulkAttachGate(w, r, bulkAttachReadPerm) {
		return
	}
	v, err := h.providerAttach.View(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "runId"))
	if err != nil {
		bulkAttachErr(w, err)
		return
	}
	httpx.OK(w, v)
}

func (h *Handler) bulkAttachItems(w http.ResponseWriter, r *http.Request) {
	if !h.bulkAttachGate(w, r, bulkAttachReadPerm) {
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	switch state {
	case "", providerattach.ItemPending, providerattach.ItemRunning, providerattach.ItemAttached, providerattach.ItemSkipped,
		providerattach.ItemFailed, providerattach.ItemCancelled:
	default:
		httpx.WriteError(w, httpx.BadRequest("Unknown state"))
		return
	}
	limit, _ := strconv.ParseInt(q.Get("limit"), 10, 64)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
	if offset < 0 {
		offset = 0
	}
	items, total, err := h.providerAttach.Items(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "runId"), state, limit, offset)
	if err != nil {
		bulkAttachErr(w, err)
		return
	}
	httpx.OK(w, map[string]any{"items": items, "total": total, "limit": limit, "offset": offset})
}

// bulkAttachCandidates lists what the scope picker offers: ?kind=projects|organizations&search=…
func (h *Handler) bulkAttachCandidates(w http.ResponseWriter, r *http.Request) {
	if !h.bulkAttachGate(w, r, bulkAttachReadPerm, projectReadPerm) {
		return
	}
	es, ok := h.bulkAttachProvider(w, r)
	if !ok {
		return
	}
	search := r.URL.Query().Get("search")
	if len(search) > 200 {
		httpx.WriteError(w, httpx.BadRequest("Search is too long"))
		return
	}
	var (
		out []providerattach.Candidate
		err error
	)
	switch r.URL.Query().Get("kind") {
	case "organizations":
		out, err = h.providerAttach.Repo.OrganizationCandidates(r.Context(), search, 50)
	case "", "projects":
		out, err = h.providerAttach.Repo.ProjectCandidates(r.Context(), es.ID, search, 50)
	default:
		httpx.WriteError(w, httpx.BadRequest("kind must be projects or organizations"))
		return
	}
	if httpx.WriteError(w, err) {
		return
	}
	httpx.List(w, out)
}

type bulkAttachRequest struct {
	Scope       providerattach.Scope `json:"scope"`
	Concurrency int                  `json:"concurrency"`
}

// bulkAttachScope decodes and checks a request's scope: the organizations or projects it names must exist.
func (h *Handler) bulkAttachScope(w http.ResponseWriter, r *http.Request) (*bulkAttachRequest, bool) {
	var req bulkAttachRequest
	if !decodeStrictBodyN(w, r, &req, 256<<10) {
		return nil, false
	}
	scope, err := providerattach.NormalizeScope(req.Scope)
	if err != nil {
		bulkAttachErr(w, err)
		return nil, false
	}
	if _, err := providerattach.NormalizeConcurrency(req.Concurrency); err != nil {
		bulkAttachErr(w, err)
		return nil, false
	}
	collection, ids := "", []string(nil)
	switch scope.Mode {
	case providerattach.ScopeOrganizations:
		collection, ids = "organization", scope.OrganizationIDs
	case providerattach.ScopeProjects:
		collection, ids = projectCollection, scope.ProjectIDs
	}
	if collection != "" {
		missing, err := h.providerAttach.Repo.MissingIDs(r.Context(), collection, ids)
		if httpx.WriteError(w, err) {
			return nil, false
		}
		if len(missing) > 0 {
			what := "Organization"
			if collection == projectCollection {
				what = "Project"
			}
			msg := what + " not found: " + missing[0]
			if len(missing) > 1 {
				msg += " (and " + strconv.Itoa(len(missing)-1) + " more)"
			}
			httpx.WriteError(w, httpx.BadRequest(msg))
			return nil, false
		}
	}
	req.Scope = scope
	return &req, true
}

func (h *Handler) bulkAttachPreview(w http.ResponseWriter, r *http.Request) {
	if !h.bulkAttachGate(w, r, bulkAttachReadPerm, projectReadPerm) {
		return
	}
	es, ok := h.bulkAttachProvider(w, r)
	if !ok {
		return
	}
	req, ok := h.bulkAttachScope(w, r)
	if !ok {
		return
	}
	p, err := h.providerAttach.Repo.Preview(r.Context(), req.Scope, es.ID)
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, p)
}

func (h *Handler) bulkAttachStart(w http.ResponseWriter, r *http.Request) {
	if !h.bulkAttachGate(w, r, bulkAttachManagePerm, projectUpdatePerm) {
		return
	}
	es, ok := h.bulkAttachProvider(w, r)
	if !ok {
		return
	}
	if es.IsDisabled() {
		bulkAttachErr(w, providerattach.ErrDisabled)
		return
	}
	if !h.bulkAttachAttachable(w, es) {
		return
	}
	req, ok := h.bulkAttachScope(w, r)
	if !ok {
		return
	}
	preview, err := h.providerAttach.Repo.Preview(r.Context(), req.Scope, es.ID)
	if httpx.WriteError(w, err) {
		return
	}
	// One run attaches at most MaxRunProjects projects (the ones not attached yet): a larger scope is split by the
	// operator (organizations, picked projects), so a run stays a bounded, reviewable unit of work.
	if preview.ToAttach > providerattach.MaxRunProjects {
		httpx.WriteError(w, httpx.BadRequest(fmt.Sprintf("This scope would attach %d projects; one run attaches at most %d. "+
			"Pick organizations or projects to split it.", preview.ToAttach, providerattach.MaxRunProjects)))
		return
	}
	run, err := h.providerAttach.Start(r.Context(), providerattach.StartRequest{ServiceID: es.ID, ServiceName: es.Name, Scope: req.Scope,
		Concurrency: req.Concurrency, Actor: adminActor(r)})
	if err != nil {
		bulkAttachErr(w, err)
		return
	}
	h.bulkAttachAudit(r, run, "start", map[string]any{"inScope": preview.InScope, "alreadyAttached": preview.AlreadyAttached,
		"deleting": preview.Deleting, "toAttach": preview.ToAttach})
	httpx.OK(w, run)
}

func (h *Handler) bulkAttachCancel(w http.ResponseWriter, r *http.Request) {
	if !h.bulkAttachGate(w, r, bulkAttachManagePerm) {
		return
	}
	v, err := h.providerAttach.Cancel(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "runId"), adminActor(r))
	if err != nil {
		bulkAttachErr(w, err)
		return
	}
	h.bulkAttachAudit(r, &v.Run, "cancel", countsMeta(v.Counts))
	httpx.OK(w, v)
}

func (h *Handler) bulkAttachRetry(w http.ResponseWriter, r *http.Request) {
	if !h.bulkAttachGate(w, r, bulkAttachManagePerm, projectUpdatePerm) {
		return
	}
	es, ok := h.bulkAttachProvider(w, r)
	if !ok {
		return
	}
	if es.IsDisabled() {
		bulkAttachErr(w, providerattach.ErrDisabled)
		return
	}
	if !h.bulkAttachAttachable(w, es) {
		return
	}
	var req struct {
		Concurrency int `json:"concurrency"`
	}
	if r.ContentLength != 0 && !decodeStrictBodyN(w, r, &req, 1<<10) {
		return
	}
	source := chi.URLParam(r, "runId")
	prev, err := h.providerAttach.View(r.Context(), es.ID, source)
	if err != nil {
		bulkAttachErr(w, err)
		return
	}
	run, err := h.providerAttach.Retry(r.Context(), es.ID, es.Name, source, adminActor(r), req.Concurrency)
	if err != nil {
		bulkAttachErr(w, err)
		return
	}
	meta := countsMeta(prev.Counts)
	meta["sourceRunId"] = source
	h.bulkAttachAudit(r, run, "retry_failed", meta)
	httpx.OK(w, run)
}

func countsMeta(c providerattach.Counts) map[string]any {
	return map[string]any{"total": c.Total, "attached": c.Attached, "skipped": c.Skipped, "failed": c.Failed,
		"pending": c.Pending, "cancelled": c.Cancelled}
}

// bulkAttachAudit writes the event of a bulk attach operation: actor, provider, scope, counts. It is written directly
// (not only through the middleware's generic event) so it carries the scope and the counts.
func (h *Handler) bulkAttachAudit(r *http.Request, run *providerattach.Run, op string, counts map[string]any) {
	if h.audit == nil || run == nil {
		return
	}
	actor := adminActor(r)
	ev := audit.AdminEvent(actor, actor)
	ev.EventContext = audit.ContextPlatform
	ev.Action = ActionBulkAttach
	ev.ResourceType = "EXTERNAL_SERVICE"
	ev.ResourceID = run.ServiceID
	ev.ResourceDisplayName = run.ServiceName
	scope := map[string]any{"mode": run.Scope.Mode}
	if n := len(run.Scope.OrganizationIDs); n > 0 {
		scope["organizationIds"] = run.Scope.OrganizationIDs[:min(n, 50)]
		scope["organizationCount"] = n
	}
	if n := len(run.Scope.ProjectIDs); n > 0 {
		scope["projectIds"] = run.Scope.ProjectIDs[:min(n, 50)]
		scope["projectCount"] = n
	}
	if run.Scope.SourceRunID != "" {
		scope["sourceRunId"] = run.Scope.SourceRunID
	}
	ev.ResourceMetadata = map[string]any{"operation": op, "runId": run.ID, "jobId": run.JobID, "scope": scope,
		"concurrency": run.Concurrency, "counts": counts}
	ev.Outcome = audit.OutcomeSuccess
	if ev.Actor != nil {
		ev.Actor.IPAddress = clientIP(r)
		ev.Actor.UserAgent = r.UserAgent()
	}
	// Synchronously, so the event is there when the answer is: it records who attached what to how many projects.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), auditWriteTimeout)
	defer cancel()
	if err := h.audit.Log(ctx, ev); err != nil {
		h.audit.LogEvent(ev) // retried in the background
	}
}

// ActionBulkAttach is the audit action of the bulk attach operations (resourceMetadata.operation says which).
const ActionBulkAttach = "BULK_ATTACH"

// decodeStrictBodyN is decodeStrictBody with its own body limit (a project scope can list thousands of ids).
func decodeStrictBodyN(w http.ResponseWriter, r *http.Request, out any, limit int64) bool {
	return decodeStrictLimited(w, r, out, limit)
}

// auditWriteTimeout bounds the synchronous write of a bulk attach audit event.
const auditWriteTimeout = 5 * time.Second

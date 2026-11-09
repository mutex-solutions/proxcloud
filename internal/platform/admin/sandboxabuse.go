package admin

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Counts derive from the durable detection audit, not a client-editable project field.
// Exact per-project count is returned only with an explicit project filter.
func (h *Handler) sandboxAbuseList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:audit:read") {
		return
	}
	f := audit.Filter{ResourceType: "SANDBOX_ABUSE", Action: "DETECTED", ProjectID: r.URL.Query().Get("projectId")}
	events, next, _, err := h.audit.Query(r.Context(), f, r.URL.Query().Get("after"), "", 50)
	if httpx.WriteError(w, err) {
		return
	}
	count := int64(0)
	if f.ProjectID != "" {
		count, err = h.audit.Count(r.Context(), f)
		if httpx.WriteError(w, err) {
			return
		}
	}
	httpx.OK(w, map[string]any{"events": events, "nextMarker": next, "projectCount": count})
}

// Read-only by construction: no cordon, patch, state initialization or timestamp updates.
func (h *Handler) sandboxRotationPlan(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") {
		return
	}
	doc, err := h.repo.FindDoc(r.Context(), externalServiceCollection, chi.URLParam(r, "id"))
	if httpx.WriteError(w, err) {
		return
	}
	if doc == nil {
		httpx.WriteError(w, httpx.NotFound("Sandbox provider not found"))
		return
	}
	es := h.sandboxOf(doc)
	if es == nil {
		httpx.WriteError(w, httpx.BadRequest("Not a sandbox provider"))
		return
	}
	ss, err := sandbox.New(es.SandboxConfig(), es.ID)
	if httpx.WriteError(w, err) {
		return
	}
	plan, err := ss.RotationInspect(r.Context())
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, plan)
}

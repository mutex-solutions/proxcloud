package org

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// AuditHandler serves the org-scoped audit log:
// CLIENT_AREA events for the org, cursor-paginated, gated by ORGANIZATION_READ.
type AuditHandler struct {
	svc    *Service
	policy *Policy
	users  *user.Repo
	audit  *audit.Service
}

func NewAuditHandler(svc *Service, policy *Policy, users *user.Repo, a *audit.Service) *AuditHandler {
	return &AuditHandler{svc: svc, policy: policy, users: users, audit: a}
}

func (h *AuditHandler) Routes(r chi.Router) {
	r.Get("/organizations/{id}/audit", h.list)
	r.Get("/organizations/{id}/audit/export", h.export)
}

// export streams the org's matching CLIENT_AREA audit
// events (capped 10000) as a CSV (UTF-8 BOM + fixed header) or JSON attachment (?format=json).
// Org-membership + ORGANIZATION_READ gated (same as list).
func (h *AuditHandler) export(w http.ResponseWriter, r *http.Request) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		fail(w, err)
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := h.svc.GetOrganizationForUser(r.Context(), id, u.Sub); err != nil {
		fail(w, err)
		return
	}
	if err := h.policy.RequirePermission(r.Context(), u.Sub, id, rbac.OrganizationRead); err != nil {
		fail(w, err)
		return
	}
	f := audit.Filter{
		OrganizationID:   id,
		RequestInterface: audit.InterfaceClientArea,
		ResourceType:     r.URL.Query().Get("resourceType"),
		Action:           r.URL.Query().Get("action"),
		Outcome:          r.URL.Query().Get("outcome"),
		Search:           r.URL.Query().Get("search"),
		From:             audit.ParseInstant(r.URL.Query().Get("from")),
		To:               audit.ParseInstant(r.URL.Query().Get("to")),
	}
	events, err := h.audit.QueryAll(r.Context(), f, 10000)
	if err != nil {
		fail(w, err)
		return
	}
	if strings.EqualFold(r.URL.Query().Get("format"), "json") {
		b, _ := json.MarshalIndent(events, "", "  ")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", "attachment; filename=audit-events.json")
		_, _ = w.Write(b)
		return
	}
	var sb strings.Builder
	sb.Write([]byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM (Excel)
	sb.WriteString("Timestamp,Interface,Context,Action,Resource Type,Resource ID,Resource Name,Actor,Actor Type,Outcome,IP Address\n")
	for i := range events {
		e := &events[i]
		ts := ""
		if e.Timestamp != nil {
			ts = e.Timestamp.UTC().Format("2006-01-02 15:04:05")
		}
		actor, atype, ip := "", "", ""
		if e.Actor != nil {
			actor, atype, ip = e.Actor.DisplayName, e.Actor.Type, e.Actor.IPAddress
		}
		cols := []string{ts, e.RequestInterface, e.EventContext, e.Action, e.ResourceType,
			e.ResourceID, e.ResourceDisplayName, actor, atype, e.Outcome, ip}
		for j, c := range cols {
			if j > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(escapeCSVField(c))
		}
		sb.WriteByte('\n')
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=audit-events.csv")
	_, _ = w.Write([]byte(sb.String()))
}

// escapeCSVField quotes a field containing a comma/quote/newline (doubling embedded quotes).
func escapeCSVField(s string) string {
	if strings.ContainsAny(s, ",\"\n\r") {
		return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\""
	}
	return s
}

func (h *AuditHandler) list(w http.ResponseWriter, r *http.Request) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		fail(w, err)
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := h.svc.GetOrganizationForUser(r.Context(), id, u.Sub); err != nil {
		fail(w, err)
		return
	}
	if err := h.policy.RequirePermission(r.Context(), u.Sub, id, rbac.OrganizationRead); err != nil {
		fail(w, err)
		return
	}
	after, before := r.URL.Query().Get("after"), r.URL.Query().Get("before")
	if after != "" && before != "" {
		fail(w, httpx.BadRequest("Cannot specify both 'after' and 'before'"))
		return
	}
	f := audit.Filter{
		OrganizationID:   id,
		RequestInterface: audit.InterfaceClientArea,
		ResourceType:     r.URL.Query().Get("resourceType"),
		Action:           r.URL.Query().Get("action"),
		Outcome:          r.URL.Query().Get("outcome"),
		Search:           r.URL.Query().Get("search"),
		From:             audit.ParseInstant(r.URL.Query().Get("from")),
		To:               audit.ParseInstant(r.URL.Query().Get("to")),
	}
	limit := audit.ParseLimit(r.URL.Query().Get("limit"))
	events, next, prev, err := h.audit.Query(r.Context(), f, after, before, limit)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.CursorList(w, events, limit, next, prev)
}

// fail is shared by the org-package handlers.
func fail(w http.ResponseWriter, err error) {
	if !httpx.WriteError(w, err) {
		httpx.Err(w, http.StatusInternalServerError, 500, "internal.error")
	}
}

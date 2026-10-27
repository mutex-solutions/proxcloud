package admin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// auditFilterFromQuery builds the audit query filter from the request query params
// (all AND-combined: requestInterface/eventContext/organizationId/projectId/
// resourceType/resourceId/actorId/action/outcome/from/to/search).
func auditFilterFromQuery(q url.Values) audit.Filter {
	return audit.Filter{
		RequestInterface: q.Get("requestInterface"),
		EventContext:     q.Get("eventContext"),
		OrganizationID:   q.Get("organizationId"),
		ProjectID:        q.Get("projectId"),
		ResourceType:     q.Get("resourceType"),
		ResourceID:       q.Get("resourceId"),
		ActorID:          q.Get("actorId"),
		Action:           q.Get("action"),
		Outcome:          q.Get("outcome"),
		Search:           q.Get("search"),
		From:             audit.ParseInstant(q.Get("from")),
		To:               audit.ParseInstant(q.Get("to")),
	}
}

// auditExport exports audit events:
// the matching events (capped 10000) as a CSV (UTF-8 BOM + fixed header) or JSON attachment.
// ?format=json → JSON, else CSV. ADMIN_AUDIT_READ.
func (h *Handler) auditExport(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:audit:read") {
		return
	}
	events, err := h.audit.QueryAll(r.Context(), auditFilterFromQuery(r.URL.Query()), 10000)
	if httpx.WriteError(w, err) {
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
	sb.Write([]byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM (Excel-friendly)
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
			sb.WriteString(escapeCSV(c))
		}
		sb.WriteByte('\n')
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=audit-events.csv")
	_, _ = w.Write([]byte(sb.String()))
}

// escapeCSV quotes a field when it contains a comma/quote/newline (doubling embedded quotes).
func escapeCSV(s string) string {
	if strings.ContainsAny(s, ",\"\n\r") {
		return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\""
	}
	return s
}

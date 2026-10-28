// Package adminapi serves the PUBLIC Admin API (the /admin-api/v1 surface): a
// machine-to-machine API authenticated by AWS SigV4 (hmac_keys, pkg/auth/sigv4.go) or an
// OIDC bearer from the dedicated admin-api realm (issuer + azp must match cfg.Auth.AdminAPI).
//
// Envelope contract (snake_case field naming, omit null fields): entities `{"data":{...}}`,
// lists `{"data":[...],"next_marker"?}`, errors `{"error":{"code","message"}}`. Pagination is
// keyset: `marker` = first _id of the next page (_id >= marker), `limit` 1..500 default 50
// (fetch limit+1; the extra row's id becomes next_marker). Any unexpected runtime error maps
// to 400 via badRequest.
package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const (
	defaultPageLimit = 50  // default page size
	maxPageLimit     = 500 // max page size
)

type Handler struct {
	db          *pgdoc.DB
	orgs        *org.Repo
	users       *user.Repo
	es          *externalservice.Service
	audit       *audit.Service
	apiIssuer   string // cfg.Auth.AdminAPI — the OIDC alternative to SigV4
	apiClientID string
	activation  *billing.ActivationService // bp activate/suspend/resume (nil → 501 when not wired)
	// bootstrapProject provisions a project onto the platform providers + ENABLED (no
	// per-spec service selection, no per-user cloud grants). nil → 501 when not wired.
	bootstrapProject func(ctx context.Context, projectID string) error
}

// SetActivation wires the billing ActivationService.
func (h *Handler) SetActivation(a *billing.ActivationService) { h.activation = a }

// SetBootstrapProject wires the project provisioning leg.
func (h *Handler) SetBootstrapProject(f func(ctx context.Context, projectID string) error) {
	h.bootstrapProject = f
}

func NewHandler(db *pgdoc.DB, orgs *org.Repo, users *user.Repo, es *externalservice.Service, a *audit.Service, apiIssuer, apiClientID string) *Handler {
	return &Handler{db: db, orgs: orgs, users: users, es: es, audit: a, apiIssuer: apiIssuer, apiClientID: apiClientID}
}

// Routes registers the /admin-api/v1 surface behind the gate.
func (h *Handler) Routes(r chi.Router) {
	r.Use(h.gate)
	h.routeUsers(r)
	h.routeOrganizations(r)
	h.routeBillingProfiles(r)
	h.routeProjects(r)
	h.routeBills(r)
	h.routeAccountCredits(r)
	h.routeServiceProviders(r)
}

// gate authorizes /admin-api/v1: a SigV4-authenticated request passes; a bearer passes only
// when it comes from the admin-api realm (issuer) with the admin-api client (azp set by the
// OIDC provider). Everything else → 403.
func (h *Handler) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := httpx.RC(r.Context())
		switch {
		case rc == nil:
			w.WriteHeader(http.StatusForbidden)
		case rc.SigV4KeyID != "":
			next.ServeHTTP(w, r)
		case h.apiIssuer != "" && rc.Issuer == h.apiIssuer && rc.Azp == h.apiClientID:
			next.ServeHTTP(w, r)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	})
}

// actorEvent presets the ADMIN_AREA audit event for the calling credential (the SigV4 key id
// or the bearer sub is the actor).
func (h *Handler) actorEvent(r *http.Request) audit.AuditEvent {
	rc := httpx.RC(r.Context())
	actor := "admin-api"
	if rc != nil && rc.SigV4KeyID != "" {
		actor = rc.SigV4KeyID
	} else if rc != nil && rc.Sub != "" {
		actor = rc.Sub
	}
	return audit.AdminEvent(actor, "Admin API")
}

// logAdmin emits a PLATFORM admin event (best-effort).
func (h *Handler) logAdmin(r *http.Request, action, resourceType, resourceID, displayName string) {
	if h.audit == nil {
		return
	}
	ev := h.actorEvent(r)
	ev.EventContext = audit.ContextPlatform
	ev.Action = action
	ev.ResourceType = resourceType
	ev.ResourceID = resourceID
	ev.ResourceDisplayName = displayName
	ev.Outcome = audit.OutcomeSuccess
	h.audit.LogAsync(ev)
}

// ── envelopes (snake_case, omit null) ──────────────────────────────────────────────────────────

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeEntity(w http.ResponseWriter, v any) {
	writeJSON(w, http.StatusOK, map[string]any{"data": v})
}

func writeCreated(w http.ResponseWriter, v any) {
	writeJSON(w, http.StatusCreated, map[string]any{"data": v})
}

func writeList[T any](w http.ResponseWriter, items []T, nextMarker string) {
	body := map[string]any{"data": items}
	if nextMarker != "" {
		body["next_marker"] = nextMarker
	}
	writeJSON(w, http.StatusOK, body)
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": errorBody{Code: code, Message: message}})
}

// Error response presets.
func apiNotFound(w http.ResponseWriter)              { writeErr(w, 404, "NOT_FOUND", "Not Found") }
func apiNotFoundMsg(w http.ResponseWriter, m string) { writeErr(w, 404, "NOT_FOUND", m) }
func badRequest(w http.ResponseWriter, m string)     { writeErr(w, 400, "BAD_REQUEST", m) }
func conflict(w http.ResponseWriter, m string)       { writeErr(w, 409, "CONFLICT", m) }

// seam marks paths whose side effect drives live cloud/orchestration subsystems that
// are not wired here.
func seam(w http.ResponseWriter, m string) {
	writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", m)
}

// ── pagination (ApiUtils) ─────────────────────────────────────────────────────────────────────

type listReq struct {
	Marker string
	Limit  int
}

// listParams parses the paging query params: limit defaults 50 (also when <=0); a NON-numeric
// limit → 400; >500 → the validation error message.
func listParams(w http.ResponseWriter, r *http.Request) (listReq, bool) {
	req := listReq{Marker: r.URL.Query().Get("marker"), Limit: defaultPageLimit}
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			badRequest(w, `For input string: "`+s+`"`)
			return req, false
		}
		if n > 0 {
			req.Limit = n
		}
	}
	if req.Limit > maxPageLimit {
		badRequest(w, "page limit can't exceed 500")
		return req, false
	}
	return req, true
}

// markerFilter applies the keyset cursor (_id >= marker).
func markerFilter(f pgdoc.M, marker string) pgdoc.M {
	if marker == "" {
		return f
	}
	f["_id"] = pgdoc.M{"$gte": marker}
	return f
}

// findPage runs the keyset find (sorted by _id — the explicit sort keeps the cursor stable)
// fetching limit+1 rows.
func findPage[T any](ctx context.Context, col *pgdoc.Store, filter pgdoc.M, req listReq) ([]T, error) {
	var out []T
	err := col.Find(ctx, markerFilter(filter, req.Marker), &out,
		pgdoc.Sort(pgdoc.Asc("_id")), pgdoc.Limit(int64(req.Limit+1)))
	if err != nil {
		return nil, err
	}
	return out, nil
}

// pageOut trims the +1 row into next_marker.
func pageOut[T any](req listReq, items []T, idOf func(T) string) ([]T, string) {
	if items == nil {
		items = []T{}
	}
	if req.Limit > 0 && len(items) > req.Limit {
		last := items[len(items)-1]
		return items[:len(items)-1], idOf(last)
	}
	return items, ""
}

// decodeBody JSON-decodes the request body (bad JSON → 400).
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		badRequest(w, "Malformed request body")
		return false
	}
	return true
}

func newID() string { return pgdoc.NewID() }

func nowUTC() time.Time { return time.Now().UTC() }

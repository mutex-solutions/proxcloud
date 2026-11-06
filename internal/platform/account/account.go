// Package account implements the customer User/Account endpoints.
// Own-record only (no RBAC gate); the principal is resolved
// (get-or-create) by the auth layer before these handlers run.
package account

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

type Handler struct {
	users *user.Repo
	audit *audit.Service
	pat   *pgdoc.Store // client_api_keys collection — self-service API tokens (see account_apikeys.go)
	jit   Provisioner  // nil when per-organization SSO is off
}

// Provisioner adds a person who signed in through an organization's IdP to that organization.
type Provisioner interface {
	Provision(ctx context.Context, rc *httpx.RequestContext, u *user.User) error
}

// SetProvisioner wires the SSO just-in-time step.
func (h *Handler) SetProvisioner(p Provisioner) { h.jit = p }

// provision runs the JIT step and answers POST /user.
func (h *Handler) provision(w http.ResponseWriter, r *http.Request, u *user.User) {
	if h.jit != nil {
		if err := h.jit.Provision(r.Context(), httpx.RC(r.Context()), u); err != nil {
			httpx.Err(w, http.StatusInternalServerError, 500, "user.create.failed")
			return
		}
	}
	httpx.OK(w, u)
}

func NewHandler(users *user.Repo, a *audit.Service, pat *pgdoc.Store) *Handler {
	return &Handler{users: users, audit: a, pat: pat}
}

// Routes mounts the account/user endpoints under the /api/v1 group.
func (h *Handler) Routes(r chi.Router) {
	r.Post("/user", h.getCustomer)
	r.Post("/user/token", h.issueToken)
	r.Post("/user/custom-info/{key}", h.setCustomInfo)
	r.Delete("/user/custom-info/{key}", h.deleteCustomInfo)
	r.Get("/account/details", h.accountDetails)
	r.Post("/account/name", h.updateName)
	r.Get("/account/audit", h.accountAudit)
	// Self-service API tokens (PATs) — auth the client API + client MCP toolset without SSO.
	r.Get("/user/api-keys", h.listAPIKeys)
	r.Post("/user/api-keys", h.createAPIKey)
	r.Delete("/user/api-keys/{id}", h.deleteAPIKey)
}

// accountAudit serves the user's own audit log:
// USER_ACCOUNT events with actor.id == sub, cursor-paginated.
func (h *Handler) accountAudit(w http.ResponseWriter, r *http.Request) {
	u, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	after, before := r.URL.Query().Get("after"), r.URL.Query().Get("before")
	if after != "" && before != "" {
		httpx.Err(w, http.StatusBadRequest, 400, "Cannot specify both 'after' and 'before'")
		return
	}
	f := audit.Filter{
		ActorID:          u.Sub,
		RequestInterface: audit.InterfaceUserAccount,
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
		httpx.Err(w, http.StatusInternalServerError, 500, "internal.error")
		return
	}
	httpx.CursorList(w, events, limit, next, prev)
}

// requireUser loads the domain User for the principal, get-or-creating it from the validated
// request-context claims on first sight (user.Repo.Require) so the account endpoints work on the
// very first call after login — no dependency on the FE's one-shot POST /user init.
func (h *Handler) requireUser(w http.ResponseWriter, r *http.Request) (*user.User, bool) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		httpx.WriteError(w, err)
		return nil, false
	}
	return u, true
}

// POST /api/v1/user -> CustomHttpResponse<User>.
// With id_token (the FE posts it form-urlencoded in the BODY; query also accepted) → get-or-create
// (remote get-or-create path). Without → resolve existing or 400 "User is
// not initialized". r.FormValue reads both the POST form body and the URL query.
func (h *Handler) getCustomer(w http.ResponseWriter, r *http.Request) {
	rc := httpx.RC(r.Context())
	if r.FormValue("id_token") != "" {
		// TODO: the id_token param could be re-decoded here; instead we trust
		// the already-validated access-token claims (same principal) to create.
		u, err := h.users.FromClaims(r.Context(), user.Claims{
			Sub: rc.Sub, Email: rc.Email, EmailVerified: rc.EmailVerified,
			GivenName: rc.GivenName, FamilyName: rc.FamilyName, Issuer: rc.Issuer, IdP: rc.IdP,
		})
		if err != nil || u == nil {
			httpx.Err(w, http.StatusInternalServerError, 500, "user.create.failed")
			return
		}
		h.provision(w, r, u)
		return
	}
	if u, ok := h.requireUser(w, r); ok {
		h.provision(w, r, u)
	}
}

// issueToken handles POST /api/v1/user/token → IssuedTokenResponse. Issues a
// stored application access token for direct cloud-API access. The
// token store is deferred; return the shape so the dashboard's call succeeds (empty token = no
// direct-API session). User-initialized-gated.
func (h *Handler) issueToken(w http.ResponseWriter, r *http.Request) {
	u, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Scopes            []string `json:"scopes"`
		DurationInSeconds int      `json:"durationInSeconds"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Scopes == nil {
		req.Scopes = []string{}
	}
	_ = u
	httpx.OK(w, map[string]any{"token": "", "expiresInSeconds": 0, "scopes": req.Scopes})
}

// GET /api/v1/account/details -> AccountDetailsDTO (raw, no envelope).
func (h *Handler) accountDetails(w http.ResponseWriter, r *http.Request) {
	if u, ok := h.requireUser(w, r); ok {
		writeRaw(w, toAccountDetails(u))
	}
}

// POST /api/v1/account/name -> User (raw). Update-if-non-blank; max len 100.
func (h *Handler) updateName(w http.ResponseWriter, r *http.Request) {
	u, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var req struct {
		FirstName string `json:"firstName"`
		LastName  string `json:"lastName"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if len(req.FirstName) > 100 || len(req.LastName) > 100 {
		httpx.Err(w, http.StatusBadRequest, 400, "validation.size")
		return
	}
	updated, _, err := h.users.UpdateName(r.Context(), u.Sub, req.FirstName, req.LastName)
	if err != nil || updated == nil {
		httpx.Err(w, http.StatusInternalServerError, 500, "user.update.failed")
		return
	}
	ev := audit.UserEvent(updated.Sub, updated.FullName())
	ev.Action = audit.ActionUpdate
	ev.ResourceType = audit.ResourceUser
	ev.ResourceID = updated.ID
	ev.ResourceDisplayName = updated.Email
	ev.Outcome = audit.OutcomeSuccess
	h.audit.LogAsync(ev)
	writeRaw(w, updated)
}

// customInfoAudit emits a USER customInfo-change event.
func (h *Handler) customInfoAudit(u *user.User, key string) {
	ev := audit.ClientUserEvent(u.Sub, u.FullName())
	ev.EventContext = audit.ContextUser
	ev.Action = audit.ActionUpdate
	ev.ResourceType = audit.ResourceUser
	ev.ResourceID = u.ID
	ev.ResourceDisplayName = u.Email
	ev.ResourceMetadata = map[string]any{"field": "customInfo", "key": key}
	ev.Outcome = audit.OutcomeSuccess
	h.audit.LogAsync(ev)
}

// POST /api/v1/user/custom-info/{key}?value=... -> CustomHttpResponse<Map>.
func (h *Handler) setCustomInfo(w http.ResponseWriter, r *http.Request) {
	u, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	key := chi.URLParam(r, "key")
	ci, err := h.users.SetCustomInfo(r.Context(), u.Sub, key, r.URL.Query().Get("value"))
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, 500, "user.update.failed")
		return
	}
	h.customInfoAudit(u, key)
	httpx.OK(w, ci)
}

// DELETE /api/v1/user/custom-info/{key} -> CustomHttpResponse<Map>.
func (h *Handler) deleteCustomInfo(w http.ResponseWriter, r *http.Request) {
	u, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	key := chi.URLParam(r, "key")
	ci, err := h.users.DeleteCustomInfo(r.Context(), u.Sub, key)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, 500, "user.update.failed")
		return
	}
	h.customInfoAudit(u, key)
	httpx.OK(w, ci)
}

func writeRaw(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

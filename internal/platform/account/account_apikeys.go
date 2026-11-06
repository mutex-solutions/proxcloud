package account

// account_apikeys.go serves the self-service Personal Access Token (PAT) surface:
//
//	GET    /api/v1/user/api-keys        list the caller's tokens (secret stripped)
//	POST   /api/v1/user/api-keys        mint a token — the secret is returned ONCE
//	DELETE /api/v1/user/api-keys/{id}   revoke one of the caller's tokens
//
// A PAT is a `pk<md5>.sk<sha1>` pair (same shape as the admin hmac key) stored in the
// client_api_keys collection with the owning user's `sub`. Presented as `Authorization: Bearer
// pk.sk`, the auth layer (pkg/auth/clientkey.go) resolves it to that user's principal, so it
// authenticates BOTH the client REST API and the client MCP toolset — e.g. from a Terraform module.
// Tokens are USER-scoped (act across the user's projects/orgs, exactly like their SSO session) and
// never carry admin rights. Every mutation is scoped to `sub == caller` so a user only ever sees or
// revokes their own tokens.

import (
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/auth"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// maxPATDescription caps the free-text label. The collection (client_api_keys) is bound at wiring
// time via pg.C(...) — see cmd/api/main.go and account.NewHandler.
const (
	maxPATDescription = 200
	maxPATDays        = 3650
)

// apiKeyView is the safe projection returned by list/create — the secret half is NEVER included
// (it exists only in the create response's one-time `token`).
type apiKeyView struct {
	ID          string     `json:"id"`
	Description string     `json:"description"`
	CreatedAt   *time.Time `json:"createdAt"`
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
	// Scope is "full" (default, also what tokens from before scopes mean) or "read".
	Scope     string     `json:"scope,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// listAPIKeys returns the caller's tokens, newest first, secret stripped.
func (h *Handler) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	u, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var keys []apiKeyView
	if err := h.pat.Find(r.Context(), pgdoc.M{"sub": u.Sub}, &keys,
		pgdoc.Sort(pgdoc.Desc("createdAt"))); err != nil {
		httpx.Err(w, http.StatusInternalServerError, 500, "internal.error")
		return
	}
	if keys == nil {
		keys = []apiKeyView{}
	}
	httpx.OK(w, keys)
}

// createAPIKey mints a `pk.sk` pair for the caller and returns the full token ONCE. Body:
// {description?}. The stored doc keeps only the secret half + the owning sub; the plaintext token
// is never re-served by list/get.
func (h *Handler) createAPIKey(w http.ResponseWriter, r *http.Request) {
	u, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Description   string `json:"description"`
		Scope         string `json:"scope"`
		ExpiresInDays int    `json:"expiresInDays"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if len(req.Description) > maxPATDescription {
		httpx.Err(w, http.StatusBadRequest, 400, "validation.size")
		return
	}
	if req.Scope == "" {
		req.Scope = auth.PATScopeFull
	}
	if req.Scope != auth.PATScopeFull && req.Scope != auth.PATScopeRead {
		httpx.Err(w, http.StatusBadRequest, 400, "scope must be full or read")
		return
	}
	if req.ExpiresInDays < 0 || req.ExpiresInDays > maxPATDays {
		httpx.Err(w, http.StatusBadRequest, 400, "expiresInDays is out of range")
		return
	}
	m := md5.Sum([]byte(uuid.NewString()))
	s := sha1.Sum([]byte(uuid.NewString()))
	pk := "pk" + hex.EncodeToString(m[:])
	sk := "sk" + hex.EncodeToString(s[:])
	now := time.Now().UTC()
	doc := pgdoc.M{
		"_id":         pk,
		"secretKey":   sk,
		"sub":         u.Sub,
		"description": req.Description,
		"createdAt":   now,
		"scope":       req.Scope,
	}
	var expiresAt *time.Time
	if req.ExpiresInDays > 0 {
		e := now.AddDate(0, 0, req.ExpiresInDays)
		expiresAt = &e
		doc["expiresAt"] = e
	}
	if _, err := h.pat.InsertOne(r.Context(), doc); err != nil {
		httpx.Err(w, http.StatusInternalServerError, 500, "internal.error")
		return
	}
	h.apiKeyAudit(u, audit.ActionCreate, pk)
	// token = the Bearer value the user pastes into a client / TF provider — shown exactly once.
	httpx.OK(w, map[string]any{
		"id":          pk,
		"token":       pk + "." + sk,
		"description": req.Description,
		"createdAt":   now,
		"scope":       req.Scope,
		"expiresAt":   expiresAt,
	})
}

// deleteAPIKey revokes one of the caller's tokens. Scoped to sub == caller so a user can only
// revoke their own; an unknown / not-owned id is a silent no-op (200).
func (h *Handler) deleteAPIKey(w http.ResponseWriter, r *http.Request) {
	u, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	deleted, err := h.pat.DeleteOne(r.Context(), pgdoc.M{"_id": id, "sub": u.Sub})
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, 500, "internal.error")
		return
	}
	// Only audit a real revoke — an unknown / not-owned id is a silent no-op, so it must not emit
	// (and can't be used to spam the security log).
	if deleted {
		h.apiKeyAudit(u, audit.ActionDelete, id)
	}
	httpx.OK(w, map[string]any{})
}

// apiKeyAudit emits a USER_ACCOUNT event for a token create/revoke (best-effort, async).
func (h *Handler) apiKeyAudit(u *user.User, action, keyID string) {
	ev := audit.ClientUserEvent(u.Sub, u.FullName())
	ev.EventContext = audit.ContextUser
	ev.Action = action
	ev.ResourceType = audit.ResourceUser
	ev.ResourceID = u.ID
	ev.ResourceDisplayName = u.Email
	ev.ResourceMetadata = map[string]any{"field": "apiKey", "keyId": keyID}
	ev.Outcome = audit.OutcomeSuccess
	h.audit.LogAsync(ev)
}

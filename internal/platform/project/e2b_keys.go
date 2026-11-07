package project

// e2b_keys.go — sandbox API keys, the E2B_API_KEY of the E2B-compatible API (e2b.go):
//
//	GET    /api/v1/project/{id}/sandbox-api-keys        list the project's keys (no secrets)
//	POST   /api/v1/project/{id}/sandbox-api-keys        mint a key {name?}; the key is returned ONCE
//	DELETE /api/v1/project/{id}/sandbox-api-keys/{keyId} revoke a key
//
// A key is bound to ONE project (E2B's keys are team-scoped and carry no project) and acts as the
// member who created it, re-checked on every call. Only its SHA-256 is stored (sandbox_api_keys,
// _id = hash), so a database read never yields a usable key. The `e2b_` prefix keeps older SDK
// releases, which validated the prefix, working.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const sandboxKeyPrefix = "e2b_"

// SetSandboxKeys wires the sandbox_api_keys collection (nil = the E2B API rejects every key).
func (h *Handler) SetSandboxKeys(s *pgdoc.Store) { h.sbKeys = s }

type sandboxKey struct {
	ID         string     `json:"id"` // sha256(key), hex
	ProjectID  string     `json:"projectId"`
	Sub        string     `json:"sub"`
	Name       string     `json:"name"`
	Hint       string     `json:"hint"` // e2b_1234…abcd
	CreatedAt  *time.Time `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

func hashSandboxKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (h *Handler) lookupSandboxKey(ctx context.Context, key string) (*sandboxKey, error) {
	if h.sbKeys == nil || !strings.HasPrefix(key, sandboxKeyPrefix) || len(key) != len(sandboxKeyPrefix)+40 {
		return nil, nil
	}
	var k sandboxKey
	found, err := h.sbKeys.Get(ctx, hashSandboxKey(key), &k)
	if err != nil || !found {
		return nil, err
	}
	k.ID = hashSandboxKey(key)
	return &k, nil
}

// touchSandboxKey records the last use, at most once a minute per key (best-effort, async).
func (h *Handler) touchSandboxKey(id string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		now := time.Now().UTC()
		_, _ = h.sbKeys.SetFields(ctx,
			pgdoc.M{"_id": id, "$or": []pgdoc.M{{"lastUsedAt": pgdoc.M{"$exists": false}}, {"lastUsedAt": pgdoc.M{"$lt": now.Add(-time.Minute)}}}},
			pgdoc.M{"lastUsedAt": now}, nil)
	}()
}

func (h *Handler) sandboxKeyProject(w http.ResponseWriter, r *http.Request) (*Project, *user.User, bool) {
	if h.sbKeys == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "sandbox API keys are not configured")
		return nil, nil, false
	}
	u, ok := h.principal(w, r)
	if !ok {
		return nil, nil, false
	}
	p, ok := h.project(w, r, u, rbac.ProjectCloudResourceManage)
	if !ok {
		return nil, nil, false
	}
	return p, u, true
}

// sandboxKeyAudit emits a PROJECT event for a key create/revoke (never the key itself).
func (h *Handler) sandboxKeyAudit(u *user.User, p *Project, action, keyID, name string) {
	ev := audit.ClientUserEvent(u.Sub, u.FullName())
	ev.EventContext = audit.ContextProject
	ev.Action = action
	ev.ResourceType = audit.ResourceProject
	ev.ResourceID = p.ID
	ev.ResourceDisplayName = p.Name
	ev.OrganizationID = p.OrganizationID
	ev.ProjectID = p.ID
	ev.ResourceMetadata = map[string]any{"field": "sandboxApiKey", "keyId": keyID[:min(12, len(keyID))], "name": name}
	ev.Outcome = audit.OutcomeSuccess
	h.audit.LogAsync(ev)
}

func (h *Handler) sandboxKeyList(w http.ResponseWriter, r *http.Request) {
	p, _, ok := h.sandboxKeyProject(w, r)
	if !ok {
		return
	}
	var keys []sandboxKey
	if err := h.sbKeys.Find(r.Context(), pgdoc.M{"projectId": p.ID}, &keys, pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime))); err != nil {
		h.fail(w, err)
		return
	}
	if keys == nil {
		keys = []sandboxKey{}
	}
	httpx.OK(w, keys)
}

func (h *Handler) sandboxKeyCreate(w http.ResponseWriter, r *http.Request) {
	p, u, ok := h.sandboxKeyProject(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	req.Name = strings.TrimSpace(req.Name)
	if len(req.Name) > 100 {
		h.fail(w, httpx.BadRequest("name must be at most 100 characters"))
		return
	}
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		h.fail(w, err)
		return
	}
	key := sandboxKeyPrefix + hex.EncodeToString(b)
	now := time.Now().UTC()
	id := hashSandboxKey(key)
	doc := pgdoc.M{
		"_id": id, "projectId": p.ID, "sub": u.Sub, "name": req.Name,
		"hint": key[:8] + "…" + key[len(key)-4:], "createdAt": now,
	}
	if _, err := h.sbKeys.InsertOne(r.Context(), doc); err != nil {
		h.fail(w, err)
		return
	}
	h.sandboxKeyAudit(u, p, audit.ActionCreate, id, req.Name)
	httpx.OK(w, map[string]any{"id": id, "key": key, "name": req.Name, "hint": doc["hint"], "createdAt": now})
}

func (h *Handler) sandboxKeyDelete(w http.ResponseWriter, r *http.Request) {
	p, u, ok := h.sandboxKeyProject(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "keyId")
	deleted, err := h.sbKeys.DeleteOne(r.Context(), pgdoc.M{"_id": id, "projectId": p.ID})
	if err != nil {
		h.fail(w, err)
		return
	}
	if deleted {
		h.sandboxKeyAudit(u, p, audit.ActionDelete, id, "")
	}
	httpx.OK(w, map[string]any{})
}

package admin

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
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// hmackey.go serves the hmac-keys surface (/api/v1/admin/hmac-keys). The bare GET list
// is already registered in handler.go via listRaw("admin:hmac_key:manage", "hmac_keys");
// this file adds the two endpoints it does NOT cover:
//   - GET    /hmac-keys/{keyId}  load by id → single, or 404 "HMAC Key Not Found".
//   - DELETE /hmac-keys/{keyId}  load by id → if present audit+delete; an absent key is a
//     SILENT no-op (NO 404). Returns HTTP 200 with an empty body.
//
// All endpoints gate on the admin:hmac_key:manage permission. The HmacKey domain stores its String id (e.g.
// "pk<md5>") in `_id` and serializes description/createdAt/updatedAt → shapeDoc (_id→id, drop
// _class) shapes the response; secretKey is STRIPPED from the by-id read (as from the list) so the
// secret half of the pair never reaches the browser — it is served in full only once, at generate
// time. Audit deferred (// TODO(audit)).

const hmacKeyPerm = "admin:hmac_key:manage"

const hmacKeyCollection = "hmac_keys"

// hmacKeyPurposeAdminAPI marks an hmac key as an Admin-API / MCP credential (vs a provider key).
// The SigV4 verifier only resolves keys carrying this purpose.
const hmacKeyPurposeAdminAPI = "admin-api"

// routeHmacKey registers the HmacKey admin mutation/by-id routes. The bare list (GET /hmac-keys)
// stays in handler.go.
func (h *Handler) routeHmacKey(r chi.Router) {
	r.Post("/hmac-keys", h.hmacKeyGenerate)
	r.Get("/hmac-keys/{id}", h.hmacKeyGet)
	r.Delete("/hmac-keys/{id}", h.hmacKeyDelete)
}

// hmacKeyGenerate mints an Admin-API SigV4 key pair (previously
// only reachable via the operator shell command / mgmt gen-hmac-key). Returns the secret ONCE —
// it is stored verbatim and never re-served in full by the list/get reads. Body {description?}.
func (h *Handler) hmacKeyGenerate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, hmacKeyPerm) {
		return
	}
	var req struct {
		Description string `json:"description"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	m := md5.Sum([]byte(uuid.NewString()))
	s := sha1.Sum([]byte(uuid.NewString()))
	id := "pk" + hex.EncodeToString(m[:])
	secret := "sk" + hex.EncodeToString(s[:])
	now := time.Now().UTC()
	// purpose:"admin-api" marks this as an Admin-API / MCP credential; the SigV4 verifier only
	// resolves admin-api-purpose keys, so provider keys (which stamp purpose:"provider") can never
	// authenticate to the Admin API.
	doc := pgdoc.M{"_id": id, "secretKey": secret, "description": req.Description, "createdAt": now, "purpose": hmacKeyPurposeAdminAPI}
	if err := h.repo.InsertDocKeepID(r.Context(), hmacKeyCollection, doc); httpx.WriteError(w, err) {
		return
	}
	// The generate response carries the plaintext secret (shown once, client must save it).
	httpx.OK(w, map[string]any{"id": id, "secretKey": secret, "description": req.Description, "createdAt": now})
}

// hmacKeyGet loads a single HMAC key by id; 404 "HMAC Key Not Found" if absent.
func (h *Handler) hmacKeyGet(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, hmacKeyPerm) {
		return
	}
	doc, err := h.repo.FindDoc(r.Context(), hmacKeyCollection, chi.URLParam(r, "id"))
	if httpx.WriteError(w, err) {
		return
	}
	if doc == nil {
		httpx.WriteError(w, httpx.NotFound("HMAC Key Not Found"))
		return
	}
	// Strip secretKey — the secret half of the pair must never reach the browser (mirrors the
	// list handler). It is served in full ONCE, at generate time.
	d := shapeDoc(doc)
	delete(d, "secretKey")
	httpx.OK(w, d)
}

// hmacKeyDelete deletes the key by id (auditing) when present. An absent key is a SILENT no-op
// (NO 404). Returns no envelope → HTTP 200 with an empty body.
func (h *Handler) hmacKeyDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, hmacKeyPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.repo.FindDoc(r.Context(), hmacKeyCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing != nil {
		if _, err := h.repo.DeleteDoc(r.Context(), hmacKeyCollection, id); httpx.WriteError(w, err) {
			return
		}
		// TODO(audit): write an admin audit event when an HMAC key is deleted.
	}
	w.WriteHeader(http.StatusOK) // HTTP 200, empty body (found or not)
}

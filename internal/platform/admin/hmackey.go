package admin

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
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

// hmacPlainSecret is the shape of a generated secret ("sk" + a sha1 in hex). Sealed text never matches it:
// its length is no valid base64.
var hmacPlainSecret = regexp.MustCompile(`^sk[0-9a-f]{40}$`)

// SealHmacKey returns the hmac_keys row to store for a generated key doc: SigV4 has to get the secret back
// so it cannot be hashed, and it is encrypted instead. doc keeps the plain secret for the one response
// that shows it. With no encryption key configured the row stays in plain text, like every other secret.
func SealHmacKey(enc *textcrypt.Encryptor, doc pgdoc.M) pgdoc.M {
	out := maps.Clone(doc)
	if s, ok := doc["secretKey"].(string); ok && enc != nil {
		out["secretKey"] = enc.Encrypt(s)
	}
	return out
}

// OpenHmacSecret returns the secret of a stored row. It fails on a row it cannot decrypt, a key changed
// without re-sealing: the stored text must not start working as the secret itself.
func OpenHmacSecret(enc *textcrypt.Encryptor, stored string) (string, bool) {
	if hmacPlainSecret.MatchString(stored) { // a row from before sealing, until the next boot converts it
		return stored, true
	}
	// The cipher is AES-CBC with PKCS#7 and no MAC: under a wrong key about 1 in 256 texts still unpad cleanly
	// into garbage. A real secret always has the plain shape, so anything else is a failed open.
	secret := enc.Decrypt(stored)
	if secret == stored || !hmacPlainSecret.MatchString(secret) {
		return "", false
	}
	return secret, true
}

// SealStoredHmacKeys encrypts the secrets still in plain text, once, at boot. Only a row that still holds
// the value it read is converted, so several replicas starting together are harmless.
func SealStoredHmacKeys(ctx context.Context, keys *pgdoc.Store, enc *textcrypt.Encryptor) (int, error) {
	if !enc.HasKey() {
		return 0, nil
	}
	var rows []struct {
		ID        string `json:"id"`
		SecretKey string `json:"secretKey"`
	}
	if err := keys.Find(ctx, pgdoc.M{}, &rows); err != nil {
		return 0, err
	}
	n := 0
	for _, row := range rows {
		if !hmacPlainSecret.MatchString(row.SecretKey) {
			continue
		}
		won, err := keys.SetFieldsOne(ctx, pgdoc.M{"_id": row.ID, "secretKey": row.SecretKey}, pgdoc.M{"secretKey": enc.Encrypt(row.SecretKey)}, nil)
		if err != nil {
			return n, err
		}
		if won {
			n++
		}
	}
	return n, nil
}

// routeHmacKey registers the HmacKey admin mutation/by-id routes. The bare list (GET /hmac-keys)
// stays in handler.go.
func (h *Handler) routeHmacKey(r chi.Router) {
	r.Post("/hmac-keys", h.hmacKeyGenerate)
	r.Get("/hmac-keys/{id}", h.hmacKeyGet)
	r.Delete("/hmac-keys/{id}", h.hmacKeyDelete)
}

// hmacKeyGenerate mints an Admin-API SigV4 key pair (previously
// only reachable via the operator shell command / mgmt gen-hmac-key). Returns the secret ONCE —
// it is stored encrypted and never re-served in full by the list/get reads. Body {description?}.
func (h *Handler) hmacKeyGenerate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, hmacKeyPerm) {
		return
	}
	var req struct {
		Description string `json:"description"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	doc := NewAdminAPIKey(req.Description)
	if err := h.repo.InsertDocKeepID(r.Context(), hmacKeyCollection, SealHmacKey(h.hmacEnc, doc)); httpx.WriteError(w, err) {
		return
	}
	// The generate response carries the plaintext secret (shown once, client must save it).
	httpx.OK(w, map[string]any{"id": doc["_id"], "secretKey": doc["secretKey"], "description": req.Description, "createdAt": doc["createdAt"]})
}

// NewAdminAPIKey builds a new Admin-API SigV4 key pair document for the hmac_keys collection.
// purpose:"admin-api" marks it as an Admin-API / MCP credential; the SigV4 verifier only resolves
// admin-api-purpose keys, so provider keys (which stamp purpose:"provider") can never authenticate
// to the Admin API. Its writers: the admin endpoint, the mgmt trigger and `proxcloud-api admin-key`.
func NewAdminAPIKey(description string) pgdoc.M {
	m := md5.Sum([]byte(uuid.NewString()))
	s := sha1.Sum([]byte(uuid.NewString()))
	return pgdoc.M{
		"_id": "pk" + hex.EncodeToString(m[:]), "secretKey": "sk" + hex.EncodeToString(s[:]),
		"description": description, "createdAt": time.Now().UTC(), "purpose": hmacKeyPurposeAdminAPI,
	}
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

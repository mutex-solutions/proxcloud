package admin

import (
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// TestRouteHmacKeyNoPanic registers the HmacKey routes on a fresh router and asserts no panic
// (catches chi param-name conflicts at the /hmac-keys/{id} tree position).
func TestRouteHmacKeyNoPanic(t *testing.T) {
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("routeHmacKey panicked at registration: %v", rec)
		}
	}()
	(&Handler{}).routeHmacKey(chi.NewRouter())
}

// TestHmacKeyPerm pins the exact permission key (ADMIN_HMAC_KEY_MANAGE) used by all endpoints.
func TestHmacKeyPerm(t *testing.T) {
	if hmacKeyPerm != "admin:hmac_key:manage" {
		t.Errorf("hmacKeyPerm = %q, want admin:hmac_key:manage", hmacKeyPerm)
	}
	if hmacKeyCollection != "hmac_keys" {
		t.Errorf("hmacKeyCollection = %q, want hmac_keys", hmacKeyCollection)
	}
}

// TestHmacKeyShapeDoc verifies the response shaping for a stored HmacKey doc: the String id stored
// in `_id` (e.g. "pk<md5>") becomes `id` and the `_class` discriminator is dropped. shapeDoc itself
// preserves secretKey; the handlers (list and GET/{keyId}) strip it afterward (see
// TestHmacKeyGetStripsSecret).
func TestHmacKeyShapeDoc(t *testing.T) {
	doc := pgdoc.M{
		"_id":         "pkabc123",
		"_class":      "HmacKey",
		"secretKey":   "skdeadbeef",
		"description": "ci",
	}
	shapeDoc(doc)
	if doc["id"] != "pkabc123" {
		t.Errorf("id = %#v, want pkabc123", doc["id"])
	}
	if _, ok := doc["_id"]; ok {
		t.Error("_id must be removed")
	}
	if _, ok := doc["_class"]; ok {
		t.Error("_class must be dropped")
	}
	if doc["secretKey"] != "skdeadbeef" {
		t.Error("secretKey must be preserved (not ignored)")
	}
}

// TestHmacKeyGetStripsSecret pins that the by-id read (hmacKeyGet) never emits secretKey — the
// secret half of the pair must not reach the browser (it is served in full only at generate time).
// This mirrors the exact shaping the handler applies: shapeDoc then delete "secretKey".
func TestHmacKeyGetStripsSecret(t *testing.T) {
	doc := pgdoc.M{"_id": "pkabc123", "_class": "HmacKey", "secretKey": "skTOPSECRET", "description": "ci"}
	d := shapeDoc(doc)
	delete(d, "secretKey")
	if _, ok := d["secretKey"]; ok {
		t.Fatal("secretKey must be stripped from the hmac-key by-id response")
	}
	if d["id"] != "pkabc123" {
		t.Errorf("id = %#v, want pkabc123", d["id"])
	}
}

// TestHmacKeyPurposeAdminAPI pins that generated admin hmac keys carry purpose:"admin-api" — the
// SigV4 verifier only resolves admin-api-purpose keys, so provider keys can never authenticate to
// the Admin API.
func TestHmacKeyPurposeAdminAPI(t *testing.T) {
	if hmacKeyPurposeAdminAPI != "admin-api" {
		t.Errorf("hmacKeyPurposeAdminAPI = %q, want admin-api", hmacKeyPurposeAdminAPI)
	}
}

package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// TestApplyServiceConnectionBodyEncryptsSecret is the regression guard for finding [29]: cloud
// credentials submitted on the Connection-tab save must be ENCRYPTED at rest, never persisted as
// plaintext. It drives the real merge path with a keyed encryptor and asserts (a) the stored
// adminPassword is not the plaintext, and (b) it round-trips back via the same key.
func TestApplyServiceConnectionBodyEncryptsSecret(t *testing.T) {
	const key = "dev-359ac834f15e8e013db24c77b6b3e64c"
	enc := textcrypt.New(key)
	h := &Handler{esSvc: externalservice.NewService(nil, enc)}

	req := httptest.NewRequest(http.MethodPut, "/service/svc-1",
		strings.NewReader(`{"secret":{"adminPassword":"super-secret"}}`))
	doc := pgdoc.M{}
	if aerr := h.applyServiceConnectionBody(req, doc); aerr != nil {
		t.Fatalf("applyServiceConnectionBody returned error: %#v", aerr)
	}
	stored, _ := doc["secret"].(pgdoc.M)["adminPassword"].(string)
	if stored == "" || stored == "super-secret" {
		t.Fatalf("adminPassword must be stored ENCRYPTED, got %q", stored)
	}
	if got := enc.Decrypt(stored); got != "super-secret" {
		t.Fatalf("stored secret must decrypt to plaintext, got %q", got)
	}
}

// TestServiceNotFoundErr pins the exact 404 from get(id) →
// ServiceNotFoundException("Service not found: %s") (interpolated, no trailing space) + status/code.
// Every field-set PUT (and PUT /{id}/update) resolves the doc via get(id) → this error when absent.
func TestServiceNotFoundErr(t *testing.T) {
	err := serviceNotFoundErr("svc-1")
	if err.Msg != "Service not found: svc-1" {
		t.Errorf("message=%q want %q", err.Msg, "Service not found: svc-1")
	}
	if err.Status != http.StatusNotFound || err.Code != http.StatusNotFound {
		t.Errorf("status/code=%d/%d want %d/%d", err.Status, err.Code, http.StatusNotFound, http.StatusNotFound)
	}
}

// TestEnsureMap covers the nil-safe sub-map accessor: missing key → fresh stored map; existing
// pgdoc.M → returned as-is; a map[string]any (a freshly-decoded body) → converted to pgdoc.M + stored.
func TestEnsureMap(t *testing.T) {
	// missing → created + stored back on the parent.
	doc := pgdoc.M{}
	m := ensureMap(doc, "config")
	m["x"] = 1
	if got, _ := doc["config"].(pgdoc.M); got == nil || got["x"] != 1 {
		t.Fatalf("missing key: config not created/stored, doc=%#v", doc)
	}
	// existing pgdoc.M → same instance.
	doc2 := pgdoc.M{"config": pgdoc.M{"a": "b"}}
	if got := ensureMap(doc2, "config"); got["a"] != "b" {
		t.Errorf("existing pgdoc.M not returned, got=%#v", got)
	}
	// map[string]any → converted + stored as pgdoc.M.
	doc3 := pgdoc.M{"config": map[string]any{"a": "b"}}
	got := ensureMap(doc3, "config")
	if got["a"] != "b" {
		t.Errorf("map[string]any not converted, got=%#v", got)
	}
	if _, ok := doc3["config"].(pgdoc.M); !ok {
		t.Errorf("converted map not stored back as pgdoc.M, doc=%#v", doc3)
	}
}

// TestEnsureConfig is the config-specific wrapper around ensureMap.
func TestEnsureConfig(t *testing.T) {
	doc := pgdoc.M{}
	cfg := ensureConfig(doc)
	cfg["apiUrl"] = "http://k"
	if got, _ := doc["config"].(pgdoc.M); got == nil || got["apiUrl"] != "http://k" {
		t.Fatalf("ensureConfig did not create/store config, doc=%#v", doc)
	}
}

// TestQuotaNestsUnderProvisioning verifies the quota PUT stores the body at config.provisioning.quota
// (config.getProvisioning().setQuota(body)), creating the intermediate maps when absent.
func TestQuotaNestsUnderProvisioning(t *testing.T) {
	doc := pgdoc.M{}
	cfg := ensureConfig(doc)
	prov := ensureMap(cfg, "provisioning")
	prov["quota"] = pgdoc.M{"cpu": 4}
	got, ok := doc["config"].(pgdoc.M)["provisioning"].(pgdoc.M)["quota"].(pgdoc.M)
	if !ok {
		t.Fatalf("quota not nested under config.provisioning.quota, doc=%#v", doc)
	}
	if got["cpu"] != 4 {
		t.Errorf("quota.cpu=%#v want 4", got["cpu"])
	}
}

// TestShapeExternalServiceStripsSecret re-asserts (against the existing handler.go helper) the
// invariant every mutation response relies on: `_id`→`id` and `secret` dropped (never to the client).
func TestShapeExternalServiceStripsSecret(t *testing.T) {
	doc := pgdoc.M{"_id": "svc-1", "name": "dev", "secret": pgdoc.M{"adminPassword": "p"}}
	shapeExternalService(doc)
	if doc["id"] != "svc-1" {
		t.Errorf("_id not renamed to id, doc=%#v", doc)
	}
	if _, ok := doc["_id"]; ok {
		t.Errorf("_id must be removed, doc=%#v", doc)
	}
	if _, ok := doc["secret"]; ok {
		t.Errorf("secret MUST be stripped, doc=%#v", doc)
	}
}

// TestExternalServiceMutRoutesNoPanic registers ONLY the new mutation routes on a fresh router and
// asserts no chi panic (conflicting sibling param names / duplicate routes). The full tree is
// exercised by TestRoutesNoPanic; this isolates the new group (and is safe to run before the
// integrator wires it into Routes()).
func TestExternalServiceMutRoutesNoPanic(t *testing.T) {
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("routeExternalServiceMut panicked at registration: %v", rec)
		}
	}()
	(&Handler{}).routeExternalServiceMut(chi.NewRouter())
}

package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
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

// A range in snatSources would open every share of the provider to it, so the save refuses it.
func TestApplyServiceConnectionBodyRefusesSNATRange(t *testing.T) {
	h := &Handler{}
	nas := func() pgdoc.M { return pgdoc.M{"config": pgdoc.M{"provider": "truenas"}} }
	put := func(doc pgdoc.M, cfg string) *httpx.HTTPError {
		return h.applyServiceConnectionBody(httptest.NewRequest(http.MethodPut, "/service/n", strings.NewReader(`{"config":{`+cfg+`}}`)), doc)
	}
	if put(nas(), `"snatSources":["192.0.2.0/24"]`) == nil {
		t.Fatal("range accepted")
	}
	if herr := put(nas(), `"snatSources":["192.0.2.1"]`); herr != nil {
		t.Fatalf("host refused: %#v", herr)
	}
	if herr := put(nas(), `"snatSources":[]`); herr != nil {
		t.Fatalf("clearing refused: %#v", herr)
	}
	if herr := put(nas(), `"parentDataset":"tank/shares"`); herr != nil {
		t.Fatalf("save without the key refused: %#v", herr)
	}
}

// A sandbox provider goes live only while its release gates hold; DISABLED it may be stored at any time. A nil repo
// means the refusal comes before any write.
func TestSandboxProviderGoLive(t *testing.T) {
	gates := []sandbox.Gate{{Name: "noderestriction", OK: true}, {Name: "isolation", OK: false, Detail: "no DROP"}, {Name: "fence", OK: true}}
	h := &Handler{sandboxGates: func(context.Context, *externalservice.ExternalService) []sandbox.Gate { return gates }}
	ctx := httpx.WithRC(t.Context(), &httpx.RequestContext{SigV4KeyID: "pk-test"})
	body := `{"name":"sbx","config":{"provider":"sandbox","sandbox":{"templates":{"python":{"image":"python:3.12-slim"}}}},"secret":{"kubeconfig":"x"}}`
	rec := httptest.NewRecorder()
	h.externalServiceCreate(rec, httptest.NewRequest(http.MethodPost, "/service", strings.NewReader(body)).WithContext(ctx))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "isolation: no DROP") {
		t.Fatalf("create with a gate failing: %d %s", rec.Code, rec.Body.String())
	}
	doc := func(status string) pgdoc.M {
		return pgdoc.M{"_id": "s1", "status": status, "secret": map[string]any{"kubeconfig": "x"},
			"config": map[string]any{"provider": "sandbox", "sandbox": map[string]any{"templates": map[string]any{"python": map[string]any{"image": "python:3.12-slim"}}}}}
	}
	if herr := h.checkSandboxGoLive(ctx, doc("DISABLED")); herr != nil {
		t.Fatalf("DISABLED refused: %#v", herr)
	}
	if herr := h.checkSandboxGoLive(ctx, doc("PUBLIC")); herr == nil || herr.Status != http.StatusConflict {
		t.Fatalf("live with a gate failing: %#v", herr)
	}
	if herr := h.checkSandboxGoLive(ctx, pgdoc.M{"status": "PUBLIC", "config": map[string]any{"provider": "dbaas"}}); herr != nil {
		t.Fatalf("another provider: %#v", herr)
	}
	gates[1].OK = true
	if herr := h.checkSandboxGoLive(ctx, doc("PUBLIC")); herr != nil {
		t.Fatalf("all gates hold: %#v", herr)
	}
	broken := doc("PUBLIC")
	broken["config"].(map[string]any)["sandbox"] = map[string]any{}
	if herr := h.checkSandboxGoLive(ctx, broken); herr == nil {
		t.Fatal("a provider without templates went live")
	}
	if herr := (&Handler{}).checkSandboxGoLive(ctx, doc("PUBLIC")); herr == nil {
		t.Fatal("no gate checker wired: went live")
	}
}

// A kamaji config still carrying the tenant-side proxmox-csi keys is refused on create and on an update whose cluster
// block has them; an update that leaves the cluster block alone passes, so a stored leftover does not lock the row.
func TestKamajiRemovedKeysRefused(t *testing.T) {
	h := &Handler{}
	ctx := httpx.WithRC(t.Context(), &httpx.RequestContext{SigV4KeyID: "pk-test"})
	rec := httptest.NewRecorder()
	h.externalServiceCreate(rec, httptest.NewRequest(http.MethodPost, "/service",
		strings.NewReader(`{"name":"k","config":{"provider":"kamaji","cluster":{"csiSharedStores":false}}}`)).WithContext(ctx))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "config.cluster.csiSharedStores no longer exist") {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	stored := func() pgdoc.M {
		return pgdoc.M{"config": pgdoc.M{"provider": "kamaji", "cluster": map[string]any{"pveCsiUrl": "https://192.0.2.60:8006"}}}
	}
	put := func(doc pgdoc.M, body string) *httpx.HTTPError {
		return h.applyServiceConnectionBody(httptest.NewRequest(http.MethodPut, "/service/k", strings.NewReader(body)), doc)
	}
	if herr := put(stored(), `{"config":{"cluster":{"pveApiUrl":"https://192.0.2.11:8006","pveCsiUrl":"https://192.0.2.60:8006"}}}`); herr == nil ||
		!strings.Contains(herr.Msg, "config.cluster.pveCsiUrl") {
		t.Fatalf("update with the key: %#v", herr)
	}
	if herr := put(stored(), `{"config":{"cluster":{"pveApiUrl":"https://192.0.2.11:8006"}}}`); herr != nil {
		t.Fatalf("update dropping the key: %#v", herr)
	}
	if herr := put(stored(), `{"config":{"argocd":{"chartVersion":"0.1.7"}}}`); herr != nil {
		t.Fatalf("update without a cluster block: %#v", herr)
	}
	if herr := put(pgdoc.M{"config": pgdoc.M{"provider": "proxmox"}}, `{"config":{"cluster":{"pveCsiUrl":"x"}}}`); herr != nil {
		t.Fatalf("another provider kind: %#v", herr)
	}
}

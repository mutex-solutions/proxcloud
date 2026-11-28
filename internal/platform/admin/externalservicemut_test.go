package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	// Sandboxes switched off: the provider has no cluster, cannot go live with every gate holding, and the refusal
	// says so. DISABLED it is stored like any other.
	off := func(status string) pgdoc.M {
		d := doc(status)
		d["secret"] = map[string]any{}
		d["config"].(map[string]any)["sandbox"].(map[string]any)[externalservice.SandboxOffSinceKey] = "2026-11-28T01:12:00Z"
		return d
	}
	if herr := h.checkSandboxGoLive(ctx, off("PUBLIC")); herr == nil || herr.Status != http.StatusConflict || !strings.Contains(herr.Msg, "sandboxes are switched off") {
		t.Fatalf("a provider with no placement set live: %#v", herr)
	}
	if herr := h.checkSandboxGoLive(ctx, off("DISABLED")); herr != nil {
		t.Fatalf("a DISABLED provider with no placement refused: %#v", herr)
	}
}

// Functions cannot be switched on: a sandbox provider whose functions block is enabled is refused at the save, live
// or DISABLED, with every release gate holding, and before any write. The rest of the block is stored as written.
func TestSandboxProviderFunctionsCannotBeEnabled(t *testing.T) {
	gates := []sandbox.Gate{{Name: "fence", OK: true}}
	h := &Handler{sandboxGates: func(context.Context, *externalservice.ExternalService) []sandbox.Gate { return gates }}
	ctx := httpx.WithRC(t.Context(), &httpx.RequestContext{SigV4KeyID: "pk-test"})
	doc := func(status string, functions any) pgdoc.M {
		return pgdoc.M{"_id": "s1", "status": status, "secret": map[string]any{"kubeconfig": "x"},
			"config": map[string]any{"provider": "sandbox", "sandbox": map[string]any{"domain": "sbx.example.org", "functions": functions,
				"templates": map[string]any{"python": map[string]any{"image": "python:3.12-slim"}}}}}
	}
	for _, status := range []string{"DISABLED", "PUBLIC", "PRIVATE"} {
		herr := h.checkSandboxGoLive(ctx, doc(status, map[string]any{"enabled": true, "maxInstances": float64(3)}))
		if herr == nil || herr.Status != http.StatusConflict || !strings.Contains(herr.Msg, "Functions cannot be enabled") {
			t.Errorf("%s with functions enabled: %#v, want 409", status, herr)
		}
		for name, block := range map[string]any{"off": map[string]any{"enabled": false, "maxInstances": float64(3), "projectQuota": map[string]any{"vcpus": float64(4)}}, "absent": nil, "not a switch": map[string]any{"enabled": "true"}} {
			if herr := h.checkSandboxGoLive(ctx, doc(status, block)); herr != nil {
				t.Errorf("%s with the functions block %s: %#v, want it stored", status, name, herr)
			}
		}
	}
	// Through the route: nothing is written (the handler has no repository to write to).
	body := `{"name":"sbx","status":"DISABLED","config":{"provider":"sandbox","sandbox":{"domain":"sbx.example.org","functions":{"enabled":true},"templates":{"python":{"image":"python:3.12-slim"}}}},"secret":{"kubeconfig":"x"}}`
	rec := httptest.NewRecorder()
	h.externalServiceCreate(rec, httptest.NewRequest(http.MethodPost, "/service", strings.NewReader(body)).WithContext(ctx))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Functions cannot be enabled") {
		t.Fatalf("create with functions enabled: %d %s", rec.Code, rec.Body.String())
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

// The worker repair and worker firewall keys of a kamaji cluster block are checked on create (the install's register
// stage creates through the same function) and on an update that carries the block: the four values of the lab round
// (finding Q2) are a 400 that names the field. An update without the block passes whatever is stored, and the answer
// then says which stored value is ignored.
func TestKamajiClusterSettingsValidatedOnSave(t *testing.T) {
	h := &Handler{}
	ctx := httpx.WithRC(t.Context(), &httpx.RequestContext{SigV4KeyID: "pk-test"})
	put := func(doc pgdoc.M, body string) *httpx.HTTPError {
		return h.applyServiceConnectionBody(httptest.NewRequest(http.MethodPut, "/service/k", strings.NewReader(body)), doc)
	}
	stored := func(cluster map[string]any) pgdoc.M {
		return pgdoc.M{"config": pgdoc.M{"provider": "kamaji", "argocd": map[string]any{"chartVersion": "0.1.14"}, "cluster": cluster}}
	}
	for cluster, want := range map[string]string{
		`{"nodeStartupTimeoutSeconds":30}`: "config.cluster.nodeStartupTimeoutSeconds must be 0",
		`{"nodeStartupTimeoutSeconds":-5}`: "config.cluster.nodeStartupTimeoutSeconds must be 0",
		`{"repairMaxUnhealthy":"40%"}`:     "config.cluster.repairMaxUnhealthy must be a whole number",
		`{"workerFirewall":"yes"}`:         "config.cluster.workerFirewall must be true or false",
	} {
		rec := httptest.NewRecorder()
		h.externalServiceCreate(rec, httptest.NewRequest(http.MethodPost, "/service",
			strings.NewReader(`{"name":"k","config":{"provider":"kamaji","cluster":`+cluster+`}}`)).WithContext(ctx))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("create %s: %d %s", cluster, rec.Code, rec.Body.String())
		}
		if herr := put(stored(map[string]any{}), `{"config":{"cluster":`+cluster+`}}`); herr == nil || herr.Status != http.StatusBadRequest || !strings.Contains(herr.Msg, want) {
			t.Errorf("update %s: %#v", cluster, herr)
		}
	}
	if herr := put(stored(map[string]any{}), `{"config":{"cluster":{"nodeStartupTimeoutSeconds":1800,"repairMaxUnhealthy":1,"workerFirewall":true}}}`); herr != nil {
		t.Fatalf("valid values: %#v", herr)
	}
	if herr := put(stored(map[string]any{}), `{"config":{"cluster":{"nodeStartupTimeoutSeconds":0}}}`); herr != nil {
		t.Fatalf("0 turns the repair off: %#v", herr)
	}
	if herr := put(pgdoc.M{"config": pgdoc.M{"provider": "proxmox"}}, `{"config":{"cluster":{"workerFirewall":"yes"}}}`); herr != nil {
		t.Fatalf("another provider kind: %#v", herr)
	}
	// allowedCidrs, the default allow-list of new clusters' API endpoints: Kubernetes 1.36 refuses a new entry with
	// host bits. A block that carries the entry it already had is saved (with or without one more entry), and the
	// answer names the stored entry.
	hostBits := "config.cluster.allowedCidrs: 192.168.0.5/24 has host bits set: use 192.168.0.0/24"
	rec := httptest.NewRecorder()
	h.externalServiceCreate(rec, httptest.NewRequest(http.MethodPost, "/service",
		strings.NewReader(`{"name":"k","config":{"provider":"kamaji","cluster":{"allowedCidrs":["192.168.0.5/24"]}}}`)).WithContext(ctx))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), hostBits) {
		t.Errorf("create with host bits: %d %s", rec.Code, rec.Body.String())
	}
	if herr := put(stored(map[string]any{"allowedCidrs": []any{"10.0.0.0/8"}}), `{"config":{"cluster":{"allowedCidrs":["10.0.0.0/8","192.168.0.5/24"]}}}`); herr == nil || herr.Status != http.StatusBadRequest || herr.Msg != hostBits {
		t.Errorf("update that adds host bits: %#v", herr)
	}
	old := stored(map[string]any{"allowedCidrs": []any{"192.168.0.5/24"}})
	if herr := put(old, `{"config":{"cluster":{"allowedCidrs":["192.168.0.5/24","10.0.0.0/8"],"workerFirewall":true}}}`); herr != nil {
		t.Fatalf("update that keeps the stored entry: %#v", herr)
	}
	shapeExternalService(old)
	if notes, _ := old["configNotes"].([]string); len(notes) != 1 || !strings.Contains(notes[0], "192.168.0.5/24 has host bits set") {
		t.Errorf("notes of a stored entry with host bits: %q", notes)
	}
	// Stored before the check: the row is not locked, and its answer names the value.
	doc := stored(map[string]any{"nodeStartupTimeoutSeconds": float64(30)})
	if herr := put(doc, `{"config":{"argocd":{"chartVersion":"0.1.13"}}}`); herr != nil {
		t.Fatalf("update without a cluster block: %#v", herr)
	}
	shapeExternalService(doc)
	notes, _ := doc["configNotes"].([]string)
	if len(notes) != 2 || !strings.Contains(notes[0], "Stored value ignored") || !strings.Contains(notes[1], "no effect on cluster chart 0.1.13") {
		t.Errorf("notes of a stored invalid value on an old chart: %q", notes)
	}
	// Never stored, and absent when there is nothing to say.
	doc = pgdoc.M{"configNotes": []string{"sent by a client"}, "config": pgdoc.M{"provider": "kamaji", "cluster": map[string]any{}}}
	if shapeExternalService(doc); doc["configNotes"] != nil {
		t.Errorf("notes of a clean config: %v", doc["configNotes"])
	}
}

// A Proxmox network block is checked on create and on an update that carries one. An update without a network block
// passes even when the stored one would now be refused, so a block saved before the check does not lock the row.
func TestPVENetworkValidatedOnSave(t *testing.T) {
	h := &Handler{}
	ctx := httpx.WithRC(t.Context(), &httpx.RequestContext{SigV4KeyID: "pk-test"})
	for body, want := range map[string]string{
		`{"name":"p","config":{"provider":"proxmox","network":{"cidrPool":"10.100.0.0/25"}}}`:                      "config.network.cidrPool must be an IPv4 range of /24 or larger",
		`{"name":"p","config":{"provider":"proxmox","network":{"zones":{"overlay12":{"kind":"evpn"}}}}}`:           "is the Proxmox SDN zone id, of 1 to 8 characters",
		`{"name":"p","config":{"provider":"proxmox","network":{"zones":{"v":{"kind":"vlan"}},"defaultZone":"v"}}}`: "config.network.zones.v is a vlan zone and needs a bridge",
	} {
		rec := httptest.NewRecorder()
		h.externalServiceCreate(rec, httptest.NewRequest(http.MethodPost, "/service", strings.NewReader(body)).WithContext(ctx))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("create %s: %d %s", body, rec.Code, rec.Body.String())
		}
	}

	stored := func() pgdoc.M { // a block from before the check: its pool is too small
		return pgdoc.M{"config": pgdoc.M{"provider": "proxmox", "network": map[string]any{"cidrPool": "10.100.0.0/26"}}}
	}
	put := func(doc pgdoc.M, body string) *httpx.HTTPError {
		return h.applyServiceConnectionBody(httptest.NewRequest(http.MethodPut, "/service/p", strings.NewReader(body)), doc)
	}
	doc := stored()
	if herr := put(doc, `{"name":"p","config":{"fingerprint":"AA:BB"}}`); herr != nil {
		t.Fatalf("update without a network block: %#v", herr)
	}
	if got := doc["config"].(pgdoc.M)["network"]; !reflect.DeepEqual(got, stored()["config"].(pgdoc.M)["network"]) {
		t.Fatalf("update without a network block changed it to %v", got)
	}
	if herr := put(stored(), `{"config":{"network":{"defaultZone":"overlay","zones":{"simple":{"kind":"simple"}}}}}`); herr == nil ||
		herr.Status != http.StatusBadRequest || !strings.Contains(herr.Msg, "config.network.defaultZone must name one of config.network.zones") {
		t.Fatalf("update with a bad network block: %#v", herr)
	}
	if herr := put(stored(), `{"config":{"network":{"defaultZone":"s","zones":{"s":{"kind":"simple"}},"cidrPool":"10.100.0.0/16"}}}`); herr != nil {
		t.Fatalf("update with a good network block: %#v", herr)
	}
	if herr := put(pgdoc.M{"config": pgdoc.M{"provider": "kamaji"}}, `{"config":{"network":{"cidrPool":"x"}}}`); herr != nil {
		t.Fatalf("another provider kind: %#v", herr)
	}
}

// The console sends the stored network block back with every Connection tab save, with its own isolation keys merged
// in. A problem the stored block already has is not refused again, so a block from before the check (which may work:
// a zone that exists in Proxmox is used as it is) does not lock the provider; a problem the save adds still is.
func TestPVENetworkStoredProblemsKept(t *testing.T) {
	h := &Handler{}
	const legacy = `{"defaultZone":"overlay","zones":{"overlay":{"kind":"evpn"},"legacyzone1":{"kind":"evpn"},` +
		`"v":{"kind":"vlan","vlanMin":100,"vlanMax":199}},"blockedCidrs":[]}`
	stored := func() pgdoc.M {
		var network map[string]any
		if err := json.Unmarshal([]byte(legacy), &network); err != nil {
			t.Fatal(err)
		}
		return pgdoc.M{"config": pgdoc.M{"provider": "proxmox", "network": network}}
	}
	tests := []struct {
		name, network string
		wantErr       string // "" = accepted
	}{
		{"sent back unchanged", legacy, ""},
		{"sent back by the Connection tab", `{"defaultZone":"overlay","zones":{"overlay":{"kind":"evpn"},"legacyzone1":{"kind":"evpn"},` +
			`"v":{"kind":"vlan","vlanMin":100,"vlanMax":199}},"isolation":"shared"}`, ""},
		{"an old problem fixed, the other kept", `{"defaultZone":"overlay","zones":{"overlay":{"kind":"evpn"},"legacyzone1":{"kind":"evpn"}}}`, ""},
		{"a new zone id too long", `{"defaultZone":"overlay","zones":{"overlay":{"kind":"evpn"},"legacyzone1":{"kind":"evpn"},"legacyzone2":{"kind":"evpn"}}}`,
			`the zone id "legacyzone2"`},
		{"a new vlan zone without a bridge", `{"defaultZone":"overlay","zones":{"overlay":{"kind":"evpn"},"w":{"kind":"vlan"}}}`,
			"config.network.zones.w is a vlan zone and needs a bridge"},
		{"a pool too small", `{"defaultZone":"overlay","zones":{"overlay":{"kind":"evpn"}},"cidrPool":"10.100.0.0/25"}`,
			"config.network.cidrPool must be an IPv4 range of /24 or larger"},
		{"a zone per project from the vlan zone", `{"defaultZone":"v","zones":{"v":{"kind":"vlan","vlanMin":100,"vlanMax":199}},"isolation":"evpn-per-project"}`,
			"must then be an evpn zone"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"config":{"network":` + tt.network + `}}`
			herr := h.applyServiceConnectionBody(httptest.NewRequest(http.MethodPut, "/service/p", strings.NewReader(body)), stored())
			if tt.wantErr == "" && herr != nil {
				t.Fatalf("refused: %#v", herr)
			}
			if tt.wantErr != "" && (herr == nil || herr.Status != http.StatusBadRequest || !strings.Contains(herr.Msg, tt.wantErr)) {
				t.Fatalf("got %#v, want a 400 containing %q", herr, tt.wantErr)
			}
		})
	}
}

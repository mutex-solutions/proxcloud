//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/function"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// TestAdminFunctionGate: an operator can switch a sandbox provider's functions block on only after a sandbox-proxy
// of that provider has reported to this portal, with the provider's own function wake token, in the last 90 seconds.
// The report is the real one: a POST to the route the proxy calls, on the real records. A report with another token
// is refused and proves nothing; a report older than the window proves nothing either; a new provider, which no
// proxy can have reported for, is never created with functions on. The proof is asked for at the switch: a later
// save of a provider whose functions are on is not refused for a proxy that is silent then, but the scheduler, the
// sealing key and the billing provider are asked at every save. The release check lists the rows, with the price
// warning, only where the function side exists.
func TestAdminFunctionGate(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("test-key-of-at-least-32-characters"))
	const iss, cid, token = "test-iss", "test-cid", "the-function-wake-token-of-sbx"

	projectH := project.NewHandler(project.NewService(project.NewRepo(db), nil, nil, nil, nil), nil, nil, nil, nil, nil, cloud.NewRepo(db), esSvc, nil, nil, "r1")
	keys, err := textcrypt.NewBlobKeys("k1", map[string]string{"k1": "test-key-of-at-least-32-characters"})
	if err != nil {
		t.Fatal(err)
	}
	projectH.SetFunctions(func(*externalservice.ExternalService) (*function.Service, error) {
		return nil, errors.New("no cluster in this test")
	},
		db.C("functions"), db.C("function_deployments"), db.C("function_hosts"), keys)
	if err := projectH.EnsureFunctionIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	proxyAPI := chi.NewRouter()
	projectH.Routes(proxyAPI)

	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, nil, esSvc, "r1", iss, cid)
	gate := admin.FunctionGate{SealingKey: projectH.FunctionSealingKey(), Proxies: projectH.FunctionProxiesReporting}
	h.SetFunctionGate(gate)
	h.SetJobsScheduler(true)
	router := chi.NewRouter()
	router.MethodNotAllowed(httpx.MethodNotAllowedHandler())
	h.Routes(router)

	sandboxBlock := func(enabled bool) pgdoc.M {
		return pgdoc.M{"domain": "sbx.example.org", "templates": pgdoc.M{"python": pgdoc.M{"image": "python:3.12-slim"}},
			"functions": pgdoc.M{"enabled": enabled, "maxInstances": 3}}
	}
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "sbx", "name": "sbx", "type": externalservice.TypeCloud, "status": externalservice.StatusDisabled,
		"config": pgdoc.M{"provider": "sandbox", "sandbox": sandboxBlock(false)},
		"secret": esSvc.EncryptSecret(map[string]any{"kubeconfig": "x", "functionWakeToken": token})})
	// A sandbox provider no platform run built the function side of: no token, the block off.
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "plain", "name": "plain", "type": externalservice.TypeCloud, "status": externalservice.StatusDisabled,
		"config": pgdoc.M{"provider": "sandbox", "sandbox": pgdoc.M{"domain": "plain.example.org", "templates": pgdoc.M{"python": pgdoc.M{"image": "python:3.12-slim"}}}},
		"secret": esSvc.EncryptSecret(map[string]any{"kubeconfig": "x"})})

	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, "/admin"+path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub", Issuer: iss, Azp: cid}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	save := func(enabled bool) (int, string) {
		t.Helper()
		body, _ := json.Marshal(pgdoc.M{"config": pgdoc.M{"sandbox": sandboxBlock(enabled)}})
		return call(http.MethodPut, "/service/sbx", string(body))
	}
	stored := func() bool {
		t.Helper()
		es, err := esSvc.Get(ctx, "sbx")
		if err != nil {
			t.Fatal(err)
		}
		return es.SandboxConfig().Functions.Enabled
	}
	report := func(bearer, replica string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/internal/functions/activity", strings.NewReader(`{"ids":[],"every":5,"replica":"`+replica+`"}`))
		req.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		proxyAPI.ServeHTTP(w, req)
		return w.Code
	}
	age := func(d time.Duration) {
		t.Helper()
		if _, err := db.C("function_proxies").SetFields(ctx, pgdoc.M{"serviceId": "sbx"}, pgdoc.M{"at": time.Now().UTC().Add(-d)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	rows := func(id string) map[string]bool {
		t.Helper()
		code, body := call(http.MethodPost, "/service/"+id+"/sandbox/release-check", "")
		var env struct {
			Data []struct {
				Name   string `json:"name"`
				OK     bool   `json:"ok"`
				Detail string `json:"detail"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &env); code != http.StatusOK || err != nil {
			t.Fatalf("release-check of %s: %d %s", id, code, body)
		}
		out := map[string]bool{}
		for _, g := range env.Data {
			if strings.HasPrefix(g.Name, "function") {
				out[g.Name] = g.OK
				if g.Detail == "" {
					t.Errorf("row %s says nothing", g.Name)
				}
			}
		}
		return out
	}
	refused := func(why, row string, code int, body string) {
		t.Helper()
		if code != http.StatusConflict || !strings.Contains(body, row+": ") || stored() {
			t.Fatalf("%s: %d %s (stored on: %v), want 409 naming %s and nothing stored", why, code, body, stored(), row)
		}
	}

	// No proxy has reported: the switch is refused, and so is a report with a token that is not the provider's.
	code, body := save(true)
	refused("no proxy has reported", "function-proxy", code, body)
	for _, bearer := range []string{"", "not-the-token", "x"} {
		if got := report(bearer, "sandbox-proxy-1"); got != http.StatusUnauthorized {
			t.Fatalf("a report with bearer %q: %d, want 401", bearer, got)
		}
	}
	code, body = save(true)
	refused("after reports that were refused", "function-proxy", code, body)
	if got := rows("sbx"); got["function-proxy"] || !got["function-scheduler"] || !got["function-sealing-key"] || !got["function-billing"] || got["function-price"] || len(got) != 5 {
		t.Fatalf("the function rows before a proxy reports: %v", got)
	}
	if got := rows("plain"); len(got) != 0 {
		t.Fatalf("a provider without the function side lists function rows: %v", got)
	}

	// The proxy reports, with nothing to report: the path works, the switch is taken.
	if got := report(token, "sandbox-proxy-1"); got != http.StatusNoContent {
		t.Fatalf("the proxy's report: %d", got)
	}
	if got := rows("sbx"); !got["function-proxy"] {
		t.Fatalf("the proxy row after a report: %v", got)
	}
	if code, body := save(true); code != http.StatusOK || !stored() {
		t.Fatalf("switch on after a report: %d %s", code, body)
	}

	// Off is always taken. A report older than the window is no proof: on again is refused.
	if code, body := save(false); code != http.StatusOK || stored() {
		t.Fatalf("switch off: %d %s", code, body)
	}
	age(admin.FunctionProxyWindow + time.Second)
	code, body = save(true)
	refused("a report older than the window", "function-proxy", code, body)
	age(admin.FunctionProxyWindow - 10*time.Second)
	if code, body := save(true); code != http.StatusOK || !stored() {
		t.Fatalf("switch on with a report inside the window: %d %s", code, body)
	}

	// On already: a save while the proxy is silent is taken (the alternative would be to switch functions off)...
	age(time.Hour)
	if code, body := save(true); code != http.StatusOK || !stored() {
		t.Fatalf("a later save of a provider whose functions are on, its proxy silent: %d %s", code, body)
	}
	// ...but the scheduler, the sealing key and the billing provider are asked every time.
	for row, broken := range map[string]func(){
		"function-scheduler":   func() { h.SetJobsScheduler(false) },
		"function-sealing-key": func() { g := gate; g.SealingKey = false; h.SetFunctionGate(g) },
		"function-billing":     func() { g := gate; g.ExternalBilling = true; h.SetFunctionGate(g) },
	} {
		broken()
		if code, body := save(true); code != http.StatusConflict || !strings.Contains(body, row+": ") || strings.Contains(body, "function-proxy: ") {
			t.Errorf("a save with functions on and %s not holding: %d %s", row, code, body)
		}
		if code, body := save(false); code != http.StatusOK {
			t.Errorf("off with %s not holding: %d %s", row, code, body)
		}
		h.SetJobsScheduler(true)
		h.SetFunctionGate(gate)
		if got := report(token, "sandbox-proxy-2"); got != http.StatusNoContent {
			t.Fatal(got)
		}
		if code, body := save(true); code != http.StatusOK {
			t.Fatalf("on again: %d %s", code, body)
		}
		age(time.Hour)
	}

	// A new provider cannot be created with functions on: no proxy can have reported for an id that did not exist.
	if got := report(token, "sandbox-proxy-1"); got != http.StatusNoContent {
		t.Fatal(got)
	}
	create, _ := json.Marshal(pgdoc.M{"name": "new", "type": "CLOUD", "status": "DISABLED", "config": pgdoc.M{"provider": "sandbox", "sandbox": sandboxBlock(true)},
		"secret": pgdoc.M{"kubeconfig": "x", "functionWakeToken": token}})
	if code, body := call(http.MethodPost, "/service", string(create)); code != http.StatusConflict || !strings.Contains(body, "function-proxy: ") {
		t.Fatalf("create with functions on: %d %s", code, body)
	}
	if n, err := db.C("externalService").Count(ctx, pgdoc.M{}); err != nil || n != 2 {
		t.Fatalf("providers after the refused create: %d %v", n, err)
	}

	// The price warning: a rule for resource type function in an enabled plan, and only that.
	mustInsert(t, db, "pricePlan", pgdoc.M{"_id": "off-plan", "name": "off", "enabled": false})
	mustInsert(t, db, "pricePlanRule", pgdoc.M{"_id": "r1", "pricePlanId": "off-plan", "resourceType": "function", "timeUnit": "minute"})
	mustInsert(t, db, "pricePlan", pgdoc.M{"_id": "on-plan", "name": "on", "enabled": true})
	mustInsert(t, db, "pricePlanRule", pgdoc.M{"_id": "r2", "pricePlanId": "on-plan", "resourceType": "sandbox", "timeUnit": "minute"})
	if got := rows("sbx"); got["function-price"] {
		t.Fatalf("a function rule in a plan that is off, and another type's rule in one that is on: %v", got)
	}
	mustInsert(t, db, "pricePlanRule", pgdoc.M{"_id": "r3", "pricePlanId": "on-plan", "resourceType": "function", "timeUnit": "minute"})
	if got := rows("sbx"); !got["function-price"] {
		t.Fatalf("a function rule in an enabled plan: %v", got)
	}
}

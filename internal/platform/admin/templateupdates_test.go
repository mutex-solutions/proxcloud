package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func tuRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	r.Route("/admin", func(r chi.Router) { h.routeTemplateUpdates(r); h.routeThirdPartyIntegration(r) })
	return r
}

func tuDo(h http.Handler, method, path, body string, rc *httpx.RequestContext) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(httpx.WithRC(req.Context(), rc))
	h.ServeHTTP(w, req)
	return w
}

// Every template-updates route is admin-only: a tenant token gets 403, never a GitHub call.
func TestTemplateUpdatesPermissionGates(t *testing.T) {
	h := &Handler{adminIssuer: "operator-realm"}
	r := tuRouter(h)
	tenant := &httpx.RequestContext{Sub: "tenant", Issuer: "tenant-realm"}
	routes := []struct{ method, path string }{
		{"GET", "/admin/template-updates"}, {"GET", "/admin/template-updates/status"}, {"GET", "/admin/template-updates/history"},
		{"GET", "/admin/template-updates/placement/svc"}, {"POST", "/admin/template-updates/vm"}, {"POST", "/admin/template-updates/sandbox"},
		{"POST", "/admin/template-updates/sandbox/rollback"}, {"GET", "/admin/template-updates/sandbox/state"}, {"GET", "/admin/template-updates/sandbox/pins"},
	}
	for _, rt := range routes {
		if w := tuDo(r, rt.method, rt.path, "{}", tenant); w.Code != http.StatusForbidden {
			t.Errorf("%s %s as tenant: %d", rt.method, rt.path, w.Code)
		}
	}
	// An operator credential passes the gate; the unwired service answers 503, the sandbox mirror (flag off) 501.
	op := &httpx.RequestContext{SigV4KeyID: "pk-test"}
	if w := tuDo(r, "GET", "/admin/template-updates", "", op); w.Code != http.StatusServiceUnavailable {
		t.Errorf("unwired discovery: %d", w.Code)
	}
	for _, path := range []string{"/admin/template-updates/sandbox", "/admin/template-updates/sandbox/rollback"} {
		if w := tuDo(r, "POST", path, "{}", op); w.Code != http.StatusNotImplemented || !strings.Contains(w.Body.String(), "images.yaml") {
			t.Errorf("%s with the flag off: %d %s", path, w.Code, w.Body.String())
		}
	}
}

// PROXCLOUD_TEMPLATE_UPDATES_SANDBOX_ENABLED off (the default): mirror and rollback answer 501 even with the service
// wired, before any GitHub, registry or cluster call; the read-only pin list stays available.
func TestSandboxUpdatesFlagOffByDefault(t *testing.T) {
	h := &Handler{adminIssuer: "operator-realm"}
	called := false
	h.SetTemplateUpdates(&templateupdates.Service{Token: func(context.Context) (string, string, error) { called = true; return "ghp_x", "i", nil }})
	h.SetSandboxCluster(func(*externalservice.ExternalService) (SandboxCluster, error) {
		called = true
		return nil, errors.New("no")
	})
	r := tuRouter(h)
	op := &httpx.RequestContext{SigV4KeyID: "pk"}
	body := `{"serviceId":"s","name":"base","digest":"sha256:` + strings.Repeat("a", 64) + `","confirmDigest":"sha256:` + strings.Repeat("a", 64) + `"}`
	if w := tuDo(r, "POST", "/admin/template-updates/sandbox", body, op); w.Code != http.StatusNotImplemented {
		t.Fatalf("mirror with the flag off: %d %s", w.Code, w.Body.String())
	}
	if w := tuDo(r, "POST", "/admin/template-updates/sandbox/rollback", `{"serviceId":"s","name":"base","confirmDigest":"x"}`, op); w.Code != http.StatusNotImplemented {
		t.Fatalf("rollback with the flag off: %d", w.Code)
	}
	if called {
		t.Fatal("a disabled endpoint reached GitHub or the cluster")
	}
	st := h.templateUpdates.GetStatus(context.Background())
	if st.SandboxMirror {
		t.Fatal("status reports the sandbox mirror enabled")
	}
}

// Mirroring needs admin:service:manage: SUPPORT (read-only) gets 403 even with the feature on.
func TestSandboxUpdatesNeedManage(t *testing.T) {
	h := &Handler{adminIssuer: "operator-realm"}
	h.SetTemplateUpdates(&templateupdates.Service{SandboxEnabled: true})
	r := tuRouter(h)
	tenant := &httpx.RequestContext{Sub: "tenant", Issuer: "tenant-realm"}
	for _, path := range []string{"/admin/template-updates/sandbox", "/admin/template-updates/sandbox/rollback"} {
		if w := tuDo(r, "POST", path, "{}", tenant); w.Code != http.StatusForbidden {
			t.Errorf("%s as tenant: %d", path, w.Code)
		}
	}
	if !rbac.Matches(rolePermissions("SUPPORT"), templateUpdatesReadPerm) || rbac.Matches(rolePermissions("SUPPORT"), templateUpdatesManagePerm) {
		t.Fatal("SUPPORT must see the sandbox state but not start a mirror or rollback")
	}
	// An unconfirmed digest is refused before any provider lookup.
	op := &httpx.RequestContext{SigV4KeyID: "pk"}
	if w := tuDo(r, "POST", "/admin/template-updates/sandbox", `{"serviceId":"s","name":"base","digest":"sha256:`+strings.Repeat("a", 64)+`","confirmDigest":""}`, op); w.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed digest: %d %s", w.Code, w.Body.String())
	}
	if w := tuDo(r, "POST", "/admin/template-updates/sandbox", `{"serviceId":"s","name":"base","token":"x"}`, op); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown body field: %d", w.Code)
	}
}

// Read vs manage: the built-in SUPPORT role can view template updates but not start one; ADMIN can.
func TestTemplateUpdatesRoleKeys(t *testing.T) {
	support, admin := rolePermissions("SUPPORT"), rolePermissions("ADMIN")
	if !rbac.Matches(support, templateUpdatesReadPerm) || rbac.Matches(support, templateUpdatesManagePerm) {
		t.Fatal("SUPPORT must read but not manage template updates")
	}
	if !rbac.Matches(admin, templateUpdatesManagePerm) {
		t.Fatal("ADMIN must manage template updates")
	}
	if rbac.Matches(rolePermissions("BILLING_ADMIN"), templateUpdatesReadPerm) {
		t.Fatal("BILLING_ADMIN must not see template updates")
	}
}

// Without a GitHub integration the page says so (409) instead of listing anonymously.
func TestTemplateUpdatesRequiresIntegration(t *testing.T) {
	h := &Handler{adminIssuer: "operator-realm"}
	h.SetTemplateUpdates(&templateupdates.Service{Token: func(context.Context) (string, string, error) { return "", "", nil }})
	w := tuDo(tuRouter(h), "GET", "/admin/template-updates", "", &httpx.RequestContext{SigV4KeyID: "pk"})
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "GitHub integration") {
		t.Fatalf("no integration: %d %s", w.Code, w.Body.String())
	}
}

// A GitHub integration is validated live before it is stored, and the token never reaches the
// response. h.repo is nil: reaching the insert would panic, so a refusal proves nothing was stored.
func TestGitHubIntegrationValidatedBeforeSave(t *testing.T) {
	const pat = "ghp_SAVE_TEST_TOKEN_0123456789"
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+pat {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"message":"Bad credentials"}`)
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo")
		fmt.Fprint(w, `{"login":"octo"}`)
	}))
	defer gh.Close()
	h := &Handler{adminIssuer: "operator-realm", githubOptions: templateupdates.Options{APIBase: gh.URL},
		githubCountFn: func(context.Context) (int64, error) { return 0, nil }}
	r := tuRouter(h)
	op := &httpx.RequestContext{SigV4KeyID: "pk"}
	for name, body := range map[string]string{
		"no token":     `{"thirdParty":"GitHub","secret":{}}`,
		"bad token":    `{"thirdParty":"GitHub","secret":{"token":"ghp_wrong"}}`,
		"no scope":     `{"thirdParty":"GitHub","secret":{"token":"` + pat + `"}}`,
		"extra secret": `{"thirdParty":"GitHub","secret":{"token":"` + pat + `","password":"x"}}`,
	} {
		w := tuDo(r, "POST", "/admin/integrations", body, op)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), pat) {
			t.Errorf("%s: token echoed", name)
		}
	}
	// Item 8: a second GitHub integration is refused (before any GitHub call).
	h.githubCountFn = func(context.Context) (int64, error) { return 1, nil }
	if w := tuDo(r, "POST", "/admin/integrations", `{"thirdParty":"GitHub","secret":{"token":"`+pat+`"}}`, op); w.Code != http.StatusConflict {
		t.Errorf("second integration: %d %s", w.Code, w.Body.String())
	}
}

// Item 7: on update a present token must be a non-empty string; an absent one keeps the stored PAT.
func TestGitHubTokenUpdateRules(t *testing.T) {
	h := &Handler{}
	ctx := context.Background()
	for name, secret := range map[string]any{
		"empty":      map[string]any{"token": ""},
		"blank":      map[string]any{"token": "   "},
		"number":     map[string]any{"token": 42},
		"null":       map[string]any{"token": nil},
		"not object": "ghp_x",
	} {
		if err := h.githubIntegrationCheck(ctx, secret, false); err == nil {
			t.Errorf("%s: accepted on update", name)
		}
	}
	for name, secret := range map[string]any{"absent secret": nil, "absent token": map[string]any{}} {
		if err := h.githubIntegrationCheck(ctx, secret, false); err != nil {
			t.Errorf("%s: %v (must keep the stored token)", name, err)
		}
		if err := h.githubIntegrationCheck(ctx, secret, true); err == nil {
			t.Errorf("%s: accepted on create", name)
		}
	}
}

// Item 5: replaying a template-update job needs admin:service:manage. Checked through the
// permission chooser with a BILLING_ADMIN-like grant set.
func TestTemplateUpdateReplayPermission(t *testing.T) {
	if !rbac.Matches(rolePermissions("BILLING_ADMIN"), "admin:bill:manage") {
		t.Skip("BILLING_ADMIN no longer manages bills")
	}
	if rbac.Matches(rolePermissions("BILLING_ADMIN"), templateUpdatesManagePerm) {
		t.Fatal("BILLING_ADMIN must not replay template update jobs")
	}
}

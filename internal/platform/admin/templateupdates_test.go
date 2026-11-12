package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

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
	}
	for _, rt := range routes {
		if w := tuDo(r, rt.method, rt.path, "{}", tenant); w.Code != http.StatusForbidden {
			t.Errorf("%s %s as tenant: %d", rt.method, rt.path, w.Code)
		}
	}
	// An operator credential passes the gate; the unwired service answers 503, the sandbox seam 501.
	op := &httpx.RequestContext{SigV4KeyID: "pk-test"}
	if w := tuDo(r, "GET", "/admin/template-updates", "", op); w.Code != http.StatusServiceUnavailable {
		t.Errorf("unwired discovery: %d", w.Code)
	}
	if w := tuDo(r, "POST", "/admin/template-updates/sandbox", "{}", op); w.Code != http.StatusNotImplemented || !strings.Contains(w.Body.String(), "images.yaml") {
		t.Errorf("sandbox seam: %d %s", w.Code, w.Body.String())
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
	h := &Handler{adminIssuer: "operator-realm", githubOptions: templateupdates.Options{APIBase: gh.URL}}
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
}

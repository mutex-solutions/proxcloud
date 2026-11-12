package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// TestRoutesNoPanic registers the entire admin route tree on a fresh chi router and asserts it does
// not panic. chi panics at registration time on conflicting sibling param names or duplicate routes
// — this is the guard that catches such conflicts as new controllers are wired into Routes().
func TestRoutesNoPanic(t *testing.T) {
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("admin Routes() panicked at registration: %v", rec)
		}
	}()
	(&Handler{}).Routes(chi.NewRouter())
}

func TestProjectGPUUsageRoute(t *testing.T) {
	router := chi.NewRouter()
	(&Handler{}).Routes(router)
	routeContext := chi.NewRouteContext()
	if !router.Match(routeContext, http.MethodGet, "/admin/project/project-1/gpu-usage") {
		t.Fatal("GET /admin/project/{id}/gpu-usage route is not registered")
	}
}

func TestKamajiCSIGatewayRoute(t *testing.T) {
	router := chi.NewRouter()
	(&Handler{}).Routes(router)
	if !router.Match(chi.NewRouteContext(), http.MethodPost, "/admin/service/svc-1/k8s-clusters/pc-c-1/csi-gateway") {
		t.Fatal("POST /admin/service/{id}/k8s-clusters/{clusterId}/csi-gateway route is not registered")
	}
	if router.Match(chi.NewRouteContext(), http.MethodPost, "/admin/service/svc-1/k8s-clusters/pc-c-1/csi-gatewayx") {
		t.Fatal("negative control matched")
	}
}

// Attaching a provider to a project provisions it (pool, RGW user, namespace…), so it must not be a GET (#214): a
// prefetch, crawler, link preview or a retried safe request would change state. GET answers 405, with the platform's
// envelope where the app router sets it and with chi's default otherwise, and never reaches the handler; POST does.
func TestProjectAttachServiceIsPostOnly(t *testing.T) {
	const path = "/admin/project/p1/external-service/svc-ceph"
	var bootstraps int
	h := &Handler{adminIssuer: "operator-realm", projectCloud: &ProjectCloudOps{Bootstrap: func(context.Context, string, string) error { bootstraps++; return nil }}}
	// The auth middleware always sets a principal; a tenant one is not an admin.
	req := func(method string) *http.Request {
		r := httptest.NewRequest(method, path, nil)
		return r.WithContext(httpx.WithRC(r.Context(), &httpx.RequestContext{Sub: "tenant", Issuer: "tenant-realm"}))
	}
	enveloped := chi.NewRouter()
	enveloped.MethodNotAllowed(httpx.MethodNotAllowedHandler()) // as internal/server.AppRouter
	h.Routes(enveloped)
	plain := chi.NewRouter()
	h.Routes(plain)
	for name, router := range map[string]*chi.Mux{"app envelope": enveloped, "chi default": plain} {
		t.Run(name, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req(method))
				if w.Code != http.StatusMethodNotAllowed {
					t.Fatalf("%s %s = %d %s, want 405", method, path, w.Code, w.Body.String())
				}
				// chi's own 405 lists what the path takes; the app's envelope handler does not set Allow.
				if allow := w.Header().Values("Allow"); router == plain && (len(allow) != 1 || allow[0] != http.MethodPost) {
					t.Errorf("%s: Allow = %q, want only POST", method, allow)
				}
			}
			// POST reaches projectAddExternalService: its first step is the admin:project:update gate, which a
			// tenant principal fails with the handler's own message. (The attach itself, with an operator, is
			// test/integration TestAdminAttachServiceIsPostAndIdempotent.)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req(http.MethodPost))
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), projectUpdatePerm) {
				t.Fatalf("POST %s = %d %s, want the handler's 403 on %s", path, w.Code, w.Body.String(), projectUpdatePerm)
			}
		})
	}
	w := httptest.NewRecorder()
	enveloped.ServeHTTP(w, req(http.MethodGet))
	if !strings.Contains(w.Body.String(), `"method_not_allowed"`) {
		t.Errorf("405 body = %s, want the error envelope", w.Body.String())
	}
	if bootstraps != 0 {
		t.Fatalf("a refused request provisioned the project %d times", bootstraps)
	}
}

// A handler method that is never routed still compiles, so assert the GPU-capacity-visible toggle
// route is actually wired (the class of bug where a handler ships completely unreachable).
func TestProjectGPUCapacityVisibleRoute(t *testing.T) {
	router := chi.NewRouter()
	(&Handler{}).Routes(router)
	if !router.Match(chi.NewRouteContext(), http.MethodPut, "/admin/project/project-1/gpu-capacity-visible") {
		t.Fatal("PUT /admin/project/{id}/gpu-capacity-visible route is not registered")
	}
}

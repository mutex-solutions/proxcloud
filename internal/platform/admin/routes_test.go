package admin

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
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

// A handler method that is never routed still compiles, so assert the GPU-capacity-visible toggle
// route is actually wired (the class of bug where a handler ships completely unreachable).
func TestProjectGPUCapacityVisibleRoute(t *testing.T) {
	router := chi.NewRouter()
	(&Handler{}).Routes(router)
	if !router.Match(chi.NewRouteContext(), http.MethodPut, "/admin/project/project-1/gpu-capacity-visible") {
		t.Fatal("PUT /admin/project/{id}/gpu-capacity-visible route is not registered")
	}
}

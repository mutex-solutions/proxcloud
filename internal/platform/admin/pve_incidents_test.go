package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func TestPVEIncidentsUnmountedReadOnlyAndTenantDenied(t *testing.T) {
	h := &Handler{adminIssuer: "operator-realm", adminClientID: "operator-app"}
	defaultRouter := chi.NewRouter()
	h.Routes(defaultRouter)
	path := "/admin/service/provider1/incidents/"
	if defaultRouter.Match(chi.NewRouteContext(), http.MethodGet, path) {
		t.Fatal("collection exposed before inventory integration")
	}
	router := chi.NewRouter()
	router.Mount("/admin/service/{id}/incidents", h.PVEIncidentRoutes(nil))
	for _, suffix := range []string{"/", "/incident-id/affected?projectId=projectA&limit=1"} {
		for _, rc := range []*httpx.RequestContext{
			{Sub: "tenant", Issuer: "tenant-realm", Azp: "tenant-app"},
			{Sub: "wrong-app", Issuer: "operator-realm", Azp: "tenant-app"},
		} {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/admin/service/provider1/incidents"+suffix, nil)
			r = r.WithContext(httpx.WithRC(r.Context(), rc))
			router.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatal("RBAC must deny before nil reader/existence lookup", w.Code, w.Body.String())
			}
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/admin/service/provider1/incidents"+suffix, nil)
		r = r.WithContext(httpx.WithRC(r.Context(), &httpx.RequestContext{Issuer: "operator-realm", Azp: "operator-app"}))
		router.ServeHTTP(w, r)
		if w.Code != 503 {
			t.Fatal("unwired read should fail closed", w.Code, w.Body.String())
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		for _, p := range []string{path, path + "incident-id/affected"} {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(method, p, nil))
			if w.Code != http.StatusMethodNotAllowed && w.Code != http.StatusNotFound {
				t.Fatal("mutation incident route", method, p, w.Code)
			}
		}
	}
}

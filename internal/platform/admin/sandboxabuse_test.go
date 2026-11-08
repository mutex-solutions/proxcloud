package admin

import (
	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSandboxSafetyRoutesFailClosedWithoutAdmin(t *testing.T) {
	h := &Handler{adminIssuer: "operator-realm"}
	r := chi.NewRouter()
	h.routeSandboxProvider(r)
	for _, path := range []string{"/sandbox/abuse?projectId=other", "/service/other/sandbox/rotation-plan"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "tenant", Issuer: "tenant-realm"}))
		r.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatalf("%s = %d %s", path, w.Code, w.Body.String())
		}
	}
}

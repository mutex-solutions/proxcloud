package admin

import (
	"context"
	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUnsupportedAutomaticActionsRejectedEvenWhileDisabled(t *testing.T) {
	for _, action := range []string{"pause", "kill"} {
		doc := pgdoc.M{"_id": "sbx", "status": "DISABLED", "config": map[string]any{"provider": "sandbox", "sandbox": map[string]any{"envd": true, "abuse": map[string]any{"action": action}}}}
		err := (&Handler{}).checkSandboxGoLive(context.Background(), doc)
		if err == nil || err.Status != http.StatusBadRequest {
			t.Fatalf("%s: %v", action, err)
		}
	}
}

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

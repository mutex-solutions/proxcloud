package admin

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Every bulk attach route is admin-only (a tenant gets 403 and nothing runs); with the service unwired an operator gets
// 503. Reading needs admin:service:read, changing needs admin:service:manage.
func TestBulkAttachRouteGates(t *testing.T) {
	h := &Handler{adminIssuer: "operator-realm"}
	r := chi.NewRouter()
	r.Route("/admin", func(r chi.Router) { h.routeProviderAttach(r); h.routeExternalServiceMut(r) })
	routes := []struct{ method, path string }{
		{"GET", "/admin/service/s/bulk-attach"}, {"GET", "/admin/service/s/bulk-attach/candidates"},
		{"POST", "/admin/service/s/bulk-attach/preview"}, {"POST", "/admin/service/s/bulk-attach"},
		{"GET", "/admin/service/s/bulk-attach/r"}, {"GET", "/admin/service/s/bulk-attach/r/items"},
		{"POST", "/admin/service/s/bulk-attach/r/cancel"}, {"POST", "/admin/service/s/bulk-attach/r/retry"},
		{"PUT", "/admin/service/s/attach-policy"},
	}
	tenant := &httpx.RequestContext{Sub: "tenant", Issuer: "tenant-realm"}
	for _, rt := range routes {
		if w := tuDo(r, rt.method, rt.path, `{"scope":{"mode":"all"}}`, tenant); w.Code != http.StatusForbidden {
			t.Errorf("%s %s as tenant: %d", rt.method, rt.path, w.Code)
		}
	}
	op := &httpx.RequestContext{SigV4KeyID: "pk"}
	for _, rt := range routes[:8] {
		if w := tuDo(r, rt.method, rt.path, `{"scope":{"mode":"all"}}`, op); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s unwired: %d", rt.method, rt.path, w.Code)
		}
	}
	support, admin := rolePermissions("SUPPORT"), rolePermissions("ADMIN")
	if !rbac.Matches(support, bulkAttachReadPerm) || rbac.Matches(support, bulkAttachManagePerm) || !rbac.Matches(admin, bulkAttachManagePerm) {
		t.Fatal("SUPPORT reads but does not start a bulk attach; ADMIN starts one")
	}
	if rbac.Matches(rolePermissions("BILLING_ADMIN"), bulkAttachReadPerm) {
		t.Fatal("BILLING_ADMIN must not see bulk attach")
	}
}

// The preview changes nothing, so the audit middleware does not log it; start/cancel/retry are logged.
func TestBulkAttachPreviewNotAudited(t *testing.T) {
	if !auditSkip("/api/v1/admin/service/s/bulk-attach/preview") {
		t.Fatal("preview audited")
	}
	for _, p := range []string{"/api/v1/admin/service/s/bulk-attach", "/api/v1/admin/service/s/bulk-attach/r/cancel", "/api/v1/admin/service/s/bulk-attach/r/retry"} {
		if auditSkip(p) {
			t.Fatalf("%s skipped", p)
		}
	}
}

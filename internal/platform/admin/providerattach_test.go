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

// Start and retry need both admin:service:manage and admin:project:update; the picker and the preview need
// admin:service:read and admin:project:read. A grant that holds only one of the two keys is refused.
func TestBulkAttachNeedsBothKeys(t *testing.T) {
	gate := func(granted []string, perms ...string) bool { return missingPerm(granted, perms) == "" }
	serviceOnly := []string{"admin:service:*"}
	projectOnly := []string{"admin:project:*"}
	both := []string{"admin:service:*", "admin:project:*"}
	for _, perms := range [][]string{{bulkAttachManagePerm, projectUpdatePerm}, {bulkAttachReadPerm, projectReadPerm}} {
		if gate(serviceOnly, perms...) || gate(projectOnly, perms...) || !gate(both, perms...) {
			t.Fatalf("%v: one key must not be enough", perms)
		}
	}
	// The built-in roles: ADMIN holds both; SUPPORT and VIEWER read both, start nothing.
	if !gate(rolePermissions("ADMIN"), bulkAttachManagePerm, projectUpdatePerm) {
		t.Fatal("ADMIN must start a bulk attach")
	}
	for _, role := range []string{"SUPPORT", "VIEWER"} {
		if gate(rolePermissions(role), bulkAttachManagePerm, projectUpdatePerm) || !gate(rolePermissions(role), bulkAttachReadPerm, projectReadPerm) {
			t.Fatalf("%s reads but does not start", role)
		}
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

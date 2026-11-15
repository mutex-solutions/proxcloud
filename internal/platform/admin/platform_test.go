package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platformlife"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// GET /admin/platform/bom needs admin:service:read and answers the embedded BOM with its sha256. It goes through
// Routes, so the test also fails when Routes stops registering the route.
func TestPlatformBOMRoute(t *testing.T) {
	h := &Handler{adminIssuer: "operator-realm"}
	r := chi.NewRouter()
	h.Routes(r)

	if w := tuDo(r, "GET", "/admin/platform/bom", "", &httpx.RequestContext{Sub: "tenant", Issuer: "tenant-realm"}); w.Code != http.StatusForbidden {
		t.Fatalf("tenant: %d", w.Code)
	}
	w := tuDo(r, "GET", "/admin/platform/bom", "", &httpx.RequestContext{SigV4KeyID: "pk-test"})
	if w.Code != http.StatusOK {
		t.Fatalf("operator: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		Data struct {
			SHA256     string `json:"sha256"`
			Release    string `json:"release"`
			Components []struct {
				Name, Cluster, Group string
				Chart                struct{ Version, Digest string }
			} `json:"components"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	cur, err := platformlife.Current()
	if err != nil {
		t.Fatal(err)
	}
	if got.Data.SHA256 != cur.SHA256 || got.Data.Release != cur.Release || len(got.Data.Components) != len(cur.Components) {
		t.Fatalf("answer %+v, embedded %s %s with %d components", got.Data, cur.SHA256, cur.Release, len(cur.Components))
	}
	if c := got.Data.Components[0]; c.Name == "" || c.Cluster == "" || c.Chart.Version == "" || c.Chart.Digest == "" {
		t.Errorf("component without name, cluster, version or digest: %+v", c)
	}
}

// platformRoutes are the installation routes with the key each needs.
var platformRoutes = []struct{ method, path, perm string }{
	{"GET", "/admin/platform/installations", serviceReadPerm},
	{"POST", "/admin/platform/installations", externalServiceManagePerm},
	{"GET", "/admin/platform/installations/pi-1", serviceReadPerm},
	{"PUT", "/admin/platform/installations/pi-1", externalServiceManagePerm},
	{"PUT", "/admin/platform/installations/pi-1/credentials", externalServiceManagePerm},
	{"POST", "/admin/platform/installations/pi-1/preflight", externalServiceManagePerm},
	{"GET", "/admin/platform/installations/pi-1/plan", serviceReadPerm},
}

// Every installation route is admin-only (a tenant gets 403). With PROXCLOUD_PLATFORM_ENABLED off (no store wired) an
// operator gets 404 platform_disabled, and nothing is read or decoded. Reads need admin:service:read, changes
// admin:service:manage: SUPPORT reads and changes nothing, ADMIN does both (the roles themselves are exercised in
// test/integration TestAdminPlatformInstallations).
func TestPlatformRouteGates(t *testing.T) {
	h := &Handler{adminIssuer: "operator-realm"}
	r := chi.NewRouter()
	h.Routes(r)
	tenant := &httpx.RequestContext{Sub: "tenant", Issuer: "tenant-realm"}
	op := &httpx.RequestContext{SigV4KeyID: "pk"}
	for _, rt := range platformRoutes {
		if w := tuDo(r, rt.method, rt.path, `{}`, tenant); w.Code != http.StatusForbidden {
			t.Errorf("%s %s as tenant: %d", rt.method, rt.path, w.Code)
		}
		w := tuDo(r, rt.method, rt.path, `not json`, op)
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), platformlife.CodePlatformDisabled) {
			t.Errorf("%s %s with the flag off: %d %s", rt.method, rt.path, w.Code, w.Body.String())
		}
		support, admin := rolePermissions("SUPPORT"), rolePermissions("ADMIN")
		if !rbac.Matches(admin, rt.perm) || rbac.Matches(support, rt.perm) != (rt.perm == serviceReadPerm) {
			t.Errorf("%s %s needs %s: SUPPORT reads only, ADMIN all", rt.method, rt.path, rt.perm)
		}
	}
	// The flag gates only the installations: the BOM stays readable.
	if w := tuDo(r, "GET", "/admin/platform/bom", "", op); w.Code != http.StatusOK {
		t.Fatalf("bom with the flag off: %d", w.Code)
	}
}

// The preflight changes nothing and is not audited; every other installation write is.
func TestPlatformAuditSkip(t *testing.T) {
	if !auditSkip("/api/v1/admin/platform/installations/pi-1/preflight") {
		t.Fatal("preflight audited")
	}
	for _, p := range []string{"/api/v1/admin/platform/installations", "/api/v1/admin/platform/installations/pi-1",
		"/api/v1/admin/platform/installations/pi-1/credentials"} {
		if auditSkip(p) || pathResourceType(p) != "PLATFORM_INSTALLATION" {
			t.Fatalf("%s: skipped or typed %s", p, pathResourceType(p))
		}
	}
}

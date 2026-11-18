package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
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
	{"POST", "/admin/platform/installations/pi-1/runs", externalServiceManagePerm},
	{"GET", "/admin/platform/installations/pi-1/runs", serviceReadPerm},
	{"GET", "/admin/platform/installations/pi-1/runs/pr-1", serviceReadPerm},
	{"POST", "/admin/platform/installations/pi-1/runs/pr-1/cancel", externalServiceManagePerm},
	{"GET", "/admin/platform/installations/pi-1/seed-kubeconfig", externalServiceManagePerm},     // a GET that hands out cluster-admin
	{"GET", "/admin/platform/installations/pi-1/platform-kubeconfig", externalServiceManagePerm}, // a GET that opens the platform's ArgoCD
	{"POST", "/admin/platform/installations/pi-1/seed-credential/rotate", externalServiceManagePerm},
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

// The audit entry of a break-glass download says who, which installation, which of the two kubeconfigs and until
// when, and has no place for the token: it is built without it.
func TestBreakGlassEventHasNoToken(t *testing.T) {
	for _, op := range []string{"seed-kubeconfig", "platform-kubeconfig"} {
		r := httptest.NewRequest("GET", "/admin/platform/installations/pi-1/"+op, nil)
		r = r.WithContext(httpx.WithRC(r.Context(), &httpx.RequestContext{Sub: "operator-1"}))
		expires := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
		ev := breakGlassEvent(r, &platformlife.Installation{ID: "pi-1", Name: "lab"}, op, expires)
		raw, _ := json.Marshal(ev)
		for _, want := range []string{"operator-1", "pi-1", "2026-11-16T10:00:00Z", ActionBreakGlass, `"operation":"` + op + `"`} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("no %s in %s", want, raw)
			}
		}
		if len(ev.ResourceMetadata) != 2 || len(ev.Changes) != 0 || strings.Contains(strings.ToLower(string(raw)), "kubeconfig\":") ||
			strings.Contains(string(raw), "eyJ") {
			t.Fatalf("the entry carries more than actor, operation and expiry: %s", raw)
		}
	}
}

// The seed credential rotation writes its own entry, ROTATE_CREDENTIAL with the two Secret names and nothing else, and
// the audit middleware leaves the route alone: its row read CREATE on the installation.
func TestSeedRotateEventNamesTheSecrets(t *testing.T) {
	path := "/api/v1/admin/platform/installations/pi-1/seed-credential/rotate"
	if !auditSkip(path) {
		t.Fatal("the middleware would add a CREATE row")
	}
	r := httptest.NewRequest("POST", path, nil)
	r = r.WithContext(httpx.WithRC(r.Context(), &httpx.RequestContext{Sub: "operator-1"}))
	ev := seedRotateEvent(r, &platformlife.Installation{ID: "pi-1", Name: "lab"}, "proxcloud-portal-token-g1", "proxcloud-portal-token-g2")
	raw, _ := json.Marshal(ev)
	for _, want := range []string{"operator-1", "pi-1", ActionRotateCredential, `"fromSecret":"proxcloud-portal-token-g1"`, `"toSecret":"proxcloud-portal-token-g2"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("no %s in %s", want, raw)
		}
	}
	if ev.Action != "ROTATE_CREDENTIAL" || ev.ResourceType != "PLATFORM_INSTALLATION" || len(ev.ResourceMetadata) != 3 || len(ev.Changes) != 0 ||
		strings.Contains(string(raw), "eyJ") {
		t.Fatalf("the entry is not the rotation's own, or carries more than the names: %s", raw)
	}
}

// A rotation that answered 502 after the new credential was stored is audited too, as a failure with both names: the
// stored cluster-admin credential changed and older token Secrets may still work. One that stored nothing writes none.
func TestSeedRotateUnfinishedEvent(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/v1/admin/platform/installations/pi-1/seed-credential/rotate", nil)
	r = r.WithContext(httpx.WithRC(r.Context(), &httpx.RequestContext{Sub: "operator-1"}))
	inst := func(secret string) *platformlife.Installation {
		return &platformlife.Installation{ID: "pi-1", Name: "lab", Status: platformlife.Status{Seed: &platformlife.SeedStatus{TokenSecret: secret}}}
	}
	if _, ok := seedRotateUnfinishedEvent(r, inst("proxcloud-portal-token-g1"), inst("proxcloud-portal-token-g1")); ok {
		t.Fatal("nothing was stored: no entry")
	}
	ev, ok := seedRotateUnfinishedEvent(r, inst("proxcloud-portal-token-g1"), inst("proxcloud-portal-token-g2"))
	raw, _ := json.Marshal(ev)
	if !ok || ev.Action != ActionRotateCredential || ev.Outcome != audit.OutcomeFailure || ev.ErrorMessage == "" ||
		ev.ResourceMetadata["fromSecret"] != "proxcloud-portal-token-g1" || ev.ResourceMetadata["toSecret"] != "proxcloud-portal-token-g2" || strings.Contains(string(raw), "eyJ") {
		t.Fatalf("the entry of an unfinished rotation: ok=%v %s", ok, raw)
	}
}

// Without an audit service the break-glass routes issue nothing: they answer 503 before they read the installation or
// ask a cluster for a token (the store here has no database, so a read would panic), and the answer is not cacheable.
func TestKubeconfigDownloadsNeedTheAuditLog(t *testing.T) {
	h := &Handler{adminIssuer: "operator-realm"}
	h.SetPlatform(platformlife.NewStore(nil, nil))
	r := chi.NewRouter()
	h.Routes(r)
	for _, op := range []string{"seed-kubeconfig", "platform-kubeconfig"} {
		w := tuDo(r, "GET", "/admin/platform/installations/pi-1/"+op, "", &httpx.RequestContext{SigV4KeyID: "pk-test"})
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "kubeconfig\":") {
			t.Fatalf("%s: %d %q %s", op, w.Code, w.Header().Get("Cache-Control"), w.Body.String())
		}
	}
}

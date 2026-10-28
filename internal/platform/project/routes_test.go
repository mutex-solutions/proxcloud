package project

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
)

// The S3 credential/key routes are gated on project:cloud_resource:api_access, NOT :manage (finding F2:
// handing out or rotating secret keys is API-credential management). This guards the permission LADDER
// that decision relies on: an exact manage grant must not imply api_access, while the wildcard roles must.
func TestS3CredentialPermissionLadder(t *testing.T) {
	// A custom role scoped to exactly `manage` must NOT satisfy the credential gate.
	if rbac.Matches([]string{rbac.ProjectCloudResourceManage}, rbac.ProjectCloudResourceAPIAcc) {
		t.Error("exact project:cloud_resource:manage must not grant api_access")
	}
	// The wildcard (what static MEMBER/OWNER hold) must satisfy it, so normal users are unaffected.
	if !rbac.Matches([]string{"project:cloud_resource:*"}, rbac.ProjectCloudResourceAPIAcc) {
		t.Error("project:cloud_resource:* should grant api_access")
	}
	if !rbac.RoleHasPermission(rbac.RoleMember, rbac.ProjectCloudResourceAPIAcc) {
		t.Error("static MEMBER should keep api_access (it holds project:cloud_resource:*)")
	}
	// api_access itself obviously satisfies the gate.
	if !rbac.Matches([]string{rbac.ProjectCloudResourceAPIAcc}, rbac.ProjectCloudResourceAPIAcc) {
		t.Error("exact api_access should grant api_access")
	}
}

// Routes must register without chi pattern panics (params share tree nodes) and the 2026-07-02
// gap-scan additions must match their expected paths.
func TestRoutes_registerAndMatch(t *testing.T) {
	r := chi.NewRouter()
	(&Handler{}).Routes(r)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/project/p1/service/svc1"},
		{http.MethodPost, "/project/p1/billing/bp1"},
		// neighbours that must keep winning over the new params:
		{http.MethodGet, "/project/p1/service/CLOUD/location"},
		{http.MethodGet, "/project/p1/billing"},
		{http.MethodGet, "/project/p1/quota-usage"},
		{http.MethodGet, "/project/p1/gpu-capacity"},
	} {
		rctx := chi.NewRouteContext()
		if !r.Match(rctx, tc.method, tc.path) {
			t.Errorf("no route for %s %s", tc.method, tc.path)
		}
	}
}

// A handler method that is never routed still COMPILES, so `go build` cannot catch a missing r.Get/r.Post
// line — which is exactly how the ceph-s3 S3-key surface first shipped completely unreachable (every
// endpoint 404 while the handlers sat there compiling happily). Assert the routes exist.
func TestRoutes_s3KeySurfaceIsReachable(t *testing.T) {
	r := chi.NewRouter()
	(&Handler{}).Routes(r)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/project/p1/s3-credentials"},
		{http.MethodPost, "/project/p1/s3-credentials/rotate"},
		{http.MethodGet, "/project/p1/s3-keys"},
		{http.MethodPost, "/project/p1/s3-keys"},
		{http.MethodPost, "/project/p1/s3-keys/k1/rotate"},
		{http.MethodDelete, "/project/p1/s3-keys/k1"},
	} {
		rctx := chi.NewRouteContext()
		if !r.Match(rctx, tc.method, tc.path) {
			t.Errorf("no route for %s %s", tc.method, tc.path)
		}
	}
	// Negative control: r.Match must be able to say NO, otherwise the assertions above are vacuous.
	if r.Match(chi.NewRouteContext(), http.MethodGet, "/project/p1/s3-keys-does-not-exist") {
		t.Fatal("router matches a route that does not exist — the assertions above prove nothing")
	}
}

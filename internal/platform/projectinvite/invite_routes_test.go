package projectinvite

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// denyAuthz refuses every project permission check (a non-member caller).
type denyAuthz struct{}

func (denyAuthz) RequireProjectPermission(context.Context, string, *project.Project, string) error {
	return httpx.Forbidden("nope")
}

// allowAuthz grants every project permission check.
type allowAuthz struct{}

func (allowAuthz) RequireProjectPermission(context.Context, string, *project.Project, string) error {
	return nil
}

// invite() must reject a caller lacking project:manage_members on the target project — otherwise
// any user could mint invites into an arbitrary project by id (finding [12]).
func TestRequireInvitePermission_deniedForNonMember(t *testing.T) {
	h := &Handler{}
	h.SetAuthorizer(denyAuthz{})
	if err := h.requireInvitePermission(context.Background(), "attacker", &project.Project{OrganizationID: "orgA"}); err == nil {
		t.Fatal("non-member caller must be denied project invite")
	}
	h.SetAuthorizer(allowAuthz{})
	if err := h.requireInvitePermission(context.Background(), "manager", &project.Project{OrganizationID: "orgA"}); err != nil {
		t.Fatalf("authorized caller must pass: %v", err)
	}
}

// The invite-create route (2026-07-02 gap scan) must register alongside the token routes and the
// static "invite"/"accept"/"decline" segments must win over {token}.
func TestRoutes_inviteCreate(t *testing.T) {
	r := chi.NewRouter()
	(&Handler{}).Routes(r)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/project-invites/invite"},
		{http.MethodGet, "/project-invites/tok1"},
		{http.MethodPost, "/project-invites/accept/tok1"},
		{http.MethodPost, "/project-invites/decline/tok1"},
	} {
		rctx := chi.NewRouteContext()
		if !r.Match(rctx, tc.method, tc.path) {
			t.Errorf("no route for %s %s", tc.method, tc.path)
		}
	}
}

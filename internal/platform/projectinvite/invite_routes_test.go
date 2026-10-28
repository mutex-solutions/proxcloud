package projectinvite

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
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

// TestCallerEmail pins the invite match rule: only an IdP-verified email matches
// invites; blank (which ByEmail/ByEmailAndToken treat as "no match") otherwise.
func TestCallerEmail(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		u    user.User
		want string
	}{
		{"verified", user.User{Email: "a@x.io", EmailConfirmedAt: &now}, "a@x.io"},
		{"unverified", user.User{Email: "a@x.io"}, ""},
		{"no email", user.User{EmailConfirmedAt: &now}, ""},
	}
	for _, c := range cases {
		if got := callerEmail(&c.u); got != c.want {
			t.Errorf("%s: callerEmail = %q, want %q", c.name, got, c.want)
		}
	}
	r := &Repo{} // a blank email returns before touching the store
	if inv, err := r.ByEmailAndToken(context.Background(), "", "tok"); inv != nil || err != nil {
		t.Errorf("ByEmailAndToken(\"\") = %v, %v; want nil, nil", inv, err)
	}
	if invs, err := r.ByEmail(context.Background(), ""); invs != nil || err != nil {
		t.Errorf("ByEmail(\"\") = %v, %v; want nil, nil", invs, err)
	}
}

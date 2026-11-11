//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func TestMemberRoleAssignmentRequiresRoleManagement(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := org.NewRepo(db)
	if err := repo.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	o, err := repo.Insert(ctx, &org.Organization{Name: "role-assignment-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	roles := org.NewRoleService(repo)
	for name, permissions := range map[string][]string{
		"MEMBERS_ONLY": {rbac.OrganizationManageMembers},
		"POWER":        {rbac.OrganizationManageRoles},
		"READ_ONLY":    {rbac.OrganizationRead},
		"WILDCARD":     {"*"},
	} {
		if _, err := roles.Create(ctx, o.ID, name, "fixture", permissions); err != nil {
			t.Fatal(err)
		}
	}
	for sub, role := range map[string]string{"manager": "MEMBERS_ONLY", "owner": rbac.RoleOwner} {
		mustInsert(t, db, "users", user.User{Sub: sub})
		if _, err := repo.AddMember(ctx, o.ID, sub, role); err != nil {
			t.Fatal(err)
		}
	}
	policy := org.NewPolicy(repo)
	if policy.HasPermission(ctx, "manager", o.ID, rbac.OrganizationManageRoles) {
		t.Fatal("fixture manager unexpectedly has role-management permission")
	}
	as := audit.NewService(audit.NewRepo(db), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := org.NewHandler(org.NewService(repo, billing.NewRepo(db), nil), policy, repo, user.NewRepo(db), as)
	router := chi.NewRouter()
	h.Routes(router)
	request := func(sub, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	path := "/organizations/" + o.ID + "/member"
	successes := int64(0)
	for i, tc := range []struct {
		actor, role string
		want        int
	}{
		{"manager", rbac.RoleOwner, 403},
		{"manager", rbac.RoleAdmin, 403},
		{"manager", "POWER", 403},
		{"manager", "WILDCARD", 403},
		{"manager", "READ_ONLY", 403}, // every custom-role assignment is a role-management action
		{"manager", rbac.RoleMember, 200},
		{"owner", "POWER", 200},
		{"owner", "READ_ONLY", 200},
		{"owner", "UNKNOWN_ROLE", 400},
		{"owner", "", 400},
	} {
		t.Run(fmt.Sprintf("%s-%s-%d", tc.actor, tc.role, i), func(t *testing.T) {
			sub := fmt.Sprintf("target-%d", i)
			email := sub + "@example.invalid"
			now := time.Now().UTC()
			mustInsert(t, db, "users", user.User{Sub: sub, Email: email, EmailConfirmedAt: &now})
			body := fmt.Sprintf(`{"email":%q,"role":%q,"projectIds":[]}`, email, tc.role)
			w := request(tc.actor, http.MethodPost, path, body)
			if w.Code != tc.want {
				t.Fatalf("got %d %s, want %d", w.Code, w.Body.String(), tc.want)
			}
			m, err := repo.FindMember(ctx, o.ID, sub)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want != 200 {
				if m != nil {
					t.Fatal("denied assignment persisted a membership")
				}
				return
			}
			successes++
			if m == nil || m.Role() != tc.role {
				t.Fatalf("successful assignment missing or wrong role: %#v", m)
			}
			if tc.role == "POWER" && !policy.HasPermission(ctx, sub, o.ID, rbac.OrganizationManageRoles) {
				t.Fatal("authorized custom role no longer resolves its real permission")
			}
		})
	}
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		p := path
		if method == http.MethodPut {
			p += "/target-6/role"
		}
		w := request("owner", method, p, "{")
		if w.Code != 400 {
			t.Fatalf("malformed %s: got %d %s", method, w.Code, w.Body.String())
		}
	}
	w := request("owner", http.MethodPut, path+"/target-6/role", `{"role":"UNKNOWN_ROLE"}`)
	if w.Code != 400 {
		t.Fatalf("unknown role update: got %d %s", w.Code, w.Body.String())
	}
	m, err := repo.FindMember(ctx, o.ID, "target-6")
	if err != nil || m == nil || m.Role() != "POWER" {
		t.Fatalf("refused role update changed membership: %#v %v", m, err)
	}
	// Wait for the production async audit writes before freshPG closes its database.
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		count, err := as.Count(ctx, audit.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if count == successes {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("audit writes incomplete: %d, want %d", count, successes)
		case <-tick.C:
		}
	}
}

//go:build integration

package integration

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// TestMemberCannotLiftSuspension drives a project owner's three routes back to ENABLED through the real handler: cancel,
// a billing profile change and opening the project. None may lift DISABLED, and none may touch a project in teardown.
func TestMemberCannotLiftSuspension(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := project.NewRepo(db)
	orgRepo := org.NewRepo(db)
	billingRepo := billing.NewRepo(db)
	users := user.NewRepo(db)
	svc := project.NewService(repo, orgRepo, billingRepo, users, nil)
	orgSvc := org.NewService(orgRepo, billingRepo, nil)
	h := project.NewHandler(svc, project.NewPolicy(org.NewPolicy(orgRepo)), orgSvc, users, audit.NewService(audit.NewRepo(db), slog.Default()), billingRepo, cloud.NewRepo(db), nil, nil, nil, "")
	router := chi.NewRouter()
	h.Routes(router)

	const sub = "owner-sub"
	for coll, doc := range map[string]pgdoc.M{
		"organization":         {"_id": "o1", "name": "o", "billingProfileId": "bp-cur"},
		"organization_members": {"organizationId": "o1", "sub": sub, "roles": []string{"OWNER"}},
		"users":                {"sub": sub, "email": "o@example.org"},
		"billingConfiguration": {"_id": "default", "baseCurrency": "USD"},
	} {
		if _, err := db.C(coll).InsertOne(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	for id, st := range map[string]string{"bp-cur": billing.StatusActive, "bp-active": billing.StatusActive, "bp-new": billing.StatusNew} {
		if _, err := db.C("billingProfile").InsertOne(ctx, pgdoc.M{"_id": id, "organizationId": "o1", "status": st}); err != nil {
			t.Fatal(err)
		}
	}
	seed := func(status string) {
		_, _ = db.C("project").DeleteMany(ctx, pgdoc.M{})
		if _, err := db.C("project").InsertOne(ctx, pgdoc.M{
			"_id": "p1", "name": "p", "status": status, "organizationId": "o1", "billingProfileId": "bp-cur",
			"memberships": []pgdoc.M{{"sub": sub, "role": project.RoleOwner}}, "services": []any{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	do := func(method, path string) int {
		req := httptest.NewRequest(method, path, nil)
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}
	status := func() string {
		p, err := repo.FindByID(ctx, "p1")
		if err != nil || p == nil {
			t.Fatalf("read back: %v %v", p, err)
		}
		return p.Status
	}

	cases := []struct {
		name, method, path, from string
		code                     int
		want                     string
	}{
		{"cancel on DISABLED", http.MethodDelete, "/project/p1/cancel", project.StatusDisabled, 400, project.StatusDisabled},
		{"cancel on ENABLED", http.MethodDelete, "/project/p1/cancel", project.StatusEnabled, 400, project.StatusEnabled},
		{"attach an ACTIVE profile on DISABLED", http.MethodPost, "/project/p1/billing/bp-active", project.StatusDisabled, 200, project.StatusDisabled},
		{"attach an ACTIVE profile on ENABLED", http.MethodPost, "/project/p1/billing/bp-active", project.StatusEnabled, 200, project.StatusEnabled},
		{"attach a NEW profile disables", http.MethodPost, "/project/p1/billing/bp-new", project.StatusEnabled, 200, project.StatusDisabled},
		{"attach while scheduled", http.MethodPost, "/project/p1/billing/bp-active", project.StatusScheduledForDeletion, 400, project.StatusScheduledForDeletion},
		{"attach mid teardown", http.MethodPost, "/project/p1/billing/bp-active", project.StatusDeleteInProgress, 400, project.StatusDeleteInProgress},
		{"attach on DELETED", http.MethodPost, "/project/p1/billing/bp-active", project.StatusDeleted, 400, project.StatusDeleted},
		// An open that cannot bring the project up says so (#216): 409, and the status stays.
		{"open DISABLED", http.MethodPost, "/project/p1/init", project.StatusDisabled, 409, project.StatusDisabled},
		{"open mid teardown", http.MethodPost, "/project/p1/init", project.StatusDeleteInProgress, 409, project.StatusDeleteInProgress},
		{"schedule mid teardown", http.MethodDelete, "/project/p1", project.StatusDeleteInProgress, 400, project.StatusDeleteInProgress},
		{"schedule on DELETED", http.MethodDelete, "/project/p1", project.StatusDeleted, 400, project.StatusDeleted},
		{"delete now on DELETED", http.MethodDelete, "/project/p1/now", project.StatusDeleted, 400, project.StatusDeleted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seed(c.from)
			if code := do(c.method, c.path); code != c.code {
				t.Errorf("status %d, want %d", code, c.code)
			}
			if got := status(); got != c.want {
				t.Errorf("stored %s, want %s", got, c.want)
			}
		})
	}

	t.Run("schedule then cancel keeps DISABLED", func(t *testing.T) {
		seed(project.StatusDisabled)
		if code := do(http.MethodDelete, "/project/p1"); code != 200 {
			t.Fatalf("schedule: %d", code)
		}
		if got := status(); got != project.StatusScheduledForDeletion {
			t.Fatalf("after schedule: %s", got)
		}
		if code := do(http.MethodDelete, "/project/p1/cancel"); code != 200 {
			t.Fatalf("cancel: %d", code)
		}
		if got := status(); got != project.StatusDisabled {
			t.Fatalf("after cancel: %s", got)
		}
	})
}

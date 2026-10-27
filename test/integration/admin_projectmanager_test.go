//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// TestAdminProjectManagerLegs drives the three /admin/projects/manage legs end-to-end against the
// throwaway Postgres: add-member (persist + 200, NOT the old state-leaking 501), remove-member (200 +
// owner-guard 400), and invite (dispatch through the wired inviteToProject leg). It also establishes
// the admin-Handler HTTP harness (REMOTE_OIDC principal → auto SUPER_ADMIN) for future waves.
func TestAdminProjectManagerLegs(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)

	if _, err := db.C("users").InsertOne(ctx, pgdoc.M{"_id": "user-1", "sub": "user-sub", "email": "u@x.io"}); err != nil {
		t.Fatal(err)
	}
	// The owner is a real user too — remove-member resolves the user by sub BEFORE the owner-role
	// guard (a missing user → 404), so seed the owner's user doc; otherwise the owner-remove below
	// 404s on the missing user instead of hitting the 400 owner guard.
	if _, err := db.C("users").InsertOne(ctx, pgdoc.M{"_id": "owner-1", "sub": "owner-sub", "email": "o@x.io"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.C("project").InsertOne(ctx, pgdoc.M{
		"_id":         "proj-1",
		"status":      "ENABLED",
		"memberships": []any{pgdoc.M{"sub": "owner-sub", "role": "OWNER"}},
	}); err != nil {
		t.Fatal(err)
	}

	const iss, cid = "test-iss", "test-cid"
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, nil, nil, "", iss, cid)
	var invited []string
	h.SetInviteToProject(func(_ context.Context, _ *user.User, email, _ string) error {
		invited = append(invited, email)
		return nil
	})
	r := chi.NewRouter()
	h.Routes(r)

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub", Issuer: iss, Azp: cid}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	members := func() []string {
		var p struct {
			Memberships []struct {
				Sub string `json:"sub"`
			} `json:"memberships"`
		}
		_, _ = db.C("project").FindOne(ctx, pgdoc.M{"_id": "proj-1"}, &p)
		out := []string{}
		for _, m := range p.Memberships {
			out = append(out, m.Sub)
		}
		return out
	}

	// add: 200 + membership persisted (the old behavior 501'd AFTER persisting — a state leak).
	if w := do(http.MethodPost, "/admin/projects/manage", `{"userId":"user-1","projectId":"proj-1","role":"MEMBER"}`); w.Code != 200 {
		t.Fatalf("add: status=%d body=%s", w.Code, w.Body)
	}
	if got := members(); len(got) != 2 {
		t.Fatalf("after add want 2 members, got %v", got)
	}
	// add again → 400 already-added
	if w := do(http.MethodPost, "/admin/projects/manage", `{"userId":"user-1","projectId":"proj-1","role":"MEMBER"}`); w.Code != 400 {
		t.Fatalf("re-add want 400, got %d", w.Code)
	}
	// remove owner → 400
	if w := do(http.MethodPost, "/admin/projects/manage/remove", `{"projectId":"proj-1","sub":"owner-sub"}`); w.Code != 400 {
		t.Fatalf("owner remove want 400, got %d", w.Code)
	}
	// remove the added member → 200, membership gone
	if w := do(http.MethodPost, "/admin/projects/manage/remove", `{"projectId":"proj-1","sub":"user-sub"}`); w.Code != 200 {
		t.Fatalf("remove: status=%d body=%s", w.Code, w.Body)
	}
	if got := members(); len(got) != 1 || got[0] != "owner-sub" {
		t.Fatalf("after remove want [owner-sub], got %v", got)
	}
	// invite (newUser → email addresses) → 200 + the wired leg fired per email
	if w := do(http.MethodPost, "/admin/projects/manage/invite", `{"projectId":"proj-1","newUser":true,"userIds":["a@x.io","b@x.io"]}`); w.Code != 200 {
		t.Fatalf("invite: status=%d body=%s", w.Code, w.Body)
	}
	if len(invited) != 2 || invited[0] != "a@x.io" {
		t.Fatalf("invite leg should fire per email, got %v", invited)
	}
}

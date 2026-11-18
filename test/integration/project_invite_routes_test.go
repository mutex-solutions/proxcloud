//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/projectinvite"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// inviteRouteMail is a notifier with or without a mail gateway behind it.
type inviteRouteMail struct {
	configured bool
	sent       int
}

func (m *inviteRouteMail) SendTemplate(context.Context, string, []string, map[string]any) error {
	m.sent++
	return nil
}
func (m *inviteRouteMail) Configured(context.Context) bool { return m.configured }

type inviteRow struct {
	ID             string `json:"id"`
	Email          string `json:"email"`
	Mine           bool   `json:"mine"`
	MailConfigured bool   `json:"mailConfigured"`
	InviteURL      string `json:"inviteUrl"`
}

// D11 of the production acceptance: an invitation answered 202 and then could be neither seen nor taken back,
// and on a site without mail its link reached nobody. The inviter and a project admin list and revoke; a member
// sees only what they sent; a tenant of another project gets the 404 of a project that does not exist.
func TestProjectInviteListAndRevoke(t *testing.T) {
	db := freshPG(t)
	ctx := context.Background()
	orgs, projects, users := org.NewRepo(db), project.NewRepo(db), user.NewRepo(db)
	if err := db.C("users").Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	orgA, err := orgs.Insert(ctx, &org.Organization{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	orgB, err := orgs.Insert(ctx, &org.Organization{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	for sub, o := range map[string]string{"alice": orgA.ID, "bob": orgA.ID, "mallory": orgB.ID} {
		if _, err := orgs.AddMember(ctx, o, sub, rbac.RoleMember); err != nil {
			t.Fatal(err)
		}
	}
	// alice owns the project, bob is a plain member; mallory owns a project of another organization.
	p, err := projects.Insert(ctx, &project.Project{Name: "p", Status: project.StatusEnabled, OrganizationID: orgA.ID,
		Memberships: []project.Membership{{Sub: "alice", Role: project.RoleOwner}, {Sub: "bob", Role: project.RoleMember}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := projects.Insert(ctx, &project.Project{Name: "other", Status: project.StatusEnabled, OrganizationID: orgB.ID,
		Memberships: []project.Membership{{Sub: "mallory", Role: project.RoleOwner}}}); err != nil {
		t.Fatal(err)
	}
	repo := projectinvite.NewRepo(db)
	if err := repo.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	mail := &inviteRouteMail{}
	h := projectinvite.NewHandler(repo, users, project.NewService(projects, orgs, nil, users, nil), orgs, nil, mail, "https://console.example.invalid")
	router := chi.NewRouter()
	h.Routes(router)
	call := func(sub, method, path, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub, Email: sub + "@example.invalid", EmailVerified: true}))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if out != nil && rec.Code < 300 {
			var env struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("%s %s: %v: %s", method, path, err, rec.Body)
			}
			if err := json.Unmarshal(env.Data, out); err != nil {
				t.Fatalf("%s %s: %v: %s", method, path, err, rec.Body)
			}
		}
		return rec.Code
	}
	list := func(sub string) (rows []inviteRow, mailConfigured bool, code int) {
		t.Helper()
		var out struct {
			MailConfigured bool        `json:"mailConfigured"`
			Invites        []inviteRow `json:"invites"`
		}
		code = call(sub, http.MethodGet, "/project-invites/project/"+p.ID, "", &out)
		return out.Invites, out.MailConfigured, code
	}

	// No mail gateway: the answer carries the link, the only way it reaches the invitee.
	var created inviteRow
	if code := call("alice", http.MethodPost, "/project-invites/invite", `{"email":"new@example.invalid","projectId":"`+p.ID+`"}`, &created); code != http.StatusAccepted {
		t.Fatalf("invite = %d", code)
	}
	if created.ID == "" || created.MailConfigured || !created.Mine || !strings.HasPrefix(created.InviteURL, "https://console.example.invalid/join-project?invite-token=") {
		t.Fatalf("invite answer = %+v", created)
	}
	token := strings.TrimPrefix(created.InviteURL, "https://console.example.invalid/join-project?invite-token=")
	// bob sent one while he could; he is a plain member now.
	if _, err := repo.Create(ctx, pgdoc.M{"email": "bobs@example.invalid", "projectId": p.ID, "token": "tok-bob", "invitedBy": "bob",
		"expiresAt": time.Now().UTC().Add(time.Hour), "deliveryMode": "legacy"}); err != nil {
		t.Fatal(err)
	}

	// The project admin sees both.
	rows, mailConfigured, code := list("alice")
	if code != http.StatusOK || len(rows) != 2 || mailConfigured {
		t.Fatalf("admin list = %d %+v mail=%v", code, rows, mailConfigured)
	}
	// A plain member sees what he sent, and cannot revoke another's by its id.
	rows, _, code = list("bob")
	if code != http.StatusOK || len(rows) != 1 || rows[0].Email != "bobs@example.invalid" || !rows[0].Mine || rows[0].InviteURL == "" {
		t.Fatalf("member list = %d %+v", code, rows)
	}
	bobs := rows[0].ID
	if code := call("bob", http.MethodDelete, "/project-invites/project/"+p.ID+"/"+created.ID, "", nil); code != http.StatusNotFound {
		t.Fatalf("member revokes another's invite = %d, want 404", code)
	}
	// A tenant of another project: the project does not exist for them.
	if _, _, code := list("mallory"); code != http.StatusNotFound {
		t.Fatalf("other tenant list = %d, want 404", code)
	}
	if code := call("mallory", http.MethodDelete, "/project-invites/project/"+p.ID+"/"+created.ID, "", nil); code != http.StatusNotFound {
		t.Fatalf("other tenant revoke = %d, want 404", code)
	}
	if inv, err := repo.ByEmailAndToken(ctx, "new@example.invalid", token); err != nil || inv == nil {
		t.Fatalf("the invite must survive the refused revokes: %v %v", inv, err)
	}

	// The inviter revokes his own; the admin revokes hers. A revoked link is dead, and the other invite stays.
	if code := call("bob", http.MethodDelete, "/project-invites/project/"+p.ID+"/"+bobs, "", nil); code != http.StatusOK {
		t.Fatalf("inviter revoke = %d", code)
	}
	if rows, _, _ := list("alice"); len(rows) != 1 || rows[0].ID != created.ID {
		t.Fatalf("after the member's revoke = %+v", rows)
	}
	if code := call("alice", http.MethodDelete, "/project-invites/project/"+p.ID+"/"+created.ID, "", nil); code != http.StatusOK {
		t.Fatalf("admin revoke = %d", code)
	}
	if inv, err := repo.ByEmailAndToken(ctx, "new@example.invalid", token); err != nil || inv != nil {
		t.Fatalf("revoked invite still resolves: %v %v", inv, err)
	}
	if code := call("alice", http.MethodDelete, "/project-invites/project/"+p.ID+"/"+created.ID, "", nil); code != http.StatusNotFound {
		t.Fatalf("second revoke = %d, want 404", code)
	}

	// With a mail gateway the mail carries the link, so the API does not hand it out.
	mail.configured = true
	created = inviteRow{}
	if code := call("alice", http.MethodPost, "/project-invites/invite", `{"email":"mailed@example.invalid","projectId":"`+p.ID+`"}`, &created); code != http.StatusAccepted {
		t.Fatalf("invite = %d", code)
	}
	if !created.MailConfigured || created.InviteURL != "" || mail.sent != 2 {
		t.Fatalf("invite with mail = %+v sent=%d", created, mail.sent)
	}
	if rows, mailConfigured, _ := list("alice"); len(rows) != 1 || !mailConfigured || rows[0].InviteURL != "" {
		t.Fatalf("list with mail = %+v mail=%v", rows, mailConfigured)
	}
}

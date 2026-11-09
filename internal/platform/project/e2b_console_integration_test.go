//go:build integration

package project

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func consoleSessionFixture(t *testing.T) (*Handler, context.Context, chi.Router) {
	t.Helper()
	ctx := context.Background()
	pgc, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("console"), tcpostgres.WithUsername("u"), tcpostgres.WithPassword("p"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(pgc) })
	dsn, _ := pgc.ConnectionString(ctx, "sslmode=disable")
	db, err := pgdoc.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(ctx) })
	for _, name := range []string{"sandbox_templates", "project", "users", "externalService", "organization_members", "cloudResource", "cloudResourceArchive", "organization"} {
		if err := db.C(name).Ensure(ctx); err != nil {
			t.Fatal(err)
		}
	}
	users, projects, orgs := user.NewRepo(db), NewRepo(db), org.NewRepo(db)
	h := &Handler{sbTemplates: db.C("sandbox_templates"), users: users, svc: NewService(projects, orgs, nil, users, nil), policy: NewPolicy(org.NewPolicy(orgs)), cloud: cloud.NewRepo(db), esSvc: externalservice.NewService(externalservice.NewRepo(db), textcrypt.New(""))}
	api := &portImportAPI{pods: map[string]map[string]any{}}
	cfg := sandbox.Config{Envd: true, Domain: "sandbox.example.org", TemplateRegistry: "registry.example.org/t", Templates: map[string]sandbox.Template{"base": {Image: "alpine:3"}}}
	h.sandboxFor = func(*externalservice.ExternalService) (*sandbox.Service, error) {
		return sandbox.NewWithAPI(api, cfg, "svc"), nil
	}
	if err := db.C("externalService").Upsert(ctx, "svc", &externalservice.ExternalService{ID: "svc", Type: "CLOUD", Config: map[string]any{"provider": "sandbox"}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"p", "foreign"} {
		p := &Project{ID: id, Status: StatusEnabled, OrganizationID: "org", Memberships: []Membership{{Sub: "owner-" + id, Role: RoleOwner}, {Sub: "reader", Role: RoleMember}}, Services: []any{map[string]any{"serviceId": "svc"}}}
		if err := db.C("project").Upsert(ctx, id, p); err != nil {
			t.Fatal(err)
		}
	}
	router := chi.NewRouter()
	h.Routes(router)
	return h, ctx, router
}
func consoleSessionRequest(router http.Handler, ctx context.Context, sub, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r = r.WithContext(httpx.WithRC(ctx, &httpx.RequestContext{Sub: sub}))
	r.Header.Set("X-API-Key", "e2b_"+strings.Repeat("a", 40))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w
}
func TestConsolePortSessionManageIsolationAndAliases(t *testing.T) {
	h, ctx, router := consoleSessionFixture(t)
	now := time.Now().UTC()
	image := "registry.example.org/t/p/0123456789abcdef0123@sha256:" + strings.Repeat("a", 64)
	for _, doc := range []*sandboxTemplateDoc{
		{ID: "ready", ProjectID: "p", Name: "ready", Aliases: []string{"existing-alias"}, CPU: 2, MemoryGiB: 2, Tags: map[string]string{"default": "done"}, Builds: []templateBuild{{BuildID: "done", Status: sandbox.BuildReady, Image: image, Diagnostics: &buildDiagnostics{ArchivedAt: &now, CleanedAt: &now}, Logs: []string{"one", "two"}}}},
		{ID: "active", ProjectID: "p", Name: "active", Builds: []templateBuild{{BuildID: "running", Status: sandbox.BuildBuilding}}},
		{ID: "internal", ProjectID: "p", Name: "private-pause", Internal: true, Kind: "snapshot"},
		{ID: "foreign-template", ProjectID: "foreign", Name: "foreign-template"},
		{ID: "waiting", ProjectID: "p", Name: "waiting", Builds: []templateBuild{{BuildID: "reserved", Status: sandbox.BuildWaiting}}},
	} {
		if err := h.sbTemplates.Upsert(ctx, doc.ID, doc); err != nil {
			t.Fatal(err)
		}
	}
	base := "/project/p/sandbox-templates"
	for _, tc := range []struct {
		sub, method, path, body string
		status                  int
	}{
		{"", "GET", base + "/capabilities", "", 401}, {"outsider", "GET", base, "", 404}, {"reader", "GET", base, "", 403},
		{"owner-foreign", "POST", base + "/ready/aliases", `{"aliases":["hijack"]}`, 404},
		{"owner-p", "POST", base + "/foreign-template/aliases", `{"aliases":["hijack"]}`, 404},
		{"owner-p", "DELETE", base + "/foreign-template", "", 404}, {"owner-p", "DELETE", base + "/internal", "", 404},
		{"owner-p", "POST", base + "/internal/prepare", "", 404}, {"owner-p", "POST", base + "/internal/aliases", `{"aliases":["hijack"]}`, 404},
		{"owner-p", "DELETE", base + "/active", "", 409}, {"owner-p", "POST", base + "/ready/aliases", `{"aliases":["active"]}`, 409},
		{"owner-p", "POST", base + "/waiting/prepare", "", 409}, {"owner-p", "GET", base + "/ready/builds/done/logs?logsOffset=-1", "", 400},
	} {
		w := consoleSessionRequest(router, ctx, tc.sub, tc.method, tc.path, tc.body)
		if w.Code != tc.status {
			t.Errorf("%s %s (%s):%d %s want%d", tc.method, tc.path, tc.sub, w.Code, w.Body, tc.status)
		}
	}
	w := consoleSessionRequest(router, ctx, "owner-p", "GET", base+"/capabilities", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"importEnabled":false`) {
		t.Fatal(w.Code, w.Body)
	}
	w = consoleSessionRequest(router, ctx, "owner-p", "GET", base, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-pause") || strings.Contains(w.Body.String(), "foreign-template") {
		t.Fatal(w.Code, w.Body)
	}
	w = consoleSessionRequest(router, ctx, "owner-p", "POST", base+"/ready/aliases", `{"aliases":["new-alias"]}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w = consoleSessionRequest(router, ctx, "owner-p", "GET", base+"/ready/builds/done/logs?logsOffset=1", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"logs":["two"]`) {
		t.Fatal(w.Code, w.Body)
	}
	w = consoleSessionRequest(router, ctx, "owner-p", "DELETE", base+"/new-alias", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	w = consoleSessionRequest(router, cancelled, "owner-p", "POST", base+"/ready/aliases", `{"aliases":["rolled-back"]}`)
	if w.Code < 400 {
		t.Fatal("canceled mutation acknowledged")
	}
	if h.sbKeys != nil {
		t.Fatal("fixture unexpectedly required API keys")
	}
	var data any
	_ = json.Unmarshal(w.Body.Bytes(), &data)
}

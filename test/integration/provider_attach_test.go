//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/cephcred"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/providerattach"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// attachRig wires the real pieces the bulk attach runs on: the durable job store, the providerattach service + runner,
// the project bootstrap (project.Handler.AttachByOperator) and the admin surface, on a fresh PostgreSQL database. The
// object-store provider "s3" points at a fake RGW admin API that counts its calls: every successful attach of a project
// to "s3" makes exactly one user create there.
type attachRig struct {
	db        *pgdoc.DB
	enc       *textcrypt.Encryptor
	jobs      *durablejobs.Store
	svc       *providerattach.Service
	runner    *providerattach.Runner
	projects  *project.Repo
	projectH  *project.Handler
	esSvc     *externalservice.Service
	adminH    *admin.Handler
	router    chi.Router
	rgwCalls  atomic.Int64    // user creates seen by the fake RGW
	rgwFail   map[string]bool // projects the fake RGW refuses
	rgwMu     sync.Mutex
	rgwURL    string
	auditRepo *audit.Repo
}

const (
	attachIss = "operator-realm"
	attachCid = "operator-client"
)

func newAttachRig(t *testing.T) *attachRig {
	t.Helper()
	ctx := context.Background()
	db := freshPG(t)
	rig := &attachRig{db: db, enc: textcrypt.New("test-key"), rgwFail: map[string]bool{}}
	rgw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/admin/user") && r.Method == http.MethodPut {
			uid := r.URL.Query().Get("uid")
			rig.rgwMu.Lock()
			fail := false
			for p := range rig.rgwFail {
				if strings.HasSuffix(uid, p) {
					fail = true
				}
			}
			rig.rgwMu.Unlock()
			if fail {
				http.Error(w, `{"Code":"AccessDenied","Message":"secret-token-xyz rejected"}`, http.StatusForbidden)
				return
			}
			rig.rgwCalls.Add(1)
		}
		if strings.HasPrefix(r.URL.Path, "/admin/user") && r.Method == http.MethodGet {
			http.Error(w, `{"Code":"NoSuchUser"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"user_id":"u","keys":[{"user":"u","access_key":"a","secret_key":"s"}]}`))
	}))
	t.Cleanup(rgw.Close)
	rig.rgwURL = rgw.URL

	rig.jobs = durablejobs.New(db)
	if err := rig.jobs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	rig.svc = providerattach.New(db, rig.jobs)
	if err := rig.svc.Repo.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"project", "organization", "externalService", "auditEvent"} {
		if err := db.C(c).Ensure(ctx); err != nil {
			t.Fatal(err)
		}
	}
	rig.esSvc = externalservice.NewService(externalservice.NewRepo(db), rig.enc)
	rig.projects = project.NewRepo(db)
	rig.projectH = project.NewHandler(project.NewService(rig.projects, nil, nil, nil, nil), nil, nil, nil, nil, nil, cloud.NewRepo(db), rig.esSvc, nil, nil, "r1")
	rig.projectH.SetCephCreds(cephcred.New(db, rig.enc), cephcred.NewKeyRepo(db, rig.enc))
	rig.runner = &providerattach.Runner{
		Store: rig.jobs, Repo: rig.svc.Repo, Lease: 10 * time.Second, Log: slog.Default(), AttemptTimeout: 20 * time.Second,
		Provider: func(ctx context.Context, id string) (*externalservice.ExternalService, error) {
			es, err := rig.esSvc.Get(ctx, id)
			if err != nil && strings.Contains(err.Error(), "not found") {
				return nil, nil
			}
			return es, err
		},
		Attach: func(ctx context.Context, projectID string, es *externalservice.ExternalService) (string, error) {
			return rig.projectH.AttachByOperator(ctx, projectID, es, true)
		},
	}
	rig.auditRepo = audit.NewRepo(db)
	rig.adminH = admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, audit.NewService(rig.auditRepo, slog.Default()), rig.esSvc, "r1", attachIss, attachCid)
	rig.adminH.SetProviderAttach(rig.svc)
	rig.adminH.SetProjectCloudOps(&admin.ProjectCloudOps{Attach: func(ctx context.Context, projectID, esID string) (string, error) {
		es, err := rig.esSvc.Get(ctx, esID)
		if err != nil {
			return "", err
		}
		return rig.projectH.AttachByOperator(ctx, projectID, es, false)
	}})
	rig.router = chi.NewRouter()
	rig.router.MethodNotAllowed(httpx.MethodNotAllowedHandler())
	rig.adminH.Routes(rig.router)
	return rig
}

// s3Row is the object-store provider the bulk attach targets.
func (rig *attachRig) s3Row(id string, extra pgdoc.M) pgdoc.M {
	cfg := pgdoc.M{"provider": "ceph-s3", "s3Endpoint": rig.rgwURL, "adminApiUrl": rig.rgwURL + "/admin", "region": "r1"}
	for k, v := range extra {
		cfg[k] = v
	}
	return pgdoc.M{"_id": id, "name": id, "type": externalservice.TypeCloud, "status": externalservice.StatusPublic, "config": cfg,
		"secret": pgdoc.M{"adminAccessKey": rig.enc.Encrypt("AK"), "adminSecretKey": rig.enc.Encrypt("SK")}}
}

func dnsRow(id string, extra pgdoc.M) pgdoc.M {
	cfg := pgdoc.M{"provider": "powerdns", "apiUrl": "http://127.0.0.1:1", "region": "r1"}
	for k, v := range extra {
		cfg[k] = v
	}
	return pgdoc.M{"_id": id, "name": id, "type": externalservice.TypeCloud, "status": externalservice.StatusPublic, "config": cfg}
}

func (rig *attachRig) project(t *testing.T, id, org, status string, services ...string) {
	t.Helper()
	svcs := []any{}
	for _, s := range services {
		svcs = append(svcs, pgdoc.M{"serviceId": s, "provider": "powerdns", "region": "r1"})
	}
	mustInsert(t, rig.db, "project", pgdoc.M{"_id": id, "name": "project " + id, "organizationId": org, "status": status, "services": svcs})
}

func (rig *attachRig) load(t *testing.T, id string) *project.Project {
	t.Helper()
	p, err := rig.projects.FindByID(context.Background(), id)
	if err != nil || p == nil {
		t.Fatalf("project %s: %v %v", id, p, err)
	}
	return p
}

func bindings(p *project.Project, serviceID string) int {
	n := 0
	for _, id := range p.ServiceIDs() {
		if id == serviceID {
			n++
		}
	}
	return n
}

// do calls the admin surface as an operator (REMOTE_OIDC: admin issuer + client → SUPER_ADMIN).
func (rig *attachRig) do(t *testing.T, method, path, body string) (int, map[string]any) {
	return rig.doAs(t, &httpx.RequestContext{Sub: "op-sub", Issuer: attachIss, Azp: attachCid}, method, path, body)
}

func (rig *attachRig) doAs(t *testing.T, rc *httpx.RequestContext, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(httpx.WithRC(req.Context(), rc))
	w := httptest.NewRecorder()
	rig.router.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// drain runs the runner until no job is claimable.
func (rig *attachRig) drain(t *testing.T) {
	t.Helper()
	for i := 0; i < 20; i++ {
		did, err := rig.runner.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !did {
			return
		}
	}
	t.Fatal("runner did not settle")
}

func (rig *attachRig) start(t *testing.T, serviceID, body string) string {
	t.Helper()
	code, out := rig.do(t, http.MethodPost, "/admin/service/"+serviceID+"/bulk-attach", body)
	if code != http.StatusOK {
		t.Fatalf("start: %d %v", code, out)
	}
	return out["data"].(map[string]any)["id"].(string)
}

func (rig *attachRig) view(t *testing.T, serviceID, runID string) *providerattach.View {
	t.Helper()
	v, err := rig.svc.View(context.Background(), serviceID, runID)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// ── policy ───────────────────────────────────────────────────────────────────────────────────────

// TestAttachPolicyOnNewProject: a new project's first bootstrap provisions onto the providers whose policy is on (or
// absent: the default) and not onto one whose policy is off; turning the policy off changes nothing for projects
// already attached.
func TestAttachPolicyOnNewProject(t *testing.T) {
	rig := newAttachRig(t)
	ctx := context.Background()
	mustInsert(t, rig.db, "externalService", dnsRow("dns-default", nil))
	mustInsert(t, rig.db, "externalService", dnsRow("dns-on", pgdoc.M{"attachToNewProjects": true}))
	mustInsert(t, rig.db, "externalService", dnsRow("dns-off", pgdoc.M{"attachToNewProjects": false}))
	rig.project(t, "new", "o1", project.StatusEnabled)
	if err := rig.projectH.EnableAndBootstrap(ctx, rig.load(t, "new")); err != nil {
		t.Fatal(err)
	}
	p := rig.load(t, "new")
	if !p.HasService("dns-default") || !p.HasService("dns-on") || p.HasService("dns-off") {
		t.Fatalf("bindings %v: want dns-default + dns-on, not dns-off", p.ServiceIDs())
	}
	// Re-entry never adds the off provider, and an operator attach of it still works (the per-project POST).
	if err := rig.projectH.EnableAndBootstrap(ctx, p); err != nil || rig.load(t, "new").HasService("dns-off") {
		t.Fatalf("re-entry: %v %v", err, rig.load(t, "new").ServiceIDs())
	}
	if code, out := rig.do(t, http.MethodPost, "/admin/project/new/external-service/dns-off", ""); code != http.StatusOK {
		t.Fatalf("manual attach of an off provider: %d %v", code, out)
	}
	if bindings(rig.load(t, "new"), "dns-off") != 1 {
		t.Fatal("manual attach did not bind")
	}
	// The policy is set through its own PUT, which keeps the rest of config; a non-boolean is refused.
	if code, out := rig.do(t, http.MethodPut, "/admin/service/dns-on/attach-policy", `{"attachToNewProjects":"no"}`); code != http.StatusBadRequest {
		t.Fatalf("string policy: %d %v", code, out)
	}
	if code, out := rig.do(t, http.MethodPut, "/admin/service/dns-on/attach-policy", `{"attachToNewProjects":false}`); code != http.StatusOK {
		t.Fatalf("policy off: %d %v", code, out)
	}
	es, _ := rig.esSvc.Get(ctx, "dns-on")
	if es.AttachToNewProjects() || es.Config["apiUrl"] != "http://127.0.0.1:1" || es.Config["region"] != "r1" {
		t.Fatalf("policy save lost config: %v", es.Config)
	}
	if !rig.load(t, "new").HasService("dns-on") {
		t.Fatal("turning the policy off detached a project")
	}
	rig.project(t, "newer", "o1", project.StatusEnabled)
	if err := rig.projectH.EnableAndBootstrap(ctx, rig.load(t, "newer")); err != nil {
		t.Fatal(err)
	}
	if p := rig.load(t, "newer"); !p.HasService("dns-default") || p.HasService("dns-on") || p.HasService("dns-off") {
		t.Fatalf("after turning dns-on off: %v", p.ServiceIDs())
	}
}

// ── bulk attach ──────────────────────────────────────────────────────────────────────────────────

// TestBulkAttachScopedOnceAndRerunNoop: the bulk attach binds exactly the scoped, eligible projects not yet attached,
// once each (one provider call per attached project), records the rest as skipped with a fixed reason, and a second run
// over the same scope attaches nothing and calls the provider for nothing. The audit event carries actor, provider,
// scope and counts.
func TestBulkAttachScopedOnceAndRerunNoop(t *testing.T) {
	rig := newAttachRig(t)
	mustInsert(t, rig.db, "externalService", rig.s3Row("s3", pgdoc.M{"attachToNewProjects": false}))
	mustInsert(t, rig.db, "externalService", dnsRow("dns", nil))
	for _, o := range []string{"o1", "o2"} {
		mustInsert(t, rig.db, "organization", pgdoc.M{"_id": o, "name": "org " + o})
	}
	rig.project(t, "a1", "o1", project.StatusEnabled, "dns")
	rig.project(t, "a2", "o1", project.StatusDisabled, "dns") // a billing hold: attached like the per-project attach does
	rig.project(t, "a3", "o1", project.StatusScheduledForDeletion, "dns")
	rig.project(t, "a4", "o1", project.StatusDeleteInProgress, "dns")
	rig.project(t, "b1", "o2", project.StatusEnabled, "dns") // out of scope
	mustInsert(t, rig.db, "project", pgdoc.M{"_id": "a5", "name": "already", "organizationId": "o1", "status": project.StatusEnabled,
		"services": []any{pgdoc.M{"serviceId": "dns", "provider": "powerdns"}, pgdoc.M{"serviceId": "s3", "provider": "ceph-s3", "region": "r1", "rgwUid": "proxcloud-a5"}}})

	// Preview tells the operator what the scope gives before starting.
	code, out := rig.do(t, http.MethodPost, "/admin/service/s3/bulk-attach/preview", `{"scope":{"mode":"organizations","organizationIds":["o1"]}}`)
	if code != http.StatusOK {
		t.Fatalf("preview: %d %v", code, out)
	}
	if pv := out["data"].(map[string]any); pv["inScope"] != 5.0 || pv["alreadyAttached"] != 1.0 || pv["deleting"] != 2.0 || pv["toAttach"] != 2.0 {
		t.Fatalf("preview %v", pv)
	}
	runID := rig.start(t, "s3", `{"scope":{"mode":"organizations","organizationIds":["o1"]},"concurrency":3}`)
	rig.drain(t)
	v := rig.view(t, "s3", runID)
	if v.Status != providerattach.RunSucceeded || v.Counts.Total != 5 || v.Counts.Attached != 2 || v.Counts.Skipped != 3 || v.Counts.Failed != 0 ||
		v.Counts.ByCode[providerattach.CodeAlreadyAttached] != 1 || v.Counts.ByCode[providerattach.CodeDeleting] != 2 {
		t.Fatalf("run %+v counts %+v", v.Status, v.Counts)
	}
	if got := rig.rgwCalls.Load(); got != 2 {
		t.Fatalf("provider calls %d, want 2 (one per attached project)", got)
	}
	for id, want := range map[string]int{"a1": 1, "a2": 1, "a3": 0, "a4": 0, "a5": 1, "b1": 0} {
		if got := bindings(rig.load(t, id), "s3"); got != want {
			t.Fatalf("%s: %d s3 bindings, want %d", id, got, want)
		}
	}
	if p := rig.load(t, "a2"); p.Status != project.StatusDisabled {
		t.Fatalf("the attach changed a DISABLED project's status: %s", p.Status)
	}
	if p := rig.load(t, "a3"); p.Status != project.StatusScheduledForDeletion {
		t.Fatalf("a project scheduled for deletion changed: %s", p.Status)
	}

	// Audit: the start event names the actor, the provider, the scope and the counts.
	evs, err := rig.auditRepo.QueryAll(context.Background(), audit.Filter{ResourceID: "s3", Action: admin.ActionBulkAttach}, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("want one bulk attach audit event: %v %v", evs, err)
	}
	ev := evs[0]
	scope, _ := ev.ResourceMetadata["scope"].(map[string]any)
	counts, _ := ev.ResourceMetadata["counts"].(map[string]any)
	if ev.Actor == nil || ev.Actor.ID != "op-sub" || ev.ResourceType != "EXTERNAL_SERVICE" || scope["mode"] != "organizations" ||
		fmt.Sprint(counts["toAttach"]) != "2" || ev.ResourceMetadata["runId"] != runID {
		t.Fatalf("audit event %+v", ev)
	}

	// The same scope again: nothing to attach, no provider call.
	again := rig.start(t, "s3", `{"scope":{"mode":"organizations","organizationIds":["o1"]}}`)
	rig.drain(t)
	v = rig.view(t, "s3", again)
	if v.Status != providerattach.RunSucceeded || v.Counts.Attached != 0 || v.Counts.ByCode[providerattach.CodeAlreadyAttached] != 3 {
		t.Fatalf("re-run %+v %+v", v.Status, v.Counts)
	}
	if got := rig.rgwCalls.Load(); got != 2 {
		t.Fatalf("a re-run called the provider: %d calls", got)
	}
	for _, id := range []string{"a1", "a2", "a5"} {
		if got := bindings(rig.load(t, id), "s3"); got != 1 {
			t.Fatalf("%s: %d bindings after re-run", id, got)
		}
	}

	// A picked-project scope with an unknown id is refused before anything is queued.
	if code, out := rig.do(t, http.MethodPost, "/admin/service/s3/bulk-attach", `{"scope":{"mode":"projects","projectIds":["b1","nope"]}}`); code != http.StatusBadRequest {
		t.Fatalf("unknown project: %d %v", code, out)
	}
	// A picked project: only it.
	picked := rig.start(t, "s3", `{"scope":{"mode":"projects","projectIds":["b1"]}}`)
	rig.drain(t)
	if v := rig.view(t, "s3", picked); v.Counts.Total != 1 || v.Counts.Attached != 1 || bindings(rig.load(t, "b1"), "s3") != 1 {
		t.Fatalf("picked run %+v", v.Counts)
	}
}

// TestBulkAttachFailingProjectsContinue: a project the provider refuses is recorded failed with a fixed code (never the
// provider's text) and the run goes on; retrying the failed projects once the provider accepts them attaches them.
func TestBulkAttachFailingProjectsContinue(t *testing.T) {
	rig := newAttachRig(t)
	mustInsert(t, rig.db, "externalService", rig.s3Row("s3", nil))
	for i := 1; i <= 6; i++ {
		rig.project(t, fmt.Sprintf("p%d", i), "o1", project.StatusEnabled, "dns")
	}
	rig.rgwMu.Lock()
	rig.rgwFail["p2"], rig.rgwFail["p5"] = true, true
	rig.rgwMu.Unlock()
	runID := rig.start(t, "s3", `{"scope":{"mode":"all"},"concurrency":2}`)
	rig.drain(t)
	v := rig.view(t, "s3", runID)
	if v.Status != providerattach.RunCompletedWithFailures || v.Counts.Attached != 4 || v.Counts.Failed != 2 ||
		v.Counts.ByCode[providerattach.CodeProviderError] != 2 {
		t.Fatalf("run %s %+v", v.Status, v.Counts)
	}
	code, out := rig.do(t, http.MethodGet, "/admin/service/s3/bulk-attach/"+runID+"/items?state=failed", "")
	if code != http.StatusOK {
		t.Fatalf("items: %d %v", code, out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "secret-token-xyz") || strings.Contains(string(raw), "AccessDenied") {
		t.Fatalf("the provider's text reached the results: %s", raw)
	}
	items := out["data"].(map[string]any)["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["projectId"] != "p2" || items[1].(map[string]any)["code"] != providerattach.CodeProviderError {
		t.Fatalf("failed items %v", items)
	}
	// The job itself succeeded: per-project failures are results, not job failures.
	if j, _ := rig.jobs.Get(context.Background(), v.JobID); j.State != "succeeded" {
		t.Fatalf("job %+v", j)
	}

	rig.rgwMu.Lock()
	delete(rig.rgwFail, "p2")
	delete(rig.rgwFail, "p5")
	rig.rgwMu.Unlock()
	code, out = rig.do(t, http.MethodPost, "/admin/service/s3/bulk-attach/"+runID+"/retry", "")
	if code != http.StatusOK {
		t.Fatalf("retry: %d %v", code, out)
	}
	retry := out["data"].(map[string]any)["id"].(string)
	rig.drain(t)
	if v := rig.view(t, "s3", retry); v.Status != providerattach.RunSucceeded || v.Counts.Total != 2 || v.Counts.Attached != 2 {
		t.Fatalf("retry run %s %+v", v.Status, v.Counts)
	}
	for i := 1; i <= 6; i++ {
		if got := bindings(rig.load(t, fmt.Sprintf("p%d", i)), "s3"); got != 1 {
			t.Fatalf("p%d: %d bindings", i, got)
		}
	}
	// Nothing failed in the retry: retrying it again is refused.
	if code, _ := rig.do(t, http.MethodPost, "/admin/service/s3/bulk-attach/"+retry+"/retry", ""); code != http.StatusConflict {
		t.Fatalf("retry of a clean run: %d", code)
	}
}

// TestBulkAttachResumesAfterLeaseLoss: an acquisition that loses its lease mid-run writes nothing more; the next
// acquisition resumes from the per-project results and finishes, with every project bound exactly once and the
// provider called once per project.
func TestBulkAttachResumesAfterLeaseLoss(t *testing.T) {
	rig := newAttachRig(t)
	ctx := context.Background()
	mustInsert(t, rig.db, "externalService", rig.s3Row("s3", nil))
	const n = 8
	for i := 1; i <= n; i++ {
		rig.project(t, fmt.Sprintf("p%d", i), "o1", project.StatusEnabled, "dns")
	}
	runID := rig.start(t, "s3", `{"scope":{"mode":"all"},"concurrency":1}`)

	// First acquisition: after the third attach, its lease is taken away (the row's lease expires as if the process had
	// stalled), and a stale owner may not write anything more.
	attaches := 0
	orig := rig.runner.Attach
	rig.runner.Attach = func(ctx context.Context, projectID string, es *externalservice.ExternalService) (string, error) {
		out, err := orig(ctx, projectID, es)
		attaches++
		if attaches == 3 {
			if _, e := rig.db.Pool.Exec(context.Background(), `UPDATE durable_job SET lease_until=clock_timestamp()-interval '1 second' WHERE kind=$1`, durablejobs.ProviderAttach); e != nil {
				t.Error(e)
			}
		}
		return out, err
	}
	if did, err := rig.runner.RunOnce(ctx); !did || err != nil {
		t.Fatalf("first acquisition: %v %v", did, err)
	}
	v := rig.view(t, "s3", runID)
	if v.Status == providerattach.RunSucceeded || v.Counts.Attached > 3 {
		t.Fatalf("the stale acquisition went on writing: %s %+v", v.Status, v.Counts)
	}
	// The item whose result could not be recorded stays running (its attach landed on the project).
	if v.Counts.Running != 1 || v.Counts.Attached != 2 {
		t.Fatalf("after the lease loss: %+v", v.Counts)
	}

	rig.runner.Attach = orig
	rig.drain(t)
	v = rig.view(t, "s3", runID)
	if v.Status != providerattach.RunSucceeded || v.Counts.Attached+v.Counts.ByCode[providerattach.CodeAlreadyAttached] != n || v.Counts.Failed != 0 {
		t.Fatalf("resumed run %s %+v", v.Status, v.Counts)
	}
	if got := rig.rgwCalls.Load(); got != n {
		t.Fatalf("provider calls %d, want %d: a project was provisioned twice", got, n)
	}
	for i := 1; i <= n; i++ {
		if got := bindings(rig.load(t, fmt.Sprintf("p%d", i)), "s3"); got != 1 {
			t.Fatalf("p%d: %d bindings", i, got)
		}
	}
	if j, _ := rig.jobs.Get(ctx, v.JobID); j.State != "succeeded" || j.Attempts != 2 {
		t.Fatalf("job %+v", j)
	}
}

// TestBulkAttachCancelAndOneActivePerProvider: a second run for the same provider is refused while one is active; a
// cancel leaves what was attached and marks the rest cancelled.
func TestBulkAttachCancelAndOneActivePerProvider(t *testing.T) {
	rig := newAttachRig(t)
	ctx := context.Background()
	mustInsert(t, rig.db, "externalService", rig.s3Row("s3", nil))
	mustInsert(t, rig.db, "externalService", rig.s3Row("s3b", nil))
	for i := 1; i <= 5; i++ {
		rig.project(t, fmt.Sprintf("p%d", i), "o1", project.StatusEnabled, "dns")
	}
	runID := rig.start(t, "s3", `{"scope":{"mode":"all"},"concurrency":1}`)
	if code, out := rig.do(t, http.MethodPost, "/admin/service/s3/bulk-attach", `{"scope":{"mode":"all"}}`); code != http.StatusConflict {
		t.Fatalf("second active run: %d %v", code, out)
	}
	// Another provider is independent.
	other := rig.start(t, "s3b", `{"scope":{"mode":"projects","projectIds":["p1"]}}`)

	attaches := 0
	orig := rig.runner.Attach
	rig.runner.Attach = func(ctx context.Context, projectID string, es *externalservice.ExternalService) (string, error) {
		out, err := orig(ctx, projectID, es)
		if es.ID == "s3" {
			attaches++
			if attaches == 2 {
				if code, body := rig.do(t, http.MethodPost, "/admin/service/s3/bulk-attach/"+runID+"/cancel", ""); code != http.StatusOK {
					t.Errorf("cancel: %d %v", code, body)
				}
			}
		}
		return out, err
	}
	rig.drain(t)
	v := rig.view(t, "s3", runID)
	if v.Status != providerattach.RunCancelled || v.Counts.Attached != 2 || v.Counts.Cancelled != 3 || v.CancelledBy != "op-sub" {
		t.Fatalf("cancelled run %s %+v", v.Status, v.Counts)
	}
	if v := rig.view(t, "s3b", other); v.Status != providerattach.RunSucceeded {
		t.Fatalf("other provider's run %s", v.Status)
	}
	if code, _ := rig.do(t, http.MethodPost, "/admin/service/s3/bulk-attach/"+runID+"/cancel", ""); code != http.StatusConflict {
		t.Fatalf("cancel of a finished run: %d", code)
	}
	// Finished: a new run may start.
	rig.start(t, "s3", `{"scope":{"mode":"all"}}`)
	rig.drain(t)
	for i := 1; i <= 5; i++ {
		if got := bindings(rig.load(t, fmt.Sprintf("p%d", i)), "s3"); got != 1 {
			t.Fatalf("p%d: %d", i, got)
		}
	}
	_ = ctx
}

// TestBulkAttachPermissions: reading needs admin:service:read; starting, cancelling and retrying need
// admin:service:manage (a SUPPORT operator, who may read, cannot start one), and a tenant gets nothing.
func TestBulkAttachPermissions(t *testing.T) {
	rig := newAttachRig(t)
	ctx := context.Background()
	mustInsert(t, rig.db, "externalService", rig.s3Row("s3", nil))
	rig.project(t, "p1", "o1", project.StatusEnabled, "dns")
	// LOCAL_IDP posture: the role comes from the adminPermission doc of the sub.
	local := admin.NewHandler(admin.NewRepo(rig.db), nil, nil, nil, nil, nil, nil, nil, nil, rig.esSvc, "r1", "", "")
	local.SetProviderAttach(rig.svc)
	router := chi.NewRouter()
	local.Routes(router)
	mustInsert(t, rig.db, "adminPermission", pgdoc.M{"_id": "support-sub", "role": "SUPPORT"})
	mustInsert(t, rig.db, "adminPermission", pgdoc.M{"_id": "admin-sub", "role": "ADMIN"})
	call := func(sub, method, path, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}
	start := `{"scope":{"mode":"all"}}`
	if code := call("support-sub", http.MethodGet, "/admin/service/s3/bulk-attach", ""); code != http.StatusOK {
		t.Fatalf("service:read list: %d", code)
	}
	if code := call("support-sub", http.MethodPost, "/admin/service/s3/bulk-attach/preview", start); code != http.StatusOK {
		t.Fatalf("service:read preview: %d", code)
	}
	if code := call("support-sub", http.MethodPost, "/admin/service/s3/bulk-attach", start); code != http.StatusForbidden {
		t.Fatalf("service:read start: %d, want 403", code)
	}
	if n, _ := rig.svc.Repo.CountRuns(ctx, "s3"); n != 0 {
		t.Fatal("a refused start recorded a run")
	}
	if code := call("tenant-sub", http.MethodGet, "/admin/service/s3/bulk-attach", ""); code != http.StatusForbidden {
		t.Fatalf("tenant: %d", code)
	}
	if code := call("admin-sub", http.MethodPost, "/admin/service/s3/bulk-attach", start); code != http.StatusOK {
		t.Fatalf("service:manage start: %d", code)
	}
	runs, _, _ := rig.svc.List(ctx, "s3", 5)
	if code := call("support-sub", http.MethodPost, "/admin/service/s3/bulk-attach/"+runs[0].ID+"/cancel", ""); code != http.StatusForbidden {
		t.Fatalf("service:read cancel: %d", code)
	}
	if code := call("admin-sub", http.MethodPost, "/admin/service/s3/bulk-attach/"+runs[0].ID+"/cancel", ""); code != http.StatusOK {
		t.Fatalf("service:manage cancel: %d", code)
	}
	// The policy PUT is a provider mutation: service:manage too.
	if code := call("support-sub", http.MethodPut, "/admin/service/s3/attach-policy", `{"attachToNewProjects":false}`); code != http.StatusForbidden {
		t.Fatalf("service:read policy: %d", code)
	}
}

// TestBulkAttachBeforeFirstBootstrap: a project the bulk attach reaches before it was ever opened keeps its first
// bootstrap: when it is first opened it still gets every provider whose policy is on.
func TestBulkAttachBeforeFirstBootstrap(t *testing.T) {
	rig := newAttachRig(t)
	ctx := context.Background()
	mustInsert(t, rig.db, "externalService", rig.s3Row("s3", pgdoc.M{"attachToNewProjects": false}))
	mustInsert(t, rig.db, "externalService", dnsRow("dns", nil))
	rig.project(t, "fresh", "o1", project.StatusDisabled) // waiting on billing, never bootstrapped
	rig.start(t, "s3", `{"scope":{"mode":"projects","projectIds":["fresh"]}}`)
	rig.drain(t)
	if p := rig.load(t, "fresh"); !p.HasService("s3") || p.HasService("dns") || p.Status != project.StatusDisabled {
		t.Fatalf("after the bulk attach: %v %s", p.ServiceIDs(), p.Status)
	}
	if err := rig.projectH.EnableAndBootstrap(ctx, rig.load(t, "fresh")); err != nil {
		t.Fatal(err)
	}
	p := rig.load(t, "fresh")
	if !p.HasService("dns") || bindings(p, "s3") != 1 || p.Status != project.StatusEnabled {
		t.Fatalf("first open after a bulk attach: %v %s", p.ServiceIDs(), p.Status)
	}
	for _, s := range p.Services {
		if m, _ := s.(map[string]any); m["attachedBy"] != nil {
			t.Fatalf("bulk mark left after the first bootstrap: %v", p.Services)
		}
	}
	calls := rig.rgwCalls.Load()
	if err := rig.projectH.EnableAndBootstrap(ctx, rig.load(t, "fresh")); err != nil || rig.rgwCalls.Load() != calls {
		t.Fatalf("a re-entry reprovisioned: %v", err)
	}
}

// TestAdminAttachRefusesDeletingProject: the per-project attach (which the bulk attach shares) refuses a project on its
// way out instead of provisioning it.
func TestAdminAttachRefusesDeletingProject(t *testing.T) {
	rig := newAttachRig(t)
	mustInsert(t, rig.db, "externalService", rig.s3Row("s3", nil))
	rig.project(t, "gone", "o1", project.StatusDeleteInProgress, "dns")
	if code, out := rig.do(t, http.MethodPost, "/admin/project/gone/external-service/s3", ""); code != http.StatusBadRequest {
		t.Fatalf("%d %v", code, out)
	}
	if rig.load(t, "gone").HasService("s3") || rig.rgwCalls.Load() != 0 {
		t.Fatal("a deleting project was attached")
	}
}

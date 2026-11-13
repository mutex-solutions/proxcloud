//go:build integration

package integration

// The built-in sandbox image mirror end to end on PostgreSQL (docs/template-updates.md §4): the admin endpoints (flag,
// permission, confirm, same-digest refusal, dedup per image, audit), the durable job through the real store (fenced
// runner, receipts, sandboxBuiltinPin, resume after a lost lease, rollback), with a fake GHCR, a fake sandbox cluster
// and a fake sandbox registry. The GitHub token must never be persisted anywhere.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/sandboximport"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

const (
	tuReg      = "192.0.2.226"
	tuPAT      = "ghp_INTEGRATION_PAT_never_persisted_0000"
	tuRegToken = "ghcr-INTEGRATION-pull-token"
	tuMirror   = "MIRROR-PASSWORD-never-returned"
	tuTag      = "20261112-9d00563"
	tuOldTag   = "20261101-aaaaaaa"
	tuService  = "sbx-1"
)

var (
	tuOld = "sha256:" + strings.Repeat("1", 64)
	tuNew = "sha256:" + strings.Repeat("2", 64)
)

// fakeGHCR is the GitHub Packages side: one sandbox-base package with an old and a new tag.
type fakeGHCR struct{}

func (fakeGHCR) ListVersions(_ context.Context, pkg string) ([]templateupdates.PackageVersion, error) {
	if pkg != "sandbox-base" {
		return nil, &templateupdates.HTTPError{Status: 404, Message: "Package not found."}
	}
	mk := func(d, tag string, at time.Time) templateupdates.PackageVersion {
		v := templateupdates.PackageVersion{Name: d, CreatedAt: at}
		v.Metadata.Container.Tags = []string{tag}
		return v
	}
	return []templateupdates.PackageVersion{mk(tuNew, tuTag, time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)),
		mk(tuOld, tuOldTag, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))}, nil
}
func (fakeGHCR) GetManifest(_ context.Context, _ string, _ string) (*templateupdates.Manifest, []byte, error) {
	return &templateupdates.Manifest{SchemaVersion: 2, MediaType: templateupdates.MediaOCIManifest,
		Config: &templateupdates.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: tuOld, Size: 10},
		Layers: []templateupdates.Descriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: tuOld, Size: 10}}}, nil, nil
}
func (fakeGHCR) FetchBlob(context.Context, string, templateupdates.Descriptor, int64) ([]byte, error) {
	return nil, errors.New("not used")
}

// tuCluster is the fake sandbox cluster: prepull DaemonSets, worker pods that succeed, and the sandbox registry
// (what the worker pushed resolves by tag and digest).
type tuCluster struct {
	mu       sync.Mutex
	prepull  string // template-prepull/p1 image
	pods     map[string]*sandbox.MirrorPod
	tokens   []string // what reached the worker request (the only place a PAT may go)
	pushed   map[string]string
	starts   int
	blockRun chan struct{}
}

func (c *tuCluster) StartMirror(_ context.Context, req sandbox.MirrorRequest) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.starts++
	c.tokens = append(c.tokens, req.SourceToken)
	name := sandbox.MirrorPodName(req.JobShort, req.Epoch)
	c.pods[name] = &sandbox.MirrorPod{Name: name, UID: "uid-" + name, Phase: "Running", Source: req.Source, Dest: req.Destination}
	return "uid-" + name, nil
}
func (c *tuCluster) GetMirrorPod(_ context.Context, _, name string) (*sandbox.MirrorPod, error) {
	if c.blockRun != nil {
		<-c.blockRun
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pods[name]
	if p == nil {
		return nil, nil
	}
	if p.Phase == "Running" {
		p.Phase = "Succeeded"
		_, d, _ := strings.Cut(p.Source, "@")
		c.pushed["library/sandbox-base@"+tuTag] = d
	}
	cp := *p
	return &cp, nil
}
func (c *tuCluster) MirrorResult(_ context.Context, p *sandbox.MirrorPod) (*sandboximport.MirrorResult, error) {
	_, d, _ := strings.Cut(p.Source, "@")
	return &sandboximport.MirrorResult{Source: p.Source, Destination: p.Dest, SourceDigest: d, DestinationDigest: d, Platform: "linux/amd64", SizeBytes: 7}, nil
}
func (c *tuCluster) DeleteMirrorPod(_ context.Context, name, uid string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.pods[name]; p != nil && p.UID == uid {
		delete(c.pods, name)
	}
	return nil
}
func (c *tuCluster) ListMirrorPods(context.Context, string) ([]sandbox.MirrorPod, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []sandbox.MirrorPod
	for _, p := range c.pods {
		out = append(out, *p)
	}
	return out, nil
}
func (c *tuCluster) PrepullImages(_ context.Context, repo string) ([]sandbox.PrepullImage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.HasPrefix(c.prepull, repo+":") {
		return []sandbox.PrepullImage{{DaemonSet: "template-prepull", Container: "p1", Image: c.prepull}}, nil
	}
	return nil, nil
}
func (c *tuCluster) PrepullProgress(_ context.Context, ref string) (int, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.prepull == ref {
		return 2, 3, nil
	}
	return 0, 3, nil
}
func (c *tuCluster) SetPrepullImage(_ context.Context, ref string) ([]sandbox.PrepullImage, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := c.prepull
	c.prepull = ref
	return []sandbox.PrepullImage{{DaemonSet: "template-prepull", Container: "p1", Image: prev}}, prev != ref, nil
}
func (c *tuCluster) RestorePrepullImages(_ context.Context, current string, restore []sandbox.PrepullImage) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range restore {
		if c.prepull == current && r.DaemonSet == "template-prepull" && r.Container == "p1" {
			c.prepull = r.Image
			return 1, nil
		}
	}
	return 0, nil
}
func (c *tuCluster) Resolve(_ context.Context, repo, ref string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok := c.pushed[repo+"@"+ref]; ok {
		return d, nil
	}
	if strings.HasPrefix(ref, "sha256:") {
		for _, d := range c.pushed {
			if d == ref {
				return d, nil
			}
		}
	}
	return "", templateupdates.ErrRegistryNotFound
}

type tuFixture struct {
	db      *pgdoc.DB
	jobs    *durablejobs.Store
	svc     *templateupdates.Service
	cluster *tuCluster
	router  http.Handler
	store   templateupdates.PGSandbox
}

func newTUFixture(t *testing.T, enabled bool) *tuFixture {
	t.Helper()
	ctx := context.Background()
	db := freshPG(t)
	jobs := durablejobs.New(db)
	if err := jobs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	enc := textcrypt.New("test-key")
	esSvc := externalservice.NewService(externalservice.NewRepo(db), enc)
	mustInsert(t, db, "externalService", pgdoc.M{"_id": tuService, "name": "Sandboxes", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
		"config": pgdoc.M{"provider": "sandbox", "region": "r1", "sandbox": pgdoc.M{"templateRegistry": tuReg + "/t",
			"templates": pgdoc.M{"base": pgdoc.M{"image": tuReg + "/library/sandbox-base:" + tuOldTag + "@" + tuOld}}}},
		"secret": esSvc.EncryptSecret(map[string]any{"kubeconfig": "x", "registryMirror": tuMirror})})
	store := templateupdates.PGSandbox{DB: db}
	svc := &templateupdates.Service{Token: func(context.Context) (string, string, error) { return tuPAT, "gh-1", nil },
		Jobs: jobs, DB: db, Receipts: templateupdates.PGReceipts{DB: db}, Sandbox: store, SandboxEnabled: enabled}
	svc.SetRegistryFactory(func(string) (templateupdates.Registry, error) { return fakeGHCR{}, nil })
	if err := svc.Receipts.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	cl := &tuCluster{prepull: tuReg + "/library/sandbox-base:" + tuOldTag + "@" + tuOld, pods: map[string]*sandbox.MirrorPod{},
		pushed: map[string]string{"library/sandbox-base@" + tuOldTag: tuOld}}
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, audit.NewService(audit.NewRepo(db), nil), esSvc, "r1", "", "")
	h.SetTemplateUpdates(svc)
	h.SetDurableJobs(jobs, nil, func(j *durablejobs.Job) error { _, err := templateupdates.DecodeSandboxPayload(j.Payload); return err })
	h.SetSandboxCluster(func(*externalservice.ExternalService) (admin.SandboxCluster, error) { return cl, nil })
	mustInsert(t, db, "adminPermission", pgdoc.M{"_id": "admin-sub", "role": "ADMIN"})
	mustInsert(t, db, "adminPermission", pgdoc.M{"_id": "support-sub", "role": "SUPPORT"})
	r := chi.NewRouter()
	h.Routes(r)
	return &tuFixture{db: db, jobs: jobs, svc: svc, cluster: cl, router: r, store: store}
}

func (f *tuFixture) do(t *testing.T, method, path, body, sub string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func (f *tuFixture) runner(lease time.Duration) *templateupdates.SandboxRunner {
	return &templateupdates.SandboxRunner{Store: f.jobs, Receipts: f.store, Lease: lease,
		Dead: f.store.DeadUncleaned, ClaimDead: f.jobs.ClaimDead,
		Build: func(context.Context, templateupdates.SandboxPayload) (*templateupdates.SandboxExecutor, error) {
			return &templateupdates.SandboxExecutor{Cluster: f.cluster, Registry: f.cluster, Receipts: f.store, Pins: f.store,
				Poll: 10 * time.Millisecond, PodWait: 5 * time.Second,
				SourceToken: func(ctx context.Context, pkg string) (string, error) {
					if tok, _, err := f.svc.Token(ctx); err != nil || tok != tuPAT || pkg != "sandbox-base" {
						return "", errors.New("unexpected token request")
					}
					return tuRegToken, nil
				}}, nil
		}}
}

func mirrorBody(digest, confirm string) string {
	return `{"serviceId":"` + tuService + `","name":"base","digest":"` + digest + `","confirmDigest":"` + confirm + `"}`
}

func TestSandboxMirrorFlagOffAndPermissions(t *testing.T) {
	f := newTUFixture(t, false)
	if code, body := f.do(t, "POST", "/admin/template-updates/sandbox", mirrorBody(tuNew, tuNew), "admin-sub"); code != http.StatusNotImplemented {
		t.Fatalf("flag off: %d %s", code, body)
	}
	if n, _ := f.db.C(templateupdates.SandboxReceiptCollection).Count(context.Background(), nil); n != 0 {
		t.Fatal("a disabled endpoint wrote a receipt")
	}
	// The pin list stays readable (installer stage 55) with the flag off; SUPPORT may read it.
	if code, body := f.do(t, "GET", "/admin/template-updates/sandbox/pins?registry="+tuReg, "", "support-sub"); code != http.StatusOK || !strings.Contains(body, `"pins":[]`) {
		t.Fatalf("pins with the flag off: %d %s", code, body)
	}
}

func TestSandboxMirrorPermissionsAndRefusals(t *testing.T) {
	f := newTUFixture(t, true)
	if code, _ := f.do(t, "POST", "/admin/template-updates/sandbox", mirrorBody(tuNew, tuNew), "support-sub"); code != http.StatusForbidden {
		t.Fatalf("SUPPORT started a mirror: %d", code)
	}
	if code, _ := f.do(t, "POST", "/admin/template-updates/sandbox", mirrorBody(tuNew, tuNew), "nobody"); code != http.StatusForbidden {
		t.Fatalf("a non-admin started a mirror: %d", code)
	}
	if code, _ := f.do(t, "POST", "/admin/template-updates/sandbox", mirrorBody(tuNew, tuOld), "admin-sub"); code != http.StatusBadRequest {
		t.Fatalf("an unconfirmed digest was accepted: %d", code)
	}
	// Not a published digest of the package (the client's digest is re-verified).
	other := "sha256:" + strings.Repeat("9", 64)
	if code, _ := f.do(t, "POST", "/admin/template-updates/sandbox", mirrorBody(other, other), "admin-sub"); code != http.StatusBadRequest {
		t.Fatalf("an unpublished digest was accepted: %d", code)
	}
	// The prepull already runs the offered digest: nothing to do (same-digest refusal).
	if code, body := f.do(t, "POST", "/admin/template-updates/sandbox", mirrorBody(tuOld, tuOld), "admin-sub"); code != http.StatusConflict || !strings.Contains(body, "already run") {
		t.Fatalf("same digest: %d %s", code, body)
	}
}

func TestSandboxMirrorEndToEnd(t *testing.T) {
	ctx := context.Background()
	f := newTUFixture(t, true)
	code, body := f.do(t, "POST", "/admin/template-updates/sandbox", mirrorBody(tuNew, tuNew), "admin-sub")
	if code != http.StatusOK {
		t.Fatalf("mirror: %d %s", code, body)
	}
	var env struct {
		Data templateupdates.SandboxReceipt `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &env)
	rec := env.Data
	if rec.Payload.Tag != tuTag || rec.Payload.Registry != tuReg || rec.RequestedBy != "admin-sub" || rec.Step != templateupdates.SbxQueued {
		t.Fatalf("receipt %+v", rec)
	}
	// Dedup per image: a second request while the first is queued is refused, and so is a rollback.
	if code, _ := f.do(t, "POST", "/admin/template-updates/sandbox", mirrorBody(tuNew, tuNew), "admin-sub"); code != http.StatusConflict {
		t.Fatalf("duplicate mirror: %d", code)
	}
	// The admin audit row carries the before/after snapshot, no secret.
	waitAudit(t, f.db, func(evs []pgdoc.M) bool {
		for _, e := range evs {
			b, _ := json.Marshal(e)
			if strings.Contains(string(b), "TEMPLATE_UPDATES") && strings.Contains(string(b), tuNew) {
				if strings.Contains(string(b), tuPAT) || strings.Contains(string(b), tuMirror) {
					t.Fatal("a secret reached the audit trail")
				}
				return true
			}
		}
		return false
	})
	// Run it.
	r := f.runner(30 * time.Second)
	if did, err := r.RunOnce(ctx); !did || err != nil {
		t.Fatalf("run: %v %v", did, err)
	}
	got, _ := f.store.Load(ctx, rec.ID)
	if got.Step != templateupdates.SbxSucceeded || got.VerifiedDigest != tuNew {
		t.Fatalf("receipt %+v", got)
	}
	if j, _ := f.jobs.Get(ctx, rec.ID); j.State != "succeeded" {
		t.Fatalf("job %+v", j)
	}
	ref := tuReg + "/library/sandbox-base:" + tuTag + "@" + tuNew
	if f.cluster.prepull != ref || len(f.cluster.pods) != 0 {
		t.Fatalf("prepull %s pods %v", f.cluster.prepull, f.cluster.pods)
	}
	pin, _ := f.store.GetPin(ctx, tuReg+"/library/sandbox-base")
	if pin == nil || pin.Digest != tuNew || pin.Actor != "admin-sub" || pin.MirrorJob != rec.ID || pin.Previous != tuReg+"/library/sandbox-base:"+tuOldTag+"@"+tuOld {
		t.Fatalf("pin %+v", pin)
	}
	// The state endpoint: waiting for prepull on 2/3 nodes, the template not switched yet, the images.yaml line.
	code, body = f.do(t, "GET", "/admin/template-updates/sandbox/state", "", "support-sub")
	if code != http.StatusOK || !strings.Contains(body, `"pulled":2`) || !strings.Contains(body, `"total":3`) || !strings.Contains(body, `"switched":false`) ||
		!strings.Contains(body, "  base: ghcr.io/mutex-solutions/sandbox-base:"+tuTag+"@"+tuNew) {
		t.Fatalf("state: %d %s", code, body)
	}
	// The installer's read: the pins of this registry.
	code, body = f.do(t, "GET", "/admin/template-updates/sandbox/pins?registry="+tuReg, "", "support-sub")
	if code != http.StatusOK || !strings.Contains(body, tuNew) || !strings.Contains(body, `"mirrorJob":"`+rec.ID+`"`) {
		t.Fatalf("pins: %d %s", code, body)
	}
	// Only the short-lived pull token reached the worker request (never the PAT); neither is persisted.
	if len(f.cluster.tokens) != 1 || f.cluster.tokens[0] != tuRegToken {
		t.Fatalf("worker tokens %v", f.cluster.tokens)
	}
	assertNoSecretPersisted(t, f.db)

	// Rollback re-points the prepull at the previous digest (still in the registry); the pin records it.
	rb := `{"serviceId":"` + tuService + `","name":"base","confirmDigest":"` + tuOld + `"}`
	if code, body = f.do(t, "POST", "/admin/template-updates/sandbox/rollback", rb, "support-sub"); code != http.StatusForbidden {
		t.Fatalf("SUPPORT rolled back: %d %s", code, body)
	}
	if code, body = f.do(t, "POST", "/admin/template-updates/sandbox/rollback", rb, "admin-sub"); code != http.StatusOK {
		t.Fatalf("rollback: %d %s", code, body)
	}
	if did, err := r.RunOnce(ctx); !did || err != nil {
		t.Fatalf("rollback run: %v %v", did, err)
	}
	if f.cluster.prepull != tuReg+"/library/sandbox-base:"+tuOldTag+"@"+tuOld || f.cluster.starts != 1 {
		t.Fatalf("after rollback: prepull %s, worker starts %d", f.cluster.prepull, f.cluster.starts)
	}
	pin, _ = f.store.GetPin(ctx, tuReg+"/library/sandbox-base")
	if pin.Action != templateupdates.SandboxRollback || pin.Digest != tuOld || pin.Previous != ref {
		t.Fatalf("pin after rollback %+v", pin)
	}
	assertNoSecretPersisted(t, f.db)
}

// A runner that loses its lease mid-job stops; a new acquisition resumes from the receipt and finishes without a
// second copy, and the stale acquisition can neither checkpoint nor complete.
func TestSandboxMirrorResumeAfterLostLease(t *testing.T) {
	ctx := context.Background()
	f := newTUFixture(t, true)
	if code, body := f.do(t, "POST", "/admin/template-updates/sandbox", mirrorBody(tuNew, tuNew), "admin-sub"); code != http.StatusOK {
		t.Fatalf("mirror: %d %s", code, body)
	}
	// Acquisition 1 claims, starts the worker, then its lease expires before it reads the result.
	j1, err := f.jobs.ClaimKinds(ctx, 300*time.Millisecond, []string{durablejobs.TemplateUpdateSandbox})
	if err != nil || j1 == nil {
		t.Fatalf("claim: %v %v", j1, err)
	}
	if err := f.jobs.Checkpoint(ctx, j1, func(tc context.Context) error {
		rec, _ := f.store.Load(tc, j1.ID)
		rec.Step, rec.Pod, rec.Epoch = templateupdates.SbxMirroring, sandbox.MirrorPodName(strings.ReplaceAll(j1.ID, "-", "")[:16], j1.Epoch), j1.Epoch
		return f.store.Save(tc, rec)
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = f.cluster.StartMirror(ctx, sandbox.MirrorRequest{JobShort: strings.ReplaceAll(j1.ID, "-", "")[:16], Epoch: j1.Epoch,
		Source: "ghcr.io/mutex-solutions/sandbox-base@" + tuNew, Destination: tuReg + "/library/sandbox-base:" + tuTag, SourceToken: tuRegToken})
	time.Sleep(400 * time.Millisecond)
	// Acquisition 2 (the runner) resumes: it adopts the existing worker's result, no second start.
	if did, err := f.runner(30 * time.Second).RunOnce(ctx); !did || err != nil {
		t.Fatalf("resume: %v %v", did, err)
	}
	rec, _ := f.store.Load(ctx, j1.ID)
	if rec.Step != templateupdates.SbxSucceeded || f.cluster.starts != 1 || rec.Epoch < 2 {
		t.Fatalf("resumed receipt %+v, starts %d", rec, f.cluster.starts)
	}
	// The stale acquisition is fenced out of every write.
	if err := f.jobs.Checkpoint(ctx, j1, nil); !errors.Is(err, durablejobs.ErrLeaseLost) {
		t.Fatalf("stale checkpoint: %v", err)
	}
	if err := f.jobs.Complete(ctx, j1, nil); !errors.Is(err, durablejobs.ErrLeaseLost) {
		t.Fatalf("stale complete: %v", err)
	}
	// Dead-job cleanup: a job that died mid-flight is re-acquired fenced, its pods removed, its receipt cleaned.
	if code, body := f.do(t, "POST", "/admin/template-updates/sandbox/rollback",
		`{"serviceId":"`+tuService+`","name":"base","confirmDigest":"`+tuOld+`"}`, "admin-sub"); code != http.StatusOK {
		t.Fatalf("rollback enqueue: %d %s", code, body)
	}
	var id string
	_ = f.db.Pool.QueryRow(ctx, `SELECT id::text FROM durable_job WHERE kind=$1 AND state='pending'`, durablejobs.TemplateUpdateSandbox).Scan(&id)
	if _, err := f.db.Pool.Exec(ctx, `UPDATE durable_job SET state='dead',finished_at=clock_timestamp(),last_error='lease_expired' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if ids, err := f.store.DeadUncleaned(ctx); err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("dead uncleaned %v %v", ids, err)
	}
	if did, err := f.runner(30 * time.Second).RunOnce(ctx); !did || err != nil {
		t.Fatalf("dead cleanup: %v %v", did, err)
	}
	dead, _ := f.store.Load(ctx, id)
	if !dead.Cleaned || dead.Step != templateupdates.SbxFailed || dead.Code != "job_dead" {
		t.Fatalf("dead receipt %+v", dead)
	}
	// A dead job no longer blocks a new request for the image.
	if ids, _ := f.store.DeadUncleaned(ctx); len(ids) != 0 {
		t.Fatalf("still listed %v", ids)
	}
}

// Concurrent mirror requests for one image produce exactly one job (advisory lock + dedup per image).
func TestSandboxMirrorConcurrentDedup(t *testing.T) {
	f := newTUFixture(t, true)
	out := make(chan int, 6)
	for i := 0; i < 6; i++ {
		go func() {
			code, _ := f.do(t, "POST", "/admin/template-updates/sandbox", mirrorBody(tuNew, tuNew), "admin-sub")
			out <- code
		}()
	}
	ok := 0
	for i := 0; i < 6; i++ {
		switch c := <-out; c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if n, _ := f.db.C(templateupdates.SandboxReceiptCollection).Count(context.Background(), nil); ok != 1 || n != 1 {
		t.Fatalf("%d accepted, %d receipts", ok, n)
	}
}

func assertNoSecretPersisted(t *testing.T, db *pgdoc.DB) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{`SELECT coalesce(string_agg(payload::text, ''), '') FROM durable_job`,
		`SELECT coalesce(string_agg(doc::text, ''), '') FROM "templateUpdateSandbox"`,
		`SELECT coalesce(string_agg(doc::text, ''), '') FROM "sandboxBuiltinPin"`,
		`SELECT coalesce(string_agg(doc::text, ''), '') FROM "auditEvent"`} {
		var all string
		if err := db.Pool.QueryRow(ctx, q).Scan(&all); err != nil && !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("%s: %v", q, err)
		}
		if strings.Contains(all, tuPAT) || strings.Contains(all, tuMirror) || strings.Contains(all, tuRegToken) {
			t.Fatalf("a secret was persisted (%s)", q)
		}
	}
}

func waitAudit(t *testing.T, db *pgdoc.DB, ok func([]pgdoc.M) bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		var evs []pgdoc.M
		_ = db.C("auditEvent").Find(context.Background(), nil, &evs)
		if ok(evs) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no audit event")
}

// DELETE /admin/template-updates/sandbox/pins/{id}: admin:service:manage, refused while a job for the image is live,
// audited, and afterwards the installer's read no longer returns the pin (images.yaml wins again). Works with the flag
// off too.
func TestSandboxPinDelete(t *testing.T) {
	ctx := context.Background()
	f := newTUFixture(t, true)
	if code, body := f.do(t, "POST", "/admin/template-updates/sandbox", mirrorBody(tuNew, tuNew), "admin-sub"); code != http.StatusOK {
		t.Fatalf("mirror: %d %s", code, body)
	}
	id := url.PathEscape(tuReg + "/library/sandbox-base")
	// No pin yet: 404.
	if code, _ := f.do(t, "DELETE", "/admin/template-updates/sandbox/pins/"+id, "", "admin-sub"); code != http.StatusNotFound {
		t.Fatalf("missing pin: %d", code)
	}
	if did, err := f.runner(30 * time.Second).RunOnce(ctx); !did || err != nil {
		t.Fatalf("run: %v %v", did, err)
	}
	// A live rollback job for the image blocks the delete.
	if code, body := f.do(t, "POST", "/admin/template-updates/sandbox/rollback",
		`{"serviceId":"`+tuService+`","name":"base","confirmDigest":"`+tuOld+`"}`, "admin-sub"); code != http.StatusOK {
		t.Fatalf("rollback: %d %s", code, body)
	}
	if code, body := f.do(t, "DELETE", "/admin/template-updates/sandbox/pins/"+id, "", "admin-sub"); code != http.StatusConflict {
		t.Fatalf("delete while busy: %d %s", code, body)
	}
	if did, err := f.runner(30 * time.Second).RunOnce(ctx); !did || err != nil {
		t.Fatalf("rollback run: %v %v", did, err)
	}
	if code, _ := f.do(t, "DELETE", "/admin/template-updates/sandbox/pins/"+id, "", "support-sub"); code != http.StatusForbidden {
		t.Fatalf("SUPPORT deleted a pin: %d", code)
	}
	if code, _ := f.do(t, "DELETE", "/admin/template-updates/sandbox/pins/"+url.PathEscape("t/x/y"), "", "admin-sub"); code != http.StatusBadRequest {
		t.Fatalf("malformed id: %d", code)
	}
	f.svc.SandboxEnabled = false // the delete stays available with the flag off
	if code, body := f.do(t, "DELETE", "/admin/template-updates/sandbox/pins/"+id, "", "admin-sub"); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	if code, body := f.do(t, "GET", "/admin/template-updates/sandbox/pins?registry="+tuReg, "", "support-sub"); code != http.StatusOK || !strings.Contains(body, `"pins":[]`) {
		t.Fatalf("pins after delete: %d %s", code, body)
	}
	waitAudit(t, f.db, func(evs []pgdoc.M) bool {
		for _, e := range evs {
			b, _ := json.Marshal(e)
			if strings.Contains(string(b), `"DELETE"`) && strings.Contains(string(b), "sandbox-base") {
				return true
			}
		}
		return false
	})
	assertNoSecretPersisted(t, f.db)
}

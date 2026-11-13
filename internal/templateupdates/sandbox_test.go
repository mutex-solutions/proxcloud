package templateupdates

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/sandboximport"
)

const (
	sbxJob      = "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"
	sbxReg      = "192.0.2.226"
	sbxPAT      = "ghp_never_persist_this_token_0000000000"
	sbxRegToken = "ghcr-short-lived-pull-token-0000"
)

var (
	sbxOld = "sha256:" + strings.Repeat("1", 64)
	sbxNew = "sha256:" + strings.Repeat("2", 64)
)

func sbxPayload() SandboxPayload {
	return SandboxPayload{Version: 1, Action: SandboxMirror, ServiceID: "svc-sbx", Name: "base", Registry: sbxReg, Tag: "20261112-9d00563", Digest: sbxNew}
}

// fakeCluster is an in-memory sandbox cluster: worker pods advance through phases, DaemonSets hold images.
type fakeCluster struct {
	mu        sync.Mutex
	pods      map[string]*sandbox.MirrorPod
	started   []sandbox.MirrorRequest
	deleted   []string
	prepull   map[string]string // "ds/container" → image
	finish    string            // phase a started pod reaches on its first read: Succeeded (default) | Failed | Running | Pending
	badRes    bool
	patches   int
	onStart   func()
	patchErr  error
	partial   bool // SetPrepullImage lands on template-prepull, then fails on the GPU DaemonSet
	deleteErr error
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{pods: map[string]*sandbox.MirrorPod{}, prepull: map[string]string{"template-prepull/p1": sbxReg + "/library/sandbox-base:20261101-aaaaaaa@" + sbxOld}}
}

func (c *fakeCluster) StartMirror(_ context.Context, req sandbox.MirrorRequest) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.onStart != nil {
		c.onStart()
	}
	c.started = append(c.started, req)
	name := sandbox.MirrorPodName(req.JobShort, req.Epoch)
	c.pods[name] = &sandbox.MirrorPod{Name: name, UID: "uid-" + name, Phase: "Pending", Source: req.Source, Dest: req.Destination}
	return "uid-" + name, nil
}
func (c *fakeCluster) GetMirrorPod(_ context.Context, _, name string) (*sandbox.MirrorPod, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pods[name]
	if p == nil {
		return nil, nil
	}
	if p.Phase == "Pending" && c.finish != "Pending" {
		p.Phase = c.finish
		if p.Phase == "" {
			p.Phase = "Succeeded"
		}
	}
	cp := *p
	return &cp, nil
}
func (c *fakeCluster) MirrorResult(_ context.Context, p *sandbox.MirrorPod) (*sandboximport.MirrorResult, error) {
	if c.badRes {
		return nil, errors.New("invalid")
	}
	_, d, _ := strings.Cut(p.Source, "@")
	return &sandboximport.MirrorResult{Source: p.Source, Destination: p.Dest, SourceDigest: d, DestinationDigest: d, Platform: "linux/amd64", SizeBytes: 42}, nil
}
func (c *fakeCluster) DeleteMirrorPod(_ context.Context, name, uid string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deleteErr != nil {
		return c.deleteErr
	}
	if p := c.pods[name]; p != nil && p.UID == uid {
		delete(c.pods, name)
		c.deleted = append(c.deleted, name)
	}
	return nil
}
func (c *fakeCluster) ListMirrorPods(context.Context, string) ([]sandbox.MirrorPod, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []sandbox.MirrorPod
	for _, p := range c.pods {
		out = append(out, *p)
	}
	return out, nil
}
func (c *fakeCluster) PrepullImages(_ context.Context, repo string) ([]sandbox.PrepullImage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []sandbox.PrepullImage
	for k, img := range c.prepull {
		if strings.HasPrefix(img, repo+":") || strings.HasPrefix(img, repo+"@") {
			ds, ct, _ := strings.Cut(k, "/")
			out = append(out, sandbox.PrepullImage{DaemonSet: ds, Container: ct, Image: img})
		}
	}
	return out, nil
}
func (c *fakeCluster) SetPrepullImage(_ context.Context, ref string) ([]sandbox.PrepullImage, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.patchErr != nil {
		return nil, false, c.patchErr
	}
	repo := ref[:strings.LastIndex(ref[:strings.Index(ref, "@")], ":")]
	if c.partial {
		for k, img := range c.prepull {
			if strings.HasPrefix(k, "template-prepull/") && strings.HasPrefix(img, repo+":") {
				c.prepull[k] = ref
			}
		}
		c.patches++
		return nil, true, errors.New("409 on template-prepull-gpu")
	}
	hit := false
	for k, img := range c.prepull {
		if strings.HasPrefix(img, repo+":") {
			c.prepull[k], hit = ref, true
		}
	}
	if !hit {
		c.prepull["template-prepull/p9"] = ref
	}
	c.patches++
	return nil, true, nil
}

func (c *fakeCluster) RestorePrepullImages(_ context.Context, current string, restore []sandbox.PrepullImage) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range restore {
		k := r.DaemonSet + "/" + r.Container
		if c.prepull[k] == current && r.Image != current {
			c.prepull[k] = r.Image
			n++
		}
	}
	return n, nil
}

type fakeReg struct {
	digest map[string]string // "repo@ref" → digest
	err    error
}

func (r *fakeReg) Resolve(_ context.Context, repo, ref string) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	d, ok := r.digest[repo+"@"+ref]
	if !ok {
		return "", ErrRegistryNotFound
	}
	return d, nil
}

type memSandbox struct {
	mu   sync.Mutex
	recs map[string]SandboxReceipt
	pins map[string]SandboxPin
}

func newMemSandbox() *memSandbox {
	return &memSandbox{recs: map[string]SandboxReceipt{}, pins: map[string]SandboxPin{}}
}
func (m *memSandbox) Load(_ context.Context, id string) (*SandboxReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	if !ok {
		return nil, nil
	}
	return &r, nil
}
func (m *memSandbox) Save(_ context.Context, r *SandboxReceipt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs[r.ID] = *r
	return nil
}
func (m *memSandbox) GetPin(_ context.Context, id string) (*SandboxPin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pins[id]
	if !ok {
		return nil, nil
	}
	return &p, nil
}
func (m *memSandbox) SavePin(_ context.Context, p *SandboxPin) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pins[p.ID] = *p
	return nil
}
func (m *memSandbox) DeletePin(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pins, id)
	return nil
}

// sbxJobs is a JobStore for TemplateUpdateSandbox; lossAfter counts fence checks before the lease is lost.
type sbxJobs struct {
	fakeJobs
	checks    int
	lossAfter int
}

func (s *sbxJobs) ClaimKinds(_ context.Context, _ time.Duration, kinds []string) (*durablejobs.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed || len(kinds) != 1 || kinds[0] != durablejobs.TemplateUpdateSandbox {
		return nil, nil
	}
	s.claimed = true
	j := *s.job
	return &j, nil
}

func (s *sbxJobs) Checkpoint(ctx context.Context, j *durablejobs.Job, fn func(context.Context) error) error {
	s.mu.Lock()
	s.checks++
	if s.lossAfter > 0 && s.checks > s.lossAfter {
		s.leaseLost = true
	}
	s.mu.Unlock()
	return s.fakeJobs.Checkpoint(ctx, j, fn)
}

func sbxFixture(t *testing.T, p SandboxPayload) (*SandboxRunner, *sbxJobs, *fakeCluster, *fakeReg, *memSandbox) {
	t.Helper()
	raw, _ := json.Marshal(p)
	jobs := &sbxJobs{fakeJobs: fakeJobs{job: &durablejobs.Job{ID: sbxJob, Kind: durablejobs.TemplateUpdateSandbox, Payload: raw, Attempts: 1, MaxAttempts: 3, Epoch: 1, State: "running"}}}
	cl, reg, mem := newFakeCluster(), &fakeReg{digest: map[string]string{"library/sandbox-" + p.Name + "@" + p.Tag: p.Digest}}, newMemSandbox()
	_ = mem.Save(context.Background(), &SandboxReceipt{ID: sbxJob, Kind: "sandbox", Payload: p, Step: SbxQueued, RequestedBy: "op-1"})
	r := &SandboxRunner{Store: jobs, Receipts: mem, Lease: 30 * time.Second,
		Build: func(context.Context, SandboxPayload) (*SandboxExecutor, error) {
			return &SandboxExecutor{Cluster: cl, Registry: reg, Receipts: mem, Pins: mem, Poll: time.Millisecond, PodWait: time.Second,
				SourceToken: func(_ context.Context, pkg string) (string, error) {
					if pkg != "sandbox-base" {
						return "", errors.New("wrong package")
					}
					return sbxRegToken, nil
				}}, nil
		}}
	return r, jobs, cl, reg, mem
}

func TestSandboxMirrorHappyPath(t *testing.T) {
	p := sbxPayload()
	r, jobs, cl, _, mem := sbxFixture(t, p)
	var finished []SandboxReceipt
	r.OnFinish = func(rec SandboxReceipt) { finished = append(finished, rec) }
	if did, err := r.RunOnce(context.Background()); !did || err != nil {
		t.Fatalf("run: %v %v", did, err)
	}
	rec := mem.recs[sbxJob]
	if !jobs.completed || rec.Step != SbxSucceeded || rec.VerifiedDigest != sbxNew || rec.SizeBytes != 42 {
		t.Fatalf("receipt %+v completed=%v", rec, jobs.completed)
	}
	if len(cl.started) != 1 || cl.started[0].Source != "ghcr.io/mutex-solutions/sandbox-base@"+sbxNew || cl.started[0].Destination != sbxReg+"/library/sandbox-base:20261112-9d00563" {
		t.Fatalf("worker %+v", cl.started)
	}
	if len(cl.pods) != 0 {
		t.Fatalf("worker pods (and their credential Secrets) outlived the job: %v", cl.pods)
	}
	if got := cl.prepull["template-prepull/p1"]; got != p.Ref() {
		t.Fatalf("prepull %s", got)
	}
	pin := mem.pins[p.Repo()]
	if pin.Digest != sbxNew || pin.Ref != p.Ref() || pin.Actor != "op-1" || pin.MirrorJob != sbxJob || pin.Previous != sbxReg+"/library/sandbox-base:20261101-aaaaaaa@"+sbxOld || pin.Template != "base" {
		t.Fatalf("pin %+v", pin)
	}
	if len(rec.Previous) != 1 || rec.Previous[0].Image != pin.Previous {
		t.Fatalf("previous %+v", rec.Previous)
	}
	if len(finished) != 1 || finished[0].Step != SbxSucceeded {
		t.Fatalf("OnFinish %+v", finished)
	}
	// Only the short-lived pull token reaches the worker request; neither it nor the PAT is persisted.
	all, _ := json.Marshal([]any{mem.recs, mem.pins, string(jobs.job.Payload)})
	if strings.Contains(string(all), sbxPAT) || strings.Contains(string(all), sbxRegToken) || cl.started[0].SourceToken != sbxRegToken {
		t.Fatal("a token was persisted, or the worker did not get the pull token")
	}
}

// The destination resolves to another digest (or not at all): permanent, nothing is pinned, the pin is untouched.
func TestSandboxMirrorRefusesAnotherDigest(t *testing.T) {
	for name, mut := range map[string]func(*fakeReg, *fakeCluster){
		"other digest": func(r *fakeReg, _ *fakeCluster) { r.digest["library/sandbox-base@20261112-9d00563"] = sbxOld },
		"missing":      func(r *fakeReg, _ *fakeCluster) { delete(r.digest, "library/sandbox-base@20261112-9d00563") },
		"refused":      func(r *fakeReg, _ *fakeCluster) { r.err = ErrRegistryRefused },
		"bad result":   func(_ *fakeReg, c *fakeCluster) { c.badRes = true },
	} {
		t.Run(name, func(t *testing.T) {
			p := sbxPayload()
			r, jobs, cl, reg, mem := sbxFixture(t, p)
			mem.pins[p.Repo()] = SandboxPin{ID: p.Repo(), Digest: sbxOld, Ref: "keep"}
			mut(reg, cl)
			_, _ = r.RunOnce(context.Background())
			rec := mem.recs[sbxJob]
			if rec.Step != SbxFailed || !jobs.permanent || jobs.completed || cl.patches != 0 {
				t.Fatalf("receipt %+v permanent=%v patches=%d", rec, jobs.permanent, cl.patches)
			}
			if mem.pins[p.Repo()].Ref != "keep" {
				t.Fatal("the pin record changed although nothing was pinned")
			}
			if len(cl.pods) != 0 || !rec.Cleaned {
				t.Fatalf("leftover pods %v cleaned=%v", cl.pods, rec.Cleaned)
			}
		})
	}
}

// Losing the lease at any fence check stops the executor before its next cluster effect.
func TestSandboxMirrorFencedAtEveryStep(t *testing.T) {
	p := sbxPayload()
	full, _, fullCl, _, _ := sbxFixture(t, p)
	_, _ = full.RunOnce(context.Background())
	_ = fullCl
	for n := 1; n < 12; n++ {
		r, jobs, cl, _, mem := sbxFixture(t, p)
		jobs.lossAfter = n
		started, patched := 0, 0
		cl.onStart = func() { started++ }
		_, _ = r.RunOnce(context.Background())
		patched = cl.patches
		rec := mem.recs[sbxJob]
		if jobs.completed && n < 8 {
			t.Fatalf("loss after %d checks: completed", n)
		}
		// Every effect has a preceding successful check: effects never exceed successful checks.
		if started+patched+len(cl.deleted) > n {
			t.Fatalf("loss after %d checks: %d starts %d patches %d deletes", n, started, patched, len(cl.deleted))
		}
		if jobs.failCode != "" && !jobs.completed {
			t.Fatalf("loss after %d: a lost acquisition must not fail/dead-letter the job (code %s)", n, jobs.failCode)
		}
		_ = rec
	}
}

// A new acquisition resumes from the receipt: a worker that already succeeded is not restarted; a verified job only
// patches the prepull.
func TestSandboxMirrorResume(t *testing.T) {
	p := sbxPayload()
	r, jobs, cl, _, mem := sbxFixture(t, p)
	name := sandbox.MirrorPodName(jobShort(sbxJob), 1)
	cl.pods[name] = &sandbox.MirrorPod{Name: name, UID: "u1", Phase: "Succeeded", Source: p.Source(), Dest: p.Destination()}
	rec := mem.recs[sbxJob]
	rec.Step, rec.Pod = SbxMirroring, name
	mem.recs[sbxJob] = rec
	jobs.job.Epoch = 2
	_, _ = r.RunOnce(context.Background())
	if len(cl.started) != 0 || mem.recs[sbxJob].Step != SbxSucceeded || len(cl.pods) != 0 {
		t.Fatalf("resume from a finished worker: started=%d step=%s pods=%v", len(cl.started), mem.recs[sbxJob].Step, cl.pods)
	}
	// A failed earlier worker is deleted and the copy redone at once under this epoch's own pod.
	r, jobs, cl, _, mem = sbxFixture(t, p)
	cl.pods[name] = &sandbox.MirrorPod{Name: name, UID: "u1", Phase: "Failed", Source: p.Source(), Dest: p.Destination()}
	rec = mem.recs[sbxJob]
	rec.Step, rec.Pod = SbxMirroring, name
	mem.recs[sbxJob] = rec
	jobs.job.Epoch = 2
	_, _ = r.RunOnce(context.Background())
	if len(cl.started) != 1 || cl.started[0].Epoch != 2 || mem.recs[sbxJob].Step != SbxSucceeded || mem.recs[sbxJob].Pod != sandbox.MirrorPodName(jobShort(sbxJob), 2) {
		t.Fatalf("a failed worker is redone under the new epoch: started=%+v step=%s", cl.started, mem.recs[sbxJob].Step)
	}
	if _, ok := cl.pods[name]; ok {
		t.Fatal("the failed worker was not deleted")
	}
}

// The prepull patch fails after the pin was recorded: retried; when the job dies, cleanup restores the pin it
// replaced (the DaemonSet never ran the new digest).
func TestSandboxMirrorPinRestoredWhenPatchNeverLanded(t *testing.T) {
	p := sbxPayload()
	r, jobs, cl, _, mem := sbxFixture(t, p)
	before := SandboxPin{ID: p.Repo(), Digest: sbxOld, Ref: sbxReg + "/library/sandbox-base:20261101-aaaaaaa@" + sbxOld, MirrorJob: "older"}
	mem.pins[p.Repo()] = before
	cl.patchErr = errors.New("409")
	jobs.job.Attempts = 3 // last attempt: terminal
	_, _ = r.RunOnce(context.Background())
	if mem.recs[sbxJob].Step != SbxFailed || mem.pins[p.Repo()].MirrorJob != "older" {
		t.Fatalf("step %s pin %+v", mem.recs[sbxJob].Step, mem.pins[p.Repo()])
	}
	// No pin before: the record this job wrote is removed again.
	r, jobs, cl, _, mem = sbxFixture(t, p)
	cl.patchErr = errors.New("409")
	jobs.job.Attempts = 3
	_, _ = r.RunOnce(context.Background())
	if _, ok := mem.pins[p.Repo()]; ok {
		t.Fatal("a pin was left although the prepull never ran the digest")
	}
}

func TestSandboxRollbackRepointsPrepull(t *testing.T) {
	p := sbxPayload()
	p.Action, p.Tag, p.Digest = SandboxRollback, "20261101-aaaaaaa", sbxOld
	r, _, cl, reg, mem := sbxFixture(t, p)
	cl.prepull["template-prepull/p1"] = sbxReg + "/library/sandbox-base:20261112-9d00563@" + sbxNew
	reg.digest = map[string]string{"library/sandbox-base@" + sbxOld: sbxOld}
	_, _ = r.RunOnce(context.Background())
	if mem.recs[sbxJob].Step != SbxSucceeded || cl.prepull["template-prepull/p1"] != p.Ref() || len(cl.started) != 0 {
		t.Fatalf("rollback: %s %s started=%d", mem.recs[sbxJob].Step, cl.prepull["template-prepull/p1"], len(cl.started))
	}
	if pin := mem.pins[p.Repo()]; pin.Action != SandboxRollback || pin.Digest != sbxOld || pin.Previous != sbxReg+"/library/sandbox-base:20261112-9d00563@"+sbxNew {
		t.Fatalf("pin %+v", pin)
	}
	// The target is gone from the registry: permanent, nothing patched.
	r, jobs, cl, reg, _ := sbxFixture(t, p)
	reg.digest = map[string]string{}
	_, _ = r.RunOnce(context.Background())
	if jobs.failCode != "rollback_target_missing" || cl.patches != 0 {
		t.Fatalf("missing target: %s patches=%d", jobs.failCode, cl.patches)
	}
}

func TestSandboxPayloadValidation(t *testing.T) {
	good := sbxPayload()
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(good)
	if _, err := DecodeSandboxPayload(append(raw[:len(raw)-1], []byte(`,"token":"x"}`)...)); err == nil {
		t.Fatal("unknown payload field accepted")
	}
	for name, mut := range map[string]func(*SandboxPayload){
		"action":   func(p *SandboxPayload) { p.Action = "delete" },
		"name":     func(p *SandboxPayload) { p.Name = "../x" },
		"tag":      func(p *SandboxPayload) { p.Tag = "latest" },
		"digest":   func(p *SandboxPayload) { p.Digest = "sha256:abc" },
		"registry": func(p *SandboxPayload) { p.Registry = "ghcr.io" },
		"reg path": func(p *SandboxPayload) { p.Registry = "a/b" },
		"service":  func(p *SandboxPayload) { p.ServiceID = "" },
	} {
		p := good
		mut(&p)
		if p.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if len(good.Key()) > 256 || good.Key() == (SandboxPayload{Version: 1, Action: SandboxRollback, ServiceID: good.ServiceID, Name: good.Name, Registry: good.Registry, Tag: good.Tag, Digest: good.Digest}).Key() {
		t.Fatal("key")
	}
	if got := ImagesYAMLLine("base", "20261112-9d00563", sbxNew); got != "  base: ghcr.io/mutex-solutions/sandbox-base:20261112-9d00563@"+sbxNew {
		t.Fatal(got)
	}
}

func TestRegistryReaderResolvesByBytes(t *testing.T) {
	body := []byte(`{"schemaVersion":2}`)
	var user string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, _ = r.BasicAuth()
		switch r.URL.Path {
		case "/v2/library/sandbox-base/manifests/good":
			_, _ = w.Write(body)
		case "/v2/library/sandbox-base/manifests/lying":
			w.Header().Set("Docker-Content-Digest", sbxOld)
			_, _ = w.Write(body)
		case "/v2/library/sandbox-base/manifests/denied":
			w.WriteHeader(403)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	rr := NewRegistryReader(strings.TrimPrefix(srv.URL, "http://"), "", "mirror", "pw", "http")
	if d, err := rr.Resolve(context.Background(), "library/sandbox-base", "good"); err != nil || !strings.HasPrefix(d, "sha256:") || user != "mirror" {
		t.Fatalf("good: %s %v %s", d, err, user)
	}
	if _, err := rr.Resolve(context.Background(), "library/sandbox-base", "lying"); err == nil {
		t.Fatal("a disagreeing digest header was trusted")
	}
	if _, err := rr.Resolve(context.Background(), "library/sandbox-base", "denied"); !errors.Is(err, ErrRegistryRefused) {
		t.Fatal(err)
	}
	if _, err := rr.Resolve(context.Background(), "library/sandbox-base", "nope"); !errors.Is(err, ErrRegistryNotFound) {
		t.Fatal(err)
	}
	if _, err := rr.Resolve(context.Background(), "t/p/x", "good"); err == nil {
		t.Fatal("a non-library path was read")
	}
	if strings.Contains(rr.String(), "pw") {
		t.Fatal("the reader prints its password")
	}
}

// One DaemonSet patched, the other refused: the landed containers are recorded and retried; a job that ends this way
// rolls the landed one back and restores the pin, so the DaemonSets never stay on different digests.
func TestSandboxPartialPrepullIsRolledBack(t *testing.T) {
	p := sbxPayload()
	r, jobs, cl, _, mem := sbxFixture(t, p)
	old := sbxReg + "/library/sandbox-base:20261101-aaaaaaa@" + sbxOld
	cl.prepull["template-prepull-gpu/p0"] = old
	cl.partial = true
	_, _ = r.RunOnce(context.Background())
	rec := mem.recs[sbxJob]
	if rec.Step != SbxPrepull || len(rec.Landed) != 1 || rec.Landed[0] != "template-prepull/p1" || jobs.failCode != "retryable" {
		t.Fatalf("first attempt: step=%s landed=%v code=%s", rec.Step, rec.Landed, jobs.failCode)
	}
	// Last attempt, still refused: terminal; the landed container goes back, the pin is restored (none before).
	r, jobs, cl2, _, _ := sbxFixture(t, p)
	cl2.prepull, cl2.partial = cl.prepull, true
	r.Receipts, jobs.job.Attempts = mem, 3
	r.Build = func(context.Context, SandboxPayload) (*SandboxExecutor, error) {
		return &SandboxExecutor{Cluster: cl2, Registry: &fakeReg{}, Receipts: mem, Pins: mem, Poll: time.Millisecond, PodWait: time.Second,
			SourceToken: func(context.Context, string) (string, error) { return sbxRegToken, nil }}, nil
	}
	_, _ = r.RunOnce(context.Background())
	if got := mem.recs[sbxJob]; got.Step != SbxFailed || !got.Cleaned {
		t.Fatalf("terminal: %+v", got)
	}
	if cl2.prepull["template-prepull/p1"] != old || cl2.prepull["template-prepull-gpu/p0"] != old {
		t.Fatalf("mixed prepull state left: %v", cl2.prepull)
	}
	if _, ok := mem.pins[p.Repo()]; ok {
		t.Fatal("the pin of a switch that was rolled back stayed")
	}
}

// A worker pod that stays Pending is replaced after the bound, not waited out; a resumed pod whose Secrets were
// never confirmed (PodReady false) is replaced at once; a failed pod delete lands in the receipt.
func TestSandboxPendingPodAndWarnings(t *testing.T) {
	p := sbxPayload()
	r, jobs, cl, _, mem := sbxFixture(t, p)
	cl.finish = "Pending"
	b := r.Build
	r.Build = func(ctx context.Context, q SandboxPayload) (*SandboxExecutor, error) {
		x, err := b(ctx, q)
		x.PendingLimit = 20 * time.Millisecond
		return x, err
	}
	_, _ = r.RunOnce(context.Background())
	if jobs.failCode != "retryable" || len(cl.pods) != 0 || len(cl.started) != 1 {
		t.Fatalf("pending pod: code=%s pods=%v started=%d", jobs.failCode, cl.pods, len(cl.started))
	}

	r, jobs, cl, _, mem = sbxFixture(t, p)
	name := sandbox.MirrorPodName(jobShort(sbxJob), 1)
	cl.pods[name] = &sandbox.MirrorPod{Name: name, UID: "u1", Phase: "Running", Source: p.Source(), Dest: p.Destination()}
	rec := mem.recs[sbxJob]
	rec.Step, rec.Pod, rec.PodReady = SbxMirroring, name, false
	mem.recs[sbxJob] = rec
	jobs.job.Epoch = 2
	_, _ = r.RunOnce(context.Background())
	if len(cl.started) != 1 || cl.started[0].Epoch != 2 || mem.recs[sbxJob].Step != SbxSucceeded {
		t.Fatalf("unconfirmed pod not replaced: started=%+v step=%s", cl.started, mem.recs[sbxJob].Step)
	}
	for _, d := range cl.deleted {
		if d == name {
			goto deleted
		}
	}
	t.Fatal("the unconfirmed pod was not deleted")
deleted:

	r, _, cl, _, mem = sbxFixture(t, p)
	cl.deleteErr = errors.New("api down")
	_, _ = r.RunOnce(context.Background())
	got := mem.recs[sbxJob]
	found := false
	for _, w := range got.Warnings {
		found = found || strings.Contains(w, "could not be deleted")
	}
	if got.Step != SbxSucceeded || !found {
		t.Fatalf("delete failure not recorded: step=%s warnings=%v", got.Step, got.Warnings)
	}
}

type orphanCluster struct {
	pods    []sandbox.MirrorPod
	deleted []string
}

func (o *orphanCluster) ListAllMirrorPods(context.Context) ([]sandbox.MirrorPod, error) {
	return o.pods, nil
}
func (o *orphanCluster) DeleteMirrorPod(_ context.Context, name, _ string) error {
	o.deleted = append(o.deleted, name)
	return nil
}

func TestSweepMirrorOrphans(t *testing.T) {
	oc := &orphanCluster{pods: []sandbox.MirrorPod{{Name: "tu-mirror-aaaaaaaaaaaaaaaa-e1", JobShort: "aaaaaaaaaaaaaaaa"},
		{Name: "tu-mirror-bbbbbbbbbbbbbbbb-e2", JobShort: "bbbbbbbbbbbbbbbb"}}}
	n, err := SweepMirrorOrphans(context.Background(), oc, func(_ context.Context, short string) (bool, error) { return short == "aaaaaaaaaaaaaaaa", nil })
	if err != nil || n != 1 || len(oc.deleted) != 1 || oc.deleted[0] != "tu-mirror-bbbbbbbbbbbbbbbb-e2" {
		t.Fatalf("swept %d %v %v", n, oc.deleted, err)
	}
}

// With the flag off the runner never claims a job, but the dead-job cleanup and the orphan sweep still run.
func TestSandboxRunnerCleanupOnly(t *testing.T) {
	p := sbxPayload()
	r, jobs, _, _, _ := sbxFixture(t, p)
	r.CleanupOnly = true
	swept := 0
	r.Orphans = func(context.Context) (int, error) { swept++; return 0, nil }
	if did, err := r.RunOnce(context.Background()); did || err != nil {
		t.Fatalf("cleanup-only run claimed work: %v %v", did, err)
	}
	if jobs.claimed || swept != 1 {
		t.Fatalf("claimed=%v swept=%d", jobs.claimed, swept)
	}
}

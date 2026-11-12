package templateupdates

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"

	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
)

// fakeJobs is an in-memory JobStore with one job and a switchable lease.
type fakeJobs struct {
	mu        sync.Mutex
	job       *durablejobs.Job
	claimed   bool
	leaseLost bool
	completed bool
	failCode  string
	permanent bool
	renewals  int
}

func (s *fakeJobs) ClaimKinds(_ context.Context, _ time.Duration, kinds []string) (*durablejobs.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed || len(kinds) != 1 || kinds[0] != durablejobs.TemplateUpdateVM {
		return nil, nil
	}
	s.claimed = true
	j := *s.job
	return &j, nil
}

func (s *fakeJobs) lost() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.leaseLost }

func (s *fakeJobs) Renew(context.Context, *durablejobs.Job, time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renewals++
	if s.leaseLost {
		return durablejobs.ErrLeaseLost
	}
	return nil
}

func (s *fakeJobs) Checkpoint(ctx context.Context, _ *durablejobs.Job, fn func(context.Context) error) error {
	if s.lost() {
		return durablejobs.ErrLeaseLost
	}
	if fn == nil {
		return nil
	}
	return fn(ctx)
}

func (s *fakeJobs) Complete(ctx context.Context, _ *durablejobs.Job, fn func(context.Context) error) error {
	if s.lost() {
		return durablejobs.ErrLeaseLost
	}
	if err := fn(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	s.completed = true
	s.mu.Unlock()
	return nil
}

func (s *fakeJobs) Fail(_ context.Context, _ *durablejobs.Job, _ time.Duration, permanent bool, code string) error {
	if s.lost() {
		return durablejobs.ErrLeaseLost
	}
	s.mu.Lock()
	s.failCode, s.permanent = code, permanent
	s.mu.Unlock()
	return nil
}

func runnerFixture(t *testing.T) (*Runner, *fakeJobs, *fakePVE, *memReceipts) {
	t.Helper()
	x, pv, _, rec, p := setup(t)
	raw, _ := json.Marshal(p)
	jobs := &fakeJobs{job: &durablejobs.Job{ID: testJob, Kind: durablejobs.TemplateUpdateVM, Payload: raw, Attempts: 1, MaxAttempts: 3, Epoch: 1, State: "running"}}
	r := &Runner{Store: jobs, Receipts: rec, Lease: 30 * time.Second,
		Build: func(context.Context, VMPayload) (*Executor, error) { return x, nil }}
	return r, jobs, pv, rec
}

func TestRunnerSuccessCompletes(t *testing.T) {
	r, jobs, pv, rec := runnerFixture(t)
	did, err := r.RunOnce(context.Background())
	if !did || err != nil {
		t.Fatalf("did %v err %v", did, err)
	}
	got, _ := rec.Load(context.Background(), testJob)
	if !jobs.completed || got.Step != StepSucceeded || pv.vms[9201] == nil {
		t.Fatalf("completed=%v receipt=%+v", jobs.completed, got)
	}
}

func TestRunnerPermanentFailureCleansOwnAndDeadLetters(t *testing.T) {
	r, jobs, pv, rec := runnerFixture(t)
	pv.importFail = true
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := rec.Load(context.Background(), testJob)
	if !jobs.permanent || jobs.failCode != "pve_import_refused" || got.Step != StepFailed || got.Code != "pve_import_refused" {
		t.Fatalf("jobs=%+v receipt=%+v", jobs, got)
	}
	if _, ok := pv.vms[got.VMID]; ok {
		t.Fatal("own half-created VM survived cleanup")
	}
	if pv.vms[9200] == nil {
		t.Fatal("existing template destroyed")
	}
}

// Lease lost mid-run (renewal refused): the runner stops, never completes, never dead-letters,
// and performs no cleanup (another acquisition owns the job).
func TestRunnerLostLeaseStopsWithoutCleanup(t *testing.T) {
	r, jobs, pv, _ := runnerFixture(t)
	pv.createHook = func() {
		jobs.mu.Lock()
		jobs.leaseLost = true
		jobs.mu.Unlock()
	}
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.completed || jobs.failCode != "" {
		t.Fatalf("a lost acquisition acknowledged the job: %+v", jobs)
	}
	for _, c := range pv.calls[len(pv.calls)-1:] {
		if c != "create 9201" {
			t.Fatalf("external work after the lease was lost: %v", pv.calls)
		}
	}
}

func TestRunnerRetryableFailureReschedules(t *testing.T) {
	r, jobs, _, _ := runnerFixture(t)
	r.Build = func(context.Context, VMPayload) (*Executor, error) { return nil, errors.New("github down") }
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.permanent || jobs.failCode != "executor_unavailable" {
		t.Fatalf("want a retry, got %+v", jobs)
	}
}

func TestRunnerInvalidPayloadIsPermanent(t *testing.T) {
	r, jobs, _, _ := runnerFixture(t)
	jobs.job.Payload = []byte(`{"version":2}`)
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !jobs.permanent || jobs.failCode != "invalid_payload" {
		t.Fatalf("got %+v", jobs)
	}
}

func TestWorkerRefusesExternalKinds(t *testing.T) {
	w := &durablejobs.Worker{Handlers: map[string]durablejobs.Handler{durablejobs.TemplateUpdateVM: func(context.Context, *durablejobs.Job) error { return nil }},
		Concurrency: 1, Poll: time.Second, Lease: time.Minute}
	if err := w.Run(context.Background()); err == nil {
		t.Fatal("the DB-only worker accepted an external-effect kind")
	}
}

// Item 10: the job key is a fixed-length hash, whatever the service id length.
func TestKeyIsBoundedHash(t *testing.T) {
	_, _, _, _, p := setup(t)
	p.ServiceID = strings.Repeat("s", 128)
	k := p.Key()
	if len(k) != len("tu-vm-")+64 || !strings.HasPrefix(k, "tu-vm-") {
		t.Fatalf("key %q", k)
	}
	q := p
	q.Node = "pve2"
	if q.Key() == k {
		t.Fatal("different node, same key")
	}
}

// Item 9: a busy upload lock makes the attempt retry without uploading.
func TestRunnerUploadLockBusyRetries(t *testing.T) {
	r, jobs, pv, _ := runnerFixture(t)
	var gotKey string
	r.UploadLock = func(_ context.Context, key string) (func(), bool, error) { gotKey = key; return nil, false, nil }
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.permanent || jobs.failCode != "retryable" || gotKey != "templateupdate-upload:svc-pve/pve1/nas-import" {
		t.Fatalf("jobs %+v key %q", jobs, gotKey)
	}
	for _, c := range pv.calls {
		if strings.HasPrefix(c, "upload") {
			t.Fatal("uploaded without the lock")
		}
	}
	released := 0
	r2, jobs2, _, _ := runnerFixture(t)
	r2.UploadLock = func(context.Context, string) (func(), bool, error) { return func() { released++ }, true, nil }
	if _, err := r2.RunOnce(context.Background()); err != nil || !jobs2.completed || released != 1 {
		t.Fatalf("with lock: completed=%v released=%d err=%v", jobs2.completed, released, err)
	}
}

// Item 5: a dead job whose cleanup never ran is re-acquired (fenced) and cleaned up; only its own
// unfinished VM and files go, and the receipt is marked cleaned.
func TestRunnerCleansUpDeadJobs(t *testing.T) {
	r, jobs, pv, rec := runnerFixture(t)
	jobs.claimed = true // nothing pending
	x, _ := r.Build(context.Background(), VMPayload{})
	// Simulate a job that died mid-import: its unfinished VM + file exist.
	_, _, _, _, p := setup(t)
	pv.vms[9201] = &fakeVM{node: "pve1", cfg: pve.VMConfig{"pool": "pc-templates", "tags": "proxcloud;" + OwnerTag(testJob)}}
	pv.files["nas-import:import/"+uploadName(testJob, 1, p.DiskDigest)] = 1
	rec.m[testJob] = Receipt{ID: testJob, Payload: p, Step: StepCreating, VMID: 9201, UploadFile: uploadName(testJob, 1, p.DiskDigest)}
	_ = x
	r.Dead = func(context.Context) ([]string, error) { return []string{testJob}, nil }
	r.ClaimDead = func(_ context.Context, id string, _ time.Duration) (*durablejobs.Job, error) {
		j := *jobs.job
		j.State, j.Attempts, j.Epoch = "running", j.MaxAttempts, 9
		return &j, nil
	}
	did, err := r.RunOnce(context.Background())
	if !did || err != nil {
		t.Fatalf("did %v err %v", did, err)
	}
	if _, ok := pv.vms[9201]; ok || len(pv.files) != 0 {
		t.Fatalf("leftovers not removed: %v %v", pv.vms, pv.files)
	}
	if pv.vms[9200] == nil {
		t.Fatal("existing template destroyed")
	}
	got, _ := rec.Load(context.Background(), testJob)
	if !got.Cleaned || got.Step != StepFailed || jobs.failCode != "cleaned_up" || !jobs.permanent {
		t.Fatalf("receipt %+v jobs %+v", got, jobs)
	}
}

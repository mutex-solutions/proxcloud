package templateupdates

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

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

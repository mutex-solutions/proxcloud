package templateupdates

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
)

// N2 on REAL PostgreSQL (TEMPLATEUPDATES_TEST_POSTGRES): a receipt already marked failed while its job row is still
// running (terminal() writes the failure, then runs the job's Cleanup) keeps the image busy for a new enqueue and for
// a pin delete (ErrSandboxBusy: HTTP 409); once the job is dead both are allowed, and the new job dedups again.
func TestPGSandboxBusyUntilTheJobIsSettled(t *testing.T) {
	db, jobs := pgFixture(t)
	ctx := context.Background()
	store := PGSandbox{DB: db}
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	s := &Service{Jobs: jobs, DB: db, Sandbox: store, SandboxEnabled: true}
	p := sbxPayload()
	noCheck := func(context.Context) error { return nil }
	first, err := s.enqueueSandbox(ctx, p, "admin", noCheck)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.enqueueSandbox(ctx, p, "admin", noCheck); !errors.Is(err, ErrSandboxBusy) {
		t.Fatalf("a queued job did not dedup: %v", err)
	}
	// The runner claims it and it fails permanently: terminal() records the failed receipt (fenced) and then runs
	// Cleanup, the job row still running.
	j, err := jobs.ClaimKinds(ctx, time.Minute, []string{durablejobs.TemplateUpdateSandbox})
	if err != nil || j == nil || j.ID != first.ID {
		t.Fatalf("claim %+v %v", j, err)
	}
	if err := jobs.Checkpoint(ctx, j, func(tc context.Context) error {
		r, err := store.Load(tc, j.ID)
		if err != nil || r == nil {
			return errors.New("receipt missing")
		}
		r.Step, r.Epoch, r.Code = SbxFailed, j.Epoch, "digest_mismatch"
		return store.Save(tc, r)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePin(ctx, &SandboxPin{ID: p.Repo(), Template: p.Name, Registry: p.Registry, Tag: p.Tag, Digest: p.Digest,
		Ref: p.Ref(), Action: p.Action, MirrorJob: "older", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.JobState(ctx, first.ID); st != "running" {
		t.Fatalf("job state %q", st)
	}
	if _, err := s.enqueueSandbox(ctx, p, "admin", noCheck); !errors.Is(err, ErrSandboxBusy) {
		t.Fatalf("an enqueue overlapped the failed job's cleanup: %v", err)
	}
	if _, err := s.DeleteSandboxPin(ctx, p.Repo()); !errors.Is(err, ErrSandboxBusy) {
		t.Fatalf("a pin delete overlapped the failed job's cleanup: %v", err)
	}
	if got, err := store.GetPin(ctx, p.Repo()); err != nil || got == nil {
		t.Fatalf("the pin went away while busy: %v", err)
	}
	// Dead-lettered but not cleaned up (its Cleanup could not run: the dead-job pass still owes it): still busy, or a
	// newer job's switch of the image could be undone by that late Cleanup.
	if err := jobs.Fail(ctx, j, 0, true, "digest_mismatch"); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.JobState(ctx, first.ID); st != "dead" {
		t.Fatalf("job state %q", st)
	}
	if ids, err := store.DeadUncleaned(ctx); err != nil || len(ids) != 1 || ids[0] != first.ID {
		t.Fatalf("the dead-job pass does not see the job: %v %v", ids, err)
	}
	if _, err := s.enqueueSandbox(ctx, p, "admin", noCheck); !errors.Is(err, ErrSandboxBusy) {
		t.Fatalf("an enqueue went ahead of a dead job's pending cleanup: %v", err)
	}
	if _, err := s.DeleteSandboxPin(ctx, p.Repo()); !errors.Is(err, ErrSandboxBusy) {
		t.Fatalf("a pin delete went ahead of a dead job's pending cleanup: %v", err)
	}
	// The dead-job pass cleaned it up: both are allowed again.
	r, err := store.Load(ctx, first.ID)
	if err != nil || r == nil {
		t.Fatalf("receipt %v", err)
	}
	r.Cleaned = true
	if err := store.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.DeleteSandboxPin(ctx, p.Repo()); err != nil || removed == nil || removed.ID != p.Repo() {
		t.Fatalf("pin delete after the job died: %+v %v", removed, err)
	}
	second, err := s.enqueueSandbox(ctx, p, "admin", noCheck)
	if err != nil || second.ID == first.ID {
		t.Fatalf("enqueue after the job died: %+v %v", second, err)
	}
	if _, err := s.enqueueSandbox(ctx, p, "admin", noCheck); !errors.Is(err, ErrSandboxBusy) {
		t.Fatalf("the new job did not dedup: %v", err)
	}
}

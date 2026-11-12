package templateupdates

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Opt-in REAL PostgreSQL tests (TEMPLATEUPDATES_TEST_POSTGRES = an explicit disposable loopback
// DSN; a throwaway database is created and dropped). They never start containers themselves.
func pgFixture(t *testing.T) (*pgdoc.DB, *durablejobs.Store) {
	t.Helper()
	dsn := os.Getenv("TEMPLATEUPDATES_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set TEMPLATEUPDATES_TEST_POSTGRES to a disposable loopback PostgreSQL DSN")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() != "127.0.0.1" || u.Port() == "" {
		t.Fatal("TEMPLATEUPDATES_TEST_POSTGRES must be an explicit loopback PostgreSQL URL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := "tu_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := pgdoc.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close(ctx)
		_, _ = admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		admin.Close()
	})
	jobs := durablejobs.New(db)
	if err := jobs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.C(ReceiptCollection).Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	return db, jobs
}

func enqueue(t *testing.T, db *pgdoc.DB, jobs *durablejobs.Store, p VMPayload) string {
	t.Helper()
	raw, _ := json.Marshal(p)
	var id string
	err := db.WithTx(context.Background(), func(tc context.Context) error {
		var err error
		id, err = jobs.EnqueueTx(tc, durablejobs.TemplateUpdateVM, p.Key(), raw, 3)
		if err != nil {
			return err
		}
		return PGReceipts{DB: db}.Save(tc, &Receipt{ID: id, Kind: "vm", Payload: p, Step: StepQueued, CreatedAt: time.Now().UTC()})
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Checkpoint commits only under a live lease; after expiry a fresh acquisition (new owner,
// higher epoch) takes over, and the stale acquisition's Checkpoint/Complete/Fail all fail and
// roll back their receipt writes.
func TestPGCheckpointFencesStaleAcquisition(t *testing.T) {
	db, jobs := pgFixture(t)
	ctx := context.Background()
	_, _, _, _, p := setup(t)
	id := enqueue(t, db, jobs, p)
	rec := PGReceipts{DB: db}

	j1, err := jobs.ClaimKinds(ctx, 300*time.Millisecond, []string{durablejobs.TemplateUpdateVM})
	if err != nil || j1 == nil || j1.ID != id {
		t.Fatalf("claim: %+v %v", j1, err)
	}
	if err := jobs.Checkpoint(ctx, j1, func(tc context.Context) error {
		r, _ := rec.Load(tc, id)
		r.Step, r.Epoch = StepUploading, j1.Epoch
		return rec.Save(tc, r)
	}); err != nil {
		t.Fatalf("live checkpoint: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	// Expired: the stale acquisition is refused and its write rolled back.
	err = jobs.Checkpoint(ctx, j1, func(tc context.Context) error {
		r, _ := rec.Load(tc, id)
		r.Step = StepTemplated
		return rec.Save(tc, r)
	})
	if !errors.Is(err, durablejobs.ErrLeaseLost) {
		t.Fatalf("expired checkpoint: %v", err)
	}
	if r, _ := rec.Load(ctx, id); r.Step != StepUploading {
		t.Fatalf("stale write committed: %+v", r)
	}
	j2, err := jobs.ClaimKinds(ctx, time.Minute, []string{durablejobs.TemplateUpdateVM})
	if err != nil || j2 == nil || j2.Epoch <= j1.Epoch || j2.Owner == j1.Owner {
		t.Fatalf("re-claim: %+v %v", j2, err)
	}
	if err := jobs.Renew(ctx, j1, time.Minute); !errors.Is(err, durablejobs.ErrLeaseLost) {
		t.Fatalf("stale renew: %v", err)
	}
	if err := jobs.Fail(ctx, j1, 0, true, "stale"); !errors.Is(err, durablejobs.ErrLeaseLost) {
		t.Fatalf("stale fail: %v", err)
	}
	if err := jobs.Complete(ctx, j1, nil); !errors.Is(err, durablejobs.ErrLeaseLost) {
		t.Fatalf("stale complete: %v", err)
	}
	// The live acquisition proceeds and fails permanently with a fixed code.
	if err := jobs.Checkpoint(ctx, j2, nil); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Fail(ctx, j2, 0, true, "digest_mismatch"); err != nil {
		t.Fatal(err)
	}
	got, _ := jobs.Get(ctx, id)
	if got.State != "dead" || got.LastError != "digest_mismatch" {
		t.Fatalf("final: %+v", got)
	}
	if err := jobs.Fail(ctx, j2, 0, true, "Not A Code: secret"); err == nil {
		t.Fatal("free-form failure text accepted")
	}
}

// End to end on PostgreSQL: the runner drives the fake PVE through the real store, a lost
// lease mid-run stops it, and a second acquisition resumes and completes.
func TestPGRunnerResumesAfterLeaseLoss(t *testing.T) {
	db, jobs := pgFixture(t)
	ctx := context.Background()
	x, pv, blobs, _, p := setup(t)
	x.Receipts = PGReceipts{DB: db}
	id := enqueue(t, db, jobs, p)

	// First acquisition: expire its lease (from another session) right inside CreateVM.
	pv.createHook = func() {
		if _, err := db.Pool.Exec(ctx, `UPDATE durable_job SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, id); err != nil {
			t.Error(err)
		}
	}
	r := &Runner{Store: jobs, Receipts: PGReceipts{DB: db}, Lease: 10 * time.Second,
		Build: func(context.Context, VMPayload) (*Executor, error) { return x, nil }}
	if did, err := r.RunOnce(ctx); !did || err != nil {
		t.Fatalf("first: %v %v", did, err)
	}
	mutationsAtLoss := pv.mutations
	j, _ := jobs.Get(ctx, id)
	if j.State != "running" {
		t.Fatalf("a lost acquisition changed the job: %+v", j)
	}
	pv.createHook = nil
	// Second acquisition (the lease expired): resumes from the receipt, no second download.
	if did, err := r.RunOnce(ctx); !did || err != nil {
		t.Fatalf("second: %v %v", did, err)
	}
	if blobs.opens != 1 {
		t.Fatalf("downloaded %d times", blobs.opens)
	}
	j, _ = jobs.Get(ctx, id)
	got, _ := PGReceipts{DB: db}.Load(ctx, id)
	if j.State != "succeeded" || got.Step != StepSucceeded || j.Epoch < 2 {
		t.Fatalf("job %+v receipt %+v", j, got)
	}
	// Exactly one finished template, and no unfinished VM of this job left behind (the empty VM
	// the lost acquisition created was re-read as ours and replaced).
	finishedN, leftovers := 0, 0
	for _, vm := range pv.vms {
		if vm.cfg.Str("name") == TemplateName(p) && vm.template {
			finishedN++
		}
		if strings.Contains(vm.cfg.Str("tags"), OwnerTag(id)) {
			leftovers++
		}
	}
	if finishedN != 1 || leftovers != 0 || pv.mutations <= mutationsAtLoss {
		t.Fatalf("finished %d leftovers %d, calls %v", finishedN, leftovers, pv.calls)
	}
}

// pgService builds a Service whose registry is a fake serving the golden "os" artifact.
func pgService(t *testing.T, db *pgdoc.DB, jobs *durablejobs.Store) (*Service, VMRequest) {
	t.Helper()
	raw, sums, disk := golden(t, "os")
	reg := &fakeRegistry{}
	reg.add("proxcloud-vm-debian-13", dg('1'), "2026-11-11T01:00:00Z", "debian-13-os-20261111000000-aaaaaaaaaaaa", artifactManifest(raw, sums, disk, false), raw, sums)
	s := &Service{Token: func(context.Context) (string, string, error) { return "ghp_x", "i", nil }, Jobs: jobs, DB: db,
		Receipts: PGReceipts{DB: db}, VMEnabled: true, newRegistry: func(string) (Registry, error) { return reg, nil }}
	req := VMRequest{Package: "proxcloud-vm-debian-13", Digest: dg('1'), ConfirmDigest: dg('1'), ServiceID: strings.Repeat("svc", 40),
		Node: "pve1", ImportStorage: "nas-import", DiskStorage: "local-zfs"}
	return s, req
}

// Items 5, 6, 10: enqueue with a 120-char service id (hashed key), duplicates refused, concurrent
// enqueues produce exactly one job, and a DEAD job (receipt never marked failed) does not block a
// new attempt.
func TestPGEnqueueDedupAndDead(t *testing.T) {
	db, jobs := pgFixture(t)
	ctx := context.Background()
	s, req := pgService(t, db, jobs)
	if err := db.C(ReceiptCollection).Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	// Concurrent enqueues of the same artifact/node: exactly one wins.
	type res struct {
		r   *Receipt
		err error
	}
	out := make(chan res, 6)
	for i := 0; i < 6; i++ {
		go func() { r, err := s.EnqueueVM(ctx, req, "admin"); out <- res{r, err} }()
	}
	won := 0
	var first *Receipt
	for i := 0; i < 6; i++ {
		x := <-out
		if x.err == nil {
			won++
			first = x.r
		} else if !errors.Is(x.err, ErrDuplicate) {
			t.Fatalf("unexpected error: %v", x.err)
		}
	}
	if won != 1 {
		t.Fatalf("%d concurrent enqueues won", won)
	}
	// The job dies (lease expired on its last attempt) without the receipt being marked failed.
	if _, err := db.Pool.Exec(ctx, `UPDATE durable_job SET state='dead',finished_at=clock_timestamp(),last_error='lease_expired' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if r, _ := (PGReceipts{DB: db}).Load(ctx, first.ID); r.Step != StepQueued {
		t.Fatalf("receipt %+v", r)
	}
	second, err := s.EnqueueVM(ctx, req, "admin")
	if err != nil || second.ID == first.ID {
		t.Fatalf("a dead job blocked a new attempt: %v", err)
	}
	if _, err := s.EnqueueVM(ctx, req, "admin"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("live second job not deduplicated: %v", err)
	}
	// The dead one is listed for cleanup; the pending one is not.
	ids, err := PGReceipts{DB: db}.DeadUncleaned(ctx)
	if err != nil || len(ids) != 1 || ids[0] != first.ID {
		t.Fatalf("dead uncleaned %v %v", ids, err)
	}
}

// ClaimDead re-acquires a dead job fenced (new owner/epoch), never as a normal run, and a crash
// mid-cleanup expires it straight back to dead; Replay still works afterwards.
func TestPGClaimDeadIsFencedAndNeverRuns(t *testing.T) {
	db, jobs := pgFixture(t)
	ctx := context.Background()
	_, _, _, _, p := setup(t)
	id := enqueue(t, db, jobs, p)
	if _, err := db.Pool.Exec(ctx, `UPDATE durable_job SET state='dead',attempts=1,finished_at=clock_timestamp(),last_error='lease_expired' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	j, err := jobs.ClaimDead(ctx, id, 200*time.Millisecond)
	if err != nil || j == nil || j.State != "running" || j.Attempts != j.MaxAttempts {
		t.Fatalf("claim dead: %+v %v", j, err)
	}
	if again, _ := jobs.ClaimDead(ctx, id, time.Minute); again != nil {
		t.Fatal("a running cleanup was claimed twice")
	}
	if err := jobs.Checkpoint(ctx, j, nil); err != nil {
		t.Fatalf("fenced checkpoint: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // the cleanup "crashes": its lease expires
	if n, err := jobs.ClaimKinds(ctx, time.Minute, []string{durablejobs.TemplateUpdateVM}); err != nil || n != nil {
		t.Fatalf("an expired cleanup acquisition became a normal run: %+v %v", n, err)
	}
	got, _ := jobs.Get(ctx, id)
	if got.State != "dead" || got.LastError != "lease_expired" {
		t.Fatalf("after crash: %+v", got)
	}
	if err := jobs.Checkpoint(ctx, j, nil); !errors.Is(err, durablejobs.ErrLeaseLost) {
		t.Fatalf("stale cleanup acquisition still fenced in: %v", err)
	}
	if err := jobs.Replay(ctx, id, "admin", "operator retry after cleanup", func(*durablejobs.Job) error { return nil }); err != nil {
		t.Fatalf("replay: %v", err)
	}
}

// Item 9: the upload lock is exclusive across sessions and released by its release func.
func TestPGUploadAdvisoryLock(t *testing.T) {
	db, _ := pgFixture(t)
	ctx := context.Background()
	rel, ok, err := db.TryAdvisoryLock(ctx, "templateupdate-upload:s/n/st")
	if err != nil || !ok {
		t.Fatalf("first: %v %v", ok, err)
	}
	if _, ok2, err := db.TryAdvisoryLock(ctx, "templateupdate-upload:s/n/st"); err != nil || ok2 {
		t.Fatalf("second acquired the held lock: %v %v", ok2, err)
	}
	if rel3, ok3, _ := db.TryAdvisoryLock(ctx, "templateupdate-upload:s/n/other"); !ok3 {
		t.Fatal("another storage's lock was blocked")
	} else {
		rel3()
	}
	rel()
	rel() // idempotent
	rel4, ok4, err := db.TryAdvisoryLock(ctx, "templateupdate-upload:s/n/st")
	if err != nil || !ok4 {
		t.Fatalf("after release: %v %v", ok4, err)
	}
	rel4()
}

// The runner's dead-job pass on PostgreSQL: cleans the leftovers of a dead job and marks it.
func TestPGRunnerCleansDeadJob(t *testing.T) {
	db, jobs := pgFixture(t)
	ctx := context.Background()
	x, pv, _, _, p := setup(t)
	x.Receipts = PGReceipts{DB: db}
	id := enqueue(t, db, jobs, p)
	pv.vms[9201] = &fakeVM{node: "pve1", cfg: pve.VMConfig{"pool": "pc-templates", "tags": "proxcloud;" + OwnerTag(id)}}
	pv.vms[9202] = &fakeVM{node: "pve1", cfg: pve.VMConfig{"pool": "pc-tenant", "tags": "proxcloud;" + OwnerTag(id)}} // a clone
	rec := PGReceipts{DB: db}
	r0, _ := rec.Load(ctx, id)
	r0.Step, r0.VMID = StepCreating, 9201
	_ = rec.Save(ctx, r0)
	if _, err := db.Pool.Exec(ctx, `UPDATE durable_job SET state='dead',finished_at=clock_timestamp(),last_error='lease_expired' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: jobs, Receipts: rec, Lease: 10 * time.Second, Dead: rec.DeadUncleaned, ClaimDead: jobs.ClaimDead,
		Build: func(context.Context, VMPayload) (*Executor, error) { return x, nil }}
	if did, err := r.RunOnce(ctx); !did || err != nil {
		t.Fatalf("cleanup pass: %v %v", did, err)
	}
	if _, ok := pv.vms[9201]; ok {
		t.Fatal("dead job's unfinished VM survived")
	}
	if _, ok := pv.vms[9202]; !ok {
		t.Fatal("a clone outside pc-templates was destroyed")
	}
	got, _ := rec.Load(ctx, id)
	j, _ := jobs.Get(ctx, id)
	if !got.Cleaned || got.Step != StepFailed || j.State != "dead" || j.LastError != "cleaned_up" {
		t.Fatalf("receipt %+v job %+v", got, j)
	}
	if ids, _ := rec.DeadUncleaned(ctx); len(ids) != 0 {
		t.Fatalf("still listed: %v", ids)
	}
	if did, _ := r.RunOnce(ctx); did {
		t.Fatal("cleaned job processed again")
	}
}

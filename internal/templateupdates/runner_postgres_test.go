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
	own := 0
	for _, vm := range pv.vms {
		if strings.Contains(vm.cfg.Str("tags"), OwnerTag(id)) {
			own++
		}
	}
	if own != 1 || pv.mutations <= mutationsAtLoss {
		t.Fatalf("own VMs %d, calls %v", own, pv.calls)
	}
}

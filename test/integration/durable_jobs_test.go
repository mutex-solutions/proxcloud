//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/chargefanout"
)

func queueFixture(t *testing.T) (*pgdoc.DB, *durablejobs.Store) {
	t.Helper()
	db := freshPG(t)
	q := durablejobs.New(db)
	if err := q.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db, q
}
func enqueueProbe(t *testing.T, q *durablejobs.Store, key string, max int) string {
	t.Helper()
	id, err := q.Enqueue(context.Background(), durablejobs.Selftest, key, []byte(`{"version":1}`), max)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func claimJob(t *testing.T, q *durablejobs.Store, ttl time.Duration) *durablejobs.Job {
	t.Helper()
	j, err := q.Claim(context.Background(), ttl)
	if err != nil || j == nil {
		t.Fatalf("claim=%+v %v", j, err)
	}
	return j
}
func expireJob(t *testing.T, db *pgdoc.DB, id string) {
	t.Helper()
	if _, err := db.Pool.Exec(context.Background(), `UPDATE durable_job SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
}

func TestDurableJobsClaimUniqueAcrossTwoReplicas(t *testing.T) {
	ctx := context.Background()
	db, q := queueFixture(t)
	q2 := durablejobs.New(db)
	const count = 24
	for i := 0; i < count; i++ {
		enqueueProbe(t, q, fmt.Sprint(i), 3)
	}
	seen := sync.Map{}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, store := range []*durablejobs.Store{q, q2} {
		wg.Go(func() {
			for {
				j, err := store.Claim(ctx, time.Minute)
				if err != nil {
					errs <- err
					return
				}
				if j == nil {
					return
				}
				if _, loaded := seen.LoadOrStore(j.ID, true); loaded {
					errs <- fmt.Errorf("job claimed twice: %s", j.ID)
					return
				}
				if err = store.Complete(ctx, j, nil); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	n := 0
	seen.Range(func(_, _ any) bool { n++; return true })
	if n != count {
		t.Fatalf("claims=%d want%d", n, count)
	}
}

func TestDurableJobsLeaseEpochAllTransitionsFenced(t *testing.T) {
	ctx := context.Background()
	db, q := queueFixture(t)
	id := enqueueProbe(t, q, "fence", 5)
	a := claimJob(t, q, time.Minute)
	if err := q.Renew(ctx, a, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	expireJob(t, db, id)
	// Expired acquisitions must not revive themselves even before a successor.
	if err := q.Renew(ctx, a, time.Minute); !errors.Is(err, durablejobs.ErrLeaseLost) {
		t.Fatalf("expired renew=%v", err)
	}
	b := claimJob(t, q, time.Minute)
	if a.Owner == b.Owner || b.Epoch <= a.Epoch {
		t.Fatalf("ownership not unique/monotonic: %+v %+v", a, b)
	}
	called := false
	for _, err := range []error{q.Complete(ctx, a, func(context.Context) error { called = true; return nil }), q.Retry(ctx, a, 0, false), q.Renew(ctx, a, time.Minute)} {
		if !errors.Is(err, durablejobs.ErrLeaseLost) {
			t.Fatalf("old-owner CAS=%v", err)
		}
	}
	if called {
		t.Fatal("old owner executed business handler")
	}
	if err := q.Complete(ctx, b, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDurableJobsExpiryDuringTransactionRollsBackBusiness(t *testing.T) {
	ctx := context.Background()
	db, q := queueFixture(t)
	enqueueProbe(t, q, "expires-in-tx", 3)
	if err := db.C("business").Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	j := claimJob(t, q, 30*time.Millisecond)
	err := q.Complete(ctx, j, func(tc context.Context) error {
		if _, err := db.C("business").InsertOne(tc, pgdoc.M{"_id": "must-rollback"}); err != nil {
			return err
		}
		tx, _ := db.Tx(tc)
		_, err := tx.Exec(tc, `SELECT pg_sleep(0.06)`)
		return err
	})
	if !errors.Is(err, durablejobs.ErrLeaseLost) {
		t.Fatalf("expired completion=%v", err)
	}
	if n, _ := db.C("business").Count(ctx, nil); n != 0 {
		t.Fatal("expired owner committed business write")
	}
	if err = q.Complete(ctx, claimJob(t, q, time.Minute), nil); err != nil {
		t.Fatal(err)
	}
}

func TestDurableJobsRetryDeadLetterCrashAndAuditedReplay(t *testing.T) {
	ctx := context.Background()
	db, q := queueFixture(t)
	id := enqueueProbe(t, q, "retry", 2)
	a := claimJob(t, q, time.Minute)
	if err := q.Retry(ctx, a, 0, false); err != nil {
		t.Fatal(err)
	}
	b := claimJob(t, q, time.Minute)
	expireJob(t, db, id) // process died on final attempt
	if b.Attempts != 2 {
		t.Fatal(b.Attempts)
	}
	if j, err := q.Claim(ctx, time.Minute); err != nil || j != nil {
		t.Fatalf("crash poison not bounded: %+v %v", j, err)
	}
	dead, err := q.Get(ctx, id)
	if err != nil || dead.State != "dead" {
		t.Fatalf("dead=%+v %v", dead, err)
	}
	if err = q.Replay(ctx, id, "operator", "repair validated", func(*durablejobs.Job) error { return errors.New("invalid") }); !errors.Is(err, durablejobs.ErrReplay) {
		t.Fatal(err)
	}
	if err = q.Replay(ctx, id, "operator", "repair validated", func(*durablejobs.Job) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var auditN int
	if err = db.Pool.QueryRow(ctx, `SELECT count(*) FROM durable_job_replay WHERE job_id=$1 AND actor='operator'`, id).Scan(&auditN); err != nil || auditN != 1 {
		t.Fatalf("audit=%d %v", auditN, err)
	}
	c := claimJob(t, q, time.Minute)
	if c.Epoch <= b.Epoch || c.Attempts != 1 {
		t.Fatalf("replay reset fence: %+v", c)
	}
	if err = q.Complete(ctx, c, nil); err != nil {
		t.Fatal(err)
	}
	if err = q.Replay(ctx, id, "operator", "must reject done", func(*durablejobs.Job) error { return nil }); !errors.Is(err, durablejobs.ErrReplay) {
		t.Fatal(err)
	}
}

func TestDurableJobsOutboxSameBusinessTransaction(t *testing.T) {
	ctx := context.Background()
	db, q := queueFixture(t)
	if err := db.C("business").Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := q.EnqueueTx(ctx, durablejobs.Selftest, "outside", []byte(`{}`), 3); err == nil {
		t.Fatal("independent outbox tx accepted")
	}
	boom := errors.New("rollback")
	var id string
	write := func(tc context.Context) error {
		if _, err := db.C("business").InsertOne(tc, pgdoc.M{"_id": "accepted"}); err != nil {
			return err
		}
		var err error
		id, err = q.EnqueueTx(tc, durablejobs.Selftest, "outbox", []byte(`{"version":1}`), 3)
		return err
	}
	if err := db.WithTx(ctx, func(tc context.Context) error {
		if err := write(tc); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if j, err := q.Claim(ctx, time.Minute); err != nil || j != nil {
		t.Fatalf("rolled-back job visible %+v %v", j, err)
	}
	if n, _ := db.C("business").Count(ctx, nil); n != 0 {
		t.Fatal("business rollback failed")
	}
	if err := db.WithTx(ctx, func(tc context.Context) error {
		if err := write(tc); err != nil {
			return err
		}
		if j, err := q.Claim(ctx, time.Minute); err != nil || j != nil {
			t.Fatalf("uncommitted outbox visible: %+v %v", j, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	j := claimJob(t, q, time.Minute)
	if j.ID != id {
		t.Fatal("different committed job")
	}
	if err := q.Complete(ctx, j, func(tc context.Context) error {
		_, err := db.C("business").SetByID(tc, "accepted", pgdoc.M{"processed": true}, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var business pgdoc.M
	if ok, err := db.C("business").Get(ctx, "accepted", &business); err != nil || !ok || business["processed"] != true {
		t.Fatalf("committed worker=%+v %v", business, err)
	}
}

func TestDurableJobsDuplicatePublishAndFrozenPayload(t *testing.T) {
	ctx := context.Background()
	_, q := queueFixture(t)
	var group sync.WaitGroup
	ids := make(chan string, 10)
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		group.Go(func() {
			id, err := q.Enqueue(ctx, durablejobs.Selftest, "dup", []byte(`{"version":1}`), 3)
			ids <- id
			errs <- err
		})
	}
	group.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("duplicate IDs %s %s", first, id)
		}
	}
	if _, err := q.Enqueue(ctx, durablejobs.Selftest, "dup", []byte(`{"version":2}`), 3); !errors.Is(err, durablejobs.ErrConflict) {
		t.Fatalf("frozen payload overwritten: %v", err)
	}
}

func TestDurableJobsRealWorkersSelftestAndPoison(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, q := queueFixture(t)
	w := &durablejobs.Worker{Store: q, Handlers: map[string]durablejobs.Handler{durablejobs.Selftest: chargefanout.SelftestHandler}, Concurrency: 2, Poll: 10 * time.Millisecond, Lease: time.Second}
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	probeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	if _, err := chargefanout.Selftest(probeCtx, q); err != nil {
		t.Fatal(err)
	}
	poison := enqueueProbe(t, q, "not-a-real-selftest-nonce", 3)
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		j, err := q.Get(ctx, poison)
		if err != nil {
			t.Fatal(err)
		}
		if j.State == "dead" {
			if j.Attempts != 1 {
				t.Fatalf("poison retried: %d", j.Attempts)
			}
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("poison not dead")
		case <-tick.C:
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not cancel gracefully")
	}
}

func TestDurableJobsExternalModePreservesNativeBacklog(t *testing.T) {
	ctx := context.Background()
	db, q := queueFixture(t)
	s, c, _ := durableBillingFixture(t, db)
	body, _ := json.Marshal(c)
	id, err := q.Enqueue(ctx, durablejobs.Billing, c.Key(), body, 8)
	if err != nil {
		t.Fatal(err)
	}
	if j, err := q.ClaimKinds(ctx, time.Minute, []string{durablejobs.Selftest}); err != nil || j != nil {
		t.Fatalf("external claimed native job: %+v %v", j, err)
	}
	pending, err := q.Get(ctx, id)
	if err != nil || pending.State != "pending" || pending.Attempts != 0 {
		t.Fatalf("native backlog consumed: %+v %v", pending, err)
	}
	j := claimJob(t, q, time.Minute)
	if err = q.Complete(ctx, j, func(tc context.Context) error { return chargefanout.Handler(s)(tc, j) }); err != nil {
		t.Fatal(err)
	}
	assertDurableBill(t, db, c)
}

func TestDurableJobsCrossDatabaseTxRejectedAtEveryBoundary(t *testing.T) {
	ctx := context.Background()
	a, q := queueFixture(t)
	var b *pgdoc.DB
	t.Run("other-database", func(t *testing.T) {
		b = freshPG(t)
		for _, db := range []*pgdoc.DB{a, b} {
			if err := db.C("business").Ensure(ctx); err != nil {
				t.Fatal(err)
			}
		}
		err := a.WithTx(ctx, func(tc context.Context) error {
			if _, err := q.EnqueueTx(tc, durablejobs.Selftest, "wrong-db", []byte(`{}`), 3); err != nil {
				return err
			}
			if _, err := b.C("business").InsertOne(tc, pgdoc.M{"_id": "wrong"}); err == nil {
				t.Fatal("wrong DB insert accepted")
			}
			if _, err := b.C("business").SetByID(tc, "wrong", pgdoc.M{"v": true}, nil); err == nil {
				t.Fatal("wrong DB update accepted")
			}
			var got pgdoc.M
			if _, err := b.C("business").Get(tc, "wrong", &got); err == nil {
				t.Fatal("wrong DB read accepted")
			}
			return b.LockXact(tc, "wrong")
		})
		if err == nil {
			t.Fatal("cross DB transaction accepted")
		}
		for _, db := range []*pgdoc.DB{a, b} {
			if n, _ := db.C("business").Count(ctx, nil); n != 0 {
				t.Fatal("cross DB business committed")
			}
		}
		if j, err := q.Claim(ctx, time.Minute); err != nil || j != nil {
			t.Fatalf("wrong DB outbox committed: %+v %v", j, err)
		}
	})
}

func TestDurableJobsBillingCrashAfterCommitBeforeAck(t *testing.T) {
	ctx := context.Background()
	db, q := queueFixture(t)
	s, c, _ := durableBillingFixture(t, db)
	body, _ := json.Marshal(c)
	id, err := q.Enqueue(ctx, durablejobs.Billing, c.Key(), body, 8)
	if err != nil {
		t.Fatal(err)
	}
	a := claimJob(t, q, time.Minute)
	// Deliberately commit the real billing business receipt without queue ACK,
	// reproducing a crash boundary that the ordinary atomic worker closes.
	if err = s.ChargeProfileByID(ctx, c.ProfileID, c.TimeUnit, c.CycleAt); err != nil {
		t.Fatal(err)
	}
	expireJob(t, db, id)
	b := claimJob(t, q, time.Minute)
	var oldEffects atomic.Int64
	if err = q.Complete(ctx, a, func(tc context.Context) error { oldEffects.Add(1); return nil }); !errors.Is(err, durablejobs.ErrLeaseLost) {
		t.Fatal(err)
	}
	if oldEffects.Load() != 0 {
		t.Fatal("old worker admitted")
	}
	if err = q.Complete(ctx, b, func(tc context.Context) error { return chargefanout.Handler(s)(tc, b) }); err != nil {
		t.Fatal(err)
	}
	assertDurableBill(t, db, c)
	// Queue payload pruning may not prune the independent financial receipt.
	if _, err = db.Pool.Exec(ctx, `UPDATE durable_job SET finished_at=clock_timestamp()-interval '8 days' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if n, err := q.Prune(ctx, 7*24*time.Hour); err != nil || n != 1 {
		t.Fatalf("prune=%d %v", n, err)
	}
	if _, err = q.Enqueue(ctx, durablejobs.Billing, c.Key(), body, 8); err != nil {
		t.Fatal(err)
	}
	again := claimJob(t, q, time.Minute)
	if err = q.Complete(ctx, again, func(tc context.Context) error { return chargefanout.Handler(s)(tc, again) }); err != nil {
		t.Fatal(err)
	}
	assertDurableBill(t, db, c)
}

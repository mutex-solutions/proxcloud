//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
)

// Admission has to recheck DB time AFTER a row-lock wait. A statement whose
// owner predicate was true when it started cannot renew/retry/ACK after expiry.
func TestDurableJobsLockWaitCannotAdmitExpiredOwner(t *testing.T) {
	for _, op := range []string{"complete", "renew", "retry"} {
		t.Run(op, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			db, q := queueFixture(t)
			id := enqueueProbe(t, q, "lock-wait", 4)
			j := claimJob(t, q, 2*time.Second)
			blocker, err := db.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err = blocker.Exec(ctx, `SELECT id FROM durable_job WHERE id=$1 FOR UPDATE`, id); err != nil {
				t.Fatal(err)
			}
			called := false
			result := make(chan error, 1)
			go func() {
				switch op {
				case "complete":
					result <- q.Complete(ctx, j, func(context.Context) error { called = true; return nil })
				case "renew":
					result <- q.Renew(ctx, j, time.Minute)
				case "retry":
					result <- q.Retry(ctx, j, 0, false)
				}
			}()
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			// This is an observed PG lock barrier, not a scheduling sleep guessing
			// whether the losing transition reached the row before lease expiry.
			for {
				var waiting bool
				if err = db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE '%SELECT lease_until FROM durable_job%')`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case err := <-result:
					t.Fatalf("transition did not wait: %v", err)
				case <-ctx.Done():
					t.Fatal("no lock wait observed")
				case <-tick.C:
				}
			}
			// The losing statement is now waiting with the old, initially valid
			// acquisition. Advance its row to expired while retaining the lock.
			if _, err = blocker.Exec(ctx, `UPDATE durable_job SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			if err = blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-result:
				if !errors.Is(err, durablejobs.ErrLeaseLost) {
					t.Fatalf("%s after lock wait=%v", op, err)
				}
			case <-ctx.Done():
				t.Fatal("transition hung")
			}
			if called {
				t.Fatal("expired Complete invoked handler after waiting")
			}
			if err = q.Complete(ctx, claimJob(t, q, time.Minute), nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

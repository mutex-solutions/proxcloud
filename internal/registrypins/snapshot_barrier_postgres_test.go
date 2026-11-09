package registrypins

import (
	"context"
	"testing"
	"time"
)

// Runtime consumers and snapshot producers cannot have the same valid exact pin
// set. This test proves actual global operation lock participation instead of
// pretending they are equally eligible contenders with incompatible roles.
func TestSnapshotAndRuntimePostgresGlobalUUIDLockBarrier(t *testing.T) {
	for _, kind := range []string{"snapshot", "runtime"} {
		t.Run(kind, func(t *testing.T) {
			db, a, b := importPGFixture(t)
			op := snapshotPGOperation()
			spec := snapshotTestSpec()
			runtime := runtimeTestSpec()
			if kind == "runtime" {
				importPGPins(t, a, op, runtime.Image)
			} else {
				importPGPins(t, a, op, spec.Base, spec.Destination)
			}
			tx, e := db.Pool.Begin(t.Context())
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(context.Background())
			if _, e = tx.Exec(t.Context(), `SELECT pg_advisory_xact_lock(hashtext($1))`, "registry-dispatch-op:"+op.ID); e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() {
				var e error
				if kind == "runtime" {
					_, e = b.AuthorizeRuntimeDispatch(context.Background(), op, runtime)
				} else {
					_, e = b.AuthorizeSnapshotDispatch(context.Background(), op, spec)
				}
				done <- e
			}()
			deadline := time.After(10 * time.Second)
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			waiting := false
			for !waiting {
				select {
				case <-deadline:
					t.Fatal("global UUID waiter absent")
				case <-tick.C:
					var n int
					if e = db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted`).Scan(&n); e != nil {
						t.Fatal(e)
					}
					waiting = n > 0
				}
			}
			select {
			case e := <-done:
				t.Fatal("authorization bypassed global lock", e)
			default:
			}
			if e = tx.Commit(t.Context()); e != nil {
				t.Fatal(e)
			}
			if e = <-done; e != nil {
				t.Fatal("valid uncontended authorizer after release", e)
			}
		})
	}
}

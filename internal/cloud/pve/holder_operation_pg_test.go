//go:build integration

package pve

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func holderPG(t *testing.T) *pgdoc.DB {
	t.Helper()
	ctx := context.Background()
	p, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("holder"), tcpostgres.WithUsername("holder"), tcpostgres.WithPassword("holder"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(p) })
	dsn, err := p.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	db, err := pgdoc.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })
	return db
}

// Return the PID only AFTER PostgreSQL reports the operation waiting on our row.
// This barrier makes the expiry test fail with a pre-row-lock clock predicate.
func waitHolderDBLock(t *testing.T, db *pgdoc.DB) int32 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		var pid int32
		err := db.Pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%operationLease%' AND query LIKE '%FOR UPDATE%' LIMIT 1`).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("operation never reached PostgreSQL row-lock wait")
			return 0
		case <-time.After(time.Millisecond):
		}
	}
}
func TestHolderPostgresLeaseAndDurableFence(t *testing.T) {
	db := holderPG(t)
	ctx := context.Background()
	r := NewHolderOperationRepo(db)
	if err := r.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	l := lock.NewLeases(db.Pool)
	t.Run("two simultaneous owners and old owner CAS", func(t *testing.T) {
		var wg sync.WaitGroup
		results := make(chan *lock.Lease, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				x, e := l.Acquire(ctx, "two", time.Minute)
				if e != nil {
					t.Error(e)
				}
				results <- x
			}()
		}
		wg.Wait()
		close(results)
		var first *lock.Lease
		wins := 0
		for x := range results {
			if x != nil {
				wins++
				first = x
			}
		}
		if wins != 1 {
			t.Fatalf("owners=%d", wins)
		}
		if _, e := db.Pool.Exec(ctx, `UPDATE "operationLease" SET until_at=clock_timestamp()-interval '1 second' WHERE name='two'`); e != nil {
			t.Fatal(e)
		}
		second, e := l.Acquire(ctx, "two", time.Minute)
		if e != nil || second == nil || second.Owner == first.Owner || second.Epoch <= first.Epoch {
			t.Fatalf("successor=%+v %v", second, e)
		}
		for name, call := range map[string]func() error{"renew": func() error { return l.Renew(ctx, *first, time.Minute) }, "check": func() error { return l.Check(ctx, *first) }, "release": func() error { return l.Release(ctx, *first) }} {
			if e := call(); !errors.Is(e, lock.ErrLeaseLost) {
				t.Fatalf("old %s=%v", name, e)
			}
		}
		if e := l.Check(ctx, *second); e != nil {
			t.Fatal("old release freed successor", e)
		}
	})
	t.Run("crash after intent and lost UPID reply survive TTL", func(t *testing.T) {
		x, e := r.Acquire(ctx, "crash", time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		op, e := r.Begin(ctx, *x, holderProof{Kind: "move", Pool: "pool", Source: holderVMIdentity{Node: "pve1", VMID: 101, UUID: "uuid=owned"}})
		if e != nil {
			t.Fatal(e)
		}
		// Worker disappears here, including before/after an unacknowledged POST.
		if _, e = db.Pool.Exec(ctx, `UPDATE "operationLease" SET until_at=clock_timestamp()-interval '1 day' WHERE name='crash'`); e != nil {
			t.Fatal(e)
		}
		r2 := NewHolderOperationRepo(db)
		next, e := r2.Acquire(ctx, "crash", time.Minute)
		if e != nil || next == nil {
			t.Fatal(e)
		}
		if _, e = r2.Begin(ctx, *next, holderProof{}); e == nil {
			t.Fatal("new writer bypassed durable intent")
		}
		p, e := r2.Pending(ctx, "crash")
		if e != nil || p == nil || p.ID != op.ID || p.UPID != "" {
			t.Fatalf("pending=%+v %v", p, e)
		}
		if e = r.RecordUPID(ctx, *x, op.ID, UPID(moveUPID)); !errors.Is(e, lock.ErrLeaseLost) {
			t.Fatalf("old owner stored reply: %v", e)
		}
		if e = r.Settle(ctx, *x, op, "SUCCEEDED"); !errors.Is(e, lock.ErrLeaseLost) {
			t.Fatalf("old owner settled: %v", e)
		}
		if e = r.Check(ctx, *next); e == nil {
			t.Fatal("unknown no-UPID cleared")
		}
	})
	t.Run("known UPID settlement exactly once not dispatch", func(t *testing.T) {
		x, e := r.Acquire(ctx, "known", time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		op, e := r.Begin(ctx, *x, holderProof{Kind: "config"})
		if e != nil {
			t.Fatal(e)
		}
		op.UPID = UPID(moveUPID)
		if e = r.RecordUPID(ctx, *x, op.ID, op.UPID); e != nil {
			t.Fatal(e)
		}
		// Simulate process crash; poll-only successor must retain the same UPID.
		_, e = db.Pool.Exec(ctx, `UPDATE "operationLease" SET until_at=clock_timestamp()-interval '1 second' WHERE name='known'`)
		if e != nil {
			t.Fatal(e)
		}
		next, e := r.Acquire(ctx, "known", time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		var wg sync.WaitGroup
		settled := make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); settled <- r.Settle(ctx, *next, op, "SUCCEEDED") }()
		}
		wg.Wait()
		close(settled)
		wins := 0
		for e := range settled {
			if e == nil {
				wins++
			} else if !errors.Is(e, lock.ErrLeaseLost) {
				t.Error(e)
			}
		}
		if wins != 1 {
			t.Fatalf("settled=%d", wins)
		}
	})
	t.Run("lock-only waits may not revive expired owners", func(t *testing.T) {
		for _, name := range []string{"checktx", "renew", "release"} {
			x, e := l.Acquire(ctx, name, time.Second)
			if e != nil {
				t.Fatal(e)
			}
			block, e := db.Pool.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			var until time.Time
			if e = block.QueryRow(ctx, `SELECT until_at FROM "operationLease" WHERE name=$1 FOR UPDATE`, name).Scan(&until); e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() {
				switch name {
				case "checktx":
					done <- pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error { return l.CheckTx(ctx, tx, *x) })
				case "renew":
					done <- l.Renew(ctx, *x, time.Minute)
				case "release":
					done <- l.Release(ctx, *x)
				}
			}()
			waitHolderDBLock(t, db)
			// Database sleep, not client-clock arithmetic. No row update => no EPQ.
			if _, e = block.Exec(ctx, `SELECT pg_sleep(1.1)`); e != nil {
				t.Fatal(e)
			}
			if e = block.Commit(ctx); e != nil {
				t.Fatal(e)
			}
			if e = <-done; !errors.Is(e, lock.ErrLeaseLost) {
				t.Fatalf("%s resurrected expired lease: %v", name, e)
			}
		}
	})
	t.Run("database partition and backend death cannot clear fence", func(t *testing.T) {
		x, e := r.Acquire(ctx, "partition", time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		op, e := r.Begin(ctx, *x, holderProof{})
		if e != nil {
			t.Fatal(e)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if e = r.RecordUPID(cancelled, *x, op.ID, UPID(moveUPID)); e == nil {
			t.Fatal("cancelled DB save succeeded")
		}
		block, e := db.Pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		var until time.Time
		if e = block.QueryRow(ctx, `SELECT until_at FROM "operationLease" WHERE name='partition' FOR UPDATE`).Scan(&until); e != nil {
			t.Fatal(e)
		}
		done := make(chan error, 1)
		go func() { done <- r.RecordUPID(ctx, *x, op.ID, UPID(moveUPID)) }()
		pid := waitHolderDBLock(t, db) // the ACTUAL reply-save transaction
		if _, e = db.Pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); e != nil {
			t.Fatal(e)
		}
		if e = <-done; e == nil {
			t.Fatal("terminated reply-save transaction succeeded")
		}
		if e = block.Rollback(ctx); e != nil {
			t.Fatal(e)
		}
		p, e := r.Pending(ctx, "partition")
		if e != nil || p == nil || p.ID != op.ID || p.UPID != "" {
			t.Fatalf("durable pending lost: %+v %v", p, e)
		}
		if e = r.Check(ctx, *x); e == nil {
			t.Fatal("partition cleared pending")
		}
	})
}

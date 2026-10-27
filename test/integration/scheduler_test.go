//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
	"github.com/mutex-solutions/proxcloud/internal/platform/scheduler"
)

// TestSchedulerRunLocked verifies the cron lock guard: a job whose ShedLock is held by
// another worker is skipped; once the lock frees, it runs.
func TestSchedulerRunLocked(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	l := lock.New(db)
	s := scheduler.New(l)

	now := time.Now().UTC().Truncate(time.Millisecond)
	runs := 0
	fn := func(context.Context) { runs++ }

	// a fleet-mate holds the lock (lockUntil = now+5m).
	if ok, err := l.Lock(ctx, "charge", 5*time.Minute, now); err != nil || !ok {
		t.Fatalf("seed lock: %v / %v", ok, err)
	}

	// blocked while held.
	if ran, _ := s.RunLocked(ctx, "charge", 5*time.Minute, 0, now.Add(time.Second), fn); ran || runs != 0 {
		t.Fatalf("expected skip while locked: ran=%v runs=%d", ran, runs)
	}
	// runs once the lock has expired.
	if ran, err := s.RunLocked(ctx, "charge", 5*time.Minute, 0, now.Add(10*time.Minute), fn); err != nil || !ran || runs != 1 {
		t.Fatalf("expected run after expiry: ran=%v runs=%d err=%v", ran, runs, err)
	}
	// a distinct, never-locked job runs immediately.
	if ran, _ := s.RunLocked(ctx, "sendBill", 5*time.Minute, 0, now.Add(10*time.Minute), fn); !ran || runs != 2 {
		t.Fatalf("expected independent job to run: ran=%v runs=%d", ran, runs)
	}

	// a PANICKING job is contained by RunLocked (an unguarded panic in a cron fn crashes the
	// whole API pod — the prod midnight-bill outage) and still releases its lock.
	boom := func(context.Context) { panic("poison doc") }
	if ran, err := s.RunLocked(ctx, "monthlyBill", 5*time.Minute, 0, now, boom); err != nil || !ran {
		t.Fatalf("expected panicking job to be contained: ran=%v err=%v", ran, err)
	}
	if ran, err := s.RunLocked(ctx, "monthlyBill", 5*time.Minute, 0, now.Add(time.Second), fn); err != nil || !ran || runs != 3 {
		t.Fatalf("expected lock released after panic: ran=%v runs=%d err=%v", ran, runs, err)
	}
}

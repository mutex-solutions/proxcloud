//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
)

// TestShedLock verifies the distributed lock (ShedLock): first acquire wins, a held lock blocks, and
// lockAtLeastFor keeps it held past unlock until the minimum interval elapses.
func TestShedLock(t *testing.T) {
	ctx := context.Background()
	l := lock.New(freshPG(t))
	now := time.Now().UTC().Truncate(time.Millisecond)

	// 1. first acquire (upsert) wins.
	if ok, err := l.Lock(ctx, "charge", 5*time.Minute, now); err != nil || !ok {
		t.Fatalf("first lock: ok=%v err=%v", ok, err)
	}
	// 2. while held (lockUntil=now+5m), a second worker is blocked.
	if ok, err := l.Lock(ctx, "charge", 5*time.Minute, now.Add(time.Second)); err != nil || ok {
		t.Fatalf("held lock should block: ok=%v err=%v", ok, err)
	}
	// 3. release with lockAtLeastFor=30s (acquired at `now`).
	if err := l.Unlock(ctx, "charge", 30*time.Second, now, now.Add(2*time.Second)); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	// 4. still blocked before now+30s (lockAtLeastFor floor).
	if ok, _ := l.Lock(ctx, "charge", 5*time.Minute, now.Add(10*time.Second)); ok {
		t.Fatal("lockAtLeastFor should still block at now+10s")
	}
	// 5. acquirable after the floor (now+40s > now+30s).
	if ok, err := l.Lock(ctx, "charge", 5*time.Minute, now.Add(40*time.Second)); err != nil || !ok {
		t.Fatalf("re-lock after lockAtLeastFor: ok=%v err=%v", ok, err)
	}
	// 6. a different lock name is independent.
	if ok, err := l.Lock(ctx, "sendBill", 5*time.Minute, now.Add(40*time.Second)); err != nil || !ok {
		t.Fatalf("independent lock: ok=%v err=%v", ok, err)
	}
}

// TestShedLockWait: Wait gives up when the lock stays busy, and its release lets the next caller in.
func TestShedLockWait(t *testing.T) {
	ctx := context.Background()
	l := lock.New(freshPG(t))
	release, ok, err := l.Wait(ctx, "w", time.Minute, 0)
	if err != nil || !ok {
		t.Fatalf("free lock: ok=%v err=%v", ok, err)
	}
	if _, ok, err := l.Wait(ctx, "w", time.Minute, time.Second); err != nil || ok {
		t.Fatalf("busy lock: ok=%v err=%v", ok, err)
	}
	go func() { time.Sleep(600 * time.Millisecond); release() }()
	release2, ok, err := l.Wait(ctx, "w", time.Minute, 10*time.Second)
	if err != nil || !ok {
		t.Fatalf("after release: ok=%v err=%v", ok, err)
	}
	release2()
}

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
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

// TestShedLockReleaseOnlyItsOwn: a holder past atMostFor has lost the lock, and its late release must not free the next
// owner's. A second lock shows the same on what Wait returns, with the times it really uses.
func TestShedLockReleaseOnlyItsOwn(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	l := lock.New(db)
	// A zone and nanoseconds, as a caller may pass them: an owner must still recognise its own lock.
	first := time.Now().In(time.FixedZone("x", 3600)).Add(-time.Hour)
	if ok, err := l.Lock(ctx, "a", time.Minute, first); err != nil || !ok {
		t.Fatalf("first owner: ok=%v err=%v", ok, err)
	}
	if err := l.Unlock(ctx, "a", 0, first, first.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if ok, err := l.Lock(ctx, "a", time.Minute, first.Add(2*time.Second)); err != nil || !ok {
		t.Fatalf("an owner's release must free it: ok=%v err=%v", ok, err)
	}

	// The lock from first+2s expires; a second owner takes it, on the same host.
	second := first.Add(5 * time.Minute)
	if ok, err := l.Lock(ctx, "a", 30*time.Minute, second); err != nil || !ok {
		t.Fatalf("second owner: ok=%v err=%v", ok, err)
	}
	// The first owner finishes late.
	if err := l.Unlock(ctx, "a", 0, first.Add(2*time.Second), second.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if ok, err := l.Lock(ctx, "a", time.Minute, second.Add(2*time.Second)); err != nil || ok {
		t.Fatalf("a late release must not free the next owner's lock: ok=%v err=%v", ok, err)
	}
	// Nor does the second owner's own release go missing.
	if err := l.Unlock(ctx, "a", 0, second, second.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if ok, err := l.Lock(ctx, "a", time.Minute, second.Add(4*time.Second)); err != nil || !ok {
		t.Fatalf("the owner's release must free it: ok=%v err=%v", ok, err)
	}

	// Another replica's lock, taken at the same instant, is not this one's either.
	if _, err := db.C("shedLock").SetByID(ctx, "a", pgdoc.M{"lockedBy": "other-host"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := l.Unlock(ctx, "a", 0, second.Add(4*time.Second), second.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if ok, err := l.Lock(ctx, "a", time.Minute, second.Add(6*time.Second)); err != nil || ok {
		t.Fatalf("another host's release must not free it: ok=%v err=%v", ok, err)
	}

	short, ok, err := l.Wait(ctx, "w", time.Millisecond, 0)
	if err != nil || !ok {
		t.Fatalf("wait: ok=%v err=%v", ok, err)
	}
	time.Sleep(50 * time.Millisecond)
	next, ok, err := l.Wait(ctx, "w", time.Minute, 2*time.Second)
	if err != nil || !ok {
		t.Fatalf("after expiry: ok=%v err=%v", ok, err)
	}
	short() // late
	if _, ok, err := l.Wait(ctx, "w", time.Minute, 0); err != nil || ok {
		t.Fatalf("Wait's late release must not free the next owner's lock: ok=%v err=%v", ok, err)
	}
	next()
	if rel, ok, err := l.Wait(ctx, "w", time.Minute, time.Second); err != nil || !ok {
		t.Fatalf("Wait's own release must free it: ok=%v err=%v", ok, err)
	} else {
		rel()
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

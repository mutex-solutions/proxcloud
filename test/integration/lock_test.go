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

// TestShedLockHoldOutlivesItsLeaseAndADeadHolderDoesNot is N2-3 of the v0.11.0-rc.1 drill at the lock: a cluster's
// converge took its lock for the longest a pass can take (20 minutes), the API was restarted in the middle of one, and
// the lock stayed held for all of it. A lock taken with Hold lasts a lease: a holder that works longer keeps it by
// extending it, and one that died (its context ended, its release never ran) loses it one lease later.
func TestShedLockHoldOutlivesItsLeaseAndADeadHolderDoesNot(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	const lease = 900 * time.Millisecond
	holder, other := lock.New(db), lock.New(db)

	alive, die := context.WithCancel(ctx)
	defer die()
	release, ok, err := holder.Hold(alive, "lbc-c1", lease, 0)
	if err != nil || !ok {
		t.Fatalf("hold: ok=%v err=%v", ok, err)
	}
	// Three leases of work: nobody else gets in.
	for end := time.Now().Add(3 * lease); time.Now().Before(end); time.Sleep(lease / 4) {
		if _, ok, err := other.Hold(ctx, "lbc-c1", lease, 0); err != nil || ok {
			t.Fatalf("a working holder lost its lock after its first lease: ok=%v err=%v", ok, err)
		}
	}
	// The holder dies. Its lock is free within one lease and a margin, not after the 20 minutes its work could take.
	die()
	died := time.Now()
	taken, ok, err := other.Hold(ctx, "lbc-c1", lease, 3*lease)
	if err != nil || !ok {
		t.Fatalf("a dead holder's lock was not taken over within three leases: ok=%v err=%v", ok, err)
	}
	if waited := time.Since(died); waited > 2*lease {
		t.Fatalf("the dead holder's lock lasted %s, want about one lease (%s)", waited, lease)
	}
	// The dead holder's release, should it still run, frees nothing of the new owner's.
	release()
	if _, ok, _ := holder.Hold(ctx, "lbc-c1", lease, 0); ok {
		t.Fatal("a late release of the dead holder freed the next owner's lock")
	}
	// A release frees the lock at once, and stops the extends: nothing holds it afterwards.
	taken()
	again, ok, err := holder.Hold(ctx, "lbc-c1", lease, 0)
	if err != nil || !ok {
		t.Fatalf("after a release: ok=%v err=%v", ok, err)
	}
	again()
	time.Sleep(lease / 2) // an extend still running would push lockUntil out again
	if ok, err := other.Lock(ctx, "lbc-c1", lease, time.Now().UTC()); err != nil || !ok {
		t.Fatalf("the lock was held after its release: ok=%v err=%v", ok, err)
	}
}

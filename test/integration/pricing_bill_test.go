//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// TestBillRepoGetAndLock verifies the bill get-or-create + optimistic/pessimistic lock
// (getCurrentBill / getAndLockBill): create once, lock held within the
// window, re-lockable after expiry, no duplicate bill, and SaveBill round-trips.
func TestBillRepoGetAndLock(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := pricing.NewRepo(db)

	start := time.Now().UTC().Truncate(24 * time.Hour)
	end := start.AddDate(0, 0, 28)
	now := start.Add(12 * time.Hour)

	// 1. create + claim.
	b1, err := repo.GetCurrentBill(ctx, "bp1", start, end, now, "USD")
	if err != nil || b1 == nil {
		t.Fatalf("getCurrentBill: %v / %v", err, b1)
	}
	if b1.Status != pricing.BillStatusOpen || b1.BillingProfileID != "bp1" {
		t.Fatalf("bill: status=%s bp=%s", b1.Status, b1.BillingProfileID)
	}

	// 2. lock held: the lease is WALL-CLOCK (lockedAt = time.Now()+1m, decoupled from the
	// charge's truncated `now`) — so the immediacy check must also use wall-clock.
	if b2, _ := repo.GetAndLockBill(ctx, "bp1", start, time.Now().UTC()); b2 != nil {
		t.Fatal("expected lock to be held (nil), got a bill")
	}

	// 3. after the lease window (wall-clock +2m > lockedAt = wall-clock +1m) it is claimable again.
	if b3, _ := repo.GetAndLockBill(ctx, "bp1", start, time.Now().UTC().Add(2*time.Minute)); b3 == nil {
		t.Fatal("expected re-lock after expiry")
	}

	// 4. no duplicate bill — exactly one OPEN bill for the cycle.
	n, err := db.C("bill").Count(ctx, pgdoc.M{
		"billingProfileId": "bp1", "status": pricing.BillStatusOpen,
		"billingCycle.startDate": pgdoc.M{"$gte": start},
	})
	if err != nil || n != 1 {
		t.Fatalf("expected 1 OPEN bill, got %d (%v)", n, err)
	}

	// 5. SaveBill round-trip (mark SENT). The lease is WALL-CLOCK — this re-acquire must
	// use wall-clock too: the old `now.Add(4m)` (bill-cycle noon) sat BEFORE the step-3 lease
	// whenever the suite ran after 12:04 UTC → time-of-day flake (caught 2026-07-02 19:58+07).
	b3, _ := repo.GetAndLockBill(ctx, "bp1", start, time.Now().UTC().Add(4*time.Minute))
	if b3 == nil {
		t.Fatal("re-acquire for save")
	}
	b3.Status = pricing.BillStatusSent
	if _, err := repo.SaveBill(ctx, b3); err != nil {
		t.Fatalf("save: %v", err)
	}
	var reread pricing.Bill
	if found, err := db.C("bill").FindOne(ctx, pgdoc.M{"billingProfileId": "bp1"}, &reread); err != nil || !found {
		t.Fatalf("reread: found=%v %v", found, err)
	}
	if reread.Status != pricing.BillStatusSent {
		t.Fatalf("save did not persist status, got %s", reread.Status)
	}
}

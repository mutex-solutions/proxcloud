//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
)

// TestAutoSuspensionAndReview exercises the auto-suspension state machine
// (executeBillingProfile + reviewBillingProfile) on the real store:
// an over-limit ACTIVE profile is suspended (process + profile flip), then once its
// balance recovers, reviewBillingProfile resumes it and resolves the process.
func TestAutoSuspensionAndReview(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)

	// BALANCE-type auto-suspension: notify at balance < -50, suspend at balance ≤ -100.
	mustInsert(t, db, "billingConfiguration", pgdoc.M{
		"baseCurrency": "USD",
		"suspensionConfiguration": pgdoc.M{
			"enabled":       true,
			"type":          "BALANCE",
			"suspendedAt":   pgdoc.M{"balance": decimalOf(t, "-100"), "days": 0},
			"notifications": []any{pgdoc.M{"balance": decimalOf(t, "-50"), "days": 0}},
		},
	})

	// ACTIVE profile owing 150 (one SENT bill, no credits) → balance -150.
	profileID := pgdoc.NewID()
	mustInsert(t, db, "billingProfile", pgdoc.M{"_id": profileID, "status": billing.StatusActive, "currency": "USD"})
	mustInsert(t, db, "bill", pgdoc.M{
		"billingProfileId": profileID, "status": "SENT", "invoiceCurrency": "USD",
		"items": []any{pgdoc.M{"resourceType": "instance", "currency": "USD", "netAmount": decimalOf(t, "150")}},
	})

	repo := billing.NewRepo(db)
	job := billing.NewSuspensionJob(repo, nil)

	// --- suspend ---
	n, err := job.ExecuteDunning(ctx)
	if err != nil {
		t.Fatalf("ExecuteDunning: %v", err)
	}
	if n != 1 {
		t.Fatalf("suspended count = %d, want 1", n)
	}
	if p, _ := repo.FindByID(ctx, profileID); p == nil || p.Status != billing.StatusSuspended {
		t.Fatalf("profile status = %v, want SUSPENDED", p)
	}
	if sp, _ := repo.FindFirstSuspensionByStatus(ctx, profileID, billing.SuspensionSuspended); sp == nil {
		t.Fatalf("no SUSPENDED suspension process created")
	}

	// idempotent: a second run does not re-suspend.
	if n2, err := job.ExecuteDunning(ctx); err != nil || n2 != 0 {
		t.Fatalf("second ExecuteDunning = %d (err %v), want 0", n2, err)
	}

	// --- recover: add 200 credit → balance +50 → no longer eligible ---
	mustInsert(t, db, "accountCredit", pgdoc.M{"billingProfileId": profileID, "amount": decimalOf(t, "200")})
	suspended, _ := repo.FindByID(ctx, profileID)
	if err := job.ReviewBillingProfile(ctx, suspended); err != nil {
		t.Fatalf("ReviewBillingProfile: %v", err)
	}
	if p, _ := repo.FindByID(ctx, profileID); p == nil || p.Status != billing.StatusActive {
		t.Fatalf("profile status after review = %v, want ACTIVE", p)
	}
	if sp, _ := repo.FindFirstSuspensionByStatus(ctx, profileID, billing.SuspensionResolved); sp == nil {
		t.Fatalf("suspension process not RESOLVED after review")
	}
}

// TestAutoSuspensionNotEligible: a profile within its limit is left ACTIVE.
func TestAutoSuspensionNotEligible(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	mustInsert(t, db, "billingConfiguration", pgdoc.M{
		"baseCurrency": "USD",
		"suspensionConfiguration": pgdoc.M{
			"enabled": true, "type": "BALANCE",
			"suspendedAt":   pgdoc.M{"balance": decimalOf(t, "-100"), "days": 0},
			"notifications": []any{pgdoc.M{"balance": decimalOf(t, "-50"), "days": 0}},
		},
	})
	pid := pgdoc.NewID()
	mustInsert(t, db, "billingProfile", pgdoc.M{"_id": pid, "status": billing.StatusActive, "currency": "USD"})
	// owes only 10 → balance -10, above the -50 start limit.
	mustInsert(t, db, "bill", pgdoc.M{
		"billingProfileId": pid, "status": "SENT", "invoiceCurrency": "USD",
		"items": []any{pgdoc.M{"resourceType": "instance", "currency": "USD", "netAmount": decimalOf(t, "10")}},
	})

	repo := billing.NewRepo(db)
	n, err := billing.NewSuspensionJob(repo, nil).ExecuteDunning(ctx)
	if err != nil || n != 0 {
		t.Fatalf("ExecuteDunning = %d (err %v), want 0", n, err)
	}
	if p, _ := repo.FindByID(ctx, pid); p == nil || p.Status != billing.StatusActive {
		t.Fatalf("profile status = %v, want ACTIVE (not eligible)", p)
	}
}

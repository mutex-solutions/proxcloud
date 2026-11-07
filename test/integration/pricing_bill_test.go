//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// TestChargeOpensAndReusesTheCycleBill: the first charge of a cycle opens its bill, back-to-back
// charges reuse it (the old lease made the next one wait or give up), an issued bill is left alone, and
// another cycle gets its own.
func TestChargeOpensAndReusesTheCycleBill(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo, eng := pricing.NewRepo(db), pricing.NewEngine(pricing.SystemClock())

	start := time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	charge := func(cs, ce time.Time) *pricing.Bill {
		t.Helper()
		at := cs.Add(12 * time.Hour)
		rc := pricing.RatingContext{TimeUnit: pricing.TimeUnitHour, CycleTimestamp: at}
		b, err := pricing.ChargeBillingResources(ctx, repo, eng, rc, pricing.BillingContext{}, "bp1", nil, nil, cs, ce, at, "USD", nil)
		if err != nil || b == nil {
			t.Fatalf("charge: %v / %v", err, b)
		}
		return b
	}

	b1 := charge(start, end)
	if b1.Status != pricing.BillStatusOpen || b1.BillingProfileID != "bp1" || b1.InvoiceCurrency != "USD" {
		t.Fatalf("bill: status=%s bp=%s currency=%s", b1.Status, b1.BillingProfileID, b1.InvoiceCurrency)
	}
	for range 3 {
		if b := charge(start, end); b.ID != b1.ID {
			t.Fatalf("back-to-back charge used bill %s, want %s", b.ID, b1.ID)
		}
	}
	if n, err := db.C("bill").Count(ctx, pgdoc.M{"billingProfileId": "bp1"}); err != nil || n != 1 {
		t.Fatalf("expected 1 bill, got %d (%v)", n, err)
	}

	// The next cycle's bill does not count as this cycle's, in either direction.
	next := charge(end, end.AddDate(0, 1, 0))
	if next.ID == b1.ID {
		t.Fatal("next cycle reused this cycle's bill")
	}
	if b := charge(start, end); b.ID != b1.ID {
		t.Fatalf("this cycle's charge used bill %s, want %s", b.ID, b1.ID)
	}
}

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

func dptr(s string) *decimal.Decimal { d := decimal.RequireFromString(s); return &d }

// TestChargeBillingResources verifies the rating-loop orchestration end-to-end against
// Postgres: a price-plan rule + a billing resource → get-or-create+lock the current bill →
// rate (Engine) → accumulate (SaveChargingToBill) → persist. Asserts a charged item lands
// on the persisted bill. (Exact rating math is covered by the pricing golden tests.)
func TestChargeBillingResources(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := pricing.NewRepo(db)
	eng := pricing.NewEngine(pricing.SystemClock())

	rule := pricing.PricePlanRule{
		TimeUnit: pricing.TimeUnitHour, ResourceType: "compute",
		Prices: []pricing.PricePlanRulePrice{{
			AttributeName: "vcpu",
			Tiers:         []pricing.PriceTier{{From: nil, To: nil, Value: dptr("5")}},
		}},
	}
	res := &pricing.BillingResource{
		ResourceID: "res-1", ProjectID: "proj-1", ResourceType: "compute",
		Values: map[string]any{"vcpu": int64(2)},
		BillingResourceType: &pricing.BillingResourceType{
			ResourceType: "compute",
			Attributes:   []pricing.ResourceAttribute{{Name: "vcpu", Type: "number"}},
		},
	}

	start := time.Now().UTC().Truncate(24 * time.Hour)
	end := start.AddDate(0, 0, 28)
	rc := pricing.RatingContext{TimeUnit: pricing.TimeUnitHour, CycleTimestamp: start.Add(12 * time.Hour)}

	bill, err := pricing.ChargeBillingResources(ctx, repo, eng, rc, pricing.BillingContext{},
		"bp-charge", []pricing.PricePlanRule{rule}, []*pricing.BillingResource{res}, start, end, start.Add(time.Hour), "USD", nil)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if bill == nil || bill.ID == "" {
		t.Fatalf("expected a persisted bill, got %v", bill)
	}
	if len(bill.Items) != 1 {
		t.Fatalf("expected 1 bill item, got %d", len(bill.Items))
	}
	if !bill.Items[0].NetAmount.GreaterThan(decimal.Zero) {
		t.Fatalf("expected a positive charge, got %s", bill.Items[0].NetAmount)
	}

	// persisted: re-read from the store and confirm the charged item survived.
	var reread pricing.Bill
	if found, err := db.C("bill").FindOne(ctx, pgdoc.M{"billingProfileId": "bp-charge"}, &reread); err != nil || !found {
		t.Fatalf("reread: found=%v %v", found, err)
	}
	if len(reread.Items) != 1 || !reread.Items[0].NetAmount.GreaterThan(decimal.Zero) {
		t.Fatalf("persisted bill missing charge: items=%d", len(reread.Items))
	}
	if reread.Items[0].ResourceID != "res-1" {
		t.Errorf("item resourceId = %q, want res-1", reread.Items[0].ResourceID)
	}
}

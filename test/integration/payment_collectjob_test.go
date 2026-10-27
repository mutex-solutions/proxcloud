//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/payment"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// TestCollectJobPaysSentBill exercises CollectService.CollectBillingProfile (the monthlyCollect
// cron body) against real Postgres + a fake gateway: a SENT bill + the profile's current card →
// charge (confirm=true) → applyPaidCollectOnBill → bill PAID + a SUCCESS CollectTransaction.
func TestCollectJobPaysSentBill(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)

	if _, err := db.C("billingConfiguration").InsertOne(ctx, pgdoc.M{"baseCurrency": "USD"}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if _, err := db.C("thirdPartyIntegration").InsertOne(ctx, pgdoc.M{
		"_id": "gw1", "thirdParty": "Stripe", "secret": pgdoc.M{"privateKey": "sk_test_x"},
	}); err != nil {
		t.Fatalf("seed gw: %v", err)
	}
	const bpID = "6a37e2f547f3b4378ba78549"
	if _, err := db.C("billingProfile").InsertOne(ctx, pgdoc.M{
		"_id": bpID, "currency": "USD", "country": "US", "status": "ACTIVE",
	}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	// a non-expired stored card on the gateway.
	cardID := pgdoc.NewID()
	if _, err := db.C("creditCard").InsertOne(ctx, pgdoc.M{
		"_id": cardID, "billingProfileId": bpID, "tokenId": "pm_saved", "panMasked": "*.4242",
		"paymentGatewayId": "gw1", "tokenExpirationDate": time.Now().UTC().AddDate(1, 0, 0),
	}); err != nil {
		t.Fatalf("seed card: %v", err)
	}
	// a SENT bill with a net of 20 (unpaid 20 in base/product currency).
	net20 := decimalOf(t, "20")
	billID := pgdoc.NewID()
	if _, err := db.C("bill").InsertOne(ctx, pgdoc.M{
		"_id": billID, "billingProfileId": bpID, "status": "SENT", "invoiceCurrency": "USD",
		"items": []any{pgdoc.M{"netAmount": net20}},
	}); err != nil {
		t.Fatalf("seed bill: %v", err)
	}

	fg := &fakeGW{}
	svc := payment.NewCollectService(repo, pricingRepo, func(string) payment.Gateway { return fg })
	profile := &billing.BillingProfile{ID: bpID, Currency: "USD", Country: "US", Status: "ACTIVE"}

	paid, err := svc.CollectBillingProfile(ctx, profile)
	if err != nil {
		t.Fatalf("collectBillingProfile: %v", err)
	}
	if paid != 1 {
		t.Fatalf("paid = %d, want 1", paid)
	}
	if fg.lastAmountCents != 2000 { // 20.00 → 2000 cents (no tax seeded)
		t.Fatalf("charged cents = %d, want 2000", fg.lastAmountCents)
	}
	// the bill is now PAID.
	var billDoc pgdoc.M
	if found, err := db.C("bill").FindOne(ctx, pgdoc.M{"_id": billID}, &billDoc); err != nil || !found {
		t.Fatalf("reload bill: found=%v err=%v", found, err)
	}
	if billDoc["status"] != "PAID" {
		t.Fatalf("bill status = %v, want PAID", billDoc["status"])
	}
	// a SUCCESS CollectTransaction linked to the bill.
	n, _ := db.C("collectTransaction").Count(ctx, pgdoc.M{"billId": billID, "status": "SUCCESS"})
	if n != 1 {
		t.Fatalf("SUCCESS collectTransaction count = %d, want 1", n)
	}

	// SUSPENDED profile → skipped (no new collection).
	susBP := &billing.BillingProfile{ID: bpID, Currency: "USD", Status: "SUSPENDED"}
	if paid, err := svc.CollectBillingProfile(ctx, susBP); err != nil || paid != 0 {
		t.Fatalf("suspended should skip: paid=%d err=%v", paid, err)
	}
	_ = decimal.Zero
}

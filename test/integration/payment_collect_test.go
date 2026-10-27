//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/payment"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// TestCollectByCardDispatch exercises payment.CollectService.CollectByCard against real Postgres with
// a fake gateway: load the saved card → PENDING CollectTransaction → charge (confirm=true,
// synchronous) → processCollect SUCCESS → SUCCESS txn + one spendable AccountCredit.
func TestCollectByCardDispatch(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)

	if _, err := db.C("billingConfiguration").InsertOne(ctx, pgdoc.M{"baseCurrency": "USD"}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if _, err := db.C("thirdPartyIntegration").InsertOne(ctx, pgdoc.M{
		"_id": "gw1", "name": "Stripe", "thirdParty": "Stripe",
		"config": pgdoc.M{"publicKey": "pk_test_x"}, "secret": pgdoc.M{"privateKey": "sk_test_x"},
	}); err != nil {
		t.Fatalf("seed gw: %v", err)
	}
	const bpID = "6a37e2f547f3b4378ba78549"
	if _, err := db.C("billingProfile").InsertOne(ctx, pgdoc.M{"_id": bpID, "currency": "USD", "country": "US", "email": "a@b.c", "firstName": "Ada"}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	// a stored card linked to the gateway.
	cardID := pgdoc.NewID()
	if _, err := db.C("creditCard").InsertOne(ctx, pgdoc.M{
		"_id": cardID, "billingProfileId": bpID, "tokenId": "pm_saved", "panMasked": "*.4242", "paymentGatewayId": "gw1",
	}); err != nil {
		t.Fatalf("seed card: %v", err)
	}

	fg := &fakeGW{}
	svc := payment.NewCollectService(repo, pricingRepo, func(string) payment.Gateway { return fg })
	profile := &billing.BillingProfile{ID: bpID, Currency: "USD", Country: "US", Email: "a@b.c", FirstName: "Ada"}

	amt := decimal.NewFromInt(15)
	txn, err := svc.CollectByCard(ctx, profile, payment.CollectRequest{CardID: cardID, Amount: &amt})
	if err != nil {
		t.Fatalf("collectByCard: %v", err)
	}
	if txn.Status != pricing.CollectTransactionStatusSuccess {
		t.Fatalf("status = %s, want SUCCESS", txn.Status)
	}
	if txn.ExternalID != "pi_collect" || txn.CreditCardID != cardID || txn.PaymentGatewayID != "gw1" {
		t.Fatalf("txn fields: %+v", txn)
	}
	if fg.lastAmountCents != 1500 { // 15.00 → 1500 cents (no tax seeded)
		t.Fatalf("amount cents = %d, want 1500", fg.lastAmountCents)
	}
	// persisted + terminal.
	n, _ := db.C("collectTransaction").Count(ctx, pgdoc.M{"billingProfileId": bpID, "status": "SUCCESS"})
	if n != 1 {
		t.Fatalf("collectTransaction SUCCESS count = %d, want 1", n)
	}
	// no billId/orderId → a spendable AccountCredit was created.
	ac, _ := db.C("accountCredit").Count(ctx, pgdoc.M{"billingProfileId": bpID})
	if ac != 1 {
		t.Fatalf("accountCredit count = %d, want 1", ac)
	}

	// missing card → 404.
	if _, err := svc.CollectByCard(ctx, profile, payment.CollectRequest{CardID: pgdoc.NewID(), Amount: &amt}); err == nil {
		t.Fatal("expected not-found for a missing card")
	}
}

// TestRefundFunds exercises payment.AddFundsService.RefundFunds: a SUCCESS deposit (with a linked
// AccountCredit) → refund SUCCESS → txn REFUNDED + the AccountCredit voided.
func TestRefundFunds(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)

	if _, err := db.C("thirdPartyIntegration").InsertOne(ctx, pgdoc.M{
		"_id": "gw1", "thirdParty": "Stripe", "secret": pgdoc.M{"privateKey": "sk_test_x"},
	}); err != nil {
		t.Fatalf("seed gw: %v", err)
	}
	// the spendable credit that the deposit created.
	creditID := pgdoc.NewID()
	if _, err := db.C("accountCredit").InsertOne(ctx, pgdoc.M{"_id": creditID, "billingProfileId": "bp1", "currency": "USD"}); err != nil {
		t.Fatalf("seed credit: %v", err)
	}
	// a SUCCESS deposit transaction linking that credit by id.
	txnID := pgdoc.NewID()
	if _, err := db.C("accountCreditTransaction").InsertOne(ctx, pgdoc.M{
		"_id": txnID, "billingProfileId": "bp1", "status": "SUCCESS", "externalId": "pi_done",
		"paymentGatewayId": "gw1", "currency": "USD", "accountCredit": pgdoc.M{"id": creditID},
	}); err != nil {
		t.Fatalf("seed txn: %v", err)
	}

	svc := payment.NewAddFundsService(repo, pricingRepo, func(string) payment.Gateway { return &fakeGW{} })
	done, err := svc.RefundFunds(ctx, txnID)
	if err != nil {
		t.Fatalf("refundFunds: %v", err)
	}
	if done.Status != "REFUNDED" {
		t.Fatalf("status = %s, want REFUNDED", done.Status)
	}
	if n, _ := db.C("accountCredit").Count(ctx, pgdoc.M{"_id": creditID}); n != 0 {
		t.Fatalf("accountCredit not voided: %d", n)
	}
}

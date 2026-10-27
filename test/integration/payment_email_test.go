//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/payment"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// TestRefundSendsRefundEmail verifies the email wiring: a successful refund sends the
// send_refunded_invoice template to the profile (reuses the captureNotifier from mail_jobs_test).
func TestRefundSendsRefundEmail(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)

	if _, err := db.C("thirdPartyIntegration").InsertOne(ctx, pgdoc.M{"_id": "gw1", "thirdParty": "Stripe", "secret": pgdoc.M{"privateKey": "sk_x"}}); err != nil {
		t.Fatalf("seed gw: %v", err)
	}
	const bpID = "6a37e2f547f3b4378ba78549"
	if _, err := db.C("billingProfile").InsertOne(ctx, pgdoc.M{"_id": bpID, "email": "ada@example.com", "firstName": "Ada", "lastName": "Lovelace", "currency": "USD"}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	creditID := pgdoc.NewID()
	if _, err := db.C("accountCredit").InsertOne(ctx, pgdoc.M{"_id": creditID, "billingProfileId": bpID, "currency": "USD"}); err != nil {
		t.Fatalf("seed credit: %v", err)
	}
	txnID := pgdoc.NewID()
	if _, err := db.C("accountCreditTransaction").InsertOne(ctx, pgdoc.M{
		"_id": txnID, "billingProfileId": bpID, "status": "SUCCESS", "externalId": "pi_x",
		"paymentGatewayId": "gw1", "currency": "USD", "externalInvoiceId": "in_42",
		"accountCredit": pgdoc.M{"id": creditID},
	}); err != nil {
		t.Fatalf("seed txn: %v", err)
	}

	svc := payment.NewAddFundsService(repo, pricingRepo, func(string) payment.Gateway { return &fakeGW{} })
	cap := &captureNotifier{}
	svc.SetNotifier(cap)

	if _, err := svc.RefundFunds(ctx, txnID); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("notifier calls = %d, want 1", len(cap.calls))
	}
	c := cap.calls[0]
	if c.key != "send_refunded_invoice" || len(c.to) != 1 || c.to[0] != "ada@example.com" {
		t.Fatalf("notification wrong: key=%s to=%v", c.key, c.to)
	}
	if c.vars["firstName"] != "Ada" || c.vars["invoiceNumber"] != "in_42" {
		t.Fatalf("vars: %+v", c.vars)
	}
}

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

// ProcessBankTransfer (the manual bank-transfer processAddFunds — no gateway call):
// APPROVED settles the PENDING txn (SUCCESS + spendable account credit), REJECTED marks it FAILED
// with the transfer's comments as gatewayMessage, and re-driving a settled txn is idempotent.
func TestProcessBankTransfer(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)

	if _, err := db.C("billingConfiguration").InsertOne(ctx, pgdoc.M{"baseCurrency": "USD"}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	bpID := pgdoc.NewID()
	if _, err := db.C("billingProfile").InsertOne(ctx, pgdoc.M{
		"_id": bpID, "currency": "USD", "country": "US", "email": "bank@demo", "firstName": "Bea",
	}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	tenD128 := decimalOf(t, "10")
	svc := payment.NewAddFundsService(repo, pricingRepo, nil) // nil gateway factory — bank path never calls it

	// (1) APPROVED → SUCCESS + credit minted.
	okTxn := pgdoc.NewID()
	if _, err := db.C("accountCreditTransaction").InsertOne(ctx, pgdoc.M{
		"_id": okTxn, "status": "PENDING", "billingProfileId": bpID, "paymentGatewayId": "gw-bank",
		"currency": "USD", "amount": tenD128, "grossAmount": tenD128,
	}); err != nil {
		t.Fatalf("seed txn: %v", err)
	}
	got, err := svc.ProcessBankTransfer(ctx, okTxn, "APPROVED", "")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got.Status != "SUCCESS" {
		t.Fatalf("status: want SUCCESS got %s", got.Status)
	}
	if n, _ := db.C("accountCredit").Count(ctx, pgdoc.M{"billingProfileId": bpID}); n != 1 {
		t.Fatalf("account credit: want 1 got %d", n)
	}
	// idempotent re-drive.
	if again, err := svc.ProcessBankTransfer(ctx, okTxn, "APPROVED", ""); err != nil || again.Status != "SUCCESS" {
		t.Fatalf("re-approve not idempotent: %v %+v", err, again)
	}
	if n, _ := db.C("accountCredit").Count(ctx, pgdoc.M{"billingProfileId": bpID}); n != 1 {
		t.Fatalf("re-approve minted a duplicate credit")
	}

	// (2) REJECTED → FAILED + gatewayMessage = comments.
	badTxn := pgdoc.NewID()
	if _, err := db.C("accountCreditTransaction").InsertOne(ctx, pgdoc.M{
		"_id": badTxn, "status": "PENDING", "billingProfileId": bpID, "paymentGatewayId": "gw-bank",
		"currency": "USD", "amount": tenD128, "grossAmount": tenD128,
	}); err != nil {
		t.Fatalf("seed txn2: %v", err)
	}
	got, err = svc.ProcessBankTransfer(ctx, badTxn, "REJECTED", "no funds received")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if got.Status != "FAILED" || got.GatewayMessage != "no funds received" {
		t.Fatalf("reject: want FAILED/'no funds received', got %s/%q", got.Status, got.GatewayMessage)
	}

	// (3) PENDING status → no state change.
	if got, err = svc.ProcessBankTransfer(ctx, badTxn, "PENDING", ""); err != nil || got.Status != "FAILED" {
		t.Fatalf("pending no-op broken: %v %+v", err, got)
	}
}

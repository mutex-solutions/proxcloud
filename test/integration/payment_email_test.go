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
		"grossAmount": decimalOf(t, "10.9"), "accountCredit": pgdoc.M{"id": creditID},
	}); err != nil {
		t.Fatalf("seed txn: %v", err)
	}

	svc := payment.NewAddFundsService(repo, pricingRepo, func(*billing.Gateway) payment.Gateway { return &fakeGW{} })
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
	// invoiceNumber is the credit note number (the PDF the txn download route serves), not the
	// (always blank) external invoice id.
	if c.vars["firstName"] != "Ada" || c.vars["invoiceNumber"] != billing.CreditNoteNumber(txnID) ||
		c.vars["amount"] != "10.90" || c.vars["currency"] != "USD" {
		t.Fatalf("vars: %+v", c.vars)
	}
}

// TestAddFundsBankTransferSendsInstructions: a deposit through the manual BankTransfer gateway
// emails send_bank_transfer_instructions (instructions from the integration config, gross amount,
// reference number, the targeted bill) and stamps billId on the pending transfer doc.
func TestAddFundsBankTransferSendsInstructions(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)

	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD"})
	mustInsert(t, db, "thirdPartyIntegration", pgdoc.M{
		"_id": "gw-bank", "thirdParty": "BankTransfer",
		"config": pgdoc.M{"minDeposit": 5, "bankTransferInstructions": "IBAN DE02 1203 0000 0000 2020 51"},
	})
	profile := &billing.BillingProfile{ID: pgdoc.NewID(), Currency: "USD", Country: "US", Email: "bea@example.com", FirstName: "Bea", LastName: "Ng"}
	mustInsert(t, db, "billingProfile", pgdoc.M{"_id": profile.ID, "currency": "USD", "country": "US", "email": profile.Email, "firstName": "Bea", "lastName": "Ng"})

	// The bill being paid: SENT with 10 unpaid net. A bill-targeted deposit takes its amount from
	// the bill (the client-supplied amount is ignored), so the request below sends a bogus 999.
	mustInsert(t, db, "bill", pgdoc.M{
		"_id": "bill-7", "billingProfileId": profile.ID, "status": "SENT", "invoiceCurrency": "USD",
		"billingCycle": pgdoc.M{"startDate": "2026-08-01T00:00:00Z", "endDate": "2026-08-31T23:59:59Z"},
		"items":        []any{pgdoc.M{"resourceType": "instance", "currency": "USD", "netAmount": decimalOf(t, "10")}},
	})

	svc := payment.NewAddFundsService(repo, pricingRepo, nil) // bank path never touches the gateway factory
	cap := &captureNotifier{}
	svc.SetNotifier(cap)

	amt := decimalOf(t, "999")
	resp, err := svc.AddFunds(ctx, profile, payment.AddFundsRequest{Amount: &amt, PaymentGatewayID: "gw-bank", BillID: "bill-7"})
	if err != nil {
		t.Fatalf("addFunds: %v", err)
	}
	if resp.ThirdParty != "BankTransfer" || resp.TransactionID == "" {
		t.Fatalf("response: %+v", resp)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("notifier calls = %d, want 1", len(cap.calls))
	}
	c := cap.calls[0]
	if c.key != "send_bank_transfer_instructions" || c.to[0] != "bea@example.com" {
		t.Fatalf("notification wrong: key=%s to=%v", c.key, c.to)
	}
	if c.vars["fullName"] != "Bea Ng" || c.vars["instructions"] != "IBAN DE02 1203 0000 0000 2020 51" ||
		c.vars["amount"] != "10.00" || c.vars["currency"] != "USD" || c.vars["billId"] != "bill-7" || c.vars["referenceNumber"] == nil {
		t.Fatalf("vars: %+v", c.vars)
	}
	bt, err := repo.BankTransferByTxnID(ctx, resp.TransactionID)
	if err != nil || bt == nil {
		t.Fatalf("bank transfer doc: %v %v", bt, err)
	}
	if bt["billId"] != "bill-7" || bt["status"] != "PENDING" {
		t.Fatalf("bank transfer doc: %+v", bt)
	}
}

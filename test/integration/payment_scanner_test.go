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

// TestTransactionScanner exercises payment.TransactionScanner against real Postgres with the fake
// gateway (transaction scanner): stuck PENDING deposits are re-driven to SUCCESS (+
// AccountCredit materializes), a stuck collect on an already-PAID bill is CANCELLED, a collect on
// a live bill is re-processed, and out-of-window / blank-externalId transactions are untouched.
func TestTransactionScanner(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)
	now := time.Now().UTC()

	if _, err := db.C("billingConfiguration").InsertOne(ctx, pgdoc.M{"baseCurrency": "USD"}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if _, err := db.C("thirdPartyIntegration").InsertOne(ctx, pgdoc.M{
		"_id": "gw1", "name": "Stripe", "thirdParty": "Stripe",
		"config": pgdoc.M{"publicKey": "pk_x"}, "secret": pgdoc.M{"privateKey": "sk_x"},
	}); err != nil {
		t.Fatalf("seed gw: %v", err)
	}
	const bpID = "6a37e2f547f3b4378ba78549"
	if _, err := db.C("billingProfile").InsertOne(ctx, pgdoc.M{
		"_id": bpID, "currency": "USD", "country": "US", "email": "a@b.c", "firstName": "Ada",
	}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	stuck := now.Add(-time.Hour)        // in the (now-24h, now-20min) window
	fresh := now.Add(-5 * time.Minute)  // too fresh — must be skipped
	ancient := now.Add(-48 * time.Hour) // too old — outside the window

	ten := decimal.NewFromInt(10)
	tenD128 := decimalOf(t, "10")
	// string _ids — the by-id finders (AccountCreditTransactionByID / BillByID) key on the id column.
	acctIDs := map[string]string{}
	seedAcct := func(key string, createdAt time.Time, externalID string) {
		id := pgdoc.NewID()
		acctIDs[key] = id
		if _, err := db.C("accountCreditTransaction").InsertOne(ctx, pgdoc.M{
			"_id": id, "status": "PENDING", "billingProfileId": bpID, "paymentGatewayId": "gw1",
			"externalId": externalID, "currency": "USD",
			"amount": tenD128, "grossAmount": tenD128, "createdAt": createdAt,
		}); err != nil {
			t.Fatalf("seed acct txn %s: %v", key, err)
		}
	}
	seedAcct("acct-stuck", stuck, "pi_fake")
	seedAcct("acct-fresh", fresh, "pi_fake")
	seedAcct("acct-ancient", ancient, "pi_fake")
	seedAcct("acct-noext", stuck, "")

	// Bills: one PAID (its stuck collect is reconciled against the gateway), one SENT (its collect re-processes).
	paidBillID, sentBillID := pgdoc.NewID(), pgdoc.NewID()
	if _, err := db.C("bill").InsertOne(ctx, pgdoc.M{"_id": paidBillID, "status": "PAID", "billingProfileId": bpID, "invoiceCurrency": "USD"}); err != nil {
		t.Fatalf("seed paid bill: %v", err)
	}
	if _, err := db.C("bill").InsertOne(ctx, pgdoc.M{"_id": sentBillID, "status": "SENT", "billingProfileId": bpID, "invoiceCurrency": "USD"}); err != nil {
		t.Fatalf("seed sent bill: %v", err)
	}
	one := decimal.NewFromInt(1)
	seedCollect := func(id, billID string, createdAt time.Time) {
		c := &pricing.CollectTransaction{
			BillID: billID, Status: pricing.CollectTransactionStatusPending, BillingProfileID: bpID,
			PaymentGatewayID: "gw1", ExternalID: "pi_collect", Currency: "USD",
			Amount: &ten, GrossAmount: &ten, ExchangeRate: &one, CreatedAt: &createdAt,
		}
		saved, err := repo.SaveCollectTransaction(ctx, c)
		if err != nil {
			t.Fatalf("seed collect %s: %v", id, err)
		}
		// pin the intended createdAt (SaveCollectTransaction stamps updatedAt only when id set).
		if _, err := db.C("collectTransaction").SetFieldsOne(ctx, pgdoc.M{"_id": saved.ID}, pgdoc.M{"createdAt": createdAt}, nil); err != nil {
			t.Fatalf("pin createdAt %s: %v", id, err)
		}
	}
	seedCollect("col-paidbill", paidBillID, stuck)
	seedCollect("col-sentbill", sentBillID, stuck)

	fg := &fakeGW{} // RetrievePaymentIntent → succeeded
	addFunds := payment.NewAddFundsService(repo, pricingRepo, func(*billing.Gateway) payment.Gateway { return fg })
	collect := payment.NewCollectService(repo, pricingRepo, func(*billing.Gateway) payment.Gateway { return fg })
	scanner := payment.NewTransactionScanner(repo, addFunds, collect, nil)

	n, err := scanner.Scan(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	// examined: acct-stuck + the 2 collects (fresh/ancient/noext filtered out).
	if n != 3 {
		t.Fatalf("scanned = %d, want 3", n)
	}

	status := func(key string) string {
		var doc pgdoc.M
		_, _ = db.C("accountCreditTransaction").FindOne(ctx, pgdoc.M{"_id": acctIDs[key]}, &doc)
		s, _ := doc["status"].(string)
		return s
	}
	if got := status("acct-stuck"); got != "SUCCESS" {
		t.Errorf("acct-stuck = %s, want SUCCESS", got)
	}
	if got := status("acct-fresh"); got != "PENDING" {
		t.Errorf("acct-fresh = %s, want PENDING (too fresh)", got)
	}
	if got := status("acct-ancient"); got != "PENDING" {
		t.Errorf("acct-ancient = %s, want PENDING (outside 24h window)", got)
	}
	if got := status("acct-noext"); got != "PENDING" {
		t.Errorf("acct-noext = %s, want PENDING (blank externalId skipped)", got)
	}
	// the deposit's AccountCredit materialized, plus the surplus credit of the PAID bill's charge:
	// the gateway (fakeGW) reports that stuck charge succeeded, so the money the customer paid
	// twice is credited to the balance instead of being cancelled blind.
	if cn, _ := db.C("accountCredit").Count(ctx, pgdoc.M{"billingProfileId": bpID}); cn != 2 {
		t.Errorf("accountCredit count = %d, want 2 (deposit + surplus)", cn)
	}
	// collect on the PAID bill → SUCCESS as a surplus credit; collect on the SENT bill → SUCCESS
	// (re-processed, settles the bill).
	var surplus []pricing.CollectTransaction
	_ = db.C("collectTransaction").Find(ctx, pgdoc.M{"billId": paidBillID}, &surplus)
	if len(surplus) != 1 || surplus[0].Status != pricing.CollectTransactionStatusSuccess || surplus[0].Metadata["surplusAccountCreditId"] == nil {
		t.Errorf("collect on PAID bill: %+v, want SUCCESS with a surplus credit", surplus)
	}
	if succeeded, _ := db.C("collectTransaction").Count(ctx, pgdoc.M{"billId": sentBillID, "status": "SUCCESS"}); succeeded != 1 {
		t.Errorf("collect on SENT bill: succeeded = %d, want 1", succeeded)
	}

	// idempotent second pass: nothing left in-window PENDING with an externalId.
	if n2, err := scanner.Scan(ctx); err != nil || n2 != 0 {
		t.Fatalf("2nd scan: n=%d err=%v (want 0)", n2, err)
	}
}

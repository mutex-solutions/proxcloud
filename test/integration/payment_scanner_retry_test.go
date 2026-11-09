//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/payment"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// flakyGW answers every intent lookup with err, until err is cleared.
type flakyGW struct {
	fakeGW
	err error
}

func (g *flakyGW) RetrievePaymentIntent(ctx context.Context, id string) (payment.PaymentIntentResult, error) {
	if g.err != nil {
		return payment.PaymentIntentResult{}, g.err
	}
	return g.fakeGW.RetrievePaymentIntent(ctx, id)
}

type scanFixture struct {
	db      *pgdoc.DB
	repo    *billing.Repo
	gw      *flakyGW
	scanner *payment.TransactionScanner
}

func newScanFixture(t *testing.T) *scanFixture {
	t.Helper()
	f := newMoneyFixture(t)
	gw := &flakyGW{}
	factory := func(*billing.Gateway) payment.Gateway { return gw }
	scanner := payment.NewTransactionScanner(f.repo,
		payment.NewAddFundsService(f.repo, pricing.NewRepo(f.db), factory),
		payment.NewCollectService(f.repo, pricing.NewRepo(f.db), factory), nil)
	return &scanFixture{db: f.db, repo: f.repo, gw: gw, scanner: scanner}
}

func (f *scanFixture) deposit(t *testing.T, createdAt time.Time) string {
	t.Helper()
	ten := decimalOf(t, "10")
	return mustInsertID(t, f.db, "accountCreditTransaction", pgdoc.M{"status": "PENDING", "billingProfileId": "bp-a", "paymentGatewayId": "gw1",
		"externalId": "pi_x", "currency": "USD", "amount": ten, "grossAmount": ten, "createdAt": createdAt})
}

func (f *scanFixture) charge(t *testing.T, billID string, createdAt time.Time) string {
	t.Helper()
	ten, one := decimal.NewFromInt(10), decimal.NewFromInt(1)
	saved, err := f.repo.SaveCollectTransaction(context.Background(), &pricing.CollectTransaction{
		BillID: billID, BillingProfileID: "bp-a", Status: pricing.CollectTransactionStatusPending, PaymentGatewayID: "gw1",
		ExternalID: "pi_x", Currency: "USD", Amount: &ten, GrossAmount: &ten, ExchangeRate: &one,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.C("collectTransaction").SetFieldsOne(context.Background(), pgdoc.M{"_id": saved.ID}, pgdoc.M{"createdAt": createdAt}, nil); err != nil {
		t.Fatal(err)
	}
	return saved.ID
}

func (f *scanFixture) row(t *testing.T, table, id string) pgdoc.M {
	t.Helper()
	var doc pgdoc.M
	if found, err := f.db.C(table).FindOne(context.Background(), pgdoc.M{"_id": id}, &doc); err != nil || !found {
		t.Fatalf("%s %s: found=%v err=%v", table, id, found, err)
	}
	return doc
}

func (f *scanFixture) scan(t *testing.T) {
	t.Helper()
	if _, err := f.scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestScannerKeepsUnknownOutcomesPending: a gateway outage closes nothing, and the deposit that did succeed
// settles once the gateway answers again, with no repair.
func TestScannerKeepsUnknownOutcomesPending(t *testing.T) {
	f := newScanFixture(t)
	old := time.Now().UTC().Add(-time.Hour)
	sent := mustInsertID(t, f.db, "bill", pgdoc.M{"billingProfileId": "bp-a", "status": "SENT", "invoiceCurrency": "USD",
		"items": []any{pgdoc.M{"resourceType": "instance", "currency": "USD", "netAmount": decimalOf(t, "10")}}})
	paid := mustInsertID(t, f.db, "bill", pgdoc.M{"billingProfileId": "bp-a", "status": "PAID", "invoiceCurrency": "USD"})
	deposit, charge, late := f.deposit(t, old), f.charge(t, sent, old), f.charge(t, paid, old)

	f.gw.err = errors.New("connection reset by peer")
	for range 3 {
		f.scan(t)
	}
	for table, id := range map[string]string{"accountCreditTransaction": deposit, "collectTransaction": charge} {
		doc := f.row(t, table, id)
		if doc["status"] != "PENDING" {
			t.Errorf("%s during the outage = %v, want PENDING", table, doc["status"])
		}
	}
	if msg, _ := f.row(t, "accountCreditTransaction", deposit)["gatewayMessage"].(string); !strings.Contains(msg, "connection reset") {
		t.Errorf("the deposit's reason = %q", msg)
	}
	if msg, _ := f.row(t, "collectTransaction", charge)["errorMessage"].(string); !strings.Contains(msg, "connection reset") {
		t.Errorf("the charge's reason = %q", msg)
	}
	// A charge on a bill paid elsewhere is not cancelled blind either: it may be money the customer paid.
	if s := f.row(t, "collectTransaction", late)["status"]; s != "PENDING" {
		t.Errorf("charge on a paid bill during the outage = %v, want PENDING", s)
	}

	f.gw.err = nil
	f.scan(t)
	if s := f.row(t, "accountCreditTransaction", deposit)["status"]; s != "SUCCESS" {
		t.Errorf("deposit after the outage = %v, want SUCCESS", s)
	}
	if s := f.row(t, "collectTransaction", charge)["status"]; s != "SUCCESS" {
		t.Errorf("charge after the outage = %v, want SUCCESS", s)
	}
	doc := f.row(t, "collectTransaction", late)
	if meta, _ := doc["metadata"].(map[string]any); doc["status"] != "SUCCESS" || meta["surplusAccountCreditId"] == nil {
		t.Errorf("charge on a paid bill after the outage = %v", doc)
	}
}

// TestScannerClosesAMissingIntent: only the gateway saying it has no such intent is terminal.
func TestScannerClosesAMissingIntent(t *testing.T) {
	f := newScanFixture(t)
	old := time.Now().UTC().Add(-time.Hour)
	sent := mustInsertID(t, f.db, "bill", pgdoc.M{"billingProfileId": "bp-a", "status": "SENT", "invoiceCurrency": "USD"})
	deposit, charge := f.deposit(t, old), f.charge(t, sent, old)
	f.gw.err = &payment.AirwallexError{HTTPStatus: 404, Code: "resource_not_found", Message: "no such intent"}
	f.scan(t)
	if s := f.row(t, "accountCreditTransaction", deposit)["status"]; s != "FAILED" {
		t.Errorf("deposit = %v, want FAILED", s)
	}
	if s := f.row(t, "collectTransaction", charge)["status"]; s != "FAILED" {
		t.Errorf("charge = %v, want FAILED", s)
	}
}

// TestScannerStopsAtItsWindow is the bound: past 24 hours nothing asks the gateway, and the row stays PENDING
// for a late callback or an operator sync.
func TestScannerStopsAtItsWindow(t *testing.T) {
	f := newScanFixture(t)
	deposit := f.deposit(t, time.Now().UTC().Add(-25*time.Hour))
	f.gw.err = errors.New("connection reset by peer")
	f.scan(t)
	doc := f.row(t, "accountCreditTransaction", deposit)
	if doc["status"] != "PENDING" || doc["gatewayMessage"] != nil {
		t.Errorf("a deposit past the window was touched: %v", doc)
	}
	f.gw.err = nil
	svc := payment.NewAddFundsService(f.repo, pricing.NewRepo(f.db), func(*billing.Gateway) payment.Gateway { return f.gw })
	if txn, err := svc.ProcessAddFunds(context.Background(), deposit); err != nil || txn.Status != "SUCCESS" {
		t.Errorf("a late sync: %+v, %v", txn, err)
	}
}

// A charge past the scanner's window keeps blocking new charges for its bill until an answer lifts the guard: this is the
// double charge after a long gateway outage.
func TestStuckChargeBlocksUntilResolved(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)
	profile, cardID := seedPostpaid(t, db, billing.PaymentModePostpaid)
	billID := mustInsertID(t, db, "bill", pgdoc.M{"billingProfileId": profile.ID, "status": "SENT", "invoiceCurrency": "USD",
		"items": []any{pgdoc.M{"netAmount": decimalOf(t, "30")}}})
	var gw payment.Gateway = &statusGW{status: "requires_payment_method"}
	factory := func(*billing.Gateway) payment.Gateway { return gw }
	collect := payment.NewCollectService(repo, pricingRepo, factory)
	scanner := payment.NewTransactionScanner(repo, payment.NewAddFundsService(repo, pricingRepo, factory), collect, nil)
	stuck := mustInsertID(t, db, "collectTransaction", pgdoc.M{"billId": billID, "billingProfileId": profile.ID, "status": "PENDING",
		"externalId": "pi_stuck", "paymentGatewayId": "gw-awx", "currency": "USD", "amount": decimalOf(t, "30"), "grossAmount": decimalOf(t, "30"),
		"exchangeRate": decimalOf(t, "1"), "creditCardId": cardID, "createdAt": time.Now().UTC().Add(-72 * time.Hour)})
	charges := func() int64 {
		n, _ := db.C("collectTransaction").Count(ctx, pgdoc.M{"billId": billID})
		return n
	}

	// The cron and the customer both stop at the guard, and the customer is told waiting will not help.
	if paid, _ := collect.CollectBillingProfile(ctx, profile); paid != 0 || charges() != 1 {
		t.Fatalf("the cron charged again: paid=%d charges=%d", paid, charges())
	}
	if _, err := collect.CollectBill(ctx, profile, billID, payment.CollectBillRequest{CardID: cardID}); !errors.Is(err, payment.ErrCollectStuck) || charges() != 1 {
		t.Fatalf("customer retry: %v, charges=%d", err, charges())
	}
	// The scanner stopped asking long ago.
	if _, err := scanner.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if row, _ := repo.CollectTransactionByID(ctx, stuck); row.Status != pricing.CollectTransactionStatusPending {
		t.Fatalf("the scan touched it: %+v", row)
	}
	// A re-check while the gateway cannot answer says so and closes nothing.
	gw = &flakyGW{err: errors.New("connection reset by peer")}
	row, err := scanner.RecheckCollect(ctx, stuck)
	if err != nil || row.Status != pricing.CollectTransactionStatusPending || !strings.Contains(row.ErrorMessage, "an operator can re-check") {
		t.Fatalf("%+v %v", row, err)
	}
	// Then it answers: the money moved, so the bill is paid by that charge and no other.
	gw = &fakeGW{}
	row, err = scanner.RecheckCollect(ctx, stuck)
	if err != nil || row.Status != pricing.CollectTransactionStatusSuccess {
		t.Fatalf("%+v %v", row, err)
	}
	if bill, _ := repo.BillByID(ctx, billID); bill.Status != pricing.BillStatusPaid || charges() != 1 {
		t.Fatalf("bill %s, charges %d", bill.Status, charges())
	}
	// Re-checking a settled charge changes nothing, and an unknown id is nil.
	if row, err = scanner.RecheckCollect(ctx, stuck); err != nil || row.Status != pricing.CollectTransactionStatusSuccess {
		t.Fatalf("%+v %v", row, err)
	}
	if row, err = scanner.RecheckCollect(ctx, "no-such"); err != nil || row != nil {
		t.Fatalf("%+v %v", row, err)
	}
}

// A charge with no gateway reference cannot be re-checked, and only such a charge can be closed by hand.
func TestCloseUnreferencedCharge(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)
	profile, cardID := seedPostpaid(t, db, billing.PaymentModePostpaid)
	billID := mustInsertID(t, db, "bill", pgdoc.M{"billingProfileId": profile.ID, "status": "SENT", "invoiceCurrency": "USD",
		"items": []any{pgdoc.M{"netAmount": decimalOf(t, "30")}}})
	fg := &fakeGW{}
	factory := func(*billing.Gateway) payment.Gateway { return fg }
	collect := payment.NewCollectService(repo, pricingRepo, factory)
	scanner := payment.NewTransactionScanner(repo, payment.NewAddFundsService(repo, pricingRepo, factory), collect, nil)
	mk := func(ext string, age time.Duration) string {
		return mustInsertID(t, db, "collectTransaction", pgdoc.M{"billId": billID, "billingProfileId": profile.ID, "status": "PENDING",
			"externalId": ext, "paymentGatewayId": "gw-awx", "currency": "USD", "amount": decimalOf(t, "30"), "grossAmount": decimalOf(t, "30"),
			"exchangeRate": decimalOf(t, "1"), "creditCardId": cardID, "createdAt": time.Now().UTC().Add(-age)})
	}
	orphan := mk("", 72*time.Hour)
	if _, err := scanner.RecheckCollect(ctx, orphan); err == nil || !strings.Contains(err.Error(), "no gateway reference") {
		t.Fatalf("re-check of an orphan: %v", err)
	}
	if paid, _ := collect.CollectBillingProfile(ctx, profile); paid != 0 || fg.lastAmountCents != 0 {
		t.Fatalf("an orphan must block too: paid=%d cents=%d", paid, fg.lastAmountCents)
	}
	fresh := mk("", time.Minute)
	if _, err := scanner.CloseCollect(ctx, fresh); err == nil || !strings.Contains(err.Error(), "too recent") {
		t.Fatalf("a charge that may still be running: %v", err)
	}
	referenced := mk("pi_x", 72*time.Hour)
	if _, err := scanner.CloseCollect(ctx, referenced); err == nil {
		t.Fatal("a charge with a reference must be re-checked, not closed")
	}
	row, err := scanner.CloseCollect(ctx, orphan)
	if err != nil || row.Status != pricing.CollectTransactionStatusCancelled {
		t.Fatalf("%+v %v", row, err)
	}
	for _, other := range []string{fresh, referenced} { // not under test from here on
		if _, err := db.C("collectTransaction").SetFieldsOne(ctx, pgdoc.M{"_id": other}, pgdoc.M{"status": "CANCELLED"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Resolved: the bill can be charged again.
	if paid, err := collect.CollectBillingProfile(ctx, profile); err != nil || paid != 1 {
		t.Fatalf("paid=%d %v", paid, err)
	}
}

// A referenced charge ends only on what the gateway answers: this is the bill that used to stay blocked for good when a
// confirm response was lost and the card declined.
func TestStuckChargeGatewayAnswers(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)
	profile, cardID := seedPostpaid(t, db, billing.PaymentModePostpaid)
	billID := mustInsertID(t, db, "bill", pgdoc.M{"billingProfileId": profile.ID, "status": "SENT", "invoiceCurrency": "USD",
		"items": []any{pgdoc.M{"netAmount": decimalOf(t, "30")}}})
	var gw payment.Gateway
	factory := func(*billing.Gateway) payment.Gateway { return gw }
	collect := payment.NewCollectService(repo, pricingRepo, factory)
	scanner := payment.NewTransactionScanner(repo, payment.NewAddFundsService(repo, pricingRepo, factory), collect, nil)
	mk := func(age time.Duration) string {
		return mustInsertID(t, db, "collectTransaction", pgdoc.M{"billId": billID, "billingProfileId": profile.ID, "status": "PENDING",
			"externalId": "pi_stuck", "paymentGatewayId": "gw-awx", "currency": "USD", "amount": decimalOf(t, "30"), "grossAmount": decimalOf(t, "30"),
			"exchangeRate": decimalOf(t, "1"), "creditCardId": cardID, "createdAt": time.Now().UTC().Add(-age)})
	}

	clearCharges := func() { // not under test from here on
		if _, err := db.C("collectTransaction").SetFields(ctx, pgdoc.M{"billId": billID, "status": "PENDING"}, pgdoc.M{"status": "CANCELLED"}, nil); err != nil {
			t.Fatal(err)
		}
	}

	// Whatever may still capture stays open, a re-check says what the gateway answered, and close refuses it.
	stuck := mk(72 * time.Hour)
	for _, status := range []string{"processing", "requires_action", "requires_capture", "pending"} {
		gw = &statusGW{status: status}
		row, err := scanner.RecheckCollect(ctx, stuck)
		if err != nil || row.Status != pricing.CollectTransactionStatusPending || !strings.Contains(row.ErrorMessage, status) || !strings.Contains(row.ErrorMessage, "may still complete") {
			t.Fatalf("re-check %s: %+v %v", status, row, err)
		}
		if _, err := scanner.CloseCollect(ctx, stuck); err == nil || !strings.Contains(err.Error(), status) {
			t.Fatalf("close while %s: %v", status, err)
		}
	}
	// So does an answer that is final (re-check records it) and no answer at all.
	for _, g := range []payment.Gateway{&statusGW{status: "succeeded"}, &statusGW{status: "canceled"}, &flakyGW{err: errors.New("connection reset by peer")}} {
		gw = g
		if _, err := scanner.CloseCollect(ctx, stuck); err == nil {
			t.Fatalf("close with %#v must be refused", g)
		}
	}
	if row, _ := repo.CollectTransactionByID(ctx, stuck); row.Status != pricing.CollectTransactionStatusPending {
		t.Fatalf("a refused close changed the row: %+v", row)
	}

	// Declined: still PENDING by itself, the message says so, and an operator can end it. The bill can then be charged.
	gw = &statusGW{status: "requires_payment_method"}
	row, err := scanner.RecheckCollect(ctx, stuck)
	if err != nil || row.Status != pricing.CollectTransactionStatusPending || !strings.Contains(row.ErrorMessage, "did not succeed") {
		t.Fatalf("%+v %v", row, err)
	}
	if paid, _ := collect.CollectBillingProfile(ctx, profile); paid != 0 {
		t.Fatal("a declined charge blocks until it is closed")
	}
	if row, err = scanner.CloseCollect(ctx, stuck); err != nil || row.Status != pricing.CollectTransactionStatusCancelled || !strings.Contains(row.ErrorMessage, "requires_payment_method") {
		t.Fatalf("%+v %v", row, err)
	}
	if _, err := collect.CollectBill(ctx, profile, billID, payment.CollectBillRequest{CardID: cardID}); err != nil {
		t.Fatalf("the bill is free again, whatever this charge answers: %v", err)
	}
	clearCharges()

	// Cancelled at the gateway: terminal on a re-check and on a scan, with nothing paid.
	gw = &statusGW{status: "canceled"}
	stuck = mk(72 * time.Hour)
	if row, err = scanner.RecheckCollect(ctx, stuck); err != nil || row.Status != pricing.CollectTransactionStatusCancelled || !strings.Contains(row.ErrorMessage, "cancelled") {
		t.Fatalf("%+v %v", row, err)
	}
	inWindow := mk(time.Hour)
	if _, err := scanner.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if row, _ = repo.CollectTransactionByID(ctx, inWindow); row.Status != pricing.CollectTransactionStatusCancelled {
		t.Fatalf("scan: %+v", row)
	}
	if bill, _ := repo.BillByID(ctx, billID); bill.Status != pricing.BillStatusSent {
		t.Fatalf("a cancelled charge paid the bill: %s", bill.Status)
	}
	// A customer retry that finds a cancelled charge goes on to charge.
	pendingCancelled := mk(72 * time.Hour)
	before, _ := db.C("collectTransaction").Count(ctx, pgdoc.M{"billId": billID})
	gw = &statusGW{status: "canceled"}
	if _, err := collect.CollectBill(ctx, profile, billID, payment.CollectBillRequest{CardID: cardID}); err != nil {
		t.Fatal(err)
	}
	if row, _ = repo.CollectTransactionByID(ctx, pendingCancelled); row.Status != pricing.CollectTransactionStatusCancelled {
		t.Fatalf("%+v", row)
	}
	if after, _ := db.C("collectTransaction").Count(ctx, pgdoc.M{"billId": billID}); after != before+1 {
		t.Fatalf("no new charge: %d -> %d", before, after)
	}
	clearCharges()

	// On a bill paid some other way, a charge that may still capture is not written off either.
	if _, err := db.C("bill").SetByID(ctx, billID, pgdoc.M{"status": "PAID"}, nil); err != nil {
		t.Fatal(err)
	}
	gw = &statusGW{status: "processing"}
	late := mk(time.Hour)
	if _, err := scanner.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if row, _ = repo.CollectTransactionByID(ctx, late); row.Status != pricing.CollectTransactionStatusPending || !strings.Contains(row.ErrorMessage, "may still complete") {
		t.Fatalf("%+v", row)
	}
}

// TestScannerDoesNotOverwriteAWinningSettle: the row read at the start of the pass must not undo a settle that
// finished while the gateway call failed.
func TestScannerDoesNotOverwriteAWinningSettle(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()
	deposit := f.deposit(t, time.Now().UTC().Add(-time.Hour))
	if _, err := f.db.C("accountCreditTransaction").SetFieldsOne(ctx, pgdoc.M{"_id": deposit}, pgdoc.M{"status": "SUCCESS"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.UpdatePendingAccountCreditTransaction(ctx, deposit, "FAILED", "stale"); err != nil {
		t.Fatal(err)
	}
	if s := f.row(t, "accountCreditTransaction", deposit)["status"]; s != "SUCCESS" {
		t.Errorf("status = %v, want SUCCESS", s)
	}
}

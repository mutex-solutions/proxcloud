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

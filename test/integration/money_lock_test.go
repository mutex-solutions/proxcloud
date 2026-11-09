//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/payment"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// TestBillWritersShareTheCreditLock races manual payments against card charges settling on one bill.
// Every writer reads the bill, adds to it and replaces it, so without the lock a writer overwrites what
// another just recorded and money that came in disappears from the bill.
func TestBillWritersShareTheCreditLock(t *testing.T) {
	ctx := context.Background()
	f := newMoneyFixture(t)
	pay := billing.NewPayService(f.repo, pricing.NewRepo(f.db))
	collect := payment.NewCollectService(f.repo, pricing.NewRepo(f.db), func(*billing.Gateway) payment.Gateway { return f.gw })
	billID := mustInsertID(t, f.db, "bill", pgdoc.M{"billingProfileId": "bp-a", "status": "SENT", "invoiceCurrency": "USD",
		"items": []any{pgdoc.M{"resourceType": "instance", "currency": "USD", "netAmount": decimalOf(t, "1000")}}})

	// Stuck card charges of 5 each, which two scanners settle at the same time.
	const charges, manuals = 8, 8
	five, one := decimal.NewFromInt(5), decimal.NewFromInt(1)
	old := time.Now().UTC().Add(-time.Hour)
	for range charges {
		saved, err := f.repo.SaveCollectTransaction(ctx, &pricing.CollectTransaction{
			BillID: billID, BillingProfileID: "bp-a", Status: pricing.CollectTransactionStatusPending, PaymentGatewayID: "gw1",
			ExternalID: "pi_collect", Currency: "USD", Amount: &five, GrossAmount: &five, ExchangeRate: &one,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.C("collectTransaction").SetFieldsOne(ctx, pgdoc.M{"_id": saved.ID}, pgdoc.M{"createdAt": old}, nil); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range manuals {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, _, err := pay.RecordManualPayment(ctx, billID, billing.ManualPayment{Amount: &five, Method: "OTHER"}); err != nil {
				t.Errorf("record payment: %v", err)
			}
		}()
	}
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := payment.NewTransactionScanner(f.repo, f.svc, collect, nil).Scan(ctx); err != nil {
				t.Errorf("scan: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	bill, err := f.repo.BillByID(ctx, billID)
	if err != nil || bill == nil {
		t.Fatalf("bill: %v", err)
	}
	seen := map[string]int{}
	total := decimal.Zero
	for _, c := range bill.CollectedAmounts {
		seen[c.CollectTransactionID]++
		total = total.Add(*c.Amount)
	}
	if len(seen) != charges+manuals {
		t.Errorf("the bill names %d transactions, want %d", len(seen), charges+manuals)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("transaction %s is on the bill %d times", id, n)
		}
	}
	if want := decimal.NewFromInt((charges + manuals) * 5); !total.Equal(want) {
		t.Errorf("collected %s on the bill, want %s", total, want)
	}
}

// TestChargeSettlingOnAPaidBillBecomesCredit: the bill was paid some other way while the charge was out
// at the gateway. The money stands as credit instead of overpaying a closed bill.
func TestChargeSettlingOnAPaidBillBecomesCredit(t *testing.T) {
	ctx := context.Background()
	f := newMoneyFixture(t)
	collect := payment.NewCollectService(f.repo, pricing.NewRepo(f.db), func(*billing.Gateway) payment.Gateway { return f.gw })
	billID := mustInsertID(t, f.db, "bill", pgdoc.M{"billingProfileId": "bp-a", "status": "SENT", "invoiceCurrency": "USD",
		"items": []any{pgdoc.M{"resourceType": "instance", "currency": "USD", "netAmount": decimalOf(t, "10")}}})
	mustInsert(t, f.db, "creditCard", pgdoc.M{"_id": "card1", "billingProfileId": "bp-a", "paymentGatewayId": "gw1", "tokenId": "pm_x"})

	// The gateway answers the charge while an operator records the bank transfer that paid the bill.
	pay := billing.NewPayService(f.repo, pricing.NewRepo(f.db))
	f.gw.onCollect = func() {
		if _, _, err := pay.RecordManualPayment(ctx, billID, billing.ManualPayment{Method: "OTHER"}); err != nil {
			t.Errorf("record payment: %v", err)
		}
	}
	txn, err := collect.CollectBill(ctx, f.profile, billID, payment.CollectBillRequest{CardID: "card1"})
	if err != nil {
		t.Fatal(err)
	}
	if txn.Status != pricing.CollectTransactionStatusSuccess || txn.Metadata["surplusAccountCreditId"] == nil {
		t.Fatalf("charge = %s %v, want SUCCESS with a surplus credit", txn.Status, txn.Metadata)
	}
	if n := f.credits(t); n != 1 {
		t.Errorf("%d credits, want 1", n)
	}
	bill, _ := f.repo.BillByID(ctx, billID)
	if len(bill.CollectedAmounts) != 1 {
		t.Errorf("%d collected amounts on the paid bill, want only the bank transfer", len(bill.CollectedAmounts))
	}
}

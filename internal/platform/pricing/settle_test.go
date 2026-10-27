package pricing

import (
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

var settleNow = time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)

// pairClient returns rates keyed by "BASE->EXCHANGED" (only consulted for unequal
// currencies — the Exchanger short-circuits equal pairs to 1).
type pairClient struct {
	rates map[string]decimal.Decimal
	calls int
}

func (c *pairClient) GetExchangeRate(base, exchanged string, _ time.Time) (decimal.Decimal, error) {
	c.calls++
	if r, ok := c.rates[base+"->"+exchanged]; ok {
		return r, nil
	}
	return decimal.Zero, fmt.Errorf("no rate %s->%s", base, exchanged)
}

func promoCredit(id, remaining string) *PromotionalCredit {
	return &PromotionalCredit{ID: id, RemainingAmount: dp(remaining)}
}

func acctCredit(id, amount, currency, invCurrency, invRate string) *AccountCredit {
	t := settleNow
	return &AccountCredit{ID: id, Amount: dp(amount), Currency: currency, InvoiceCurrency: invCurrency, InvoiceExchangeRate: dp(invRate), CreatedAt: &t}
}

func TestTakePromotionalCredits(t *testing.T) {
	x := NewExchanger(&pairClient{}) // base==profile below → no rate fetch
	t.Run("full_cover", func(t *testing.T) {
		c := promoCredit("p1", "100")
		applied, err := TakePromotionalCredits([]*PromotionalCredit{c}, mustDec("30"), "USD", "USD", x, settleNow)
		if err != nil || len(applied) != 1 || !applied[0].Amount.Equal(mustDec("30")) || !c.RemainingAmount.Equal(mustDec("70")) {
			t.Fatalf("applied=%v rem=%v err=%v", applied, c.RemainingAmount, err)
		}
		if applied[0].InvoiceCurrency != "" || applied[0].GrossAmount != nil || applied[0].Currency != "USD" {
			t.Errorf("promo applied metadata: invCcy/gross must be empty, currency=base; got %+v", applied[0])
		}
	})
	t.Run("partial_then_next", func(t *testing.T) {
		c0, c1 := promoCredit("p0", "40"), promoCredit("p1", "100")
		applied, _ := TakePromotionalCredits([]*PromotionalCredit{c0, c1}, mustDec("120"), "USD", "USD", x, settleNow)
		if len(applied) != 2 || !applied[0].Amount.Equal(mustDec("40")) || !applied[1].Amount.Equal(mustDec("80")) {
			t.Fatalf("applied=%v", applied)
		}
		if !c0.RemainingAmount.IsZero() || !c1.RemainingAmount.Equal(mustDec("20")) {
			t.Errorf("balances c0=%v c1=%v", c0.RemainingAmount, c1.RemainingAmount)
		}
	})
	t.Run("under_coverage_drops_leftover", func(t *testing.T) {
		c0, c1 := promoCredit("p0", "10"), promoCredit("p1", "20")
		applied, _ := TakePromotionalCredits([]*PromotionalCredit{c0, c1}, mustDec("100"), "USD", "USD", x, settleNow)
		if len(applied) != 2 || !c0.RemainingAmount.IsZero() || !c1.RemainingAmount.IsZero() {
			t.Fatalf("applied=%v c0=%v c1=%v", applied, c0.RemainingAmount, c1.RemainingAmount)
		}
	})
	t.Run("exact_cover_diff_zero", func(t *testing.T) {
		c := promoCredit("p1", "50")
		applied, _ := TakePromotionalCredits([]*PromotionalCredit{c}, mustDec("50"), "USD", "USD", x, settleNow)
		if len(applied) != 1 || !applied[0].Amount.Equal(mustDec("50")) || !c.RemainingAmount.IsZero() {
			t.Fatalf("applied=%v rem=%v", applied, c.RemainingAmount)
		}
	})
}

func TestSettleAccountCredits(t *testing.T) {
	t.Run("same_currency_full_cover", func(t *testing.T) {
		x := NewExchanger(&pairClient{})
		c := acctCredit("a1", "100", "USD", "USD", "1")
		s, err := SettleAccountCredits([]*AccountCredit{c}, mustDec("40"), "USD", x, settleNow)
		if err != nil || len(s) != 1 || !s[0].Settled.Equal(mustDec("40")) || !c.Amount.Equal(mustDec("60")) {
			t.Fatalf("settled=%v balance=%v err=%v", s, c.Amount, err)
		}
	})
	t.Run("cross_currency_non_reciprocal", func(t *testing.T) {
		// gross USD; credit base USD invoice RON; USD->RON=3 but RON->USD=0.30 (not 1/3).
		x := NewExchanger(&pairClient{rates: map[string]decimal.Decimal{"USD->RON": mustDec("3"), "RON->USD": mustDec("0.30")}})
		c := acctCredit("a1", "100", "USD", "RON", "1")
		s, err := SettleAccountCredits([]*AccountCredit{c}, mustDec("30"), "USD", x, settleNow)
		// remInternal=30*3=90; creditAmount=100*3=300; diff>=0 → settled=90 (RON); covered=90*0.30=27 (USD).
		if err != nil || !s[0].Settled.Equal(mustDec("27")) || !c.Amount.Equal(mustDec("73")) {
			t.Fatalf("covered=%v balance=%v err=%v (want 27 / 73)", s[0].Settled, c.Amount, err)
		}
	})
}

func TestSettleBillAmountWaterfall(t *testing.T) {
	x := NewExchanger(&pairClient{})
	bill := &Bill{InvoiceCurrency: "USD", Items: []BillItem{{NetAmount: mustDec("100")}}}
	promo := []*PromotionalCredit{promoCredit("p1", "30")}
	acct := []*AccountCredit{acctCredit("a1", "200", "USD", "USD", "1")}
	gotPromo, gotAcct, err := SettleBillAmount(bill, mustDec("100"), promo, acct, []TaxRate{taxRate(lvl(0, 19))}, "USD", "USD", x, settleNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotPromo) != 1 || len(gotAcct) != 1 {
		t.Fatalf("promo=%d acct=%d", len(gotPromo), len(gotAcct))
	}
	if len(bill.AppliedPromotionalCredits) != 1 || !bill.AppliedPromotionalCredits[0].Amount.Equal(mustDec("30")) {
		t.Errorf("applied promo = %v", bill.AppliedPromotionalCredits)
	}
	aac := bill.AppliedAccountCredits[0]
	if !aac.Amount.Equal(mustDec("70")) || !aac.GrossAmount.Equal(mustDec("83.30")) { // 70 net, +19% tax
		t.Errorf("applied account amount=%v gross=%v, want 70 / 83.30", aac.Amount, aac.GrossAmount)
	}
	if !promo[0].RemainingAmount.IsZero() || !acct[0].Amount.Equal(mustDec("130")) {
		t.Errorf("balances promo=%v acct=%v, want 0 / 130", promo[0].RemainingAmount, acct[0].Amount)
	}
	if u := GetUnpaidAmountBillProductCurrency(bill); !u.Equal(mustDec("0")) {
		t.Errorf("unpaid = %v, want 0", u)
	}
}

// TestSettleBillAmountNilInvoiceExchangeRate replicates the stored-credit shape that froze the
// prod monthly bill run: invoiceCurrency set, invoiceExchangeRate ABSENT (a deposit-minted or
// legacy credit). Settling must default the rate to 1 instead of nil-panicking.
func TestSettleBillAmountNilInvoiceExchangeRate(t *testing.T) {
	x := NewExchanger(&pairClient{})
	bill := &Bill{InvoiceCurrency: "USD", Items: []BillItem{{NetAmount: mustDec("100")}}}
	c := acctCredit("a1", "200", "USD", "USD", "1")
	c.InvoiceExchangeRate = nil
	_, acct, err := SettleBillAmount(bill, mustDec("100"), nil, []*AccountCredit{c}, nil, "USD", "USD", x, settleNow)
	if err != nil || len(acct) != 1 {
		t.Fatalf("acct=%v err=%v", acct, err)
	}
	aac := bill.AppliedAccountCredits[0]
	if !aac.Amount.Equal(mustDec("100")) || !aac.ExchangeRate.Equal(mustDec("1")) || !aac.GrossAmount.Equal(mustDec("100")) {
		t.Errorf("applied = %+v, want amount 100 @ rate 1", aac)
	}
	if !c.Amount.Equal(mustDec("100")) {
		t.Errorf("credit balance = %v, want 100", c.Amount)
	}
}

func TestSettleAccountCreditAmountScale(t *testing.T) {
	x := NewExchanger(&pairClient{})
	bill := &Bill{InvoiceCurrency: "USD", Items: []BillItem{{NetAmount: mustDec("10.005")}}}
	acct := []*AccountCredit{acctCredit("a1", "10.005", "USD", "USD", "1")}
	if _, _, err := SettleBillAmount(bill, mustDec("10.005"), nil, acct, nil, "USD", "USD", x, settleNow); err != nil {
		t.Fatal(err)
	}
	if !bill.AppliedAccountCredits[0].Amount.Equal(mustDec("10.01")) { // scaleHalfUp(10.005) = 10.01
		t.Errorf("amount = %v, want 10.01", bill.AppliedAccountCredits[0].Amount)
	}
}

func TestApplyPaidCollectOnBill(t *testing.T) {
	t.Run("same_currency_identity", func(t *testing.T) {
		bill := &Bill{InvoiceCurrency: "USD", Items: []BillItem{{NetAmount: mustDec("100")}}}
		ct := &CollectTransaction{ID: "c1", Currency: "USD", Amount: dp("50"), GrossAmount: dp("59.5"), ExchangeRate: dp("1")}
		ApplyPaidCollectOnBill(bill, ct, "USD")
		cc := bill.CollectedAmounts[0]
		if !cc.Amount.Equal(mustDec("50")) || !cc.GrossAmount.Equal(mustDec("59.5")) || cc.InvoiceCurrency != "USD" {
			t.Errorf("collected = %+v", cc)
		}
	})
	t.Run("cross_currency_frozen_rate_divide", func(t *testing.T) {
		bill := &Bill{InvoiceCurrency: "USD", Items: []BillItem{{NetAmount: mustDec("100")}}}
		ct := &CollectTransaction{ID: "c1", Currency: "RON", Amount: dp("100.00"), ExchangeRate: dp("4.5")}
		ApplyPaidCollectOnBill(bill, ct, "USD")
		if !bill.CollectedAmounts[0].Amount.Equal(mustDec("22.22")) { // 100.00 / 4.5 HALF_UP @ scale 2
			t.Errorf("amount = %v, want 22.22", bill.CollectedAmounts[0].Amount)
		}
	})
	t.Run("flips_paid", func(t *testing.T) {
		bill := &Bill{InvoiceCurrency: "USD", Status: BillStatusSent, Items: []BillItem{{NetAmount: mustDec("100")}},
			AppliedAccountCredits: []AppliedAccountCredit{{Amount: dp("80")}}}
		ct := &CollectTransaction{ID: "c1", Currency: "USD", Amount: dp("25"), ExchangeRate: dp("1")}
		ApplyPaidCollectOnBill(bill, ct, "USD") // unpaid = 100 - 80 - 25 = -5 ≤ 0
		if bill.Status != BillStatusPaid {
			t.Errorf("status = %s, want PAID", bill.Status)
		}
	})
}

func TestGetUnpaidAmount(t *testing.T) {
	t.Run("nil_buckets_safe", func(t *testing.T) {
		bill := &Bill{Items: []BillItem{{NetAmount: mustDec("30")}}, AppliedPromotionalCredits: []AppliedPromotionalCredit{{Amount: dp("10")}}}
		if u := GetUnpaidAmountBillProductCurrency(bill); !u.Equal(mustDec("20")) {
			t.Errorf("unpaid = %v, want 20", u)
		}
	})
	t.Run("with_adjustments", func(t *testing.T) {
		bill := &Bill{Items: []BillItem{{NetAmount: mustDec("100")}}, Adjustments: []BillAdjustment{{Amount: dp("-15")}},
			AppliedAccountCredits: []AppliedAccountCredit{{Amount: dp("20")}}}
		if u := GetUnpaidAmountBillProductCurrency(bill); !u.Equal(mustDec("65")) { // (100-15) - 20
			t.Errorf("unpaid = %v, want 65", u)
		}
	})
}

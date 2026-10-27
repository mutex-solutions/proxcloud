package pricing

import (
	"testing"
	"time"
)

func TestBillGrossInCurrencyTaxThenFX(t *testing.T) {
	now := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	bill := &Bill{InvoiceCurrency: "USD", Items: []BillItem{{NetAmount: mustDec("100.00")}}}
	rates := []TaxRate{taxRate(lvl(0, 19))}

	t.Run("tax_first_then_fx", func(t *testing.T) {
		x := NewExchanger(&stubClient{rate: mustDec("4")})
		got, err := BillGrossInCurrency(bill, rates, "RON", "USD", now, x)
		if err != nil {
			t.Fatal(err)
		}
		// gross base = 100*1.19 = 119.00 ; then ×4 = 476.00. (NOT tax-of-FX'd-net = 100*4*1.19 also 476 here,
		// but FX-then-tax would tax in target ccy — the contract is tax in base FIRST.)
		if !got.Equal(mustDec("476")) {
			t.Errorf("gross = %s, want 476", got)
		}
	})
	t.Run("same_currency_no_fetch", func(t *testing.T) {
		c := &stubClient{rate: mustDec("4")}
		got, _ := BillGrossInCurrency(bill, rates, "USD", "USD", now, NewExchanger(c))
		if !got.Equal(mustDec("119")) || c.calls != 0 {
			t.Errorf("gross = %s calls = %d, want 119 / 0", got, c.calls)
		}
	})
	t.Run("includes_adjustment", func(t *testing.T) {
		// A −10 BillAdjustment (e.g. a savings-contract discount) must lower the taxed gross:
		// net-with-adj = 100 − 10 = 90 ; gross = 90 × 1.19 = 107.10 (items-only would wrongly give 119).
		adj := mustDec("-10")
		adjBill := &Bill{InvoiceCurrency: "USD", Items: []BillItem{{NetAmount: mustDec("100.00")}}, Adjustments: []BillAdjustment{{Amount: &adj}}}
		got, _ := BillGrossInCurrency(adjBill, rates, "USD", "USD", now, NewExchanger(&stubClient{rate: mustDec("4")}))
		if !got.Equal(mustDec("107.10")) {
			t.Errorf("gross with adjustment = %s, want 107.10", got)
		}
	})
}

func TestBillNetInProfileCurrency(t *testing.T) {
	now := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	bill := &Bill{InvoiceCurrency: "USD", Items: []BillItem{{NetAmount: mustDec("50")}}}

	t.Run("identity_when_same", func(t *testing.T) {
		c := &stubClient{rate: mustDec("1.5")}
		got, _ := BillNetInProfileCurrency(bill, "USD", "USD", now, NewExchanger(c))
		if !got.Equal(mustDec("50")) || c.calls != 0 {
			t.Errorf("net = %s calls = %d, want 50 / 0", got, c.calls)
		}
	})
	t.Run("multiply_to_profile", func(t *testing.T) {
		got, _ := BillNetInProfileCurrency(bill, "RON", "USD", now, NewExchanger(&stubClient{rate: mustDec("1.5")}))
		if !got.Equal(mustDec("75")) {
			t.Errorf("net = %s, want 75", got)
		}
	})
}

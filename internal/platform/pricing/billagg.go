package pricing

import (
	"time"

	"github.com/shopspring/decimal"
)

// ScaleUpItems: at send
// time, re-round every item's net from the build scale (16) down to scale 2 (HALF_UP).
func ScaleUpItems(bill *Bill) {
	for i := range bill.Items {
		bill.Items[i].NetAmount = scaleHalfUp(bill.Items[i].NetAmount)
	}
}

// BillNetBaseCurrency: Σ item net
// amounts, in the bill's base/product currency. (Adjustments are deferred, so this
// is the plain item sum — the WithAdjustments variant lands with the adjustment slice.)
func BillNetBaseCurrency(bill *Bill) decimal.Decimal {
	total := decimal.Zero
	for i := range bill.Items {
		total = total.Add(bill.Items[i].NetAmount)
	}
	return total
}

// BillGrossInCurrency taxes the bill net (base
// currency) FIRST, then FX-converts the gross to the target currency. This ordering
// (tax → FX, on the bill AGGREGATE in base currency) is required. The net
// taxed is the WITH-ADJUSTMENTS net —
// a savings-discount / price-adjustment-rule BillAdjustment must move the taxed gross too
// (items-only would under/over-tax a bill carrying adjustments).
func BillGrossInCurrency(bill *Bill, rates []TaxRate, targetCurrency, baseCurrency string, now time.Time, x *Exchanger) (decimal.Decimal, error) {
	gross := CalculateGrossAmount(GetNetAmountBillProductCurrencyWithAdjustments(bill), rates)
	rate, err := x.GetExchangeRate(baseCurrency, targetCurrency, now)
	if err != nil {
		return decimal.Zero, err
	}
	return rate.Mul(gross), nil
}

// BillNetInProfileCurrency converts the bill net (base
// currency, WITH adjustments) to the billing-profile currency (multiply; identity when equal).
func BillNetInProfileCurrency(bill *Bill, profileCurrency, baseCurrency string, now time.Time, x *Exchanger) (decimal.Decimal, error) {
	return x.ExchangeToBillingProfileCurrency(GetNetAmountBillProductCurrencyWithAdjustments(bill), profileCurrency, baseCurrency, now)
}

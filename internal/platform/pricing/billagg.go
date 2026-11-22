package pricing

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// ScaleUpItems: at send
// time, re-round every item's net from the build scale (16) down to scale 2 (HALF_UP).
//
// Sandboxes are the one type whose items are often worth less than half a cent each (an agent's
// sandbox lives a minute or two, and so does a build), so rounded one by one they were all
// invoiced 0.00, however many a project ran. What the sandbox items that round to 0.00 accrued is
// therefore summed per project and invoiced as ONE added line, rounded once. Nothing else moves:
// every item, those sandboxes' included, is rounded as before (they stay on the bill at 0.00,
// with their minutes and applied amounts), and a bill without such sandboxes is sent as it was.
// Applied again to a bill already scaled it adds nothing: the items are 0.00 by then.
// ponytail: a sandbox item of 0.005 or more is rounded by itself, like every other item.
func ScaleUpItems(bill *Bill) {
	type short struct {
		sum      decimal.Decimal
		n        int
		currency string
	}
	shorts := map[string]*short{}
	var projects []string // in the order of their first short sandbox
	for i := range bill.Items {
		it := &bill.Items[i]
		net := scaleHalfUp(it.NetAmount)
		if it.ResourceType == sandboxResourceType && net.IsZero() && !it.NetAmount.IsZero() {
			s := shorts[it.ProjectID]
			if s == nil {
				s = &short{currency: it.Currency}
				shorts[it.ProjectID] = s
				projects = append(projects, it.ProjectID)
			}
			s.sum, s.n = s.sum.Add(it.NetAmount), s.n+1
		}
		it.NetAmount = net
	}
	for _, p := range projects {
		s := shorts[p]
		if net := scaleHalfUp(s.sum); net.IsPositive() {
			bill.Items = append(bill.Items, BillItem{
				Name:                  fmt.Sprintf("Sandbox runs below 0.005 each (%d)", s.n),
				ProjectID:             p,
				ResourceType:          sandboxResourceType,
				Currency:              s.currency,
				NetAmount:             net,
				AppliedPricePlanRules: []BillItemAppliedPricePlanRule{},
			})
		}
	}
}

// sandboxResourceType is the billing resource type of a sandbox, and of a template or function
// build (billingresource.SandboxProvider).
const sandboxResourceType = "sandbox"

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

package billing

import (
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// BillDto + BillItemDto are the client wire shapes for GET /api/v1/bill/{bp}[/{billId}].
// Money is a JSON NUMBER (a big-decimal value), so the fields are json.Number — never
// money.Money (which quotes).
//
// Null-omit behavior: toBillDto never sets vatRate/dueAt/sentAt
// → they stay null → OMITTED; it never sets year/month → they stay 0 → primitive ints, EMITTED.
// The gross/net/unpaid math reuses the existing pricing layer (tax → FX), so this is the
// I/O mapping only.
type BillItemDto struct {
	Name         string `json:"name,omitempty"`
	ResourceID   string `json:"resourceId,omitempty"`
	ResourceType string `json:"resourceType,omitempty"`
	// ProjectID lets the (org-scoped) bill be grouped/filtered per project on the client.
	ProjectID string      `json:"projectId,omitempty"`
	Currency  string      `json:"currency,omitempty"`
	NetAmount json.Number `json:"netAmount"`
}

type BillDto struct {
	ID                        string                             `json:"id,omitempty"`
	Status                    string                             `json:"status,omitempty"`
	Items                     []BillItemDto                      `json:"items"`
	BillingCycle              *pricing.BillBillingCycle          `json:"billingCycle,omitempty"`
	InvoiceNumber             string                             `json:"invoiceNumber,omitempty"`
	InvoiceCurrency           string                             `json:"invoiceCurrency,omitempty"`
	BillingProfileID          string                             `json:"billingProfileId,omitempty"`
	GrossAmount               json.Number                        `json:"grossAmount"`
	NetAmount                 json.Number                        `json:"netAmount"`
	UnpaidGrossAmount         json.Number                        `json:"unpaidGrossAmount"`
	AppliedAccountCredits     []pricing.AppliedAccountCredit     `json:"appliedAccountCredits,omitempty"`
	CollectedAmounts          []pricing.AppliedCollectedCredit   `json:"collectedAmounts,omitempty"`
	AppliedPromotionalCredits []pricing.AppliedPromotionalCredit `json:"appliedPromotionalCredits,omitempty"`
	Adjustments               []pricing.BillAdjustment           `json:"adjustments,omitempty"`
	// toBillDto leaves year/month at 0 (primitive ints — always emitted, NON_NULL keeps them).
	Year  int `json:"year"`
	Month int `json:"month"`
	// vatRate/dueAt/sentAt are NOT set by toBillDto → null → omitted (NON_NULL).
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

// ToBillDto builds the client BillDto from (billingProfile, bill). `rates` are the profile's tax
// rates, `baseCurrency` the billingConfiguration base currency, `x` the FX exchanger
// (same-currency ⇒ identity, no fetch); `now` is the FX-rate instant. The net base is the
// with-adjustments product-currency net (matches getNetAmountBillProductCurrencyWithAdjustments).
func ToBillDto(profile *BillingProfile, bill *pricing.Bill, rates []pricing.TaxRate, baseCurrency string, x *pricing.Exchanger, now time.Time) (*BillDto, error) {
	// A profile an operator activated before it had billing details carries no currency: it bills in the base currency.
	currency := profile.Currency
	if currency == "" {
		currency = baseCurrency
	}
	netBase := pricing.GetNetAmountBillProductCurrencyWithAdjustments(bill)
	netAmount, err := x.ExchangeToBillingProfileCurrency(netBase, currency, baseCurrency, now)
	if err != nil {
		return nil, err
	}
	rate, err := x.GetExchangeRate(baseCurrency, currency, now)
	if err != nil {
		return nil, err
	}
	grossAmount := rate.Mul(pricing.CalculateGrossAmount(netBase, rates))

	unpaid := decimal.Zero
	if bill.Status == pricing.BillStatusSent {
		unpaidGrossBase := pricing.CalculateGrossAmount(pricing.GetUnpaidAmountBillProductCurrency(bill), rates)
		unpaid, err = x.ExchangeToBillingProfileCurrency(unpaidGrossBase, currency, baseCurrency, now)
		if err != nil {
			return nil, err
		}
	}

	// Items and adjustments are stored in the base currency; the net and gross above are in the profile's. Every amount
	// goes out in the profile's currency, so the bill's lines add up to its total.
	items := make([]BillItemDto, 0, len(bill.Items))
	for i := range bill.Items {
		it := &bill.Items[i]
		items = append(items, BillItemDto{
			Name:         it.Name,
			ResourceID:   it.ResourceID,
			ResourceType: it.ResourceType,
			ProjectID:    it.ProjectID,
			Currency:     currency,
			NetAmount:    num(rate.Mul(it.NetAmount)),
		})
	}
	var adjustments []pricing.BillAdjustment
	for _, a := range bill.Adjustments {
		if a.Amount != nil {
			v := rate.Mul(*a.Amount)
			a.Amount = &v
		}
		adjustments = append(adjustments, a)
	}

	return &BillDto{
		ID:                        bill.ID,
		Status:                    string(bill.Status),
		Items:                     items,
		BillingCycle:              bill.BillingCycle,
		InvoiceNumber:             bill.InvoiceNumber,
		InvoiceCurrency:           bill.InvoiceCurrency,
		BillingProfileID:          bill.BillingProfileID,
		GrossAmount:               num(grossAmount),
		NetAmount:                 num(netAmount),
		UnpaidGrossAmount:         num(unpaid),
		AppliedAccountCredits:     bill.AppliedAccountCredits,
		CollectedAmounts:          bill.CollectedAmounts,
		AppliedPromotionalCredits: bill.AppliedPromotionalCredits,
		Adjustments:               adjustments,
		CreatedAt:                 bill.CreatedAt,
		UpdatedAt:                 bill.UpdatedAt,
	}, nil
}

// num renders a Decimal as a JSON number. The canonical decimal string is
// emitted unquoted; scale-exact big-decimal matching is a refinement.
func num(d decimal.Decimal) json.Number { return json.Number(d.String()) }

package billing

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// An operator can activate a profile before it has billing details, so it has no currency: its
// bills read in the base currency instead of failing (the bill list answered 500).
func TestToBillDto_NoProfileCurrency(t *testing.T) {
	dto, err := ToBillDto(&BillingProfile{}, goldenBill(pricing.BillStatusSent, "1.25"), nil, "USD", pricing.NewExchanger(nil), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if dto.NetAmount != "1.25" || dto.UnpaidGrossAmount != "1.25" {
		t.Fatalf("net = %s, unpaid = %s", dto.NetAmount, dto.UnpaidGrossAmount)
	}
}

type fixedRate decimal.Decimal

func (r fixedRate) GetExchangeRate(string, string, time.Time) (decimal.Decimal, error) {
	return decimal.Decimal(r), nil
}

// A profile in its own currency: the net is converted, so the items and adjustments must be too, or the bill page's
// subtotal (net minus adjustments) mixes currencies (EUR at 0.9, items $100, adjustment -$10 printed 91.00, not 90).
func TestToBillDto_OneCurrency(t *testing.T) {
	bill := goldenBill(pricing.BillStatusOpen, "100")
	minus10 := decimal.RequireFromString("-10")
	bill.Adjustments = []pricing.BillAdjustment{{Amount: &minus10, Type: "SAVINGS_CONTRACT"}}
	x := pricing.NewExchanger(fixedRate(decimal.RequireFromString("0.9")))
	dto, err := ToBillDto(&BillingProfile{Currency: "EUR"}, bill, nil, "USD", x, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	dec := func(s string) decimal.Decimal { return decimal.RequireFromString(s) }
	net, item, adj := dec(string(dto.NetAmount)), dec(string(dto.Items[0].NetAmount)), *dto.Adjustments[0].Amount
	if !net.Equal(dec("81")) || !item.Equal(dec("90")) || !adj.Equal(dec("-9")) || dto.Items[0].Currency != "EUR" {
		t.Fatalf("net %s item %s %s adjustment %s", net, item, dto.Items[0].Currency, adj)
	}
	if !net.Sub(adj).Equal(item) {
		t.Fatalf("subtotal %s is not the items' %s", net.Sub(adj), item)
	}
	if !bill.Adjustments[0].Amount.Equal(minus10) {
		t.Fatalf("the stored adjustment changed: %s", bill.Adjustments[0].Amount)
	}
}

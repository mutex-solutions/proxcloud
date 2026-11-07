package billing

import (
	"testing"
	"time"

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

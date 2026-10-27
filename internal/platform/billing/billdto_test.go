package billing

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

func goldenBill(status pricing.BillStatus, itemNet string) *pricing.Bill {
	cs := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	ce := cs.AddDate(0, 1, 0)
	return &pricing.Bill{
		ID: "bill-1", Status: status, BillingProfileID: "bp-1", InvoiceCurrency: "USD",
		BillingCycle: &pricing.BillBillingCycle{StartDate: &cs, EndDate: &ce},
		Items: []pricing.BillItem{{
			Name: "instance_traffic", ResourceID: "instance_traffic-x", ResourceType: "instance_traffic",
			Currency: "USD", NetAmount: decimal.RequireFromString(itemNet),
		}},
	}
}

// TestToBillDto_OpenNoTax: same-currency (FX identity) + no tax rates ⇒ gross == net == the
// item sum; OPEN bill ⇒ unpaidGrossAmount = 0. Uses the live golden bill shape.
func TestToBillDto_OpenNoTax(t *testing.T) {
	profile := &BillingProfile{Currency: "USD"}
	bill := goldenBill(pricing.BillStatusOpen, "6.9454374396006267")
	x := pricing.NewExchanger(nil) // same-currency path never calls the client

	dto, err := ToBillDto(profile, bill, nil, "USD", x, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// net is the raw item sum (FX identity, not scaled); gross is scaleHalfUp(2) even with no
	// tax (calculateGrossAmount always 2-scales) — so gross == 6.95, net stays full-scale.
	if dto.NetAmount != "6.9454374396006267" {
		t.Fatalf("netAmount = %s, want 6.9454374396006267", dto.NetAmount)
	}
	wantGross := json.Number(pricing.CalculateGrossAmount(decimal.RequireFromString("6.9454374396006267"), nil).String())
	if dto.GrossAmount != wantGross {
		t.Fatalf("grossAmount = %s, want %s (scaleHalfUp(2) of net)", dto.GrossAmount, wantGross)
	}
	if dto.UnpaidGrossAmount != "0" {
		t.Fatalf("unpaidGrossAmount (OPEN) = %s, want 0", dto.UnpaidGrossAmount)
	}
	if len(dto.Items) != 1 || dto.Items[0].NetAmount != "6.9454374396006267" || dto.Items[0].ResourceType != "instance_traffic" {
		t.Fatalf("item mapping wrong: %+v", dto.Items)
	}
	if dto.Status != "OPEN" || dto.BillingProfileID != "bp-1" || dto.InvoiceCurrency != "USD" {
		t.Fatalf("passthrough wrong: %+v", dto)
	}
}

// TestToBillDto_SentUnpaid: a SENT bill with no applied credits ⇒ unpaidGrossAmount == net
// (gross of the full unpaid net; no tax here).
func TestToBillDto_SentUnpaid(t *testing.T) {
	profile := &BillingProfile{Currency: "USD"}
	bill := goldenBill(pricing.BillStatusSent, "10.5")
	dto, err := ToBillDto(profile, bill, nil, "USD", pricing.NewExchanger(nil), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	wantUnpaid := json.Number(pricing.CalculateGrossAmount(decimal.RequireFromString("10.5"), nil).String())
	if dto.UnpaidGrossAmount != wantUnpaid {
		t.Fatalf("unpaidGrossAmount (SENT, no credits) = %s, want %s", dto.UnpaidGrossAmount, wantUnpaid)
	}
}

// TestBillDtoJSON_NonNull: year/month emitted as 0; vatRate/dueAt/sentAt omitted; items always
// present; money fields are JSON NUMBERS (unquoted).
func TestBillDtoJSON_NonNull(t *testing.T) {
	profile := &BillingProfile{Currency: "USD"}
	dto, err := ToBillDto(profile, goldenBill(pricing.BillStatusOpen, "1"), nil, "USD", pricing.NewExchanger(nil), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"year", "month", "items", "grossAmount", "netAmount", "unpaidGrossAmount", "status"} {
		if _, ok := m[k]; !ok {
			t.Errorf("expected key %q present", k)
		}
	}
	for _, k := range []string{"vatRate", "dueAt", "sentAt"} {
		if _, ok := m[k]; ok {
			t.Errorf("key %q must be omitted (null, never set by toBillDto)", k)
		}
	}
	if string(m["year"]) != "0" || string(m["month"]) != "0" {
		t.Errorf("year/month must serialize as 0, got year=%s month=%s", m["year"], m["month"])
	}
	// money must be an unquoted JSON number, not a quoted string (big-decimal → number)
	for _, k := range []string{"grossAmount", "netAmount", "unpaidGrossAmount"} {
		if v := string(m[k]); len(v) == 0 || v[0] == '"' {
			t.Errorf("%s must be an unquoted JSON number, got %s", k, v)
		}
	}
}

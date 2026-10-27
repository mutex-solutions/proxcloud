package admin

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
)

func dec(s string) *decimal.Decimal { d, _ := decimal.NewFromString(s); return &d }

func TestCreateSavingsContractReqDecode(t *testing.T) {
	var req createSavingsContractReq
	body := `{"savingsPlanId":"sp1","durationMonths":12,"monthlyCommittedAmount":100.50,"paidUpfront":true,"startDate":"NEXT_MONTH"}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.SavingsPlanID != "sp1" || req.DurationMonths != 12 || !req.PaidUpfront || req.StartDate != "NEXT_MONTH" {
		t.Errorf("decoded req mismatch: %+v", req)
	}
	if req.MonthlyCommittedAmount == nil || !req.MonthlyCommittedAmount.Equal(decimal.RequireFromString("100.50")) {
		t.Errorf("monthlyCommittedAmount mismatch: %v", req.MonthlyCommittedAmount)
	}
}

func TestSavingsContractStartDate(t *testing.T) {
	now := time.Now().UTC()
	cur, err := savingsContractStartDate("CURRENT_MONTH")
	if err != nil {
		t.Fatalf("CURRENT_MONTH err: %v", err)
	}
	if cur.Year() != now.Year() || cur.Month() != now.Month() || cur.Day() != 1 ||
		cur.Hour() != 0 || cur.Minute() != 0 || cur.Location() != time.UTC {
		t.Errorf("CURRENT_MONTH = %v, want first-of-month midnight UTC", cur)
	}
	next, err := savingsContractStartDate("NEXT_MONTH")
	if err != nil {
		t.Fatalf("NEXT_MONTH err: %v", err)
	}
	wantNext := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	if !next.Equal(wantNext) {
		t.Errorf("NEXT_MONTH = %v, want %v", next, wantNext)
	}
	if _, e := savingsContractStartDate("WHENEVER"); e == nil {
		t.Errorf("bad enum should error")
	} else if e.Status != 400 {
		t.Errorf("bad enum status = %d, want 400", e.Status)
	}
}

func TestSavingsContractDiscountRate(t *testing.T) {
	schedule := &billing.SavingsPlanSchedule{
		DurationMonths: 12,
		NoUpfrontTiers: []billing.SavingsPlanTier{
			{StartAmount: dec("0"), Discount: dec("0.05")},
			{StartAmount: dec("100"), Discount: dec("0.10")},
			{StartAmount: dec("500"), Discount: dec("0.20")},
		},
		UpfrontTiers: []billing.SavingsPlanTier{
			{StartAmount: dec("0"), Discount: dec("0.15")},
			{StartAmount: dec("100"), Discount: dec("0.25")},
		},
	}

	// monthly=150, no-upfront → tiers 0 and 100 qualify → max discount 0.10.
	got, err := savingsContractDiscountRate(false, schedule, decimal.RequireFromString("150"))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !got.Equal(decimal.RequireFromString("0.10")) {
		t.Errorf("no-upfront 150 discount = %s, want 0.10", got)
	}

	// monthly=600, no-upfront → all three qualify → max 0.20.
	got, _ = savingsContractDiscountRate(false, schedule, decimal.RequireFromString("600"))
	if !got.Equal(decimal.RequireFromString("0.20")) {
		t.Errorf("no-upfront 600 discount = %s, want 0.20", got)
	}

	// monthly=150, upfront → tiers 0 and 100 qualify → max 0.25.
	got, _ = savingsContractDiscountRate(true, schedule, decimal.RequireFromString("150"))
	if !got.Equal(decimal.RequireFromString("0.25")) {
		t.Errorf("upfront 150 discount = %s, want 0.25", got)
	}

	// boundary: startAmount == monthly qualifies (<=).
	got, _ = savingsContractDiscountRate(false, schedule, decimal.RequireFromString("100"))
	if !got.Equal(decimal.RequireFromString("0.10")) {
		t.Errorf("no-upfront 100 (boundary) discount = %s, want 0.10", got)
	}
}

func TestSavingsContractDiscountRateNoTierMatches(t *testing.T) {
	schedule := &billing.SavingsPlanSchedule{
		NoUpfrontTiers: []billing.SavingsPlanTier{
			{StartAmount: dec("100"), Discount: dec("0.10")},
		},
	}
	// monthly=50 < the only tier startAmount 100 → no tier qualifies → 400.
	_, err := savingsContractDiscountRate(false, schedule, decimal.RequireFromString("50"))
	if err == nil {
		t.Fatal("expected error when no tier matches")
	}
	if err.Status != 400 {
		t.Errorf("status = %d, want 400", err.Status)
	}
	if err.Msg != "No savings plan found for the given monthly commited amount" {
		t.Errorf("msg = %q", err.Msg)
	}
}

func TestSavingsContractDiscountRateEmptyTiers(t *testing.T) {
	schedule := &billing.SavingsPlanSchedule{} // no tiers at all
	_, err := savingsContractDiscountRate(true, schedule, decimal.RequireFromString("100"))
	if err == nil || err.Status != 400 {
		t.Fatalf("empty tiers should 400, got %v", err)
	}
}

func TestSavingsContractBodySetMapOmitsBlank(t *testing.T) {
	// An empty body → all optional fields omitted (null/omitted → dropped).
	d := savingsContractBody{}.setMap()
	if len(d) != 0 {
		t.Errorf("empty body should produce empty setMap, got %#v", d)
	}
}

func TestSavingsContractBodySetMapPopulated(t *testing.T) {
	var b savingsContractBody
	body := `{"billingProfileId":"bp1","savingsPlanId":"sp1","savingsPlanName":"Plan","discountRate":0.15,"monthlyCommittedAmount":200,"paidUpfront":true,"orderId":"ord1","status":"CANCELLED","targets":[{"resourceType":"instance","filters":[]}]}`
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatal(err)
	}
	d := b.setMap()
	if d["billingProfileId"] != "bp1" || d["savingsPlanId"] != "sp1" || d["savingsPlanName"] != "Plan" {
		t.Errorf("string fields mismatch: %#v", d)
	}
	if d["orderId"] != "ord1" || d["status"] != "CANCELLED" {
		t.Errorf("orderId/status mismatch: %#v", d)
	}
	dr, ok := d["discountRate"].(decimal.Decimal)
	if !ok || !dr.Equal(decimal.RequireFromString("0.15")) {
		t.Errorf("discountRate mismatch: %#v", d["discountRate"])
	}
	if _, ok := d["targets"]; !ok {
		t.Errorf("targets should be present, got %#v", d)
	}
	// paidUpfront is intentionally NOT part of setMap (handled... actually update overwrites it via
	// the existing doc's value — update DOES set paidUpfront; verify it's covered).
}

func TestSavingsContractBodySetMapNullTargetsOmitted(t *testing.T) {
	var b savingsContractBody
	// targets omitted entirely → should not appear in the set map (so the existing value, dropped
	// in the handler, becomes null — matching setTargets(null)).
	if err := json.Unmarshal([]byte(`{"billingProfileId":"bp1"}`), &b); err != nil {
		t.Fatal(err)
	}
	d := b.setMap()
	if _, ok := d["targets"]; ok {
		t.Errorf("omitted targets must not be in setMap, got %#v", d["targets"])
	}
}

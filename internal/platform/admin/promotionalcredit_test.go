package admin

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestCreatePromotionalCreditReqDecode(t *testing.T) {
	var req createPromotionalCreditReq
	if err := json.Unmarshal([]byte(`{"amount":12.34,"daysValidity":30,"billingProfileId":"bp1"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.DaysValidity != 30 || req.BillingProfileID != "bp1" {
		t.Errorf("decoded req mismatch: %+v", req)
	}
	amt, ok, err := req.amountDecimal()
	if err != nil || !ok {
		t.Fatalf("amount must parse: ok=%v err=%v", ok, err)
	}
	if !amt.Equal(decimal.RequireFromString("12.34")) {
		t.Errorf("amount=%s want 12.34", amt)
	}
}

func TestAmountDecimalAbsent(t *testing.T) {
	// No amount field → ok=false (null), distinct from a zero amount.
	var req createPromotionalCreditReq
	if err := json.Unmarshal([]byte(`{"daysValidity":30,"billingProfileId":"bp1"}`), &req); err != nil {
		t.Fatal(err)
	}
	_, ok, err := req.amountDecimal()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok {
		t.Error("absent amount must be ok=false")
	}
}

func TestAmountDecimalZeroAndNegative(t *testing.T) {
	// Zero/negative parse fine (ok=true) — the handler rejects them with the >0 guard, not the parser.
	for _, s := range []string{"0", "-5"} {
		req := createPromotionalCreditReq{Amount: json.Number(s)}
		amt, ok, err := req.amountDecimal()
		if err != nil || !ok {
			t.Fatalf("%q: ok=%v err=%v", s, ok, err)
		}
		if amt.Cmp(decimal.Zero) > 0 {
			t.Errorf("%q must be <= 0", s)
		}
	}
}

func TestAmountDecimalInvalid(t *testing.T) {
	req := createPromotionalCreditReq{Amount: json.Number("not-a-number")}
	if _, _, err := req.amountDecimal(); err == nil {
		t.Error("invalid amount must error")
	}
}

func TestAmountDecimalPositive(t *testing.T) {
	req := createPromotionalCreditReq{Amount: json.Number("100.50")}
	amt, ok, err := req.amountDecimal()
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if amt.Cmp(decimal.Zero) <= 0 {
		t.Error("100.50 must be > 0")
	}
}

func TestAddDays(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	got := addDays(base, 365)
	want := time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("addDays(+365)=%v want %v", got, want)
	}
	if !addDays(base, 0).Equal(base) {
		t.Errorf("addDays(+0) must be the base time")
	}
}

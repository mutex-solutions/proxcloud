package admin

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
)

func mustDec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("mustDec(%q): %v", s, err)
	}
	return d
}

func TestPromotionCodeReqDecode(t *testing.T) {
	var req promotionCodeReq
	body := `{"code":"SUMMER","description":"d","amount":12.50,"status":"DISABLED","targetOrganizationIds":["o1","o2"]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.Code != "SUMMER" || req.Description != "d" {
		t.Errorf("decoded code/desc mismatch: %+v", req)
	}
	if req.Amount == nil || req.Amount.String() != "12.50" {
		t.Errorf("amount should decode as json.Number 12.50, got %v", req.Amount)
	}
	if req.Status == nil || *req.Status != "DISABLED" {
		t.Errorf("status should be DISABLED, got %v", req.Status)
	}
	if req.TargetOrganizationIDs == nil || len(*req.TargetOrganizationIDs) != 2 {
		t.Errorf("targetOrganizationIds should decode to 2 ids, got %v", req.TargetOrganizationIDs)
	}
	if req.Status == nil {
		t.Error("status pointer should be non-nil when present")
	}
}

func TestPromotionCodeReqDecodeOmitted(t *testing.T) {
	var req promotionCodeReq
	if err := json.Unmarshal([]byte(`{"code":"X"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Amount != nil {
		t.Error("omitted amount must stay nil (→ 'Amount must be greater than 0')")
	}
	if req.Status != nil {
		t.Error("omitted status must stay nil (create → ACTIVE; update → preserve)")
	}
	if req.TargetOrganizationIDs != nil {
		t.Error("omitted targetOrganizationIds must stay nil")
	}
}

func TestDecimalIsPositive(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"12.50", true}, {"0", false}, {"0.00", false}, {"-1", false}, {"-0.01", false},
		{"1", true}, {"0.0000001", true}, {"100", true},
	}
	for _, c := range cases {
		if got := decimalIsPositive(mustDec(t, c.s)); got != c.want {
			t.Errorf("decimalIsPositive(%q)=%v want %v", c.s, got, c.want)
		}
	}
}

func TestValidatePromotionCode(t *testing.T) {
	// blank code → "Code is required"
	if e := validatePromotionCode("  ", true, true); e == nil || e.Msg != "Code is required" {
		t.Errorf("blank code want 'Code is required', got %v", e)
	}
	// missing amount → "Amount must be greater than 0"
	if e := validatePromotionCode("X", false, false); e == nil || e.Msg != "Amount must be greater than 0" {
		t.Errorf("missing amount want 'Amount must be greater than 0', got %v", e)
	}
	// non-positive amount → "Amount must be greater than 0"
	if e := validatePromotionCode("X", true, false); e == nil || e.Msg != "Amount must be greater than 0" {
		t.Errorf("non-positive amount want 'Amount must be greater than 0', got %v", e)
	}
	// valid → nil
	if e := validatePromotionCode("X", true, true); e != nil {
		t.Errorf("valid input want nil, got %v", e)
	}
}

func TestPromotionCodeReqDoc(t *testing.T) {
	// minimal: only code + amount; optional blanks omitted; status NOT set by doc().
	req := promotionCodeReq{Code: "SAVE"}
	d := req.doc("SAVE", mustDec(t, "5"), true)
	if d["code"] != "SAVE" {
		t.Errorf("code must be set, got %#v", d["code"])
	}
	if _, ok := d["amount"]; !ok {
		t.Error("amount must be set when amountSet")
	}
	for _, k := range []string{"description", "creditValidityDuration", "validFrom", "validUntil", "targetOrganizationIds", "status"} {
		if _, ok := d[k]; ok {
			t.Errorf("blank/unset %q must be omitted from doc()", k)
		}
	}

	// amountSet=false → no amount key.
	d2 := promotionCodeReq{Code: "Y"}.doc("Y", decimal.Decimal{}, false)
	if _, ok := d2["amount"]; ok {
		t.Error("amount must be omitted when amountSet is false")
	}

	// targetOrganizationIds present (even empty) → emitted (non-null empty list preserved).
	empty := []string{}
	d3 := promotionCodeReq{Code: "Z", TargetOrganizationIDs: &empty}.doc("Z", mustDec(t, "1"), true)
	if v, ok := d3["targetOrganizationIds"]; !ok {
		t.Error("present targetOrganizationIds must be emitted")
	} else if got, _ := v.([]string); got == nil {
		t.Errorf("targetOrganizationIds should be a non-nil slice, got %#v", v)
	}
}

func TestPushReqDecode(t *testing.T) {
	var req pushReq
	if err := json.Unmarshal([]byte(`{"organizationIds":["a","b"]}`), &req); err != nil {
		t.Fatal(err)
	}
	if len(req.OrganizationIDs) != 2 || req.OrganizationIDs[0] != "a" {
		t.Errorf("organizationIds decode mismatch: %+v", req)
	}
	var empty pushReq
	if err := json.Unmarshal([]byte(`{}`), &empty); err != nil {
		t.Fatal(err)
	}
	if len(empty.OrganizationIDs) != 0 {
		t.Error("missing organizationIds → empty (→ 'At least one organization is required')")
	}
}

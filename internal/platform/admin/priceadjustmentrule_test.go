package admin

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
)

// TestPriceAdjustmentRuleReqDecode verifies the request body decodes faithfully, including nested
// tier startAmount + modifier value decoded as decimal (not float) from JSON numbers, and the target
// filters.
func TestPriceAdjustmentRuleReqDecode(t *testing.T) {
	body := `{
		"name":"Volume Discount",
		"enabled":true,
		"description":"desc",
		"pricePlanId":"pp-1",
		"targets":[{"resourceType":"server","filters":[{"attributeName":"region","operator":"eq","value":"eu"}]}],
		"tiers":[
			{"startAmount":100,"modifier":{"operator":"subtract","asPercentage":true,"value":10}},
			{"startAmount":500.50,"modifier":{"operator":"add","asPercentage":false,"value":25}}
		]
	}`
	var req priceAdjustmentRuleReq
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != "Volume Discount" || !req.Enabled || req.Description != "desc" || req.PricePlanID != "pp-1" {
		t.Errorf("scalar mismatch: %+v", req)
	}
	if len(req.Targets) != 1 || req.Targets[0].ResourceType != "server" || len(req.Targets[0].Filters) != 1 {
		t.Fatalf("targets mismatch: %+v", req.Targets)
	}
	if req.Targets[0].Filters[0].AttributeName != "region" || req.Targets[0].Filters[0].Operator != "eq" {
		t.Errorf("filter mismatch: %+v", req.Targets[0].Filters[0])
	}
	if len(req.Tiers) != 2 {
		t.Fatalf("want 2 tiers, got %d", len(req.Tiers))
	}
	t0 := req.Tiers[0]
	if t0.StartAmount == nil || !t0.StartAmount.Equal(decimal.RequireFromString("100")) {
		t.Errorf("tier0 startAmount=%v want 100", t0.StartAmount)
	}
	if t0.Modifier == nil || t0.Modifier.Operator != "subtract" || !t0.Modifier.AsPercentage ||
		t0.Modifier.Value == nil || !t0.Modifier.Value.Equal(decimal.RequireFromString("10")) {
		t.Errorf("tier0 modifier mismatch: %+v", t0.Modifier)
	}
	t1 := req.Tiers[1]
	if t1.StartAmount == nil || !t1.StartAmount.Equal(decimal.RequireFromString("500.50")) {
		t.Errorf("tier1 startAmount=%v want 500.50", t1.StartAmount)
	}
	if t1.Modifier == nil || t1.Modifier.AsPercentage || t1.Modifier.Operator != "add" {
		t.Errorf("tier1 modifier mismatch: %+v", t1.Modifier)
	}
}

// TestPriceAdjustmentRuleValidate exercises validate: name then pricePlanId, both
// required → 400 with the exact messages (name checked first).
func TestPriceAdjustmentRuleValidate(t *testing.T) {
	cases := []struct {
		name    string
		req     priceAdjustmentRuleReq
		wantMsg string // "" = nil (valid)
	}{
		{"missing name", priceAdjustmentRuleReq{PricePlanID: "pp-1"}, parNameRequired},
		{"missing pricePlanId", priceAdjustmentRuleReq{Name: "X"}, parPlanIDRequired},
		{"name precedes plan", priceAdjustmentRuleReq{}, parNameRequired},
		{"valid", priceAdjustmentRuleReq{Name: "X", PricePlanID: "pp-1"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.req.validate()
			if c.wantMsg == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error %q, got nil", c.wantMsg)
			}
			if err.Msg != c.wantMsg {
				t.Errorf("msg=%q want %q", err.Msg, c.wantMsg)
			}
			if err.Status != 400 || err.Code != 400 {
				t.Errorf("status/code = %d/%d, want 400/400", err.Status, err.Code)
			}
		})
	}
}

// TestPriceAdjustmentRuleReqToDomain verifies toDomain maps every create-body field and preserves
// money, leaving id/timestamps unset (assigned by the repo / preserved on update).
func TestPriceAdjustmentRuleReqToDomain(t *testing.T) {
	v := decimal.RequireFromString("10")
	sa := decimal.RequireFromString("100")
	req := priceAdjustmentRuleReq{
		Name: "X", Enabled: true, Description: "d", PricePlanID: "pp-1",
		Tiers: []priceAdjustmentRuleTier{{
			StartAmount: &sa,
			Modifier:    &priceAdjustmentRuleModifier{Operator: "add", AsPercentage: true, Value: &v},
		}},
	}
	d := req.toDomain()
	if d.Name != "X" || !d.Enabled || d.Description != "d" || d.PricePlanID != "pp-1" {
		t.Errorf("scalar mismatch: %+v", d)
	}
	if d.ID != "" || d.CreatedAt != nil || d.UpdatedAt != nil {
		t.Errorf("toDomain must not set id/timestamps: %+v", d)
	}
	if len(d.Tiers) != 1 || d.Tiers[0].StartAmount == nil || !d.Tiers[0].StartAmount.Equal(sa) {
		t.Fatalf("tier startAmount lost: %+v", d.Tiers)
	}
	if d.Tiers[0].Modifier == nil || d.Tiers[0].Modifier.Value == nil || !d.Tiers[0].Modifier.Value.Equal(v) {
		t.Errorf("modifier value lost: %+v", d.Tiers[0].Modifier)
	}
}

// TestPriceAdjustmentRuleToDto verifies the response mapper emits money as a JSON number (not a quoted
// string), `id` (not `_id`), and the primitive `enabled`.
func TestPriceAdjustmentRuleToDto(t *testing.T) {
	sa := decimal.RequireFromString("100")
	val := decimal.RequireFromString("0.05")
	rule := &priceAdjustmentRule{
		ID: "rule-1", Name: "X", Enabled: false, PricePlanID: "pp-1",
		Tiers: []priceAdjustmentRuleTier{{
			StartAmount: &sa,
			Modifier:    &priceAdjustmentRuleModifier{Operator: "subtract", AsPercentage: true, Value: &val},
		}},
	}
	b, err := json.Marshal(priceAdjustmentRuleToDto(rule))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	// money must be a bare JSON number, never a quoted string.
	for _, want := range []string{`"id":"rule-1"`, `"enabled":false`, `"startAmount":100`, `"value":0.05`, `"asPercentage":true`} {
		if !contains(s, want) {
			t.Errorf("DTO json missing %q: %s", want, s)
		}
	}
	for _, bad := range []string{`"startAmount":"100"`, `"value":"0.05"`, `"_id"`, `"_class"`} {
		if contains(s, bad) {
			t.Errorf("DTO json must NOT contain %q: %s", bad, s)
		}
	}
}

// TestPriceAdjustmentRuleTiersToDtosNil verifies nil tiers map to nil (omitted, not []).
func TestPriceAdjustmentRuleTiersToDtosNil(t *testing.T) {
	if got := tiersToDtos(nil); got != nil {
		t.Errorf("nil tiers must map to nil, got %#v", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

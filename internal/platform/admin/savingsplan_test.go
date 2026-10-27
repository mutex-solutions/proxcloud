package admin

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
)

// TestSavingsPlanReqDecode verifies the request body decodes faithfully, including nested
// schedule/tier money decoded as decimal (not float) from a JSON number.
func TestSavingsPlanReqDecode(t *testing.T) {
	body := `{
		"name":"Compute Saver",
		"available":true,
		"description":"desc",
		"accessMode":"SCOPED",
		"targets":[{"resourceType":"server","filters":[]}],
		"billingProfiles":[{"billingProfileId":"bp-1"}],
		"savingSchedule":[{"durationMonths":12,"maxAmount":1000,
			"noUpfrontTiers":[{"startAmount":0,"discount":0.05}],
			"upfrontTiers":[{"startAmount":100,"discount":0.10}]}]
	}`
	var req savingsPlanReq
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != "Compute Saver" || !req.Available || req.Description != "desc" || req.AccessMode != "SCOPED" {
		t.Errorf("scalar mismatch: %+v", req)
	}
	if len(req.Targets) != 1 || req.Targets[0].ResourceType != "server" {
		t.Errorf("targets mismatch: %+v", req.Targets)
	}
	if len(req.BillingProfiles) != 1 || req.BillingProfiles[0].BillingProfileID != "bp-1" {
		t.Errorf("billingProfiles mismatch: %+v", req.BillingProfiles)
	}
	if len(req.SavingSchedule) != 1 {
		t.Fatalf("want 1 schedule, got %d", len(req.SavingSchedule))
	}
	s := req.SavingSchedule[0]
	if s.DurationMonths != 12 {
		t.Errorf("durationMonths=%d want 12", s.DurationMonths)
	}
	if s.MaxAmount == nil || !s.MaxAmount.Equal(decimal.RequireFromString("1000")) {
		t.Errorf("maxAmount=%v want 1000", s.MaxAmount)
	}
	if len(s.NoUpfrontTiers) != 1 || s.NoUpfrontTiers[0].Discount == nil ||
		!s.NoUpfrontTiers[0].Discount.Equal(decimal.RequireFromString("0.05")) {
		t.Errorf("noUpfront tier discount mismatch: %+v", s.NoUpfrontTiers)
	}
	if len(s.UpfrontTiers) != 1 || s.UpfrontTiers[0].StartAmount == nil ||
		!s.UpfrontTiers[0].StartAmount.Equal(decimal.RequireFromString("100")) {
		t.Errorf("upfront tier startAmount mismatch: %+v", s.UpfrontTiers)
	}
}

// TestSavingsPlanReqToDomain verifies toDomain maps every mutable field and preserves money.
func TestSavingsPlanReqToDomain(t *testing.T) {
	d := decimal.RequireFromString("0.05")
	req := savingsPlanReq{
		Name:        "X",
		Available:   true,
		Description: "d",
		AccessMode:  "PUBLIC",
		SavingSchedule: []savingsPlanScheduleReq{{
			DurationMonths: 6,
			NoUpfrontTiers: []savingsPlanTierReq{{Discount: &d}},
		}},
	}
	plan := req.toDomain()
	if plan.Name != "X" || !plan.Available || plan.Description != "d" || plan.AccessMode != "PUBLIC" {
		t.Errorf("scalar mismatch: %+v", plan)
	}
	if len(plan.SavingSchedule) != 1 || plan.SavingSchedule[0].DurationMonths != 6 {
		t.Fatalf("schedule mismatch: %+v", plan.SavingSchedule)
	}
	tier := plan.SavingSchedule[0].NoUpfrontTiers
	if len(tier) != 1 || tier[0].Discount == nil || !tier[0].Discount.Equal(d) {
		t.Errorf("tier money lost: %+v", tier)
	}
	// nil schedule → nil (omitted, not []).
	if got := (savingsPlanReq{}).toDomain(); got.SavingSchedule != nil {
		t.Errorf("nil savingSchedule must map to nil, got %#v", got.SavingSchedule)
	}
}

// TestIsEligibleForBillingProfile exercises the eligibility predicate truth table.
func TestIsEligibleForBillingProfile(t *testing.T) {
	cases := []struct {
		name string
		plan billing.SavingsPlan
		bp   string
		want bool
	}{
		{"null accessMode → all", billing.SavingsPlan{AccessMode: ""}, "bp-1", true},
		{"PUBLIC → all", billing.SavingsPlan{AccessMode: "PUBLIC"}, "bp-1", true},
		{"SCOPED nil profiles → all", billing.SavingsPlan{AccessMode: "SCOPED"}, "bp-1", true},
		{"SCOPED match", billing.SavingsPlan{AccessMode: "SCOPED",
			BillingProfiles: []billing.SavingsPlanBillingProfile{{BillingProfileID: "bp-1"}}}, "bp-1", true},
		{"SCOPED no match", billing.SavingsPlan{AccessMode: "SCOPED",
			BillingProfiles: []billing.SavingsPlanBillingProfile{{BillingProfileID: "bp-2"}}}, "bp-1", false},
		{"SCOPED empty profiles", billing.SavingsPlan{AccessMode: "SCOPED",
			BillingProfiles: []billing.SavingsPlanBillingProfile{}}, "bp-1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isEligibleForBillingProfile(&c.plan, c.bp); got != c.want {
				t.Errorf("isEligibleForBillingProfile=%v want %v", got, c.want)
			}
		})
	}
}

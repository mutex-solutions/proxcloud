package billing

import (
	"encoding/json"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// TestSavingsContractToDto: money (discountRate, monthlyCommittedAmount) → unquoted numbers;
// durationMonths/paidUpfront emitted as primitives; targets passthrough.
func TestSavingsContractToDto(t *testing.T) {
	c := &SavingsContract{
		ID: "sc1", BillingProfileID: "bp1", Status: "ACTIVE", SavingsPlanID: "sp1",
		DurationMonths: 12, DiscountRate: dp("0.15"), MonthlyCommittedAmount: dp("100"), PaidUpfront: true,
		Targets: []SavingsPlanTarget{{ResourceType: "instance", Filters: []pricing.PricePlanRuleFilter{{AttributeName: "region", Operator: "eq", Value: "eu"}}}},
	}
	raw, _ := json.Marshal(SavingsContractToDto(c))
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	if string(m["discountRate"]) != "0.15" || string(m["monthlyCommittedAmount"]) != "100" {
		t.Fatalf("money must be unquoted numbers: discountRate=%s committed=%s", m["discountRate"], m["monthlyCommittedAmount"])
	}
	if string(m["durationMonths"]) != "12" || string(m["paidUpfront"]) != "true" {
		t.Fatalf("primitives wrong: duration=%s upfront=%s", m["durationMonths"], m["paidUpfront"])
	}
	if _, ok := m["targets"]; !ok {
		t.Fatal("targets should be present")
	}
}

// TestSavingsPlanToDto: nested schedule/tier money → unquoted numbers all the way down.
func TestSavingsPlanToDto(t *testing.T) {
	p := &SavingsPlan{
		ID: "sp1", Name: "plan", Available: true, AccessMode: "PUBLIC",
		SavingSchedule: []SavingsPlanSchedule{{
			DurationMonths: 12, MaxAmount: dp("1000"),
			UpfrontTiers: []SavingsPlanTier{{StartAmount: dp("0"), Discount: dp("0.1")}, {StartAmount: dp("500"), Discount: dp("0.2")}},
		}},
		BillingProfiles: []SavingsPlanBillingProfile{{BillingProfileID: "bp1"}},
	}
	dto := SavingsPlanToDto(p)
	raw, _ := json.Marshal(dto)
	var top map[string]json.RawMessage
	_ = json.Unmarshal(raw, &top)
	if string(top["available"]) != "true" {
		t.Fatalf("available = %s", top["available"])
	}
	// drill into savingSchedule[0].upfrontTiers[0].discount — must be an unquoted number
	var sched []map[string]json.RawMessage
	_ = json.Unmarshal(top["savingSchedule"], &sched)
	if string(sched[0]["maxAmount"]) != "1000" {
		t.Fatalf("maxAmount = %s, want 1000 (unquoted)", sched[0]["maxAmount"])
	}
	var tiers []map[string]json.RawMessage
	_ = json.Unmarshal(sched[0]["upfrontTiers"], &tiers)
	if string(tiers[0]["discount"]) != "0.1" || string(tiers[1]["startAmount"]) != "500" {
		t.Fatalf("nested tier money must be unquoted: %v", tiers)
	}
}

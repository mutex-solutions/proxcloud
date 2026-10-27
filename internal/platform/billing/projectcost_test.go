package billing

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

func mustDec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func numEq(t *testing.T, label string, got any, want string) {
	t.Helper()
	jn, ok := got.(json.Number)
	if !ok {
		t.Errorf("%s: not a json.Number: %#v", label, got)
		return
	}
	g, err := decimal.NewFromString(string(jn))
	if err != nil || !g.Equal(mustDec(want)) {
		t.Errorf("%s = %s, want %s", label, jn, want)
	}
}

func TestProjectCostInfoMap(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	bill := pricing.Bill{
		BillingCycle: &pricing.BillBillingCycle{StartDate: &start},
		Items: []pricing.BillItem{
			{ProjectID: "projA", Name: "vmA", ResourceID: "r1", ResourceType: "instance", NetAmount: mustDec("0.40")},
			{ProjectID: "projA", Name: "volA", ResourceID: "r2", ResourceType: "volume", NetAmount: mustDec("0.10")},
			{ProjectID: "projB", Name: "vmB", ResourceID: "r3", ResourceType: "instance", NetAmount: mustDec("1.00")},
			{ProjectID: "", Name: "orphan", ResourceID: "r4", ResourceType: "instance", NetAmount: mustDec("9.99")}, // no projectId → skipped
		},
	}
	m := ProjectCostInfoMap([]pricing.Bill{bill}, now, nil)

	if len(m) != 2 {
		t.Fatalf("expected 2 projects, got %d: %v", len(m), m)
	}
	a, _ := m["projA"].(map[string]any)
	if a == nil {
		t.Fatal("projA missing")
	}
	numEq(t, "projA.currentMonthCosts", a["currentMonthCosts"], "0.50")
	abt, _ := a["currentMonthCostsByType"].(map[string]any)
	numEq(t, "projA.byType.Compute", abt["Compute"], "0.40")
	numEq(t, "projA.byType.Block Storage", abt["Block Storage"], "0.10")

	b, _ := m["projB"].(map[string]any)
	if b == nil {
		t.Fatal("projB missing")
	}
	numEq(t, "projB.currentMonthCosts", b["currentMonthCosts"], "1.00")

	if _, ok := m[""]; ok {
		t.Errorf("empty projectId should be skipped")
	}
}

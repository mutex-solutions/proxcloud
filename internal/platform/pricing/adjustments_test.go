package pricing

import (
	"testing"

	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func billWith(items ...BillItem) *Bill { return &Bill{Items: items} }

func item(resourceType, net string) BillItem {
	return BillItem{ResourceType: resourceType, NetAmount: d(net)}
}

// adjAmount returns the single adjustment's amount (fails if not exactly one).
func adjAmount(t *testing.T, bill *Bill) decimal.Decimal {
	t.Helper()
	if len(bill.Adjustments) != 1 || bill.Adjustments[0].Amount == nil {
		t.Fatalf("want 1 adjustment with amount, got %d: %+v", len(bill.Adjustments), bill.Adjustments)
	}
	return *bill.Adjustments[0].Amount
}

func TestSavingsContractDiscount(t *testing.T) {
	e := NewEngine(nil)
	// committed 100, rate 10%, usage 100 (filterless target → all "instance" items).
	// rateFrac=0.1, discount=min(100,100)*0.1=10, committedDiscount=(100*10)/100=10,
	// diff=100-(100-10)=10 ≥0 → -discount = -10.
	c := SavingsContractAdj{ID: "c1", SavingsPlanName: "SP", MonthlyCommittedAmount: d("100"), DiscountRate: d("10"),
		Targets: []AdjustmentTarget{{ResourceType: "instance"}}}
	bill := billWith(item("instance", "100"))
	e.ApplySavingsContractDiscounts(bill, []SavingsContractAdj{c}, nil)
	if got := adjAmount(t, bill); !got.Equal(d("-10")) {
		t.Fatalf("savings discount = %s, want -10", got)
	}
	if bill.Adjustments[0].Type != AdjustmentTypeSavingsContract || bill.Adjustments[0].ContractID != "c1" {
		t.Fatalf("bad adjustment shape: %+v", bill.Adjustments[0])
	}
}

func TestSavingsContractDiscountBelowCommit(t *testing.T) {
	e := NewEngine(nil)
	// usage 50 < committed 100 → no adjustment.
	c := SavingsContractAdj{ID: "c1", MonthlyCommittedAmount: d("100"), DiscountRate: d("10"),
		Targets: []AdjustmentTarget{{ResourceType: "instance"}}}
	bill := billWith(item("instance", "50"))
	e.ApplySavingsContractDiscounts(bill, []SavingsContractAdj{c}, nil)
	if len(bill.Adjustments) != 0 {
		t.Fatalf("want no adjustment below commit, got %+v", bill.Adjustments)
	}
}

func TestSavingsContractDiscountPaidUpfront(t *testing.T) {
	e := NewEngine(nil)
	// paidUpfront: -min(committed, usage) = -min(100,120) = -100.
	c := SavingsContractAdj{ID: "c1", MonthlyCommittedAmount: d("100"), DiscountRate: d("10"), PaidUpfront: true,
		Targets: []AdjustmentTarget{{ResourceType: "instance"}}}
	bill := billWith(item("instance", "120"))
	e.ApplySavingsContractDiscounts(bill, []SavingsContractAdj{c}, nil)
	if got := adjAmount(t, bill); !got.Equal(d("-100")) {
		t.Fatalf("upfront discount = %s, want -100", got)
	}
}

func TestSavingsContractOnlyTargetResourceTypeCounts(t *testing.T) {
	e := NewEngine(nil)
	// target instance only; a volume item must NOT count toward usage.
	c := SavingsContractAdj{ID: "c1", MonthlyCommittedAmount: d("100"), DiscountRate: d("10"),
		Targets: []AdjustmentTarget{{ResourceType: "instance"}}}
	bill := billWith(item("instance", "60"), item("volume", "80"))
	e.ApplySavingsContractDiscounts(bill, []SavingsContractAdj{c}, nil)
	// usage = 60 < 100 → no adjustment (proves volume's 80 didn't count).
	if len(bill.Adjustments) != 0 {
		t.Fatalf("volume must not count; got %+v", bill.Adjustments)
	}
}

func TestPriceAdjustmentPercentageSubtract(t *testing.T) {
	e := NewEngine(nil)
	// no targets → all items (200); tier start 0, -10% → 200*10/100=20, subtract → -20.
	r := PriceAdjustmentRule{ID: "r1", Name: "R", Enabled: true, Tiers: []PriceAdjustmentTier{
		{StartAmount: d("0"), Modifier: PriceAdjustmentModifier{AsPercentage: true, Value: d("10"), Operator: "subtract"}},
	}}
	bill := billWith(item("instance", "150"), item("volume", "50"))
	e.ApplyPriceAdjustmentRules(bill, []PriceAdjustmentRule{r}, nil)
	if got := adjAmount(t, bill); !got.Equal(d("-20")) {
		t.Fatalf("price-adjustment = %s, want -20", got)
	}
	if bill.Adjustments[0].Type != AdjustmentTypePriceAdjustmentRule || bill.Adjustments[0].PriceAdjustmentRuleID != "r1" {
		t.Fatalf("bad adjustment shape: %+v", bill.Adjustments[0])
	}
}

func TestPriceAdjustmentTierSelectionDesc(t *testing.T) {
	e := NewEngine(nil)
	// two tiers: start 0 → +5 (flat), start 100 → +10 (flat). usage 150 → highest crossed tier (100) → +10.
	r := PriceAdjustmentRule{ID: "r1", Name: "R", Enabled: true, Tiers: []PriceAdjustmentTier{
		{StartAmount: d("0"), Modifier: PriceAdjustmentModifier{Value: d("5"), Operator: "add"}},
		{StartAmount: d("100"), Modifier: PriceAdjustmentModifier{Value: d("10"), Operator: "add"}},
	}}
	bill := billWith(item("instance", "150"))
	e.ApplyPriceAdjustmentRules(bill, []PriceAdjustmentRule{r}, nil)
	if got := adjAmount(t, bill); !got.Equal(d("10")) {
		t.Fatalf("tier select = %s, want 10", got)
	}
	// usage 50 → only the start-0 tier crossed → +5.
	bill2 := billWith(item("instance", "50"))
	e.ApplyPriceAdjustmentRules(bill2, []PriceAdjustmentRule{r}, nil)
	if got := adjAmount(t, bill2); !got.Equal(d("5")) {
		t.Fatalf("tier select low = %s, want 5", got)
	}
}

func TestPriceAdjustmentDisabledOrZeroSkipped(t *testing.T) {
	e := NewEngine(nil)
	disabled := PriceAdjustmentRule{ID: "r1", Enabled: false, Tiers: []PriceAdjustmentTier{
		{StartAmount: d("0"), Modifier: PriceAdjustmentModifier{Value: d("5"), Operator: "add"}}}}
	bill := billWith(item("instance", "100"))
	e.ApplyPriceAdjustmentRules(bill, []PriceAdjustmentRule{disabled}, nil)
	if len(bill.Adjustments) != 0 {
		t.Fatalf("disabled rule must not apply; got %+v", bill.Adjustments)
	}
	// zero usage → skip.
	enabled := PriceAdjustmentRule{ID: "r2", Enabled: true, Tiers: []PriceAdjustmentTier{
		{StartAmount: d("0"), Modifier: PriceAdjustmentModifier{Value: d("5"), Operator: "add"}}}}
	zero := billWith(item("instance", "0"))
	e.ApplyPriceAdjustmentRules(zero, []PriceAdjustmentRule{enabled}, nil)
	if len(zero.Adjustments) != 0 {
		t.Fatalf("zero usage must not apply; got %+v", zero.Adjustments)
	}
}

func TestUpsertAdjustmentReplacesByKey(t *testing.T) {
	e := NewEngine(nil)
	c := SavingsContractAdj{ID: "c1", MonthlyCommittedAmount: d("100"), DiscountRate: d("10"),
		Targets: []AdjustmentTarget{{ResourceType: "instance"}}}
	bill := billWith(item("instance", "100"))
	e.ApplySavingsContractDiscounts(bill, []SavingsContractAdj{c}, nil)
	e.ApplySavingsContractDiscounts(bill, []SavingsContractAdj{c}, nil) // re-apply
	if len(bill.Adjustments) != 1 {
		t.Fatalf("re-apply must upsert (1 adjustment), got %d", len(bill.Adjustments))
	}
}

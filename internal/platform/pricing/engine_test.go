package pricing

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func mustDec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

func dp(s string) *decimal.Decimal { d := mustDec(s); return &d }

func computeType() *BillingResourceType {
	yes := true
	return &BillingResourceType{ResourceType: "compute", Attributes: []ResourceAttribute{
		{Name: "vcpu", Type: "number"},
		{Name: "region", Type: "string"},
		{Name: "tier", Type: "string"},
		{Name: "backups", Type: "boolean"},
		{Name: "usageBytes", Type: "number", IsUsage: &yes},
	}}
}

func computeRes(values map[string]any) *BillingResource {
	return &BillingResource{ResourceType: "compute", Values: values, BillingResourceType: computeType()}
}

// rule with a single priced attribute over the given tiers (hourly, no proration).
func priceRule(attr string, tiers []PriceTier) PricePlanRule {
	return PricePlanRule{TimeUnit: "hour", ResourceType: "compute",
		Prices: []PricePlanRulePrice{{AttributeName: attr, Tiers: tiers}}}
}

func rateSingle(t *testing.T, e *Engine, rule PricePlanRule, res *BillingResource) (PricePlanRuleResult, bool) {
	t.Helper()
	out, err := e.ApplyPricePlanRules([]PricePlanRule{rule}, res, rule.TimeUnit)
	if err != nil {
		t.Fatalf("rating error: %v", err)
	}
	if len(out) == 0 {
		return PricePlanRuleResult{}, false
	}
	return out[0], true
}

func wantNet(t *testing.T, e *Engine, rule PricePlanRule, res *BillingResource, want string) {
	t.Helper()
	r, ok := rateSingle(t, e, rule, res)
	if !ok {
		t.Fatalf("expected a result, got none")
	}
	if len(r.Amounts) != 1 {
		t.Fatalf("expected 1 amount, got %d", len(r.Amounts))
	}
	if !r.Amounts[0].NetAmount.Equal(mustDec(want)) {
		t.Errorf("net = %s, want %s", r.Amounts[0].NetAmount, want)
	}
}

// TestMalformedAttributeAborts: a present-but-wrong-typed attribute makes rating return an error
// (aborting the whole profile charge) rather than
// silently rating it ZERO.
func TestMalformedAttributeAborts(t *testing.T) {
	e := NewEngine(SystemClock())
	flatTier := []PriceTier{{Value: dp("1")}} // from==nil → rates the whole value

	t.Run("non-numeric value on a number attribute → error", func(t *testing.T) {
		res := computeRes(map[string]any{"vcpu": "not-a-number"})
		if _, err := e.ApplyPricePlanRules([]PricePlanRule{priceRule("vcpu", flatTier)}, res, "hour"); err == nil {
			t.Fatalf("expected a rating error for a non-numeric numeric attribute")
		}
	})
	t.Run("non-boolean value on a boolean attribute → error", func(t *testing.T) {
		res := computeRes(map[string]any{"backups": 42}) // int, not a bool/"true"/"false"
		if _, err := e.ApplyPricePlanRules([]PricePlanRule{priceRule("backups", flatTier)}, res, "hour"); err == nil {
			t.Fatalf("expected a rating error for a non-boolean boolean attribute")
		}
	})
	t.Run("well-formed numeric value → no error", func(t *testing.T) {
		res := computeRes(map[string]any{"vcpu": 2})
		if _, err := e.ApplyPricePlanRules([]PricePlanRule{priceRule("vcpu", flatTier)}, res, "hour"); err != nil {
			t.Fatalf("well-formed value should not error: %v", err)
		}
	})
	t.Run("numeric STRING still accepted (numeric strings are parsed)", func(t *testing.T) {
		res := computeRes(map[string]any{"vcpu": "3"})
		if _, err := e.ApplyPricePlanRules([]PricePlanRule{priceRule("vcpu", flatTier)}, res, "hour"); err != nil {
			t.Fatalf("numeric string should rate, not error: %v", err)
		}
	})
	t.Run("missing attribute → ZERO, no error (missing key)", func(t *testing.T) {
		res := computeRes(map[string]any{})
		if _, err := e.ApplyPricePlanRules([]PricePlanRule{priceRule("vcpu", flatTier)}, res, "hour"); err != nil {
			t.Fatalf("missing attribute should be ZERO not an error: %v", err)
		}
	})
}

func TestApplyTier(t *testing.T) {
	e := NewEngine(SystemClock())

	t.Run("graduated_from1_adjustment", func(t *testing.T) {
		rule := priceRule("vcpu", []PriceTier{{From: dp("1"), To: dp("10"), Value: dp("5")}, {From: dp("11"), To: dp("20"), Value: dp("4")}})
		wantNet(t, e, rule, computeRes(map[string]any{"vcpu": 15}), "70") // 10*5 + 5*4
	})
	t.Run("from0_no_adjustment", func(t *testing.T) {
		rule := priceRule("vcpu", []PriceTier{{From: dp("0"), To: dp("10"), Value: dp("5")}, {From: dp("10"), To: nil, Value: dp("4")}})
		wantNet(t, e, rule, computeRes(map[string]any{"vcpu": 15}), "70") // 10*5 + 5*4
	})
	t.Run("open_top_tier_not_reached", func(t *testing.T) {
		rule := priceRule("vcpu", []PriceTier{{From: dp("0"), To: dp("10"), Value: dp("5")}, {From: dp("10"), To: nil, Value: dp("4")}})
		wantNet(t, e, rule, computeRes(map[string]any{"vcpu": 8}), "40") // 8*5; tier2 qty<from contributes 0
	})
	t.Run("flat_from_null_existence", func(t *testing.T) {
		rule := priceRule("existence", []PriceTier{{From: nil, To: nil, Value: dp("2.5")}})
		wantNet(t, e, rule, computeRes(map[string]any{"vcpu": 15}), "2.5") // existence qty 1 * 2.5
	})
	t.Run("flat_from_null_ignores_to", func(t *testing.T) {
		rule := priceRule("vcpu", []PriceTier{{From: nil, To: dp("5"), Value: dp("3")}})
		wantNet(t, e, rule, computeRes(map[string]any{"vcpu": 8}), "24") // whole 8*3, To ignored
	})
}

func TestZeroAndMissingValue(t *testing.T) {
	e := NewEngine(SystemClock())
	rule := priceRule("vcpu", []PriceTier{{From: dp("0"), To: nil, Value: dp("5")}})
	for _, c := range []struct {
		name string
		vals map[string]any
	}{
		{"value_zero", map[string]any{"vcpu": 0}},
		{"value_missing", map[string]any{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, ok := rateSingle(t, e, rule, computeRes(c.vals))
			if !ok || len(r.Amounts) != 1 {
				t.Fatalf("amount should still be emitted, got ok=%v amounts=%d", ok, len(r.Amounts))
			}
			if !r.Amounts[0].NetAmount.IsZero() {
				t.Errorf("net = %s, want 0", r.Amounts[0].NetAmount)
			}
		})
	}
}

// A tier with a nil Value fails the multiply/divide and aborts the
// charge (same posture as TestMalformedAttributeAborts). Rating must ERROR, not silently rate 0.
func TestNilTierValueAborts(t *testing.T) {
	e := NewEngine(SystemClock())
	t.Run("flat_nil_value", func(t *testing.T) {
		rule := priceRule("vcpu", []PriceTier{{From: nil, To: nil, Value: nil}})
		if _, err := e.ApplyPricePlanRules([]PricePlanRule{rule}, computeRes(map[string]any{"vcpu": 2}), "hour"); err == nil {
			t.Error("nil tier value must abort (error), not rate 0")
		}
	})
	t.Run("nil_value_even_when_qty_below_from", func(t *testing.T) {
		// from set + resourceValue < from would yield result 0, but the nil value still fails result.multiply(null).
		rule := priceRule("vcpu", []PriceTier{{From: dp("100"), To: dp("200"), Value: nil}})
		if _, err := e.ApplyPricePlanRules([]PricePlanRule{rule}, computeRes(map[string]any{"vcpu": 2}), "hour"); err == nil {
			t.Error("nil tier value must abort even when the tier wouldn't contribute")
		}
	})
	t.Run("present_value_still_works", func(t *testing.T) {
		rule := priceRule("vcpu", []PriceTier{{From: nil, To: nil, Value: dp("5")}})
		if _, err := e.ApplyPricePlanRules([]PricePlanRule{rule}, computeRes(map[string]any{"vcpu": 2}), "hour"); err != nil {
			t.Errorf("present value must not error: %v", err)
		}
	})
}

func TestExistenceAndBoolean(t *testing.T) {
	e := NewEngine(SystemClock())
	t.Run("existence_qty_one_no_attr_declared", func(t *testing.T) {
		rule := priceRule("existence", []PriceTier{{From: nil, To: nil, Value: dp("2.5")}})
		wantNet(t, e, rule, computeRes(map[string]any{}), "2.5")
	})
	t.Run("boolean_true_qty_one", func(t *testing.T) {
		rule := priceRule("backups", []PriceTier{{From: nil, To: nil, Value: dp("7")}})
		wantNet(t, e, rule, computeRes(map[string]any{"backups": true}), "7")
	})
	t.Run("boolean_false_qty_zero_emitted", func(t *testing.T) {
		rule := priceRule("backups", []PriceTier{{From: nil, To: nil, Value: dp("7")}})
		r, ok := rateSingle(t, e, rule, computeRes(map[string]any{"backups": false}))
		if !ok || len(r.Amounts) != 1 || !r.Amounts[0].NetAmount.IsZero() {
			t.Fatalf("want emitted zero amount, got ok=%v %v", ok, r.Amounts)
		}
	})
}

func TestFilters(t *testing.T) {
	e := NewEngine(SystemClock())
	t.Run("resourcetype_gate", func(t *testing.T) {
		rule := priceRule("vcpu", []PriceTier{{From: nil, To: nil, Value: dp("5")}})
		rule.ResourceType = "storage" // mismatch vs compute resource
		if _, ok := rateSingle(t, e, rule, computeRes(map[string]any{"vcpu": 3})); ok {
			t.Error("resourceType mismatch must yield no result")
		}
	})
	t.Run("region_eq_match_and_miss", func(t *testing.T) {
		rule := priceRule("vcpu", []PriceTier{{From: nil, To: nil, Value: dp("5")}})
		rule.Filters = []PricePlanRuleFilter{{AttributeName: "region", Operator: "eq", Value: "eu-west-1"}}
		if _, ok := rateSingle(t, e, rule, computeRes(map[string]any{"vcpu": 3, "region": "us-east-1"})); ok {
			t.Error("region mismatch must yield no result")
		}
		if _, ok := rateSingle(t, e, rule, computeRes(map[string]any{"vcpu": 3, "region": "eu-west-1"})); !ok {
			t.Error("region match must yield a result")
		}
	})
	t.Run("number_gte_swap", func(t *testing.T) {
		rule := priceRule("vcpu", []PriceTier{{From: nil, To: nil, Value: dp("5")}})
		rule.Filters = []PricePlanRuleFilter{{AttributeName: "vcpu", Operator: "gte", Value: 4}}
		if _, ok := rateSingle(t, e, rule, computeRes(map[string]any{"vcpu": 8})); !ok {
			t.Error("vcpu 8 gte 4 must pass")
		}
		if _, ok := rateSingle(t, e, rule, computeRes(map[string]any{"vcpu": 2})); ok {
			t.Error("vcpu 2 gte 4 must fail")
		}
	})
	t.Run("in_is_case_sensitive", func(t *testing.T) {
		rule := priceRule("vcpu", []PriceTier{{From: nil, To: nil, Value: dp("5")}})
		rule.Filters = []PricePlanRuleFilter{{AttributeName: "region", Operator: "in", Values: []any{"eu-west-1", "us-east-1"}}}
		if _, ok := rateSingle(t, e, rule, computeRes(map[string]any{"vcpu": 3, "region": "eu-west-1"})); !ok {
			t.Error("exact 'in' member must pass")
		}
		if _, ok := rateSingle(t, e, rule, computeRes(map[string]any{"vcpu": 3, "region": "EU-WEST-1"})); ok {
			t.Error("'in' must be case-sensitive (mis-cased fails)")
		}
	})
}

func TestModifiers(t *testing.T) {
	e := NewEngine(SystemClock())
	base70 := func() PricePlanRule {
		return priceRule("vcpu", []PriceTier{{From: dp("1"), To: dp("10"), Value: dp("5")}, {From: dp("11"), To: dp("20"), Value: dp("4")}})
	}
	t.Run("percentage_add_gated", func(t *testing.T) {
		rule := base70()
		rule.Modifiers = []PricePlanRuleModifier{{AttributeName: "tier", AttributeValue: "premium", Operator: "eq", ModifierOperator: "add", AsPercentage: true, Value: mustDec("10")}}
		wantNet(t, e, rule, computeRes(map[string]any{"vcpu": 15, "tier": "premium"}), "77") // 70 + 70*10/100
		wantNet(t, e, rule, computeRes(map[string]any{"vcpu": 15, "tier": "basic"}), "70")   // gate false
	})
	t.Run("sequential_chain_pct_of_current", func(t *testing.T) {
		rule := priceRule("existence", []PriceTier{{From: nil, To: nil, Value: dp("100")}})
		rule.Modifiers = []PricePlanRuleModifier{
			{AttributeName: "tier", AttributeValue: "premium", Operator: "eq", ModifierOperator: "add", AsPercentage: true, Value: mustDec("10")},
			{AttributeName: "tier", AttributeValue: "premium", Operator: "eq", ModifierOperator: "subtract", AsPercentage: true, Value: mustDec("10")},
		}
		wantNet(t, e, rule, computeRes(map[string]any{"tier": "premium"}), "99") // 100 -> 110 -> 110-11
	})
	t.Run("subtract_fixed_and_unknown_op", func(t *testing.T) {
		rule := priceRule("existence", []PriceTier{{From: nil, To: nil, Value: dp("50")}})
		rule.Modifiers = []PricePlanRuleModifier{{AttributeName: "tier", AttributeValue: "premium", Operator: "eq", ModifierOperator: "subtract", AsPercentage: false, Value: mustDec("5")}}
		wantNet(t, e, rule, computeRes(map[string]any{"tier": "premium"}), "45")
		rule.Modifiers[0].ModifierOperator = "multiply"
		wantNet(t, e, rule, computeRes(map[string]any{"tier": "premium"}), "50") // unknown op -> unchanged
	})
}

func TestMonthlyProration(t *testing.T) {
	at := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	e := NewEngine(fixedClock{t: at})
	tier := []PriceTier{{From: nil, To: nil, Value: dp("100")}}

	t.Run("prorated_month", func(t *testing.T) {
		rule := PricePlanRule{TimeUnit: "month", ResourceType: "compute", Prices: []PricePlanRulePrice{{AttributeName: "existence", Tiers: tier}}}
		res := &BillingResource{ResourceType: "compute", CreatedAt: &at, Values: map[string]any{}, BillingResourceType: computeType()}
		wantNet(t, e, rule, res, "53.0913978494623655913978494623655880") // (100/744 DECIMAL128) * 395 hours
	})
	t.Run("no_proration_when_display_price", func(t *testing.T) {
		rule := PricePlanRule{TimeUnit: "month", ResourceType: "compute", Prices: []PricePlanRulePrice{{AttributeName: "existence", Tiers: tier}}}
		res := &BillingResource{ResourceType: "compute", CreatedAt: &at, DisplayPrice: true, Values: map[string]any{}, BillingResourceType: computeType()}
		wantNet(t, e, rule, res, "100")
	})
	t.Run("no_proration_when_hourly", func(t *testing.T) {
		rule := PricePlanRule{TimeUnit: "hour", ResourceType: "compute", Prices: []PricePlanRulePrice{{AttributeName: "existence", Tiers: tier}}}
		res := &BillingResource{ResourceType: "compute", CreatedAt: &at, Values: map[string]any{}, BillingResourceType: computeType()}
		wantNet(t, e, rule, res, "100")
	})
}

func TestMultiPriceAmountsAndAggregate(t *testing.T) {
	e := NewEngine(SystemClock())
	rule := PricePlanRule{TimeUnit: "hour", ResourceType: "compute", Prices: []PricePlanRulePrice{
		{AttributeName: "vcpu", Tiers: []PriceTier{{From: dp("1"), To: dp("10"), Value: dp("5")}, {From: dp("11"), To: dp("20"), Value: dp("4")}}},
		{AttributeName: "existence", Tiers: []PriceTier{{From: nil, To: nil, Value: dp("2.5")}}},
	}}
	r, ok := rateSingle(t, e, rule, computeRes(map[string]any{"vcpu": 15}))
	if !ok || len(r.Amounts) != 2 {
		t.Fatalf("expected 2 amounts (not summed in engine), got ok=%v n=%d", ok, len(r.Amounts))
	}
	if total := SumNetAmount([]PricePlanRuleResult{r}); !total.Equal(mustDec("72.5")) {
		t.Errorf("aggregate = %s, want 72.5", total) // 70 + 2.5
	}
}

func TestUndeclaredAttributeErrors(t *testing.T) {
	e := NewEngine(SystemClock())
	// A filter referencing an attribute present in values but NOT declared in the
	// resource type must abort rating with an error (not-found).
	rule := priceRule("vcpu", []PriceTier{{From: nil, To: nil, Value: dp("5")}})
	rule.Filters = []PricePlanRuleFilter{{AttributeName: "undeclared", Operator: "eq", Value: "x"}}
	if _, err := e.ApplyPricePlanRules([]PricePlanRule{rule}, computeRes(map[string]any{"vcpu": 3, "undeclared": "x"}), "hour"); err == nil {
		t.Error("expected error for an undeclared attribute (not-found)")
	}
}

func TestDivDecimal128(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"1", "3", "0.3333333333333333333333333333333333"},     // 34 sig
		{"10", "3", "3.333333333333333333333333333333333"},     // 34 sig
		{"100", "744", "0.1344086021505376344086021505376344"}, // proration per-hour
	}
	for _, c := range cases {
		got := divDecimal128(mustDec(c.a), mustDec(c.b))
		if !got.Equal(mustDec(c.want)) {
			t.Errorf("%s/%s = %s, want %s", c.a, c.b, got, c.want)
		}
	}
	// Banker's rounding (HALF_EVEN, not HALF_UP): a 35-significant tie whose kept
	// digit is even rounds DOWN. HALF_UP would give ...001.
	tie := divDecimal128(mustDec("1.0000000000000000000000000000000005"), mustDec("1"))
	if !tie.Equal(mustDec("1")) {
		t.Errorf("HALF_EVEN tie = %s, want 1 (HALF_UP would round up)", tie)
	}
}

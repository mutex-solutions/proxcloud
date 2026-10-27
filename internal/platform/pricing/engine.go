package pricing

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// Engine holds the rating math. It is pure given an
// injected Clock (used only by monthly proration). Methods that resolve a resource
// attribute can return an error — an undeclared attribute yields a not-found error,
// aborting the whole rating call, so we bubble it up.
type Engine struct{ clock Clock }

// NewEngine builds an Engine with the given Clock (SystemClock in production).
func NewEngine(c Clock) *Engine {
	if c == nil {
		c = SystemClock()
	}
	return &Engine{clock: c}
}

var (
	dZero = decimal.Zero
	dOne  = decimal.NewFromInt(1)
)

// ApplyPricePlanRules rates every rule
// whose timeUnit matches; a rule that does not match the resource yields no result.
func (e *Engine) ApplyPricePlanRules(rules []PricePlanRule, resource *BillingResource, timeUnit string) ([]PricePlanRuleResult, error) {
	out := make([]PricePlanRuleResult, 0, len(rules))
	for i := range rules {
		rule := rules[i]
		if !equalsIgnoreCase(rule.TimeUnit, timeUnit) {
			continue
		}
		res, ok, err := e.applyPricePlanRule(rule, resource)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, res)
		}
	}
	return out, nil
}

// SumNetAmount reduces the rule results: per rule
// sum its amounts' net amounts, then sum across rules (zero default).
func SumNetAmount(results []PricePlanRuleResult) decimal.Decimal {
	total := dZero
	for _, r := range results {
		for _, a := range r.Amounts {
			total = total.Add(a.NetAmount)
		}
	}
	return total
}

func (e *Engine) applyPricePlanRule(rule PricePlanRule, resource *BillingResource) (PricePlanRuleResult, bool, error) {
	apply, err := e.applyFilters(rule, resource)
	if err != nil {
		return PricePlanRuleResult{}, false, err
	}
	if !apply {
		return PricePlanRuleResult{}, false, nil
	}
	amounts, err := e.applyRulePrices(rule, resource, rule.Prices)
	if err != nil {
		return PricePlanRuleResult{}, false, err
	}
	amounts, err = e.applyModifiers(rule.Modifiers, resource, amounts)
	if err != nil {
		return PricePlanRuleResult{}, false, err
	}
	return PricePlanRuleResult{PricePlanRule: rule, Amounts: amounts}, true, nil
}

// applyFilters: resourceType must match (case-insensitive); then ALL filters must pass.
func (e *Engine) applyFilters(rule PricePlanRule, resource *BillingResource) (bool, error) {
	if !equalsIgnoreCase(rule.ResourceType, resource.ResourceType) {
		return false, nil
	}
	if len(rule.Filters) == 0 {
		return true, nil
	}
	for _, f := range rule.Filters {
		ok, err := e.applyFilter(f, resource)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func (e *Engine) applyRulePrices(rule PricePlanRule, resource *BillingResource, prices []PricePlanRulePrice) ([]PricePlanRuleResultAmount, error) {
	if prices == nil {
		return []PricePlanRuleResultAmount{}, nil
	}
	out := make([]PricePlanRuleResultAmount, 0, len(prices))
	for _, p := range prices {
		a, ok, err := e.applyRulePrice(rule, p, resource)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, a)
		}
	}
	return out, nil
}

func (e *Engine) applyRulePrice(rule PricePlanRule, price PricePlanRulePrice, resource *BillingResource) (PricePlanRuleResultAmount, bool, error) {
	name := price.AttributeName
	tiers := price.Tiers
	if tiers == nil || name == "" {
		return PricePlanRuleResultAmount{}, false, nil
	}
	skip, err := e.skipCurrentPrice(resource, name)
	if err != nil {
		return PricePlanRuleResultAmount{}, false, err
	}
	if skip {
		return PricePlanRuleResultAmount{}, false, nil
	}
	value, err := e.getResourceValueByType(name, resource)
	if err != nil {
		return PricePlanRuleResultAmount{}, false, err
	}
	total := dZero
	if value.Cmp(dZero) > 0 {
		for _, t := range tiers {
			tv, err := e.applyTier(rule, resource, t, value)
			if err != nil {
				return PricePlanRuleResultAmount{}, false, err
			}
			total = total.Add(tv)
		}
	}
	return PricePlanRuleResultAmount{AttributeName: name, AttributeValue: value, NetAmount: total}, true, nil
}

func (e *Engine) skipCurrentPrice(resource *BillingResource, attributeName string) (bool, error) {
	if resource.DisplayPrice && attributeName != "existence" {
		attr, err := resource.BillingResourceType.ResourceAttributeByName(attributeName)
		if err != nil {
			return false, err
		}
		if attr.IsUsage != nil && *attr.IsUsage {
			return true, nil
		}
	}
	return false, nil
}

func (e *Engine) getResourceValueByType(attributeName string, resource *BillingResource) (decimal.Decimal, error) {
	if equalsIgnoreCase(attributeName, "existence") {
		return dOne, nil
	}
	v, present := resource.Values[attributeName]
	if !present {
		return dZero, nil
	}
	attrType, err := resource.BillingResourceType.AttributeTypeByName(attributeName)
	if err != nil {
		return dZero, err
	}
	if equalsIgnoreCase(attrType, "boolean") {
		b, ok := toBool(v)
		if !ok {
			// A non-boolean value for a boolean-declared attribute aborts the whole
			// charge for the profile rather than being silently rated ZERO.
			return dZero, fmt.Errorf("rating: attribute %q is declared boolean but its value is not a boolean: %v", attributeName, v)
		}
		if b {
			return dOne, nil
		}
		return dZero, nil
	}
	d, ok := toDecimal(v)
	if !ok {
		// A non-numeric value for a numeric attribute aborts the whole profile's charge
		// rather than being silently rated ZERO (which would under-/over-bill a malformed attribute).
		return dZero, fmt.Errorf("rating: attribute %q is not convertible to a number: %v", attributeName, v)
	}
	return d, nil
}

// applyTier applies graduated tiers (the from-1
// inclusive-lower-bound adjustment, open-ended top tier, and from==nil flat tier).
// No rounding/scale — full precision.
func (e *Engine) applyTier(rule PricePlanRule, resource *BillingResource, tier PriceTier, resourceValue decimal.Decimal) (decimal.Decimal, error) {
	from := tier.From
	to := tier.To
	amount, err := e.getTierValue(rule, resource, tier)
	if err != nil {
		return dZero, err
	}
	result := dZero
	if from == nil {
		result = resourceValue
	} else if resourceValue.Cmp(*from) >= 0 {
		if to == nil {
			result = resourceValue.Sub(*from)
		} else {
			adjFrom := *from
			if from.Cmp(dZero) != 0 {
				adjFrom = from.Sub(dOne)
			}
			tierMax := to.Sub(adjFrom)
			fromDiff := resourceValue.Sub(adjFrom)
			if fromDiff.Cmp(tierMax) > 0 {
				result = tierMax
			} else {
				result = fromDiff
			}
		}
	}
	return result.Mul(amount), nil
}

// getTierValue: for a "month" rule on a
// non-display resource created mid-month, prorate the tier value per hour
// (DECIMAL128 division) × elapsed hours.
func (e *Engine) getTierValue(rule PricePlanRule, resource *BillingResource, tier PriceTier) (decimal.Decimal, error) {
	// A null tier value would fail the multiply/divide below, aborting the whole
	// profile's charge (same class as the malformed-attribute abort). Error out
	// instead of silently rating 0.
	if tier.Value == nil {
		return dZero, fmt.Errorf("price tier has no value (rule %s)", rule.ID)
	}
	value := *tier.Value
	if equalsIgnoreCase(rule.TimeUnit, "month") && !resource.DisplayPrice {
		createdAt := e.clock.Now()
		if resource.CreatedAt != nil {
			createdAt = *resource.CreatedAt
		}
		now := e.clock.Now()
		if createdAt.After(firstDayOfCurrentMonth(now)) {
			lastDay := lastDayOfCurrentMonth(now)
			hoursDiff := int64(lastDay.Sub(createdAt) / 3600000000000) // whole hours between, truncated toward zero
			maxHours := int64(totalHoursCurrentMonth(now))
			if hoursDiff < maxHours {
				pricePerHour := divDecimal128(value, decimal.NewFromInt(maxHours))
				value = pricePerHour.Mul(decimal.NewFromInt(hoursDiff))
			}
		}
	}
	return value, nil
}

// applyModifiers chains modifiers over each
// amount's net amount (each modifier's output feeds the next), mutating in place.
func (e *Engine) applyModifiers(modifiers []PricePlanRuleModifier, resource *BillingResource, amounts []PricePlanRuleResultAmount) ([]PricePlanRuleResultAmount, error) {
	if modifiers == nil {
		return amounts, nil
	}
	for i := range amounts {
		mp := amounts[i].NetAmount
		for _, m := range modifiers {
			v, err := e.applyModifier(m, resource, mp)
			if err != nil {
				return nil, err
			}
			mp = v
		}
		amounts[i].NetAmount = mp
	}
	return amounts, nil
}

func (e *Engine) applyModifier(modifier PricePlanRuleModifier, resource *BillingResource, basePrice decimal.Decimal) (decimal.Decimal, error) {
	values := resource.Values
	if values == nil {
		return basePrice, nil
	}
	v, present := values[modifier.AttributeName]
	if !present {
		return basePrice, nil
	}
	attrType, err := resource.BillingResourceType.AttributeTypeByName(modifier.AttributeName)
	if err != nil {
		return basePrice, err
	}
	if v == nil {
		return basePrice, nil
	}
	if e.applyOperator(modifier.Operator, modifier.AttributeValue, v, attrType) {
		return e.applyModifierValue(modifier, basePrice), nil
	}
	return basePrice, nil
}

func (e *Engine) applyModifierValue(modifier PricePlanRuleModifier, basePrice decimal.Decimal) decimal.Decimal {
	var modifierValue decimal.Decimal
	if modifier.AsPercentage {
		modifierValue = divDecimal128(basePrice.Mul(modifier.Value), decimal.NewFromInt(100))
	} else {
		modifierValue = modifier.Value
	}
	switch modifier.ModifierOperator {
	case "add":
		return basePrice.Add(modifierValue)
	case "subtract":
		return basePrice.Sub(modifierValue)
	default:
		return basePrice
	}
}

func (e *Engine) applyFilter(filter PricePlanRuleFilter, resource *BillingResource) (bool, error) {
	if resource.Values == nil {
		return false, nil
	}
	v, present := resource.Values[filter.AttributeName]
	if !present {
		return false, nil
	}
	if v == nil {
		return false, nil
	}
	attrType, err := resource.BillingResourceType.AttributeTypeByName(filter.AttributeName)
	if err != nil {
		return false, err
	}
	if filter.Operator == "in" {
		return e.applyOperatorOnList(filter.Operator, filter.Values, v, attrType), nil
	}
	return e.applyOperator(filter.Operator, filter.Value, v, attrType), nil
}

// applyOperator evaluates a comparison operator. NOTE the "number" branch
// SWAPS arguments (v2=ruleValue, v1=resourceValue) — comparisons are resource-vs-rule.
func (e *Engine) applyOperator(operator string, ruleValue, resourceValue any, typ string) bool {
	switch typ {
	case "boolean":
		v1, _ := toBool(ruleValue)
		v2, _ := toBool(resourceValue)
		return v1 == v2
	case "string":
		v1 := toString(ruleValue)
		v2 := toString(resourceValue)
		switch operator {
		case "eq":
			return equalsIgnoreCase(v1, v2)
		case "neq":
			return !equalsIgnoreCase(v1, v2)
		case "contains":
			return containsIgnoreCase(v2, v1)
		case "startsWith":
			return startsWithIgnoreCase(v2, v1)
		default:
			return false
		}
	case "number":
		v2, ok2 := toDecimal(ruleValue)
		v1, ok1 := toDecimal(resourceValue)
		if !ok1 || !ok2 {
			return false
		}
		c := v1.Cmp(v2)
		switch operator {
		case "eq":
			return c == 0
		case "neq":
			return c != 0
		case "gt":
			return c > 0
		case "gte":
			return c >= 0
		case "lt":
			return c < 0
		case "lte":
			return c <= 0
		default:
			return false
		}
	default:
		return false
	}
}

// applyOperatorOnList: only string type +
// "in" operator; membership is CASE-SENSITIVE exact (list contains), unlike eq/neq.
func (e *Engine) applyOperatorOnList(operator string, ruleValues []any, resourceValue any, typ string) bool {
	if typ != "string" {
		return false
	}
	v1List := toStringSlice(ruleValues)
	if len(v1List) == 0 {
		return false
	}
	if operator != "in" {
		return false
	}
	v2 := toString(resourceValue)
	for _, s := range v1List {
		if s == v2 {
			return true
		}
	}
	return false
}

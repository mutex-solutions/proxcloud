package pricing

import (
	"sort"
	"time"

	"github.com/cockroachdb/apd/v3"
	"github.com/shopspring/decimal"
)

// adjustments.go applies the charge-time bill adjustments after rating each resource:
//   - a savings contract discounts the bill when target usage
//     reaches its monthly commitment.
//   - a price-plan-scoped rule adds a tiered add/subtract adjustment.
// Both build a BillingResource per matching bill item (from item.metadata + the catalog type) and
// reuse the Engine's filter matcher. The savings
// discount divides at precision 2, HALF_UP; the price-adjustment percentage uses DECIMAL128.

// AdjustmentTarget mirrors SavingsPlanTarget / PriceAdjustmentTarget: bill items of ResourceType
// passing ALL Filters contribute to the target amount.
type AdjustmentTarget struct {
	ResourceType string                `json:"resourceType,omitempty"`
	Filters      []PricePlanRuleFilter `json:"filters,omitempty"`
}

// SavingsContractAdj is the savings-contract input to the bill discount (SavingsContract subset).
// DiscountRate is a whole/decimal percent (e.g. 10 = 10%).
type SavingsContractAdj struct {
	ID                     string
	SavingsPlanName        string
	MonthlyCommittedAmount decimal.Decimal
	DiscountRate           decimal.Decimal
	PaidUpfront            bool
	StartDate              *time.Time
	EndDate                *time.Time
	Targets                []AdjustmentTarget
}

// PriceAdjustmentRule is the price-adjustment-rule domain (the charge-relevant subset; json
// tags let a repo decode the `priceAdjustmentRule` records straight into it).
type PriceAdjustmentRule struct {
	ID          string                `json:"id,omitempty"`
	Name        string                `json:"name,omitempty"`
	Description string                `json:"description,omitempty"`
	PricePlanID string                `json:"pricePlanId,omitempty"`
	Enabled     bool                  `json:"enabled"`
	Targets     []AdjustmentTarget    `json:"targets,omitempty"`
	Tiers       []PriceAdjustmentTier `json:"tiers,omitempty"`
}

type PriceAdjustmentTier struct {
	StartAmount decimal.Decimal         `json:"startAmount"`
	Modifier    PriceAdjustmentModifier `json:"modifier"`
}

type PriceAdjustmentModifier struct {
	AsPercentage bool            `json:"asPercentage"`
	Value        decimal.Decimal `json:"value"`
	Operator     string          `json:"operator,omitempty"`
}

// divMathCtx2 divides at 2 significant
// digits, HALF_UP (the savings discount fractions).
func divMathCtx2(a, b decimal.Decimal) decimal.Decimal {
	ad, _, _ := apd.NewFromString(a.String())
	bd, _, _ := apd.NewFromString(b.String())
	res := new(apd.Decimal)
	_, _ = mathCtx2.Quo(res, ad, bd)
	out, _ := decimal.NewFromString(res.Text('f'))
	return out
}

func decimalMin(a, b decimal.Decimal) decimal.Decimal {
	if a.Cmp(b) <= 0 {
		return a
	}
	return b
}

// catalogTypeFor finds the BillingResourceType for a resourceType (nil if absent).
func catalogTypeFor(catalog []*BillingResourceType, resourceType string) *BillingResourceType {
	for _, t := range catalog {
		if t != nil && t.ResourceType == resourceType {
			return t
		}
	}
	return nil
}

// matchesAllFilters — every filter passes (a filter error → false).
func (e *Engine) matchesAllFilters(filters []PricePlanRuleFilter, resource *BillingResource) bool {
	for i := range filters {
		ok, err := e.applyFilter(filters[i], resource)
		if err != nil || !ok {
			return false
		}
	}
	return true
}

// targetItemsAmount sums the net amount of bill items matching any target:
// item.resourceType == target.resourceType AND all target filters pass.
func (e *Engine) targetItemsAmount(bill *Bill, targets []AdjustmentTarget, catalog []*BillingResourceType) decimal.Decimal {
	total := decimal.Zero
	for ti := range targets {
		target := &targets[ti]
		brt := catalogTypeFor(catalog, target.ResourceType)
		for i := range bill.Items {
			it := &bill.Items[i]
			if it.ResourceType != target.ResourceType {
				continue
			}
			res := &BillingResource{ResourceType: it.ResourceType, ResourceID: it.ResourceID, Values: it.Metadata, BillingResourceType: brt}
			if e.matchesAllFilters(target.Filters, res) {
				total = total.Add(it.NetAmount)
			}
		}
	}
	return total
}

func sumItemsNet(bill *Bill) decimal.Decimal {
	total := decimal.Zero
	for i := range bill.Items {
		total = total.Add(bill.Items[i].NetAmount)
	}
	return total
}

// upsertAdjustment sets the amount on an existing matching adjustment, else appends
// the new one.
func upsertAdjustment(bill *Bill, match func(*BillAdjustment) bool, neu BillAdjustment) {
	for i := range bill.Adjustments {
		if match(&bill.Adjustments[i]) {
			bill.Adjustments[i].Amount = neu.Amount
			return
		}
	}
	bill.Adjustments = append(bill.Adjustments, neu)
}

// ApplySavingsContractDiscounts applies the savings-contract discount:
// for each contract whose target usage ≥ monthlyCommittedAmount, upsert a SAVINGS_CONTRACT
// adjustment (keyed by contract id). Mutates bill.Adjustments in place.
func (e *Engine) ApplySavingsContractDiscounts(bill *Bill, contracts []SavingsContractAdj, catalog []*BillingResourceType) {
	for i := range contracts {
		c := &contracts[i]
		usage := e.targetItemsAmount(bill, c.Targets, catalog)
		if usage.Cmp(c.MonthlyCommittedAmount) < 0 {
			continue
		}
		amt := savingsAdjustmentAmount(c, usage)
		cid := c.ID
		upsertAdjustment(bill, func(a *BillAdjustment) bool { return a.ContractID == cid }, BillAdjustment{
			Amount:            &amt,
			Type:              AdjustmentTypeSavingsContract,
			Description:       "Adjustment based on Savings Contract " + c.SavingsPlanName,
			SavingsPlanName:   c.SavingsPlanName,
			StartDateContract: c.StartDate,
			EndDateContract:   c.EndDate,
			ContractID:        c.ID,
		})
	}
}

// savingsAdjustmentAmount computes the savings adjustment (a negative = a discount).
func savingsAdjustmentAmount(c *SavingsContractAdj, usage decimal.Decimal) decimal.Decimal {
	committed := c.MonthlyCommittedAmount
	if c.PaidUpfront {
		// upfront = committed; return -min(committed, usage)
		return decimalMin(committed, usage).Neg()
	}
	rateFrac := divMathCtx2(c.DiscountRate, decimal.NewFromInt(100)) // discountRate/100
	discount := decimalMin(usage, committed).Mul(rateFrac)
	// diff = usage - (committed - (committed*discountRate)/100)
	committedDiscount := divMathCtx2(committed.Mul(c.DiscountRate), decimal.NewFromInt(100))
	diff := usage.Sub(committed.Sub(committedDiscount))
	if diff.Cmp(decimal.Zero) >= 0 {
		return discount.Neg()
	}
	return diff.Neg()
}

// ApplyPriceAdjustmentRules applies the price-adjustment rules: for each
// enabled rule, compute the target amount (all items when the rule has no targets) and add the
// tiered modifier as a PRICE_ADJUSTMENT_RULE adjustment (keyed by rule id). Mutates in place.
func (e *Engine) ApplyPriceAdjustmentRules(bill *Bill, rules []PriceAdjustmentRule, catalog []*BillingResourceType) {
	for i := range rules {
		r := &rules[i]
		if !r.Enabled {
			continue
		}
		var usage decimal.Decimal
		if len(r.Targets) > 0 {
			usage = e.targetItemsAmount(bill, r.Targets, catalog)
		} else {
			usage = sumItemsNet(bill)
		}
		if usage.IsZero() {
			continue
		}
		amt := tieredAdjustmentAmount(r.Tiers, usage)
		if amt.IsZero() {
			continue
		}
		desc := r.Description
		if desc == "" {
			desc = r.Name
		}
		rid := r.ID
		upsertAdjustment(bill, func(a *BillAdjustment) bool { return a.PriceAdjustmentRuleID == rid }, BillAdjustment{
			Amount:                  &amt,
			Type:                    AdjustmentTypePriceAdjustmentRule,
			Description:             desc,
			PriceAdjustmentRuleID:   r.ID,
			PriceAdjustmentRuleName: r.Name,
		})
	}
}

// tieredAdjustmentAmount: tiers sorted DESC by startAmount;
// the first tier whose startAmount ≤ amount supplies the modifier.
func tieredAdjustmentAmount(tiers []PriceAdjustmentTier, amount decimal.Decimal) decimal.Decimal {
	if len(tiers) == 0 {
		return decimal.Zero
	}
	sorted := append([]PriceAdjustmentTier(nil), tiers...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].StartAmount.Cmp(sorted[j].StartAmount) > 0 })
	for i := range sorted {
		if amount.Cmp(sorted[i].StartAmount) >= 0 {
			return paModifierValue(sorted[i].Modifier, amount)
		}
	}
	return decimal.Zero
}

// paModifierValue: percentage → base*value/100
// (DECIMAL128); operator add → +modifier, subtract → −modifier, else → base.
func paModifierValue(m PriceAdjustmentModifier, base decimal.Decimal) decimal.Decimal {
	v := m.Value
	if m.AsPercentage {
		v = divDecimal128(base.Mul(m.Value), decimal.NewFromInt(100))
	}
	switch m.Operator {
	case "add":
		return v
	case "subtract":
		return v.Neg()
	default:
		return base
	}
}

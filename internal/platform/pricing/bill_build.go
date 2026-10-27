package pricing

import (
	"time"

	"github.com/shopspring/decimal"
)

// round16 rounds to scale 16, HALF_UP (used as a
// scale not a sig-figs context). HALF_UP == half away from zero for BOTH signs, so
// shopspring Round(16) matches even for negative accumulations.
func round16(d decimal.Decimal) decimal.Decimal { return d.Round(16) }

func ptr64(v int64) *int64 { return &v }

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// SaveChargingToBill finds-or-creates the
// resource's BillItem, then accumulates this cadence's rated results onto it in the
// bill's base/product currency (NO tax, NO FX — those act on the bill aggregate,
// see billagg.go). Mutates bill in place. `clockNow` supplies the current time
// for the new-item + updatedAt stamps (deterministic; those fields are masked).
func SaveChargingToBill(rc RatingContext, bc BillingContext, bill *Bill, res *BillingResource, results []PricePlanRuleResult, clockNow time.Time) {
	if bill.Items == nil {
		bill.Items = []BillItem{}
	}
	idx := -1
	for i := range bill.Items {
		if bill.Items[i].ResourceID == res.ResourceID && bill.Items[i].ResourceType == res.ResourceType {
			idx = i
			break
		}
	}
	if idx < 0 {
		last := getLastRateTime(bill, res)
		now := clockNow
		bill.Items = append(bill.Items, BillItem{
			Name:                  getBillItemName(res),
			ProjectID:             res.ProjectID,
			NetAmount:             decimal.Zero,
			TimeUnits:             &BillItemTimeUnits{Minute: ptr64(0), Hour: ptr64(0), Month: ptr64(0), MinuteLastRateTime: last, HourLastRateTime: last},
			ResourceID:            res.ResourceID,
			ResourceType:          res.ResourceType,
			CreatedAt:             &now,
			Currency:              bill.InvoiceCurrency,
			UpdatedAt:             &now,
			Metadata:              res.Values,
			AppliedPricePlanRules: []BillItemAppliedPricePlanRule{},
		})
		idx = len(bill.Items) - 1
	}
	item := &bill.Items[idx]
	if item.AppliedPricePlanRules == nil {
		item.AppliedPricePlanRules = []BillItemAppliedPricePlanRule{}
	}
	updateBillItems(rc, bc, res, results, item)
	now := clockNow
	item.UpdatedAt = &now
}

// getLastRateTime returns nil when the resource has no
// createdAt, OR when the bill has no billing cycle / start date. Otherwise the
// later of createdAt and the cycle start.
func getLastRateTime(bill *Bill, res *BillingResource) *time.Time {
	if res.CreatedAt == nil {
		return nil
	}
	if bill.BillingCycle == nil || bill.BillingCycle.StartDate == nil {
		return nil
	}
	createdAt := *res.CreatedAt
	start := *bill.BillingCycle.StartDate
	if createdAt.Before(start) {
		return &start
	}
	return &createdAt
}

// getBillItemName returns values["display_name"] if
// present, else the resource id.
func getBillItemName(res *BillingResource) string {
	if v, ok := res.Values["display_name"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return res.ResourceID
}

// updateBillItems: skip ineligible resources; gate
// on the cap (frozen once net ≥ cap); advance the cadence counter; create/update the
// applied price-plan-rule amounts; re-derive item.NetAmount = round16(min(Σ, cap)).
func updateBillItems(rc RatingContext, bc BillingContext, res *BillingResource, results []PricePlanRuleResult, item *BillItem) {
	if res.NotEligibleForBilling {
		return
	}
	totalCap := getTotalCap(bc, rc.TimeUnit, results)
	if !(totalCap.Cmp(item.NetAmount) > 0) { // strict >
		return
	}
	diff := getTimeUnitsDiff(bc, rc, item, res)
	ct := rc.CycleTimestamp
	switch rc.TimeUnit {
	case TimeUnitMinute:
		*item.TimeUnits.Minute = diff + *item.TimeUnits.Minute
		item.TimeUnits.MinuteLastRateTime = &ct
	case TimeUnitHour:
		*item.TimeUnits.Hour = diff + *item.TimeUnits.Hour
		item.TimeUnits.HourLastRateTime = &ct
	case TimeUnitMonth:
		*item.TimeUnits.Month = diff + *item.TimeUnits.Month
		item.TimeUnits.MonthLastRateTime = &ct
	}
	for i := range results {
		ppid := results[i].PricePlanRule.ID
		if applied := findAppliedRule(item, ppid); applied != nil {
			updateAppliedPricePlanRule(results[i], applied, diff)
		} else {
			createAppliedPricePlanRule(item, results[i], ppid, diff)
		}
	}
	calc := calculateBillItemAppliedAmounts(item)
	net := calc
	if calc.Cmp(totalCap) > 0 {
		net = totalCap
	}
	item.NetAmount = round16(net)
	item.Name = getBillItemName(res)
	item.Metadata = res.Values
}

// getTotalCap: Σ over results of
// (timeUnitLimit × Σ amounts). No rounding.
func getTotalCap(bc BillingContext, timeUnit string, results []PricePlanRuleResult) decimal.Decimal {
	limit := decimal.NewFromInt(int64(bc.timeUnitLimit(timeUnit)))
	total := decimal.Zero
	for i := range results {
		sum := decimal.Zero
		for _, a := range results[i].Amounts {
			sum = sum.Add(a.NetAmount)
		}
		total = total.Add(limit.Mul(sum))
	}
	return total
}

// getTimeUnitsDiff: MONTH always 1; MINUTE/HOUR
// = 1 on a nil watermark, else min(elapsed-units, remaining-headroom) — NOT floored
// at zero. chargeTime = deletedAt if set, else the cycle timestamp.
func getTimeUnitsDiff(bc BillingContext, rc RatingContext, item *BillItem, res *BillingResource) int64 {
	chargeTime := rc.CycleTimestamp
	if res.DeletedAt != nil {
		chargeTime = *res.DeletedAt
	}
	limit := int64(bc.timeUnitLimit(rc.TimeUnit))
	switch rc.TimeUnit {
	case TimeUnitMinute:
		if item.TimeUnits.MinuteLastRateTime == nil {
			return 1
		}
		elapsed := int64(chargeTime.Sub(*item.TimeUnits.MinuteLastRateTime) / time.Minute)
		return min64(elapsed, limit-*item.TimeUnits.Minute)
	case TimeUnitHour:
		if item.TimeUnits.HourLastRateTime == nil {
			return 1
		}
		elapsed := int64(chargeTime.Sub(*item.TimeUnits.HourLastRateTime) / time.Hour)
		return min64(elapsed, limit-*item.TimeUnits.Hour)
	case TimeUnitMonth:
		return 1
	}
	return 0
}

func calculateBillItemAppliedAmounts(item *BillItem) decimal.Decimal {
	total := decimal.Zero
	for i := range item.AppliedPricePlanRules {
		for _, a := range item.AppliedPricePlanRules[i].AppliedAmounts {
			total = total.Add(a.NetAmount)
		}
	}
	return total
}

// createAppliedPricePlanRule: per amount
// round16(perUnit) × diff (round FIRST, then × diff).
func createAppliedPricePlanRule(item *BillItem, result PricePlanRuleResult, ppid string, diff int64) {
	applied := BillItemAppliedPricePlanRule{PricePlanRuleID: ppid, AppliedAmounts: []BillItemAppliedPricePlanRuleAmount{}}
	d := decimal.NewFromInt(diff)
	for _, amt := range result.Amounts {
		net := round16(amt.NetAmount).Mul(d)
		applied.AppliedAmounts = append(applied.AppliedAmounts, BillItemAppliedPricePlanRuleAmount{
			NetAmount: net, AttributeName: amt.AttributeName, LastAttributeValue: amt.AttributeValue,
		})
	}
	item.AppliedPricePlanRules = append(item.AppliedPricePlanRules, applied)
}

// updateAppliedPricePlanRule: per amount
// round16(perUnit) then — for an existing attribute — OVERWRITE_TOTAL stores it bare,
// else accumulate prev + perUnit×diff; a NEW attribute stores bare perUnit (NO ×diff,
// asymmetric with create).
func updateAppliedPricePlanRule(result PricePlanRuleResult, applied *BillItemAppliedPricePlanRule, diff int64) {
	if applied.AppliedAmounts == nil {
		applied.AppliedAmounts = []BillItemAppliedPricePlanRuleAmount{}
	}
	overwrite := applyOverwriteTotal(result.PricePlanRule)
	d := decimal.NewFromInt(diff)
	for _, amt := range result.Amounts {
		net := round16(amt.NetAmount)
		if existing := findAppliedAmount(applied, amt.AttributeName); existing != nil {
			if overwrite {
				existing.NetAmount = net
			} else {
				existing.NetAmount = existing.NetAmount.Add(net.Mul(d))
			}
			existing.LastAttributeValue = amt.AttributeValue
		} else {
			applied.AppliedAmounts = append(applied.AppliedAmounts, BillItemAppliedPricePlanRuleAmount{
				NetAmount: net, AttributeName: amt.AttributeName, LastAttributeValue: amt.AttributeValue,
			})
		}
	}
}

// applyOverwriteTotal reports whether the rule overwrites the total (null applyMethod → false).
func applyOverwriteTotal(rule PricePlanRule) bool { return rule.ApplyMethod == ApplyOverwriteTotal }

func findAppliedRule(item *BillItem, ppid string) *BillItemAppliedPricePlanRule {
	for i := range item.AppliedPricePlanRules {
		if item.AppliedPricePlanRules[i].PricePlanRuleID == ppid {
			return &item.AppliedPricePlanRules[i]
		}
	}
	return nil
}

func findAppliedAmount(applied *BillItemAppliedPricePlanRule, attr string) *BillItemAppliedPricePlanRuleAmount {
	for i := range applied.AppliedAmounts {
		if applied.AppliedAmounts[i].AttributeName == attr {
			return &applied.AppliedAmounts[i]
		}
	}
	return nil
}

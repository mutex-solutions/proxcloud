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
		// A blind interval (stale cloud state) is written off instead of back-billed on recovery;
		// every other ineligible state (BUILD, …) keeps its watermark and is back-billed.
		if res.SkipInterval {
			advanceWatermark(rc, item, res)
		}
		return
	}
	totalCap := getTotalCap(bc, rc.TimeUnit, results)
	if !(totalCap.Cmp(item.NetAmount) > 0) { // strict >
		return
	}
	diff := getTimeUnitsDiff(bc, rc, item, res)
	switch rc.TimeUnit {
	case TimeUnitMinute:
		*item.TimeUnits.Minute = diff + *item.TimeUnits.Minute
	case TimeUnitHour:
		*item.TimeUnits.Hour = diff + *item.TimeUnits.Hour
	case TimeUnitMonth:
		*item.TimeUnits.Month = diff + *item.TimeUnits.Month
	}
	advanceWatermark(rc, item, res)
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

// advanceWatermark never goes past the original cutoff or rewinds a newer mark.
// Deleted resources use the earlier deletion, rounded up by getTimeUnitsDiff on
// their final interval; repeating that ended interval is a no-op.
func advanceWatermark(rc RatingContext, item *BillItem, res *BillingResource) {
	ct := rc.CycleTimestamp
	if res.DeletedAt != nil && res.DeletedAt.Before(ct) {
		ct = *res.DeletedAt
	}
	switch rc.TimeUnit {
	case TimeUnitMinute:
		if last := item.TimeUnits.MinuteLastRateTime; last == nil || ct.After(*last) {
			item.TimeUnits.MinuteLastRateTime = &ct
		}
	case TimeUnitHour:
		if last := item.TimeUnits.HourLastRateTime; last == nil || ct.After(*last) {
			item.TimeUnits.HourLastRateTime = &ct
		}
	case TimeUnitMonth:
		if last := item.TimeUnits.MonthLastRateTime; last == nil || ct.After(*last) {
			item.TimeUnits.MonthLastRateTime = &ct
		}
	}
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

// getTimeUnitsDiff: MONTH always 1; MINUTE/HOUR = 1 on a nil watermark, else the units started
// since the watermark, clamped to [0, remaining headroom]. chargeTime = deletedAt if set, else the
// cycle timestamp.
//
// Units are counted up (a started hour is a billed hour), so a resource pays for every clock unit
// it existed in: created 10:20, deleted 12:50 = 3 hours (10, 11, 12). The watermark sits on a unit
// boundary after the first charge, so this only changes the first partial interval and the last
// one. The floor makes re-rating an archived row (deletedAt behind the watermark) a no-op instead
// of a refund, which is what lets the charge job feed archived rows on every pass.
func getTimeUnitsDiff(bc BillingContext, rc RatingContext, item *BillItem, res *BillingResource) int64 {
	chargeTime := rc.CycleTimestamp
	if res.DeletedAt != nil && res.DeletedAt.Before(chargeTime) {
		chargeTime = *res.DeletedAt
	}
	limit := int64(bc.timeUnitLimit(rc.TimeUnit))
	var last *time.Time
	var used int64
	var unit time.Duration
	switch rc.TimeUnit {
	case TimeUnitMinute:
		last, used, unit = item.TimeUnits.MinuteLastRateTime, *item.TimeUnits.Minute, time.Minute
	case TimeUnitHour:
		last, used, unit = item.TimeUnits.HourLastRateTime, *item.TimeUnits.Hour, time.Hour
	case TimeUnitMonth:
		return 1
	default:
		return 0
	}
	if last == nil {
		return 1
	}
	elapsed := chargeTime.Sub(*last)
	units := int64(elapsed / unit)
	if elapsed%unit > 0 {
		units++
	}
	return max(0, min64(units, limit-used))
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

// createAppliedPricePlanRule: per amount round16(perUnit) × diff (round FIRST, then × diff).
// OVERWRITE_TOTAL stores the amount bare, like updateAppliedPricePlanRule: a usage value is already
// a month-to-date total, and multiplying it by the hours since the resource was created would bill
// it hundreds of times over the first time it is rated.
func createAppliedPricePlanRule(item *BillItem, result PricePlanRuleResult, ppid string, diff int64) {
	applied := BillItemAppliedPricePlanRule{PricePlanRuleID: ppid, AppliedAmounts: []BillItemAppliedPricePlanRuleAmount{}}
	d := decimal.NewFromInt(diff)
	if applyOverwriteTotal(result.PricePlanRule) {
		d = decimal.NewFromInt(1)
	}
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

// ProjectedAccrual is what `units` more charge passes of timeUnit would add to a bill item, given the
// resource's rating results for that time unit (the month-end forecast; nothing is written). item is
// the resource's bill item, nil when it has none yet; net is that item's net as the projection
// stands (item.NetAmount plus what earlier time units already projected onto it). It follows
// updateBillItems:
//   - per unit it adds Σ round16(amount), the per-unit accrual createAppliedPricePlanRule and
//     updateAppliedPricePlanRule multiply by the units;
//   - the units stop at the time-unit limit (limit − units already charged this month), so a month
//     rule charged once is never projected again;
//   - the item stops at its cap (getTotalCap): nothing is added once its net reaches it.
//
// An OVERWRITE_TOTAL result on a minute or hour rule adds nothing: its amount is a month-to-date
// usage total that replaces the previous one, and how the usage grows is unknown. usageHeld reports
// that such a total was held at its current value, so the caller can call the forecast approximate.
// On a month rule the single charge stores the amount as is, so it counts like any other.
func ProjectedAccrual(bc BillingContext, timeUnit string, results []PricePlanRuleResult, item *BillItem, net decimal.Decimal, units int64) (add decimal.Decimal, usageHeld bool) {
	if units <= 0 {
		return decimal.Zero, false
	}
	perUnit := decimal.Zero
	for i := range results {
		overwrite := applyOverwriteTotal(results[i].PricePlanRule) && timeUnit != TimeUnitMonth
		for _, a := range results[i].Amounts {
			if overwrite {
				usageHeld = usageHeld || !a.NetAmount.IsZero()
				continue
			}
			perUnit = perUnit.Add(round16(a.NetAmount))
		}
	}
	if perUnit.IsZero() {
		return decimal.Zero, usageHeld
	}
	var used int64
	if item != nil && item.TimeUnits != nil {
		var counter *int64
		switch timeUnit {
		case TimeUnitMinute:
			counter = item.TimeUnits.Minute
		case TimeUnitHour:
			counter = item.TimeUnits.Hour
		case TimeUnitMonth:
			counter = item.TimeUnits.Month
		}
		if counter != nil {
			used = *counter
		}
	}
	units = min64(units, int64(bc.timeUnitLimit(timeUnit))-used)
	if units <= 0 {
		return decimal.Zero, usageHeld
	}
	totalCap := getTotalCap(bc, timeUnit, results)
	if !(totalCap.Cmp(net) > 0) {
		return decimal.Zero, usageHeld
	}
	add = perUnit.Mul(decimal.NewFromInt(units))
	if room := totalCap.Sub(net); add.Cmp(room) > 0 {
		add = room
	}
	return add, usageHeld
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

package pricing

import (
	"encoding/json"
	"math/rand"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// The four functions below are the bill-item accrual as it was before the engine learned running
// intervals (ChargeFrom, VariableRate, EarlierInterval): SaveChargingToBill, updateBillItems,
// advanceWatermark and getTimeUnitsDiff, copied verbatim under test-only names. Everything they call
// (the cap, the two rounding functions, the first watermark) is unchanged and shared.
// TestResourcesWithoutIntervalsAreChargedAsBefore runs both on the same random charges.

func legacySaveChargingToBill(rc RatingContext, bc BillingContext, bill *Bill, res *BillingResource, results []PricePlanRuleResult, clockNow time.Time) {
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
	legacyUpdateBillItems(rc, bc, res, results, item)
	now := clockNow
	item.UpdatedAt = &now
}

func legacyUpdateBillItems(rc RatingContext, bc BillingContext, res *BillingResource, results []PricePlanRuleResult, item *BillItem) {
	if res.NotEligibleForBilling {
		// A blind interval (stale cloud state) is written off instead of back-billed on recovery;
		// every other ineligible state (BUILD, …) keeps its watermark and is back-billed.
		if res.SkipInterval {
			legacyAdvanceWatermark(rc, item, res)
		}
		return
	}
	totalCap := getTotalCap(bc, rc.TimeUnit, results)
	if !(totalCap.Cmp(item.NetAmount) > 0) { // strict >
		return
	}
	diff := legacyGetTimeUnitsDiff(bc, rc, item, res)
	switch rc.TimeUnit {
	case TimeUnitMinute:
		*item.TimeUnits.Minute = diff + *item.TimeUnits.Minute
	case TimeUnitHour:
		*item.TimeUnits.Hour = diff + *item.TimeUnits.Hour
	case TimeUnitMonth:
		*item.TimeUnits.Month = diff + *item.TimeUnits.Month
	}
	legacyAdvanceWatermark(rc, item, res)
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

func legacyAdvanceWatermark(rc RatingContext, item *BillItem, res *BillingResource) {
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

func legacyGetTimeUnitsDiff(bc BillingContext, rc RatingContext, item *BillItem, res *BillingResource) int64 {
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

// A resource that sets none of the three fields gets the same bill item from the engine as from the
// copy above, after every charge of a random sequence: units, counters, watermarks, every applied
// amount and the net, compared as serialized decimals. The sequences mix the three time units,
// charges out of order and repeated, deletions off the unit boundary, resources without createdAt,
// not eligible with and without SkipInterval, zero and 18-decimal amounts, usage totals, a rule that
// comes and goes, and unit limits small enough to reach the limit and the cap.
func TestResourcesWithoutIntervalsAreChargedAsBefore(t *testing.T) {
	rng := rand.New(rand.NewSource(20261126))
	base := time.Date(2026, 10, 31, 20, 0, 0, 0, time.UTC)
	cycle := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	units := []string{TimeUnitMinute, TimeUnitHour, TimeUnitMonth}
	amounts := []string{"0", "0.0003", "5", "0.123456789012345678", "41"}
	methods := []string{"", ApplyAddToTotal, ApplyOverwriteTotal}
	secs := func(n int) time.Duration { return time.Duration(rng.Intn(n)) * time.Second }
	for seq := 0; seq < 2000; seq++ {
		newBill := func() *Bill {
			b := &Bill{InvoiceCurrency: "USD"}
			if seq%10 != 0 { // one in ten has no cycle: nil watermarks
				b.BillingCycle = &BillBillingCycle{StartDate: &cycle}
			}
			return b
		}
		was, is := newBill(), newBill()
		bc := BillingContext{}
		if seq%3 == 0 {
			bc.TimeUnitLimits = map[string]int{TimeUnitMinute: 1 + rng.Intn(90), TimeUnitHour: 1 + rng.Intn(4), TimeUnitMonth: 1}
		}
		var created [2]*time.Time
		for i := range created {
			if rng.Intn(8) != 0 {
				c := base.Add(secs(4*3600) - 2*time.Hour) // before and after the first charges
				if rng.Intn(6) == 0 {
					c = cycle.AddDate(0, 0, -3) // before the cycle
				}
				created[i] = &c
			}
		}
		for pass := 0; pass < 30; pass++ {
			unit := units[rng.Intn(len(units))]
			ct := base.Add(secs(5 * 3600))
			switch unit {
			case TimeUnitMinute:
				ct = ct.Truncate(time.Minute)
			case TimeUnitHour:
				ct = ct.Truncate(time.Hour)
			default:
				ct = ct.Truncate(24 * time.Hour)
			}
			i := rng.Intn(2)
			res := &BillingResource{ResourceID: []string{"a", "b"}[i], ResourceType: "compute", CreatedAt: created[i], Values: map[string]any{"display_name": "n"}}
			if rng.Intn(5) == 0 {
				d := base.Add(secs(5 * 3600))
				res.DeletedAt = &d
			}
			if rng.Intn(7) == 0 {
				res.NotEligibleForBilling, res.SkipInterval = true, rng.Intn(2) == 0
			}
			var results []PricePlanRuleResult
			for r := 0; r < rng.Intn(3); r++ {
				var amts []PricePlanRuleResultAmount
				for a := 0; a <= rng.Intn(2); a++ {
					amts = append(amts, amt([]string{"vcpus", "ram_gb"}[a], amounts[rng.Intn(len(amounts))]))
				}
				results = append(results, result([]string{"rule1", "rule2"}[r], methods[rng.Intn(len(methods))], amts...))
			}
			rc := RatingContext{TimeUnit: unit, CycleTimestamp: ct}
			legacySaveChargingToBill(rc, bc, was, res, results, ct)
			SaveChargingToBill(rc, bc, is, res, results, ct)
			a, err := json.Marshal(was)
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(is)
			if err != nil {
				t.Fatal(err)
			}
			if string(a) != string(b) {
				t.Fatalf("sequence %d, charge %d (%s at %s): the bill moved\n before: %s\n    now: %s", seq, pass, unit, ct.Format(time.RFC3339), a, b)
			}
		}
	}
}

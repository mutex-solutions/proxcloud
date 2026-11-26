package pricing

import (
	"testing"
	"time"
)

// A resource charged by its running intervals: ChargeFrom, VariableRate, EarlierInterval. What the
// provider of such a resource hands the engine (internal/cloud/billingresource): ChargeFrom = when
// the interval started, stamped once per interval; an ended interval's DeletedAt on a minute boundary
// (the start's minute plus the runtime rounded up); earlier intervals before the resource's own.

var intervalDay = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

// hms is a time of intervalDay.
func hms(h, m, s int) time.Time {
	return intervalDay.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
}

func perUnit(net string) []PricePlanRuleResult {
	return []PricePlanRuleResult{result("rule1", ApplyAddToTotal, amt("vcpus", net))}
}

// interval is resource "r", created at 10:00:00, running since `from`; ended at `end` when given.
func interval(from time.Time, end ...time.Time) *BillingResource {
	created := hms(10, 0, 0)
	res := billRes("r", nil)
	res.CreatedAt, res.ChargeFrom, res.VariableRate = &created, &from, true
	if len(end) == 1 {
		res.DeletedAt = &end[0]
	}
	return res
}

// intervalBill is a November bill and its charges.
type intervalBill struct {
	t    *testing.T
	bill *Bill
	bc   BillingContext
}

func newIntervalBill(t *testing.T) *intervalBill {
	cycle := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	return &intervalBill{t: t, bill: &Bill{InvoiceCurrency: "USD", BillingCycle: &BillBillingCycle{StartDate: &cycle}}}
}

func (b *intervalBill) charge(res *BillingResource, unit string, at time.Time, results []PricePlanRuleResult) {
	SaveChargingToBill(RatingContext{TimeUnit: unit, CycleTimestamp: at}, b.bc, b.bill, res, results, at)
}

func (b *intervalBill) minute(res *BillingResource, at time.Time) {
	b.charge(res, TimeUnitMinute, at, perUnit("5"))
}

// want checks the item's minutes, net and minute watermark.
func (b *intervalBill) want(when string, minutes int64, net string, mark time.Time) {
	b.t.Helper()
	it := onlyItem(b.t, b.bill)
	if *it.TimeUnits.Minute != minutes || !it.NetAmount.Equal(mustDec(net)) || it.TimeUnits.MinuteLastRateTime == nil || !it.TimeUnits.MinuteLastRateTime.Equal(mark) {
		b.t.Fatalf("%s: %d minutes, net %s, watermark %v; want %d, %s, %v", when, *it.TimeUnits.Minute, it.NetAmount, it.TimeUnits.MinuteLastRateTime, minutes, net, mark)
	}
}

// The watermark does not follow the clock while a resource is stopped, so every start after the
// first relies on ChargeFrom: the idle gap is not billed.
func TestChargeFromAfterTheWatermark(t *testing.T) {
	b := newIntervalBill(t)
	first := interval(hms(10, 0, 20))
	for m := 1; m <= 3; m++ {
		b.minute(first, hms(10, m, 0))
	}
	b.want("three charges while it runs", 3, "15", hms(10, 3, 0))
	b.minute(interval(hms(10, 0, 20), hms(10, 4, 0)), hms(10, 4, 0)) // stopped at 10:03:20: billed to 10:04
	b.want("its end", 4, "20", hms(10, 4, 0))

	woken := interval(hms(10, 30, 45))
	b.minute(woken, hms(10, 31, 0))
	b.want("a wake 26 minutes later is charged from its own minute", 5, "25", hms(10, 31, 0))
	b.minute(woken, hms(10, 32, 0))
	b.want("and goes on from the watermark", 6, "30", hms(10, 32, 0))

	// The rule is what does it: the same charges without ChargeFrom bill the idle time.
	plain := newIntervalBill(t)
	res := interval(hms(10, 0, 20))
	res.ChargeFrom = nil
	plain.minute(res, hms(10, 4, 0))
	plain.minute(res, hms(10, 31, 0))
	plain.want("without ChargeFrom", 31, "155", hms(10, 31, 0))
}

// ChargeFrom never moves the count before the watermark: a start older than it, or one that moved
// back, recharges nothing.
func TestChargeFromBeforeTheWatermark(t *testing.T) {
	b := newIntervalBill(t)
	b.minute(interval(hms(10, 0, 20)), hms(10, 10, 0))
	b.want("ten minutes", 10, "50", hms(10, 10, 0))
	b.minute(interval(hms(10, 0, 20)), hms(10, 11, 0))
	b.want("the watermark wins over an older start", 11, "55", hms(10, 11, 0))
	b.minute(interval(hms(9, 0, 0)), hms(10, 12, 0))
	b.want("a start that moved back", 12, "60", hms(10, 12, 0))
}

// An interval that began after the charge (a charge that ran late, or out of order) counts nothing;
// the watermark still moves to the charge, never past it.
func TestChargeFromAfterTheCharge(t *testing.T) {
	b := newIntervalBill(t)
	b.minute(interval(hms(10, 0, 20), hms(10, 2, 0)), hms(10, 5, 0))
	b.want("an ended interval", 2, "10", hms(10, 2, 0))
	late := interval(hms(10, 20, 30))
	b.minute(late, hms(10, 15, 0))
	b.want("the interval began after the charge", 2, "10", hms(10, 15, 0))
	b.minute(late, hms(10, 21, 0))
	b.want("the next charge counts from its start", 3, "15", hms(10, 21, 0))
}

func TestIntervalChargeTwiceAndOutOfOrder(t *testing.T) {
	b := newIntervalBill(t)
	res := interval(hms(10, 0, 20))
	b.minute(res, hms(10, 5, 0))
	b.minute(res, hms(10, 5, 0))
	b.want("the same charge twice", 5, "25", hms(10, 5, 0))
	b.minute(res, hms(10, 3, 0))
	b.want("an older charge after a newer one", 5, "25", hms(10, 5, 0))
	b.minute(res, hms(10, 6, 0))
	b.want("the next", 6, "30", hms(10, 6, 0))
}

// The source's TestEarlierInterval, with what THIS engine charges: started units are counted up and
// the watermark of an ended interval is its end, so the interval 12:00:30 to 12:03:30 on an item
// created 12:00:20 is 4 minutes (the source floors: 3) and the one running since 12:04:10 brings it
// to 5 at the 12:05 charge. With ends on minute boundaries, which is all a provider sends, both
// engines charge the same minutes. The source's last case (a resource without ChargeFrom is not
// floored) does not exist here: this engine floors every resource.
func TestEarlierInterval(t *testing.T) {
	rule := []PricePlanRuleResult{result("rule1", "", amt("vcpu", "5"))}
	cycle := billNow.Add(-24 * time.Hour)
	created := billNow.Add(20 * time.Second) // 12:00:20
	at := func(d time.Duration) *time.Time { ts := billNow.Add(d); return &ts }
	earlier := billRes("r", nil)
	earlier.CreatedAt, earlier.EarlierInterval = &created, true
	earlier.ChargeFrom, earlier.DeletedAt = at(30*time.Second), at(3*time.Minute+30*time.Second) // 12:00:30 to 12:03:30
	running := billRes("r", nil)
	running.CreatedAt, running.ChargeFrom = &created, at(4*time.Minute+10*time.Second) // since 12:04:10

	bill := &Bill{InvoiceCurrency: "USD", BillingCycle: &BillBillingCycle{StartDate: &cycle}}
	charge := func(res *BillingResource, unit string, d time.Duration) {
		SaveChargingToBill(RatingContext{TimeUnit: unit, CycleTimestamp: *at(d)}, BillingContext{}, bill, res, rule, *at(d))
	}
	for _, unit := range []string{TimeUnitHour, TimeUnitMonth} {
		if charge(earlier, unit, time.Hour); len(bill.Items) != 0 {
			t.Fatalf("an earlier interval on a %s charge: %+v, want nothing", unit, bill.Items)
		}
	}
	check := func(minutes int64, watermark time.Duration, when string) {
		t.Helper()
		it := onlyItem(t, bill)
		if *it.TimeUnits.Minute != minutes || !it.TimeUnits.MinuteLastRateTime.Equal(*at(watermark)) || *it.TimeUnits.Hour != 0 || *it.TimeUnits.Month != 0 {
			t.Fatalf("%s: %d minutes, watermark %v; want %d, %v", when, *it.TimeUnits.Minute, it.TimeUnits.MinuteLastRateTime, minutes, *at(watermark))
		}
	}
	charge(earlier, TimeUnitMinute, 5*time.Minute)
	check(4, 3*time.Minute+30*time.Second, "the interval that ended: 12:00:20 to 12:03:30, and the watermark is its end, not the charge")
	charge(running, TimeUnitMinute, 5*time.Minute)
	check(5, 5*time.Minute, "the one that runs, from its own start: 12:04:00 to 12:05:00")
	for _, d := range []time.Duration{5 * time.Minute, 2 * time.Minute, 6 * time.Minute} { // again, older, the next
		charge(earlier, TimeUnitMinute, d)
		charge(earlier, TimeUnitHour, d)
	}
	check(5, 5*time.Minute, "the interval that ended, on later charges")

	charge(running, TimeUnitMinute, 2*time.Minute)
	check(5, 5*time.Minute, "a charge older than the watermark")
	running.ChargeFrom = at(9*time.Minute + 30*time.Second)
	charge(running, TimeUnitMinute, 7*time.Minute)
	check(5, 7*time.Minute, "an interval that began two minutes after the charge")
	charge(running, TimeUnitMinute, 10*time.Minute)
	check(6, 10*time.Minute, "and its first minute")
}

// An earlier interval is the same resource with a start and an end, so feeding it on every charge
// for as long as the row carries it charges it once.
func TestEarlierIntervalFedManyTimes(t *testing.T) {
	b := newIntervalBill(t)
	earlier := interval(hms(10, 0, 20), hms(10, 4, 0))
	earlier.EarlierInterval = true
	running := interval(hms(10, 6, 10))
	for m := 7; m < 17; m++ {
		b.minute(earlier, hms(10, m, 0))
		b.minute(running, hms(10, m, 0))
	}
	b.want("4 minutes of the first interval, 10 of the second", 14, "70", hms(10, 16, 0))
}

// Order: earlier intervals come before the resource's own. Fed after it, the earlier one's time is
// behind the watermark and is lost: a wrong order charges less, never more.
func TestEarlierIntervalFedAfterTheRunningOne(t *testing.T) {
	b := newIntervalBill(t)
	earlier := interval(hms(10, 0, 20), hms(10, 4, 0))
	earlier.EarlierInterval = true
	b.minute(interval(hms(10, 6, 10)), hms(10, 8, 0))
	b.minute(earlier, hms(10, 8, 0))
	b.want("the wrong order", 2, "10", hms(10, 8, 0)) // the right order is 4 + 2
}

// An earlier interval is rated on minute charges only, and only when both its ends are known.
func TestEarlierIntervalOnlyClosedAndOnlyOnMinuteCharges(t *testing.T) {
	closed := interval(hms(10, 0, 20), hms(10, 4, 0))
	closed.EarlierInterval = true
	noEnd := interval(hms(10, 0, 20))
	noEnd.EarlierInterval = true
	noStart := interval(hms(10, 0, 20), hms(10, 4, 0))
	noStart.EarlierInterval, noStart.ChargeFrom = true, nil
	for _, tc := range []struct {
		name string
		res  *BillingResource
		unit string
	}{
		{"hour charge", closed, TimeUnitHour}, {"month charge", closed, TimeUnitMonth},
		{"no end", noEnd, TimeUnitMinute}, {"no start", noStart, TimeUnitMinute},
	} {
		b := newIntervalBill(t)
		b.charge(tc.res, tc.unit, hms(11, 0, 0), perUnit("5"))
		if len(b.bill.Items) != 0 {
			t.Errorf("%s: %+v, want no item", tc.name, b.bill.Items)
		}
	}
	// Why not on an hour charge: its end is not on an hour boundary, and would leave the hour
	// watermark inside the hour for the running interval to count again. Two clock hours are two.
	b := newIntervalBill(t)
	running := interval(hms(11, 2, 0))
	b.charge(closed, TimeUnitHour, hms(12, 0, 0), perUnit("5"))
	b.charge(running, TimeUnitHour, hms(12, 0, 0), perUnit("5"))
	if it := onlyItem(t, b.bill); *it.TimeUnits.Hour != 1 {
		t.Errorf("an hour rule over 11:02 to 12:00: %d hours, want 1", *it.TimeUnits.Hour)
	}
}

// A minute two intervals share is charged once, at the first interval's size.
func TestTwoIntervalsShareAMinute(t *testing.T) {
	b := newIntervalBill(t)
	first := interval(hms(10, 0, 30), hms(10, 4, 0)) // two instances, stopped 10:03:50
	first.EarlierInterval = true
	second := interval(hms(10, 3, 55)) // one instance, started in the same minute
	b.charge(first, TimeUnitMinute, hms(10, 6, 0), perUnit("10"))
	b.charge(second, TimeUnitMinute, hms(10, 6, 0), perUnit("5"))
	b.want("10:00 to 10:04 at 10, 10:04 to 10:06 at 5", 6, "50", hms(10, 6, 0))
}

// K20: a start that has not served yet is fed NOT eligible, without SkipInterval, so the watermark
// stays. Once it serves it is billed back from the start of that interval: not from the end of the
// interval before it, and not from the row's creation.
func TestNotEligibleIntervalIsBilledBackFromItsStart(t *testing.T) {
	b := newIntervalBill(t)
	b.minute(interval(hms(10, 0, 10), hms(10, 3, 0)), hms(10, 3, 0))
	b.want("an interval that served", 3, "15", hms(10, 3, 0))

	starting := interval(hms(10, 20, 15))
	starting.NotEligibleForBilling = true
	for m := 21; m <= 23; m++ {
		b.minute(starting, hms(10, m, 0))
	}
	b.want("three charges while it has not served: nothing, and the watermark stays", 3, "15", hms(10, 3, 0))
	b.minute(interval(hms(10, 20, 15)), hms(10, 24, 0))
	b.want("served: 10:20 to 10:24, not the 17 idle minutes before", 7, "35", hms(10, 24, 0))

	// A start that never served ends without having been eligible: its resource is no longer fed, and
	// the next interval is charged from its own start.
	b.minute(interval(hms(10, 40, 5)), hms(10, 41, 0))
	b.want("the next interval", 8, "40", hms(10, 41, 0))
}

// A stale charge (the provider's state could not be observed) writes its time off for a running
// interval as for any resource.
func TestStaleChargeInsideAnInterval(t *testing.T) {
	b := newIntervalBill(t)
	res := interval(hms(10, 0, 20))
	b.minute(res, hms(10, 2, 0))
	blind := interval(hms(10, 0, 20))
	blind.NotEligibleForBilling, blind.SkipInterval = true, true
	b.minute(blind, hms(10, 10, 0))
	b.want("the stale charge", 2, "10", hms(10, 10, 0))
	b.minute(res, hms(10, 12, 0))
	b.want("after it", 4, "20", hms(10, 12, 0))
}

// A rate that falls leaves the item above a month of the new rate. A fixed-rate resource is frozen
// there, and its watermark with it; a variable-rate one is charged on, up to the unit limit.
func TestVariableRateIsNotFrozenByTheCap(t *testing.T) {
	limits := BillingContext{TimeUnitLimits: map[string]int{TimeUnitMinute: 100}}
	for _, variable := range []bool{false, true} {
		b := newIntervalBill(t)
		b.bc = limits
		res := interval(hms(10, 0, 0))
		res.VariableRate = variable
		for m := 1; m <= 20; m++ {
			b.charge(res, TimeUnitMinute, hms(10, m, 0), perUnit("40")) // 40 instances
		}
		b.want("20 minutes at 40", 20, "800", hms(10, 20, 0))
		b.charge(res, TimeUnitMinute, hms(10, 21, 0), perUnit("1")) // scaled down: a month of this rate is 100
		if !variable {
			b.want("a fixed rate: frozen", 20, "800", hms(10, 20, 0))
			continue
		}
		b.want("a variable rate: the next minute is charged", 21, "801", hms(10, 21, 0))
		b.charge(res, TimeUnitMinute, hms(10, 22, 0), nil) // a charge that rates to nothing is still skipped
		b.want("no rule", 21, "801", hms(10, 21, 0))
		for m := 22; m <= 150; m++ {
			b.charge(res, TimeUnitMinute, hms(10, m, 0), perUnit("1"))
		}
		b.want("the unit limit still holds", 100, "880", hms(12, 30, 0))
	}
}

// The month charge runs every hour and counts one unit each time; only the cap makes a month rule
// charge once. So VariableRate does not lift it there.
func TestVariableRateKeepsTheCapOnMonthCharges(t *testing.T) {
	b := newIntervalBill(t)
	res := interval(hms(0, 0, 20))
	for h := 0; h < 24; h++ {
		b.charge(res, TimeUnitMonth, intervalDay, perUnit("5"))
	}
	if it := onlyItem(t, b.bill); *it.TimeUnits.Month != 1 || !it.NetAmount.Equal(mustDec("5")) {
		t.Fatalf("24 month charges: %d months, net %s; want 1, 5", *it.TimeUnits.Month, it.NetAmount)
	}
}

// The charge at 00:00 on the 1st closes the old month's bill; the new month's bill starts a new item
// with its watermark at the cycle start. A running interval is split there. An interval that starts
// later in the new month is charged from its own start, not from the 1st.
func TestRunningIntervalAcrossTheMonthClose(t *testing.T) {
	october, november := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	created := october.AddDate(0, 0, 9)
	from := november.Add(-9*time.Minute - 30*time.Second) // 31 October 23:50:30
	res := billRes("r", nil)
	res.CreatedAt, res.ChargeFrom, res.VariableRate = &created, &from, true

	old := &intervalBill{t: t, bill: &Bill{InvoiceCurrency: "USD", BillingCycle: &BillBillingCycle{StartDate: &october}}}
	old.minute(res, november.Add(-5*time.Minute))
	old.minute(res, november) // the closing charge
	old.want("October", 10, "50", november)

	next := &intervalBill{t: t, bill: &Bill{InvoiceCurrency: "USD", BillingCycle: &BillBillingCycle{StartDate: &november}}}
	next.minute(res, november.Add(time.Minute))
	next.want("November's first charge", 1, "5", november.Add(time.Minute))

	woken := intervalDay.Add(10*time.Hour + 20*time.Second) // idle since the 1st, started on the 10th
	later := &intervalBill{t: t, bill: &Bill{InvoiceCurrency: "USD", BillingCycle: &BillBillingCycle{StartDate: &november}}}
	res2 := *res
	res2.ChargeFrom = &woken
	later.minute(&res2, hms(10, 1, 0))
	later.want("a first start on the 10th", 1, "5", hms(10, 1, 0))
}

// Without a watermark (no createdAt, or a bill without a cycle) the count is 1 whatever ChargeFrom
// says. A provider of running intervals always has both; this pins what happens if one does not.
func TestChargeFromWithoutAWatermark(t *testing.T) {
	bill := &Bill{InvoiceCurrency: "USD"}
	from := hms(9, 0, 0)
	res := billRes("r", nil)
	res.ChargeFrom = &from
	SaveChargingToBill(RatingContext{TimeUnit: TimeUnitMinute, CycleTimestamp: hms(10, 0, 0)}, BillingContext{}, bill, res, perUnit("5"), hms(10, 0, 0))
	if it := onlyItem(t, bill); *it.TimeUnits.Minute != 1 {
		t.Fatalf("%d minutes, want 1", *it.TimeUnits.Minute)
	}
}

// The projection follows the charge: a variable-rate resource is projected past the cap on a minute
// or hour rule, and not on a month rule.
func TestProjectedAccrualVariableRate(t *testing.T) {
	limits := BillingContext{TimeUnitLimits: map[string]int{TimeUnitMinute: 100}}
	item := &BillItem{NetAmount: mustDec("800"), TimeUnits: &BillItemTimeUnits{Minute: ptr64(20), Hour: ptr64(0), Month: ptr64(0)}}
	for _, tc := range []struct {
		unit     string
		variable bool
		want     string
	}{
		{TimeUnitMinute, false, "0"}, {TimeUnitMinute, true, "30"},
		{TimeUnitMonth, false, "0"}, {TimeUnitMonth, true, "0"},
	} {
		add, _ := ProjectedAccrual(limits, tc.unit, perUnit("1"), item, item.NetAmount, 30, tc.variable)
		if !add.Equal(mustDec(tc.want)) {
			t.Errorf("%s, variable %v: %s, want %s", tc.unit, tc.variable, add, tc.want)
		}
	}
	if add, _ := ProjectedAccrual(limits, TimeUnitMinute, perUnit("1"), item, item.NetAmount, 500, true); !add.Equal(mustDec("80")) {
		t.Errorf("past the unit limit: %s, want 80", add)
	}
	// A charge that rates to nothing or less is skipped for every resource, so nothing is projected.
	if add, _ := ProjectedAccrual(limits, TimeUnitMinute, perUnit("-1"), item, item.NetAmount, 30, true); !add.IsZero() {
		t.Errorf("a negative rate: %s, want 0", add)
	}
}

// ChargeFrom is cut to the charge's unit, because a started unit is a billed unit at this end too:
// an interval from 11:50 to 12:10 under an hour rule existed in two clock hours.
func TestChargeFromIsCutToTheUnit(t *testing.T) {
	b := newIntervalBill(t)
	b.charge(interval(hms(11, 50, 0), hms(12, 10, 0)), TimeUnitHour, hms(13, 0, 0), perUnit("5"))
	if it := onlyItem(t, b.bill); *it.TimeUnits.Hour != 2 {
		t.Fatalf("%d hours, want 2 (11 and 12)", *it.TimeUnits.Hour)
	}
}

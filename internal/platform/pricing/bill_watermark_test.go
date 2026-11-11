package pricing

import (
	"testing"
	"time"
)

// Older queued cycles may run after a newer one; they must not rewind a watermark
// and make the next new cycle charge that interval for a second time.
func TestWatermarksNeverRewind(t *testing.T) {
	start := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for _, unit := range []string{TimeUnitMinute, TimeUnitHour} {
		d := time.Hour
		if unit == TimeUnitMinute {
			d = time.Minute
		}
		cycle := start.AddDate(0, 0, -4)
		bill := &Bill{InvoiceCurrency: "USD", BillingCycle: &BillBillingCycle{StartDate: &cycle}}
		res := billRes("r", nil)
		res.CreatedAt = &start
		rules := []PricePlanRuleResult{result("rule1", ApplyAddToTotal, amt("vcpu", "5"))}
		for _, n := range []int{2, 1, 3} {
			ct := start.Add(time.Duration(n) * d)
			SaveChargingToBill(RatingContext{TimeUnit: unit, CycleTimestamp: ct}, BillingContext{}, bill, res, rules, ct)
		}
		it := onlyItem(t, bill)
		if !it.NetAmount.Equal(mustDec("15")) {
			t.Fatalf("%s out-of-order net=%s want15", unit, it.NetAmount)
		}
	}
}

// A row archived after a cycle boundary but before that cycle's charge ran (deleted 13:00:10, the 13:00 charge runs at
// 13:00:30) is rated up to its deletion, so the next charge must not rate the 13:00 hour again: the hours it existed
// in are 10, 11, 12 and 13.
func TestDeletedPastTheCycleIsBilledOnce(t *testing.T) {
	day := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	at := func(h, m, s int) time.Time {
		return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
	}
	cycle := day.AddDate(0, 0, -4)
	bill := &Bill{InvoiceCurrency: "USD", BillingCycle: &BillBillingCycle{StartDate: &cycle}}
	rule := []PricePlanRuleResult{result("rule1", ApplyAddToTotal, amt("vcpu", "5"))}
	res := billRes("r", nil)
	created := at(10, 20, 0)
	res.CreatedAt = &created
	charge := func(ct time.Time, r *BillingResource) {
		SaveChargingToBill(RatingContext{TimeUnit: TimeUnitHour, CycleTimestamp: ct}, BillingContext{}, bill, r, rule, ct)
	}
	charge(at(11, 0, 0), res)
	charge(at(12, 0, 0), res)
	archived := *res
	deleted := at(13, 0, 10)
	archived.DeletedAt = &deleted
	for h := 13; h <= 15; h++ {
		charge(at(h, 0, 0), &archived)
	}
	if it := onlyItem(t, bill); *it.TimeUnits.Hour != 4 || !it.NetAmount.Equal(mustDec("20")) {
		t.Errorf("hour=%d net=%s, want 4 / 20", *it.TimeUnits.Hour, it.NetAmount)
	}
}

// A sandbox's minutes (internal/cloud/billingresource/sandbox.go hands the engine a start and an end on minute
// boundaries): 39 s run in the 18:48 minute is one minute, 4m50s from 18:44 is five, whichever charges see them.
func TestSandboxMinutesBilledOnce(t *testing.T) {
	day := time.Date(2026, 11, 7, 18, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return day.Add(time.Duration(m) * time.Minute) }
	cycle := day.AddDate(0, 0, -6)
	rule := []PricePlanRuleResult{result("rule1", ApplyAddToTotal, amt("vcpus", "1"))}
	for _, tc := range []struct {
		name              string
		start, end        time.Time
		running, archived []time.Time // charges while the row runs, then after it ended
		want              int64
	}{
		{"short, ended before the first charge", at(48), at(49), nil, []time.Time{at(48), at(50)}, 1},
		{"rated while running, then ended", at(44), at(49), []time.Time{at(48)}, []time.Time{at(50), at(51)}, 5},
	} {
		bill := &Bill{InvoiceCurrency: "USD", BillingCycle: &BillBillingCycle{StartDate: &cycle}}
		res := billRes("r", nil)
		res.CreatedAt = &tc.start
		for _, ct := range tc.running {
			SaveChargingToBill(RatingContext{TimeUnit: TimeUnitMinute, CycleTimestamp: ct}, BillingContext{}, bill, res, rule, ct)
		}
		ended := *res
		ended.DeletedAt = &tc.end
		for _, ct := range tc.archived {
			SaveChargingToBill(RatingContext{TimeUnit: TimeUnitMinute, CycleTimestamp: ct}, BillingContext{}, bill, &ended, rule, ct)
		}
		if it := onlyItem(t, bill); *it.TimeUnits.Minute != tc.want {
			t.Errorf("%s: %d minutes, want %d", tc.name, *it.TimeUnits.Minute, tc.want)
		}
	}
}

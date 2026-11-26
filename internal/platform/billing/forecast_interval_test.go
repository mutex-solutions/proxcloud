package billing

import (
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// The projection counts from where the next charge will: a resource charged by its running intervals
// (ChargeFrom) from the start of its interval, cut to the unit, when that is after the watermark.
func TestUnitsLeftFromTheStartOfTheRunningInterval(t *testing.T) {
	monthStart := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 11, 15, 12, 30, 0, 0, time.UTC)
	end := time.Date(2026, 11, 15, 18, 0, 0, 0, time.UTC)
	mark := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC) // charged through 08:00, then stopped
	item := &pricing.BillItem{TimeUnits: &pricing.BillItemTimeUnits{MinuteLastRateTime: &mark, HourLastRateTime: &mark}}
	woke := time.Date(2026, 11, 15, 12, 10, 40, 0, time.UTC)
	old := time.Date(2026, 11, 15, 6, 0, 0, 0, time.UTC)
	created := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		unit       string
		item       *pricing.BillItem
		chargeFrom *time.Time
		want       int64
	}{
		{"no ChargeFrom: from the watermark", pricing.TimeUnitHour, item, nil, 10},
		{"hours from the hour it started in", pricing.TimeUnitHour, item, &woke, 6},
		{"minutes from the minute it started in", pricing.TimeUnitMinute, item, &woke, 350},
		{"a start before the watermark changes nothing", pricing.TimeUnitHour, item, &old, 10},
		{"no item yet: not from the month start", pricing.TimeUnitHour, nil, &woke, 6},
		{"a month rule is one unit either way", pricing.TimeUnitMonth, item, &woke, 1},
	} {
		if got := unitsLeft(tc.unit, tc.item, &created, tc.chargeFrom, monthStart, now, end); got != tc.want {
			t.Errorf("%s: %d units, want %d", tc.name, got, tc.want)
		}
	}
	// The cut to the unit shows when the end is inside a unit (a resource that ends at 17:05): the
	// charge counts the clock hours 12 to 17, six of them; from 12:10:40 uncut it would be five.
	if got := unitsLeft(pricing.TimeUnitHour, item, &created, &woke, monthStart, now, time.Date(2026, 11, 15, 17, 5, 0, 0, time.UTC)); got != 6 {
		t.Errorf("an end inside the hour: %d units, want 6", got)
	}
}

// ProjectMonthEnd hands both fields on: the idle time since the watermark is not projected, and a
// variable-rate resource above a month of its current rate is.
func TestForecast_RunningIntervalResource(t *testing.T) {
	now := time.Date(2026, 11, 30, 20, 0, 0, 0, time.UTC) // four hours to the month end
	mark := now.Add(-48 * time.Hour)
	woke := now.Add(-20 * time.Minute)
	bill := openBill(chargedHourly("p", "f", "function", "9000", 3, mark)) // far above 720 × 0.01
	res := ForecastResource{ResourceID: "f", ResourceType: "function", ProjectID: "p", Rates: []ForecastRate{hourRate("r", "0.01")}}
	if f := ProjectMonthEnd([]pricing.Bill{bill}, ForecastInput{Resources: []ForecastResource{res}}, now); !f.Projected.IsZero() {
		t.Fatalf("a fixed-rate item above its cap: projected %s, want 0", f.Projected)
	}
	res.ChargeFrom, res.VariableRate = &woke, true
	f := ProjectMonthEnd([]pricing.Bill{bill}, ForecastInput{Resources: []ForecastResource{res}}, now)
	decEq(t, "five hours from 19:00, not the 52 since the watermark", f.Projected, "0.05")
}

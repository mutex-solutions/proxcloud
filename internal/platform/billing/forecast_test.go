package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// The month-end forecast tests run on a fixed clock. Rates are rated amounts as the charge job's
// Engine returns them (a PricePlanRuleResult per matching rule); the forecast multiplies them out
// exactly as the hourly accrual does, so every expectation below is hand-computable.

var (
	fcMonthStart = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	fcMonthEnd   = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
)

func tptr(t time.Time) *time.Time { return &t }
func i64(v int64) *int64          { return &v }

// hourRate is a resource's hourly rating: one ADD_TO_TOTAL rule pricing `amount` per hour.
func hourRate(ruleID, amount string) ForecastRate {
	return ForecastRate{TimeUnit: pricing.TimeUnitHour, Results: []pricing.PricePlanRuleResult{{
		PricePlanRule: pricing.PricePlanRule{ID: ruleID, TimeUnit: pricing.TimeUnitHour},
		Amounts:       []pricing.PricePlanRuleResultAmount{{AttributeName: "vcpus", NetAmount: mustDec(amount)}},
	}}}
}

// openBill is the month's OPEN bill with the given items.
func openBill(items ...pricing.BillItem) pricing.Bill {
	s, e := fcMonthStart, fcMonthEnd
	return pricing.Bill{ID: "b1", Status: pricing.BillStatusOpen, BillingCycle: &pricing.BillBillingCycle{StartDate: &s, EndDate: &e}, Items: items}
}

// chargedHourly is a bill item the hourly pass has charged `hours` hours, the last at `last`.
func chargedHourly(project, id, typ, net string, hours int64, last time.Time) pricing.BillItem {
	return pricing.BillItem{
		ProjectID: project, ResourceID: id, ResourceType: typ, Name: id, NetAmount: mustDec(net),
		TimeUnits: &pricing.BillItemTimeUnits{Minute: i64(0), Hour: i64(hours), Month: i64(0), HourLastRateTime: &last},
	}
}

func decEq(t *testing.T, label string, got decimal.Decimal, want string) {
	t.Helper()
	if !got.Equal(mustDec(want)) {
		t.Errorf("%s = %s, want %s", label, got, want)
	}
}

// Mid-month, right after the hourly pass: forecast = current + rate × whole hours left in the month.
func TestForecast_MidMonthRunningVM(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC) // the 12:00 pass has run
	// Running since the 1st: 14 days × 24 + 12 = 348 hours charged at 0.05 = 17.40.
	bills := []pricing.Bill{openBill(chargedHourly("p1", "vm1", "instance", "17.40", 348, now))}
	in := ForecastInput{Resources: []ForecastResource{{
		ResourceID: "vm1", ResourceType: "instance", ProjectID: "p1",
		Rates: []ForecastRate{hourRate("r-cpu", "0.05")},
	}}}
	f := ProjectMonthEnd(bills, in, now)

	hoursLeft := int64(fcMonthEnd.Sub(now) / time.Hour) // 15 days 12 h = 372
	if hoursLeft != 372 {
		t.Fatalf("fixture: hours left = %d", hoursLeft)
	}
	decEq(t, "projected", f.Projected, "18.60") // 0.05 × 372
	ci := costInfoFor(bills, now)
	ApplyForecast(ci, f, "")
	numEq(t, "forecastedMonthEndCosts", ci["forecastedMonthEndCosts"], "36.00") // 17.40 + 18.60
	numEq(t, "forecast Compute", ci["forecastedMonthEndCostsByType"].(map[string]any)["Compute"], "36.00")
	if ci["forecastApproximate"] != false {
		t.Errorf("a fully rated forecast is not approximate: %v", ci["forecastApproximate"])
	}
	top := ci["topResourcePrices"].([]any)[0].(map[string]any)
	numEq(t, "top forecastedCost", top["forecastedCost"], "36.00")
	numEq(t, "top currentCost", top["currentCost"], "17.40")
	// current + projected over the whole month = 720 hours × 0.05, exactly what the charge job bills.
	numEq(t, "full month", ci["forecastedMonthEndCosts"], mustDec("0.05").Mul(decimal.NewFromInt(720)).String())
}

// Between two hourly passes the hour in progress is still to bill: the next pass charges it.
func TestForecast_BetweenPassesCountsTheHourInProgress(t *testing.T) {
	last := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	now := last.Add(40 * time.Minute)
	bills := []pricing.Bill{openBill(chargedHourly("p1", "vm1", "instance", "17.40", 348, last))}
	in := ForecastInput{Resources: []ForecastResource{{ResourceID: "vm1", ResourceType: "instance", ProjectID: "p1",
		Rates: []ForecastRate{hourRate("r", "0.05")}}}}
	decEq(t, "projected", ProjectMonthEnd(bills, in, now).Projected, "18.60") // still 372 hours from the 12:00 watermark
}

// End of month: once the month's charges are complete nothing is left, forecast == current; in the
// last hour, only that hour is.
func TestForecast_EndOfMonth(t *testing.T) {
	// Every hour of November charged (720, the watermark at the month end, as the closing pass on
	// the 1st at 00:00 leaves it): read at the last instant of the month, nothing is left.
	now := fcMonthEnd.Add(-time.Nanosecond)
	bills := []pricing.Bill{openBill(chargedHourly("p1", "vm1", "instance", "36.00", 720, fcMonthEnd))}
	in := ForecastInput{Resources: []ForecastResource{{ResourceID: "vm1", ResourceType: "instance", ProjectID: "p1",
		Rates: []ForecastRate{hourRate("r", "0.05")}}}}
	f := ProjectMonthEnd(bills, in, now)
	decEq(t, "projected at month end", f.Projected, "0")
	ci := costInfoFor(bills, now)
	ApplyForecast(ci, f, "")
	numEq(t, "forecast == current", ci["forecastedMonthEndCosts"], "36.00")

	// In the last hour, before that pass: one hour is left.
	now = time.Date(2026, 11, 30, 23, 30, 0, 0, time.UTC)
	bills = []pricing.Bill{openBill(chargedHourly("p1", "vm1", "instance", "35.95", 719, time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC)))}
	decEq(t, "last hour", ProjectMonthEnd(bills, in, now).Projected, "0.05")
}

// Nothing running: forecast == current, and it is a real projection (not approximate).
func TestForecast_NoRunningResources(t *testing.T) {
	now := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	bills := []pricing.Bill{openBill(chargedHourly("p1", "gone", "instance", "4.00", 80, time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)))}
	f := ProjectMonthEnd(bills, ForecastInput{}, now)
	ci := costInfoFor(bills, now)
	ApplyForecast(ci, f, "")
	numEq(t, "forecast == current", ci["forecastedMonthEndCosts"], "4.00")
	numEq(t, "byType == current", ci["forecastedMonthEndCostsByType"].(map[string]any)["Compute"], "4.00")
	if ci["forecastApproximate"] != false {
		t.Errorf("no running resources is an exact forecast, got approximate")
	}
}

// Mixed types: per-category forecasts add up to the total, and each project gets its own slice.
func TestForecast_MixedTypesConsistentByType(t *testing.T) {
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC) // 264 hours left
	bills := []pricing.Bill{openBill(
		chargedHourly("p1", "vm1", "instance", "11.40", 456, now), // two hour rules: 0.025 × 456
		chargedHourly("p1", "vol1", "volume", "0.912", 456, now),
		chargedHourly("p2", "ip1", "floating_ip", "1.824", 456, now),
		// Traffic is a usage total: held at its month-to-date value.
		pricing.BillItem{ProjectID: "p1", ResourceID: "instance_traffic-vm1", ResourceType: "instance_traffic", Name: "vm1", NetAmount: mustDec("0.30")},
	)}
	traffic := ForecastRate{TimeUnit: pricing.TimeUnitHour, Results: []pricing.PricePlanRuleResult{{
		PricePlanRule: pricing.PricePlanRule{ID: "r-traffic", TimeUnit: pricing.TimeUnitHour, ApplyMethod: pricing.ApplyOverwriteTotal},
		Amounts:       []pricing.PricePlanRuleResultAmount{{AttributeName: "total_traffic_mb", NetAmount: mustDec("0.30")}},
	}}}
	in := ForecastInput{Resources: []ForecastResource{
		{ResourceID: "vm1", ResourceType: "instance", ProjectID: "p1", Rates: []ForecastRate{hourRate("r-cpu", "0.02"), hourRate("r-ram", "0.005")}},
		{ResourceID: "instance_traffic-vm1", ResourceType: "instance_traffic", ProjectID: "p1", Rates: []ForecastRate{traffic}},
		{ResourceID: "vol1", ResourceType: "volume", ProjectID: "p1", Rates: []ForecastRate{hourRate("r-vol", "0.002")}},
		{ResourceID: "ip1", ResourceType: "floating_ip", ProjectID: "p2", Rates: []ForecastRate{hourRate("r-ip", "0.004")}},
	}}
	f := ProjectMonthEnd(bills, in, now)
	// vm1 0.025×264 = 6.60 · vol1 0.002×264 = 0.528 · ip1 0.004×264 = 1.056 · traffic held = 0
	decEq(t, "projected", f.Projected, "8.184")
	decEq(t, "Compute", f.ByCategory["Compute"], "6.60")
	decEq(t, "Block Storage", f.ByCategory["Block Storage"], "0.528")
	decEq(t, "Networking", f.ByCategory["Networking"], "1.056")
	decEq(t, "p1", f.ByProject["p1"], "7.128")
	decEq(t, "p2", f.ByProject["p2"], "1.056")

	ci := costInfoFor(bills, now)
	ApplyForecast(ci, f, "")
	sum := decimal.Zero
	for _, v := range ci["forecastedMonthEndCostsByType"].(map[string]any) {
		sum = sum.Add(numberOf(v))
	}
	if total := numberOf(ci["forecastedMonthEndCosts"]); !sum.Equal(total) {
		t.Errorf("Σ forecastedMonthEndCostsByType = %s, forecastedMonthEndCosts = %s", sum, total)
	}
	numEq(t, "total", ci["forecastedMonthEndCosts"], "22.62") // 14.436 current + 8.184
	if ci["forecastApproximate"] != true {
		t.Errorf("a held usage total makes the forecast approximate")
	}
	if got := ci["forecastApproximateReasons"].([]string); len(got) != 1 || got[0] != ForecastUsage {
		t.Errorf("reasons = %v, want [usage]", got)
	}

	projects := ProjectCostInfoMap(bills, now, nil)
	ApplyForecastToProjects(projects, f)
	p2 := projects["p2"].(map[string]any)
	numEq(t, "p2 forecast", p2["forecastedMonthEndCosts"], "2.88") // 1.824 + 1.056
	if p2["forecastApproximate"] != false {
		t.Errorf("p2 has no usage total: not approximate")
	}
	if projects["p1"].(map[string]any)["forecastApproximate"] != true {
		t.Errorf("p1 holds the traffic total: approximate")
	}
}

// The month is the billing cycle, UTC: a viewer east of UTC is already in December at 05:00 local on
// Dec 1, but the November bill is the one forecast until 00:00 UTC, and only up to it.
func TestForecast_TimezoneBoundaryIsUTC(t *testing.T) {
	saigon := time.FixedZone("ICT", 7*3600)
	// 2026-12-01 05:00 +07:00 = 2026-11-30 22:00 UTC: two hours (22, 23) left in November.
	now := time.Date(2026, 12, 1, 5, 0, 0, 0, saigon)
	lastPass := time.Date(2026, 11, 30, 22, 0, 0, 0, time.UTC)
	bills := []pricing.Bill{openBill(chargedHourly("p1", "vm1", "instance", "35.90", 718, lastPass))}
	in := ForecastInput{Resources: []ForecastResource{{ResourceID: "vm1", ResourceType: "instance", ProjectID: "p1",
		Rates: []ForecastRate{hourRate("r", "0.05")}}}}
	f := ProjectMonthEnd(bills, in, now)
	decEq(t, "projected", f.Projected, "0.10")
	ci := costInfoFor(bills, now)
	ApplyForecast(ci, f, "")
	numEq(t, "current is November's", ci["currentMonthCosts"], "35.90")
	numEq(t, "forecast", ci["forecastedMonthEndCosts"], "36.00")

	// Just past 00:00 UTC (07:00 local) it is December: November's bill is no longer current. December
	// has 744 hours, but an hour rule stops at the 720-unit monthly limit, as the charge does.
	now = time.Date(2026, 12, 1, 7, 0, 1, 0, saigon)
	f = ProjectMonthEnd(bills, in, now)
	decEq(t, "December, capped at the hour limit", f.Projected, mustDec("0.05").Mul(decimal.NewFromInt(720)).String())
	ci = costInfoFor(bills, now)
	ApplyForecast(ci, f, "")
	numEq(t, "December so far", ci["currentMonthCosts"], "0")
}

// A resource with no bill item yet (created after the last pass) starts at its creation, and one
// that ends before the month does (a sandbox with a timeout) stops there.
func TestForecast_NewResourceAndPlannedEnd(t *testing.T) {
	now := time.Date(2026, 11, 30, 18, 0, 0, 0, time.UTC) // 6 hours left
	created := time.Date(2026, 11, 30, 17, 20, 0, 0, time.UTC)
	ends := time.Date(2026, 11, 30, 20, 30, 0, 0, time.UTC)
	in := ForecastInput{Resources: []ForecastResource{
		// Not charged yet: 17:20 → month end = 6 h 40 min → 7 started hours.
		{ResourceID: "vm-new", ResourceType: "instance", ProjectID: "p1", CreatedAt: &created, Rates: []ForecastRate{hourRate("r", "0.10")}},
		// Minute-rated sandbox, charged up to 18:00, ending at 20:30: 150 minutes.
		{ResourceID: "sb", ResourceType: "sandbox", ProjectID: "p1", EndsAt: &ends, Rates: []ForecastRate{{
			TimeUnit: pricing.TimeUnitMinute,
			Results: []pricing.PricePlanRuleResult{{PricePlanRule: pricing.PricePlanRule{ID: "r-sb", TimeUnit: pricing.TimeUnitMinute},
				Amounts: []pricing.PricePlanRuleResultAmount{{AttributeName: "vcpus", NetAmount: mustDec("0.001")}}}},
		}}},
	}}
	sb := pricing.BillItem{ProjectID: "p1", ResourceID: "sb", ResourceType: "sandbox", NetAmount: mustDec("0.06"),
		TimeUnits: &pricing.BillItemTimeUnits{Minute: i64(60), Hour: i64(0), Month: i64(0), MinuteLastRateTime: &now}}
	f := ProjectMonthEnd([]pricing.Bill{openBill(sb)}, in, now)
	decEq(t, "new vm", f.ByResource["vm-new"], "0.70")
	decEq(t, "sandbox until its end", f.ByResource["sb"], "0.15")
	// The new VM has no bill line yet; its project still gets a projection.
	projects := ProjectCostInfoMap([]pricing.Bill{openBill(sb)}, now, nil)
	f.ByProject["p-only-new"] = mustDec("1")
	ApplyForecastToProjects(projects, f)
	if _, ok := projects["p-only-new"]; !ok {
		t.Errorf("a project with a projection but no bill line yet must be listed")
	}
}

// The forecast stops where the charge does: the monthly unit limit and the item cap.
func TestForecast_RespectsTimeUnitLimitAndCap(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC) // 372 hours left
	// Hour limit 400 (configured): 348 charged, only 52 more may be.
	bc := pricing.BillingContext{TimeUnitLimits: map[string]int{pricing.TimeUnitHour: 400}}
	bills := []pricing.Bill{openBill(chargedHourly("p1", "vm1", "instance", "17.40", 348, now))}
	in := ForecastInput{Billing: bc, Resources: []ForecastResource{{ResourceID: "vm1", ResourceType: "instance", ProjectID: "p1",
		Rates: []ForecastRate{hourRate("r", "0.05")}}}}
	decEq(t, "limited", ProjectMonthEnd(bills, in, now).Projected, "2.60") // 52 × 0.05

	// A month rule is paid up front: once charged it adds nothing; not yet charged, it adds once.
	month := ForecastRate{TimeUnit: pricing.TimeUnitMonth, Results: []pricing.PricePlanRuleResult{{
		PricePlanRule: pricing.PricePlanRule{ID: "r-m", TimeUnit: pricing.TimeUnitMonth},
		Amounts:       []pricing.PricePlanRuleResultAmount{{AttributeName: "existence", NetAmount: mustDec("5")}},
	}}}
	charged := chargedHourly("p1", "lb", "load_balancer", "5", 0, now)
	charged.TimeUnits.Month = i64(1)
	in = ForecastInput{Resources: []ForecastResource{{ResourceID: "lb", ResourceType: "load_balancer", ProjectID: "p1", Rates: []ForecastRate{month}}}}
	decEq(t, "month rule already paid", ProjectMonthEnd([]pricing.Bill{openBill(charged)}, in, now).Projected, "0")
	decEq(t, "month rule not yet paid", ProjectMonthEnd(nil, in, now).Projected, "5")
}

// What cannot be rated keeps its current cost and is flagged with its type; a resolution failure
// flags everything; no forecaster at all leaves the forecast as current, flagged unavailable.
func TestForecast_ApproximateWhenUnrated(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	bills := []pricing.Bill{openBill(
		chargedHourly("p1", "vm1", "instance", "17.40", 348, now),
		chargedHourly("p1", "img", "image", "1.00", 348, now),
	)}
	in := ForecastInput{
		Resources: []ForecastResource{{ResourceID: "vm1", ResourceType: "instance", ProjectID: "p1", Rates: []ForecastRate{hourRate("r", "0.05")}}},
		Unrated:   []ForecastUnratedResource{{ResourceType: "image", ProjectID: "p1"}},
	}
	f := ProjectMonthEnd(bills, in, now)
	ci := costInfoFor(bills, now)
	ApplyForecast(ci, f, "")
	numEq(t, "forecast keeps the unrated image at its current cost", ci["forecastedMonthEndCosts"], "37.00") // 18.40 + 18.60
	if ci["forecastApproximate"] != true {
		t.Fatalf("unrated → approximate")
	}
	if got := ci["forecastUnratedTypes"].([]string); len(got) != 1 || got[0] != "image" {
		t.Errorf("unrated types = %v", got)
	}

	errF := MonthEndForecast(context.Background(), failingForecaster{}, "bp", bills, now)
	ci = costInfoFor(bills, now)
	ApplyForecast(ci, errF, "")
	numEq(t, "resolution error → current", ci["forecastedMonthEndCosts"], "18.40")
	if r := ci["forecastApproximateReasons"].([]string); len(r) != 1 || r[0] != ForecastUnavailable {
		t.Errorf("reasons = %v", r)
	}

	ci = costInfoFor(bills, now)
	ApplyForecast(ci, MonthEndForecast(context.Background(), nil, "bp", bills, now), "")
	if ci["forecastApproximate"] != true {
		t.Errorf("no forecaster → approximate")
	}
	// The untouched envelope says the same, so no client ever reads current as a projection.
	if CostInfoMap(decimal.Zero, decimal.Zero, map[string]any{}, map[string]any{}, []any{})["forecastApproximate"] != true {
		t.Errorf("CostInfoMap default must be approximate")
	}
}

// Only the month's OPEN bill feeds the watermarks (an issued bill of the month is not charged again;
// the oldest OPEN bill is the one the charge writes to).
func TestForecast_ReadsTheChargedOpenBill(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	older := openBill(chargedHourly("p1", "vm1", "instance", "17.40", 348, now))
	older.ID = "a"
	newer := openBill(chargedHourly("p1", "vm1", "instance", "0", 0, fcMonthStart))
	newer.ID = "b"
	in := ForecastInput{Resources: []ForecastResource{{ResourceID: "vm1", ResourceType: "instance", ProjectID: "p1", Rates: []ForecastRate{hourRate("r", "0.05")}}}}
	decEq(t, "projected from the oldest OPEN bill", ProjectMonthEnd([]pricing.Bill{newer, older}, in, now).Projected, "18.60")
}

type failingForecaster struct{}

func (failingForecaster) ForecastInput(context.Context, string, time.Time) (ForecastInput, error) {
	return ForecastInput{}, errors.New("resolution failed")
}

func costInfoFor(bills []pricing.Bill, now time.Time) map[string]any {
	return CostInfoMap(BillCostBreakdown(bills, now, nil))
}

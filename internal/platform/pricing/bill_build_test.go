package pricing

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

var billNow = time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)

func amt(name, net string) PricePlanRuleResultAmount {
	return PricePlanRuleResultAmount{AttributeName: name, NetAmount: mustDec(net), AttributeValue: mustDec("1")}
}

func result(ruleID, applyMethod string, amounts ...PricePlanRuleResultAmount) PricePlanRuleResult {
	return PricePlanRuleResult{PricePlanRule: PricePlanRule{ID: ruleID, ApplyMethod: applyMethod}, Amounts: amounts}
}

func billRes(id string, values map[string]any) *BillingResource {
	if values == nil {
		values = map[string]any{}
	}
	return &BillingResource{ResourceID: id, ResourceType: "compute", Values: values}
}

// existing item with all counters 0 and the given hour watermark.
func itemWith(tu *BillItemTimeUnits) BillItem {
	return BillItem{ResourceID: "r", ResourceType: "compute", NetAmount: decimal.Zero, TimeUnits: tu, AppliedPricePlanRules: []BillItemAppliedPricePlanRule{}}
}

func onlyItem(t *testing.T, bill *Bill) *BillItem {
	t.Helper()
	if len(bill.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(bill.Items))
	}
	return &bill.Items[0]
}

func TestSaveChargingNewItemHourly(t *testing.T) {
	bill := &Bill{InvoiceCurrency: "USD"} // no billing cycle, res has no createdAt → nil watermark → diff 1
	SaveChargingToBill(RatingContext{TimeUnit: TimeUnitHour, CycleTimestamp: billNow}, BillingContext{}, bill,
		billRes("r", nil), []PricePlanRuleResult{result("rule1", "", amt("vcpu", "5"))}, billNow)
	it := onlyItem(t, bill)
	if !it.NetAmount.Equal(mustDec("5")) {
		t.Errorf("netAmount = %s, want 5", it.NetAmount)
	}
	if it.Name != "r" || it.Currency != "USD" {
		t.Errorf("name/currency = %s/%s", it.Name, it.Currency)
	}
	if *it.TimeUnits.Hour != 1 || it.TimeUnits.HourLastRateTime == nil || !it.TimeUnits.HourLastRateTime.Equal(billNow) {
		t.Errorf("hour counter/watermark wrong: %d %v", *it.TimeUnits.Hour, it.TimeUnits.HourLastRateTime)
	}
	if it.TimeUnits.MonthLastRateTime != nil {
		t.Error("MonthLastRateTime must start nil on a new item")
	}
	if len(it.AppliedPricePlanRules) != 1 || !it.AppliedPricePlanRules[0].AppliedAmounts[0].NetAmount.Equal(mustDec("5")) {
		t.Errorf("applied amount wrong: %+v", it.AppliedPricePlanRules)
	}
}

func TestSaveChargingDisplayNameAndMetadata(t *testing.T) {
	bill := &Bill{InvoiceCurrency: "USD"}
	vals := map[string]any{"display_name": "web-1", "region": "eu"}
	SaveChargingToBill(RatingContext{TimeUnit: TimeUnitHour, CycleTimestamp: billNow}, BillingContext{}, bill,
		billRes("r", vals), []PricePlanRuleResult{result("rule1", "", amt("vcpu", "5"))}, billNow)
	it := onlyItem(t, bill)
	if it.Name != "web-1" {
		t.Errorf("name = %s, want web-1", it.Name)
	}
	if it.Metadata["region"] != "eu" {
		t.Errorf("metadata not round-tripped: %v", it.Metadata)
	}
}

func TestSaveChargingNotEligible(t *testing.T) {
	bill := &Bill{InvoiceCurrency: "USD"}
	res := billRes("r", nil)
	res.NotEligibleForBilling = true
	SaveChargingToBill(RatingContext{TimeUnit: TimeUnitHour, CycleTimestamp: billNow}, BillingContext{}, bill,
		res, []PricePlanRuleResult{result("rule1", "", amt("vcpu", "5"))}, billNow)
	it := onlyItem(t, bill)
	if !it.NetAmount.IsZero() || len(it.AppliedPricePlanRules) != 0 {
		t.Errorf("ineligible resource must not be charged: net=%s applied=%d", it.NetAmount, len(it.AppliedPricePlanRules))
	}
}

func TestSaveChargingCapGateFrozen(t *testing.T) {
	// item already at the cap → gate (cap > net) false → no mutation at all.
	tu := &BillItemTimeUnits{Minute: ptr64(0), Hour: ptr64(7), Month: ptr64(0)}
	hw := billNow.Add(-3 * time.Hour)
	tu.HourLastRateTime = &hw
	item := itemWith(tu)
	item.NetAmount = mustDec("3600") // == cap (720 * 5)
	item.Name = "frozen"
	bill := &Bill{InvoiceCurrency: "USD", Items: []BillItem{item}}
	SaveChargingToBill(RatingContext{TimeUnit: TimeUnitHour, CycleTimestamp: billNow}, BillingContext{}, bill,
		billRes("r", nil), []PricePlanRuleResult{result("rule1", "", amt("vcpu", "5"))}, billNow)
	it := onlyItem(t, bill)
	if !it.NetAmount.Equal(mustDec("3600")) || *it.TimeUnits.Hour != 7 || it.Name != "frozen" || len(it.AppliedPricePlanRules) != 0 {
		t.Errorf("frozen item mutated: net=%s hour=%d name=%s applied=%d", it.NetAmount, *it.TimeUnits.Hour, it.Name, len(it.AppliedPricePlanRules))
	}
}

func TestSaveChargingCapClamp(t *testing.T) {
	// MONTH limit 1 → cap = 1 * 5 = 5; a rule whose accumulated calc would exceed 5 is clamped.
	// Force calc > cap by charging MONTH twice on the same ADD rule (diff always 1).
	bill := &Bill{InvoiceCurrency: "USD"}
	rc := RatingContext{TimeUnit: TimeUnitMonth, CycleTimestamp: billNow}
	res := billRes("r", nil)
	rule := []PricePlanRuleResult{result("rule1", "", amt("vcpu", "5"))}
	SaveChargingToBill(rc, BillingContext{}, bill, res, rule, billNow) // calc 5, cap 5 → net 5
	SaveChargingToBill(RatingContext{TimeUnit: TimeUnitMonth, CycleTimestamp: billNow.Add(48 * time.Hour)}, BillingContext{}, bill, res, rule, billNow)
	it := onlyItem(t, bill)
	// second charge: gate cap(5) > net(5)? false → frozen. So net stays 5 (cap clamp already bound it).
	if !it.NetAmount.Equal(mustDec("5")) {
		t.Errorf("net = %s, want clamped 5", it.NetAmount)
	}
}

func TestGetTimeUnitsDiff(t *testing.T) {
	rule := []PricePlanRuleResult{result("rule1", "", amt("vcpu", "5"))}
	run := func(tu *BillItemTimeUnits, rc RatingContext, res *BillingResource) *BillItem {
		item := itemWith(tu)
		bill := &Bill{InvoiceCurrency: "USD", Items: []BillItem{item}}
		SaveChargingToBill(rc, BillingContext{}, bill, res, rule, billNow)
		return &bill.Items[0]
	}
	t.Run("minute_elapsed_130", func(t *testing.T) {
		w := billNow.Add(-130 * time.Minute)
		it := run(&BillItemTimeUnits{Minute: ptr64(0), MinuteLastRateTime: &w, Hour: ptr64(0), Month: ptr64(0)},
			RatingContext{TimeUnit: TimeUnitMinute, CycleTimestamp: billNow}, billRes("r", nil))
		if *it.TimeUnits.Minute != 130 || !it.AppliedPricePlanRules[0].AppliedAmounts[0].NetAmount.Equal(mustDec("650")) {
			t.Errorf("minute=%d applied=%s, want 130 / 650", *it.TimeUnits.Minute, it.AppliedPricePlanRules[0].AppliedAmounts[0].NetAmount)
		}
	})
	t.Run("clamp_to_headroom", func(t *testing.T) {
		w := billNow.Add(-5 * time.Hour)
		it := run(&BillItemTimeUnits{Minute: ptr64(0), Hour: ptr64(719), HourLastRateTime: &w, Month: ptr64(0)},
			RatingContext{TimeUnit: TimeUnitHour, CycleTimestamp: billNow}, billRes("r", nil))
		if *it.TimeUnits.Hour != 720 { // 719 + min(5, 720-719)=1
			t.Errorf("hour = %d, want 720", *it.TimeUnits.Hour)
		}
	})
	t.Run("negative_not_floored", func(t *testing.T) {
		w := billNow.Add(2 * time.Hour) // watermark AFTER charge → elapsed -2
		it := run(&BillItemTimeUnits{Minute: ptr64(0), Hour: ptr64(719), HourLastRateTime: &w, Month: ptr64(0)},
			RatingContext{TimeUnit: TimeUnitHour, CycleTimestamp: billNow}, billRes("r", nil))
		if *it.TimeUnits.Hour != 717 || !it.AppliedPricePlanRules[0].AppliedAmounts[0].NetAmount.Equal(mustDec("-10")) {
			t.Errorf("hour=%d applied=%s, want 717 / -10", *it.TimeUnits.Hour, it.AppliedPricePlanRules[0].AppliedAmounts[0].NetAmount)
		}
	})
	t.Run("month_always_one", func(t *testing.T) {
		w := billNow.Add(-1000 * time.Hour)
		it := run(&BillItemTimeUnits{Minute: ptr64(0), Hour: ptr64(0), Month: ptr64(900), MonthLastRateTime: &w},
			RatingContext{TimeUnit: TimeUnitMonth, CycleTimestamp: billNow}, billRes("r", nil))
		if *it.TimeUnits.Month != 901 {
			t.Errorf("month = %d, want 901 (diff always 1)", *it.TimeUnits.Month)
		}
	})
	t.Run("deleted_at_overrides_charge_time", func(t *testing.T) {
		w := billNow.Add(-5 * time.Hour)
		res := billRes("r", nil)
		del := billNow.Add(-3 * time.Hour)
		res.DeletedAt = &del // chargeTime = -3h → elapsed (−3h − −5h)=2h → diff 2
		it := run(&BillItemTimeUnits{Minute: ptr64(0), Hour: ptr64(0), HourLastRateTime: &w, Month: ptr64(0)},
			RatingContext{TimeUnit: TimeUnitHour, CycleTimestamp: billNow}, res)
		if *it.TimeUnits.Hour != 2 {
			t.Errorf("hour = %d, want 2 (deletedAt charge time)", *it.TimeUnits.Hour)
		}
	})
}

func TestAccumulateAddToTotal(t *testing.T) {
	w := billNow.Add(-2 * time.Hour)
	item := itemWith(&BillItemTimeUnits{Minute: ptr64(0), Hour: ptr64(0), HourLastRateTime: &w, Month: ptr64(0)})
	bill := &Bill{InvoiceCurrency: "USD", Items: []BillItem{item}}
	rule := []PricePlanRuleResult{result("rule1", ApplyAddToTotal, amt("vcpu", "5"))}
	SaveChargingToBill(RatingContext{TimeUnit: TimeUnitHour, CycleTimestamp: billNow}, BillingContext{}, bill, billRes("r", nil), rule, billNow)                    // diff 2 → 10
	SaveChargingToBill(RatingContext{TimeUnit: TimeUnitHour, CycleTimestamp: billNow.Add(3 * time.Hour)}, BillingContext{}, bill, billRes("r", nil), rule, billNow) // diff 3 → +15
	it := onlyItem(t, bill)
	if !it.AppliedPricePlanRules[0].AppliedAmounts[0].NetAmount.Equal(mustDec("25")) || !it.NetAmount.Equal(mustDec("25")) {
		t.Errorf("accumulated applied=%s net=%s, want 25", it.AppliedPricePlanRules[0].AppliedAmounts[0].NetAmount, it.NetAmount)
	}
}

func TestOverwriteTotal(t *testing.T) {
	w := billNow.Add(-2 * time.Hour)
	item := itemWith(&BillItemTimeUnits{Minute: ptr64(0), Hour: ptr64(0), HourLastRateTime: &w, Month: ptr64(0)})
	bill := &Bill{InvoiceCurrency: "USD", Items: []BillItem{item}}
	rule := []PricePlanRuleResult{result("rule1", ApplyOverwriteTotal, amt("vcpu", "7"))}
	SaveChargingToBill(RatingContext{TimeUnit: TimeUnitHour, CycleTimestamp: billNow}, BillingContext{}, bill, billRes("r", nil), rule, billNow)                    // create: 7*2=14
	SaveChargingToBill(RatingContext{TimeUnit: TimeUnitHour, CycleTimestamp: billNow.Add(5 * time.Hour)}, BillingContext{}, bill, billRes("r", nil), rule, billNow) // overwrite: bare 7
	it := onlyItem(t, bill)
	if !it.AppliedPricePlanRules[0].AppliedAmounts[0].NetAmount.Equal(mustDec("7")) {
		t.Errorf("overwrite applied = %s, want bare 7 (no ×diff, no add)", it.AppliedPricePlanRules[0].AppliedAmounts[0].NetAmount)
	}
}

func TestNewAttributeOnExistingRuleNoDiff(t *testing.T) {
	w := billNow.Add(-3 * time.Hour)
	item := itemWith(&BillItemTimeUnits{Minute: ptr64(0), Hour: ptr64(0), HourLastRateTime: &w, Month: ptr64(0)})
	bill := &Bill{InvoiceCurrency: "USD", Items: []BillItem{item}}
	SaveChargingToBill(RatingContext{TimeUnit: TimeUnitHour, CycleTimestamp: billNow}, BillingContext{}, bill, billRes("r", nil),
		[]PricePlanRuleResult{result("rule1", ApplyAddToTotal, amt("vcpu", "5"))}, billNow) // diff 3 → vcpu 15
	SaveChargingToBill(RatingContext{TimeUnit: TimeUnitHour, CycleTimestamp: billNow.Add(2 * time.Hour)}, BillingContext{}, bill, billRes("r", nil),
		[]PricePlanRuleResult{result("rule1", ApplyAddToTotal, amt("vcpu", "5"), amt("storage", "9"))}, billNow) // diff 2: vcpu +10=25; storage NEW bare 9
	it := onlyItem(t, bill)
	amts := it.AppliedPricePlanRules[0].AppliedAmounts
	got := map[string]decimal.Decimal{}
	for _, a := range amts {
		got[a.AttributeName] = a.NetAmount
	}
	if !got["vcpu"].Equal(mustDec("25")) {
		t.Errorf("vcpu = %s, want accumulated 25", got["vcpu"])
	}
	if !got["storage"].Equal(mustDec("9")) {
		t.Errorf("storage = %s, want bare 9 (new attr, NO ×diff)", got["storage"])
	}
}

func TestScaleUpItems(t *testing.T) {
	bill := &Bill{Items: []BillItem{{NetAmount: mustDec("12.3456789012345678")}, {NetAmount: mustDec("0.005")}}}
	ScaleUpItems(bill)
	if !bill.Items[0].NetAmount.Equal(mustDec("12.35")) {
		t.Errorf("[0] = %s, want 12.35", bill.Items[0].NetAmount)
	}
	if !bill.Items[1].NetAmount.Equal(mustDec("0.01")) { // .005 half away from zero → .01
		t.Errorf("[1] = %s, want 0.01", bill.Items[1].NetAmount)
	}
}

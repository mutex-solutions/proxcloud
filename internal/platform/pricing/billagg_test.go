package pricing

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestBillGrossInCurrencyTaxThenFX(t *testing.T) {
	now := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	bill := &Bill{InvoiceCurrency: "USD", Items: []BillItem{{NetAmount: mustDec("100.00")}}}
	rates := []TaxRate{taxRate(lvl(0, 19))}

	t.Run("tax_first_then_fx", func(t *testing.T) {
		x := NewExchanger(&stubClient{rate: mustDec("4")})
		got, err := BillGrossInCurrency(bill, rates, "RON", "USD", now, x)
		if err != nil {
			t.Fatal(err)
		}
		// gross base = 100*1.19 = 119.00 ; then ×4 = 476.00. (NOT tax-of-FX'd-net = 100*4*1.19 also 476 here,
		// but FX-then-tax would tax in target ccy — the contract is tax in base FIRST.)
		if !got.Equal(mustDec("476")) {
			t.Errorf("gross = %s, want 476", got)
		}
	})
	t.Run("same_currency_no_fetch", func(t *testing.T) {
		c := &stubClient{rate: mustDec("4")}
		got, _ := BillGrossInCurrency(bill, rates, "USD", "USD", now, NewExchanger(c))
		if !got.Equal(mustDec("119")) || c.calls != 0 {
			t.Errorf("gross = %s calls = %d, want 119 / 0", got, c.calls)
		}
	})
	t.Run("includes_adjustment", func(t *testing.T) {
		// A −10 BillAdjustment (e.g. a savings-contract discount) must lower the taxed gross:
		// net-with-adj = 100 − 10 = 90 ; gross = 90 × 1.19 = 107.10 (items-only would wrongly give 119).
		adj := mustDec("-10")
		adjBill := &Bill{InvoiceCurrency: "USD", Items: []BillItem{{NetAmount: mustDec("100.00")}}, Adjustments: []BillAdjustment{{Amount: &adj}}}
		got, _ := BillGrossInCurrency(adjBill, rates, "USD", "USD", now, NewExchanger(&stubClient{rate: mustDec("4")}))
		if !got.Equal(mustDec("107.10")) {
			t.Errorf("gross with adjustment = %s, want 107.10", got)
		}
	})
}

func TestBillNetInProfileCurrency(t *testing.T) {
	now := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	bill := &Bill{InvoiceCurrency: "USD", Items: []BillItem{{NetAmount: mustDec("50")}}}

	t.Run("identity_when_same", func(t *testing.T) {
		c := &stubClient{rate: mustDec("1.5")}
		got, _ := BillNetInProfileCurrency(bill, "USD", "USD", now, NewExchanger(c))
		if !got.Equal(mustDec("50")) || c.calls != 0 {
			t.Errorf("net = %s calls = %d, want 50 / 0", got, c.calls)
		}
	})
	t.Run("multiply_to_profile", func(t *testing.T) {
		got, _ := BillNetInProfileCurrency(bill, "RON", "USD", now, NewExchanger(&stubClient{rate: mustDec("1.5")}))
		if !got.Equal(mustDec("75")) {
			t.Errorf("net = %s, want 75", got)
		}
	})
}

// scaleUpItemsBefore is ScaleUpItems as it was before the sum: every item rounded by itself.
func scaleUpItemsBefore(bill *Bill) {
	for i := range bill.Items {
		bill.Items[i].NetAmount = scaleHalfUp(bill.Items[i].NetAmount)
	}
}

func billJSON(t *testing.T, b *Bill) string {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A sandbox item under half a cent is invoiced 0.00 on its own line, so 10,000 one-minute
// sandboxes at 0.0002 were invoiced nothing. Their sum is now one added line, rounded once.
func TestScaleUpItemsInvoicesShortSandboxes(t *testing.T) {
	bill := &Bill{InvoiceCurrency: "USD"}
	for i := 0; i < 10000; i++ {
		bill.Items = append(bill.Items, BillItem{ResourceID: fmt.Sprintf("sbx-%d", i), ResourceType: "sandbox", ProjectID: "p1", Currency: "USD", NetAmount: mustDec("0.0002")})
	}
	ScaleUpItems(bill)
	if len(bill.Items) != 10001 {
		t.Fatalf("%d items, want the 10000 sandboxes and one line", len(bill.Items))
	}
	for i := 0; i < 10000; i++ {
		if !bill.Items[i].NetAmount.IsZero() {
			t.Fatalf("sandbox %d is invoiced %s on its own line too, want 0.00", i, bill.Items[i].NetAmount)
		}
	}
	line := bill.Items[10000]
	if !line.NetAmount.Equal(mustDec("2")) || line.Name != "Sandbox runs below 0.005 each (10000)" || line.ResourceType != "sandbox" ||
		line.ProjectID != "p1" || line.Currency != "USD" || line.ResourceID != "" {
		t.Fatalf("line = %+v, want 2.00 for 10000 sandboxes of p1", line)
	}
	if total := bill.CalculateTotalAmount(); !total.Equal(mustDec("2")) {
		t.Fatalf("invoiced %s, want 2.00", total)
	}
	before := billJSON(t, bill)
	ScaleUpItems(bill) // again: the items are 0.00 by now and the line is not one of them
	if after := billJSON(t, bill); after != before {
		t.Fatal("a second ScaleUpItems changed the bill")
	}
}

// The line is per project, holds only the sandbox items that round to 0.00, and is within one
// rounding step (0.005) of what they accrued. A sandbox item of 0.005 or more keeps its own
// rounding; a sum that itself rounds to 0.00 adds no line.
func TestScaleUpItemsShortSandboxLines(t *testing.T) {
	sbx := func(project, net string) BillItem {
		return BillItem{ResourceType: "sandbox", ProjectID: project, NetAmount: mustDec(net)}
	}
	bill := &Bill{Items: []BillItem{
		sbx("p2", "0.004"), sbx("p1", "0.0049"), sbx("p1", "0.005"), sbx("p1", "1.234"), sbx("p2", "0.004"),
		sbx("p1", "0.0012"), sbx("p1", "0"), sbx("p3", "0.004"), sbx("p2", "0.0044"),
	}}
	ScaleUpItems(bill)
	want := []string{"0", "0", "0.01", "1.23", "0", "0", "0", "0", "0", "0.01", "0.01"} // + p2: 0.0124, p1: 0.0061; p3: 0.004 is no line
	if len(bill.Items) != len(want) {
		t.Fatalf("%d items, want %d: %+v", len(bill.Items), len(want), bill.Items)
	}
	for i, w := range want {
		if !bill.Items[i].NetAmount.Equal(mustDec(w)) {
			t.Errorf("[%d] = %s, want %s", i, bill.Items[i].NetAmount, w)
		}
	}
	if p2, p1 := bill.Items[9], bill.Items[10]; p2.ProjectID != "p2" || p2.Name != "Sandbox runs below 0.005 each (3)" ||
		p1.ProjectID != "p1" || p1.Name != "Sandbox runs below 0.005 each (2)" {
		t.Errorf("lines = %+v, %+v; want p2 (3 runs) then p1 (2 runs)", p2, p1)
	}
}

// Only sandbox items are summed. A bill without a sandbox under half a cent is sent byte for
// byte as before, whatever else is under half a cent on it; on a mixed bill every item that was
// there is as before and the line is added after them.
func TestScaleUpItemsLeavesOtherTypesAlone(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	items := func() []BillItem {
		var out []BillItem
		for _, c := range []struct{ typ, net string }{
			{"instance", "12.3456789012345678"}, {"instance", "0.004"}, {"instance", "0.0002"}, {"volume", "0.0049"},
			{"function", "0.0031"}, {"sandbox_snapshot", "0.0012"}, {"floating_ip", "0.005"}, {"sandbox", "3.14159"}, {"sandbox", "0"},
		} {
			out = append(out, BillItem{Name: c.typ, ResourceID: "r-" + c.net, ResourceType: c.typ, ProjectID: "p1", Currency: "USD", CreatedAt: &now,
				NetAmount: mustDec(c.net), TimeUnits: &BillItemTimeUnits{Minute: ptr64(3)}, Metadata: map[string]any{"vcpus": 2.0},
				AppliedPricePlanRules: []BillItemAppliedPricePlanRule{{PricePlanRuleID: "rule", AppliedAmounts: []BillItemAppliedPricePlanRuleAmount{{AttributeName: "vcpus", NetAmount: mustDec(c.net)}}}}})
		}
		return out
	}
	got, was := &Bill{InvoiceCurrency: "USD", Items: items()}, &Bill{InvoiceCurrency: "USD", Items: items()}
	ScaleUpItems(got)
	scaleUpItemsBefore(was)
	if billJSON(t, got) != billJSON(t, was) {
		t.Fatalf("a bill without short sandboxes changed:\n%s\nwas\n%s", billJSON(t, got), billJSON(t, was))
	}

	short := BillItem{ResourceID: "sbx", ResourceType: "sandbox", ProjectID: "p1", Currency: "USD", NetAmount: mustDec("0.0049")}
	got = &Bill{InvoiceCurrency: "USD", Items: append(items(), short, short, short)}
	was = &Bill{InvoiceCurrency: "USD", Items: append(items(), short, short, short)}
	ScaleUpItems(got)
	scaleUpItemsBefore(was)
	n := len(was.Items)
	if len(got.Items) != n+1 || !got.Items[n].NetAmount.Equal(mustDec("0.01")) {
		t.Fatalf("mixed bill: %d items, want %d and a line of 0.01 (3 × 0.0049)", len(got.Items), n+1)
	}
	got.Items = got.Items[:n]
	if billJSON(t, got) != billJSON(t, was) {
		t.Fatal("mixed bill: an item that was on the bill changed")
	}
}

package billing

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestItemsSummaryHTML(t *testing.T) {
	// Grouping by resourceType, amount-desc ordering, escaping, blank type → Other, net total.
	bill := &pricing.Bill{InvoiceCurrency: "USD", Items: []pricing.BillItem{
		{ResourceType: "volume", NetAmount: d("2.50")},
		{ResourceType: "instance", NetAmount: d("10.00")},
		{ResourceType: "volume", NetAmount: d("1.25")},
		{ResourceType: "<b>x</b>", NetAmount: d("0.10")},
		{NetAmount: d("0.05")},
	}}
	got := itemsSummaryHTML(bill, "USD")
	for _, want := range []string{"Net amount (USD)", "&lt;b&gt;x&lt;/b&gt;", ">Other<", ">0.05<", ">3.75<", ">13.90<"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<b>x</b>") {
		t.Error("label not escaped")
	}
	order := []string{">instance<", ">volume<", ">&lt;b&gt;x&lt;/b&gt;<", ">Other<", "Total (net)"}
	last := -1
	for _, s := range order {
		i := strings.Index(got, s)
		if i < 0 || i < last {
			t.Fatalf("row order wrong at %q:\n%s", s, got)
		}
		last = i
	}

	// Cap: 15 distinct types → 12 rows + Other carrying the 3 smallest.
	big := &pricing.Bill{InvoiceCurrency: "EUR"}
	for i := 0; i < 15; i++ {
		big.Items = append(big.Items, pricing.BillItem{ResourceType: string(rune('a' + i)), NetAmount: decimal.NewFromInt(int64(15 - i))})
	}
	got = itemsSummaryHTML(big, "EUR")
	if n := strings.Count(got, "<tr>"); n != 1+12+1+1 { // header + 12 + Other + total
		t.Fatalf("rows = %d:\n%s", n-3, got)
	}
	if !strings.Contains(got, ">Other</td><td align=\"right\" style=\"padding:8px 10px;border-bottom:1px solid #e5e7eb;\">6.00<") || !strings.Contains(got, ">120.00<") {
		t.Fatalf("Other/total wrong:\n%s", got)
	}
	if strings.Contains(got, ">m<") || strings.Contains(got, ">o<") {
		t.Fatal("smallest types must fold into Other")
	}
}

func TestAmountDue(t *testing.T) {
	ten := d("10")
	bill := &pricing.Bill{Items: []pricing.BillItem{{ResourceType: "instance", NetAmount: ten}}}
	gst := []pricing.TaxRate{{Name: "GST", RateLevels: []pricing.TaxLevel{{Level: 1, Percentage: 9}}}}
	if got := amountDue(bill, nil); got != "10.00" {
		t.Errorf("no tax: %q", got)
	}
	if got := amountDue(bill, gst); got != "10.90" {
		t.Errorf("gross: %q", got)
	}
	four := d("4")
	bill.AppliedAccountCredits = []pricing.AppliedAccountCredit{{Amount: &four}}
	if got := amountDue(bill, gst); got != "6.54" {
		t.Errorf("partially covered: %q", got)
	}
	bill.AppliedPromotionalCredits = []pricing.AppliedPromotionalCredit{{Amount: &ten}}
	if got := amountDue(bill, gst); got != "0.00" {
		t.Errorf("covered: %q", got)
	}
}

func TestBillMailVarsDates(t *testing.T) {
	p := &BillingProfile{FirstName: "Ann", LastName: "Lee", Currency: "SGD"}
	bill := &pricing.Bill{Items: []pricing.BillItem{{ResourceType: "instance", NetAmount: d("1")}}}
	v := billMailVars(p, bill)
	if v["dueDate"] != "" || v["issueDate"] != "" || v["currency"] != "SGD" || v["itemsCount"] != 1 {
		t.Fatalf("vars: %v", v)
	}
}

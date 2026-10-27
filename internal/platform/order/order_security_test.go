package order

import (
	"testing"

	"github.com/shopspring/decimal"
)

// TestCoversOrderTotal is the regression for finding [14]: a payment must not mark an order PAID for
// less than what it owes (NetAmount+TaxAmount), so a settled gross below the total does not cover it.
func TestCoversOrderTotal(t *testing.T) {
	net := decimal.NewFromInt(100)
	tax := decimal.NewFromInt(20)
	o := &Order{NetAmount: &net, TaxAmount: &tax}

	if CoversOrderTotal(o, decimal.NewFromInt(119)) {
		t.Error("gross 119 < 120 owed must NOT cover the order")
	}
	if !CoversOrderTotal(o, decimal.NewFromInt(120)) {
		t.Error("gross == owed must cover")
	}
	if !CoversOrderTotal(o, decimal.NewFromInt(200)) {
		t.Error("gross > owed must cover")
	}
	if CoversOrderTotal(nil, decimal.NewFromInt(1000)) {
		t.Error("nil order must not be coverable")
	}
}

// TestShouldMarkPaid is the regression for the [14] wiring: a settlement flips an order PAID only
// when the order belongs to the PAYING profile AND the gross covers net+tax. A foreign profile or a
// short gross must NOT flip (so a payment can't pay off — or take over — another profile's order).
func TestShouldMarkPaid(t *testing.T) {
	net := decimal.NewFromInt(100)
	tax := decimal.NewFromInt(20)
	o := &Order{BillingProfileID: "bpA", NetAmount: &net, TaxAmount: &tax}

	if !ShouldMarkPaid(o, "bpA", decimal.NewFromInt(120)) {
		t.Error("own profile + covering gross must flip PAID")
	}
	if ShouldMarkPaid(o, "bpB", decimal.NewFromInt(120)) {
		t.Error("foreign profile must NOT flip PAID even when the gross covers")
	}
	if ShouldMarkPaid(o, "bpA", decimal.NewFromInt(119)) {
		t.Error("short gross must NOT flip PAID even for the owning profile")
	}
	if ShouldMarkPaid(nil, "bpA", decimal.NewFromInt(1000)) {
		t.Error("missing order must NOT flip PAID")
	}
}

package billing

import (
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// A server's instance + instance_traffic must merge into one top-resource row, and the list must be
// sorted by total cost (so cheap volumes don't crowd out the expensive VMs).
func TestBillCostBreakdown_TopMergedAndSorted(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	bill := pricing.Bill{
		BillingCycle: &pricing.BillBillingCycle{StartDate: &start},
		Items: []pricing.BillItem{
			// Volume first — mirrors the real bill's storage order; it must NOT lead "top".
			{Name: "pvc-1", ResourceID: "v1", ResourceType: "volume", NetAmount: mustDec("0.02")},
			{Name: "big-vm", ResourceID: "s1", ResourceType: "instance", NetAmount: mustDec("100.00")},
			{Name: "big-vm", ResourceID: "instance_traffic-s1", ResourceType: "instance_traffic", NetAmount: mustDec("5.00")},
			{Name: "small-vm", ResourceID: "s2", ResourceType: "instance", NetAmount: mustDec("1.00")},
		},
	}
	_, _, _, _, top := BillCostBreakdown([]pricing.Bill{bill}, now, nil)

	// instance + instance_traffic collapse into one row → 3 entries, not 4.
	if len(top) != 3 {
		t.Fatalf("expected 3 merged entries, got %d: %#v", len(top), top)
	}

	first := top[0].(map[string]any)
	res := first["resource"].(map[string]any)
	if res["name"] != "big-vm" {
		t.Fatalf("top[0] should be big-vm (highest cost), got %v", res["name"])
	}
	if res["id"] != "s1" {
		t.Errorf("merged id should be the server id s1, got %v", res["id"])
	}
	if res["type"] != "SERVER" {
		t.Errorf("merged type should be SERVER, got %v", res["type"])
	}
	numEq(t, "big-vm merged cost (100 + 5 traffic)", first["currentCost"], "105.00")

	names := make([]string, len(top))
	for i, e := range top {
		names[i] = e.(map[string]any)["resource"].(map[string]any)["name"].(string)
	}
	if names[0] != "big-vm" || names[1] != "small-vm" || names[2] != "pvc-1" {
		t.Errorf("sort order should be by cost desc, got %v", names)
	}
}

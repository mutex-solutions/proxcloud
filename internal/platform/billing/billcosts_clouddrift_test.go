package billing_test

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// TestBillCostBreakdown_CloudTypeParity guards against drift between the billing package's inlined
// cloud-type discriminator constants (billcosts.go: cloudTypeServer/Volume/FloatingIP/LoadBalancer/
// Bucket) and internal/cloud's CloudResourceType source of truth. The billing package deliberately
// must NOT import internal/cloud (so it can be extracted into the standalone billing module), so we
// pin the parity from an EXTERNAL test package (billing_test) that is allowed to import both.
//
// It also covers every mapped billing type + its FE data-key (the original test only pinned SERVER).
//
// DELETE this guard when the billing package is extracted (Phase 1): at that point the inlined
// constants become billing's own wire contract and internal/cloud is no longer the source of truth.
func TestBillCostBreakdown_CloudTypeParity(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) // current-month cycle → items land in `top`

	cases := []struct {
		billingType string
		resourceID  string
		wantType    string // the cloud.Type* the billing constant must equal
		wantDataKey string
	}{
		{"instance", "s1", cloud.TypeServer, "server"},
		{"volume", "v1", cloud.TypeVolume, "volume"},
		{"floating_ip", "f1", cloud.TypeFloatingIP, "floatingIp"},
		{"load_balancer", "lb1", cloud.TypeLoadBalancer, "loadBalancer"},
		{"bucket", "b1", cloud.TypeBucket, "bucket"},
	}

	items := make([]pricing.BillItem, 0, len(cases))
	for _, c := range cases {
		items = append(items, pricing.BillItem{
			Name:         c.resourceID,
			ResourceID:   c.resourceID,
			ResourceType: c.billingType,
			NetAmount:    decimal.RequireFromString("1.00"),
		})
	}
	bill := pricing.Bill{BillingCycle: &pricing.BillBillingCycle{StartDate: &start}, Items: items}

	_, _, _, _, top := billing.BillCostBreakdown([]pricing.Bill{bill}, now, nil)

	byID := map[string]map[string]any{}
	for _, e := range top {
		res := e.(map[string]any)["resource"].(map[string]any)
		byID[res["id"].(string)] = res
	}
	for _, c := range cases {
		res, ok := byID[c.resourceID]
		if !ok {
			t.Fatalf("%s: no topResourcePrices entry for id %s", c.billingType, c.resourceID)
		}
		if res["type"] != c.wantType {
			t.Errorf("%s: emitted type = %v, want cloud constant %q (billcosts.go constant drifted from internal/cloud)", c.billingType, res["type"], c.wantType)
		}
		data, _ := res["data"].(map[string]any)
		if _, ok := data[c.wantDataKey]; !ok {
			t.Errorf("%s: resource.data missing FE key %q; got %v", c.billingType, c.wantDataKey, data)
		}
	}
}

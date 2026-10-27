package billingresource

import (
	"context"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

func TestDatabaseClusterBilling(t *testing.T) {
	p := NewDatabaseClusterProvider()
	if p.Type() != cloud.TypeDatabaseCluster {
		t.Fatal("type")
	}
	cr := &cloud.CloudResource{
		ID: "r1", ProjectID: "p1", Type: cloud.TypeDatabaseCluster,
		Data: map[string]any{"database": map[string]any{
			"name": "orders", "engine": "postgresql", "version": "17", "status": "READY",
			"replicas": float64(3), "cpu": float64(2), "memory_gib": float64(8), "storage_gib": float64(100),
			"endpoint": "10.1.0.5",
		}},
	}
	brs, err := p.GetBillingInformation(context.Background(), billingapi.BillingContext{}, cr)
	if err != nil || len(brs) != 1 {
		t.Fatalf("brs=%v err=%v", brs, err)
	}
	br := brs[0]
	if br.ResourceType != "database_cluster" || br.NotEligibleForBilling {
		t.Fatalf("br = %+v", br)
	}
	// Totals are pre-multiplied by replicas — the pricing engine rates attributes independently.
	if br.Values["vcpus_total"] != 6.0 || br.Values["ram_gb_total"] != 24.0 || br.Values["storage_gb_total"] != 300.0 {
		t.Fatalf("totals = %v", br.Values)
	}
	if br.Values["engine"] != "postgresql" {
		t.Fatal("engine attribute (per-engine premium filter key)")
	}
	// Every declared attribute must be present in Values (the stamp/schema completeness rule).
	for _, attr := range databaseClusterType().Attributes {
		if _, has := br.Values[attr.Name]; !has {
			t.Errorf("attribute %q declared but not stamped", attr.Name)
		}
	}
	// Provisioning (no endpoint) is free.
	cr.Data["database"].(map[string]any)["endpoint"] = ""
	brs, _ = p.GetBillingInformation(context.Background(), billingapi.BillingContext{}, cr)
	if !brs[0].NotEligibleForBilling {
		t.Fatal("no endpoint must be not-eligible")
	}
	// A row with no data payload stays not-eligible instead of erroring.
	brs, err = p.GetBillingInformation(context.Background(), billingapi.BillingContext{}, &cloud.CloudResource{ID: "r2", Type: cloud.TypeDatabaseCluster, Data: map[string]any{}})
	if err != nil || !brs[0].NotEligibleForBilling {
		t.Fatalf("empty data: brs=%v err=%v", brs, err)
	}
}

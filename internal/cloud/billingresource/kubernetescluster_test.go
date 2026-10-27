package billingresource

import (
	"context"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

func TestKubernetesClusterBilling(t *testing.T) {
	p := NewKubernetesClusterProvider()
	cr := &cloud.CloudResource{
		ID: "r1", ProjectID: "p1", Type: cloud.TypeKubernetesCluster,
		Data: map[string]any{"cluster": map[string]any{
			"name": "prod", "version": "1.35.4", "status": "READY",
			"endpoint": "10.0.0.5:6443", "cp_replicas": float64(3),
			"node_groups": []any{map[string]any{"name": "w"}},
		}},
	}
	brs, err := p.GetBillingInformation(context.Background(), billingapi.BillingContext{}, cr)
	if err != nil || len(brs) != 1 {
		t.Fatalf("brs = %v, %v", brs, err)
	}
	br := brs[0]
	if br.ResourceType != "kubernetes_cluster" || br.NotEligibleForBilling {
		t.Errorf("type/eligible = %s/%v", br.ResourceType, br.NotEligibleForBilling)
	}
	if br.Values["display_name"] != "prod" || br.Values["node_groups"] != 1 || br.Values["cp_replicas"] != float64(3) {
		t.Errorf("values = %v", br.Values)
	}

	// Still provisioning (no endpoint yet) → not billed.
	cr.Data = map[string]any{"cluster": map[string]any{"name": "new", "status": "PENDING"}}
	brs, _ = p.GetBillingInformation(context.Background(), billingapi.BillingContext{}, cr)
	if !brs[0].NotEligibleForBilling {
		t.Error("provisioning cluster must not bill")
	}

	// Catalog carries the type (admin price-plan form).
	found := false
	for _, bt := range Catalog() {
		if bt.ResourceType == "kubernetes_cluster" {
			found = true
		}
	}
	if !found {
		t.Error("kubernetes_cluster missing from Catalog()")
	}
}

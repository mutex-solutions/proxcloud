package billingresource

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

func TestImageProvider(t *testing.T) {
	p := NewImageProvider()
	brs, err := p.GetBillingInformation(context.Background(), billingapi.BillingContext{}, &cloud.CloudResource{
		ID: "r1", ProjectID: "p1", ExternalID: "200", Type: cloud.TypeImage,
		Data: map[string]any{"image": map[string]any{"name": "golden", "status": "active", "size": float64(3<<30 + 1)}},
	})
	if err != nil || len(brs) != 1 {
		t.Fatalf("%v %+v", err, brs)
	}
	b := brs[0]
	if b.ResourceType != "image" || b.ResourceID != "r1" || b.Values["display_name"] != "golden" {
		t.Fatalf("%+v", b)
	}
	if got := b.Values["size_gb"].(decimal.Decimal); !got.Equal(decimal.NewFromInt(4)) {
		t.Fatalf("size_gb = %s, want 4 (rounded up)", got)
	}
	// A row the portal just wrote has no size yet: rated 0, not an error.
	brs, err = p.GetBillingInformation(context.Background(), billingapi.BillingContext{}, &cloud.CloudResource{
		ExternalID: "201", Data: map[string]any{"image": map[string]any{"name": "fresh"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := brs[0].Values["size_gb"]; ok {
		t.Fatalf("%+v", brs[0].Values)
	}
}

func TestCatalogHasNoInertTypes(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Catalog() {
		seen[c.ResourceType] = true
	}
	if !seen["image"] || !seen["floating_ip"] {
		t.Fatalf("catalog: %v", seen)
	}
	if seen["load_balancer"] {
		t.Fatal("load_balancer has no resource behind it")
	}
}

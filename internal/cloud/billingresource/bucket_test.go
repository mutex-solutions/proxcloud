package billingresource

import (
	"context"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// TestBucketProvider: the flat bucket cache row → a "bucket" billing resource with size ceil'd
// to whole GB (any non-empty bucket bills at least 1), and the type is in the catalog so the
// admin rule form can price it.
func TestBucketProvider(t *testing.T) {
	p := NewBucketProvider()
	rs, err := p.GetBillingInformation(context.Background(), billingapi.BillingContext{}, &cloud.CloudResource{
		ID: "r1", ProjectID: "p1", Type: cloud.TypeBucket, ExternalID: "backups",
		Data: map[string]any{"bucketName": "backups", "sizeInBytes": float64(1536 * 1024 * 1024), "objectCount": float64(42)},
	})
	if err != nil || len(rs) != 1 {
		t.Fatalf("rs=%v err=%v", rs, err)
	}
	r := rs[0]
	if r.ResourceType != "bucket" || r.NotEligibleForBilling {
		t.Errorf("resource = %+v", r)
	}
	if r.Values["size_gb"] != float64(2) { // 1.5 GiB → ceil 2
		t.Errorf("size_gb = %v, want 2", r.Values["size_gb"])
	}
	if r.Values["object_count"] != float64(42) || r.Values["display_name"] != "backups" {
		t.Errorf("values = %v", r.Values)
	}

	// Empty bucket bills 0 GB (an existence-priced rule still charges the flat fee).
	rs, _ = p.GetBillingInformation(context.Background(), billingapi.BillingContext{}, &cloud.CloudResource{
		ID: "r2", ProjectID: "p1", Type: cloud.TypeBucket, ExternalID: "empty", Data: map[string]any{},
	})
	if rs[0].Values["size_gb"] != float64(0) || rs[0].Values["display_name"] != "empty" {
		t.Errorf("empty bucket values = %v", rs[0].Values)
	}

	found := false
	for _, bt := range Catalog() {
		if bt.ResourceType == "bucket" {
			found = true
		}
	}
	if !found {
		t.Error("bucket type missing from the catalog")
	}
}

package billingresource

import (
	"context"
	"math"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// BucketProvider maps a BUCKET (ceph-s3 / swift object storage) → a "bucket" BillingResource.
// The sync caches each bucket FLAT (bucketName/sizeInBytes/objectCount — providers.BucketProvider),
// so the billable quantity is the stored size: `size_gb` is ceil'd to a whole GB so a rule priced
// per-GB (like the volume rule) charges at least one unit for any non-empty bucket, and
// `existence` pricing keeps working for a flat per-bucket fee.
type BucketProvider struct{}

func NewBucketProvider() *BucketProvider { return &BucketProvider{} }

func (p *BucketProvider) Type() string { return cloud.TypeBucket }

func (p *BucketProvider) GetBillingInformation(_ context.Context, _ billingapi.BillingContext, cr *cloud.CloudResource) ([]*billingapi.BillingResource, error) {
	name := str(cr.Data["bucketName"])
	if name == "" {
		name = cr.ExternalID
	}
	values := map[string]any{
		"display_name": name,
		"size_gb":      sizeGB(cr.Data["sizeInBytes"]),
		"object_count": numOf(cr.Data["objectCount"]),
	}
	return []*billingapi.BillingResource{{
		ResourceID:          cr.ID,
		ProjectID:           cr.ProjectID,
		ResourceType:        "bucket",
		Values:              values,
		BillingResourceType: bucketType(),
	}}, nil
}

func bucketType() *billingapi.BillingResourceType {
	n := func(nm string) billingapi.ResourceAttribute {
		return billingapi.ResourceAttribute{Name: nm, Type: "number"}
	}
	return &billingapi.BillingResourceType{ResourceType: "bucket", Attributes: []billingapi.ResourceAttribute{
		{Name: "display_name", Type: "string"}, n("size_gb"), n("object_count"),
	}}
}

// sizeGB converts a stored byte count to whole GB, rounding UP (min 1 for any non-empty bucket).
func sizeGB(v any) float64 {
	b := numOf(v)
	if b <= 0 {
		return 0
	}
	return math.Ceil(b / (1024 * 1024 * 1024))
}

// numOf reads a numeric doc value (JSON round-trips numbers as float64; bson may keep ints).
func numOf(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

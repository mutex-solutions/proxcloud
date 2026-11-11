// niche_sync.go — the BUCKET sync provider (RGW and RustFS answer through the same client).
package providers

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/client"
)

type BucketProvider struct {
	cc        *client.Client
	region    string
	projectID string
}

func NewBucketProvider(cc *client.Client, region, projectID string) *BucketProvider {
	return &BucketProvider{cc: cc, region: region, projectID: projectID}
}

func (p *BucketProvider) Type() string      { return cloud.TypeBucket }
func (p *BucketProvider) ProjectID() string { return p.projectID }

// CompareKeys empty = whole-map compareMaps (the object-store provider passes a null dataKey). The
// bucket data is FLAT (no wrapping key); whole-map compare with number tolerance stops the numeric
// objectCount/sizeInBytes from churning on a JSON round-trip.
func (p *BucketProvider) CompareKeys() []string { return []string{} }

// MergeCached keeps the last known usage when RustFS's scanner could not answer.
func (p *BucketProvider) MergeCached(cached, fresh map[string]any) {
	client.KeepLastUsage(cached, fresh)
}

func (p *BucketProvider) List(ctx context.Context) ([]cloud.CloudResource, error) {
	bs, err := p.cc.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}
	return bucketsToResources(bs, p.region, p.projectID), nil
}

// bucketsToResources mirrors refreshResource's BUCKET shape: externalId = the bucket name, data = the
// flat DataBucket map (cr.Data = b, NOT wrapped).
func bucketsToResources(bs []map[string]any, region, projectID string) []cloud.CloudResource {
	out := make([]cloud.CloudResource, 0, len(bs))
	for _, b := range bs {
		name, _ := b["bucketName"].(string)
		if name == "" {
			continue
		}
		out = append(out, cloud.CloudResource{
			Type: cloud.TypeBucket, ExternalID: name, Region: region, ProjectID: projectID, Data: b,
		})
	}
	return out
}

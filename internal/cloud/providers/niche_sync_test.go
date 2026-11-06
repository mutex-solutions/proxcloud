package providers

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

func TestBucketsToResources(t *testing.T) {
	in := []map[string]any{
		{"bucketName": "pw-bucket", "objectCount": 3, "sizeInGb": 0.0},
		{"objectCount": 1}, // skipped — no bucketName
	}
	got := bucketsToResources(in, "RegionOne", "proj-1")
	if len(got) != 1 || got[0].ExternalID != "pw-bucket" || got[0].Type != cloud.TypeBucket {
		t.Fatalf("got %#v", got)
	}
	// bucket data is the FLAT map (cr.Data = b), not wrapped.
	if got[0].Data["bucketName"] != "pw-bucket" || got[0].Data["objectCount"] != 3 {
		t.Errorf("flat bucket data mismatch: %#v", got[0].Data)
	}
}

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
)

type fakeProvider struct{ items []cloud.CloudResource }

func (f fakeProvider) Type() string                                        { return cloud.TypeNetwork }
func (f fakeProvider) List(context.Context) ([]cloud.CloudResource, error) { return f.items, nil }

// TestProviderSync verifies the read-sync: a provider's listed resources are upserted into
// the cache, serviceId is stamped, data round-trips, and a re-sync is idempotent (upsert by
// {externalId, serviceId}).
func TestProviderSync(t *testing.T) {
	ctx := context.Background()
	repo := cloud.NewRepo(freshPG(t))

	fp := fakeProvider{items: []cloud.CloudResource{
		{Type: cloud.TypeNetwork, ExternalID: "net-1", ProjectID: "proj-net", Region: "RegionOne",
			Data: map[string]any{"network": map[string]any{"id": "net-1", "name": "a"}}},
		{Type: cloud.TypeNetwork, ExternalID: "net-2", ProjectID: "proj-net", Region: "RegionOne",
			Data: map[string]any{"network": map[string]any{"id": "net-2", "name": "b"}}},
	}}
	now := time.Now().UTC().Truncate(time.Millisecond)

	n, err := providers.Sync(ctx, fp, repo, "svc-net", now)
	if err != nil || n != 2 {
		t.Fatalf("sync: n=%d err=%v", n, err)
	}
	all, err := repo.FindAllByProjectID(ctx, "proj-net")
	if err != nil || len(all) != 2 {
		t.Fatalf("after sync: %d docs (%v)", len(all), err)
	}
	for _, r := range all {
		if r.ServiceID != "svc-net" || r.Type != cloud.TypeNetwork {
			t.Errorf("stamp: serviceId=%q type=%q", r.ServiceID, r.Type)
		}
		net, _ := r.Data["network"].(map[string]any)
		if net == nil || net["id"] == nil {
			t.Errorf("data round-trip: %v", r.Data)
		}
	}

	// re-sync (newer ts) → idempotent: still 2 (upsert by {externalId, serviceId}).
	if _, err := providers.Sync(ctx, fp, repo, "svc-net", now.Add(time.Minute)); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	all2, _ := repo.FindAllByProjectID(ctx, "proj-net")
	if len(all2) != 2 {
		t.Fatalf("re-sync not idempotent: %d docs", len(all2))
	}
}

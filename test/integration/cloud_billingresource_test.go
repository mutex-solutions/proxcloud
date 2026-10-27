//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/billingresource"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// fakeBRProvider emits one BillingResource per cloud resource (keyed by its id).
type fakeBRProvider struct{ typ string }

func (f fakeBRProvider) Type() string { return f.typ }
func (f fakeBRProvider) GetBillingInformation(_ context.Context, _ billingapi.BillingContext, cr *cloud.CloudResource) ([]*billingapi.BillingResource, error) {
	return []*billingapi.BillingResource{{ResourceID: cr.ID, ProjectID: cr.ProjectID, ResourceType: cr.Type}}, nil
}

// TestGetBillingResources verifies the cloud→billing dispatch: a service's cloud resources
// are flat-mapped through their type's Provider; types with no registered Provider skip.
func TestGetBillingResources(t *testing.T) {
	ctx := context.Background()
	repo := cloud.NewRepo(freshPG(t))
	now := time.Now().UTC().Truncate(time.Millisecond)

	seed := func(extID, typ string) {
		if _, err := repo.Insert(ctx, &cloud.CloudResource{
			ExternalID: extID, ServiceID: "svc-b", ProjectID: "proj-b", Type: typ,
			CreatedAt: &now, UpdatedAt: &now,
		}); err != nil {
			t.Fatalf("seed %s: %v", extID, err)
		}
	}
	seed("srv-a", cloud.TypeServer)
	seed("srv-b", cloud.TypeServer)
	seed("vol-a", cloud.TypeVolume) // unregistered type → skipped

	registry := map[string]billingresource.Provider{cloud.TypeServer: fakeBRProvider{typ: cloud.TypeServer}}

	brs, err := billingresource.GetBillingResources(ctx, repo, registry, "proj-b", "svc-b", billingapi.BillingContext{})
	if err != nil {
		t.Fatalf("getBillingResources: %v", err)
	}
	if len(brs) != 2 { // 2 servers mapped, volume skipped
		t.Fatalf("expected 2 billing resources, got %d", len(brs))
	}
	for _, br := range brs {
		if br.ResourceType != cloud.TypeServer || br.ProjectID != "proj-b" {
			t.Errorf("unexpected BR: type=%s proj=%s", br.ResourceType, br.ProjectID)
		}
	}
}

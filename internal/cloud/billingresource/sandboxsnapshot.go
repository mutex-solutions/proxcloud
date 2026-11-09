package billingresource

import (
	"context"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// SandboxSnapshotProvider maps a SANDBOX_SNAPSHOT (a sandbox's filesystem snapshot: what a paused
// sandbox or an E2B snapshot keeps) → a "sandbox_snapshot" BillingResource. Price it like
// volumes: `hour` rules on size_gb. The row exists from the moment the snapshot is stored until
// it is deleted (resume of a paused sandbox, or DELETE /templates/{id}), which archives it.
type SandboxSnapshotProvider struct{}

func NewSandboxSnapshotProvider() *SandboxSnapshotProvider { return &SandboxSnapshotProvider{} }

func (p *SandboxSnapshotProvider) Type() string { return cloud.TypeSandboxSnapshot }

func (p *SandboxSnapshotProvider) GetBillingInformation(_ context.Context, _ billingapi.BillingContext, cr *cloud.CloudResource) ([]*billingapi.BillingResource, error) {
	d := cr.Data
	created, err := time.Parse(time.RFC3339, str(d["createdAt"]))
	if err != nil {
		return nil, nil
	}
	cr.Info = &cloud.Info{CreatedAt: &created}
	name := str(d["name"])
	if name == "" {
		name = cr.ExternalID
	}
	return []*billingapi.BillingResource{{
		ResourceID:   cr.ID,
		ProjectID:    cr.ProjectID,
		ResourceType: "sandbox_snapshot",
		Values: map[string]any{
			"display_name": name,
			"kind":         d["kind"], // "pause" or "snapshot"
			"size_gb":      num(d["sizeGB"]),
		},
		BillingResourceType: sandboxSnapshotType(),
	}}, nil
}

func sandboxSnapshotType() *billingapi.BillingResourceType {
	return &billingapi.BillingResourceType{ResourceType: "sandbox_snapshot", Attributes: []billingapi.ResourceAttribute{
		{Name: "display_name", Type: "string"}, {Name: "kind", Type: "string"}, {Name: "size_gb", Type: "number"},
	}}
}

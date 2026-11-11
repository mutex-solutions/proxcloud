package billingresource

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// ShareProvider maps a SHARE (TrueNAS NFS/SMB) → a "share" BillingResource. Same basis choice as a bucket:
// price `size_gb` to bill what is used, `quota_gb` to bill what is allocated. `protocol` lets a rule filter.
type ShareProvider struct{}

func NewShareProvider() *ShareProvider { return &ShareProvider{} }

func (p *ShareProvider) Type() string { return cloud.TypeShare }

func (p *ShareProvider) GetBillingInformation(_ context.Context, _ billingapi.BillingContext, cr *cloud.CloudResource) ([]*billingapi.BillingResource, error) {
	sh, ok := mapAt(cr.Data, "share")
	if !ok {
		return []*billingapi.BillingResource{}, nil
	}
	return []*billingapi.BillingResource{{
		ResourceID:   cr.ID,
		ProjectID:    cr.ProjectID,
		ResourceType: "share",
		Values: map[string]any{
			"display_name": str(sh["name"]),
			"protocol":     str(sh["protocol"]),
			"size_gb":      sizeGB(sh["usedBytes"]),
			"quota_gb":     quotaGB(sh["sizeBytes"], sh["usedBytes"]),
		},
		BillingResourceType: shareType(),
	}}, nil
}

func shareType() *billingapi.BillingResourceType {
	return &billingapi.BillingResourceType{ResourceType: "share", Attributes: []billingapi.ResourceAttribute{
		{Name: "display_name", Type: "string"}, {Name: "protocol", Type: "string"},
		{Name: "size_gb", Type: "number"}, {Name: "quota_gb", Type: "number"},
	}}
}

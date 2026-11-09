package billingresource

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// ImageProvider maps a project template (IMAGE) → an "image" BillingResource priced by its
// provisioned size. Catalog templates live in the operator's own pool and never reach this cache.
type ImageProvider struct{}

func NewImageProvider() *ImageProvider { return &ImageProvider{} }

func (p *ImageProvider) Type() string { return cloud.TypeImage }

var bytesPerGiB = decimal.NewFromInt(1 << 30)

func (p *ImageProvider) GetBillingInformation(_ context.Context, _ billingapi.BillingContext, cr *cloud.CloudResource) ([]*billingapi.BillingResource, error) {
	img, ok := mapAt(cr.Data, "image")
	if !ok {
		return []*billingapi.BillingResource{}, nil
	}
	values := map[string]any{"display_name": str(img["name"]), "status": str(img["status"])}
	if str(img["name"]) == "" {
		values["display_name"] = fmt.Sprintf("image-%s", cr.ExternalID)
	}
	// Rounded up to whole GiB like a disk: a row the portal just wrote has no size until the next
	// sample (at most a minute), and an absent attribute rates 0.
	if size, ok := toDec(img["size"]); ok {
		values["size_gb"] = size.Div(bytesPerGiB).Ceil()
	}
	return []*billingapi.BillingResource{{
		ResourceID:          cr.ID,
		ProjectID:           cr.ProjectID,
		ResourceType:        "image",
		Values:              values,
		BillingResourceType: imageType(),
	}}, nil
}

func imageType() *billingapi.BillingResourceType {
	return &billingapi.BillingResourceType{ResourceType: "image", Attributes: []billingapi.ResourceAttribute{
		{Name: "size_gb", Type: "number"},
		{Name: "status", Type: "string"},
		{Name: "display_name", Type: "string"},
	}}
}

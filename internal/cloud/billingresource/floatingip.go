package billingresource

import (
	"context"
	"fmt"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// FloatingIPProvider maps a FLOATING_IP →
// a "floating_ip" BillingResource (typically a flat per-IP charge).
type FloatingIPProvider struct{}

func NewFloatingIPProvider() *FloatingIPProvider { return &FloatingIPProvider{} }

func (p *FloatingIPProvider) Type() string { return cloud.TypeFloatingIP }

func (p *FloatingIPProvider) GetBillingInformation(_ context.Context, _ billingapi.BillingContext, cr *cloud.CloudResource) ([]*billingapi.BillingResource, error) {
	values := map[string]any{"display_name": fmt.Sprintf("floating-ip-%s", cr.ExternalID)}
	if fip, ok := mapAt(cr.Data, "floatingIp"); ok {
		values["status"] = fip["status"]
		values["floating_network_id"] = fip["floating_network_id"]
	}
	return []*billingapi.BillingResource{{
		ResourceID:          cr.ID,
		ProjectID:           cr.ProjectID,
		ResourceType:        "floating_ip",
		Values:              values,
		BillingResourceType: floatingIPType(),
	}}, nil
}

func floatingIPType() *billingapi.BillingResourceType {
	s := func(n string) billingapi.ResourceAttribute {
		return billingapi.ResourceAttribute{Name: n, Type: "string"}
	}
	return &billingapi.BillingResourceType{ResourceType: "floating_ip", Attributes: []billingapi.ResourceAttribute{
		s("status"), s("floating_network_id"), s("display_name"),
	}}
}

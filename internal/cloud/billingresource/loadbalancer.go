package billingresource

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// LoadBalancerProvider maps a
// LOAD_BALANCER → a "load_balancer" BillingResource (flavor/status attributes).
type LoadBalancerProvider struct{}

func NewLoadBalancerProvider() *LoadBalancerProvider { return &LoadBalancerProvider{} }

func (p *LoadBalancerProvider) Type() string { return cloud.TypeLoadBalancer }

func (p *LoadBalancerProvider) GetBillingInformation(_ context.Context, _ billingapi.BillingContext, cr *cloud.CloudResource) ([]*billingapi.BillingResource, error) {
	values := map[string]any{}
	notEligible := false
	if lb, ok := mapAt(cr.Data, "loadBalancer"); ok {
		values["flavor_id"] = lb["flavor_id"]
		values["display_name"] = lb["name"]
		values["operating_status"] = lb["operating_status"]
		// operating_status == ERROR → not eligible.
		notEligible = str(lb["operating_status"]) == "ERROR"
	}
	return []*billingapi.BillingResource{{
		ResourceID:            cr.ID,
		ProjectID:             cr.ProjectID,
		ResourceType:          "load_balancer",
		Values:                values,
		BillingResourceType:   loadBalancerType(),
		NotEligibleForBilling: notEligible,
	}}, nil
}

func loadBalancerType() *billingapi.BillingResourceType {
	s := func(n string) billingapi.ResourceAttribute {
		return billingapi.ResourceAttribute{Name: n, Type: "string"}
	}
	return &billingapi.BillingResourceType{ResourceType: "load_balancer", Attributes: []billingapi.ResourceAttribute{
		s("flavor_id"), s("display_name"), s("operating_status"),
	}}
}

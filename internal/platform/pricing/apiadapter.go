package pricing

import "github.com/mutex-solutions/proxcloud/pkg/billingapi"

// This file is the RATING side of the charge-flow seam: it maps the billingapi wire contract into
// the engine's own types. Resolution (reading the cloud cache and building billable units) happens
// in internal/cloud/billingresource, which speaks only billingapi and never imports this package —
// that is what lets the rating half move into the standalone billing service while resolution stays
// in proxcloud.
//
// The mapping is a plain field copy: Values is passed through by reference (its numbers are already
// engine-compatible — see the billingapi package doc on json.Number).

// FromAPIResource maps one wire resource into the engine's resource. Returns nil for nil.
func FromAPIResource(in *billingapi.BillingResource) *BillingResource {
	if in == nil {
		return nil
	}
	return &BillingResource{
		ResourceID:            in.ResourceID,
		ProjectID:             in.ProjectID,
		ResourceType:          in.ResourceType,
		CreatedAt:             in.CreatedAt,
		DeletedAt:             in.DeletedAt,
		DisplayPrice:          in.DisplayPrice,
		Values:                in.Values,
		NotEligibleForBilling: in.NotEligibleForBilling,
		BillingResourceType:   FromAPIResourceType(in.BillingResourceType),
	}
}

// FromAPIResources maps a slice of wire resources, dropping nil entries. Never returns nil.
func FromAPIResources(in []*billingapi.BillingResource) []*BillingResource {
	out := make([]*BillingResource, 0, len(in))
	for _, r := range in {
		if c := FromAPIResource(r); c != nil {
			out = append(out, c)
		}
	}
	return out
}

// FromAPIResourceType maps a wire resource-type schema. Returns nil for nil.
func FromAPIResourceType(in *billingapi.BillingResourceType) *BillingResourceType {
	if in == nil {
		return nil
	}
	attrs := make([]ResourceAttribute, 0, len(in.Attributes))
	for _, a := range in.Attributes {
		attrs = append(attrs, ResourceAttribute{Type: a.Type, Name: a.Name, IsUsage: a.IsUsage})
	}
	return &BillingResourceType{ResourceType: in.ResourceType, Attributes: attrs}
}

// FromAPIResourceTypes maps a catalog, dropping nil entries. Never returns nil.
func FromAPIResourceTypes(in []*billingapi.BillingResourceType) []*BillingResourceType {
	out := make([]*BillingResourceType, 0, len(in))
	for _, t := range in {
		if c := FromAPIResourceType(t); c != nil {
			out = append(out, c)
		}
	}
	return out
}

// FromAPIBillingContext maps the wire billing context into the engine's.
func FromAPIBillingContext(in billingapi.BillingContext) BillingContext {
	return BillingContext{TimeUnitLimits: in.TimeUnitLimits}
}

// ToAPIBillingContext maps the engine's billing context out to the wire — used by the resolution
// side, which is handed a BillingContext but speaks billingapi to the providers.
func ToAPIBillingContext(in BillingContext) billingapi.BillingContext {
	return billingapi.BillingContext{TimeUnitLimits: in.TimeUnitLimits}
}

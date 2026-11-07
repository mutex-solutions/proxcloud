// Package billingresource is the cloud→billing bridge:
// list a service's cloud resources and turn each into the priced BillingResource(s) the
// rating loop consumes. This package holds the dispatch/registry orchestration; the per-type Values
// mapping (e.g. the SERVER/VPS provider reading UsageMetrics traffic into attribute
// values) lands as concrete Providers in following slices.
package billingresource

import (
	"context"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// Provider builds the BillingResource(s) for one CloudResourceType:
// reads the resource's data (+ its
// UsageMetrics usage, for traffic-billed types) into the priced attribute Values.
type Provider interface {
	Type() string
	GetBillingInformation(ctx context.Context, bc billingapi.BillingContext, cr *cloud.CloudResource) ([]*billingapi.BillingResource, error)
}

// GetBillingResources lists the
// service's cloud resources and flat-maps each through its type's Provider. Resources whose
// type has no registered Provider are skipped (faithful: only billable types contribute).
func GetBillingResources(ctx context.Context, repo *cloud.Repo, registry map[string]Provider, projectID, serviceID string, bc billingapi.BillingContext) ([]*billingapi.BillingResource, error) {
	resources, err := repo.FindByProjectAndService(ctx, projectID, serviceID)
	if err != nil {
		return nil, err
	}
	out := []*billingapi.BillingResource{}
	for i := range resources {
		cr := &resources[i]
		p, ok := registry[cr.Type]
		if !ok {
			continue
		}
		brs, err := p.GetBillingInformation(ctx, bc, cr)
		if err != nil {
			return nil, err
		}
		stampResourceValues(brs, cr)
		out = append(out, brs...)
	}
	return out, nil
}

// GetArchivedBillingResources is GetBillingResources over the project's rows archived since `since`
// (the cycle start), each with DeletedAt set: they owe the interval between their last charge and
// their deletion, which no live row will ever pay. The rating floor makes re-rating one a no-op, so
// feeding them on every pass is safe and needs no "already billed" bookkeeping.
//
// ponytail: costs a history read per project per charge, growing with the month's churn. Keep a
// billed marker on the history row if a project ever archives thousands of rows a month.
func GetArchivedBillingResources(ctx context.Context, repo *cloud.Repo, registry map[string]Provider, projectID, serviceID string, since time.Time, bc billingapi.BillingContext) ([]*billingapi.BillingResource, error) {
	archived, err := repo.FindArchivedSince(ctx, projectID, serviceID, since)
	if err != nil {
		return nil, err
	}
	out := []*billingapi.BillingResource{}
	for i := range archived {
		h := &archived[i]
		p, ok := registry[h.Type]
		if !ok {
			continue
		}
		cr := &cloud.CloudResource{
			ID: h.CloudResourceID, ProjectID: h.ProjectID, ExternalID: h.ExternalID, ServiceID: h.ServiceID,
			Type: h.Type, Region: h.Region, Data: h.Data, CreatedAt: h.CreatedAt,
		}
		brs, err := p.GetBillingInformation(ctx, bc, cr)
		if err != nil {
			return nil, err
		}
		stampResourceValues(brs, cr)
		for _, b := range brs {
			if b != nil {
				b.DeletedAt = h.DeletedAt
			}
		}
		out = append(out, brs...)
	}
	return out, nil
}

// stampResourceValues runs after each provider builds its BillingResources:
// inject `region` + `service_id` into
// the resource Values (so a price-plan rule FILTERED by region/service_id can match) and stamp
// `createdAt` from the cloud resource's info (else its doc createdAt) onto both Values and the
// BillingResource.CreatedAt — the latter drives mid-month proration in the rating engine
// (getTierValue). Without this, region/service_id filters never match and a `month`-timeUnit rule
// never prorates a mid-month resource (it rates the full month).
func stampResourceValues(brs []*billingapi.BillingResource, cr *cloud.CloudResource) {
	created := cr.CreatedAt
	if cr.Info != nil && cr.Info.CreatedAt != nil {
		created = cr.Info.CreatedAt
	}
	for _, b := range brs {
		if b == nil {
			continue
		}
		if b.Values == nil {
			b.Values = map[string]any{}
		}
		// A resource younger than its row (a server's snapshot) already says when it began.
		if b.CreatedAt == nil {
			b.CreatedAt = created
		}
		if b.CreatedAt != nil {
			b.Values["createdAt"] = *b.CreatedAt
		}
		// region/service_id are FILTER attributes: applyFilter looks up AttributeTypeByName on the
		// resource's type, so the value MUST be matched by a declared attribute or the filter errors
		// out (vs the previous benign no-match when the key was absent). We declare
		// them as "string" attributes idempotently.
		// When the resource has no type we leave the values unset (a filter then benignly no-matches).
		if b.BillingResourceType != nil {
			ensureStringAttr(b.BillingResourceType, "region")
			ensureStringAttr(b.BillingResourceType, "service_id")
			b.Values["region"] = cr.Region
			b.Values["service_id"] = cr.ServiceID
		}
	}
}

// ensureStringAttr adds a `string` ResourceAttribute named `name` to the type if absent (idempotent),
// so a price-plan filter on that attribute resolves instead of erroring on a missing attribute.
func ensureStringAttr(t *billingapi.BillingResourceType, name string) {
	for i := range t.Attributes {
		if t.Attributes[i].Name == name {
			return
		}
	}
	t.Attributes = append(t.Attributes, billingapi.ResourceAttribute{Name: name, Type: "string"})
}

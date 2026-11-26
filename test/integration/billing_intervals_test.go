//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/billingresource"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/billingjob"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// intervalProvider stands in for a provider of a resource charged by its running intervals. No such
// provider is registered in the product yet; the test's feed says what one hands the charge job.
type intervalProvider struct {
	feed func(cr *cloud.CloudResource) []*billingapi.BillingResource
}

func (p *intervalProvider) Type() string { return "INTERVAL_TEST" }
func (p *intervalProvider) GetBillingInformation(_ context.Context, _ billingapi.BillingContext, cr *cloud.CloudResource) ([]*billingapi.BillingResource, error) {
	return p.feed(cr), nil
}

// TestRunningIntervalsThroughTheChargeJob drives the charge job on PostgreSQL with a resource that
// starts, stops and starts again: the three fields travel provider → wire contract → adapter →
// engine → the stored bill, and the watermark comes back from the database between charges. It bills
// the minutes it ran, on ONE item: no idle gap, no minute twice, a start that has not served yet
// billed back once it serves. Then the external charge path: such a resource is not posted.
func TestRunningIntervalsThroughTheChargeJob(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	day := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	at := func(h, m, s int) time.Time {
		return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
	}

	cloudRepo := cloud.NewRepo(db)
	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD"})
	profileID := mustInsertID(t, db, "billingProfile", pgdoc.M{
		"status": billing.StatusActive, "currency": "USD", "pricePlanConfig": pgdoc.M{"includePublicPricePlans": true},
	})
	orgID := mustInsertID(t, db, "organization", pgdoc.M{"name": "acme", "billingProfileId": profileID})
	projectID := mustInsertID(t, db, "project", pgdoc.M{
		"name": "p1", "status": project.StatusEnabled, "organizationId": orgID,
		"memberships": []any{}, "services": []any{pgdoc.M{"serviceId": "svc-x"}},
	})
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-x", "type": externalservice.TypeCloud, "name": "dev", "config": pgdoc.M{}})
	created := at(10, 0, 0)
	if _, err := cloudRepo.Insert(ctx, &cloud.CloudResource{
		ExternalID: "fn-1", ServiceID: "svc-x", ProjectID: projectID, Type: "INTERVAL_TEST", CreatedAt: &created, UpdatedAt: &created,
	}); err != nil {
		t.Fatal(err)
	}
	mustInsert(t, db, "pricePlan", pricing.PricePlan{ID: "pp", Enabled: true, AccessMode: pricing.AccessPublic})
	mustInsert(t, db, "pricePlanRule", pricing.PricePlanRule{
		ID: "per-instance-minute", PricePlanID: "pp", TimeUnit: pricing.TimeUnitMinute, ResourceType: "interval_test",
		Prices: []pricing.PricePlanRulePrice{{AttributeName: "instances", Tiers: []pricing.PriceTier{{Value: dptr("0.01")}}}},
	})

	typ := func() *billingapi.BillingResourceType {
		return &billingapi.BillingResourceType{ResourceType: "interval_test", Attributes: []billingapi.ResourceAttribute{{Name: "instances", Type: "number"}}}
	}
	// one is an interval of the row: its start, its size, its end when it has ended.
	one := func(cr *cloud.CloudResource, from time.Time, instances int, end ...time.Time) *billingapi.BillingResource {
		b := &billingapi.BillingResource{
			ResourceID: cr.ID, ProjectID: cr.ProjectID, ResourceType: "interval_test", BillingResourceType: typ(),
			Values: map[string]any{"instances": instances}, ChargeFrom: &from, VariableRate: true,
		}
		if len(end) == 1 {
			b.DeletedAt = &end[0]
		}
		return b
	}
	provider := &intervalProvider{}
	driver := billingjob.New(billingjob.Deps{
		Billing: billing.NewRepo(db), ExternalServices: externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("k")),
		Projects: project.NewRepo(db), Orgs: org.NewRepo(db), Pricing: pricing.NewRepo(db),
		Engine: pricing.NewEngine(pricing.SystemClock()), Cloud: cloudRepo,
		Registry: map[string]billingresource.Provider{provider.Type(): provider},
	})
	charge := func(unit string, now time.Time, feed func(cr *cloud.CloudResource) []*billingapi.BillingResource) {
		t.Helper()
		provider.feed = feed
		if err := driver.Charge(ctx, unit, now); err != nil {
			t.Fatalf("%s charge at %s: %v", unit, now.Format(time.RFC3339), err)
		}
	}
	want := func(when string, minutes int64, net string, mark time.Time) {
		t.Helper()
		var bill pricing.Bill
		if found, err := db.C("bill").FindOne(ctx, pgdoc.M{"billingProfileId": profileID}, &bill); err != nil || !found {
			t.Fatalf("%s: no bill (%v)", when, err)
		}
		if len(bill.Items) != 1 {
			t.Fatalf("%s: %d bill items, want one for the resource's whole life", when, len(bill.Items))
		}
		it := bill.Items[0]
		if *it.TimeUnits.Minute != minutes || !it.NetAmount.Equal(decimalOf(t, net)) || !it.TimeUnits.MinuteLastRateTime.Equal(mark) || *it.TimeUnits.Hour != 0 {
			t.Fatalf("%s: %d minutes, net %s, watermark %v; want %d, %s, %v", when, *it.TimeUnits.Minute, it.NetAmount, it.TimeUnits.MinuteLastRateTime, minutes, net, mark)
		}
	}
	minute, hour := pricing.TimeUnitMinute, pricing.TimeUnitHour

	// A: two instances from 10:00:10 to 10:02:20, billed to 10:03. B: one instance from 10:02:40 (the
	// same minute A ended in) to 10:04:50, billed to 10:05. C: three instances from 10:30:15, which
	// serve from 10:32:40.
	a := func(cr *cloud.CloudResource, end ...time.Time) *billingapi.BillingResource {
		return one(cr, at(10, 0, 10), 2, end...)
	}
	charge(minute, at(10, 1, 4), func(cr *cloud.CloudResource) []*billingapi.BillingResource {
		return []*billingapi.BillingResource{a(cr)}
	})
	want("A runs", 1, "0.02", at(10, 1, 0))

	// The 10:02 charge is missed. At 10:03 A has ended and B runs: A comes back as an earlier interval.
	aThenB := func(cr *cloud.CloudResource) []*billingapi.BillingResource {
		earlier := a(cr, at(10, 3, 0))
		earlier.EarlierInterval = true
		return []*billingapi.BillingResource{earlier, one(cr, at(10, 2, 40), 1)}
	}
	charge(minute, at(10, 3, 4), aThenB)
	want("A to its end at two instances; the minute B shares with it is not charged again", 3, "0.06", at(10, 3, 0))
	charge(minute, at(10, 4, 4), aThenB)
	want("A again is nothing, B one minute", 4, "0.07", at(10, 4, 0))
	charge(hour, at(11, 0, 4), aThenB)
	want("an hour charge", 4, "0.07", at(10, 4, 0))

	bEnded := func(cr *cloud.CloudResource) []*billingapi.BillingResource {
		return []*billingapi.BillingResource{one(cr, at(10, 2, 40), 1, at(10, 5, 0))}
	}
	charge(minute, at(10, 5, 4), bEnded)
	want("B to its end", 5, "0.08", at(10, 5, 0))
	charge(minute, at(10, 20, 4), bEnded)
	want("stopped: nothing", 5, "0.08", at(10, 5, 0))

	starting := func(cr *cloud.CloudResource) []*billingapi.BillingResource {
		c := one(cr, at(10, 30, 15), 3)
		c.NotEligibleForBilling = true
		return []*billingapi.BillingResource{c}
	}
	charge(minute, at(10, 31, 4), starting)
	charge(minute, at(10, 32, 4), starting)
	want("C has not served yet", 5, "0.08", at(10, 5, 0))
	cRuns := func(cr *cloud.CloudResource) []*billingapi.BillingResource {
		return []*billingapi.BillingResource{one(cr, at(10, 30, 15), 3)}
	}
	charge(minute, at(10, 33, 4), cRuns)
	want("C served: billed back from 10:30, not from 10:05", 8, "0.17", at(10, 33, 0))
	charge(minute, at(10, 33, 30), cRuns)
	want("the same charge again", 8, "0.17", at(10, 33, 0))

	// The external charge path posts none of them: each of the three fields alone keeps a resource out.
	plain := func(cr *cloud.CloudResource, id string) *billingapi.BillingResource {
		return &billingapi.BillingResource{ResourceID: id, ProjectID: cr.ProjectID, ResourceType: "interval_test", BillingResourceType: typ(), Values: map[string]any{"instances": 1}}
	}
	provider.feed = func(cr *cloud.CloudResource) []*billingapi.BillingResource {
		from := at(10, 30, 15)
		withStart, variable, earlier := plain(cr, "start"), plain(cr, "variable"), plain(cr, "earlier")
		withStart.ChargeFrom, variable.VariableRate, earlier.EarlierInterval = &from, true, true
		return []*billingapi.BillingResource{withStart, plain(cr, "plain"), variable, earlier, one(cr, from, 3)}
	}
	req, err := driver.ResolveByProfileID(ctx, profileID, minute, at(10, 34, 4))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Services) != 1 || len(req.Services[0].Resources) != 1 || req.Services[0].Resources[0].ResourceID != "plain" {
		t.Fatalf("posted to the external billing service: %+v, want only the plain resource", req.Services)
	}
}

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/billingresource"
	"github.com/mutex-solutions/proxcloud/internal/cloud/metrics"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/billingjob"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// TestChargeDriverEndToEnd exercises the WHOLE Track-1.1 charge driver against the store:
// the gate (billingConfiguration exists) → load ACTIVE profiles + all
// externalServices → resolve the profile's ENABLED projects-with-services (via its org) →
// dispatch each project's SERVER cloud resource through the registry into BillingResources →
// select the PUBLIC price plan's MINUTE rules → rate + assemble onto the locked bill.
//
// The cache (cloudResource + usageMetrics) and config (pricePlan/rule) are seeded; no live
// cloud. 100 MB of traffic @ 0.01/MB ⇒ a charged instance_traffic item of exactly 1.
func TestChargeDriverEndToEnd(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	now := time.Now().UTC()
	y, mo, _ := now.Date()
	cycleStart := time.Date(y, mo, 1, 0, 0, 0, 0, time.UTC)
	cycleEnd := cycleStart.AddDate(0, 1, 0)

	billingRepo := billing.NewRepo(db)
	orgRepo := org.NewRepo(db)
	projectRepo := project.NewRepo(db)
	cloudRepo := cloud.NewRepo(db)
	metricsRepo := metrics.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("dev-key"))

	// billingConfiguration must exist for isBillingEnabled.
	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD"})

	// ACTIVE billing profile that includes public price plans.
	profileID := mustInsertID(t, db, "billingProfile", pgdoc.M{
		"status":          billing.StatusActive,
		"currency":        "USD",
		"pricePlanConfig": pgdoc.M{"includePublicPricePlans": true},
	})

	// An org referencing the profile, and an ENABLED project in that org attached to svc-x.
	orgID, err := db.C("organization").InsertOne(ctx, pgdoc.M{"name": "acme", "billingProfileId": profileID})
	if err != nil {
		t.Fatal(err)
	}
	projectID := mustInsertID(t, db, "project", pgdoc.M{
		"name": "p1", "status": project.StatusEnabled, "organizationId": orgID,
		"memberships": []any{}, "services": []any{pgdoc.M{"serviceId": "svc-x"}},
	})

	// The external service the project is attached to.
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-x", "type": externalservice.TypeCloud, "name": "dev", "config": pgdoc.M{}})

	// A SERVER cloud resource for (project, svc-x) + its month traffic (100 MB). createdAt is
	// exactly ONE MINUTE before the charge: the pipeline stamps BillingResource.CreatedAt from the doc,
	// and the minute-rule accrual = elapsed minutes since createdAt — one
	// minute → exactly 1 time unit → net = 100 MB × 0.01 × 1 = 1. (A createdAt equal to the charge
	// instant rates 0; an earlier fixture relied on the nil-watermark default of 1.)
	seededAt := now.Truncate(time.Minute).Add(-time.Minute)
	srv, err := cloudRepo.Insert(ctx, &cloud.CloudResource{
		ExternalID: "vm-1", ServiceID: "svc-x", ProjectID: projectID, Type: cloud.TypeServer,
		Data:      map[string]any{"flavorName": "m5.large", "server": map[string]any{"name": "vm", "status": "ACTIVE", "flavor": map[string]any{"ram": int64(2048), "vcpus": int64(2), "disk": int64(20)}}},
		CreatedAt: &seededAt, UpdatedAt: &seededAt,
	})
	if err != nil {
		t.Fatalf("seed server: %v", err)
	}
	if _, err := metricsRepo.Save(ctx, &metrics.UsageMetrics{
		ResourceID: srv.ID, ResourceType: cloud.TypeServer,
		BillingCycle: &metrics.BillBillingCycle{StartDate: &cycleStart, EndDate: &cycleEnd},
		Details:      &metrics.UsageMetricsDetails{TotalTrafficMb: decimal.RequireFromString("100")},
	}); err != nil {
		t.Fatalf("seed usage metrics: %v", err)
	}

	// A PUBLIC price plan + a MINUTE rule charging total_traffic_mb at 0.01 / MB.
	mustInsert(t, db, "pricePlan", pricing.PricePlan{ID: "pp-public", Enabled: true, AccessMode: pricing.AccessPublic})
	mustInsert(t, db, "pricePlanRule", pricing.PricePlanRule{
		PricePlanID: "pp-public", TimeUnit: pricing.TimeUnitMinute, ResourceType: "instance_traffic",
		Prices: []pricing.PricePlanRulePrice{{
			AttributeName: "total_traffic_mb",
			Tiers:         []pricing.PriceTier{{From: dptr("0"), To: nil, Value: dptr("0.01")}},
		}},
	})

	driver := billingjob.New(billingjob.Deps{
		Billing:          billingRepo,
		ExternalServices: esSvc,
		Projects:         projectRepo,
		Orgs:             orgRepo,
		Pricing:          pricingRepo,
		Engine:           pricing.NewEngine(pricing.SystemClock()),
		Cloud:            cloudRepo,
		Registry:         map[string]billingresource.Provider{cloud.TypeServer: billingresource.NewServerProvider(metricsRepo)},
		Now:              func() time.Time { return now },
	})

	if err := driver.Charge(ctx, pricing.TimeUnitMinute, now); err != nil {
		t.Fatalf("charge driver: %v", err)
	}

	// The driver produced a bill for the profile carrying the traffic charge.
	var bill pricing.Bill
	found, err := db.C("bill").FindOne(ctx, pgdoc.M{"billingProfileId": profileID}, &bill)
	if err != nil || !found {
		t.Fatalf("no bill produced for profile %s: found=%v err=%v", profileID, found, err)
	}
	trafficID := "instance_traffic-" + srv.ID
	var net *decimal.Decimal
	for i := range bill.Items {
		if bill.Items[i].ResourceID == trafficID {
			n := bill.Items[i].NetAmount
			net = &n
		}
	}
	if net == nil {
		t.Fatalf("no instance_traffic charge on the bill; items=%+v", bill.Items)
	}
	if !net.Equal(decimal.RequireFromString("1")) {
		t.Fatalf("traffic net = %s, want exactly 1 (100 MB × 0.01/MB)", net)
	}
	t.Logf("charge driver: profile %s → instance_traffic net = %s", profileID, net)
}

func TestChargeDriverGatedOffWithoutBillingConfig(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	// No billingConfiguration doc → isBillingEnabled false → no-op (no panic, no bill).
	driver := billingjob.New(billingjob.Deps{
		Billing:          billing.NewRepo(db),
		ExternalServices: externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("k")),
		Projects:         project.NewRepo(db),
		Orgs:             org.NewRepo(db),
		Pricing:          pricing.NewRepo(db),
		Engine:           pricing.NewEngine(pricing.SystemClock()),
		Cloud:            cloud.NewRepo(db),
		Registry:         map[string]billingresource.Provider{},
	})
	if err := driver.Charge(ctx, pricing.TimeUnitMinute, time.Now().UTC()); err != nil {
		t.Fatalf("gated charge should be a no-op, got %v", err)
	}
	n, err := db.C("bill").Count(ctx, pgdoc.M{})
	if err != nil || n != 0 {
		t.Fatalf("expected no bills when billing disabled, got %d (err=%v)", n, err)
	}
}

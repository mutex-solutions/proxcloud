//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// TestMonthEndForecastFollowsTheCharge runs the real hourly charge on a running VM, then asks the
// forecaster (the charge job's resolution + rules, read-only) for the month end: it must be the
// current cost plus the VM's hourly rate × the hours left, and nothing may be written by it.
func TestMonthEndForecastFollowsTheCharge(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC) // an hourly pass
	created := now.Add(-3 * time.Hour)

	billingRepo := billing.NewRepo(db)
	cloudRepo := cloud.NewRepo(db)
	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD"})
	profileID := mustInsertID(t, db, "billingProfile", pgdoc.M{
		"status": billing.StatusActive, "currency": "USD", "pricePlanConfig": pgdoc.M{"includePublicPricePlans": true},
	})
	orgID, err := db.C("organization").InsertOne(ctx, pgdoc.M{"name": "acme", "billingProfileId": profileID})
	if err != nil {
		t.Fatal(err)
	}
	projectID := mustInsertID(t, db, "project", pgdoc.M{
		"name": "p1", "status": project.StatusEnabled, "organizationId": orgID,
		"memberships": []any{}, "services": []any{pgdoc.M{"serviceId": "svc-x"}},
	})
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-x", "type": externalservice.TypeCloud, "name": "dev", "config": pgdoc.M{}})
	server := func(name, status string) *cloud.CloudResource {
		return &cloud.CloudResource{
			ExternalID: name, ServiceID: "svc-x", ProjectID: projectID, Type: cloud.TypeServer,
			Data: map[string]any{"flavorName": "m1", "server": map[string]any{
				"name": name, "status": status, "flavor": map[string]any{"ram": int64(2048), "vcpus": int64(2), "disk": int64(20)},
			}},
			CreatedAt: &created, UpdatedAt: &created,
		}
	}
	vm, err := cloudRepo.Insert(ctx, server("vm-running", "ACTIVE"))
	if err != nil {
		t.Fatal(err)
	}
	// A server in ERROR is not billed by the charge, so it must not be forecast either.
	if _, err := cloudRepo.Insert(ctx, server("vm-error", "ERROR")); err != nil {
		t.Fatal(err)
	}
	// 0.01 per vCPU-hour → the VM costs 0.02 an hour.
	mustInsert(t, db, "pricePlan", pricing.PricePlan{ID: "pp", Enabled: true, AccessMode: pricing.AccessPublic})
	mustInsert(t, db, "pricePlanRule", pricing.PricePlanRule{
		PricePlanID: "pp", TimeUnit: pricing.TimeUnitHour, ResourceType: "instance",
		Prices: []pricing.PricePlanRulePrice{{AttributeName: "vcpus", Tiers: []pricing.PriceTier{{Value: dptr("0.01")}}}},
	})

	driver := billingjob.New(billingjob.Deps{
		Billing: billingRepo, ExternalServices: externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("k")),
		Projects: project.NewRepo(db), Orgs: org.NewRepo(db), Pricing: pricing.NewRepo(db),
		Engine: pricing.NewEngine(pricing.SystemClock()), Cloud: cloudRepo,
		Registry: map[string]billingresource.Provider{cloud.TypeServer: billingresource.NewServerProvider(metrics.NewRepo(db))},
		Now:      func() time.Time { return now },
	})
	if err := driver.ChargeProfileByID(ctx, profileID, pricing.TimeUnitHour, now); err != nil {
		t.Fatalf("charge: %v", err)
	}
	bills, err := billingRepo.BillsByBillingProfile(ctx, profileID)
	if err != nil || len(bills) != 1 {
		t.Fatalf("bills = %d (%v)", len(bills), err)
	}
	cur, _ := billing.MonthlyBillCosts(bills, now)
	if !cur.Equal(decimal.RequireFromString("0.06")) { // 3 hours × 0.02
		t.Fatalf("current = %s, want 0.06", cur)
	}

	f := billing.MonthEndForecast(ctx, driver, profileID, bills, now)
	ci := billing.CostInfoMap(billing.BillCostBreakdown(bills, now, nil))
	billing.ApplyForecast(ci, f, "")
	hoursLeft := decimal.NewFromInt(int64(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).Sub(now) / time.Hour)) // 372
	want := cur.Add(decimal.RequireFromString("0.02").Mul(hoursLeft))                                         // 0.06 + 7.44
	if got := decimal.RequireFromString(ci["forecastedMonthEndCosts"].(json.Number).String()); !got.Equal(want) {
		t.Fatalf("forecast = %s, want %s", got, want)
	}
	if ci["forecastApproximate"] != false {
		t.Fatalf("fully rated → not approximate, got %v (%v)", ci["forecastApproximate"], ci["forecastApproximateReasons"])
	}
	if got := f.ByResource[vm.ID]; !got.Equal(decimal.RequireFromString("0.02").Mul(hoursLeft)) {
		t.Fatalf("vm projection = %s", got)
	}

	// Read-only: the forecast changed no bill.
	after, _ := billingRepo.BillsByBillingProfile(ctx, profileID)
	if c2, _ := billing.MonthlyBillCosts(after, now); !c2.Equal(cur) || len(after) != 1 {
		t.Fatalf("forecast wrote: current %s → %s, bills %d", cur, c2, len(after))
	}

	// Strictly read-only: an ACTIVE profile with no currency (which FindByID backfills) stays blank.
	if _, err := db.C("billingProfile").SetByID(ctx, profileID, pgdoc.M{}, []string{"currency"}); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.ForecastInput(ctx, profileID, now); err != nil {
		t.Fatal(err)
	}
	var raw pgdoc.M
	if found, err := db.C("billingProfile").Get(ctx, profileID, &raw); err != nil || !found {
		t.Fatalf("reload profile: %v", err)
	}
	if c, ok := raw["currency"]; ok && c != "" {
		t.Fatalf("the forecast wrote the profile's currency: %v", c)
	}

	// A suspended profile accrues nothing more: forecast == current.
	if _, err := db.C("billingProfile").SetByID(ctx, profileID, pgdoc.M{"status": billing.StatusSuspended}, nil); err != nil {
		t.Fatal(err)
	}
	f = billing.MonthEndForecast(ctx, driver, profileID, bills, now)
	if !f.Projected.IsZero() {
		t.Fatalf("suspended profile projected %s", f.Projected)
	}

	// Past the resource cap the resolution refuses rather than run unbounded.
	if _, err := db.C("billingProfile").SetByID(ctx, profileID, pgdoc.M{"status": billing.StatusActive, "currency": "USD"}, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < billing.ForecastMaxResources; i++ {
		if _, err := cloudRepo.Insert(ctx, server(fmt.Sprintf("bulk-%d", i), "ACTIVE")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := driver.ForecastInput(ctx, profileID, now); !errors.Is(err, billing.ErrForecastTooLarge) {
		t.Fatalf("over the cap: err = %v, want ErrForecastTooLarge", err)
	}
}

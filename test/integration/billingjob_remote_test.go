//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/billingresource"
	"github.com/mutex-solutions/proxcloud/internal/cloud/metrics"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/billingclient"
	"github.com/mutex-solutions/proxcloud/internal/platform/billingjob"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// chargeFixture is the shared scenario for the in-process/remote comparison: one ACTIVE profile,
// one ENABLED project on svc-x, one SERVER with 100 MB of monthly traffic, and a public price plan
// charging 0.01 per MB per minute. Returns the profile id, the traffic resource id and the driver.
func chargeFixture(t *testing.T, db *pgdoc.DB, now time.Time) (profileID, trafficID string, driver *billingjob.Service) {
	t.Helper()
	ctx := context.Background()
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

	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD"})
	profileID = mustInsertID(t, db, "billingProfile", pgdoc.M{
		"status": billing.StatusActive, "currency": "USD",
		"pricePlanConfig": pgdoc.M{"includePublicPricePlans": true},
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

	seededAt := now.Truncate(time.Minute).Add(-time.Minute)
	srv, err := cloudRepo.Insert(ctx, &cloud.CloudResource{
		ExternalID: "nova-1", ServiceID: "svc-x", ProjectID: projectID, Type: cloud.TypeServer,
		Data:      map[string]any{"flavorName": "m5.large", "server": map[string]any{"name": "vm", "status": "ACTIVE", "flavor": map[string]any{"ram": int64(2048), "vcpus": int64(2), "disk": int64(20)}}},
		CreatedAt: &seededAt, UpdatedAt: &seededAt,
	})
	if err != nil {
		t.Fatalf("seed server: %v", err)
	}
	if _, err := metricsRepo.Save(ctx, &metrics.GnocchiMetrics{
		ResourceID: srv.ID, ResourceType: cloud.TypeServer,
		BillingCycle: &metrics.BillBillingCycle{StartDate: &cycleStart, EndDate: &cycleEnd},
		Details:      &metrics.GnocchiMetricsDetails{TotalTrafficMb: decimal.RequireFromString("100")},
	}); err != nil {
		t.Fatalf("seed gnocchi: %v", err)
	}

	mustInsert(t, db, "pricePlan", pricing.PricePlan{ID: "pp-public", Enabled: true, AccessMode: pricing.AccessPublic})
	mustInsert(t, db, "pricePlanRule", pricing.PricePlanRule{
		PricePlanID: "pp-public", TimeUnit: pricing.TimeUnitMinute, ResourceType: "instance_traffic",
		Prices: []pricing.PricePlanRulePrice{{
			AttributeName: "total_traffic_mb",
			Tiers:         []pricing.PriceTier{{From: dptr("0"), To: nil, Value: dptr("0.01")}},
		}},
	})

	driver = billingjob.New(billingjob.Deps{
		Billing: billingRepo, ExternalServices: esSvc, Projects: projectRepo, Orgs: orgRepo,
		Pricing: pricingRepo, Engine: pricing.NewEngine(pricing.SystemClock()), Cloud: cloudRepo,
		Registry: map[string]billingresource.Provider{cloud.TypeServer: billingresource.NewServerProvider(metricsRepo)},
		Now:      func() time.Time { return now },
	})
	return profileID, "instance_traffic-" + srv.ID, driver
}

// trafficNet returns the profile's charged instance_traffic amount.
func trafficNet(t *testing.T, db *pgdoc.DB, profileID, trafficID string) decimal.Decimal {
	t.Helper()
	var bill pricing.Bill
	found, err := db.C("bill").FindOne(context.Background(), pgdoc.M{"billingProfileId": profileID}, &bill)
	if err != nil || !found {
		t.Fatalf("no bill for profile %s: found=%v err=%v", profileID, found, err)
	}
	for i := range bill.Items {
		if bill.Items[i].ResourceID == trafficID {
			return bill.Items[i].NetAmount
		}
	}
	t.Fatalf("no instance_traffic charge on the bill; items=%+v", bill.Items)
	return decimal.Zero
}

// TestRemoteChargeMatchesInProcess is the golden-compare that makes the billing split safe to cut
// over: the same seeded cloud state, charged once by the in-process driver and once through the
// remote path, must produce the SAME bill.
//
// The remote leg is real: resolution runs locally, the resources are serialized and POSTed, and the
// stub billing service decodes them with the contract's decoder and rates them with the same
// pricing engine the extracted service runs. So this exercises the actual failure modes of the
// split — serialization, decoding and the resolution/rating handoff — not a mock of them.
func TestRemoteChargeMatchesInProcess(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	// The two legs run as subtests purely so each gets its own database: freshPG derives the
	// database name from t.Name().
	var wantNet decimal.Decimal
	t.Run("in-process", func(t *testing.T) {
		db := freshPG(t)
		profileID, trafficID, driver := chargeFixture(t, db, now)
		if err := driver.Charge(ctx, pricing.TimeUnitMinute, now); err != nil {
			t.Fatalf("in-process charge: %v", err)
		}
		wantNet = trafficNet(t, db, profileID, trafficID)
	})
	if wantNet.IsZero() {
		t.Fatal("the in-process leg did not produce a charge; nothing to compare against")
	}

	remoteDB := freshPG(t)
	remoteProfile, remoteTrafficID, remoteDriver := chargeFixture(t, remoteDB, now)
	remotePricing := pricing.NewRepo(remoteDB)
	remoteEngine := pricing.NewEngine(pricing.SystemClock())

	var sawResources int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cloud/internal/v1/profiles/active":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"profileIds": []string{remoteProfile}}})

		case "/cloud/internal/v1/charge":
			var req billingclient.ChargeRequest
			// Exactly how the billing service decodes it — UseNumber, not json.Unmarshal.
			if err := decodeCharge(r, &req); err != nil {
				t.Errorf("decode charge: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for _, svc := range req.Services {
				sawResources += len(svc.Resources)
				rc := pricing.RatingContext{TimeUnit: req.TimeUnit, CycleTimestamp: req.CycleTimestamp}
				plans := pricing.SelectPricePlansForService(remotePricing.PlanSource(r.Context()), nil, true, svc.ServiceID)
				rules := pricing.ApplicableRules(plans, remotePricing.RuleSource(r.Context()), req.TimeUnit)
				if _, err := pricing.ChargeBillingResources(
					r.Context(), remotePricing, remoteEngine, rc, pricing.BillingContext{},
					req.ProfileID, rules, pricing.FromAPIResources(svc.Resources),
					req.CycleStart, req.CycleEnd, req.CycleTimestamp, "USD", nil,
				); err != nil {
					t.Errorf("remote rating: %v", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"profileId": req.ProfileID, "charged": true}})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := billingclient.New(srv.URL, "test-token", 30*time.Second)
	if err := billingjob.RemoteCharge(ctx, remoteDriver, client, pricing.TimeUnitMinute, now, nil); err != nil {
		t.Fatalf("remote charge: %v", err)
	}
	if sawResources == 0 {
		t.Fatal("the billing service received no resources — resolution produced nothing")
	}

	gotNet := trafficNet(t, remoteDB, remoteProfile, remoteTrafficID)
	if !gotNet.Equal(wantNet) {
		t.Errorf("BILL DRIFT across the split: remote = %s, in-process = %s", gotNet, wantNet)
	}
	if !wantNet.Equal(decimal.RequireFromString("1")) {
		t.Errorf("fixture sanity: expected both legs to charge exactly 1, got %s", wantNet)
	}
	t.Logf("in-process and remote both charged %s across %d resolved resources", gotNet, sawResources)
}

// TestResolveByProfileIDCarriesTheCatalog: the resource-type catalog must travel with the request,
// because the billing service needs it to resolve the attributes that savings-contract targets and
// price-adjustment rules filter on, and it cannot derive it (it describes OpenStack resources).
func TestResolveByProfileIDCarriesTheCatalog(t *testing.T) {
	now := time.Now().UTC()
	db := freshPG(t)
	profileID, _, driver := chargeFixture(t, db, now)

	req, err := driver.ResolveByProfileID(context.Background(), profileID, pricing.TimeUnitMinute, now)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(req.Catalog) == 0 {
		t.Error("catalog must travel with the charge request")
	}
	if req.ProfileID != profileID {
		t.Errorf("profileId = %s, want %s", req.ProfileID, profileID)
	}
	if len(req.Services) == 0 {
		t.Fatal("expected at least one service with resources")
	}
	// Every resolved resource must carry its type schema, or the engine cannot rate its attributes.
	for _, svc := range req.Services {
		for _, r := range svc.Resources {
			if r.BillingResourceType == nil {
				t.Errorf("resource %s has no BillingResourceType", r.ResourceID)
			}
		}
	}
}

// decodeCharge decodes a charge request the way the billing service does: with the contract's
// UseNumber decoder, so numeric literals survive exactly.
func decodeCharge(r *http.Request, out *billingclient.ChargeRequest) error {
	return billingapi.NewDecoder(r.Body).Decode(out)
}

// TestChargeReplaySafety pins what happens when the SAME charge is applied twice — the situation
// the remote path makes materially more likely, since an HTTP timeout leaves the caller unsure
// whether the server committed.
//
// Both cadences turn out to be replay-safe, by two DIFFERENT mechanisms, and it is worth recording
// which is which because they fail under different conditions:
//
//   - MINUTE/HOUR are idempotent via the accrual watermark. diff = elapsed(cycleTimestamp -
//     lastRateTime), and the watermark advances to cycleTimestamp, so replaying the same timestamp
//     yields diff = 0 (bill_build.go getTimeUnitsDiff). This holds only while the caller replays the
//     SAME cycleTimestamp — which truncateForTimeUnit guarantees within a given minute/hour.
//
//   - MONTH is idempotent via the cap gate, not the watermark: getTimeUnitsDiff returns 1
//     unconditionally for that cadence, but updateBillItems refuses to accrue once
//     item.NetAmount has reached totalCap = timeUnitLimit(month) x price, and the default month
//     limit is 1 — so the first charge saturates the cap and the replay is a no-op. This one is
//     therefore sensitive to billingConfiguration.settings.timeUnitLimits: a month limit above 1
//     would re-open the double-accrual window.
//
// Neither property is enforced by an explicit idempotency key, so both are worth pinning: if a
// future change to the watermark or the cap gate breaks them, the remote charge path would start
// double-billing on retry, silently.
func TestChargeReplaySafety(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	t.Run("minute cadence is idempotent on replay", func(t *testing.T) {
		db := freshPG(t)
		profileID, trafficID, driver := chargeFixture(t, db, now)

		if err := driver.Charge(ctx, pricing.TimeUnitMinute, now); err != nil {
			t.Fatalf("first charge: %v", err)
		}
		first := trafficNet(t, db, profileID, trafficID)

		// Replay the identical tick: same `now`, so the same cycleTimestamp.
		if err := driver.Charge(ctx, pricing.TimeUnitMinute, now); err != nil {
			t.Fatalf("replayed charge: %v", err)
		}
		second := trafficNet(t, db, profileID, trafficID)

		if !second.Equal(first) {
			t.Errorf("replaying a minute charge changed the bill: %s -> %s (expected idempotent)", first, second)
		}
	})

	t.Run("month cadence is idempotent on replay (via the cap gate)", func(t *testing.T) {
		db := freshPG(t)
		profileID, _, driver := chargeFixture(t, db, now)

		// Price the instance per month so the MONTH cadence has something to charge.
		mustInsert(t, db, "pricePlanRule", pricing.PricePlanRule{
			PricePlanID: "pp-public", TimeUnit: pricing.TimeUnitMonth, ResourceType: "instance",
			Prices: []pricing.PricePlanRulePrice{{
				AttributeName: "vcpus",
				Tiers:         []pricing.PriceTier{{From: dptr("0"), To: nil, Value: dptr("1")}},
			}},
		})

		if err := driver.Charge(ctx, pricing.TimeUnitMonth, now); err != nil {
			t.Fatalf("first charge: %v", err)
		}
		first := instanceNet(t, db, profileID)
		if err := driver.Charge(ctx, pricing.TimeUnitMonth, now); err != nil {
			t.Fatalf("replayed charge: %v", err)
		}
		second := instanceNet(t, db, profileID)

		if first.IsZero() {
			t.Fatal("fixture did not charge the instance; the month cadence is not being exercised")
		}
		if !second.Equal(first) {
			t.Errorf("replaying a month charge changed the bill: %s -> %s. The cap gate that makes "+
				"month replays safe has regressed; the remote charge path will double-bill on retry.",
				first, second)
		}
	})
}

// instanceNet returns the profile's charged "instance" amount.
func instanceNet(t *testing.T, db *pgdoc.DB, profileID string) decimal.Decimal {
	t.Helper()
	var bill pricing.Bill
	found, err := db.C("bill").FindOne(context.Background(), pgdoc.M{"billingProfileId": profileID}, &bill)
	if err != nil || !found {
		t.Fatalf("no bill for profile %s: found=%v err=%v", profileID, found, err)
	}
	total := decimal.Zero
	for i := range bill.Items {
		if bill.Items[i].ResourceType == "instance" {
			total = total.Add(bill.Items[i].NetAmount)
		}
	}
	return total
}

// TestRemoteChargeCreatesBillForProfileWithNoResources guards a difference that is easy to
// introduce as an "optimisation": the in-process driver calls ChargeBillingResources once per
// external service whether or not that service has resources, and that call opens the current bill
// via GetCurrentBill, which CREATES it when absent.
//
// So an ACTIVE profile with nothing billable still gets a bill each tick. If the remote path drops
// empty service groups to save bytes, those profiles silently stop getting bills — visible to the
// customer, and not something any amount-based assertion would catch.
func TestRemoteChargeCreatesBillForProfileWithNoResources(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	db := freshPG(t)

	billingRepo := billing.NewRepo(db)
	orgRepo := org.NewRepo(db)
	projectRepo := project.NewRepo(db)
	cloudRepo := cloud.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("dev-key"))

	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD"})
	profileID := mustInsertID(t, db, "billingProfile", pgdoc.M{
		"status": billing.StatusActive, "currency": "USD",
		"pricePlanConfig": pgdoc.M{"includePublicPricePlans": true},
	})
	orgID, err := db.C("organization").InsertOne(ctx, pgdoc.M{"name": "acme", "billingProfileId": profileID})
	if err != nil {
		t.Fatal(err)
	}
	// An ENABLED project attached to the service, but with NO cloud resources behind it.
	mustInsertID(t, db, "project", pgdoc.M{
		"name": "empty", "status": project.StatusEnabled, "organizationId": orgID,
		"memberships": []any{}, "services": []any{pgdoc.M{"serviceId": "svc-x"}},
	})
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-x", "type": externalservice.TypeCloud, "name": "dev", "config": pgdoc.M{}})

	driver := billingjob.New(billingjob.Deps{
		Billing: billingRepo, ExternalServices: esSvc, Projects: projectRepo, Orgs: orgRepo,
		Pricing: pricingRepo, Engine: pricing.NewEngine(pricing.SystemClock()), Cloud: cloudRepo,
		Registry: map[string]billingresource.Provider{},
		Now:      func() time.Time { return now },
	})

	req, err := driver.ResolveByProfileID(ctx, profileID, pricing.TimeUnitMinute, now)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(req.Services) == 0 {
		t.Fatal("a service with no resources must still be sent, or the bill is never created " +
			"for profiles that have nothing billable")
	}
	if n := len(req.Services[0].Resources); n != 0 {
		t.Fatalf("expected an empty resource group, got %d resources", n)
	}

	// And the in-process driver, for comparison, does create the bill.
	if err := driver.Charge(ctx, pricing.TimeUnitMinute, now); err != nil {
		t.Fatalf("in-process charge: %v", err)
	}
	var bill pricing.Bill
	found, err := db.C("bill").FindOne(ctx, pgdoc.M{"billingProfileId": profileID}, &bill)
	if err != nil {
		t.Fatalf("load bill: %v", err)
	}
	if !found {
		t.Error("the in-process driver should create a bill even with no billable resources; " +
			"the remote path must match")
	}
}

//go:build integration

package integration

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

type failBillingResource struct {
	billingresource.Provider
	fail atomic.Bool
}

func (p *failBillingResource) GetBillingInformation(ctx context.Context, bc billingapi.BillingContext, cr *cloud.CloudResource) ([]*billingapi.BillingResource, error) {
	if p.fail.Load() && cr.ServiceID == "svc-2" {
		return nil, errors.New("second service resolution failed")
	}
	return p.Provider.GetBillingInformation(ctx, bc, cr)
}

// Fixed original October cutoff, even though a worker may run in November. Real
// ServerProvider -> config selection -> rating -> bill money/units, no fake charger.
func durableBillingFixture(t *testing.T, db *pgdoc.DB, clocks ...pricing.Clock) (*billingjob.Service, billingjob.Cycle, *failBillingResource) {
	t.Helper()
	ctx := context.Background()
	cutoff := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	created := cutoff.Add(-2 * time.Hour)
	profile := mustInsertID(t, db, "billingProfile", pgdoc.M{"status": billing.StatusActive, "currency": "USD", "pricePlanConfig": pgdoc.M{"includePublicPricePlans": true}})
	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD"})
	orgID := mustInsertID(t, db, "organization", pgdoc.M{"billingProfileId": profile})
	projectID := mustInsertID(t, db, "project", pgdoc.M{"status": project.StatusEnabled, "organizationId": orgID, "services": []any{pgdoc.M{"serviceId": "svc-1"}, pgdoc.M{"serviceId": "svc-2"}}})
	cloudRepo := cloud.NewRepo(db)
	for _, svc := range []string{"svc-1", "svc-2"} {
		mustInsert(t, db, "externalService", pgdoc.M{"_id": svc, "type": externalservice.TypeCloud, "config": pgdoc.M{}})
		_, err := cloudRepo.Insert(ctx, &cloud.CloudResource{ExternalID: svc + "-vm", ServiceID: svc, ProjectID: projectID, Type: cloud.TypeServer, CreatedAt: &created,
			Data: map[string]any{"server": map[string]any{"name": svc, "status": "ACTIVE", "flavor": map[string]any{"vcpus": 2, "ram": 2048, "disk": 20}}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	mustInsert(t, db, "pricePlan", pricing.PricePlan{ID: "pp", Enabled: true, AccessMode: pricing.AccessPublic})
	mustInsert(t, db, "pricePlanRule", pricing.PricePlanRule{PricePlanID: "pp", TimeUnit: pricing.TimeUnitHour, ResourceType: "instance", Prices: []pricing.PricePlanRulePrice{{AttributeName: "vcpus", Tiers: []pricing.PriceTier{{Value: dptr("3")}}}}})
	provider := &failBillingResource{Provider: billingresource.NewServerProvider(metrics.NewRepo(db))}
	clock := pricing.SystemClock()
	if len(clocks) > 0 {
		clock = clocks[0]
	}
	s := billingjob.New(billingjob.Deps{DB: db, Billing: billing.NewRepo(db), Pricing: pricing.NewRepo(db), Engine: pricing.NewEngine(clock), Cloud: cloudRepo,
		Projects: project.NewRepo(db), Orgs: org.NewRepo(db), ExternalServices: externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("test")), Registry: map[string]billingresource.Provider{cloud.TypeServer: provider}})
	if err := s.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := billingjob.NewCycle(profile, pricing.TimeUnitHour, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	return s, c, provider
}

func assertDurableBill(t *testing.T, db *pgdoc.DB, c billingjob.Cycle) {
	t.Helper()
	var bills []pricing.Bill
	if err := db.C("bill").Find(context.Background(), pgdoc.M{"billingProfileId": c.ProfileID}, &bills); err != nil {
		t.Fatal(err)
	}
	if len(bills) != 1 || !bills[0].CalculateTotalAmount().Equal(*dptr("24")) {
		t.Fatalf("bills=%+v; want one bill net24", bills)
	}
	if bills[0].BillingCycle.StartDate.Month() != time.October {
		t.Fatalf("original cycle moved: %+v", bills[0].BillingCycle)
	}
	for _, item := range bills[0].Items {
		if item.ResourceType == "instance" && (*item.TimeUnits.Hour != 2 || !item.TimeUnits.HourLastRateTime.Equal(c.CycleAt)) {
			t.Fatalf("original units/watermark changed: %+v", item)
		}
	}
}

func TestDurableBillingWholeProfileReceipt(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	s, c, p := durableBillingFixture(t, db)
	p.fail.Store(true)
	if err := s.ChargeProfileByID(ctx, c.ProfileID, c.TimeUnit, c.CycleAt); err == nil {
		t.Fatal("second service error swallowed")
	}
	for _, table := range []string{"bill", "billingChargeReceipt"} {
		n, err := db.C(table).Count(ctx, nil)
		if err != nil || n != 0 {
			t.Fatalf("partial profile commit %s=%d err%v", table, n, err)
		}
	}
	p.fail.Store(false)
	if err := s.ChargeProfileByID(ctx, c.ProfileID, c.TimeUnit, c.CycleAt); err != nil {
		t.Fatal(err)
	}
	assertDurableBill(t, db, c)
	// Simulate crash after the business transaction committed but before ACK.
	// Even deleting the bill cannot cause a matching retained receipt to charge again.
	if err := s.ChargeProfileByID(ctx, c.ProfileID, c.TimeUnit, c.CycleAt); err != nil {
		t.Fatal(err)
	}
	assertDurableBill(t, db, c)
	n, err := db.C("billingChargeReceipt").Count(ctx, nil)
	if err != nil || n != 1 {
		t.Fatalf("receipt count=%d err%v", n, err)
	}
}

func TestDurableBillingConcurrentReceipt(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	s, c, _ := durableBillingFixture(t, db)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Go(func() { errs <- s.ChargeProfileByID(ctx, c.ProfileID, c.TimeUnit, c.CycleAt) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertDurableBill(t, db, c)
}

func TestPgdocOwnedTransactionBridgeRollback(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	if err := db.C("business").Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	errBoom := errors.New("rollback")
	err := db.WithTx(ctx, func(tc context.Context) error {
		tx, err := db.Tx(tc)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(tc, `INSERT INTO business(id,doc) VALUES('raw','{}')`); err != nil {
			return err
		}
		if _, err = db.C("business").InsertOne(tc, pgdoc.M{"_id": "doc"}); err != nil {
			return err
		}
		return db.WithTx(tc, func(nested context.Context) error {
			tx2, err := db.Tx(nested)
			if err != nil || tx != tx2 {
				t.Fatalf("nested transaction not joined: %v", err)
			}
			return errBoom
		})
	})
	if !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	if n, err := db.C("business").Count(ctx, nil); err != nil || n != 0 {
		t.Fatalf("raw/document rollback=%d %v", n, err)
	}
}

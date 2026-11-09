//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/billingjob"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/shopspring/decimal"
)

type ratingClock struct{ at time.Time }

func (c ratingClock) Now() time.Time { return c.at }

func TestDurableBillingMonthlyOriginalClockAcrossMonth(t *testing.T) {
	ctx := context.Background()
	var total decimal.Decimal
	for i, at := range []time.Time{time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)} {
		t.Run(at.Format("2006-01-02"), func(t *testing.T) {
			db := freshPG(t)
			s, c, _ := durableBillingFixture(t, db, ratingClock{at})
			if _, err := db.C("pricePlanRule").DeleteMany(ctx, nil); err != nil {
				t.Fatal(err)
			}
			mustInsert(t, db, "pricePlanRule", pricing.PricePlanRule{PricePlanID: "pp", TimeUnit: pricing.TimeUnitMonth, ResourceType: "instance", Prices: []pricing.PricePlanRulePrice{{AttributeName: "vcpus", Tiers: []pricing.PriceTier{{Value: dptr("31")}}}}})
			c, _ = billingjob.NewCycle(c.ProfileID, pricing.TimeUnitMonth, c.CycleAt.Add(24*time.Hour))
			if err := s.ChargeProfileByID(ctx, c.ProfileID, c.TimeUnit, c.CycleAt); err != nil {
				t.Fatal(err)
			}
			var bill pricing.Bill
			found, err := db.C("bill").FindOne(ctx, pgdoc.M{"billingProfileId": c.ProfileID}, &bill)
			if err != nil || !found {
				t.Fatal(err)
			}
			if bill.BillingCycle.StartDate.Month() != time.October || !bill.CalculateTotalAmount().Equal(*dptr("66.1666666666666666")) {
				t.Fatalf("monthly original cycle not prorated: %+v", bill)
			}
			if i == 0 {
				total = bill.CalculateTotalAmount()
			} else if !total.Equal(bill.CalculateTotalAmount()) {
				t.Fatalf("worker clock changed original monthly charge %s vs%s", total, bill.CalculateTotalAmount())
			}
		})
	}
}

func TestDurableBillingFutureDeletionCannotCrossOriginalCutoff(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	s, c, _ := durableBillingFixture(t, db)
	repo := cloud.NewRepo(db)
	var resources []cloud.CloudResource
	if err := db.C("cloudResource").Find(ctx, nil, &resources); err != nil {
		t.Fatal(err)
	}
	deleted := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for i := range resources {
		if err := repo.DeleteAndArchive(ctx, &resources[i], deleted); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ChargeProfileByID(ctx, c.ProfileID, c.TimeUnit, c.CycleAt); err != nil {
		t.Fatal(err)
	}
	assertDurableBill(t, db, c) // ONLY Oct15 10:00->12:00; never through Nov2
	closing := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := s.ChargeProfileByID(ctx, c.ProfileID, c.TimeUnit, closing); err != nil {
		t.Fatal(err)
	}
	var bill pricing.Bill
	if _, err := db.C("bill").FindOne(ctx, pgdoc.M{"billingProfileId": c.ProfileID}, &bill); err != nil {
		t.Fatal(err)
	}
	for _, item := range bill.Items {
		if item.ResourceType == "instance" && item.TimeUnits.HourLastRateTime.After(closing) {
			t.Fatalf("October charged after cycle end: %+v", item)
		}
	}
}

func TestDurableBillingIssuedCycleNeedsReconciliation(t *testing.T) {
	for _, status := range []pricing.BillStatus{pricing.BillStatusSent, pricing.BillStatusPaid} {
		t.Run(string(status), func(t *testing.T) {
			ctx := context.Background()
			db := freshPG(t)
			s, c, _ := durableBillingFixture(t, db)
			start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			end := start.AddDate(0, 1, 0)
			mustInsert(t, db, "bill", pricing.Bill{BillingProfileID: c.ProfileID, Status: status, BillingCycle: &pricing.BillBillingCycle{StartDate: &start, EndDate: &end}, Items: []pricing.BillItem{}})
			for _, cutoff := range []time.Time{c.CycleAt, end} {
				if err := s.ChargeProfileByID(ctx, c.ProfileID, c.TimeUnit, cutoff); !errors.Is(err, pricing.ErrChargeReconciliation) {
					t.Fatalf("issued cycle silently acknowledged %v: %v", cutoff, err)
				}
			}
			if n, _ := db.C("bill").Count(ctx, nil); n != 1 {
				t.Fatalf("issued cycle reopened: %d", n)
			}
			if n, _ := db.C("billingChargeReceipt").Count(ctx, nil); n != 0 {
				t.Fatalf("reconciliation incorrectly receipted: %d", n)
			}
		})
	}
}

func TestDurableBillingInvoiceWaitsForKnownBacklog(t *testing.T) {
	ctx := context.Background()
	db, q := queueFixture(t)
	s, c, _ := durableBillingFixture(t, db)
	if err := s.ChargeProfileByID(ctx, c.ProfileID, c.TimeUnit, c.CycleAt); err != nil {
		t.Fatal(err)
	}
	later, _ := billingjob.NewCycle(c.ProfileID, c.TimeUnit, c.CycleAt.Add(time.Hour))
	body, _ := json.Marshal(later)
	_, err := q.Enqueue(ctx, durablejobs.Billing, later.Key(), body, 2)
	if err != nil {
		t.Fatal(err)
	}
	r := billing.NewRepo(db)
	send := billing.NewBillSendService(r, pricing.NewRepo(db))
	send.SetChargeBacklog(q.BillingBacklogTx)
	profile, err := r.FindByID(ctx, c.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	issueAt := time.Date(2026, 11, 1, 0, 30, 0, 0, time.UTC)
	if ok, err := send.SendBillForProfile(ctx, profile, issueAt); err != nil || ok {
		t.Fatalf("issued with pending backlog=%v %v", ok, err)
	}
	j := claimJob(t, q, time.Minute)
	if ok, err := send.SendBillForProfile(ctx, profile, issueAt); err != nil || ok {
		t.Fatalf("issued with running backlog=%v %v", ok, err)
	}
	if err = q.Retry(ctx, j, 0, true); err != nil {
		t.Fatal(err)
	}
	if ok, err := send.SendBillForProfile(ctx, profile, issueAt); err != nil || ok {
		t.Fatalf("issued with dead backlog=%v %v", ok, err)
	}
	if err = q.Replay(ctx, j.ID, "operator", "repaired transient cause", func(j *durablejobs.Job) error {
		_, err := billingjob.NewCycle(c.ProfileID, c.TimeUnit, later.CycleAt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	retry := claimJob(t, q, time.Minute)
	if err = q.Complete(ctx, retry, func(tc context.Context) error {
		return s.ChargeProfileByID(tc, later.ProfileID, later.TimeUnit, later.CycleAt)
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := send.SendBillForProfile(ctx, profile, issueAt); err != nil || !ok {
		t.Fatalf("did not issue after successful reconciliation=%v %v", ok, err)
	}
	if n, _ := db.C("bill").Count(ctx, pgdoc.M{"billingProfileId": c.ProfileID, "status": "SENT"}); n != 1 {
		t.Fatalf("want exactly one issued bill got%d", n)
	}
}

func TestDurableBillingCanceledPrepareDoesNotPoison(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	r := pricing.NewRepo(db)
	bad, cancel := context.WithCancel(ctx)
	cancel()
	if err := r.PrepareCharges(bad); err == nil {
		t.Fatal("canceled prepare passed")
	}
	if err := r.PrepareCharges(ctx); err != nil {
		t.Fatalf("canceled init cached permanently: %v", err)
	}
}

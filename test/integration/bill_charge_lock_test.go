//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
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
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// lockCycle is a month no real clock is in, so nothing here depends on the day the suite runs.
var (
	lockCycleStart = time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC)
	lockCycleEnd   = lockCycleStart.AddDate(0, 1, 0)
)

func hourRule() pricing.PricePlanRule {
	return pricing.PricePlanRule{
		ID: "rule-hour", TimeUnit: pricing.TimeUnitHour, ResourceType: "compute",
		Prices: []pricing.PricePlanRulePrice{{AttributeName: "vcpu", Tiers: []pricing.PriceTier{{Value: dptr("1")}}}},
	}
}

func computeResource(id string, createdAt time.Time) *pricing.BillingResource {
	return &pricing.BillingResource{
		ResourceID: id, ProjectID: "p", ResourceType: "compute", CreatedAt: &createdAt,
		Values: map[string]any{"vcpu": int64(1)},
		BillingResourceType: &pricing.BillingResourceType{
			ResourceType: "compute",
			Attributes:   []pricing.ResourceAttribute{{Name: "vcpu", Type: "number"}},
		},
	}
}

func billsOf(t *testing.T, db *pgdoc.DB, profileID string) []pricing.Bill {
	t.Helper()
	var out []pricing.Bill
	if err := db.C("bill").Find(context.Background(), pgdoc.M{"billingProfileId": profileID}, &out); err != nil {
		t.Fatalf("list bills: %v", err)
	}
	return out
}

// together runs the functions at the same instant, like two crons on one spec.
func together(fns ...func()) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fn()
		}()
	}
	close(start)
	wg.Wait()
}

// TestChargeHourlyAndMonthlyOpenOneBill is the review's repro: hourlyCharge and monthlyCharge share a
// cron spec and have different ShedLock names, so they meet in one process on every top of hour. It
// made two OPEN bills in 113 of 200 passes.
func TestChargeHourlyAndMonthlyOpenOneBill(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo, eng := pricing.NewRepo(db), pricing.NewEngine(pricing.SystemClock())
	at := lockCycleStart.Add(5 * time.Hour)
	duplicated := 0
	for i := range 200 {
		bp := fmt.Sprintf("bp-%d", i)
		charge := func(unit string) func() {
			return func() {
				rc := pricing.RatingContext{TimeUnit: unit, CycleTimestamp: at}
				if _, err := pricing.ChargeBillingResources(ctx, repo, eng, rc, pricing.BillingContext{}, bp, nil, nil, lockCycleStart, lockCycleEnd, at, "USD", nil); err != nil {
					t.Errorf("charge %s: %v", unit, err)
				}
			}
		}
		together(charge(pricing.TimeUnitHour), charge(pricing.TimeUnitMonth))
		if n := len(billsOf(t, db, bp)); n != 1 {
			duplicated++
		}
	}
	if duplicated != 0 {
		t.Fatalf("%d of 200 passes left more than one OPEN bill", duplicated)
	}
}

// TestChargeDoesNotRebillTheMonth is what the duplicate costs: an item first seen on the second bill
// starts its watermark at the cycle start, so it charges the month to date again. The review saw it in
// 27 of 40 rounds. Sequential passes inside one minute also cover the old lease, which made a pass
// claim the bill by adding a minute to its own clock.
func TestChargeDoesNotRebillTheMonth(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo, eng := pricing.NewRepo(db), pricing.NewEngine(pricing.SystemClock())
	rules := []pricing.PricePlanRule{hourRule()}
	const hours = 24
	wrong := 0
	for i := range 40 {
		bp := fmt.Sprintf("bp-%d", i)
		res := computeResource("vm-"+bp, lockCycleStart.Add(-24*time.Hour))
		pass := func(unit string, at time.Time, r []*pricing.BillingResource) func() {
			return func() {
				rc := pricing.RatingContext{TimeUnit: unit, CycleTimestamp: at}
				if _, err := pricing.ChargeBillingResources(ctx, repo, eng, rc, pricing.BillingContext{}, bp, rules, r, lockCycleStart, lockCycleEnd, at, "USD", nil); err != nil {
					t.Errorf("charge: %v", err)
				}
			}
		}
		first := lockCycleStart.Add(time.Hour)
		together(pass(pricing.TimeUnitHour, first, []*pricing.BillingResource{res}), pass(pricing.TimeUnitMonth, first, nil))
		for h := 2; h <= hours; h++ {
			pass(pricing.TimeUnitHour, lockCycleStart.Add(time.Duration(h)*time.Hour), []*pricing.BillingResource{res})()
		}
		total := decimal.Zero
		for _, b := range billsOf(t, db, bp) {
			for j := range b.Items {
				total = total.Add(b.Items[j].NetAmount)
			}
		}
		if !total.Equal(decimal.NewFromInt(hours)) {
			wrong++
			t.Logf("round %d: billed %s for %d hours", i, total, hours)
		}
	}
	if wrong != 0 {
		t.Fatalf("%d of 40 rounds billed the wrong amount", wrong)
	}
}

// TestChargeLosesNoConcurrentCharge is M4: a second claimant used to get the same bill, and the slower
// save replaced the document with its older copy.
func TestChargeLosesNoConcurrentCharge(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo, eng := pricing.NewRepo(db), pricing.NewEngine(pricing.SystemClock())
	rules := []pricing.PricePlanRule{hourRule()}
	at := lockCycleStart.Add(5 * time.Hour)
	const writers = 16
	for round := range 5 {
		bp := fmt.Sprintf("bp-%d", round)
		fns := make([]func(), writers)
		for w := range writers {
			res := computeResource(fmt.Sprintf("vm-%d", w), lockCycleStart)
			fns[w] = func() {
				rc := pricing.RatingContext{TimeUnit: pricing.TimeUnitHour, CycleTimestamp: at}
				if _, err := pricing.ChargeBillingResources(ctx, repo, eng, rc, pricing.BillingContext{}, bp, rules, []*pricing.BillingResource{res}, lockCycleStart, lockCycleEnd, at, "USD", nil); err != nil {
					t.Errorf("charge: %v", err)
				}
			}
		}
		together(fns...)
		bills := billsOf(t, db, bp)
		items := 0
		for _, b := range bills {
			items += len(b.Items)
		}
		if len(bills) != 1 || items != writers {
			t.Fatalf("round %d: %d bills holding %d items, want 1 holding %d", round, len(bills), items, writers)
		}
	}
}

// rolloverFixture is one ACTIVE profile with one 1-vCPU server charged $1 an hour and an overwrite
// traffic rule at 0.01 per MB, running from 21:00 on the last day of the cycle. October holds 100 MB,
// and November's metrics doc already exists at zero, which is what the metrics job leaves at 00:00.
func rolloverFixture(t *testing.T, db *pgdoc.DB) (profileID string, driver *billingjob.Service) {
	t.Helper()
	ctx := context.Background()
	billingRepo := billing.NewRepo(db)
	metricsRepo := metrics.NewRepo(db)
	cloudRepo := cloud.NewRepo(db)
	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD"})
	profileID = mustInsertID(t, db, "billingProfile", pgdoc.M{
		"status": billing.StatusActive, "currency": "USD",
		"pricePlanConfig": pgdoc.M{"includePublicPricePlans": true},
	})
	orgID := mustInsertID(t, db, "organization", pgdoc.M{"name": "acme", "billingProfileId": profileID})
	projectID := mustInsertID(t, db, "project", pgdoc.M{
		"name": "p1", "status": project.StatusEnabled, "organizationId": orgID,
		"memberships": []any{}, "services": []any{pgdoc.M{"serviceId": "svc-x"}},
	})
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-x", "type": externalservice.TypeCloud, "name": "dev", "config": pgdoc.M{}})
	created := lockCycleEnd.Add(-3 * time.Hour)
	srv, err := cloudRepo.Insert(ctx, &cloud.CloudResource{
		ExternalID: "vm-1", ServiceID: "svc-x", ProjectID: projectID, Type: cloud.TypeServer,
		Data:      map[string]any{"flavorName": "s1", "server": map[string]any{"name": "vm", "status": "ACTIVE", "flavor": map[string]any{"ram": int64(1024), "vcpus": int64(1), "disk": int64(10)}}},
		CreatedAt: &created, UpdatedAt: &created,
	})
	if err != nil {
		t.Fatalf("seed server: %v", err)
	}
	novEnd := lockCycleEnd.AddDate(0, 1, 0)
	for _, m := range []struct {
		start, end time.Time
		mb         string
	}{{lockCycleStart, lockCycleEnd, "100"}, {lockCycleEnd, novEnd, "0"}} {
		start, end := m.start, m.end
		if _, err := metricsRepo.Save(ctx, &metrics.UsageMetrics{
			ResourceID: srv.ID, ResourceType: cloud.TypeServer,
			BillingCycle: &metrics.BillBillingCycle{StartDate: &start, EndDate: &end},
			Details:      &metrics.UsageMetricsDetails{TotalTrafficMb: decimal.RequireFromString(m.mb)},
		}); err != nil {
			t.Fatalf("seed metrics: %v", err)
		}
	}
	mustInsert(t, db, "pricePlan", pricing.PricePlan{ID: "pp", Enabled: true, AccessMode: pricing.AccessPublic})
	mustInsert(t, db, "pricePlanRule", pricing.PricePlanRule{
		PricePlanID: "pp", TimeUnit: pricing.TimeUnitHour, ResourceType: "instance",
		Prices: []pricing.PricePlanRulePrice{{AttributeName: "vcpus", Tiers: []pricing.PriceTier{{From: dptr("0"), Value: dptr("1")}}}},
	})
	mustInsert(t, db, "pricePlanRule", pricing.PricePlanRule{
		PricePlanID: "pp", TimeUnit: pricing.TimeUnitHour, ResourceType: "instance_traffic", ApplyMethod: pricing.ApplyOverwriteTotal,
		Prices: []pricing.PricePlanRulePrice{{AttributeName: "total_traffic_mb", Tiers: []pricing.PriceTier{{From: dptr("0"), Value: dptr("0.01")}}}},
	})
	driver = billingjob.New(billingjob.Deps{
		Billing: billingRepo, ExternalServices: externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("dev-key")),
		Projects: project.NewRepo(db), Orgs: org.NewRepo(db), Pricing: pricing.NewRepo(db),
		Engine: pricing.NewEngine(pricing.SystemClock()), Cloud: cloudRepo,
		Registry: map[string]billingresource.Provider{cloud.TypeServer: billingresource.NewServerProvider(metricsRepo)},
	})
	return profileID, driver
}

// cycleBills splits a profile's bills into those of the two cycles, and fails on more than one per cycle.
func cycleBills(t *testing.T, db *pgdoc.DB, profileID string) (oct, nov *pricing.Bill) {
	t.Helper()
	bills := billsOf(t, db, profileID)
	for i := range bills {
		b := &bills[i]
		pick := &nov
		if b.BillingCycle.StartDate.Before(lockCycleEnd) {
			pick = &oct
		}
		if *pick != nil {
			t.Fatalf("two bills for the cycle starting %s", b.BillingCycle.StartDate)
		}
		*pick = b
	}
	return oct, nov
}

// serverHours returns the hours and the amount of the instance line, zero when the bill or line is absent.
func serverHours(b *pricing.Bill) (int64, decimal.Decimal) {
	if b == nil {
		return 0, decimal.Zero
	}
	for i := range b.Items {
		if b.Items[i].ResourceType == "instance" {
			return *b.Items[i].TimeUnits.Hour, b.Items[i].NetAmount
		}
	}
	return 0, decimal.Zero
}

func trafficAmount(b *pricing.Bill) decimal.Decimal {
	if b != nil {
		for i := range b.Items {
			if b.Items[i].ResourceType == "instance_traffic" {
				return b.Items[i].NetAmount
			}
		}
	}
	return decimal.Zero
}

// TestRolloverBillsLastHourOnce is m2: a VM running 21:00 to 01:00 across the month's end used to pay for
// 3 hours of the 4, because the 00:00 charge opened the new bill with nothing elapsed. The 00:00 pass
// belongs on October's bill and November starts from 00:00, so it is 3 + 1, each hour once. October's
// traffic total must survive that pass too, though November's metrics doc already exists.
func TestRolloverBillsLastHourOnce(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	profileID, driver := rolloverFixture(t, db)
	hour := func(at time.Time) {
		t.Helper()
		if err := driver.Charge(ctx, pricing.TimeUnitHour, at); err != nil {
			t.Fatalf("charge at %s: %v", at, err)
		}
	}
	hour(lockCycleEnd.Add(-2 * time.Hour))
	hour(lockCycleEnd.Add(-time.Hour))
	october, _ := cycleBills(t, db, profileID)
	if !trafficAmount(october).Equal(decimal.NewFromInt(1)) {
		t.Fatalf("fixture: October traffic = %s, want 1", trafficAmount(october))
	}

	// The real clock reads a few seconds past midnight; monthlyCharge fires with it and opens November.
	if err := driver.Charge(ctx, pricing.TimeUnitMonth, lockCycleEnd.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	hour(lockCycleEnd.Add(2 * time.Second))
	october, november := cycleBills(t, db, profileID)
	if h, net := serverHours(october); h != 3 || !net.Equal(decimal.NewFromInt(3)) {
		t.Errorf("October after the closing pass: %d hours, %s; want 3, 3", h, net)
	}
	if !trafficAmount(october).Equal(decimal.NewFromInt(1)) {
		t.Errorf("closing pass moved October's traffic to %s, want 1", trafficAmount(october))
	}
	if h, _ := serverHours(november); h != 0 {
		t.Errorf("November after the closing pass: %d hours, want 0", h)
	}

	// A repeat of the closing pass (a debug trigger, a second replica) changes nothing.
	hour(lockCycleEnd.Add(10 * time.Minute))
	hour(lockCycleEnd.Add(time.Hour))
	october, november = cycleBills(t, db, profileID)
	if h, _ := serverHours(october); h != 3 {
		t.Errorf("October at the end: %d hours, want 3", h)
	}
	if h, net := serverHours(november); h != 1 || !net.Equal(decimal.NewFromInt(1)) {
		t.Errorf("November at the end: %d hours, %s; want 1, 1", h, net)
	}
}

// TestLateClosingChargeOpensNoBill: a closing pass behind the bill send must not open a second October
// bill, whose new items would start at the cycle start and charge the month again.
// The missing closing interval requires visible reconciliation, not a silent
// success; the issued October money and November are unaffected.
func TestLateClosingChargeOpensNoBill(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	profileID, driver := rolloverFixture(t, db)
	if err := driver.Charge(ctx, pricing.TimeUnitHour, lockCycleEnd.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	billingRepo := billing.NewRepo(db)
	profile, err := billingRepo.FindByID(ctx, profileID)
	if err != nil {
		t.Fatal(err)
	}
	send := billing.NewBillSendService(billingRepo, pricing.NewRepo(db))
	if ok, err := send.SendBillForProfile(ctx, profile, lockCycleEnd.Add(30*time.Minute)); err != nil || !ok {
		t.Fatalf("send: ok=%v err=%v", ok, err)
	}
	if err := driver.Charge(ctx, pricing.TimeUnitHour, lockCycleEnd); !errors.Is(err, pricing.ErrChargeReconciliation) {
		t.Fatalf("late issued closing interval must require reconciliation: %v", err)
	}
	if n, err := db.C("bill").Count(ctx, pgdoc.M{"billingProfileId": profileID}); err != nil || n != 1 {
		t.Fatalf("issued month reopened: count=%d err=%v", n, err)
	}
	if n, err := db.C("billingChargeReceipt").Count(ctx, nil); err != nil || n != 0 {
		t.Fatalf("failed closing acquired receipt: count=%d err=%v", n, err)
	}
	october, november := cycleBills(t, db, profileID)
	if october.Status != pricing.BillStatusSent {
		t.Errorf("October status = %s, want SENT", october.Status)
	}
	if h, _ := serverHours(october); h != 2 {
		t.Errorf("October: %d hours, want the 2 charged before the send", h)
	}
	if november != nil {
		t.Errorf("closing pass opened a November bill")
	}
	if err := driver.Charge(ctx, pricing.TimeUnitHour, lockCycleEnd.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, november = cycleBills(t, db, profileID); november == nil {
		t.Fatal("no November bill")
	} else if h, _ := serverHours(november); h != 1 {
		t.Errorf("November: %d hours, want 1", h)
	}
}

// TestSendBillKeepsRacingClosingCharge: the send reads the bill and then replaces it. Without reading it
// again under the lock, a closing charge landing in between is lost. Whichever comes first, one bill
// holds what the charge reported.
func TestSendBillKeepsRacingClosingCharge(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD"})
	repo, eng := pricing.NewRepo(db), pricing.NewEngine(pricing.SystemClock())
	billingRepo := billing.NewRepo(db)
	send := billing.NewBillSendService(billingRepo, repo)
	rules := []pricing.PricePlanRule{hourRule()}
	lastHour := lockCycleEnd.Add(-time.Hour)
	for i := range 30 {
		bp := mustInsertID(t, db, "billingProfile", pgdoc.M{"status": billing.StatusActive, "currency": "USD", "email": "a@example.com"})
		profile, err := billingRepo.FindByID(ctx, bp)
		if err != nil {
			t.Fatal(err)
		}
		res := []*pricing.BillingResource{computeResource("vm", lastHour.Add(-time.Hour))}
		charge := func(at time.Time) (*pricing.Bill, error) {
			rc := pricing.RatingContext{TimeUnit: pricing.TimeUnitHour, CycleTimestamp: at}
			return pricing.ChargeBillingResources(ctx, repo, eng, rc, pricing.BillingContext{}, bp, rules, res, lockCycleStart, lockCycleEnd, at, "USD", nil)
		}
		if _, err := charge(lastHour); err != nil {
			t.Fatal(err)
		}
		var closing *pricing.Bill
		var closingErr error
		var issued bool
		together(
			func() {
				closing, closingErr = charge(lockCycleEnd)
				if closingErr != nil && !errors.Is(closingErr, pricing.ErrChargeReconciliation) {
					t.Errorf("closing charge: %v", closingErr)
				}
			},
			func() {
				var err error
				if issued, err = send.SendBillForProfile(ctx, profile, lockCycleEnd.Add(time.Second)); err != nil {
					t.Errorf("send: %v", err)
				}
			},
		)
		bills := billsOf(t, db, bp)
		if len(bills) != 1 {
			t.Fatalf("round %d: %d bills", i, len(bills))
		}
		if errors.Is(closingErr, pricing.ErrChargeReconciliation) && (!issued || bills[0].Status != pricing.BillStatusSent || closing != nil) {
			t.Fatalf("round %d: reconciliation without sender winning: issued=%v bill=%s closing=%+v", i, issued, bills[0].Status, closing)
		}
		want := bills[0].Items[0].NetAmount
		if closing != nil && !want.Equal(decimal.NewFromInt(2)) {
			t.Fatalf("round %d: the closing charge reported success and the bill holds %s, want 2", i, want)
		}
		if closing == nil && !want.Equal(decimal.NewFromInt(1)) {
			t.Fatalf("round %d: the closing charge was skipped and the bill holds %s, want 1", i, want)
		}
	}
}

// TestSendBillLeavesTheRunningBillOnThe31st: last month was computed as "the 31st of last month", which
// normalises back into this month whenever last month is shorter. The daily send then issued the running
// bill on 31 October, and the next charge opened another that re-billed the month.
func TestSendBillLeavesTheRunningBillOnThe31st(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD"})
	repo, eng := pricing.NewRepo(db), pricing.NewEngine(pricing.SystemClock())
	billingRepo := billing.NewRepo(db)
	bp := mustInsertID(t, db, "billingProfile", pgdoc.M{"status": billing.StatusActive, "currency": "USD", "email": "a@example.com"})
	profile, err := billingRepo.FindByID(ctx, bp)
	if err != nil {
		t.Fatal(err)
	}
	at := lockCycleStart.Add(29 * 24 * time.Hour)
	rc := pricing.RatingContext{TimeUnit: pricing.TimeUnitHour, CycleTimestamp: at}
	res := []*pricing.BillingResource{computeResource("vm", lockCycleStart)}
	if _, err := pricing.ChargeBillingResources(ctx, repo, eng, rc, pricing.BillingContext{}, bp, []pricing.PricePlanRule{hourRule()}, res, lockCycleStart, lockCycleEnd, at, "USD", nil); err != nil {
		t.Fatal(err)
	}
	ok, err := billing.NewBillSendService(billingRepo, repo).SendBillForProfile(ctx, profile, time.Date(2025, 10, 31, 0, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	bills := billsOf(t, db, bp)
	if ok || bills[0].Status != pricing.BillStatusOpen {
		t.Fatalf("31 October issued October's own bill: finalized=%v status=%s", ok, bills[0].Status)
	}
}

// TestAdminBillUpdateKeepsRacingCharge: the admin update replaces the whole document, so outside the lock it
// undoes a charge landing between its read and its write.
func TestAdminBillUpdateKeepsRacingCharge(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo, eng := pricing.NewRepo(db), pricing.NewEngine(pricing.SystemClock())
	const iss, cid = "https://idp.example", "admin-client"
	router := newAdminRouter(db, iss, cid, nil)
	admin := &httpx.RequestContext{Sub: "op-1", Email: "op@example.com", Issuer: iss, Azp: cid}
	rules := []pricing.PricePlanRule{hourRule()}
	res := []*pricing.BillingResource{computeResource("vm", lockCycleStart)}
	const hours = 60
	var bill *pricing.Bill
	for h := 1; h <= hours; h++ {
		at := lockCycleStart.Add(time.Duration(h) * time.Hour)
		rc := pricing.RatingContext{TimeUnit: pricing.TimeUnitHour, CycleTimestamp: at}
		fns := []func(){func() {
			b, err := pricing.ChargeBillingResources(ctx, repo, eng, rc, pricing.BillingContext{}, "bp", rules, res, lockCycleStart, lockCycleEnd, at, "USD", nil)
			if err != nil {
				t.Errorf("charge: %v", err)
			}
			if b != nil {
				bill = b
			}
		}}
		if bill != nil {
			id := bill.ID
			for range 8 { // several, so one is mid-way when the charge lands
				fns = append(fns, func() {
					w := doAdmin(router, admin, "PUT", "/admin/bill/"+id, `{"status":"OPEN","invoiceCurrency":"USD","billingProfileId":"bp"}`)
					if w.Code != 200 {
						t.Errorf("update: %d %s", w.Code, w.Body)
					}
				})
			}
		}
		together(fns...)
	}
	bills := billsOf(t, db, "bp")
	if len(bills) != 1 {
		t.Fatalf("%d bills", len(bills))
	}
	if h, got := *bills[0].Items[0].TimeUnits.Hour, bills[0].Items[0].NetAmount; h != hours || !got.Equal(decimal.NewFromInt(hours)) {
		t.Fatalf("bill holds %d hours, %s; want %d", h, got, hours)
	}
}

// docQuery returns the SQL block following a marker in docs/billing.md, so the operator's queries are the ones tested.
func docQuery(t *testing.T, marker string) string {
	t.Helper()
	raw, err := os.ReadFile("../../docs/billing.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(raw), "<!-- "+marker+" -->\n```sql\n")
	if !ok {
		t.Fatalf("no %s block in docs/billing.md", marker)
	}
	sql, _, _ := strings.Cut(rest, "```")
	return sql
}

// TestDuplicateBillQueries runs the documented queries: empty duplicates go, one bill per group stays, a pair
// holding money on both sides is left for a person, and an issued bill is never touched.
func TestDuplicateBillQueries(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	cycle := pgdoc.M{"startDate": lockCycleStart, "endDate": lockCycleEnd}
	seed := func(bp, status string, items []any, adjustments []any) string {
		return mustInsertID(t, db, "bill", pgdoc.M{"billingProfileId": bp, "status": status, "billingCycle": cycle, "items": items, "adjustments": adjustments})
	}
	money := []any{pgdoc.M{"resourceId": "vm", "netAmount": decimalOf(t, "12.50")}}
	zero := []any{pgdoc.M{"resourceId": "vm", "netAmount": decimalOf(t, "0")}}
	adjustment := []any{pgdoc.M{"amount": decimalOf(t, "-1")}}

	keep := []string{
		seed("single", "OPEN", nil, nil),
		seed("with-money", "OPEN", money, nil),
		seed("both-empty", "OPEN", []any{}, nil),
		seed("human", "OPEN", money, nil), seed("human", "OPEN", money, nil),
		seed("adjusted", "OPEN", []any{}, adjustment), seed("adjusted", "OPEN", money, nil),
		seed("issued", "SENT", zero, nil), seed("issued", "OPEN", money, nil),
	}
	gone := []string{
		seed("with-money", "OPEN", zero, nil),
		seed("both-empty", "OPEN", nil, nil),
	}

	listed := func() map[string]bool {
		rows, err := db.Pool.Query(ctx, docQuery(t, "duplicate-bills-list"))
		if err != nil {
			t.Fatalf("list query: %v", err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var profile, c string
			var n int64
			var detail any
			if err := rows.Scan(&profile, &c, &n, &detail); err != nil {
				t.Fatal(err)
			}
			out[profile] = true
		}
		return out
	}
	if got := listed(); len(got) != 5 || !got["with-money"] || !got["both-empty"] || !got["human"] || !got["adjusted"] || !got["issued"] {
		t.Fatalf("list before: %v", got)
	}
	if _, err := db.Pool.Exec(ctx, docQuery(t, "duplicate-bills-delete")); err != nil {
		t.Fatalf("delete query: %v", err)
	}
	for _, id := range keep {
		if found, err := db.C("bill").Exists(ctx, pgdoc.M{"_id": id}); err != nil || !found {
			t.Errorf("bill %s was deleted (%v)", id, err)
		}
	}
	for _, id := range gone {
		if found, err := db.C("bill").Exists(ctx, pgdoc.M{"_id": id}); err != nil || found {
			t.Errorf("bill %s should be gone (%v)", id, err)
		}
	}
	if got := listed(); len(got) != 3 || !got["human"] || !got["adjusted"] || !got["issued"] {
		t.Fatalf("list after: %v", got)
	}
}

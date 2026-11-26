//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
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

var updateBillingEquality = flag.Bool("update", false, "rewrite the recorded bills of TestBillingEquality")

const billingEqualityGolden = "testdata/billing_equality.golden.txt"

// TestBillingEquality is the guard of the billing engine: one profile with a row of every registered
// billable type is charged through a script of passes, and after EVERY pass every bill of the profile
// must equal the recorded one, to the last decimal of each applied amount, each counter and each
// watermark. The record was written from main before the engine learned running intervals
// (ChargeFrom, VariableRate, EarlierInterval), so a change that moves an existing item by a cent, or
// moves a watermark while the money happens to agree, fails here.
//
// The golden is rewritten only when billing is MEANT to change:
//
//	go test -tags=integration ./test/integration -run TestBillingEquality -update
//
// and the diff of testdata/billing_equality.golden.txt is then the change to review. A new billable
// type adds a row to the fixture (the test refuses a catalog type without an item), and its golden
// diff only adds lines.
func TestBillingEquality(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	// October has 31 days: the default minute and hour limits (43200, 720) are those of 30.
	day := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	at := func(h, m, s int) time.Time {
		return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
	}
	stamp := func(tm time.Time) string { return tm.Format(time.RFC3339) }

	cloudRepo := cloud.NewRepo(db)
	metricsRepo := metrics.NewRepo(db)
	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD"})
	profileID := mustInsertID(t, db, "billingProfile", pgdoc.M{
		"status": billing.StatusActive, "currency": "USD", "pricePlanConfig": pgdoc.M{"includePublicPricePlans": true},
	})
	orgID := mustInsertID(t, db, "organization", pgdoc.M{"name": "acme", "billingProfileId": profileID})
	projectID := mustInsertID(t, db, "project", pgdoc.M{
		"name": "p1", "status": project.StatusEnabled, "organizationId": orgID,
		"memberships": []any{}, "services": []any{pgdoc.M{"serviceId": "svc-pve"}},
	})
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-pve", "type": externalservice.TypeCloud, "name": "pve", "config": pgdoc.M{"provider": "proxmox"}})

	// Rows: every type of the charge job's registry (cmd/api/main.go), so every catalog type gets an item.
	names := map[string]string{} // row id → fixture name
	put := func(name, typ string, created *time.Time, data map[string]any) *cloud.CloudResource {
		t.Helper()
		res, err := cloudRepo.Insert(ctx, &cloud.CloudResource{
			ExternalID: name, ServiceID: "svc-pve", ProjectID: projectID, Type: typ, Region: "r1",
			Data: data, CreatedAt: created, UpdatedAt: created,
		})
		if err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		names[res.ID] = name
		return res
	}
	tp := func(tm time.Time) *time.Time { return &tm }
	server := func(name, status string, extra map[string]any) map[string]any {
		s := map[string]any{"name": name, "status": status, "flavor": map[string]any{"ram": 4096, "vcpus": 2, "disk": 20}}
		for k, v := range extra {
			s[k] = v
		}
		return map[string]any{"flavorName": "m2", "server": s}
	}
	vmActive := put("vm-active", cloud.TypeServer, tp(at(8, 13, 27)), server("vm-active", "ACTIVE", map[string]any{
		"protection": map[string]any{"state": "active", "mode": "replica", "since": stamp(at(8, 40, 10)), "diskGb": 20},
		"snapshots":  map[string]any{"diskGb": 20, "items": []any{map[string]any{"name": "snap1", "createdAt": stamp(at(9, 5, 42))}}},
	}))
	put("vm-build", cloud.TypeServer, tp(at(9, 2, 11)), server("vm-build", "BUILD", nil)) // not eligible, back-billed later
	put("vm-nomark", cloud.TypeServer, nil, server("vm-nomark", "ACTIVE", nil))            // no createdAt: a nil watermark
	// Created before the cycle and first rated on the 31st: the units stop at the limit, the net at the cap.
	put("vol-old", cloud.TypeVolume, tp(time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)), map[string]any{"volume": map[string]any{"name": "vol-old", "size": 100, "volume_type": "std", "status": "in-use"}})
	volArchived := put("vol-archived", cloud.TypeVolume, tp(at(9, 10, 5)), map[string]any{"volume": map[string]any{"name": "vol-archived", "size": 10, "volume_type": "std", "status": "available"}})
	put("fip", cloud.TypeFloatingIP, tp(at(8, 30, 0)), map[string]any{"floatingIp": map[string]any{"status": "ACTIVE", "floating_ip_address": "203.0.113.7"}})
	put("img", cloud.TypeImage, tp(at(7, 59, 59)), map[string]any{"image": map[string]any{"name": "img", "status": "active", "size": 5<<30 + 1}})
	put("k8s", cloud.TypeKubernetesCluster, tp(at(6, 45, 30)), map[string]any{"cluster": map[string]any{"name": "k8s", "version": "1.36", "status": "READY", "cp_replicas": 3, "endpoint": "https://10.0.0.5:6443", "node_groups": []any{map[string]any{"name": "a"}}}})
	put("db", cloud.TypeDatabaseCluster, tp(at(6, 50, 0)), map[string]any{"database": map[string]any{"name": "db", "engine": "postgresql", "version": "17", "status": "READY", "replicas": 2, "cpu": 2, "memory_gib": 4, "storage_gib": 20, "endpoint": "10.0.0.9"}})
	put("bkt", cloud.TypeBucket, tp(at(5, 0, 1)), map[string]any{"bucketName": "bkt", "sizeInBytes": 3 << 30, "objectCount": 12})
	put("shr", cloud.TypeShare, tp(at(5, 30, 0)), map[string]any{"share": map[string]any{"name": "shr", "protocol": "nfs", "usedBytes": 1 << 30, "sizeBytes": 10 << 30}})
	// A running sandbox with no timeout: with one, the provider reads the wall clock.
	put("sbx-run", cloud.TypeSandbox, tp(at(9, 40, 50)), map[string]any{"name": "sbx-run", "status": "RUNNING", "runningSince": stamp(at(9, 41, 7)), "cpu": 2, "memoryGiB": 4, "template": "base"})
	sbxEnded := put("sbx-ended", cloud.TypeSandbox, tp(at(9, 19, 58)), map[string]any{"name": "sbx-ended", "status": "DELETED", "runningSince": stamp(at(9, 20, 7)), "endedAt": stamp(at(9, 24, 46)), "cpu": 1, "memoryGiB": 2, "template": "base"})
	if err := cloudRepo.DeleteAndArchive(ctx, sbxEnded, at(9, 26, 30)); err != nil {
		t.Fatal(err)
	}
	put("sbx-snap", cloud.TypeSandboxSnapshot, tp(at(9, 25, 0)), map[string]any{"name": "sbx-snap", "kind": "pause", "sizeGB": 2, "createdAt": stamp(at(9, 25, 0))})

	// Traffic of vm-active: the month's total, and the next month's doc, which the closing pass must not read.
	october, november := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	traffic := func(cycle time.Time, id, mb string) string {
		t.Helper()
		end := cycle.AddDate(0, 1, 0)
		m, err := metricsRepo.Save(ctx, &metrics.UsageMetrics{
			ID: id, ResourceID: vmActive.ID, ResourceType: cloud.TypeServer,
			BillingCycle: &metrics.BillBillingCycle{StartDate: &cycle, EndDate: &end},
			Details:      &metrics.UsageMetricsDetails{TotalTrafficMb: decimal.RequireFromString(mb)},
		})
		if err != nil {
			t.Fatalf("seed traffic: %v", err)
		}
		return m.ID
	}
	octoberTraffic := traffic(october, "", "1234.5")
	traffic(november, "", "7")

	// Rules: a minute, an hour and a month rule for every catalog type, each price a different prime
	// (so a unit that moved from one rule to another cannot cancel out), on `existence` plus one of the
	// type's own attributes where it has one; and a usage total (OVERWRITE_TOTAL) on the traffic.
	mustInsert(t, db, "pricePlan", pricing.PricePlan{ID: "pp", Enabled: true, AccessMode: pricing.AccessPublic})
	primes := []int64{2, 3, 5, 7, 11, 13, 17, 19, 23, 29, 31, 37, 41, 43, 47, 53, 59, 61, 67, 71, 73, 79, 83, 89, 97, 101, 103, 107, 109, 113, 127, 131, 137, 139, 149, 151, 157, 163, 167, 173, 179, 181, 191, 193, 197, 199, 211, 223, 227, 229, 233, 239, 241, 251, 257, 263, 269, 271, 277, 281, 283, 293, 307, 311, 313, 317, 331, 337, 347, 349, 353, 359, 367, 373, 379, 383, 389, 397}
	next := 0
	price := func(exp int32) *decimal.Decimal {
		d := decimal.New(primes[next], exp)
		next++
		return &d
	}
	own := map[string]string{
		"instance": "vcpus", "volume": "size", "image": "size_gb", "kubernetes_cluster": "cp_replicas",
		"database_cluster": "vcpus_total", "bucket": "size_gb", "share": "quota_gb", "server_protection": "size_gb",
		"server_snapshot": "size_gb", "sandbox": "vcpus", "sandbox_snapshot": "size_gb",
	}
	catalog := billingresource.Catalog()
	for _, typ := range catalog {
		// minute: primes × 0.0001, hour: × 0.01, month: × 1. Minute and hour nets stay under a month
		// rule's cap for a resource a day old, and the pre-cycle volume crosses all three caps.
		for _, u := range []struct {
			unit string
			exp  int32
		}{{pricing.TimeUnitMinute, -4}, {pricing.TimeUnitHour, -2}, {pricing.TimeUnitMonth, 0}} {
			prices := []pricing.PricePlanRulePrice{{AttributeName: "existence", Tiers: []pricing.PriceTier{{Value: price(u.exp)}}}}
			if attr := own[typ.ResourceType]; attr != "" {
				prices = append(prices, pricing.PricePlanRulePrice{AttributeName: attr, Tiers: []pricing.PriceTier{{Value: price(u.exp - 1)}}})
			}
			mustInsert(t, db, "pricePlanRule", pricing.PricePlanRule{
				ID: "rule-" + typ.ResourceType + "-" + u.unit, PricePlanID: "pp", TimeUnit: u.unit, ResourceType: typ.ResourceType, Prices: prices,
			})
		}
	}
	// 18 decimals: the amount is rounded to 16 first, then multiplied by the units.
	mustInsert(t, db, "pricePlanRule", pricing.PricePlanRule{
		ID: "rule-floating_ip-minute-18dp", PricePlanID: "pp", TimeUnit: pricing.TimeUnitMinute, ResourceType: "floating_ip",
		Prices: []pricing.PricePlanRulePrice{{AttributeName: "existence", Tiers: []pricing.PriceTier{{Value: dptr("0.000401234567890155")}}}},
	})
	mustInsert(t, db, "pricePlanRule", pricing.PricePlanRule{
		ID: "rule-instance_traffic-usage", PricePlanID: "pp", TimeUnit: pricing.TimeUnitHour, ResourceType: "instance_traffic", ApplyMethod: pricing.ApplyOverwriteTotal,
		Prices: []pricing.PricePlanRulePrice{{AttributeName: "total_traffic_mb", Tiers: []pricing.PriceTier{{Value: dptr("0.0409")}}}},
	})

	driver := billingjob.New(billingjob.Deps{
		Billing: billing.NewRepo(db), ExternalServices: externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("k")),
		Projects: project.NewRepo(db), Orgs: org.NewRepo(db), Pricing: pricing.NewRepo(db),
		Engine: pricing.NewEngine(pricing.SystemClock()), Cloud: cloudRepo,
		Registry: map[string]billingresource.Provider{ // as cmd/api/main.go
			cloud.TypeServer:            billingresource.NewServerProvider(metricsRepo),
			cloud.TypeVolume:            billingresource.NewVolumeProvider(),
			cloud.TypeFloatingIP:        billingresource.NewFloatingIPProvider(),
			cloud.TypeImage:             billingresource.NewImageProvider(),
			cloud.TypeKubernetesCluster: billingresource.NewKubernetesClusterProvider(),
			cloud.TypeDatabaseCluster:   billingresource.NewDatabaseClusterProvider(),
			cloud.TypeBucket:            billingresource.NewBucketProvider(),
			cloud.TypeShare:             billingresource.NewShareProvider(),
			cloud.TypeSandbox:           billingresource.NewSandboxProvider(),
			cloud.TypeSandboxSnapshot:   billingresource.NewSandboxSnapshotProvider(),
		},
	})

	var got strings.Builder
	n := 0
	// pass runs one charge and records every bill. The provider's heartbeat is a minute old unless
	// the pass is meant to find it stale.
	pass := func(unit string, now time.Time, note string, heartbeatAge time.Duration) {
		t.Helper()
		if err := cloudRepo.SetMeterFields(ctx, "svc-pve", pgdoc.M{"lastOkAt": now.Add(-heartbeatAge)}); err != nil {
			t.Fatal(err)
		}
		if err := driver.Charge(ctx, unit, now); err != nil {
			t.Fatalf("pass %d (%s %s): %v", n+1, unit, stamp(now), err)
		}
		n++
		fmt.Fprintf(&got, "== pass %02d: %s at %s (%s)\n", n, unit, stamp(now), note)
		got.WriteString(dumpBills(t, db, profileID, names))
	}
	const fresh, stale = time.Minute, 31 * time.Minute
	minute, hour, month := pricing.TimeUnitMinute, pricing.TimeUnitHour, pricing.TimeUnitMonth

	pass(minute, at(10, 0, 7), "the first pass of every row", fresh)
	pass(minute, at(10, 0, 31), "the same minute again", fresh)
	pass(hour, at(10, 0, 12), "the first hour pass", fresh)
	pass(month, at(10, 0, 15), "the first month pass", fresh)
	pass(month, at(11, 0, 15), "the month pass an hour later: the cap gate", fresh)
	pass(minute, at(10, 7, 3), "a later minute", fresh)
	pass(minute, at(10, 3, 3), "an earlier minute, out of order", fresh)

	put("vm-build", cloud.TypeServer, tp(at(9, 2, 11)), server("vm-build", "ACTIVE", nil)) // now eligible: billed back
	traffic(october, octoberTraffic, "2000.25")
	pass(minute, at(10, 12, 5), "the BUILD server came up; more traffic", fresh)
	pass(hour, at(11, 0, 9), "the usage total is replaced", fresh)

	pass(minute, at(11, 20, 4), "the heartbeat is 31 minutes old: written off", stale)
	pass(hour, at(12, 0, 4), "stale on an hour pass", stale)
	pass(minute, at(12, 2, 4), "after recovery", fresh)

	if err := cloudRepo.DeleteAndArchive(ctx, volArchived, at(12, 5, 40)); err != nil { // off the minute and the hour
		t.Fatal(err)
	}
	pass(minute, at(12, 6, 2), "a volume was archived at 12:05:40", fresh)
	pass(minute, at(12, 7, 2), "the archived row again", fresh)
	pass(hour, at(13, 0, 2), "the archived row on an hour pass", fresh)
	pass(hour, at(14, 0, 2), "and again", fresh)
	pass(minute, at(23, 59, 2), "the last minute pass inside the month", fresh)

	pass(minute, at(24, 0, 3), "the closing minute pass: 00:00 on the 1st bills October", fresh)
	pass(hour, at(24, 0, 6), "the closing hour pass", fresh)
	pass(minute, at(24, 1, 3), "the first minute pass of November", fresh)
	pass(hour, at(25, 0, 6), "the first hour pass of November", fresh)
	pass(month, at(25, 0, 9), "the month pass of November", fresh)
	pass(minute, at(25, 2, 3), "November goes on", fresh)

	// Every catalog type has an item on the October bill: a type added later needs a row here.
	for _, typ := range catalog {
		if !strings.Contains(got.String(), "\n  "+typ.ResourceType+" ") {
			t.Errorf("no bill item of type %s: the fixture must have a row for every billable type", typ.ResourceType)
		}
	}

	if *updateBillingEquality {
		if err := os.WriteFile(billingEqualityGolden, []byte(got.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %s", billingEqualityGolden)
		return
	}
	want, err := os.ReadFile(billingEqualityGolden)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		gl, wl := strings.Split(got.String(), "\n"), strings.Split(string(want), "\n")
		pass := ""
		for i := 0; i < len(gl) && i < len(wl); i++ {
			if strings.HasPrefix(wl[i], "== ") {
				pass = wl[i]
			}
			if gl[i] != wl[i] {
				t.Fatalf("the bills moved (line %d, %s)\n recorded: %s\n      now: %s", i+1, pass, wl[i], gl[i])
			}
		}
		t.Fatalf("the bills moved: %d lines now, %d recorded", len(gl), len(wl))
	}
}

// dumpBills writes every bill of the profile in a canonical form: money as exact decimal strings,
// items sorted by type and fixture name, generated ids replaced by fixture names. An item's
// createdAt/updatedAt and its metadata (a copy of the provider's values) are left out.
func dumpBills(t *testing.T, db *pgdoc.DB, profileID string, names map[string]string) string {
	t.Helper()
	var bills []pricing.Bill
	if err := db.C("bill").Find(context.Background(), pgdoc.M{"billingProfileId": profileID}, &bills, pgdoc.Sort(pgdoc.AscK("billingCycle.startDate", pgdoc.KTime), pgdoc.Asc("_id"))); err != nil {
		t.Fatal(err)
	}
	named := func(s string) string {
		for id, name := range names {
			s = strings.ReplaceAll(s, id, name)
		}
		return s
	}
	ts := func(p *time.Time) string {
		if p == nil {
			return "-"
		}
		return p.UTC().Format(time.RFC3339Nano)
	}
	var b strings.Builder
	for i := range bills {
		bill := &bills[i]
		fmt.Fprintf(&b, "bill %s..%s %s total=%s items=%d\n", ts(bill.BillingCycle.StartDate), ts(bill.BillingCycle.EndDate), bill.Status, bill.CalculateTotalAmount().String(), len(bill.Items))
		lines := make([]string, 0, len(bill.Items))
		for j := range bill.Items {
			it := &bill.Items[j]
			var l strings.Builder
			fmt.Fprintf(&l, "  %s %s name=%q net=%s minute=%d hour=%d month=%d marks=%s|%s|%s\n", it.ResourceType, named(it.ResourceID), it.Name, it.NetAmount.String(),
				*it.TimeUnits.Minute, *it.TimeUnits.Hour, *it.TimeUnits.Month, ts(it.TimeUnits.MinuteLastRateTime), ts(it.TimeUnits.HourLastRateTime), ts(it.TimeUnits.MonthLastRateTime))
			for _, r := range it.AppliedPricePlanRules {
				for _, a := range r.AppliedAmounts {
					last, err := json.Marshal(a.LastAttributeValue)
					if err != nil {
						t.Fatal(err)
					}
					fmt.Fprintf(&l, "    %s %s=%s last=%s\n", r.PricePlanRuleID, a.AttributeName, a.NetAmount.String(), last)
				}
			}
			lines = append(lines, l.String())
		}
		sort.Strings(lines)
		b.WriteString(strings.Join(lines, ""))
	}
	return b.String()
}

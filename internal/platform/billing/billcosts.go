package billing

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// CloudResource `type` discriminator values, mirrored from internal/cloud's CloudResourceType
// constants. Inlined here on purpose: the billing domain must stay free of any internal/cloud
// import so it can be extracted into the standalone billing service. These are the stable wire
// values the FE resource-name/created helpers read (data.<key>) — keep them in sync with
// internal/cloud/resource.go if the cloud resource types ever change.
const (
	cloudTypeServer       = "SERVER"
	cloudTypeVolume       = "VOLUME"
	cloudTypeFloatingIP   = "FLOATING_IP"
	cloudTypeLoadBalancer = "LOAD_BALANCER"
	cloudTypeBucket       = "BUCKET"
)

// MonthlyBillCosts sums a profile's bill-item net amounts into the current-month and previous-month
// buckets, keyed by each bill's billing-cycle start month (currentMonth/
// lastMonth — the same per-month bill-net aggregation the client cost-info dashboard uses). The
// forecast is left equal to current by the caller (the live prorated re-rate is deferred).
func MonthlyBillCosts(bills []pricing.Bill, now time.Time) (current, last decimal.Decimal) {
	current, last = decimal.Zero, decimal.Zero
	curYear, curMonth, _ := now.Date()
	lastT := now.AddDate(0, -1, 0)
	lastYear, lastMonth, _ := lastT.Date()
	for i := range bills {
		b := &bills[i]
		cycleStart := now
		if b.BillingCycle != nil && b.BillingCycle.StartDate != nil {
			cycleStart = b.BillingCycle.StartDate.UTC()
		}
		y, m, _ := cycleStart.Date()
		for j := range b.Items {
			switch {
			case y == curYear && m == curMonth:
				current = current.Add(b.Items[j].NetAmount)
			case y == lastYear && m == lastMonth:
				last = last.Add(b.Items[j].NetAmount)
			}
		}
	}
	return current, last
}

// CreatedAtLookup returns a cloud resource's creation time by id (nil when unknown). Used to stamp
// the topResourcePrices CREATED column; pass nil to omit it.
type CreatedAtLookup func(resourceID string) *time.Time

// BillCostBreakdown aggregates a profile's bills into the dashboard cost overview shared by the
// client project dashboard and the admin billing-profile detail (the usage-overview
// cost fields): the current/previous-month net, the
// cost-by-category maps (currentMonthCostsByType / lastMonthCostsByType — the admin costs-comparison
// chart reads both), and the per-item topResourcePrices list (each entry mirrors a CloudResource so
// the FE name/created helpers resolve). Forecast is left equal to current by the caller (prorate
// deferred).
func BillCostBreakdown(bills []pricing.Bill, now time.Time, createdAt CreatedAtLookup) (current, last decimal.Decimal, byType, lastByType map[string]any, top []any) {
	current, last = decimal.Zero, decimal.Zero
	byCat := map[string]decimal.Decimal{}
	lastByCat := map[string]decimal.Decimal{}
	// topResourcePrices is aggregated per underlying resource, not per bill item: a server's
	// instance_traffic cost merges into the server itself (keyed by the stripped id) so it isn't a
	// duplicate row, and the list is sorted by total cost so "top" actually means top spenders.
	agg := map[string]*topEntry{}
	order := []string{}
	curYear, curMonth, _ := now.Date()
	lastMonthTime := now.AddDate(0, -1, 0)
	lastYear, lastMonth, _ := lastMonthTime.Date()
	for i := range bills {
		b := &bills[i]
		cycleStart := now
		if b.BillingCycle != nil && b.BillingCycle.StartDate != nil {
			cycleStart = b.BillingCycle.StartDate.UTC()
		}
		y, m, _ := cycleStart.Date()
		switch {
		case y == curYear && m == curMonth:
			for j := range b.Items {
				it := &b.Items[j]
				current = current.Add(it.NetAmount)
				cat := resourceBillingCategory(it.ResourceType)
				byCat[cat] = byCat[cat].Add(it.NetAmount)
				id := canonicalResourceID(it.ResourceType, it.ResourceID)
				e := agg[id]
				if e == nil {
					e = &topEntry{id: id}
					agg[id] = e
					order = append(order, id)
				}
				e.cost = e.cost.Add(it.NetAmount)
				// Prefer the primary resource's metadata (the instance) over its traffic line, but
				// either maps to the server so a traffic-only group still names correctly.
				if it.ResourceType != "instance_traffic" || e.name == "" {
					e.cloudType, e.dataKey = billingResourceCloudShape(it.ResourceType)
					e.name = it.Name
				}
			}
		case y == lastYear && m == lastMonth:
			for j := range b.Items {
				it := &b.Items[j]
				last = last.Add(it.NetAmount)
				cat := resourceBillingCategory(it.ResourceType)
				lastByCat[cat] = lastByCat[cat].Add(it.NetAmount)
			}
		}
	}
	// Sort by total cost desc (stable → equal-cost items keep first-seen order).
	sort.SliceStable(order, func(i, j int) bool { return agg[order[i]].cost.GreaterThan(agg[order[j]].cost) })
	top = make([]any, 0, len(order))
	for _, id := range order {
		e := agg[id]
		// resource mirrors a CloudResource (the FE name helper reads data.<key>.name; the created
		// helper reads resource.createdAt → the CREATED column).
		res := map[string]any{
			"id": e.id, "type": e.cloudType, "name": e.name,
			"data": map[string]any{e.dataKey: map[string]any{"id": e.id, "name": e.name}},
		}
		if createdAt != nil {
			if ts := createdAt(e.id); ts != nil {
				res["createdAt"] = ts.UTC().Format(time.RFC3339)
			}
		}
		top = append(top, map[string]any{
			"resource":       res,
			"currentCost":    json.Number(e.cost.String()),
			"forecastedCost": json.Number(e.cost.String()),
		})
	}
	byType = decimalCatMap(byCat)
	lastByType = decimalCatMap(lastByCat)
	return current, last, byType, lastByType, top
}

// topEntry accumulates one resource's total cost across its bill items (e.g. an instance plus its
// instance_traffic) for the topResourcePrices list.
type topEntry struct {
	cost      decimal.Decimal
	id        string
	name      string
	cloudType string
	dataKey   string
}

// canonicalResourceID collapses a bill item's resource id to the underlying resource: an
// instance_traffic line (id "instance_traffic-<serverId>") folds into its server so the two don't
// show as separate rows. Everything else is its own id.
func canonicalResourceID(resourceType, resourceID string) string {
	if resourceType == "instance_traffic" {
		return strings.TrimPrefix(resourceID, "instance_traffic-")
	}
	return resourceID
}

// CostInfoMap renders a breakdown (current/last + by-type maps + topResourcePrices) into the CostInfo
// envelope every field present (all fields non-null) shared by the profile-level and the
// per-project cost info. Forecast = current (live prorate deferred).
func CostInfoMap(cur, last decimal.Decimal, byType, lastByType map[string]any, top []any) map[string]any {
	return map[string]any{
		"lastMonthCosts":                json.Number(last.String()),
		"currentMonthCosts":             json.Number(cur.String()),
		"currentMonthCostsByType":       byType,
		"forecastedMonthEndCostsByType": byType,
		"lastMonthCostsByType":          lastByType,
		"forecastedMonthEndCosts":       json.Number(cur.String()),
		"topResourcePrices":             top,
	}
}

// ProjectCostInfoMap builds the per-project CostInfo (the admin profile-detail
// per-project drill-down): group the profile's bill items by `projectId` and run the cost breakdown
// over each project's items → {projectId: CostInfo}. Items carry projectId (SaveChargingToBill sets
// BillItem.ProjectID); an item with no projectId is skipped. Never nil.
func ProjectCostInfoMap(bills []pricing.Bill, now time.Time, createdAt CreatedAtLookup) map[string]any {
	order := []string{}
	seen := map[string]bool{}
	for i := range bills {
		for j := range bills[i].Items {
			if p := bills[i].Items[j].ProjectID; p != "" && !seen[p] {
				seen[p] = true
				order = append(order, p)
			}
		}
	}
	out := map[string]any{}
	for _, pid := range order {
		sub := make([]pricing.Bill, 0, len(bills))
		for i := range bills {
			b := bills[i] // struct copy; reassigning b.Items below leaves the input slice untouched
			items := make([]pricing.BillItem, 0, len(b.Items))
			for j := range b.Items {
				if b.Items[j].ProjectID == pid {
					items = append(items, b.Items[j])
				}
			}
			if len(items) == 0 {
				continue
			}
			b.Items = items
			sub = append(sub, b)
		}
		c, l, bt, lbt, tp := BillCostBreakdown(sub, now, createdAt)
		out[pid] = CostInfoMap(c, l, bt, lbt, tp)
	}
	return out
}

// decimalCatMap renders a category→decimal map as category→json.Number (money as a JSON number).
func decimalCatMap(in map[string]decimal.Decimal) map[string]any {
	out := map[string]any{}
	for cat, amt := range in {
		out[cat] = json.Number(amt.String())
	}
	return out
}

// billingResourceCloudShape maps a billing-resource type (instance/volume/…) to the CloudResource
// {type, data-key} the FE's resource-name helper reads (data.<key>.name). Keeps the type as-is when
// unmapped.
func billingResourceCloudShape(billingType string) (cloudType, dataKey string) {
	switch billingType {
	case "instance", "instance_traffic":
		return cloudTypeServer, "server"
	case "volume":
		return cloudTypeVolume, "volume"
	case "floating_ip":
		return cloudTypeFloatingIP, "floatingIp"
	case "load_balancer":
		return cloudTypeLoadBalancer, "loadBalancer"
	case "bucket":
		return cloudTypeBucket, "bucket"
	default:
		return billingType, billingType
	}
}

// resourceBillingCategory maps a resourceType → the
// dashboard cost category. Default Compute (the fallback).
func resourceBillingCategory(resourceType string) string {
	switch resourceType {
	case "network", "port", "floating_ip", "router", "subnet", "security_group", "load_balancer":
		return "Networking"
	case "volume", "volume_snapshot", "volume_backup":
		return "Block Storage"
	case "bucket", "object_store":
		return "Object Storage"
	default:
		return "Compute"
	}
}

package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"golang.org/x/sync/singleflight"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// forecast.go projects the month-end cost: the current month's bill so far plus what the charge job
// will still add before the month closes, for the resources it bills right now.
//
// The rating is the charge job's own. The Forecaster (billingjob.Service) resolves the profile's
// billable resources exactly as a charge pass does (same ENABLED projects, providers, eligibility,
// price-plan rules, time-unit limits) and rates each one with the same Engine. This file turns those
// rates into the projection with pricing.ProjectedAccrual, which follows the bill-item accrual
// (per-unit rounding, time-unit limit, item cap). Nothing here prices anything by itself.
//
// Semantics, all following the charge job:
//   - The month is the billing cycle: UTC calendar months (billingjob.monthBounds). There is no
//     per-profile time zone; every profile's bill runs from the 1st 00:00 UTC to the next 1st 00:00
//     UTC, and the pass at that instant closes the month.
//   - The charge bills every clock unit a resource exists in, counted from its bill item's watermark
//     (its last charge). So the units still to bill are ceil((month end − watermark) / unit). Right
//     after the hourly pass that is the whole hours left in the month; between passes it includes
//     the hour in progress, which the next pass bills. A month rule adds its amount once if it has
//     not been charged this month (it normally has: it is paid up front).
//   - Only what keeps accruing is projected: resources of ENABLED projects under an ACTIVE profile,
//     eligible for billing now (a server in BUILD/ERROR/DELETED is not), up to their own end (a
//     deleted row's last interval, a sandbox's shutdown).
//   - Usage totals (OVERWRITE_TOTAL rules, e.g. traffic) are held at their month-to-date value.
//   - The current cost is the sum of bill-item nets: bill adjustments (savings contracts,
//     price-adjustment rules) and tax are not in it, so they are not in the forecast either.
//
// What cannot be projected keeps its current cost, and the forecast says so (Approximate, with the
// reasons and the unrated resource types) rather than passing a guess off as a projection.

// Reasons a forecast is approximate.
const (
	// ForecastUnrated: the price-plan rules could not rate some billable resource (the charge would
	// fail on it too); its cost is held at what it has accrued.
	ForecastUnrated = "unrated"
	// ForecastUsage: a usage total (traffic) is held at its month-to-date value.
	ForecastUsage = "usage"
	// ForecastStale: a provider's metering is stale; the charge job bills none of its resources and
	// writes that time off until it recovers, so they are held at their current cost (not projected).
	ForecastStale = "stale"
	// ForecastUnavailable: no projection could be made (resolution failed, timed out, too many
	// resources, or panicked); the forecast is the current cost.
	ForecastUnavailable = "unavailable"
	// ForecastExternalBilling: an external billing service rates the bills
	// (PROXCLOUD_BILLING_PROVIDER=external); the projection uses this repository's price plans and
	// engine, which that service may not follow.
	ForecastExternalBilling = "external_billing"
)

// ForecastMaxResources caps how many billable resources one forecast resolves; a profile above it
// gets no projection ("unavailable") rather than an unbounded cost-info request.
const ForecastMaxResources = 2000

// ErrForecastTooLarge is returned by a Forecaster when the profile has more than
// ForecastMaxResources billable resources.
var ErrForecastTooLarge = errors.New("forecast: too many billable resources")

// Forecaster resolves and rates a profile's billable resources the way the charge job does.
// billingjob.Service implements it.
type Forecaster interface {
	ForecastInput(ctx context.Context, profileID string, now time.Time) (ForecastInput, error)
}

// ForecastRate is one billable resource's rating for one time unit, from the charge job's rules.
type ForecastRate struct {
	TimeUnit string
	Results  []pricing.PricePlanRuleResult
}

// ForecastResource is a resource the charge job bills right now, with its rates per time unit. Its
// bill item, if any, is found by ResourceID + ResourceType (the key SaveChargingToBill uses).
type ForecastResource struct {
	ResourceID   string
	ResourceType string
	ProjectID    string
	// CreatedAt is where its first charge starts when it has no bill item yet.
	CreatedAt *time.Time
	// EndsAt is when it stops billing (a deleted row, a sandbox's end), nil when it runs on.
	EndsAt *time.Time
	Rates  []ForecastRate
}

// ForecastUnratedResource is a billable resource the rules could not rate.
type ForecastUnratedResource struct {
	ResourceType string
	ProjectID    string
}

// ForecastInput is what a profile's forecast needs from the charge job's resolution.
type ForecastInput struct {
	Billing   pricing.BillingContext
	Resources []ForecastResource
	Unrated   []ForecastUnratedResource
	// Stale is set when a provider the profile uses has stale metering.
	Stale bool
	// ExternalBilling is set when an external billing service rates the bills.
	ExternalBilling bool
}

// Forecast is a projected month end: the projection (what is still to be added, on top of the
// current cost) for the profile, per cost category (the categories currentMonthCostsByType uses),
// per project and per resource (keyed by the topResourcePrices id).
type Forecast struct {
	Projected    decimal.Decimal
	ByCategory   map[string]decimal.Decimal
	ByProject    map[string]decimal.Decimal
	ByProjectCat map[string]map[string]decimal.Decimal
	ByResource   map[string]decimal.Decimal
	// scope ("" = applies to every project) → reason → true / unrated resource type → true.
	reasons map[string]map[string]bool
	unrated map[string]map[string]bool
}

func newForecast() *Forecast {
	return &Forecast{
		Projected:    decimal.Zero,
		ByCategory:   map[string]decimal.Decimal{},
		ByProject:    map[string]decimal.Decimal{},
		ByProjectCat: map[string]map[string]decimal.Decimal{},
		ByResource:   map[string]decimal.Decimal{},
		reasons:      map[string]map[string]bool{},
		unrated:      map[string]map[string]bool{},
	}
}

func mark(m map[string]map[string]bool, scope, key string) {
	if m[scope] == nil {
		m[scope] = map[string]bool{}
	}
	m[scope][key] = true
}

// collect gathers the keys of a scope (projectID "" = the profile: every scope; a project: its own
// and the profile-wide ones), sorted. Never nil.
func collect(m map[string]map[string]bool, projectID string) []string {
	out := []string{}
	seen := map[string]bool{}
	for scope, keys := range m {
		if projectID != "" && scope != projectID && scope != "" {
			continue
		}
		for k := range keys {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ApproximateReasons lists why the forecast for a scope (projectID "" = the profile) is approximate,
// empty when it is a full projection. A nil forecast (none could be made) is "unavailable".
func (f *Forecast) ApproximateReasons(projectID string) []string {
	if f == nil {
		return []string{ForecastUnavailable}
	}
	return collect(f.reasons, projectID)
}

// UnratedTypes lists the billing resource types the rules could not rate in a scope, sorted.
func (f *Forecast) UnratedTypes(projectID string) []string {
	if f == nil {
		return []string{}
	}
	return collect(f.unrated, projectID)
}

// MonthEndForecast resolves and projects a profile's month end. A nil forecaster gives nil (no
// projection: the cost info then reports the current cost, approximate). A resolution error
// (including a timeout or the resource cap) or a panic keeps every current cost and marks the
// forecast unavailable: a cost-info read never fails because of its forecast.
func MonthEndForecast(ctx context.Context, fc Forecaster, profileID string, bills []pricing.Bill, now time.Time) (f *Forecast) {
	if fc == nil || profileID == "" {
		return nil
	}
	unavailable := func() *Forecast {
		u := newForecast()
		mark(u.reasons, "", ForecastUnavailable)
		return u
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("month-end forecast panicked", "billingProfile", profileID, "panic", r)
			f = unavailable()
		}
	}()
	in, err := fc.ForecastInput(ctx, profileID, now)
	if err != nil {
		if !errors.Is(err, ErrForecastTooLarge) {
			slog.Warn("month-end forecast unavailable", "billingProfile", profileID, "err", err)
		}
		return unavailable()
	}
	return ProjectMonthEnd(bills, in, now)
}

// forecastCacheTTL is how long a resolved forecast input is reused for a profile.
const forecastCacheTTL = 60 * time.Second

// forecastResolveTimeout bounds one resolution, detached from the request so a canceled caller
// does not fail the resolution every concurrent caller is waiting on.
const forecastResolveTimeout = 3 * time.Second

// forecastCacheMax bounds the cache; past it, expired entries are swept and, if still full, the
// new entry is simply not stored.
const forecastCacheMax = 1024

// CachedForecaster wraps a Forecaster for the request path: a profile's resolved input is cached
// for forecastCacheTTL per (profile, UTC billing month), concurrent callers share one resolution
// (singleflight), and a resolution is bounded by forecastResolveTimeout. Only the resolution is
// cached: ProjectMonthEnd still runs per request on fresh bills and the request's own clock, so
// the projection moves with time between refreshes. Errors are not cached.
type CachedForecaster struct {
	inner Forecaster
	// external marks every forecast approximate (ForecastExternalBilling).
	external bool
	now      func() time.Time
	ttl      time.Duration
	timeout  time.Duration

	mu    sync.Mutex
	cache map[forecastKey]forecastEntry
	sf    singleflight.Group
}

type forecastKey struct {
	profileID string
	month     time.Time
}

type forecastEntry struct {
	in      ForecastInput
	expires time.Time
}

// NewCachedForecaster wraps inner. external = an external billing service rates the bills.
func NewCachedForecaster(inner Forecaster, external bool) *CachedForecaster {
	return &CachedForecaster{
		inner: inner, external: external, now: func() time.Time { return time.Now().UTC() },
		ttl: forecastCacheTTL, timeout: forecastResolveTimeout, cache: map[forecastKey]forecastEntry{},
	}
}

// ForecastInput returns the cached input for (profile, the billing month of `now`) or resolves it.
func (c *CachedForecaster) ForecastInput(ctx context.Context, profileID string, now time.Time) (ForecastInput, error) {
	month, _ := billingMonth(now)
	key := forecastKey{profileID, month}
	if in, ok := c.get(key); ok {
		return in, nil
	}
	v, err, _ := c.sf.Do(profileID+"|"+month.Format(time.RFC3339), func() (any, error) {
		if in, ok := c.get(key); ok { // filled while this caller waited for the flight
			return in, nil
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
		defer cancel()
		in, err := c.resolve(rctx, profileID, now)
		if err != nil {
			return ForecastInput{}, err
		}
		in.ExternalBilling = in.ExternalBilling || c.external
		c.put(key, in)
		return in, nil
	})
	if err != nil {
		return ForecastInput{}, err
	}
	return v.(ForecastInput), nil
}

// resolve runs the inner resolution, turning a panic into an error so singleflight's waiters get
// an answer and the caller falls back to "unavailable".
func (c *CachedForecaster) resolve(ctx context.Context, profileID string, now time.Time) (in ForecastInput, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("forecast resolution panicked: %v", r)
		}
	}()
	in, err = c.inner.ForecastInput(ctx, profileID, now)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err() // finished past the deadline: do not cache a possibly partial input
	}
	return in, err
}

func (c *CachedForecaster) get(key forecastKey) (ForecastInput, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if !ok || !c.now().Before(e.expires) {
		return ForecastInput{}, false
	}
	return e.in, true
}

func (c *CachedForecaster) put(key forecastKey, in ForecastInput) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.cache) >= forecastCacheMax {
		for k, e := range c.cache {
			if !now.Before(e.expires) {
				delete(c.cache, k)
			}
		}
		if len(c.cache) >= forecastCacheMax {
			return
		}
	}
	c.cache[key] = forecastEntry{in: in, expires: now.Add(c.ttl)}
}

// ProjectMonthEnd projects each resource and time unit from its watermark to the end of the current
// billing month (or its own end), against its bill item on the month's OPEN bill.
func ProjectMonthEnd(bills []pricing.Bill, in ForecastInput, now time.Time) *Forecast {
	f := newForecast()
	now = now.UTC()
	monthStart, monthEnd := billingMonth(now)
	items := openBillItems(bills, monthStart, monthEnd)
	for i := range in.Resources {
		res := &in.Resources[i]
		if res.CreatedAt != nil && res.CreatedAt.After(now) {
			continue // the charge skips a resource younger than its pass
		}
		end := monthEnd
		if res.EndsAt != nil && res.EndsAt.Before(end) {
			end = res.EndsAt.UTC()
		}
		item := items[itemKey{res.ResourceID, res.ResourceType}]
		net := decimal.Zero
		if item != nil {
			net = item.NetAmount
		}
		added := decimal.Zero
		for _, rate := range sortedRates(res.Rates) {
			units := unitsLeft(rate.TimeUnit, item, res.CreatedAt, monthStart, now, end)
			add, held := pricing.ProjectedAccrual(in.Billing, rate.TimeUnit, rate.Results, item, net.Add(added), units)
			if held {
				mark(f.reasons, res.ProjectID, ForecastUsage)
			}
			added = added.Add(add)
		}
		if added.IsZero() {
			continue
		}
		cat := resourceBillingCategory(res.ResourceType)
		f.Projected = f.Projected.Add(added)
		f.ByCategory[cat] = f.ByCategory[cat].Add(added)
		if res.ProjectID != "" {
			f.ByProject[res.ProjectID] = f.ByProject[res.ProjectID].Add(added)
			pc := f.ByProjectCat[res.ProjectID]
			if pc == nil {
				pc = map[string]decimal.Decimal{}
				f.ByProjectCat[res.ProjectID] = pc
			}
			pc[cat] = pc[cat].Add(added)
		}
		id := canonicalResourceID(res.ResourceType, res.ResourceID)
		f.ByResource[id] = f.ByResource[id].Add(added)
	}
	for _, u := range in.Unrated {
		mark(f.reasons, u.ProjectID, ForecastUnrated)
		mark(f.unrated, u.ProjectID, u.ResourceType)
	}
	if in.Stale {
		mark(f.reasons, "", ForecastStale)
	}
	if in.ExternalBilling {
		mark(f.reasons, "", ForecastExternalBilling)
	}
	return f
}

// ApplyForecast sets a CostInfo map's forecast fields (a map built by CostInfoMap) from the
// forecast for a scope (projectID "" = the profile):
//   - forecastedMonthEndCosts = currentMonthCosts + the projection;
//   - forecastedMonthEndCostsByType = currentMonthCostsByType + the projection per category, so the
//     categories always add up to the total;
//   - each topResourcePrices entry's forecastedCost = its currentCost + its own projection;
//   - forecastApproximate / forecastApproximateReasons / forecastUnratedTypes, so a client can say
//     what kind of number it shows.
//
// A nil forecast leaves the forecast at the current cost, approximate ("unavailable").
func ApplyForecast(ci map[string]any, f *Forecast, projectID string) {
	if ci == nil {
		return
	}
	projected := decimal.Zero
	byCat := map[string]decimal.Decimal{}
	if f != nil {
		if projectID == "" {
			projected, byCat = f.Projected, f.ByCategory
		} else {
			projected = f.ByProject[projectID]
			if pc := f.ByProjectCat[projectID]; pc != nil {
				byCat = pc
			}
		}
	}
	ci["forecastedMonthEndCosts"] = json.Number(numberOf(ci["currentMonthCosts"]).Add(projected).String())
	forecastByType := map[string]any{}
	if cur, ok := ci["currentMonthCostsByType"].(map[string]any); ok {
		for cat, v := range cur {
			forecastByType[cat] = json.Number(numberOf(v).String())
		}
	}
	for cat, add := range byCat {
		forecastByType[cat] = json.Number(numberOf(forecastByType[cat]).Add(add).String())
	}
	ci["forecastedMonthEndCostsByType"] = forecastByType
	if top, ok := ci["topResourcePrices"].([]any); ok && f != nil {
		for _, e := range top {
			entry, ok := e.(map[string]any)
			if !ok {
				continue
			}
			res, _ := entry["resource"].(map[string]any)
			id, _ := res["id"].(string)
			if add, ok := f.ByResource[id]; ok {
				entry["forecastedCost"] = json.Number(numberOf(entry["currentCost"]).Add(add).String())
			}
		}
	}
	reasons := f.ApproximateReasons(projectID)
	ci["forecastApproximate"] = len(reasons) > 0
	ci["forecastApproximateReasons"] = reasons
	ci["forecastUnratedTypes"] = f.UnratedTypes(projectID)
}

// ApplyForecastToProjects applies the forecast to a {projectId: CostInfo} map (ProjectCostInfoMap's
// output), first adding a zero CostInfo for every project that has a projection but nothing billed
// yet this month (a project whose first VM has not seen its first hourly pass).
func ApplyForecastToProjects(projects map[string]any, f *Forecast) {
	if projects == nil {
		return
	}
	if f != nil {
		for pid := range f.ByProject {
			if _, ok := projects[pid]; !ok {
				projects[pid] = CostInfoMap(decimal.Zero, decimal.Zero, map[string]any{}, map[string]any{}, []any{})
			}
		}
	}
	for pid, v := range projects {
		if ci, ok := v.(map[string]any); ok {
			ApplyForecast(ci, f, pid)
		}
	}
}

// numberOf reads a money value of a CostInfo map (json.Number; anything else is zero).
func numberOf(v any) decimal.Decimal {
	if n, ok := v.(json.Number); ok {
		if d, err := decimal.NewFromString(string(n)); err == nil {
			return d
		}
	}
	return decimal.Zero
}

// billingMonth is the billing cycle `now` falls in: [1st 00:00 UTC, next 1st 00:00 UTC), the charge
// job's cycle (billingjob.monthBounds).
func billingMonth(now time.Time) (time.Time, time.Time) {
	y, m, _ := now.UTC().Date()
	start := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// unitsLeft counts the units of timeUnit the charge job still bills for a resource before end. Each
// pass adds getTimeUnitsDiff = ceil((pass − watermark) / unit) and moves the watermark to the pass,
// so the passes still to come add ceil((end − watermark) / unit) together. Without a bill item, its
// first charge starts at the resource's creation (not before the month start), or, without that, at
// the unit in progress. A month rule has one unit while the resource has not ended
// (ProjectedAccrual drops it when it has been charged this month already).
func unitsLeft(timeUnit string, item *pricing.BillItem, createdAt *time.Time, monthStart, now, end time.Time) int64 {
	var unit time.Duration
	var watermark *time.Time
	switch timeUnit {
	case pricing.TimeUnitMonth:
		if !end.After(now) {
			return 0
		}
		return 1
	case pricing.TimeUnitMinute:
		unit = time.Minute
		if item != nil && item.TimeUnits != nil {
			watermark = item.TimeUnits.MinuteLastRateTime
		}
	case pricing.TimeUnitHour:
		unit = time.Hour
		if item != nil && item.TimeUnits != nil {
			watermark = item.TimeUnits.HourLastRateTime
		}
	default:
		return 0
	}
	last := now.Truncate(unit)
	switch {
	case watermark != nil:
		last = watermark.UTC()
	case createdAt != nil:
		last = createdAt.UTC()
	}
	if last.Before(monthStart) {
		last = monthStart
	}
	if !end.After(last) {
		return 0
	}
	elapsed := end.Sub(last)
	n := int64(elapsed / unit)
	if elapsed%unit > 0 {
		n++
	}
	return n
}

type itemKey struct{ id, typ string }

// openBillItems indexes the items of the month's OPEN bill by (resourceId, resourceType). The charge
// writes to the oldest OPEN bill of the cycle (pricing.Repo.openBill sorts by _id; ids are
// time-ordered), so that is the one read here.
func openBillItems(bills []pricing.Bill, monthStart, monthEnd time.Time) map[itemKey]*pricing.BillItem {
	var open *pricing.Bill
	for i := range bills {
		b := &bills[i]
		if b.Status != pricing.BillStatusOpen || b.BillingCycle == nil || b.BillingCycle.StartDate == nil {
			continue
		}
		s := b.BillingCycle.StartDate.UTC()
		if s.Before(monthStart) || !s.Before(monthEnd) {
			continue
		}
		if open == nil || b.ID < open.ID {
			open = b
		}
	}
	out := map[itemKey]*pricing.BillItem{}
	if open == nil {
		return out
	}
	for j := range open.Items {
		it := &open.Items[j]
		out[itemKey{it.ResourceID, it.ResourceType}] = it
	}
	return out
}

// sortedRates merges a resource's rates per time unit (a charge pass rates all of a unit's rules
// together, and the item cap is over all of them) and orders them month → hour → minute, so that when
// the cap binds the larger units fill it first (without a binding cap the order does not matter).
func sortedRates(in []ForecastRate) []ForecastRate {
	rank := map[string]int{pricing.TimeUnitMonth: 0, pricing.TimeUnitHour: 1, pricing.TimeUnitMinute: 2}
	out := []ForecastRate{}
	at := map[string]int{}
	for _, r := range in {
		if i, ok := at[r.TimeUnit]; ok {
			out[i].Results = append(out[i].Results, r.Results...)
			continue
		}
		at[r.TimeUnit] = len(out)
		out = append(out, ForecastRate{TimeUnit: r.TimeUnit, Results: append([]pricing.PricePlanRuleResult(nil), r.Results...)})
	}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].TimeUnit] < rank[out[j].TimeUnit] })
	return out
}

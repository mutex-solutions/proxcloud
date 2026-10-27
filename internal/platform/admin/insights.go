package admin

import (
	"context"
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// insights.go computes getAdminInsights — the admin Dashboard charts:
//   - MRR (Paid Bills): PAID bills (billingCycle.startDate in the 13-month window), grouped by the
//     bill's createdAt month + item currency, summing item netAmounts.
//   - MRR (Inbound Payments): SUCCESS account-credit + collect transactions in the window, grouped by
//     createdAt month + currency, summing grossAmounts.
//   - New Users / New Billing Profiles: daily creation counts over the last 30 days.
// Each series is normalized to ALL points in its range (zero-filled), so the FE plots a continuous line.

// MRRDetails is {year, month, total: {currency → amount}}.
type MRRDetails struct {
	Year  int                    `json:"year"`
	Month int                    `json:"month"`
	Total map[string]json.Number `json:"total"`
}

// DailyCountDetails is {year, month, day, count}.
type DailyCountDetails struct {
	Year  int `json:"year"`
	Month int `json:"month"`
	Day   int `json:"day"`
	Count int `json:"count"`
}

type ym struct{ y, m int }

// buildInsights computes the full AdminInsights (headline numbers + the 4 chart series).
func (h *Handler) buildInsights(ctx context.Context) AdminInsights {
	now := time.Now().UTC()
	curY, curM, _ := now.Date()
	firstOfCur := time.Date(curY, curM, 1, 0, 0, 0, 0, time.UTC)
	start := firstOfCur.AddDate(0, -13, 0) // 13 months back (the x-axis bucket origin)
	// The MRR MATCH window is [getLastDayOfCurrentMonth() − 13mo, getLastDayOfCurrentMonth()],
	// inclusive end-of-month (getLastDayOfCurrentMonth = last day at end-of-day) — NOT
	// [firstOfMonth − 13mo, firstOfNextMonth). The difference is the oldest bucket's lower edge: a bill
	// whose billingCycle.startDate is the 1st of the month 13 months ago (e.g. May 1) is BEFORE
	// lastDay−13mo (May 30) → excluded (its x-axis point stays zero), while the naive
	// firstOfMonth−13mo bound would wrongly count it. The x-axis points (`start`→firstOfCur) are
	// unchanged (getMonthlyBeginningsBetween firstDayOfMonth's the same span).
	lastOfMonth := firstOfCur.AddDate(0, 1, 0).Add(-time.Nanosecond) // getLastDayOfCurrentMonth (end-of-day)
	matchStart := lastOfMonth.AddDate(0, -13, 0)                     // endDate.minus(Period.ofMonths(13))
	matchEnd := lastOfMonth
	thirty := now.AddDate(0, 0, -30).Truncate(24 * time.Hour)

	// --- MRR: paid bills (bucket by createdAt month + item currency) ---
	billBuckets := map[ym]map[string]decimal.Decimal{}
	var currentCosts decimal.Decimal
	if bills, err := h.billing.AllBills(ctx); err == nil {
		for i := range bills {
			b := &bills[i]
			if b.Status != pricing.BillStatusPaid {
				continue
			}
			cs := billCycleStart(b, now)
			if cs.Before(matchStart) || cs.After(matchEnd) {
				continue
			}
			at := now
			if b.CreatedAt != nil {
				at = b.CreatedAt.UTC()
			}
			y, m, _ := at.Date()
			for j := range b.Items {
				cur := b.Items[j].Currency
				if cur == "" {
					cur = b.InvoiceCurrency
				}
				addBucket(billBuckets, ym{y, int(m)}, cur, b.Items[j].NetAmount)
				if y == curY && m == curM {
					currentCosts = currentCosts.Add(b.Items[j].NetAmount)
				}
			}
		}
	}

	// --- MRR: inbound payments (account-credit + collect, SUCCESS, by createdAt month + currency) ---
	payBuckets := map[ym]map[string]decimal.Decimal{}
	var currentPayments decimal.Decimal
	addPay := func(at *time.Time, status, currency string, gross *decimal.Decimal) {
		if status != "SUCCESS" || at == nil || gross == nil {
			return
		}
		t := at.UTC()
		if t.Before(matchStart) || t.After(matchEnd) {
			return
		}
		y, m, _ := t.Date()
		addBucket(payBuckets, ym{y, int(m)}, currency, *gross)
		if y == curY && m == curM {
			currentPayments = currentPayments.Add(*gross)
		}
	}
	if txns, err := h.billing.AllAccountCreditTransactions(ctx); err == nil {
		for i := range txns {
			addPay(txns[i].CreatedAt, txns[i].Status, txns[i].Currency, txns[i].GrossAmount)
		}
	}
	if txns, err := h.billing.AllCollectTransactions(ctx); err == nil {
		for i := range txns {
			addPay(txns[i].CreatedAt, string(txns[i].Status), txns[i].Currency, txns[i].GrossAmount)
		}
	}

	points := monthlyPoints(start, firstOfCur)
	ccF, _ := currentCosts.Float64()
	cpF, _ := currentPayments.Float64()
	return AdminInsights{
		CurrentMonthCosts:    ccF,
		CurrentMonthPayments: cpF,
		Bills:                normalizeMRR(billBuckets, points),
		Payments:             normalizeMRR(payBuckets, points),
		NewUsers:             dailyCounts(h.collectTimestamps(ctx, "users", "createdAt", pgdoc.M{"createdAt": pgdoc.M{"$gte": thirty}}), thirty, now),
		NewBillingProfiles:   dailyCounts(h.collectTimestamps(ctx, "billingProfile", "activatedAt", pgdoc.M{"status": "ACTIVE", "activatedAt": pgdoc.M{"$gte": thirty}}), thirty, now),
	}
}

// collectTimestamps returns the `field` timestamps of the matching docs in a collection.
func (h *Handler) collectTimestamps(ctx context.Context, collection, field string, filter pgdoc.M) []time.Time {
	docs, err := h.repo.ListRawFiltered(ctx, collection, filter)
	if err != nil {
		return nil
	}
	out := make([]time.Time, 0, len(docs))
	for _, d := range docs {
		if t, ok := asTime(d[field]); ok {
			out = append(out, t.UTC())
		}
	}
	return out
}

func addBucket(buckets map[ym]map[string]decimal.Decimal, k ym, currency string, amt decimal.Decimal) {
	if currency == "" {
		currency = "USD"
	}
	if buckets[k] == nil {
		buckets[k] = map[string]decimal.Decimal{}
	}
	buckets[k][currency] = buckets[k][currency].Add(amt)
}

// normalizeMRR fills every monthly point (zero-filled empty totals) from the buckets.
func normalizeMRR(buckets map[ym]map[string]decimal.Decimal, points []time.Time) []MRRDetails {
	out := make([]MRRDetails, 0, len(points))
	for _, p := range points {
		y, m, _ := p.Date()
		total := map[string]json.Number{}
		for cur, amt := range buckets[ym{y, int(m)}] {
			total[cur] = json.Number(amt.String())
		}
		out = append(out, MRRDetails{Year: y, Month: int(m), Total: total})
	}
	return out
}

// dailyCounts buckets timestamps by day + fills every day in [from, to].
func dailyCounts(times []time.Time, from, to time.Time) []DailyCountDetails {
	type ymd struct{ y, m, d int }
	counts := map[ymd]int{}
	for _, t := range times {
		y, m, d := t.Date()
		counts[ymd{y, int(m), d}]++
	}
	out := []DailyCountDetails{}
	for day := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC); !day.After(to); day = day.AddDate(0, 0, 1) {
		y, m, d := day.Date()
		out = append(out, DailyCountDetails{Year: y, Month: int(m), Day: d, Count: counts[ymd{y, int(m), d}]})
	}
	return out
}

// monthlyPoints returns the first-of-month for each month in [start, end] inclusive.
func monthlyPoints(start, end time.Time) []time.Time {
	out := []time.Time{}
	for p := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC); !p.After(end); p = p.AddDate(0, 1, 0) {
		out = append(out, p)
	}
	return out
}

func billCycleStart(b *pricing.Bill, fallback time.Time) time.Time {
	if b.BillingCycle != nil && b.BillingCycle.StartDate != nil {
		return b.BillingCycle.StartDate.UTC()
	}
	return fallback
}

// asTime coerces a stored time value (RFC3339 string or time.Time) to time.Time.
func asTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, true
	case *time.Time:
		if t != nil {
			return *t, true
		}
	case string:
		if tt, err := time.Parse(time.RFC3339, t); err == nil {
			return tt, true
		}
	}
	return time.Time{}, false
}

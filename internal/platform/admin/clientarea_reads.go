package admin

// clientarea_reads.go serves the admin "Client Area" LIST/detail reads that return rich,
// joined rows (not the bare domain): the project list (project + organization +
// usedVcpus/Ram/BlockStorage), the bill list (bill + billing profile), the bill financial
// overview (recomputed net/gross/unpaid), and the cloud-resource list (resource + project,
// reduced projection). Without these the admin tables render rows missing their joined/computed
// columns. Money is emitted as JSON numbers, not the quoted decimal strings a
// raw document passthrough produces.

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/paging"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/shopspring/decimal"
)

// shapeDeep recursively shapes a decoded value to its API JSON: every sub-doc gets _id→id +
// _class dropped, and every decimal.Decimal (money) becomes a json.Number so it serializes UNQUOTED as
// a plain number (decimal.Decimal.MarshalJSON quotes it otherwise). Dates (RFC3339 strings) and
// string ids already marshal to the wire shapes, so they pass through. Mutates maps/slices in place.
func shapeDeep(v any) any {
	switch t := v.(type) {
	case pgdoc.M:
		delete(t, "_class")
		for k, val := range t {
			t[k] = shapeDeep(val)
		}
		if id, ok := t["_id"]; ok {
			t["id"] = id
			delete(t, "_id")
		}
		return t
	case []any:
		for i := range t {
			t[i] = shapeDeep(t[i])
		}
		return t
	case decimal.Decimal:
		return json.Number(t.String())
	default:
		return v
	}
}

// decodeTyped re-decodes a raw pgdoc.M into a typed struct THROUGH the pgdoc codec (the same codec
// the store uses), so money fields (decimal strings in jsonb) land in their decimal.Decimal targets.
func decodeTyped(doc pgdoc.M, out any) error {
	body, id, err := pgdoc.Marshal(doc)
	if err != nil {
		return err
	}
	return pgdoc.Unmarshal(body, id, out)
}

// ListRawSorted is ListRaw with a single-field sort (the admin list reads sort createdAt/_id DESC).
func (r *Repo) ListRawSorted(ctx context.Context, collection, field string, dir int) ([]pgdoc.M, error) {
	out := []pgdoc.M{}
	if err := r.c(collection).Find(ctx, nil, &out, pgdoc.Sort(sortKeyFor(field, dir))); err != nil {
		return nil, err
	}
	return out, nil
}

// ListRawSortedPage is the offset-paged variant of ListRawSorted (page window + total). Paginating
// here also bounds the per-row N+1 hydration in the admin list handlers to one page.
func (r *Repo) ListRawSortedPage(ctx context.Context, collection, field string, dir int, p paging.Params) ([]pgdoc.M, int64, error) {
	return paging.Offset[pgdoc.M](ctx, r.c(collection), pgdoc.M{}, []pgdoc.SortKey{sortKeyFor(field, dir)}, p)
}

// projectAdminList returns every project enriched for the admin table: the project doc + the
// joined `organization` + usedVcpus/usedRam/usedBlockStorage (computed cloud usage — 0 with no
// live metrics), sorted createdAt DESC. NB despite the name, only organization is joined here
// (billingProfile is NOT populated).
func (h *Handler) projectAdminList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:project:read") {
		return
	}
	pg, ok := paging.FromRequest(w, r)
	if !ok {
		return
	}
	projects, total, err := h.listRawSortedMaybePaged(r.Context(), "project", "createdAt", -1, pg)
	if httpx.WriteError(w, err) {
		return
	}
	out := make([]pgdoc.M, 0, len(projects))
	for _, p := range projects {
		orgID, _ := p["organizationId"].(string)
		sd, _ := shapeDeep(p).(pgdoc.M)
		if orgID != "" {
			if org, err := h.repo.OrgFindByID(r.Context(), orgID); err == nil && org != nil {
				sd["organization"] = shapeDeep(org)
			}
		}
		sd["usedVcpus"] = 0
		sd["usedRam"] = 0
		sd["usedBlockStorage"] = 0
		out = append(out, sd)
	}
	emitAdminList(w, pg, out, total)
}

// listRawSortedMaybePaged returns one page (+ total) when pg is active, else the full sorted list
// (total = len) — the shared read behind the admin client-area list handlers. emitAdminList picks
// the matching envelope.
func (h *Handler) listRawSortedMaybePaged(ctx context.Context, collection, field string, dir int, pg paging.Params) ([]pgdoc.M, int64, error) {
	if pg.Active {
		return h.repo.ListRawSortedPage(ctx, collection, field, dir, pg)
	}
	rows, err := h.repo.ListRawSorted(ctx, collection, field, dir)
	return rows, int64(len(rows)), err
}

// listRawFilteredSortedMaybePaged is listRawSortedMaybePaged with a WHERE filter (server-side
// facet filtering, e.g. the admin cloud-resource type dropdown) — one page (+ total) when active,
// else the full filtered list.
func (h *Handler) listRawFilteredSortedMaybePaged(ctx context.Context, collection string, filter pgdoc.M, field string, dir int, pg paging.Params) ([]pgdoc.M, int64, error) {
	if pg.Active {
		return paging.Offset[pgdoc.M](ctx, h.repo.c(collection), filter, []pgdoc.SortKey{sortKeyFor(field, dir)}, pg)
	}
	rows := []pgdoc.M{}
	if err := h.repo.c(collection).Find(ctx, filter, &rows, pgdoc.Sort(sortKeyFor(field, dir))); err != nil {
		return nil, 0, err
	}
	return rows, int64(len(rows)), nil
}

// emitAdminList writes the offset envelope (data + paging{limit,offset,total}) when paging is
// active, else the plain list — keeping un-migrated admin pages byte-compatible.
func emitAdminList(w http.ResponseWriter, pg paging.Params, out []pgdoc.M, total int64) {
	if pg.Active {
		httpx.Page(w, out, paging.OffsetPaging(pg, total))
		return
	}
	httpx.List(w, out)
}

// billAdminList returns every bill for the admin table: the bill doc (money as numbers) + the
// joined `billingProfile`, sorted createdAt DESC.
func (h *Handler) billAdminList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:bill:read") {
		return
	}
	pg, ok := paging.FromRequest(w, r)
	if !ok {
		return
	}
	bills, total, err := h.listRawSortedMaybePaged(r.Context(), "bill", "createdAt", -1, pg)
	if httpx.WriteError(w, err) {
		return
	}
	out := make([]pgdoc.M, 0, len(bills))
	for _, b := range bills {
		bpID, _ := b["billingProfileId"].(string)
		sd, _ := shapeDeep(b).(pgdoc.M)
		if bpID != "" {
			if bp, err := h.repo.BillingProfileByIDRaw(r.Context(), bpID); err == nil && bp != nil {
				sd["billingProfile"] = shapeDeep(bp)
			}
		}
		out = append(out, sd)
	}
	emitAdminList(w, pg, out, total)
}

// billingProfileAdminList returns every billing profile for the admin table: the profile doc +
// the 6 computed financials (balance/accountCredit/promotionalCredit via the balance layer;
// currentMonth/lastMonth from the bills; forecastedMonthEnd = currentMonth, flagged
// forecastApproximate — the projection is per profile, in its cost info). Sorted _id DESC.
func (h *Handler) billingProfileAdminList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:billing_profile:read") {
		return
	}
	pg, ok := paging.FromRequest(w, r)
	if !ok {
		return
	}
	profiles, total, err := h.listRawSortedMaybePaged(r.Context(), "billingProfile", "_id", -1, pg)
	if httpx.WriteError(w, err) {
		return
	}
	bal := billing.NewBalanceService(h.billing)
	now := time.Now().UTC()
	out := make([]pgdoc.M, 0, len(profiles))
	for _, p := range profiles {
		id := idToString(p["_id"])
		sd, _ := shapeDeep(p).(pgdoc.M)
		zero := json.Number("0")
		accountCredit, balance, promo := zero, zero, zero
		currentMonth, lastMonth, forecast := zero, zero, zero
		if id != "" {
			if v, err := h.billing.AccountCreditTotal(r.Context(), id); err == nil {
				accountCredit = json.Number(v.String())
			}
			if v, err := h.billing.AvailablePromotionalTotal(r.Context(), id, now); err == nil {
				promo = json.Number(v.String())
			}
			if v, err := bal.CurrentBalance(r.Context(), id, now); err == nil {
				balance = json.Number(v.String())
			}
			// This Month / Prev. Month — the profile's bill-item net per month (same bill summation as
			// the client cost-info dashboard). forecastedMonthEnd stays the current cost here: the
			// projection resolves and rates every billable resource of the profile, too heavy for
			// every row of a list. forecastApproximate says so; the profile's cost info projects it.
			if bills, err := h.billing.BillsByBillingProfile(r.Context(), id); err == nil {
				cur, lst := billing.MonthlyBillCosts(bills, now)
				currentMonth = json.Number(cur.String())
				lastMonth = json.Number(lst.String())
				forecast = currentMonth
			}
		}
		sd["balance"] = balance
		sd["accountCredit"] = accountCredit
		sd["promotionalCredit"] = promo
		sd["currentMonth"] = currentMonth
		sd["lastMonth"] = lastMonth
		sd["forecastedMonthEnd"] = forecast
		sd["forecastApproximate"] = true
		out = append(out, sd)
	}
	emitAdminList(w, pg, out, total)
}

// billFinancialOverview builds the bill financial overview: recompute net (product
// currency, with adjustments), gross + unpaid-gross (taxed → FX'd to the profile currency) through
// the golden-tested pricing engine, and shape the bill's own fields (money as numbers). The result
// is the overview field set (year/month default 0; currency = the base currency).
func (h *Handler) billFinancialOverview(ctx context.Context, billDoc pgdoc.M) (pgdoc.M, error) {
	var bill pricing.Bill
	if err := decodeTyped(billDoc, &bill); err != nil {
		return nil, err
	}
	bpRaw, err := h.repo.BillingProfileByIDRaw(ctx, bill.BillingProfileID)
	if err != nil {
		return nil, err
	}
	if bpRaw == nil {
		return nil, httpx.NotFound("Billing profile not found")
	}
	var bp billing.BillingProfile
	if err := decodeTyped(bpRaw, &bp); err != nil {
		return nil, err
	}
	baseCcy, _ := h.billing.BaseCurrency(ctx)
	now := time.Now().UTC()
	var rates []pricing.TaxRate
	if h.pricing != nil {
		all, _ := h.pricing.AllTaxRates(ctx)
		rates = pricing.SelectTaxRates(all, bp.Country, bp.Company, now)
	}
	x := pricing.NewExchanger(nil)
	dto, err := billing.ToBillDto(&bp, &bill, rates, baseCcy, x, now)
	if err != nil {
		return nil, err
	}
	net := pricing.GetNetAmountBillProductCurrencyWithAdjustments(&bill)

	sd, _ := shapeDeep(billDoc).(pgdoc.M)
	fo := pgdoc.M{
		"id":                 sd["id"],
		"status":             sd["status"],
		"billingProfileId":   sd["billingProfileId"],
		"currency":           baseCcy,
		"totalAmount":        json.Number(net.String()),
		"totalInvoiceAmount": dto.GrossAmount,
		"unpaidAmount":       dto.UnpaidGrossAmount,
		"year":               0,
		"month":              0,
	}
	if v, ok := sd["items"]; ok {
		fo["items"] = v
	} else {
		fo["items"] = pgdoc.A{}
	}
	for _, k := range []string{"invoiceCurrency", "invoiceGatewayId", "appliedAccountCredits",
		"collectedAmounts", "appliedPromotionalCredits", "billingCycle", "dueAt", "sentAt", "lockedAt"} {
		if v, ok := sd[k]; ok {
			fo[k] = v
		}
	}
	return fo, nil
}

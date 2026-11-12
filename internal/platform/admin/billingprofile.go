package admin

// billingprofile.go serves the MUTATIONS (and the few not-yet-registered reads) of the
// billing-profile surface (/api/v1/admin/billing-profile). Follows the custommenu.go reference:
// id-aware CRUD via the crud.go helpers, exact perms / error strings / response envelopes,
// `_id`→`id` shaping on the way out.
//
// Perms:
//   - read   = ADMIN_BILLING_PROFILE_READ   = admin:billing_profile:read
//   - update = ADMIN_BILLING_PROFILE_UPDATE = admin:billing_profile:update
//
// IN SCOPE (faithful datastore state-flips via the crud.go helpers):
//   PUT  /{id}                              update — overwrite the ~17 editable profile fields
//   PUT  /automatic-suspension/{id}         set suspensionConfiguration + overwriteSuspension
//   PUT  /tax-configuration/{id}            set taxConfiguration
//   PUT  /project-provisioning-quota/{id}   set projectProvisioningQuota
//   PUT  /reseller/{id}                     set reseller
//   PUT  /message-templates/{id}            set messageTemplateConfig
//   DELETE /{id}                            in-use guards (bill/project/card) → delete by id
//   POST /validations/{validationId}/status/{status}   flip the validation doc's status
//
// EXTERNAL INTEGRATION POINTS (external/cross-service — NOT executed; 501 after any faithful pre-step):
//   POST /{id}                              create  — create the billing profile +
//                                           activation + optional project create
//   POST /{id}/action/{status}             status transition — activation / suspension
//                                           (KYC verify / suspend / unsuspend), KYC + cloud side
//                                           effects; only the deterministic guards are faithful.
// (The validation APPROVED branch is LIVE since dev229: it marks the activation constraint completed
// + sends the billing_profile_validated mail via h.activation; nil activation → the original 501 posture.)
//
// DEFERRED reads (need the BillingSummary / usage / cost-rollup compute — money engine, must NOT be
// reimplemented here): GET /{id} (BillingSummary), GET (AggregatedBillingProfile list w/ balances),
// GET /search, GET /financial/{id}, GET {id}/cost-info, GET /validations. Left UNregistered (the bare
// GET /billing-profile list is already in handler.go). See 'deferred'.
//
// Audit is deferred this pass (// TODO(audit)); the persisted state +
// response are faithful, which is what the admin UI exercises.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const billingProfileCollection = "billingProfile"

const billingProfileValidationCollection = "billingProfileValidation"

const (
	billingProfileReadPerm   = "admin:billing_profile:read"
	billingProfileUpdatePerm = "admin:billing_profile:update"
)

// routeBillingProfile registers ONLY the billing-profile admin mutation routes. The bare GET list
// (`/billing-profile`) is already registered in handler.go and is NOT re-registered here. The {id}
// param name is reused at the `/{id}` position (chi requires a single param name at a path position).
func (h *Handler) routeBillingProfile(r chi.Router) {
	r.Post("/billing-profile", h.billingProfileCreate)
	r.Get("/billing-profile/financial/{id}", h.billingProfileFinancialSummary)
	r.Get("/billing-profile/search", h.billingProfileSearch)
	r.Get("/billing-profile/validations", h.billingProfileValidations)
	r.Get("/billing-profile/{id}", h.billingProfileByID)
	r.Get("/billing-profile/{id}/cost-info", h.billingProfileCostInfo)
	r.Put("/billing-profile/{id}", h.billingProfileUpdate)
	r.Delete("/billing-profile/{id}", h.billingProfileDelete)
	r.Post("/billing-profile/{id}/action/{status}", h.billingProfileUpdateStatus)
	r.Put("/billing-profile/automatic-suspension/{id}", h.billingProfileAutomaticSuspension)
	r.Put("/billing-profile/tax-configuration/{id}", h.billingProfileTaxConfiguration)
	r.Put("/billing-profile/project-provisioning-quota/{id}", h.billingProfileProvisioningQuota)
	r.Put("/billing-profile/reseller/{id}", h.billingProfileReseller)
	r.Put("/billing-profile/message-templates/{id}", h.billingProfileMessageTemplates)
	r.Post("/billing-profile/validations/{validationId}/status/{status}", h.billingProfileValidationStatus)
}

// billingProfileByID loads the profile by id or 404s, then returns a
// BillingSummary (profile + computed financials), NOT
// the raw profile. ADMIN_BILLING_PROFILE_READ.
func (h *Handler) billingProfileByID(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:billing_profile:read") {
		return
	}
	id := chi.URLParam(r, "id")
	raw, err := h.repo.BillingProfileByIDRaw(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if raw == nil {
		httpx.WriteError(w, billingProfileIDNotFound(id))
		return
	}
	var bp billing.BillingProfile
	if err := decodeTyped(raw, &bp); httpx.WriteError(w, err) {
		return
	}
	// Compute the real financials (balance / account credit / promotional credit);
	// without WithFinancials the summary carries the placeholder 0s
	// (the admin bp-detail stat row then reads $0 despite real credits).
	summary := billing.ToSummary(&bp).WithFinancials(r.Context(), h.billing, time.Now().UTC())
	httpx.OK(w, summary)
}

// billingProfileFinancialSummary loads the profile or 404s, then returns a BillingProfileFinancialOverview
// {currency, totalCredit, totalPromotionalCredit, currentMonthUsage(=current-month bill net),
// totalSuccessfulBillTransactions, totalSuccessfulAddFundsTransactions, numberOfTransactionsLastMonth}.
func (h *Handler) billingProfileFinancialSummary(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:billing_profile:read") {
		return
	}
	id := chi.URLParam(r, "id")
	raw, err := h.repo.BillingProfileByIDRaw(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if raw == nil {
		httpx.WriteError(w, billingProfileIDNotFound(id))
		return
	}
	now := time.Now().UTC()
	cutoff := now.AddDate(0, 0, -30)
	credit, _ := h.billing.AccountCreditTotal(r.Context(), id)
	promo, _ := h.billing.AvailablePromotionalTotal(r.Context(), id, now)
	// currentMonthUsage = the current-month bill's net
	// (the bill net, NOT live metering).
	usage := json.Number("0")
	if bills, err := h.billing.BillsByBillingProfile(r.Context(), id); err == nil {
		c, _ := billing.MonthlyBillCosts(bills, now)
		usage = json.Number(c.String())
	}
	succBill, _ := h.repo.CountBy(r.Context(), "collectTransaction", pgdoc.M{"billingProfileId": id, "status": "SUCCESS"})
	succAdd, _ := h.repo.CountBy(r.Context(), "accountCreditTransaction", pgdoc.M{"billingProfileId": id, "status": "SUCCESS"})
	c1, _ := h.repo.CountBy(r.Context(), "collectTransaction", pgdoc.M{"billingProfileId": id, "createdAt": pgdoc.M{"$gt": cutoff}})
	c2, _ := h.repo.CountBy(r.Context(), "accountCreditTransaction", pgdoc.M{"billingProfileId": id, "createdAt": pgdoc.M{"$gt": cutoff}})
	baseCcy, _ := h.billing.BaseCurrency(r.Context())
	httpx.OK(w, pgdoc.M{
		"currency":                            baseCcy,
		"totalCredit":                         json.Number(credit.String()),
		"totalPromotionalCredit":              json.Number(promo.String()),
		"currentMonthUsage":                   usage,
		"totalSuccessfulBillTransactions":     succBill,
		"totalSuccessfulAddFundsTransactions": succAdd,
		"numberOfTransactionsLastMonth":       c1 + c2,
	})
}

// billingProfileCostInfo returns a BillingUsageOverview. Cost fields are
// computed from the profile's bills (current/last month net, by-category, topResourcePrices — the
// same rollup the client cost-info dashboard uses); balance/accountCredit/promotionalCredits/
// dueAmount are real (the balance layer). projects is an empty map (the FE keys off
// billingProfileCostInfo, not per-project).
func (h *Handler) billingProfileCostInfo(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:billing_profile:read") {
		return
	}
	id := chi.URLParam(r, "id")
	raw, err := h.repo.BillingProfileByIDRaw(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if raw == nil {
		httpx.WriteError(w, billingProfileIDNotFound(id))
		return
	}
	now := time.Now().UTC()
	bal := billing.NewBalanceService(h.billing)
	credit, _ := h.billing.AccountCreditTotal(r.Context(), id)
	promo, _ := h.billing.AvailablePromotionalTotal(r.Context(), id, now)
	balance, _ := bal.CurrentBalance(r.Context(), id, now)
	dueAmt, _ := bal.CurrentDue(r.Context(), id)
	zero := json.Number("0")
	// Per-month bill-net costs + by-category + topResourcePrices — the SAME rollup the client
	// cost-info dashboard uses (the admin overview reuses it). The
	// CREATED column reads resource.createdAt → look it up from the cloud cache. The month-end
	// forecast is the client's too (billing/forecast.go).
	cur, last := zero, zero
	costInfo := billing.CostInfoMap(decimal.Zero, decimal.Zero, map[string]any{}, map[string]any{}, []any{})
	projects := map[string]any{}
	var fc *billing.Forecast
	if bills, err := h.billing.BillsByBillingProfile(r.Context(), id); err == nil {
		fc = billing.MonthEndForecast(r.Context(), h.forecaster, id, bills, now)
		createdAtLookup := func(rid string) *time.Time {
			if cr, _ := h.cloud.FindByID(r.Context(), rid); cr != nil {
				return cr.CreatedAt
			}
			return nil
		}
		c, l, bt, lbt, tp := billing.BillCostBreakdown(bills, now, createdAtLookup)
		cur, last = json.Number(c.String()), json.Number(l.String())
		// CostInfo with every field present (each field defaulted) — the dashboard charts
		// read billingProfileCostInfo.topResourcePrices / .currentMonthCostsByType, so an omitted/null
		// billingProfileCostInfo crashes the FE.
		costInfo = billing.CostInfoMap(c, l, bt, lbt, tp)
		// projects: per-project CostInfo for the admin profile-detail per-project drill-down
		// (a CostInfo per project of the profile) — grouped by bill-item projectId.
		projects = billing.ProjectCostInfoMap(bills, now, createdAtLookup)
	}
	billing.ApplyForecast(costInfo, fc, "")
	billing.ApplyForecastToProjects(projects, fc)
	httpx.OK(w, pgdoc.M{
		"projects":                   projects,
		"billingProfileCostInfo":     costInfo,
		"balance":                    json.Number(balance.String()),
		"dueAmount":                  json.Number(dueAmt.String()),
		"accountCredit":              json.Number(credit.String()),
		"promotionalCredits":         json.Number(promo.String()),
		"currentMonthCosts":          cur,
		"lastMonthCosts":             last,
		"proratedMonthEndCosts":      costInfo["forecastedMonthEndCosts"],
		"forecastedMonthEndCosts":    costInfo["forecastedMonthEndCosts"],
		"forecastApproximate":        costInfo["forecastApproximate"],
		"forecastApproximateReasons": costInfo["forecastApproximateReasons"],
		"forecastUnratedTypes":       costInfo["forecastUnratedTypes"],
	})
}

// billingProfileValidations returns the PENDING validations,
// each joined with its billing profile (BillingProfileValidationWithProfile).
// Greenfield → empty.
func (h *Handler) billingProfileValidations(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:billing_profile:read") {
		return
	}
	vals, err := h.repo.ListRawFiltered(r.Context(), billingProfileValidationCollection, pgdoc.M{"status": "PENDING"})
	if httpx.WriteError(w, err) {
		return
	}
	out := make([]pgdoc.M, 0, len(vals))
	for _, v := range vals {
		bpID, _ := v["billingProfileId"].(string)
		sd, _ := shapeDeep(v).(pgdoc.M)
		if bpID != "" {
			if bp, err := h.repo.BillingProfileByIDRaw(r.Context(), bpID); err == nil && bp != nil {
				sd["billingProfile"] = shapeDeep(bp)
			}
		}
		out = append(out, sd)
	}
	httpx.List(w, out)
}

// billingProfileSearch filters billing profiles by the non-blank query params (exact match) → shaped
// list of BillingProfile.
func (h *Handler) billingProfileSearch(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:billing_profile:read") {
		return
	}
	filter := pgdoc.M{}
	for k, vs := range r.URL.Query() {
		if len(vs) > 0 && vs[0] != "" {
			filter[k] = vs[0]
		}
	}
	profiles, err := h.repo.ListRawFiltered(r.Context(), billingProfileCollection, filter)
	if httpx.WriteError(w, err) {
		return
	}
	out := make([]pgdoc.M, 0, len(profiles))
	for _, p := range profiles {
		if sd, ok := shapeDeep(p).(pgdoc.M); ok {
			out = append(out, sd)
		}
	}
	httpx.List(w, out)
}

// billingProfileIDNotFound builds the exact 404
// ("Billing profile with id %s not found. " — trailing space, interpolated) returned
// when the profile id is absent.
func billingProfileIDNotFound(id string) *httpx.HTTPError {
	return httpx.NotFound(fmt.Sprintf("Billing profile with id %s not found. ", id))
}

// findBillingProfileOr404 loads the profile doc by id (id-aware) or writes the exact 404 and
// returns ok=false. (Shared by every mutation that resolves the profile first.)
func (h *Handler) findBillingProfileOr404(w http.ResponseWriter, r *http.Request, id string) (pgdoc.M, bool) {
	doc, err := h.repo.FindDoc(r.Context(), billingProfileCollection, id)
	if httpx.WriteError(w, err) {
		return nil, false
	}
	if doc == nil {
		httpx.WriteError(w, billingProfileIDNotFound(id))
		return nil, false
	}
	return doc, true
}

// ─── PUT /{id} — update ──────────────────────────────────────────────────────────────────────────

// billingProfileUpdateReq holds the editable BillingProfile fields the update copies from the
// request body onto the loaded profile (~17 fields). The body IS a full BillingProfile but only
// these fields are applied (the rest of the loaded doc is preserved). Optional strings are pointers so
// an absent field is skipped (it stays as-is) while a present "" clears it — a
// missing key decodes to nil and a present key to its value, which is then written.
// pricePlanConfig is passed through as raw JSON (admin-configured sub-doc).
type billingProfileUpdateReq struct {
	FirstName       *string          `json:"firstName"`
	LastName        *string          `json:"lastName"`
	Company         *bool            `json:"company"`
	CompanyName     *string          `json:"companyName"`
	VatCode         *string          `json:"vatCode"`
	Bank            *string          `json:"bank"`
	Iban            *string          `json:"iban"`
	TaxPayer        *bool            `json:"taxPayer"`
	Phone           *string          `json:"phone"`
	ZipCode         *string          `json:"zipCode"`
	Address         *string          `json:"address"`
	City            *string          `json:"city"`
	County          *string          `json:"county"`
	Country         *string          `json:"country"`
	Email           *string          `json:"email"`
	Currency        *string          `json:"currency"`
	PricePlanConfig *json.RawMessage `json:"pricePlanConfig"`
	// contacts = the extra invoice/billing e-mail recipients (validated + de-duplicated, max 10).
	Contacts *[]billing.Contact `json:"contacts"`
	// paymentMode (PREPAID | POSTPAID) + paymentGatewayId — the customer's payment settings.
	PaymentMode      *string `json:"paymentMode"`
	PaymentGatewayID *string `json:"paymentGatewayId"`
}

// setMap builds the update document for the editable fields. Every present field is written (including
// its zero value); an absent field is left untouched
// (the loaded doc keeps it). The bool fields are always written (they default to false), so company /
// taxPayer are set whenever present (which they always are after a full-profile round-trip).
// A bad contact e-mail is a 400.
func (req billingProfileUpdateReq) setMap() (pgdoc.M, error) {
	d := pgdoc.M{}
	if req.Contacts != nil {
		contacts, err := billing.NormalizeContacts(*req.Contacts)
		if err != nil {
			return nil, err
		}
		d["contacts"] = contacts
	}
	if req.FirstName != nil {
		d["firstName"] = *req.FirstName
	}
	if req.LastName != nil {
		d["lastName"] = *req.LastName
	}
	if req.Company != nil {
		d["company"] = *req.Company
	}
	if req.CompanyName != nil {
		d["companyName"] = *req.CompanyName
	}
	if req.VatCode != nil {
		d["vatCode"] = *req.VatCode
	}
	if req.Bank != nil {
		d["bank"] = *req.Bank
	}
	if req.Iban != nil {
		d["iban"] = *req.Iban
	}
	if req.TaxPayer != nil {
		d["taxPayer"] = *req.TaxPayer
	}
	if req.Phone != nil {
		d["phone"] = *req.Phone
	}
	if req.ZipCode != nil {
		d["zipCode"] = *req.ZipCode
	}
	if req.Address != nil {
		d["address"] = *req.Address
	}
	if req.City != nil {
		d["city"] = *req.City
	}
	if req.County != nil {
		d["county"] = *req.County
	}
	if req.Country != nil {
		d["country"] = *req.Country
	}
	if req.Email != nil {
		d["email"] = *req.Email
	}
	if req.Currency != nil {
		d["currency"] = *req.Currency
	}
	if req.PricePlanConfig != nil {
		d["pricePlanConfig"] = rawJSON(*req.PricePlanConfig)
	}
	if req.PaymentMode != nil {
		mode := billing.ValidPaymentMode(*req.PaymentMode)
		if mode == "" {
			return nil, httpx.BadRequest("Invalid payment mode: use PREPAID or POSTPAID.")
		}
		d["paymentMode"] = mode
	}
	if req.PaymentGatewayID != nil {
		d["paymentGatewayId"] = strings.TrimSpace(*req.PaymentGatewayID)
	}
	return d, nil
}

// billingProfileUpdate loads the profile or 404s → copies the editable fields
// → saves → returns it. ADMIN_BILLING_PROFILE_UPDATE.
func (h *Handler) billingProfileUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billingProfileUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req billingProfileUpdateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, ok := h.findBillingProfileOr404(w, r, id)
	if !ok {
		return
	}
	set, err := req.setMap()
	if httpx.WriteError(w, err) {
		return
	}
	for k, v := range set {
		existing[k] = v
	}
	if _, err := h.repo.SetFields(r.Context(), billingProfileCollection, id, set); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event for the profile update (UPDATE, ORGANIZATION) with the before/after diff.
	httpx.OK(w, shapeDoc(existing))
}

// ─── PUT /automatic-suspension/{id} ──────────────────────────────────────────────────────────────

// automaticSuspensionConfigReq is the automatic-suspension request body (optional):
// overwriteSuspension (primitive bool) + suspensionConfiguration (BillingAutomaticSuspensionConfig,
// passed through as raw JSON — admin-configured sub-doc with enabled/type/suspendedAt/notifications).
type automaticSuspensionConfigReq struct {
	OverwriteSuspension     bool             `json:"overwriteSuspension"`
	SuspensionConfiguration *json.RawMessage `json:"suspensionConfiguration"`
}

// billingProfileAutomaticSuspension loads the profile or 404s →
// sets suspensionConfiguration + overwriteSuspension → saves → returns it. The body is optional;
// an empty/absent body decodes to the zero value (overwriteSuspension=false,
// suspensionConfiguration=null) — the admin UI always sends a body, so we treat absent as the zero request.
func (h *Handler) billingProfileAutomaticSuspension(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billingProfileUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req automaticSuspensionConfigReq
	if r.Body != nil {
		// Tolerate an empty body (required=false) — leave req at its zero value on EOF.
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
			return
		}
	}
	existing, ok := h.findBillingProfileOr404(w, r, id)
	if !ok {
		return
	}
	set := pgdoc.M{"overwriteSuspension": req.OverwriteSuspension}
	if req.SuspensionConfiguration != nil {
		set["suspensionConfiguration"] = rawJSON(*req.SuspensionConfiguration)
	} else {
		set["suspensionConfiguration"] = nil
	}
	for k, v := range set {
		existing[k] = v
	}
	if _, err := h.repo.SetFields(r.Context(), billingProfileCollection, id, set); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event for the suspension-config change (CONFIGURE, ORGANIZATION).
	httpx.OK(w, shapeDoc(existing))
}

// ─── PUT /tax-configuration/{id} ─────────────────────────────────────────────────────────────────

// taxConfigurationReq mirrors BillingProfileTaxConfiguration {disableAutomaticTaxCalculation, taxRuleId}.
type taxConfigurationReq struct {
	DisableAutomaticTaxCalculation bool    `json:"disableAutomaticTaxCalculation"`
	TaxRuleID                      *string `json:"taxRuleId"`
}

func (req taxConfigurationReq) doc() pgdoc.M {
	d := pgdoc.M{"disableAutomaticTaxCalculation": req.DisableAutomaticTaxCalculation}
	if req.TaxRuleID != nil {
		d["taxRuleId"] = *req.TaxRuleID
	}
	return d
}

// billingProfileTaxConfiguration loads the profile or 404s →
// sets taxConfiguration → saves → returns it.
func (h *Handler) billingProfileTaxConfiguration(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billingProfileUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req taxConfigurationReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, ok := h.findBillingProfileOr404(w, r, id)
	if !ok {
		return
	}
	existing["taxConfiguration"] = req.doc()
	if _, err := h.repo.SetFields(r.Context(), billingProfileCollection, id, pgdoc.M{"taxConfiguration": req.doc()}); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event for the tax-config change (CONFIGURE, ORGANIZATION).
	httpx.OK(w, shapeDoc(existing))
}

// ─── PUT /project-provisioning-quota/{id} ────────────────────────────────────────────────────────

// projectProvisioningQuotaReq mirrors ProjectProvisioningQuota {enabled, limit}.
type projectProvisioningQuotaReq struct {
	Enabled bool `json:"enabled"`
	Limit   int  `json:"limit"`
}

// billingProfileProvisioningQuota loads the profile or 404s →
// sets projectProvisioningQuota → saves → returns it.
func (h *Handler) billingProfileProvisioningQuota(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billingProfileUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req projectProvisioningQuotaReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, ok := h.findBillingProfileOr404(w, r, id)
	if !ok {
		return
	}
	quota := pgdoc.M{"enabled": req.Enabled, "limit": req.Limit}
	existing["projectProvisioningQuota"] = quota
	if _, err := h.repo.SetFields(r.Context(), billingProfileCollection, id, pgdoc.M{"projectProvisioningQuota": quota}); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event for the provisioning-quota change (CONFIGURE, ORGANIZATION).
	httpx.OK(w, shapeDoc(existing))
}

// ─── PUT /reseller/{id} ──────────────────────────────────────────────────────────────────────────

// resellerReq mirrors BillingProfileReseller {enabled}.
type resellerReq struct {
	Enabled bool `json:"enabled"`
}

// billingProfileReseller loads the profile or 404s → sets reseller.enabled → saves → returns it.
func (h *Handler) billingProfileReseller(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billingProfileUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req resellerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, ok := h.findBillingProfileOr404(w, r, id)
	if !ok {
		return
	}
	reseller := pgdoc.M{"enabled": req.Enabled}
	existing["reseller"] = reseller
	if _, err := h.repo.SetFields(r.Context(), billingProfileCollection, id, pgdoc.M{"reseller": reseller}); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event for the reseller change (CONFIGURE, ORGANIZATION).
	httpx.OK(w, shapeDoc(existing))
}

// ─── PUT /message-templates/{id} ─────────────────────────────────────────────────────────────────

// messageTemplateConfigReq mirrors BillingProfileMessageTemplateConfig {disabled, messageTemplates[]}.
// messageTemplates is passed through as raw JSON (list of BillingProfileMessageTemplate sub-docs).
type messageTemplateConfigReq struct {
	Disabled         bool             `json:"disabled"`
	MessageTemplates *json.RawMessage `json:"messageTemplates"`
}

func (req messageTemplateConfigReq) doc() pgdoc.M {
	d := pgdoc.M{"disabled": req.Disabled}
	if req.MessageTemplates != nil {
		d["messageTemplates"] = rawJSON(*req.MessageTemplates)
	}
	return d
}

// billingProfileMessageTemplates loads the profile or 404s → sets messageTemplateConfig →
// saves → returns it.
func (h *Handler) billingProfileMessageTemplates(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billingProfileUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req messageTemplateConfigReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, ok := h.findBillingProfileOr404(w, r, id)
	if !ok {
		return
	}
	existing["messageTemplateConfig"] = req.doc()
	if _, err := h.repo.SetFields(r.Context(), billingProfileCollection, id, pgdoc.M{"messageTemplateConfig": req.doc()}); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event for the message-template change (CONFIGURE, ORGANIZATION).
	httpx.OK(w, shapeDoc(existing))
}

// ─── DELETE /{id} ────────────────────────────────────────────────────────────────────────────────

// billingProfileDelete runs the three in-use guards (bills → projects → cards) then
// deletes (get-or-404 → delete by id → success). Each guard 400s with its exact message.
// NOTE the card-guard reuses the *projects* message string (a pre-existing quirk, kept faithfully).
func (h *Handler) billingProfileDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billingProfileUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	// Guard: is the billing profile referenced by any bill?
	inUse, err := h.repo.ExistsByBillingProfileID(r.Context(), "bill", id)
	if httpx.WriteError(w, err) {
		return
	}
	if inUse {
		httpx.WriteError(w, httpx.BadRequest("Billing profile is in use for some bills"))
		return
	}
	// Guard: does the billing profile pay for any project, its own or through its organization?
	paidFor, err := h.repo.ProjectPaidBy(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if paidFor != "" {
		httpx.WriteError(w, httpx.BadRequest("Billing profile is in use for some projects"))
		return
	}
	// Guard: is the billing profile referenced by any credit card? The
	// "…for some projects" message is reused here (kept faithful to the source).
	inUse, err = h.repo.ExistsByBillingProfileID(r.Context(), "creditCard", id)
	if httpx.WriteError(w, err) {
		return
	}
	if inUse {
		httpx.WriteError(w, httpx.BadRequest("Billing profile is in use for some projects"))
		return
	}
	// Delete: get-or-404 → delete by id.
	if _, ok := h.findBillingProfileOr404(w, r, id); !ok {
		return
	}
	if _, err := h.repo.DeleteDoc(r.Context(), billingProfileCollection, id); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event when a billing profile is deleted (DELETE, ORGANIZATION).
	httpx.OK(w, "Successful operation")
}

// ─── POST /{id}/action/{status} — status transition ──────────────────────────────────────────────

// billingProfileUpdateStatus parses the status, loads the profile or 404s, then applies the
// transition matrix. The deterministic guards ARE faithful:
//   - an invalid status value → 400.
//   - desired == current → 400 "Billing profile with id %s already has status %s ".
//   - any transition other than NEW→ACTIVE / ACTIVE→SUSPENDED / SUSPENDED→ACTIVE →
//     400 "Status %s is not supported ".
//
// The three SUPPORTED transitions all drive activation / suspension (KYC verify, cloud
// suspend/resume, audit) — cross-service external effects not wired into admin.Handler → 501.
func (h *Handler) billingProfileUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billingProfileUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	status := chi.URLParam(r, "status")
	// Reject an invalid status value → 400.
	if !isValidBillingProfileStatus(status) {
		httpx.WriteError(w, httpx.BadRequest(fmt.Sprintf("Invalid billing profile status %s", status)))
		return
	}
	existing, ok := h.findBillingProfileOr404(w, r, id)
	if !ok {
		return
	}
	current, _ := existing["status"].(string)
	if status == current {
		httpx.WriteError(w, httpx.BadRequest(fmt.Sprintf("Billing profile with id %s already has status %s ", id, status)))
		return
	}
	supported := (status == "ACTIVE" && current == "NEW") ||
		(status == "SUSPENDED" && current == "ACTIVE") ||
		(status == "ACTIVE" && current == "SUSPENDED")
	if !supported {
		httpx.WriteError(w, httpx.BadRequest(fmt.Sprintf("Status %s is not supported ", status)))
		return
	}
	if h.activation == nil {
		httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented,
			fmt.Sprintf("updateBillingProfileStatus transition not implemented: %s -> %s", current, status)))
		return
	}
	// The transition matrix drives the activation flow
	// (KYC verify + activate / suspend + process flip / unsuspend + resolve).
	bp, err := h.billing.FindByID(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if bp == nil {
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("Billing profile with id %s not found. ", id)))
		return
	}
	switch {
	case status == "ACTIVE" && current == "NEW":
		if err := h.activation.MarkKycVerificationsAsVerified(r.Context(), bp); httpx.WriteError(w, err) {
			return
		}
		if err := h.activation.Activate(r.Context(), bp, billing.SourceAdmin); httpx.WriteError(w, err) {
			return
		}
	case status == "SUSPENDED" && current == "ACTIVE":
		if err := h.activation.Suspend(r.Context(), bp, billing.SourceAdmin); httpx.WriteError(w, err) {
			return
		}
		_ = h.activation.SuspendProcessIfExists(r.Context(), bp, billing.SourceAdmin)
	case status == "ACTIVE" && current == "SUSPENDED":
		if err := h.activation.Unsuspend(r.Context(), bp, billing.SourceAdmin); httpx.WriteError(w, err) {
			return
		}
		_ = h.activation.ResolveSuspensionIfExists(r.Context(), bp, billing.SourceAdmin)
	}
	// re-read + audit the status change (UPDATE, ORGANIZATION, {status, previousStatus}) + return the updated doc.
	updated, ok := h.findBillingProfileOr404(w, r, id)
	if !ok {
		return
	}
	// TODO(audit): audit the status change {status, previousStatus} — the global middleware logs the action.
	httpx.OK(w, shapeDoc(updated))
}

// isValidBillingProfileStatus reports whether s is one of the statuses {NEW,ACTIVE,SUSPENDED,SKIP}.
func isValidBillingProfileStatus(s string) bool {
	switch s {
	case "NEW", "ACTIVE", "SUSPENDED", "SKIP":
		return true
	default:
		return false
	}
}

// ─── POST /{id} — create ─────────────────────────────────────────────────────────────────────────

// billingProfileCreate creates a billing profile +
// marks the activation constraint completed + (optionally creates the project).
// The whole chain (org resolution, owner population, currency, activation, optional project create) is
// in the billing/org/project/activation services — none wired into admin.Handler → 501. No
// faithful pre-step exists (the request carries no id to resolve).
func (h *Handler) billingProfileCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billingProfileUpdatePerm) {
		return
	}
	// Decode to validate it is well-formed JSON (a malformed body would 400 before the
	// service call), but the create itself is not wired.
	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented,
		"createBillingProfile not implemented"))
}

// ─── POST /validations/{validationId}/status/{status} ────────────────────────────────────────────

// billingProfileValidationStatus commits the locked approval/mail decision first.
// Activation remains a postcommit best-effort business tail, NOT mail atomicity.
func (h *Handler) billingProfileValidationStatus(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billingProfileUpdatePerm) {
		return
	}
	validationID := chi.URLParam(r, "validationId")
	status := chi.URLParam(r, "status")
	// the status path variable — an invalid enum → 400.
	if !isValidValidationStatus(status) {
		httpx.WriteError(w, httpx.BadRequest(fmt.Sprintf("Invalid validation status %s", status)))
		return
	}
	decision, err := h.decideValidationStatus(r.Context(), validationID, status)
	if httpx.WriteError(w, err) {
		return
	}
	existing := decision.doc
	if status == "APPROVED" {
		if h.validationActivation == nil {
			// Activation unwired (tests) → the original not-implemented posture.
			httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented,
				"validation APPROVED activation/notify not implemented"))
			return
		}
		// Load the profile referenced by the validation →
		// mark the activation constraint completed (VALIDATION) → send the validation notification.
		bpID := decision.profileID
		bp, err := h.billing.FindByID(r.Context(), bpID)
		if httpx.WriteError(w, err) {
			return
		}
		if bp == nil {
			// Profile absent → 404 (trailing period+space).
			httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("Billing profile with id %s not found. ", bpID)))
			return
		}
		if err := h.validationActivation.Activate(r.Context(), bp, billing.SourceValidation); httpx.WriteError(w, err) {
			return
		}
		if !decision.suppressLegacy {
			h.validationActivation.NotifyValidation(r.Context(), bp)
		}
	}
	httpx.OK(w, shapeDoc(existing))
}

// isValidValidationStatus reports whether s is one of the validation statuses.
func isValidValidationStatus(s string) bool {
	switch s {
	case "PENDING", "APPROVED", "REJECTED":
		return true
	default:
		return false
	}
}

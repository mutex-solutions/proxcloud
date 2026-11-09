package admin

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// savingscontract.go implements the savings-contract surface (/api/v1/admin/savings-contracts) —
// the mutations (POST create, PUT update, DELETE) + the two extra list reads (by-billing-profile,
// by-savings-plan-with-billing-profile). The two plain reads (bare list + GET /{id}) are already
// registered in handler.go (listRaw / rawByID) and are intentionally NOT re-registered here.
//
// Perms (admin:* permission keys): read endpoints gate ADMIN_SAVINGS_PLAN_READ
// (admin:savings_plan:read); the mutations gate ADMIN_SAVINGS_PLAN_MANAGE (admin:savings_plan:manage).
//
// update/delete should also write admin audit events — deferred this pass (// TODO(audit)); the
// persisted state + the response envelope are faithful.
//
// Storage uses the id-aware crud.go helpers + shapeDoc (the endpoints return the RAW
// SavingsContract domain as a single-object envelope, exactly like the by-id read), with the
// money/tier business logic (create's discount-rate selection) decoded through the typed billing
// domain (decimal.Decimal) so the decimal compares are faithful.

const savingsContractPerm = "admin:savings_plan:read"
const savingsContractManagePerm = "admin:savings_plan:manage"

const savingsContractCollection = "savingsContract"

// routeSavingsContract registers ONLY the new savings-contract routes (the bare list and GET /{id}
// already live in handler.go). chi: the path position after `/savings-contracts/` already uses the
// param name `{id}` (the registered GET /{id}); the new {savingsContractId}/{billingProfileId}
// param routes MUST reuse `{id}` to avoid a chi registration panic. `billing-profile` and
// `savings-plan` are static siblings of the `{id}` param (no conflict).
func (h *Handler) routeSavingsContract(r chi.Router) {
	r.Post("/savings-contracts/{id}", h.savingsContractCreate)
	r.Put("/savings-contracts/{id}", h.savingsContractUpdate)
	r.Delete("/savings-contracts/{id}", h.savingsContractDelete)
	r.Post("/savings-contracts/{id}/record-payment", h.savingsContractRecordPayment)
	r.Get("/savings-contracts/billing-profile/{id}", h.savingsContractsByBillingProfile)
	r.Get("/savings-contracts/savings-plan/{id}", h.savingsContractsBySavingsPlan)
}

// createSavingsContractReq is the create-savings-contract request body (POST create).
// startDate is the enum CURRENT_MONTH | NEXT_MONTH.
type createSavingsContractReq struct {
	SavingsPlanID          string           `json:"savingsPlanId"`
	DurationMonths         int              `json:"durationMonths"`
	MonthlyCommittedAmount *decimal.Decimal `json:"monthlyCommittedAmount"`
	PaidUpfront            bool             `json:"paidUpfront"`
	StartDate              string           `json:"startDate"`
	PaymentReference       string           `json:"paymentReference"`
}

// savingsContractCreate validates the billing profile + available plan + no-duplicate-active,
// computes start/end dates + the discount rate from the matching schedule's tiers, persists a new
// ACTIVE contract, and returns it. Gated ADMIN_SAVINGS_PLAN_MANAGE.
func (h *Handler) savingsContractCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, savingsContractManagePerm) {
		return
	}
	billingProfileID := chi.URLParam(r, "id")
	var req createSavingsContractReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}

	// Load the billing profile; 404 if it doesn't exist.
	bp, err := h.repo.savingsContractFindBillingProfile(r.Context(), billingProfileID)
	if httpx.WriteError(w, err) {
		return
	}
	if bp == nil {
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("Billing profile with id %s not found. ", billingProfileID)))
		return
	}

	// Load the savings plan, only when it's marked available.
	plan, err := h.repo.savingsContractAvailablePlan(r.Context(), req.SavingsPlanID)
	if httpx.WriteError(w, err) {
		return
	}
	if plan == nil {
		httpx.WriteError(w, httpx.NotFound("Savings plan not found"))
		return
	}

	// Reject a second ACTIVE contract for the same (plan, billing profile) → 400.
	exists, err := h.repo.savingsContractActiveExists(r.Context(), plan.ID, billingProfileID)
	if httpx.WriteError(w, err) {
		return
	}
	if exists {
		httpx.WriteError(w, httpx.BadRequest("You already have a savings contract for this savings plan"))
		return
	}

	startDate, perr := savingsContractStartDate(req.StartDate)
	if perr != nil {
		httpx.WriteError(w, perr)
		return
	}
	endDate := startDate.AddDate(0, req.DurationMonths, 0)

	// schedule lookup by durationMonths → 400 "No schedule found for the given duration".
	var schedule *billing.SavingsPlanSchedule
	for i := range plan.SavingSchedule {
		if plan.SavingSchedule[i].DurationMonths == req.DurationMonths {
			schedule = &plan.SavingSchedule[i]
			break
		}
	}
	if schedule == nil {
		httpx.WriteError(w, httpx.BadRequest("No schedule found for the given duration"))
		return
	}

	monthly := decimal.Zero
	if req.MonthlyCommittedAmount != nil {
		monthly = *req.MonthlyCommittedAmount
	}
	discountRate, derr := savingsContractDiscountRate(req.PaidUpfront, schedule, monthly)
	if derr != nil {
		httpx.WriteError(w, derr)
		return
	}

	// Build the stored doc (omit blank optional strings; targets is copied from the plan's targets).
	doc := pgdoc.M{
		"billingProfileId": billingProfileID,
		"savingsPlanId":    plan.ID,
		"status":           billing.SavingsStatusActive,
		"durationMonths":   req.DurationMonths,
		"startDate":        startDate,
		"endDate":          endDate,
		"discountRate":     discountRate,
		"paidUpfront":      req.PaidUpfront,
	}
	if plan.Name != "" {
		doc["savingsPlanName"] = plan.Name
	}
	if plan.Targets != nil {
		doc["targets"] = plan.Targets
	}
	if req.MonthlyCommittedAmount != nil {
		doc["monthlyCommittedAmount"] = *req.MonthlyCommittedAmount
	}
	// ACTIVE at once means the operator took the money outside the portal: the contract records who says so.
	if req.PaidUpfront {
		doc["paymentRecordedBy"] = operatorID(r)
		if req.PaymentReference != "" {
			doc["paymentReference"] = req.PaymentReference
		}
	}

	saved, err := h.repo.InsertDoc(r.Context(), savingsContractCollection, doc)
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, shapeDoc(saved))
}

// savingsContractUpdate loads the contract (404 if absent), overwrites the mutable fields from the
// request body, persists, and returns the updated record. Gated ADMIN_SAVINGS_PLAN_MANAGE.
// Overwrites: billingProfileId, savingsPlanId, savingsPlanName, targets, startDate, endDate,
// discountRate, monthlyCommittedAmount, paidUpfront, orderId, status (durationMonths/createdAt are
// NOT overwritten by update). A null field in the body is dropped from the stored doc.
func (h *Handler) savingsContractUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, savingsContractManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req savingsContractBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, err := h.repo.FindDoc(r.Context(), savingsContractCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Savings contract not found"))
		return
	}
	before := maps.Clone(existing) // snapshot the pre-mutation state for the audit field-diff
	// Overwrite the mutable fields (drop-first so an omitted/null body field ends up absent).
	// durationMonths/createdAt are NOT overwritten by update; paidUpfront is a plain bool → always
	// set from the body (defaults false when omitted).
	for _, k := range []string{
		"billingProfileId", "savingsPlanId", "savingsPlanName", "targets", "startDate",
		"endDate", "discountRate", "monthlyCommittedAmount", "orderId", "status", "paidUpfront",
	} {
		delete(existing, k)
	}
	existing["paidUpfront"] = req.PaidUpfront
	for k, v := range req.setMap() {
		existing[k] = v
	}
	// An edit must not start the discount of an upfront contract nobody has paid for.
	if existing["status"] == billing.SavingsStatusActive && req.PaidUpfront && existing["paymentTransactionId"] == nil && existing["paymentRecordedBy"] == nil {
		httpx.WriteError(w, httpx.BadRequest("This upfront contract has no payment on record: use record-payment, or set it PENDING_PAYMENT."))
		return
	}
	if err := h.repo.ReplaceDoc(r.Context(), savingsContractCollection, id, existing); httpx.WriteError(w, err) {
		return
	}
	// UPDATE audit: the audit middleware records the field-level before/after diff on the audit
	// event. Re-read the AFTER from the datastore so both snapshots are store-decoded (same store
	// types/shape) — avoids spurious diffs from comparing the Go-rebuilt map against the
	// datastore-decoded `before`.
	after, _ := h.repo.FindDoc(r.Context(), savingsContractCollection, id)
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, shapeDoc(existing))
}

// savingsContractRecordPayment confirms an upfront contract's money arrived outside the portal: it activates
// a PENDING_PAYMENT contract the way a settled deposit would, and records who says so. Refused on a contract
// not waiting for one. Gated ADMIN_SAVINGS_PLAN_MANAGE; the audit middleware records the event.
func (h *Handler) savingsContractRecordPayment(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, savingsContractManagePerm) {
		return
	}
	var req struct {
		Reference string `json:"reference"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	if req.Reference = strings.TrimSpace(req.Reference); req.Reference == "" {
		httpx.WriteError(w, httpx.BadRequest("A payment reference is required"))
		return
	}
	id := chi.URLParam(r, "id")
	c, err := h.billing.SavingsContractByID(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if c == nil {
		httpx.WriteError(w, httpx.NotFound("Savings contract not found"))
		return
	}
	if !c.PaidUpfront || c.Status != billing.SavingsStatusPendingPayment {
		httpx.WriteError(w, httpx.BadRequest("This savings contract is not waiting for a payment."))
		return
	}
	before, err := h.repo.FindDoc(r.Context(), savingsContractCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	won, err := h.billing.RecordUpfrontPayment(r.Context(), c, req.Reference, operatorID(r), time.Now().UTC())
	if httpx.WriteError(w, err) {
		return
	}
	if !won { // a deposit settled or the customer cancelled while this was on its way
		httpx.WriteError(w, httpx.NewError(http.StatusConflict, http.StatusConflict, "This savings contract is not waiting for a payment."))
		return
	}
	after, err := h.repo.FindDoc(r.Context(), savingsContractCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, shapeDoc(after))
}

// operatorID names who made an admin request, for the record a payment confirmation leaves.
func operatorID(r *http.Request) string {
	if rc := httpx.RC(r.Context()); rc != nil {
		if rc.Sub != "" {
			return rc.Sub
		}
		if rc.SigV4KeyID != "" {
			return rc.SigV4KeyID
		}
	}
	return "operator"
}

// savingsContractDelete loads the contract (404 if absent) and deletes it. Responds with HTTP 200
// and an EMPTY body (not the envelope, not 202). Gated ADMIN_SAVINGS_PLAN_MANAGE.
func (h *Handler) savingsContractDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, savingsContractManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.repo.FindDoc(r.Context(), savingsContractCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Savings contract not found"))
		return
	}
	if _, err := h.repo.DeleteDoc(r.Context(), savingsContractCollection, id); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an organization-scoped admin audit event when a contract is deleted.
	// The handler returns no body → HTTP 200, empty body.
	w.WriteHeader(http.StatusOK)
}

// savingsContractsByBillingProfile returns the raw SavingsContract list for a billing profile as a
// list envelope. Gated ADMIN_SAVINGS_PLAN_READ.
func (h *Handler) savingsContractsByBillingProfile(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, savingsContractPerm) {
		return
	}
	items, err := h.repo.savingsContractsByBillingProfile(r.Context(), chi.URLParam(r, "id"))
	if httpx.WriteError(w, err) {
		return
	}
	for i := range items {
		shapeDoc(items[i])
	}
	httpx.List(w, items)
}

// savingsContractsBySavingsPlan returns the contracts for a savings plan, each joined to its
// billing profile, newest first, as a list envelope. Gated ADMIN_SAVINGS_PLAN_READ.
func (h *Handler) savingsContractsBySavingsPlan(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, savingsContractPerm) {
		return
	}
	items, err := h.repo.savingsContractsBySavingsPlanWithBillingProfile(r.Context(), chi.URLParam(r, "id"))
	if httpx.WriteError(w, err) {
		return
	}
	for i := range items {
		shapeDoc(items[i])
	}
	httpx.List(w, items)
}

// --- pure helpers (unit-tested) ---

// savingsContractStartDate computes the start date: CURRENT_MONTH → first day of the
// current month, NEXT_MONTH → first day of next month (both midnight UTC). Any other value is a bad
// enum and is rejected with a 400.
func savingsContractStartDate(start string) (time.Time, *httpx.HTTPError) {
	now := time.Now().UTC()
	switch start {
	case "CURRENT_MONTH":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), nil
	case "NEXT_MONTH":
		first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		return first.AddDate(0, 1, 0), nil
	default:
		return time.Time{}, httpx.BadRequest("Invalid request body")
	}
}

// savingsContractDiscountRate picks the discount rate: from the schedule's upfront/no-upfront tiers,
// take the max discount among tiers whose startAmount <= monthlyCommittedAmount → 400 if none.
func savingsContractDiscountRate(paidUpfront bool, schedule *billing.SavingsPlanSchedule, monthly decimal.Decimal) (decimal.Decimal, *httpx.HTTPError) {
	tiers := schedule.NoUpfrontTiers
	if paidUpfront {
		tiers = schedule.UpfrontTiers
	}
	var best *decimal.Decimal
	for i := range tiers {
		t := tiers[i]
		if t.StartAmount == nil {
			continue
		}
		if t.StartAmount.Cmp(monthly) <= 0 {
			d := t.Discount
			if d == nil {
				z := decimal.Zero
				d = &z
			}
			if best == nil || d.Cmp(*best) > 0 {
				best = d
			}
		}
	}
	if best == nil {
		return decimal.Zero, httpx.BadRequest("No savings plan found for the given monthly commited amount")
	}
	return *best, nil
}

// savingsContractBody holds the mutable fields of the SavingsContract request body on PUT update.
// Money fields are *decimal.Decimal (stored as a decimal string in jsonb). A nil pointer /
// blank string → the field is omitted from the update map, so it's dropped from the stored doc.
type savingsContractBody struct {
	BillingProfileID       string            `json:"billingProfileId"`
	SavingsPlanID          string            `json:"savingsPlanId"`
	SavingsPlanName        string            `json:"savingsPlanName"`
	Targets                []json.RawMessage `json:"targets"`
	StartDate              *time.Time        `json:"startDate"`
	EndDate                *time.Time        `json:"endDate"`
	DiscountRate           *decimal.Decimal  `json:"discountRate"`
	MonthlyCommittedAmount *decimal.Decimal  `json:"monthlyCommittedAmount"`
	PaidUpfront            bool              `json:"paidUpfront"`
	OrderID                string            `json:"orderId"`
	Status                 string            `json:"status"`
}

// SavingsPlanTargetBody holds a SavingsPlanTarget on the update body (resourceType + filters).
type SavingsPlanTargetBody struct {
	ResourceType string                   `json:"resourceType"`
	Filters      []map[string]interface{} `json:"filters"`
}

// setMap builds the map of overwritten update fields. Blank strings and nil pointers are omitted
// (a null/omitted body field is dropped from the stored doc).
func (b savingsContractBody) setMap() pgdoc.M {
	d := pgdoc.M{}
	if b.BillingProfileID != "" {
		d["billingProfileId"] = b.BillingProfileID
	}
	if b.SavingsPlanID != "" {
		d["savingsPlanId"] = b.SavingsPlanID
	}
	if b.SavingsPlanName != "" {
		d["savingsPlanName"] = b.SavingsPlanName
	}
	if b.Targets != nil {
		targets := make([]SavingsPlanTargetBody, 0, len(b.Targets))
		for _, raw := range b.Targets {
			var t SavingsPlanTargetBody
			if err := json.Unmarshal(raw, &t); err == nil {
				if t.Filters == nil {
					t.Filters = []map[string]interface{}{}
				}
				targets = append(targets, t)
			}
		}
		d["targets"] = targets
	}
	if b.StartDate != nil {
		d["startDate"] = *b.StartDate
	}
	if b.EndDate != nil {
		d["endDate"] = *b.EndDate
	}
	if b.DiscountRate != nil {
		d["discountRate"] = *b.DiscountRate
	}
	if b.MonthlyCommittedAmount != nil {
		d["monthlyCommittedAmount"] = *b.MonthlyCommittedAmount
	}
	if b.OrderID != "" {
		d["orderId"] = b.OrderID
	}
	if b.Status != "" {
		d["status"] = b.Status
	}
	return d
}

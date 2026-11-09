package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// bill.go serves the /api/v1/admin/bill surface — the MUTATIONS (PUT update, DELETE) plus
// the reads still missing from handler.go.
//
// Already registered in handler.go and intentionally NOT re-registered here:
//   - GET /admin/bill                       (bare list → h.listRaw("admin:bill:read","bill"))
//
// Newly registered here:
//   - PUT    /admin/bill/{id}                       update — status/invoiceCurrency/invoiceGatewayId/billingProfileId
//   - POST   /admin/bill/{id}/record-payment        record an offline (bank transfer / other) payment on a SENT bill
//   - DELETE /admin/bill/{id}                       delete
//   - GET    /admin/bill/{id}                       bill financial overview (pricing recompute — not wired)
//   - GET    /admin/bill/{billingProfileId}/billing-profile  bills for a billing profile → overview page (not wired)
//   - GET    /admin/bill/download/{billId}          statement PDF render (not wired)
//
// Perms (admin:* permission key):
//   - read endpoints  → ADMIN_BILL_READ   = admin:bill:read
//   - update/delete   → ADMIN_BILL_MANAGE = admin:bill:manage
//
// update/delete write audit events (UPDATE/DELETE BILL) — deferred this
// pass (// TODO(audit)); the persisted state + the response envelope are faithful.
//
// ⚠ MONEY/PRICING: the two overview reads build a BillFinancialOverview by RECOMPUTING
// net/gross/unpaid through the golden-tested pricing engine (gross amount, unpaid gross
// amount, and net amount in product currency with adjustments). That engine
// is not wired into the admin Handler (no new dep allowed this pass), and a raw passthrough would emit
// the wrong DTO shape + the wrong (un-taxed/un-FX'd) numbers — worse than failing. So both overview
// reads are NOT WIRED (501). The statement download renders a PDF — an external render,
// also not wired.

const billCollection = "bill"

const billReadPerm = "admin:bill:read"
const billManagePerm = "admin:bill:manage"

// routeBill registers the bill endpoints not already in handler.go. chi: the path
// position after `/bill/` already uses no param (the bare list is on `/bill`); the {id} param routes
// introduce `{id}`, and `download/{billId}` + `{billingProfileId}/billing-profile` must share that
// position carefully — `download` is a STATIC sibling of the `{id}` param (no conflict), while the
// `{billingProfileId}/billing-profile` two-segment route reuses the same first-segment param name
// `{id}` (chi requires one param name at a tree position) and is distinguished by its trailing
// `billing-profile` literal.
func (h *Handler) routeBill(r chi.Router) {
	r.Put("/bill/{id}", h.billUpdate)
	r.Post("/bill/{id}/record-payment", h.billRecordPayment)
	r.Delete("/bill/{id}", h.billDelete)
	r.Get("/bill/download/{billId}", h.billDownload)
	r.Get("/bill/{id}", h.billGet)
	r.Get("/bill/{id}/billing-profile", h.billsByBillingProfile)
}

// billNotFound builds the exact 404 returned when a bill id is absent,
// interpolating the id into the message using the same convention as the
// other admin 404s.
func billNotFound(id string) *httpx.HTTPError {
	return httpx.NotFound(fmt.Sprintf("Bill with id %s not found", id))
}

// billUpdateReq holds the four mutable fields the update copies from the request-body bill:
// status, invoiceCurrency, invoiceGatewayId, billingProfileId. (Exactly these four are overwritten —
// every other bill field, incl. items/createdAt, keeps the persisted value.) A
// null/omitted body field is dropped here when empty.
type billUpdateReq struct {
	Status           string `json:"status"`
	InvoiceCurrency  string `json:"invoiceCurrency"`
	InvoiceGatewayID string `json:"invoiceGatewayId"`
	BillingProfileID string `json:"billingProfileId"`
}

// setMap builds the update document for the four overwritten fields. A blank string is omitted
// so the field is dropped rather than written as empty.
func (req billUpdateReq) setMap() pgdoc.M {
	d := pgdoc.M{}
	if req.Status != "" {
		d["status"] = req.Status
	}
	if req.InvoiceCurrency != "" {
		d["invoiceCurrency"] = req.InvoiceCurrency
	}
	if req.InvoiceGatewayID != "" {
		d["invoiceGatewayId"] = req.InvoiceGatewayID
	}
	if req.BillingProfileID != "" {
		d["billingProfileId"] = req.BillingProfileID
	}
	return d
}

// billUpdate updates a bill: load by id or 404 → overwrite
// status/invoiceCurrency/invoiceGatewayId/billingProfileId from the body → save → return the bill.
// Gated ADMIN_BILL_MANAGE.
func (h *Handler) billUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req billUpdateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	if req.Status != "" && !isValidBillStatus(req.Status) {
		httpx.WriteError(w, httpx.BadRequest("Invalid bill status: must be one of OPEN, SENT, PAID"))
		return
	}
	found, err := h.repo.FindDoc(r.Context(), billCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if found == nil {
		httpx.WriteError(w, billNotFound(id))
		return
	}
	var existing, before pgdoc.M
	err = h.withBillLocks(r.Context(), []string{billProfileID(found), req.BillingProfileID}, func(ctx context.Context) error {
		var err error
		if existing, err = h.repo.FindDoc(ctx, billCollection, id); err != nil {
			return err
		}
		if existing == nil {
			return billNotFound(id)
		}
		before = maps.Clone(existing) // snapshot the pre-mutation state for the audit field-diff
		// Overwrite exactly the four editable fields (drop-first so an omitted/null body
		// field is cleared rather than left at its old value).
		for _, k := range []string{"status", "invoiceCurrency", "invoiceGatewayId", "billingProfileId"} {
			delete(existing, k)
		}
		for k, v := range req.setMap() {
			existing[k] = v
		}
		return h.repo.ReplaceDoc(ctx, billCollection, id, existing)
	})
	if httpx.WriteError(w, err) {
		return
	}
	// UPDATE audit: field-level before/after diff onto the audit event (the middleware
	// diffs the before and after snapshots into the event's changes). Re-read the AFTER from the datastore so
	// both snapshots are store-decoded (same store types/shape) — avoids spurious diffs from comparing
	// the Go-rebuilt map against the datastore-decoded `before`.
	after, _ := h.repo.FindDoc(r.Context(), billCollection, id)
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, shapeDoc(existing))
}

func billProfileID(doc pgdoc.M) string {
	id, _ := doc["billingProfileId"].(string)
	return id
}

// withBillLocks runs fn holding the profile lock of each profile a bill write touches (two when a bill
// moves between profiles), so it cannot undo a charge or a payment. A bill is replaced whole, and an
// open one is rewritten by the charge cron all the time. Sorted, so two moves cannot deadlock.
func (h *Handler) withBillLocks(ctx context.Context, profileIDs []string, fn func(ctx context.Context) error) error {
	slices.Sort(profileIDs)
	for _, id := range slices.Backward(slices.Compact(profileIDs)) {
		next := fn
		fn = func(ctx context.Context) error { return h.billing.WithProfileLock(ctx, id, next) }
	}
	return fn(ctx)
}

// isValidBillStatus reports whether s is one of the bill lifecycle statuses (OPEN → SENT → PAID).
func isValidBillStatus(s string) bool {
	switch pricing.BillStatus(s) {
	case pricing.BillStatusOpen, pricing.BillStatusSent, pricing.BillStatusPaid:
		return true
	}
	return false
}

// billRecordPaymentReq is the record-payment body. amount is a decimal (string or number);
// omitted → the bill's full unpaid amount. paidAt is RFC3339; omitted → now.
type billRecordPaymentReq struct {
	Amount    json.Number `json:"amount"`
	Method    string      `json:"method"` // BANK_TRANSFER | OTHER
	Reference string      `json:"reference"`
	PaidAt    string      `json:"paidAt"`
}

// billRecordPayment handles POST /admin/bill/{id}/record-payment: an accountant confirms an
// offline payment (bank transfer, …) against a SENT bill. The PayService records a SUCCESS
// collect transaction on the bill; a full amount flips it PAID (bill_is_paid mail + suspension
// auto-resume, like paying with credits), a partial one leaves it SENT with the collected amount.
// OPEN → 400 (not issued yet), PAID → 409. Returns the bill financial overview (GET /bill/{id}).
// Gated ADMIN_BILL_MANAGE; the audit middleware records the CREATE BILL event with the field diff.
func (h *Handler) billRecordPayment(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billManagePerm) {
		return
	}
	if h.pay == nil {
		httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented, "record payment not wired"))
		return
	}
	id := chi.URLParam(r, "id")
	var req billRecordPaymentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	p := billing.ManualPayment{
		Method:    strings.ToUpper(strings.TrimSpace(req.Method)),
		Reference: strings.TrimSpace(req.Reference),
	}
	if p.Method != "BANK_TRANSFER" && p.Method != "OTHER" {
		httpx.WriteError(w, httpx.BadRequest("method must be BANK_TRANSFER or OTHER"))
		return
	}
	if req.Amount != "" {
		d, err := decimal.NewFromString(req.Amount.String())
		if err != nil {
			httpx.WriteError(w, httpx.BadRequest("Invalid amount"))
			return
		}
		p.Amount = &d
	}
	if req.PaidAt != "" {
		t, err := time.Parse(time.RFC3339, req.PaidAt)
		if err != nil {
			httpx.WriteError(w, httpx.BadRequest("Invalid paidAt: RFC3339 timestamp expected"))
			return
		}
		p.PaidAt = t
	}
	if rc := httpx.RC(r.Context()); rc != nil {
		p.RecordedBy = rc.Sub
		if p.RecordedBy == "" {
			p.RecordedBy = rc.SigV4KeyID
		}
	}
	before, err := h.repo.FindDoc(r.Context(), billCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if before == nil {
		httpx.WriteError(w, billNotFound(id))
		return
	}
	_, _, err = h.pay.RecordManualPayment(r.Context(), id, p)
	switch {
	case errors.Is(err, billing.ErrBillNotFound):
		err = billNotFound(id)
	case errors.Is(err, billing.ErrBillAlreadyPaid):
		err = httpx.NewError(http.StatusConflict, http.StatusConflict, billing.ErrBillAlreadyPaid.Error())
	case errors.Is(err, billing.ErrBillNotIssued), errors.Is(err, billing.ErrInvalidAmount):
		err = httpx.BadRequest(err.Error())
	}
	if httpx.WriteError(w, err) {
		return
	}
	after, err := h.repo.FindDoc(r.Context(), billCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	audit.RecordSnapshots(r.Context(), before, after)
	// The payment is committed at this point; an orphan bill (profile gone) still gets a 200 with
	// the raw bill instead of the overview's 404, so the caller never retries a recorded payment.
	fo, err := h.billFinancialOverview(r.Context(), after)
	if err != nil {
		httpx.OK(w, shapeDoc(after))
		return
	}
	httpx.OK(w, fo)
}

// billDelete deletes a bill by id (the bill is NOT looked up first — the audit event is
// logged, then the delete runs unconditionally) → {data:"Successful operation"}.
// Gated ADMIN_BILL_MANAGE.
func (h *Handler) billDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	// The delete is idempotent (no 404 on a missing id) — the handler returns
	// success regardless. DeleteDoc returns 0 deleted for a missing id; we ignore the count.
	found, err := h.repo.FindDoc(r.Context(), billCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if found != nil {
		err = h.withBillLocks(r.Context(), []string{billProfileID(found)}, func(ctx context.Context) error {
			_, err := h.repo.DeleteDoc(ctx, billCollection, id)
			return err
		})
		if httpx.WriteError(w, err) {
			return
		}
	}
	// TODO(audit): write an admin audit event for the bill deletion (DELETE BILL, resourceId=id).
	httpx.OK(w, "Successful operation")
}

// billGet builds a BillFinancialOverview
// by recomputing net/gross/unpaid through the pricing engine. The engine is not wired into the
// admin Handler this pass and a raw passthrough would emit the wrong shape/numbers (see file header).
// Returns 404 first if the bill is absent,
// otherwise 501. Gated ADMIN_BILL_READ.
func (h *Handler) billGet(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billReadPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.repo.FindDoc(r.Context(), billCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, billNotFound(id))
		return
	}
	fo, err := h.billFinancialOverview(r.Context(), existing)
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, fo)
}

// billsByBillingProfile maps each bill for a billing profile to a BillFinancialOverview.
// Same pricing recompute as billGet (see file header). Gated ADMIN_BILL_READ.
func (h *Handler) billsByBillingProfile(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billReadPerm) {
		return
	}
	bpID := chi.URLParam(r, "id")
	bills, err := h.repo.ListRawFiltered(r.Context(), billCollection, pgdoc.M{"billingProfileId": bpID})
	if httpx.WriteError(w, err) {
		return
	}
	out := make([]pgdoc.M, 0, len(bills))
	for _, bd := range bills {
		fo, err := h.billFinancialOverview(r.Context(), bd)
		if httpx.WriteError(w, err) {
			return
		}
		out = append(out, fo)
	}
	httpx.List(w, out)
}

// billDownload renders the
// consumption-summary statement PDF and streams the
// bytes. Returns the bill 404 first if it is absent. Gated ADMIN_BILL_READ.
func (h *Handler) billDownload(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billReadPerm) {
		return
	}
	id := chi.URLParam(r, "billId")
	bill, err := h.billing.BillByID(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if bill == nil {
		httpx.WriteError(w, billNotFound(id))
		return
	}
	// Statement-for billing profile (best-effort: an empty profile still renders a valid PDF).
	bp, _ := h.billing.FindByID(r.Context(), bill.BillingProfileID)
	if bp == nil {
		bp = &billing.BillingProfile{ID: bill.BillingProfileID}
	}
	issuer, _ := h.billing.IssuerInfo(r.Context())
	var allRates []pricing.TaxRate
	if h.pricing != nil {
		allRates, _ = h.pricing.AllTaxRates(r.Context())
	}
	data, filename, err := billing.BillStatementPDF(bill, bp, issuer, billing.InvoiceTaxRates(allRates, bp, bill, time.Now().UTC()))
	if httpx.WriteError(w, err) {
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	_, _ = w.Write(data)
}

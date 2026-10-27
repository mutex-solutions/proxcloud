package admin

// promotionalcredit.go implements the promotional-credit surface (/api/v1/admin/promotional-credits) —
// the MUTATIONS (create / update / delete) + the by-billing-profile list. The plain by-id read
// (GET /promotional-credits/{id}) is already registered in handler.go (rawByID → 400 "Promotional
// credit not found"), so it is NOT re-registered here.
//
// Behavior:
//   - create:  daysValidity<=0 → 400 "Days validity must be greater than 0"; amount==null||<=0 →
//              400 "Amount must be greater than 0"; else persist {initialAmount, remainingAmount,
//              expirationDate=now+daysValidity days, billingProfileId, code=null} → the credit.
//   - list:    all credits for the billing profile → list envelope.
//   - delete:  load by id → if present delete (audit); ABSENT is a SILENT no-op (NO 404) →
//              "Successful operation".
//   - update:  load by (id, billingProfileId); if present mutate (amount→initial+remaining,
//              daysValidity!=0→new expiration, code!=null→code) + save; the handler ALWAYS
//              returns null regardless → an empty {} envelope. The PUT also reuses
//              the request's billingProfileId as the match key.
//
// Each mutation is audited — deferred this pass (// TODO(audit)); state +
// response are faithful, which is what the admin UI exercises. Perms: create/update/delete gate
// ADMIN_PROMOTIONAL_CREDIT_MANAGE; list gates ADMIN_ACCOUNT_CREDIT_READ (matching the
// authorization on each method).

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const (
	promoCreditManagePerm = "admin:promotional_credit:manage"
	promoCreditReadPerm   = "admin:account_credit:read"
	promotionalCreditColl = "promotionalCredit"
)

// routePromotionalCredit registers the promotional-credit mutations + list. The plain
// by-id GET is already registered in handler.go (rawByID) → intentionally omitted here.
func (h *Handler) routePromotionalCredit(r chi.Router) {
	r.Post("/promotional-credits", h.promoCreditCreate)
	r.Get("/promotional-credits/billing-profile/{billingProfileId}", h.promoCreditList)
	r.Put("/promotional-credits/{id}", h.promoCreditUpdate)
	r.Delete("/promotional-credits/{id}", h.promoCreditDelete)
}

// createPromotionalCreditReq is the create-promotional-credit request body. amount is decoded as
// json.Number so it round-trips decimals precisely and a missing/null amount is distinguishable
// from 0; daysValidity is a primitive int (0 means "unset" for the update path).
type createPromotionalCreditReq struct {
	Amount           json.Number `json:"amount"`
	DaysValidity     int         `json:"daysValidity"`
	BillingProfileID string      `json:"billingProfileId"`
}

// amountDecimal parses the request amount. ok=false when the field is absent/blank (null).
func (req createPromotionalCreditReq) amountDecimal() (amt decimal.Decimal, ok bool, err error) {
	s := req.Amount.String()
	if s == "" {
		return decimal.Decimal{}, false, nil
	}
	d, perr := decimal.NewFromString(s)
	if perr != nil {
		return decimal.Decimal{}, false, perr
	}
	return d, true, nil
}

// addDays adds calendar days to `now` — DST-naive at UTC, matching the original day-based
// advance well enough for the stored date.
func addDays(now time.Time, days int) time.Time {
	return now.AddDate(0, 0, days)
}

// promoCreditCreate creates a promotional credit: validate daysValidity>0 and amount>0, persist, return the credit.
func (h *Handler) promoCreditCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, promoCreditManagePerm) {
		return
	}
	var req createPromotionalCreditReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	// Order: check daysValidity FIRST, then amount.
	if req.DaysValidity <= 0 {
		httpx.WriteError(w, httpx.BadRequest("Days validity must be greater than 0"))
		return
	}
	amt, ok, err := req.amountDecimal()
	if err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	if !ok || amt.Cmp(decimal.Zero) <= 0 {
		httpx.WriteError(w, httpx.BadRequest("Amount must be greater than 0"))
		return
	}
	expiration := addDays(time.Now().UTC(), req.DaysValidity)
	doc := pgdoc.M{
		"initialAmount":    amt,
		"remainingAmount":  amt,
		"expirationDate":   expiration,
		"billingProfileId": req.BillingProfileID,
		// code is null on the admin create path (no code field) → omitted.
	}
	saved, err := h.repo.InsertDoc(r.Context(), promotionalCreditColl, doc)
	if httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write a CREATE admin audit event for the credit (amount).
	httpx.OK(w, shapeDoc(saved))
}

// promoCreditList lists all promotional credits for a billing profile → list envelope.
func (h *Handler) promoCreditList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, promoCreditReadPerm) {
		return
	}
	items, err := h.repo.ListRawFiltered(r.Context(), promotionalCreditColl,
		pgdoc.M{"billingProfileId": chi.URLParam(r, "billingProfileId")})
	if httpx.WriteError(w, err) {
		return
	}
	for i := range items {
		shapeDoc(items[i])
	}
	httpx.List(w, items)
}

// promoCreditUpdate updates a promotional credit: looks the credit
// up by (id, request.billingProfileId); if present, conditionally mutates fields and saves. The
// handler ALWAYS returns null (even when found), so the response is an empty {}
// envelope (httpx.Empty) in BOTH the found and not-found cases.
func (h *Handler) promoCreditUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, promoCreditManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req createPromotionalCreditReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	before, _ := h.repo.FindDoc(r.Context(), promotionalCreditColl, id) // raw pre-mutation snapshot for the audit diff
	existing, err := h.repo.PromotionalCreditByIDAndBillingProfile(r.Context(), id, req.BillingProfileID)
	if httpx.WriteError(w, err) {
		return
	}
	if existing != nil {
		set := pgdoc.M{}
		if amt, ok, perr := req.amountDecimal(); perr == nil && ok {
			set["initialAmount"] = amt
			set["remainingAmount"] = amt
		}
		if req.DaysValidity != 0 {
			set["expirationDate"] = addDays(time.Now().UTC(), req.DaysValidity)
		}
		// code is always null on this path → never set.
		if len(set) > 0 {
			if _, err := h.repo.SetFields(r.Context(), promotionalCreditColl, id, set); httpx.WriteError(w, err) {
				return
			}
		}
	}
	// UPDATE audit: field-level diff (the middleware diffs before vs after).
	after, _ := h.repo.FindDoc(r.Context(), promotionalCreditColl, id)
	audit.RecordSnapshots(r.Context(), before, after)
	// Returns null unconditionally → {} envelope.
	httpx.Empty(w)
}

// promoCreditDelete deletes a promotional credit: load by id → if present delete (audit). ABSENT =
// SILENT no-op (NO 404). Returns "Successful operation" either way.
func (h *Handler) promoCreditDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, promoCreditManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.repo.FindDoc(r.Context(), promotionalCreditColl, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing != nil {
		if _, err := h.repo.DeleteDoc(r.Context(), promotionalCreditColl, id); httpx.WriteError(w, err) {
			return
		}
		// TODO(audit): write a DELETE admin audit event for the credit.
	}
	httpx.OK(w, "Successful operation")
}

package admin

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/payment"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// banktransfer.go serves the MUTATIONS of the bank-transfer surface (/api/v1/admin/bank-transfer):
// approve + reject. The two reads (GET list, GET /{id}) are ALREADY registered in handler.go
// (h.bankTransferList / h.bankTransferByID) and are intentionally NOT re-registered here.
//
// Call graph:
//
//	approve(id) = approveBankTransfer(id) THEN processAddFunds(accId)
//	reject(id)  = rejectBankTransfer(id, null) THEN processAddFunds(accId)
//
// approveBankTransfer = getBankTransfer(id)-or-404 → status=APPROVED → save.
// rejectBankTransfer  = getBankTransfer(id)-or-404 → status=REJECTED, comments=comments(null) → save.
//
// LIVE since dev230: after the persisted status flip, processAddFunds runs through
// h.refund.ProcessBankTransfer — for the manual BankTransfer gateway that is a pure datastore
// settlement (processAddFunds reads the transfer's status; no vendor call):
// APPROVED credits the account (txn SUCCESS + spendable credit + review/bill-settle side-effects),
// REJECTED marks the txn FAILED with the transfer's comments. h.refund == nil (tests) → 501.
// Response = the updated bankTransfer doc (single(bankTransfer)).
// (An audit UPDATE event is also written — the middleware emits it.)

const bankTransferCollection = "bankTransfer"

const bankTransferManagePerm = "admin:transaction:manage"

// routeBankTransfer registers ONLY the bank-transfer mutation routes. The {id} param name reuses
// the one handler.go already uses on /bank-transfer/{id} (chi requires a single param name at a
// given path position).
func (h *Handler) routeBankTransfer(r chi.Router) {
	r.Post("/bank-transfer/{id}/approve", h.bankTransferApprove)
	r.Post("/bank-transfer/{id}/reject", h.bankTransferReject)
	r.Get("/bank-transfer/{id}/proof", h.bankTransferProof)
}

// bankTransferProof streams the customer's proof-of-payment file (admin:transaction:read).
func (h *Handler) bankTransferProof(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:transaction:read") {
		return
	}
	id := chi.URLParam(r, "id")
	bt, err := h.repo.FindDoc(r.Context(), bankTransferCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if bt == nil {
		httpx.WriteError(w, bankTransferNotFound(id))
		return
	}
	payment.WriteBankTransferProof(w, bt)
}

// bankTransferNotFound is the exact 404
// ("Bank transfer %s not found " — trailing space, interpolated).
func bankTransferNotFound(id string) *httpx.HTTPError {
	return httpx.NotFound(fmt.Sprintf("Bank transfer %s not found ", id))
}

// bankTransferApprove approves a transfer:
// getBankTransfer-or-404 → status=APPROVED → save → processAddFunds(accId).
func (h *Handler) bankTransferApprove(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, bankTransferManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.repo.FindDoc(r.Context(), bankTransferCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, bankTransferNotFound(id))
		return
	}
	// status=APPROVED, persisted (the datastore effect of approveBankTransfer).
	if _, err := h.repo.SetFields(r.Context(), bankTransferCollection, id, pgdoc.M{"status": "APPROVED"}); httpx.WriteError(w, err) {
		return
	}
	h.bankTransferSettle(w, r, id, "APPROVED")
}

// bankTransferSettle runs the processAddFunds tail after an approve/reject flip and writes
// the response (the updated bankTransfer doc, single(bankTransfer)).
func (h *Handler) bankTransferSettle(w http.ResponseWriter, r *http.Request, id, status string) {
	if h.refund == nil {
		httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented,
			"processAddFunds not implemented"))
		return
	}
	updated, err := h.repo.FindDoc(r.Context(), bankTransferCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	txnID, _ := updated["accountCreditTransactionId"].(string)
	comments, _ := updated["comments"].(string)
	if _, err := h.refund.ProcessBankTransfer(r.Context(), txnID, status, comments); httpx.WriteError(w, err) {
		return
	}
	// UPDATE BANK_TRANSFER audit — the middleware emits the admin event.
	httpx.OK(w, billing.BankTransferPublic(updated))
}

// bankTransferReject rejects a transfer (comments=null from the
// caller): getBankTransfer-or-404 → status=REJECTED, comments=null → save → processAddFunds.
func (h *Handler) bankTransferReject(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, bankTransferManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.repo.FindDoc(r.Context(), bankTransferCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, bankTransferNotFound(id))
		return
	}
	// status=REJECTED + comments CLEARED (the caller passes comments=null; an entity-save
	// DROPS null fields from the stored doc — $unset, not a stored null, else the raw read emits
	// "comments":null and breaks null-omission — live-caught on the dev230 drill).
	if _, err := h.repo.SetAndUnsetFields(r.Context(), bankTransferCollection, id,
		pgdoc.M{"status": "REJECTED"}, pgdoc.M{"comments": ""}); httpx.WriteError(w, err) {
		return
	}
	h.bankTransferSettle(w, r, id, "REJECTED")
}

package admin

// txn_reads.go implements the transaction admin GET reads filtered by paymentGatewayId
// across the three transaction types + the live-gateway sync (not wired).

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// txnByGateway lists a collection's transactions filtered by paymentGatewayId (the {id} path param
// here is the paymentGatewayId). Money is a decimal.Decimal stored as a decimal string.
// Gated ADMIN_TRANSACTION_READ.
func (h *Handler) txnByGateway(collection string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.require(w, r, "admin:transaction:read") {
			return
		}
		gw := chi.URLParam(r, "id")
		docs, err := h.repo.ListRawFiltered(r.Context(), collection, pgdoc.M{"paymentGatewayId": gw})
		if httpx.WriteError(w, err) {
			return
		}
		out := make([]pgdoc.M, 0, len(docs))
		for _, d := range docs {
			if sd, ok := shapeDeep(d).(pgdoc.M); ok {
				out = append(out, sd)
			}
		}
		httpx.List(w, out)
	}
}

// accountCreditTxnSync re-drives the add-funds flow for a transaction: it re-runs the gateway
// confirm (Stripe PI retrieve / BankTransfer doc dispatch) and returns the refreshed txn. Gated
// ADMIN_TRANSACTION_MANAGE (an earlier read-gate here was a bug — fixed dev232). h.refund unwired
// (tests) → 501 after the 404.
func (h *Handler) accountCreditTxnSync(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:transaction:manage") {
		return
	}
	id := chi.URLParam(r, "id")
	doc, err := h.repo.FindByIDRaw(r.Context(), "accountCreditTransaction", id)
	if httpx.WriteError(w, err) {
		return
	}
	if doc == nil {
		httpx.WriteError(w, httpx.NotFound("Transaction not found"))
		return
	}
	if h.refund == nil {
		httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented,
			"transaction gateway sync not implemented"))
		return
	}
	txn, err := h.refund.ProcessAddFunds(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, txn)
}

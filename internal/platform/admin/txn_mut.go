package admin

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// txn_mut.go implements the admin transaction record-edit MUTATIONS:
//   PUT/DELETE /admin/account-credit-transactions/{id}
//   PUT/DELETE /admin/collect-transactions/{id}
//   PUT/DELETE /admin/credit-card-transaction/{id}
// All gated ADMIN_TRANSACTION_MANAGE. update = load-or-404 → overwrite a fixed set of scalar
// fields → save → return the single record. delete = load-or-404 → delete by id → success message. These
// are pure record edits (the Stripe-touching one is the separate /refund/{id}, handled elsewhere). The
// collections are empty under greenfield (every call 404s) but the surface must exist (else 405).
//
// MONEY SAFETY: amount/exchangeRate/accountCredit are money fields (decimal.Decimal, stored as
// decimal strings in jsonb). The request body is decoded with UseNumber() and money fields are parsed
// into decimal.Decimal so they never round-trip through float64; the response goes through shapeDeep
// (decimal.Decimal → unquoted json.Number).

const txnPerm = "admin:transaction:manage"

// txnMoney is the set of money (decimal.Decimal) fields per transaction type.
var (
	accountCreditTxnFields = []string{"currency", "externalId", "amount", "invoiceGatewayId", "paymentGatewayId", "billingProfileId", "externalInvoiceId", "exchangeRate", "status", "gatewayMessage", "accountCredit"}
	collectTxnFields       = []string{"billId", "currency", "externalId", "amount", "errorMessage", "exchangeRate", "invoiceGatewayId", "paymentGatewayId", "billingProfileId", "externalInvoiceId", "status"}
	creditCardTxnFields    = []string{"externalId", "externalInvoiceId", "currency", "amount", "billingProfileId", "invoiceGatewayId", "paymentGatewayId", "exchangeRate", "metadata", "errorMessage", "status"}

	txnMoneyFields = map[string]bool{"amount": true, "exchangeRate": true, "accountCredit": true}
)

// routeTransactionMut registers the PUT/DELETE record edits for the 3 transaction collections.
func (h *Handler) routeTransactionMut(r chi.Router) {
	r.Put("/account-credit-transactions/{id}", h.txnUpdate("accountCreditTransaction", accountCreditTxnFields))
	r.Delete("/account-credit-transactions/{id}", h.txnDelete("accountCreditTransaction"))
	r.Put("/collect-transactions/{id}", h.txnUpdate("collectTransaction", collectTxnFields))
	r.Delete("/collect-transactions/{id}", h.txnDelete("collectTransaction"))
	r.Put("/credit-card-transaction/{id}", h.txnUpdate("creditCardTransaction", creditCardTxnFields))
	r.Delete("/credit-card-transaction/{id}", h.txnDelete("creditCardTransaction"))
}

// txnUpdate handles the transaction update: load-or-404 → overwrite the listed fields (money fields as
// decimal.Decimal) → save → return the single record. An absent/null req field clears it (nulls omitted on the way out).
func (h *Handler) txnUpdate(collection string, fields []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.require(w, r, txnPerm) {
			return
		}
		id := chi.URLParam(r, "id")
		existing, err := h.repo.FindDoc(r.Context(), collection, id)
		if httpx.WriteError(w, err) {
			return
		}
		if existing == nil {
			httpx.WriteError(w, httpx.NotFound("Transaction not found"))
			return
		}
		dec := json.NewDecoder(r.Body)
		dec.UseNumber()
		var req map[string]any
		if err := dec.Decode(&req); err != nil {
			httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
			return
		}
		for _, f := range fields {
			delete(existing, f)
			v, ok := req[f]
			if !ok || v == nil {
				continue
			}
			if txnMoneyFields[f] {
				if d, ok := toDecimalVal(v); ok {
					existing[f] = d
				}
				continue
			}
			existing[f] = v
		}
		if err := h.repo.ReplaceDoc(r.Context(), collection, id, existing); httpx.WriteError(w, err) {
			return
		}
		// TODO(audit): write an admin audit event for a transaction update (before/after diff).
		httpx.OK(w, shapeDeep(existing))
	}
}

// txnDelete handles the transaction delete: load-or-404 → delete by id → success message.
func (h *Handler) txnDelete(collection string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.require(w, r, txnPerm) {
			return
		}
		id := chi.URLParam(r, "id")
		existing, err := h.repo.FindDoc(r.Context(), collection, id)
		if httpx.WriteError(w, err) {
			return
		}
		if existing == nil {
			httpx.WriteError(w, httpx.NotFound("Transaction not found"))
			return
		}
		if _, err := h.repo.DeleteDoc(r.Context(), collection, id); httpx.WriteError(w, err) {
			return
		}
		// TODO(audit): write an admin audit event when a transaction is deleted (PLATFORM scope).
		httpx.OK(w, "Successful operation")
	}
}

// toDecimalVal parses a JSON money value (json.Number / string / float64) into a decimal.Decimal
// without losing precision through float (json.Number keeps the exact source text).
func toDecimalVal(v any) (decimal.Decimal, bool) {
	var s string
	switch n := v.(type) {
	case json.Number:
		s = n.String()
	case string:
		s = n
	case float64:
		s = strconv.FormatFloat(n, 'f', -1, 64)
	default:
		return decimal.Decimal{}, false
	}
	d, err := decimal.NewFromString(s)
	return d, err == nil
}

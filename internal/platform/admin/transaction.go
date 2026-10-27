package admin

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/paging"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// txnFilter builds the admin transaction-list WHERE filter from the query (optional
// ?billingProfileId= facet). Empty filter = all transactions.
func txnFilter(r *http.Request) pgdoc.M {
	f := pgdoc.M{}
	if bp := r.URL.Query().Get("billingProfileId"); bp != "" {
		f["billingProfileId"] = bp
	}
	return f
}

// transaction.go implements the transactions surface (/api/v1/admin/transactions). It has a
// single endpoint — GET /{billingProfileId} — that returns the UNION of a billing profile's collect
// transactions and account-credit transactions, each mapped into a unified DTO. There are
// NO mutations on this surface.
//
// The endpoint does, in order:
//
//	require ADMIN_TRANSACTION_READ
//	load the profile's collect transactions (newest first, all statuses) → map each to the DTO
//	load the profile's account-credit transactions (newest first, all statuses) → map each to the DTO
//	concatenate: collect first, then credit
//	return the list envelope {data:[…], paging}
//
// The two loaders are the SAME repo methods already used by the per-type by-billing-profile reads in
// handler.go (billing.Repo.AllCollectTransactionsByProfile / AllAccountCreditTransactionsByProfile —
// both createdAt DESC, all statuses). The per-element shape here is the merged DTO, NOT
// the per-type collect/account-credit DTOs, so it is mapped locally.

const transactionReadPerm = "admin:transaction:read"

// routeTransaction registers the transaction read routes. {billingProfileId} reuses the param name
// handler.go already uses on the sibling /…-transactions/{billingProfileId}/billing-profile routes;
// this path (/transactions/{billingProfileId}) is a distinct prefix so there is no chi conflict.
func (h *Handler) routeTransaction(r chi.Router) {
	r.Get("/transactions/{billingProfileId}", h.transactionsByBillingProfile)
	// Global (platform-wide) transaction lists — the old admin's Financial → Transactions showed
	// EVERY profile's transactions, not one profile's. These bare-collection GETs sit at a distinct
	// tree node from the /{id} and /{billingProfileId}/billing-profile routes (no chi conflict).
	r.Get("/account-credit-transactions", h.allAccountCreditTransactions)
	r.Get("/collect-transactions", h.allCollectTransactions)
	// Admin receipt download for a collect transaction (the old admin let operators pull the
	// per-transaction receipt PDF). Reuses billing.CollectReceiptPDF — the same ProxCloud-generated
	// receipt the client bill-history download serves. `download` is a static sibling so it does
	// not collide with the {billingProfileId} param at this node.
	r.Get("/collect-transactions/download/{transactionId}", h.collectTransactionDownload)
}

// collectTransactionDownload streams the receipt PDF for a collect transaction (id in the path).
// 404 if the transaction is absent; the billing profile is best-effort (an empty profile still
// renders a valid receipt). Gated ADMIN_TRANSACTION_READ.
func (h *Handler) collectTransactionDownload(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, transactionReadPerm) {
		return
	}
	id := chi.URLParam(r, "transactionId")
	txn, err := h.billing.CollectTransactionByID(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if txn == nil {
		httpx.WriteError(w, httpx.NotFound("Transaction "+id+" not found "))
		return
	}
	bp, _ := h.billing.FindByID(r.Context(), txn.BillingProfileID)
	if bp == nil {
		bp = &billing.BillingProfile{ID: txn.BillingProfileID}
	}
	data, filename, err := billing.CollectReceiptPDF(txn, bp)
	if httpx.WriteError(w, err) {
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	_, _ = w.Write(data)
}

// adminTransactionDto is the merged transaction wire shape.
// Both mappers target this one shape; the difference is which fields are set per
// source type (account-credit uses gatewayMessage as BOTH errorMessage and gatewayMessage; collect
// sets creditCardId). Every nullable field is omitempty so an unset field is dropped from
// the JSON. The `accountCredit` field is never populated by
// either mapper, so it is intentionally not emitted. Money is a JSON number (json.Number), not a
// quoted string.
type adminTransactionDto struct {
	ID                string         `json:"id,omitempty"`
	TransactionType   string         `json:"transactionType,omitempty"`
	Currency          string         `json:"currency,omitempty"`
	ExternalID        string         `json:"externalId,omitempty"`
	BillID            string         `json:"billId,omitempty"`
	Amount            json.Number    `json:"amount,omitempty"`
	ErrorMessage      string         `json:"errorMessage,omitempty"`
	GrossAmount       json.Number    `json:"grossAmount,omitempty"`
	InvoiceGatewayID  string         `json:"invoiceGatewayId,omitempty"`
	PaymentGatewayID  string         `json:"paymentGatewayId,omitempty"`
	BillingProfileID  string         `json:"billingProfileId,omitempty"`
	ExternalInvoiceID string         `json:"externalInvoiceId,omitempty"`
	ExchangeRate      json.Number    `json:"exchangeRate,omitempty"`
	CreditCardID      string         `json:"creditCardId,omitempty"`
	Status            string         `json:"status,omitempty"`
	GatewayMessage    string         `json:"gatewayMessage,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`
	CreatedAt         *time.Time     `json:"createdAt,omitempty"`
	UpdatedAt         *time.Time     `json:"updatedAt,omitempty"`
}

// transactionsByBillingProfile lists a billing profile's transactions: collect first, then account-credit,
// each mapped to the merged DTO, returned as a list envelope.
func (h *Handler) transactionsByBillingProfile(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, transactionReadPerm) {
		return
	}
	bp := chi.URLParam(r, "billingProfileId")

	collects, err := h.billing.AllCollectTransactionsByProfile(r.Context(), bp)
	if httpx.WriteError(w, err) {
		return
	}
	credits, err := h.billing.AllAccountCreditTransactionsByProfile(r.Context(), bp)
	if httpx.WriteError(w, err) {
		return
	}

	out := make([]adminTransactionDto, 0, len(collects)+len(credits))
	for i := range collects {
		out = append(out, mapCollectToTransaction(&collects[i]))
	}
	for i := range credits {
		out = append(out, mapAccountCreditToTransaction(&credits[i]))
	}
	httpx.List(w, out)
}

// allAccountCreditTransactions returns EVERY account-credit transaction platform-wide (the old
// admin's global Financial → Account credit transactions list), mapped to the merged DTO.
func (h *Handler) allAccountCreditTransactions(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, transactionReadPerm) {
		return
	}
	pg, ok := paging.FromRequest(w, r)
	if !ok {
		return
	}
	if pg.Active {
		credits, total, err := h.billing.AllAccountCreditTransactionsOffset(r.Context(), txnFilter(r), pg)
		if httpx.WriteError(w, err) {
			return
		}
		out := make([]adminTransactionDto, 0, len(credits))
		for i := range credits {
			out = append(out, mapAccountCreditToTransaction(&credits[i]))
		}
		httpx.Page(w, out, paging.OffsetPaging(pg, total))
		return
	}
	credits, err := h.billing.AllAccountCreditTransactions(r.Context())
	if httpx.WriteError(w, err) {
		return
	}
	out := make([]adminTransactionDto, 0, len(credits))
	for i := range credits {
		out = append(out, mapAccountCreditToTransaction(&credits[i]))
	}
	httpx.List(w, out)
}

// allCollectTransactions returns EVERY collect transaction platform-wide (the old admin's global
// Financial → Collect transactions list), mapped to the merged DTO.
func (h *Handler) allCollectTransactions(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, transactionReadPerm) {
		return
	}
	pg, ok := paging.FromRequest(w, r)
	if !ok {
		return
	}
	if pg.Active {
		collects, total, err := h.billing.AllCollectTransactionsOffset(r.Context(), txnFilter(r), pg)
		if httpx.WriteError(w, err) {
			return
		}
		out := make([]adminTransactionDto, 0, len(collects))
		for i := range collects {
			out = append(out, mapCollectToTransaction(&collects[i]))
		}
		httpx.Page(w, out, paging.OffsetPaging(pg, total))
		return
	}
	collects, err := h.billing.AllCollectTransactions(r.Context())
	if httpx.WriteError(w, err) {
		return
	}
	out := make([]adminTransactionDto, 0, len(collects))
	for i := range collects {
		out = append(out, mapCollectToTransaction(&collects[i]))
	}
	httpx.List(w, out)
}

// mapCollectToTransaction maps a CollectTransaction to the merged DTO: sets
// transactionType="collect" and the creditCardId; does NOT set gatewayMessage (collect has none).
func mapCollectToTransaction(t *pricing.CollectTransaction) adminTransactionDto {
	return adminTransactionDto{
		ID:                t.ID,
		TransactionType:   "collect",
		Currency:          t.Currency,
		ExternalID:        t.ExternalID,
		BillID:            t.BillID,
		Amount:            txnNum(t.Amount),
		ErrorMessage:      t.ErrorMessage,
		GrossAmount:       txnNum(t.GrossAmount),
		InvoiceGatewayID:  t.InvoiceGatewayID,
		PaymentGatewayID:  t.PaymentGatewayID,
		BillingProfileID:  t.BillingProfileID,
		ExternalInvoiceID: t.ExternalInvoiceID,
		ExchangeRate:      txnNum(t.ExchangeRate),
		CreditCardID:      t.CreditCardID,
		Status:            string(t.Status),
		Metadata:          t.Metadata,
		CreatedAt:         t.CreatedAt,
		UpdatedAt:         t.UpdatedAt,
	}
}

// mapAccountCreditToTransaction maps an AccountCreditTransaction to the merged DTO:
// sets transactionType="account-credit", uses gatewayMessage as BOTH errorMessage and gatewayMessage,
// and does NOT set creditCardId (account-credit has none).
func mapAccountCreditToTransaction(t *billing.AccountCreditTransaction) adminTransactionDto {
	return adminTransactionDto{
		ID:                t.ID,
		TransactionType:   "account-credit",
		Currency:          t.Currency,
		ExternalID:        t.ExternalID,
		BillID:            t.BillID,
		Amount:            txnNum(t.Amount),
		ErrorMessage:      t.GatewayMessage,
		GrossAmount:       txnNum(t.GrossAmount),
		InvoiceGatewayID:  t.InvoiceGatewayID,
		PaymentGatewayID:  t.PaymentGatewayID,
		BillingProfileID:  t.BillingProfileID,
		ExternalInvoiceID: t.ExternalInvoiceID,
		ExchangeRate:      txnNum(t.ExchangeRate),
		Status:            t.Status,
		GatewayMessage:    t.GatewayMessage,
		Metadata:          map[string]any(t.Metadata),
		CreatedAt:         t.CreatedAt,
		UpdatedAt:         t.UpdatedAt,
	}
}

// txnNum renders a nullable money Decimal as a JSON number, or "" (dropped via omitempty) when nil —
// a JSON number, with a null amount omitted.
func txnNum(d *decimal.Decimal) json.Number {
	if d == nil {
		return ""
	}
	return json.Number(d.String())
}

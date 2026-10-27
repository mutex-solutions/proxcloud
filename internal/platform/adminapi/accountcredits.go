package adminapi

// accountcredits.go serves /admin-api/v1/account_credits. Create follows the /api/v1/admin
// build: amount/initialAmount as full-precision decimals, currency =
// billingConfiguration.baseCurrency, invoiceCurrency = the profile's; a cross-currency create
// needs the external ExchangeClient (not wired) → 501.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
)

type apiAccountCredit struct {
	ID               string      `json:"id,omitempty"`
	BillingProfileID string      `json:"billing_profile_id,omitempty"`
	InitialAmount    json.Number `json:"initial_amount,omitempty"`
	Amount           json.Number `json:"amount,omitempty"`
	Currency         string      `json:"currency,omitempty"`
	CreatedAt        *time.Time  `json:"created_at,omitempty"`
	UpdatedAt        *time.Time  `json:"updated_at,omitempty"`
}

// creditDoc is the stored accountCredit shape (money = a decimal string).
type creditDoc struct {
	ID               string           `json:"id,omitempty"`
	BillingProfileID string           `json:"billingProfileId,omitempty"`
	InitialAmount    decimal.Decimal  `json:"initialAmount,omitempty"`
	Amount           decimal.Decimal  `json:"amount,omitempty"`
	Currency         string           `json:"currency,omitempty"`
	InvoiceCurrency  string           `json:"invoiceCurrency,omitempty"`
	InvoiceRate      *decimal.Decimal `json:"invoiceExchangeRate,omitempty"`
	CreatedAt        *time.Time       `json:"createdAt,omitempty"`
	UpdatedAt        *time.Time       `json:"updatedAt,omitempty"`
}

func mapCredit(c *creditDoc) apiAccountCredit {
	return apiAccountCredit{
		ID: c.ID, BillingProfileID: c.BillingProfileID,
		InitialAmount: json.Number(c.InitialAmount.String()), Amount: json.Number(c.Amount.String()),
		Currency: c.Currency, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

func (h *Handler) routeAccountCredits(r chi.Router) {
	r.Get("/account_credits", h.creditsList)
	r.Post("/account_credits", h.creditCreate)
	r.Get("/account_credits/{id}", h.creditGet)
	r.Delete("/account_credits/{id}", h.creditDelete)
}

func (h *Handler) creditsList(w http.ResponseWriter, r *http.Request) {
	req, ok := listParams(w, r)
	if !ok {
		return
	}
	bpID := r.URL.Query().Get("billing_profile_id")
	// The controller first resolves the profile (getBillingProfile → 404 when absent).
	if found, err := h.db.C("billingProfile").Exists(r.Context(), pgdoc.M{"_id": bpID}); err != nil || !found {
		apiNotFound(w)
		return
	}
	credits, err := findPage[creditDoc](r.Context(), h.db.C("accountCredit"), pgdoc.M{"billingProfileId": bpID}, req)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	page, next := pageOut(req, credits, func(c creditDoc) string { return c.ID })
	out := make([]apiAccountCredit, 0, len(page))
	for i := range page {
		out = append(out, mapCredit(&page[i]))
	}
	writeList(w, out, next)
}

func (h *Handler) creditGet(w http.ResponseWriter, r *http.Request) {
	var c creditDoc
	if found, err := h.db.C("accountCredit").Get(r.Context(), chi.URLParam(r, "id"), &c); err != nil || !found {
		apiNotFound(w)
		return
	}
	writeEntity(w, mapCredit(&c))
}

func (h *Handler) creditCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BillingProfileID string      `json:"billing_profile_id"`
		Amount           json.Number `json:"amount"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	var bp struct {
		Currency string `json:"currency"`
	}
	if found, err := h.db.C("billingProfile").Get(r.Context(), req.BillingProfileID, &bp); err != nil || !found {
		apiNotFound(w)
		return
	}
	var cfg struct {
		BaseCurrency string `json:"baseCurrency"`
	}
	_, _ = h.db.C("billingConfiguration").FindOne(r.Context(), pgdoc.M{}, &cfg)
	if cfg.BaseCurrency != bp.Currency {
		seam(w, "account credit exchange-rate lookup not implemented")
		return
	}
	amt, err := decimal.NewFromString(req.Amount.String())
	if err != nil {
		badRequest(w, "Invalid amount")
		return
	}
	one := decimal.NewFromInt(1)
	now := nowUTC()
	c := creditDoc{
		ID: newID(), BillingProfileID: req.BillingProfileID,
		InitialAmount: amt, Amount: amt,
		Currency: cfg.BaseCurrency, InvoiceCurrency: bp.Currency, InvoiceRate: &one,
		CreatedAt: &now, UpdatedAt: &now,
	}
	if _, err := h.db.C("accountCredit").InsertOne(r.Context(), &c); err != nil {
		badRequest(w, err.Error())
		return
	}
	// Audit with EventContext ORGANIZATION for account credits.
	ev := h.actorEvent(r)
	ev.EventContext = audit.ContextOrganization
	ev.Action = audit.ActionCreate
	ev.ResourceType = "ACCOUNT_CREDIT"
	ev.ResourceID = c.ID
	ev.Outcome = audit.OutcomeSuccess
	if h.audit != nil {
		h.audit.LogAsync(ev)
	}
	writeEntity(w, mapCredit(&c))
}

func (h *Handler) creditDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ok, err := h.db.C("accountCredit").Exists(r.Context(), pgdoc.M{"_id": id})
	if err != nil || !ok {
		apiNotFound(w)
		return
	}
	if _, err := h.db.C("accountCredit").DeleteByID(r.Context(), id); err != nil {
		badRequest(w, err.Error())
		return
	}
	ev := h.actorEvent(r)
	ev.EventContext = audit.ContextOrganization
	ev.Action = audit.ActionDelete
	ev.ResourceType = "ACCOUNT_CREDIT"
	ev.ResourceID = id
	ev.Outcome = audit.OutcomeSuccess
	if h.audit != nil {
		h.audit.LogAsync(ev)
	}
	w.WriteHeader(http.StatusAccepted)
}

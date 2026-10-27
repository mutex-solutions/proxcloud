package org

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/order"
	"github.com/mutex-solutions/proxcloud/internal/platform/paging"
	"github.com/mutex-solutions/proxcloud/internal/platform/payment"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// BillingHandler serves the client billing-profile read endpoints (the
// billing-profile GET paths). Lives in the org package because it needs
// org membership/permission resolution (org already depends on billing → no cycle).
type BillingHandler struct {
	svc          *Service
	policy       *Policy
	billing      *billing.Repo
	pricing      *pricing.Repo                // tax rates for BillDto gross (nil-safe: nil → no tax)
	pay          *billing.PayService          // pay a SENT bill from the credit balance
	addFunds     *payment.AddFundsService     // deposit via a payment gateway (Stripe)
	registerCard *payment.RegisterCardService // add a card (SetupIntent)
	collect      *payment.CollectService      // deposit by charging a saved card (collect)
	uiBaseURL    string                       // redirect target for the payment confirm callbacks
	users        *user.Repo
	orders       *order.Repo // create-order route
}

func NewBillingHandler(svc *Service, policy *Policy, billingRepo *billing.Repo, pricingRepo *pricing.Repo, pay *billing.PayService, addFunds *payment.AddFundsService, registerCard *payment.RegisterCardService, collect *payment.CollectService, uiBaseURL string, users *user.Repo, orders *order.Repo) *BillingHandler {
	return &BillingHandler{svc: svc, policy: policy, billing: billingRepo, pricing: pricingRepo, pay: pay, addFunds: addFunds, registerCard: registerCard, collect: collect, uiBaseURL: uiBaseURL, users: users, orders: orders}
}

func (h *BillingHandler) Routes(r chi.Router) {
	r.Get("/billing-profile", h.list)
	r.Post("/billing-profile", h.create)
	r.Get("/billing-profile/countries", h.countries) // static segment — chi prefers it over {billingProfileId}
	r.Get("/billing-profile/{billingProfileId}", h.get)
	r.Put("/billing-profile/{billingProfileId}", h.update)
	r.Delete("/billing-profile/{billingProfileId}", h.delete)
	r.Get("/billing-profile/{billingProfileId}/restricted", h.restricted)
	r.Get("/billing-profile/{billingProfileId}/validation", h.validation)
	// Identity-validation document submit (POST /{id}/validation,
	// multipart) → external KYC provider; not configured here → 501 after access-check.
	r.Post("/billing-profile/{billingProfileId}/validation", h.validationUpload)
	// Billing-domain client list endpoints (empty under the greenfield seed; per-element
	// DTO mapping deferred — see billing.Repo.listRaw).
	r.Get("/bill/gateways", h.billInvoiceGateways) // static — chi prefers it over {billingProfileId}
	r.Get("/bill/{billingProfileId}", h.bills)
	// Org billing dashboard: profile aggregate + per-project breakdown (static "cost-info"
	// wins over the {billId} sibling at this node).
	r.Get("/bill/{billingProfileId}/cost-info", h.orgCostInfo)
	r.Get("/bill/{billingProfileId}/{billId}", h.billByID) // single bill detail
	// Client bill statement PDF (the "download" static
	// segment wins over the {billId} param sibling).
	r.Get("/bill/{billingProfileId}/download/{billId}/statement", h.billStatementDownload)
	r.Get("/promotional-credits/{billingProfileId}", h.promotionalCredits)
	r.Get("/savings-contracts/{billingProfileId}", h.savingsContracts)
	// SavingsContract client mutations. chi: position-2 param shares ONE
	// name {savingsContractId}; the eligible route reads it as the savingsPlanId.
	r.Post("/savings-contracts/{billingProfileId}", h.createSavingsContract)
	r.Delete("/savings-contracts/{billingProfileId}/{savingsContractId}", h.cancelSavingsContract)
	r.Post("/savings-contracts/{billingProfileId}/{savingsContractId}/cancel-non-upfront", h.cancelSavingsContract)
	r.Post("/savings-contracts/{billingProfileId}/{savingsContractId}/extend", h.extendSavingsContract)
	r.Get("/savings-contracts/{billingProfileId}/{savingsContractId}/eligible", h.savingsEligible)
	r.Get("/savings-plans", h.savingsPlans)                                           // ?billingProfileId=
	r.Get("/account-credit-transactions", h.accountCreditTransactions)                // ?billingProfileId=
	r.Get("/account-credit-transactions/{transactionId}", h.accountCreditTransaction) // single (404 by id)
	// Invoice PDF download. The PDF render is an
	// external invoice-provider call → 501 (same posture as the admin bill statement download). chi:
	// the position-1 param must reuse the {transactionId} node name; the real txn id is {downloadTxnId}.
	r.Get("/account-credit-transactions/{transactionId}/download/{downloadTxnId}", h.accountCreditTxnDownload)
	r.Get("/collect-transactions", h.collectTransactions) // ?billingProfileId=
	// Single collect transaction by id. The param
	// name must match the sibling {billingProfileId} routes at this chi tree node; it carries the txn id.
	r.Get("/collect-transactions/{billingProfileId}", h.collectTransactionByID)
	r.Get("/collect-transactions/{billingProfileId}/bill/{billId}", h.collectTransactionsByBill)
	// Create order: POST /api/v1/orders/{billingProfileId} (the GET /orders/{id} lives in
	// the order package; create lives here for the billing-profile access gate + tax).
	r.Post("/orders/{billingProfileId}", h.createOrder)
	// Collect-transaction invoice PDF download → 501.
	r.Get("/collect-transactions/{billingProfileId}/download/{transactionId}", h.collectTxnDownload)
	// pay a SENT bill from the credit balance (no gateway).
	r.Post("/payment/{billingProfileId}/bill/{billId}/pay", h.payBill)
	// list the available payment gateways for a profile.
	r.Get("/payment/{billingProfileId}/gateway", h.paymentGateways)
	// add funds via a gateway (→ PaymentIntent). The path is
	// /payment/deposit/{bpId} (static "deposit" precedes the {billingProfileId} param routes).
	r.Post("/payment/deposit/{billingProfileId}", h.deposit)
	// deposit by charging a saved card (→ collect,
	// synchronous confirm=true). cardId is in the CollectRequest body, not the path.
	r.Post("/payment/deposit/{billingProfileId}/card", h.depositByCard)
	// Stripe redirect confirm callback (whitelisted in auth — no bearer; finalizes the txn).
	r.Get("/callbacks/payment/stripe/funds/confirm/{transactionId}", h.stripeFundsConfirm)
	// register a card (SetupIntent) + list saved cards + the card-confirm callback.
	r.Post("/card/{billingProfileId}/add", h.addCard)
	r.Get("/card/{billingProfileId}", h.listCards)
	// Card CRUD: delete a card (path param is the CARD id — chi forces it to share the
	// {billingProfileId} param name at this node, read as cardId), set-default, get a card transaction.
	r.Delete("/card/{billingProfileId}", h.deleteCard)
	r.Post("/card/{billingProfileId}/{cardId}/default", h.setDefaultCard)
	r.Get("/card/{billingProfileId}/transactions/{transactionId}", h.getCardTransaction)
	r.Get("/callbacks/payment/stripe/card/confirm/{transactionId}", h.stripeCardConfirm)
	// KYC + websso callbacks. These integrations are NOT configured in this
	// deployment → 501 (would verify with the vendor; the posture for an
	// unconfigured external integration is a 501). Whitelisted (no bearer).
	r.Post("/callbacks/auth/websso", h.callbackSeam)
	// KYC verification (multipart → an external KYC provider) — same 501 posture.
	r.Post("/kyc/{projectId}", h.callbackSeam)
}

// callbackSeam is the 501 for KYC/SSO integrations that are not configured in this
// deployment. It would verify with the vendor;
// when no such integration exists, the external call is not wired.
func (h *BillingHandler) callbackSeam(w http.ResponseWriter, r *http.Request) {
	httpx.Err(w, http.StatusNotImplemented, http.StatusNotImplemented, "payment/KYC gateway not configured")
}

// requireBillingProfileRead resolves and read-gates a profile:
// load the profile by id → 404 (interpolated) if absent → resolve its org, 404 "Billing Profile has
// no organization associated" → BILLING_PROFILE_READ 403. Used by the bill/promotional-credit/
// savings client list endpoints.
func (h *BillingHandler) requireBillingProfileRead(w http.ResponseWriter, r *http.Request, u *user.User, bpID string) (*billing.BillingProfile, bool) {
	bp, err := h.billing.FindByID(r.Context(), bpID)
	if err != nil {
		fail(w, err)
		return nil, false
	}
	if bp == nil {
		fail(w, httpx.NotFound(fmt.Sprintf("Billing profile with id %s not found. ", bpID)))
		return nil, false
	}
	o, err := h.svc.FindOrganization(r.Context(), bp.OrganizationID)
	if err != nil {
		fail(w, err)
		return nil, false
	}
	if o == nil {
		fail(w, httpx.NotFound("Billing Profile has no organization associated"))
		return nil, false
	}
	if !h.policy.HasPermission(r.Context(), u.Sub, o.ID, rbac.BillingProfileRead) {
		fail(w, httpx.Forbidden("You do not have permission to perform this action: "+rbac.Description(rbac.BillingProfileRead)))
		return nil, false
	}
	return bp, true
}

// orgCostInfo handles GET /bill/{billingProfileId}/cost-info: the org-wide billing overview for the
// org billing dashboard — the profile aggregate plus a per-project breakdown ({projectId: CostInfo},
// only projects that have billed items). Reuses the same cost breakdown as the project dashboard,
// so category/top-resource logic stays single-sourced. BILLING_PROFILE_READ gated.
func (h *BillingHandler) orgCostInfo(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, ok := h.requireBillingProfileRead(w, r, u, chi.URLParam(r, "billingProfileId"))
	if !ok {
		return
	}
	now := time.Now().UTC()
	bills, err := h.billing.BillsByBillingProfile(r.Context(), bp.ID)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"billingProfileCostInfo": billing.CostInfoMap(billing.BillCostBreakdown(bills, now, nil)),
		"projects":               billing.ProjectCostInfoMap(bills, now, nil),
		"currency":               bp.Currency,
	})
}

// bills lists a profile's bills (→ list envelope).
// billInvoiceGateways handles GET /api/v1/bill/gateways →
// the available invoice gateways: the invoice gateways the client Bill History
// Transactions tab renders. No User/billing-profile — a flat list (the FE was calling this and the
// {billingProfileId} param route was swallowing "gateways" → "Billing profile with id gateways not
// found"; a static segment fixes it).
func (h *BillingHandler) billInvoiceGateways(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.principal(w, r); !ok {
		return
	}
	gws, err := h.billing.ListInvoiceGateways(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	httpx.List(w, gws)
}

// billByID handles GET /api/v1/bill/{billingProfileId}/{billId}: one bill of
// the profile as a BillDto. 404 "not_found" when the bill is absent or belongs to another profile.
func (h *BillingHandler) billByID(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	// Invoice reads gate on BILLING_PROFILE_READ_INVOICES (not the coarse billing_profile:read).
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileReadInvoices)
	if !ok {
		return
	}
	bill, err := h.billing.BillByID(r.Context(), chi.URLParam(r, "billId"))
	if err != nil {
		fail(w, err)
		return
	}
	if bill == nil || bill.BillingProfileID != bp.ID {
		fail(w, httpx.NotFound("not_found"))
		return
	}
	dtos, err := h.toBillDtos(r.Context(), bp, []pricing.Bill{*bill})
	if err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, dtos[0])
}

// collectTransactionsByBill handles
// GET /api/v1/collect-transactions/{billingProfileId}/bill/{billId}: the profile's collect
// transactions filtered to one bill — the Bill-detail page's payments list.
func (h *BillingHandler) collectTransactionsByBill(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileReadTransactions)
	if !ok {
		return
	}
	billID := chi.URLParam(r, "billId")
	txs, err := h.billing.CollectTransactionsByBillingProfile(r.Context(), bp.ID)
	if err != nil {
		fail(w, err)
		return
	}
	out := []pricing.CollectTransaction{}
	for i := range txs {
		if txs[i].BillID == billID {
			out = append(out, txs[i])
		}
	}
	httpx.List(w, billing.CollectTransactionsToDtos(out))
}

func (h *BillingHandler) bills(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	// Invoice reads gate on BILLING_PROFILE_READ_INVOICES (not the coarse billing_profile:read).
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileReadInvoices)
	if !ok {
		return
	}
	pg, ok := paging.FromRequest(w, r)
	if !ok {
		return
	}
	if pg.Active {
		bills, next, prev, err := h.billing.BillsPage(r.Context(), bp.ID, pg)
		if err != nil {
			fail(w, err)
			return
		}
		dtos, err := h.toBillDtos(r.Context(), bp, bills)
		if err != nil {
			fail(w, err)
			return
		}
		httpx.CursorList(w, dtos, pg.Limit, next, prev)
		return
	}
	bills, err := h.billing.BillsByBillingProfile(r.Context(), bp.ID)
	if err != nil {
		fail(w, err)
		return
	}
	dtos, err := h.toBillDtos(r.Context(), bp, bills)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.List(w, dtos)
}

// billStatementDownload handles GET /bill/{bpId}/download/{billId}/
// statement: the profile read gate, the bill-of-profile 404
// ("Bill %s not found " when absent or owned by another profile), then the consumption-summary statement PDF —
// the same billing.BillStatementPDF render the admin download streams. The PDF content type is set
// explicitly so the browser doesn't sniff.
func (h *BillingHandler) billStatementDownload(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	// Statement download gates on BILLING_PROFILE_DOWNLOAD_INVOICES (not the coarse billing_profile:read).
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileDownloadInvoices)
	if !ok {
		return
	}
	billID := chi.URLParam(r, "billId")
	bill, err := h.billing.BillByID(r.Context(), billID)
	if err != nil {
		fail(w, err)
		return
	}
	if bill == nil || bill.BillingProfileID != bp.ID {
		fail(w, httpx.NotFound(fmt.Sprintf("Bill %s not found ", billID)))
		return
	}
	data, _, err := billing.BillStatementPDF(bill, bp)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	_, _ = w.Write(data)
}

// toBillDtos maps bills via billing.ToBillDto: the profile's tax
// rates (nil pricing repo or no rates → no tax) + base currency + a same-currency Exchanger
// (live FX deferred). Empty bills → empty slice (the common seed case).
func (h *BillingHandler) toBillDtos(ctx context.Context, bp *billing.BillingProfile, bills []pricing.Bill) ([]billing.BillDto, error) {
	baseCcy, _ := h.billing.BaseCurrency(ctx)
	now := time.Now().UTC()
	var rates []pricing.TaxRate
	if h.pricing != nil {
		all, _ := h.pricing.AllTaxRates(ctx)
		rates = pricing.SelectTaxRates(all, bp.Country, bp.Company, now)
	}
	x := pricing.NewExchanger(nil)
	out := make([]billing.BillDto, 0, len(bills))
	for i := range bills {
		d, err := billing.ToBillDto(bp, &bills[i], rates, baseCcy, x, now)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, nil
}

// promotionalCredits lists a profile's promotional credits.
func (h *BillingHandler) promotionalCredits(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, ok := h.requireBillingProfileRead(w, r, u, chi.URLParam(r, "billingProfileId"))
	if !ok {
		return
	}
	pcs, err := h.billing.PromotionalCreditsByBillingProfile(r.Context(), bp.ID)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.List(w, billing.PromotionalCreditsToDtos(pcs))
}

// savingsContracts lists a profile's savings contracts (single envelope,
// NOT the list/paging envelope).
func (h *BillingHandler) savingsContracts(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, ok := h.requireBillingProfileRead(w, r, u, chi.URLParam(r, "billingProfileId"))
	if !ok {
		return
	}
	scs, err := h.billing.SavingsContractsByBillingProfile(r.Context(), bp.ID)
	if err != nil {
		fail(w, err)
		return
	}
	dtos := make([]billing.SavingsContractDto, 0, len(scs))
	for i := range scs {
		dtos = append(dtos, billing.SavingsContractToDto(&scs[i]))
	}
	httpx.OK(w, dtos)
}

// savingsPlans lists the savings plans available to a profile
// (?billingProfileId= query param; list envelope).
func (h *BillingHandler) savingsPlans(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireBillingProfileRead(w, r, u, r.URL.Query().Get("billingProfileId")); !ok {
		return
	}
	plans, err := h.billing.AvailableSavingsPlans(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	dtos := make([]billing.SavingsPlanDto, 0, len(plans))
	for i := range plans {
		dtos = append(dtos, billing.SavingsPlanToDto(&plans[i]))
	}
	httpx.List(w, dtos)
}

// accountCreditTransactions lists a profile's account-credit transactions
// (?billingProfileId=; BILLING_PROFILE_READ_TRANSACTIONS-gated; list envelope).
func (h *BillingHandler) accountCreditTransactions(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, r.URL.Query().Get("billingProfileId"), rbac.BillingProfileReadTransactions)
	if !ok {
		return
	}
	pg, ok := paging.FromRequest(w, r)
	if !ok {
		return
	}
	if pg.Active {
		items, next, prev, err := h.billing.ListAccountCreditTransactionsPage(r.Context(), bp.ID, pg)
		if err != nil {
			fail(w, err)
			return
		}
		httpx.CursorList(w, billing.AccountCreditTransactionsToDtos(items), pg.Limit, next, prev)
		return
	}
	items, err := h.billing.ListAccountCreditTransactions(r.Context(), bp.ID)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.List(w, billing.AccountCreditTransactionsToDtos(items))
}

// accountCreditTransaction gets one transaction by id:
// require the caller → load the transaction by id (404 "Transaction %s not found " if absent)
// → access-check the caller against the transaction's billing profile. The happy path's typed DTO + access check are
// deferred; under the seed the table is empty so the 404 path is what's exercised.
func (h *BillingHandler) accountCreditTransaction(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "transactionId")
	txn, err := h.billing.AccountCreditTransactionByID(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	if txn == nil {
		fail(w, httpx.NotFound(fmt.Sprintf("Transaction %s not found ", id)))
		return
	}
	// access-check against the transaction's billing profile: a caller must be a reading member of the
	// transaction's billing profile — mirrors collectTransactionByID (no cross-tenant txn read).
	if _, _, ok := h.requireBillingProfilePermission(w, r, u, txn.BillingProfileID, rbac.BillingProfileReadTransactions); !ok {
		return
	}
	httpx.OK(w, billing.AccountCreditTransactionToDto(txn))
}

// collectTransactions lists a profile's collect transactions
// (?billingProfileId=; BILLING_PROFILE_READ_TRANSACTIONS-gated; list envelope).
func (h *BillingHandler) collectTransactions(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, r.URL.Query().Get("billingProfileId"), rbac.BillingProfileReadTransactions)
	if !ok {
		return
	}
	pg, ok := paging.FromRequest(w, r)
	if !ok {
		return
	}
	if pg.Active {
		txs, next, prev, err := h.billing.CollectTransactionsPage(r.Context(), bp.ID, pg)
		if err != nil {
			fail(w, err)
			return
		}
		httpx.CursorList(w, billing.CollectTransactionsToDtos(txs), pg.Limit, next, prev)
		return
	}
	txs, err := h.billing.CollectTransactionsByBillingProfile(r.Context(), bp.ID)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.List(w, billing.CollectTransactionsToDtos(txs))
}

// collectTransactionByID gets one collect transaction by id:
// load it by id (404 "Transaction %s not found ") → access-check the caller against
// the transaction's billing profile → single DTO. Registered under the {billingProfileId} param name
// (chi: it shares the tree node with the /{billingProfileId}/bill|download routes, so the param name
// must match) but the value is the transaction id.
func (h *BillingHandler) collectTransactionByID(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "billingProfileId") // shared param name; this is the txn id
	txn, err := h.billing.CollectTransactionByID(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	if txn == nil {
		fail(w, httpx.NotFound(fmt.Sprintf("Transaction %s not found ", id)))
		return
	}
	if _, _, ok := h.requireBillingProfilePermission(w, r, u, txn.BillingProfileID, rbac.BillingProfileReadTransactions); !ok {
		return
	}
	httpx.OK(w, billing.CollectTransactionToDto(txn))
}

// payBill pays a SENT bill from the profile's credit balance (from the balance;
// no external gateway). Read-gated like the other billing reads; returns
// the updated BillDto (single envelope). 400s: already-paid / open-bill / not-enough-credit.
func (h *BillingHandler) payBill(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileUpdate)
	if !ok {
		return
	}
	bill, err := h.pay.PayBillWithCredits(r.Context(), bp, chi.URLParam(r, "billId"), time.Now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, billing.ErrBillNotFound):
			httpx.Err(w, http.StatusNotFound, http.StatusNotFound, err.Error())
		case errors.Is(err, billing.ErrBillAlreadyPaid), errors.Is(err, billing.ErrCannotPayOpenBill), errors.Is(err, billing.ErrNotEnoughCredit):
			httpx.Err(w, http.StatusBadRequest, http.StatusBadRequest, err.Error())
		default:
			fail(w, err)
		}
		return
	}
	dtos, err := h.toBillDtos(r.Context(), bp, []pricing.Bill{*bill})
	if err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, dtos[0])
}

// paymentGateways lists the available payment gateways for a profile. Membership +
// BILLING_PROFILE_READ gated (via getBillingProfile); empty under the greenfield seed.
func (h *BillingHandler) paymentGateways(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireBillingProfileRead(w, r, u, chi.URLParam(r, "billingProfileId")); !ok {
		return
	}
	gws, err := h.billing.ListGateways(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	httpx.List(w, gws)
}

// deposit creates a gateway PaymentIntent for a deposit and
// return its client secret (single envelope). Membership + BILLING_PROFILE_READ gated.
func (h *BillingHandler) deposit(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileAddFunds)
	if !ok {
		return
	}
	var req payment.AddFundsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, httpx.BadRequest("invalid request body"))
		return
	}
	resp, err := h.addFunds.AddFunds(r.Context(), bp, req)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, *resp)
}

// depositByCard charges a saved card to deposit funds.
// Membership + BILLING_PROFILE_READ gated (getBillingProfile).
// Synchronous (Stripe confirm=true, no callback); returns the terminal CollectTransaction.
func (h *BillingHandler) depositByCard(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileAddFunds)
	if !ok {
		return
	}
	var req payment.CollectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, httpx.BadRequest("invalid request body"))
		return
	}
	txn, err := h.collect.CollectByCard(r.Context(), bp, req)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, billing.CollectTransactionToDto(txn))
}

// stripeFundsConfirm is the whitelisted
// redirect target Stripe sends the browser back to. Finalizes the transaction (retrieve the
// PaymentIntent → credit on success) then 302-redirects to the UI (never errors to the user —
// failures still redirect to the UI base URL).
// stripeFundsConfirm finalizes a deposit: the client fetches it (cross-origin, api host)
// after confirming the PaymentIntent in-page, so it returns 200 — a redirect to the UI base
// would be followed cross-origin by fetch and fail CORS ("Failed to fetch").
func (h *BillingHandler) stripeFundsConfirm(w http.ResponseWriter, r *http.Request) {
	txnID := chi.URLParam(r, "transactionId")
	if _, err := h.addFunds.ProcessAddFunds(r.Context(), txnID); err != nil {
		fail(w, err)
		return
	}
	httpx.Empty(w)
}

// addCard registers a card (SetupIntent) → client secret.
func (h *BillingHandler) addCard(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileManagePaymentMethods)
	if !ok {
		return
	}
	var req payment.RegisterCardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, httpx.BadRequest("invalid request body"))
		return
	}
	resp, err := h.registerCard.RegisterCard(r.Context(), bp, req)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, *resp)
}

// listCards returns a profile's stored cards (empty under seed).
func (h *BillingHandler) listCards(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, ok := h.requireBillingProfileRead(w, r, u, chi.URLParam(r, "billingProfileId"))
	if !ok {
		return
	}
	cards, err := h.billing.CreditCardsByBillingProfile(r.Context(), bp.ID)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.List(w, cards)
}

// stripeCardConfirm is the whitelisted redirect
// target finalizing card registration (retrieve SetupIntent → store card) then 302 to the UI.
// stripeCardConfirm finalizes an add-card (retrieves the SetupIntent, stores the card): the
// client fetches it cross-origin after the in-page SetupIntent confirmation, so it returns 200
// (a redirect to the UI base would be followed by fetch cross-origin and fail CORS).
func (h *BillingHandler) stripeCardConfirm(w http.ResponseWriter, r *http.Request) {
	txnID := chi.URLParam(r, "transactionId")
	if _, err := h.registerCard.ProcessRegisterCard(r.Context(), txnID); err != nil {
		fail(w, err)
		return
	}
	httpx.Empty(w)
}

// deleteCard finds the card by
// id (404 "Card not found "), access-checks via its billing profile, then deletes (pure-DB) → 204.
// The {billingProfileId} path param actually carries the CARD id (chi node param-name reuse).
func (h *BillingHandler) deleteCard(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	cardID := chi.URLParam(r, "billingProfileId")
	card, err := h.billing.CreditCardByID(r.Context(), cardID)
	if err != nil {
		fail(w, err)
		return
	}
	if card == nil {
		fail(w, httpx.NotFound("Card not found "))
		return
	}
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, card.BillingProfileID, rbac.BillingProfileManagePaymentMethods)
	if !ok {
		return
	}
	if err := h.billing.DeleteCreditCard(r.Context(), bp.ID, cardID); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setDefaultCard resolves the card by id+profile (404 "Could not find the card with id %s . "), sets the profile's
// defaultCardId, persist, return the profile.
func (h *BillingHandler) setDefaultCard(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileManagePaymentMethods)
	if !ok {
		return
	}
	cardID := chi.URLParam(r, "cardId")
	card, err := h.billing.CreditCardByIDAndBillingProfile(r.Context(), cardID, bp.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if card == nil {
		fail(w, httpx.NotFound(fmt.Sprintf("Could not find the card with id %s . ", cardID)))
		return
	}
	bp.DefaultCardID = card.ID
	updated, err := h.billing.Update(r.Context(), bp)
	if err != nil {
		fail(w, err)
		return
	}
	// Same response shape as the other billing-profile endpoints (the BillingSummary DTO, proper
	// JSON field names) — not the raw domain struct (which would serialize Go field names).
	httpx.OK(w, billing.ToSummary(updated))
}

// getCardTransaction returns a card transaction scoped to the
// profile (404 "Credit card transaction with id %s not found ").
func (h *BillingHandler) getCardTransaction(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileReadTransactions)
	if !ok {
		return
	}
	txnID := chi.URLParam(r, "transactionId")
	txn, err := h.billing.CreditCardTransactionByIDAndBillingProfile(r.Context(), txnID, bp.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if txn == nil {
		fail(w, httpx.NotFound(fmt.Sprintf("Credit card transaction with id %s not found ", txnID)))
		return
	}
	httpx.OK(w, *txn)
}

// accountCreditTxnDownload access-checks the
// profile, then returns 501: the invoice PDF is rendered by an external invoice provider (not implemented here).
// The {transactionId} path param carries the billingProfileId (chi node param-name reuse).
func (h *BillingHandler) accountCreditTxnDownload(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireBillingProfileRead(w, r, u, chi.URLParam(r, "transactionId")); !ok {
		return
	}
	httpx.Err(w, http.StatusNotImplemented, http.StatusNotImplemented, "invoice PDF download not implemented")
}

// collectTxnDownload access-checks then returns a self-contained receipt PDF.
func (h *BillingHandler) collectTxnDownload(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileReadTransactions)
	if !ok {
		return
	}
	txnID := chi.URLParam(r, "transactionId")
	txn, err := h.billing.CollectTransactionByID(r.Context(), txnID)
	if err != nil {
		fail(w, err)
		return
	}
	if txn == nil || txn.BillingProfileID != bp.ID {
		fail(w, httpx.NotFound(fmt.Sprintf("Transaction %s not found ", txnID)))
		return
	}
	// An external invoice gateway would proxy a PDF; the demo has none, so ProxCloud generates a
	// self-contained receipt instead (functional, not the external invoice).
	data, filename, err := billing.CollectReceiptPDF(txn, bp)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	_, _ = w.Write(data)
}

// validationUpload accepts a multipart identity
// document submitted for KYC validation. The document is processed by an external KYC provider (not
// configured here) → 501 after the membership/access check (the vendor is called when configured).
func (h *BillingHandler) validationUpload(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	if _, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileReadInvoices); !ok {
		return
	}
	httpx.Err(w, http.StatusNotImplemented, http.StatusNotImplemented, "identity validation provider not configured")
}

// createOrder computes per-item tax (profile tax
// rates), totals, and persists an Order(status=CREATED). Dispatching an order to a
// type-specific handler is deferred — savings contracts are created via their own endpoint.
func (h *BillingHandler) createOrder(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileUpdate)
	if !ok {
		return
	}
	var req order.Order
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, httpx.BadRequest("invalid request body"))
		return
	}
	var rates []pricing.TaxRate
	if h.pricing != nil {
		all, _ := h.pricing.AllTaxRates(r.Context())
		rates = pricing.SelectTaxRates(all, bp.Country, bp.Company, time.Now().UTC())
	}
	totalNet, totalTax := decimal.Zero, decimal.Zero
	for i := range req.Items {
		net := decimal.Zero
		if req.Items[i].NetAmount != nil {
			net = *req.Items[i].NetAmount
		}
		tax := pricing.CalculateTaxAmount(net, rates)
		req.Items[i].TaxAmount = &tax
		totalNet = totalNet.Add(net)
		totalTax = totalTax.Add(tax)
	}
	o := &order.Order{
		BillingProfileID: bp.ID, NetAmount: &totalNet, TaxAmount: &totalTax,
		Items: req.Items, Status: order.OrderStatusCreated,
	}
	created, err := h.orders.Create(r.Context(), o)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, created)
}

// createSavingsContract creates a savings contract.
// Body {savingsPlanId, durationMonths, monthlyCommittedAmount, paidUpfront, startDate}.
func (h *BillingHandler) createSavingsContract(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileUpdate)
	if !ok {
		return
	}
	var req struct {
		SavingsPlanID          string      `json:"savingsPlanId"`
		DurationMonths         int         `json:"durationMonths"`
		MonthlyCommittedAmount json.Number `json:"monthlyCommittedAmount"`
		PaidUpfront            bool        `json:"paidUpfront"`
		StartDate              string      `json:"startDate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, httpx.BadRequest("invalid request body"))
		return
	}
	monthly, _ := decimal.NewFromString(req.MonthlyCommittedAmount.String())
	c, err := h.billing.CreateSavingsContract(r.Context(), bp.ID, billing.CreateSavingsContractInput{
		SavingsPlanID: req.SavingsPlanID, DurationMonths: req.DurationMonths,
		MonthlyCommittedAmount: monthly, PaidUpfront: req.PaidUpfront, StartDate: req.StartDate,
	}, time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, billing.SavingsContractToDto(c))
}

// requireSavingsContract loads a contract owned by the path billing-profile (404 "Savings contract
// not found" when absent or owned by another profile), after the membership/read gate.
func (h *BillingHandler) requireSavingsContract(w http.ResponseWriter, r *http.Request) (*billing.SavingsContract, bool) {
	u, ok := h.principal(w, r)
	if !ok {
		return nil, false
	}
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileUpdate)
	if !ok {
		return nil, false
	}
	c, err := h.billing.SavingsContractByID(r.Context(), chi.URLParam(r, "savingsContractId"))
	if err != nil {
		fail(w, err)
		return nil, false
	}
	if c == nil || c.BillingProfileID != bp.ID {
		fail(w, httpx.NotFound("Savings contract not found"))
		return nil, false
	}
	return c, true
}

// cancelSavingsContract cancels a contract (also the cancel-non-upfront route — the
// guard rejects upfront contracts either way): reject upfront / non-ACTIVE, flip to CANCELLED.
func (h *BillingHandler) cancelSavingsContract(w http.ResponseWriter, r *http.Request) {
	c, ok := h.requireSavingsContract(w, r)
	if !ok {
		return
	}
	if c.PaidUpfront {
		fail(w, httpx.BadRequest("Cannot cancel upfront contracts"))
		return
	}
	if c.Status != billing.SavingsStatusActive {
		fail(w, httpx.BadRequest("Only active contracts can be cancelled"))
		return
	}
	c.Status = billing.SavingsStatusCancelled
	if err := h.billing.SaveSavingsContract(r.Context(), c); err != nil {
		fail(w, err)
		return
	}
	// cancel → cancelReminders (drop pending expiry reminders). Best-effort.
	_ = h.billing.CancelReminders(r.Context(), billing.ResourceTypeSavingsContract, c.ID)
	httpx.OK(w, billing.SavingsContractToDto(c))
}

// extendSavingsContract handles extend: ACTIVE-only; endDate += durationMonths.
func (h *BillingHandler) extendSavingsContract(w http.ResponseWriter, r *http.Request) {
	c, ok := h.requireSavingsContract(w, r)
	if !ok {
		return
	}
	if c.Status != billing.SavingsStatusActive {
		fail(w, httpx.BadRequest("Only active contracts can be extended"))
		return
	}
	if c.EndDate != nil {
		ne := c.EndDate.AddDate(0, c.DurationMonths, 0)
		c.EndDate = &ne
	}
	if err := h.billing.SaveSavingsContract(r.Context(), c); err != nil {
		fail(w, err)
		return
	}
	// extend → cancelReminders: the old window is dropped so the next daily run schedules a
	// fresh reminder against the new endDate. Best-effort.
	_ = h.billing.CancelReminders(r.Context(), billing.ResourceTypeSavingsContract, c.ID)
	httpx.OK(w, billing.SavingsContractToDto(c))
}

// savingsEligible reports true when the profile has
// no ACTIVE contract for the (available) plan. The {savingsContractId} path param is the planId here.
func (h *BillingHandler) savingsEligible(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, ok := h.requireBillingProfileRead(w, r, u, chi.URLParam(r, "billingProfileId"))
	if !ok {
		return
	}
	planID := chi.URLParam(r, "savingsContractId")
	plan, err := h.billing.AvailableSavingsPlanByID(r.Context(), planID)
	if err != nil {
		fail(w, err)
		return
	}
	if plan == nil {
		fail(w, httpx.NotFound("Savings plan not found"))
		return
	}
	exists, err := h.billing.ExistsActiveSavingsContract(r.Context(), plan.ID, bp.ID)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, !exists)
}

// restricted reports whether the profile is payment-restricted by overdue bills
// (true when the profile has overdue bills).
// Gated by BILLING_PROFILE_READ via requireBillingProfilePermission.
func (h *BillingHandler) restricted(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileRead)
	if !ok {
		return
	}
	overdue, err := h.billing.HasOverdueBills(r.Context(), bp.ID)
	if err != nil {
		fail(w, err)
		return
	}
	out := billing.Restricted{BillingProfileID: bp.ID, Restricted: overdue}
	if overdue {
		out.Reason = "You have outstanding bills"
	}
	httpx.OK(w, out)
}

// validation returns the profile's identity validation.
// Gated by BILLING_PROFILE_READ_INVOICES.
// No identityValidationId → empty IdentityValidation ({}); otherwise the stored record
// with its document blanked (this model omits the document field entirely).
func (h *BillingHandler) validation(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileReadInvoices)
	if !ok {
		return
	}
	if strings.TrimSpace(bp.IdentityValidationID) == "" {
		httpx.OK(w, billing.IdentityValidation{})
		return
	}
	v, err := h.billing.GetIdentityValidationByID(r.Context(), bp.IdentityValidationID)
	if err != nil {
		fail(w, err)
		return
	}
	if v == nil {
		fail(w, httpx.NotFound(fmt.Sprintf("The account validation with id %s was not found. ", bp.IdentityValidationID)))
		return
	}
	httpx.OK(w, *v)
}

// requireBillingProfilePermission resolves a profile with the correct
// error-path precedence: (1) load the profile by the PATH id → 404
// "Billing profile with id %s not found. " (id interpolated, trailing space) if absent;
// (2) resolve a member-owned org → 404 "Billing Profile not found"; (3) enforce the
// permission → 403. Returns the loaded profile + its org, or false after writing the error.
func (h *BillingHandler) requireBillingProfilePermission(w http.ResponseWriter, r *http.Request, u *user.User, bpID, perm string) (*billing.BillingProfile, *Organization, bool) {
	bp, err := h.billing.FindByID(r.Context(), bpID)
	if err != nil {
		fail(w, err)
		return nil, nil, false
	}
	if bp == nil {
		fail(w, httpx.NotFound(fmt.Sprintf("Billing profile with id %s not found. ", bpID)))
		return nil, nil, false
	}
	o, err := h.svc.GetOrganizationForBillingProfile(r.Context(), bpID, u.Sub)
	if err != nil {
		fail(w, err)
		return nil, nil, false
	}
	if !h.policy.HasPermission(r.Context(), u.Sub, o.ID, perm) {
		fail(w, httpx.Forbidden("You do not have permission to perform this action: "+rbac.Description(perm)))
		return nil, nil, false
	}
	return bp, o, true
}

// delete removes a billing profile:
// gated by BILLING_PROFILE_DELETE on the owning org; returns 202 Accepted (empty).
func (h *BillingHandler) delete(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileDelete)
	if !ok {
		return
	}
	if err := h.billing.Delete(r.Context(), bp.ID); err != nil {
		fail(w, err)
		return
	}
	httpx.Accepted(w)
}

// countries returns the static billing country list. Authed via the RS layer but takes
// no User/permission.
func (h *BillingHandler) countries(w http.ResponseWriter, r *http.Request) {
	httpx.List(w, billing.Countries())
}

func (h *BillingHandler) principal(w http.ResponseWriter, r *http.Request) (*user.User, bool) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		fail(w, err)
		return nil, false
	}
	return u, true
}

// get returns one profile's summary; membership-gated only (no explicit
// permission check beyond getOrganizationForBillingProfile).
func (h *BillingHandler) get(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	o, err := h.svc.GetOrganizationForBillingProfile(r.Context(), chi.URLParam(r, "billingProfileId"), u.Sub)
	if err != nil {
		fail(w, err)
		return
	}
	bp, err := h.billing.FindByID(r.Context(), o.BillingProfileID)
	if err != nil {
		fail(w, err)
		return
	}
	if bp == nil {
		// Dangling org→profile ref: loading the profile by org.billingProfileId
		// yields the interpolated 404. Edge only (shouldn't happen).
		fail(w, httpx.NotFound(fmt.Sprintf("Billing profile with id %s not found. ", o.BillingProfileID)))
		return
	}
	httpx.OK(w, billing.ToSummary(bp).WithFinancials(r.Context(), h.billing, time.Now().UTC()))
}

// update fills/edits billing details. Gated by BILLING_PROFILE_UPDATE
// on the owning org; merges the request body, normalizes the phone, runs the
// auto-activation flow, persists, and returns the updated summary.
func (h *BillingHandler) update(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	bp, _, ok := h.requireBillingProfilePermission(w, r, u, chi.URLParam(r, "billingProfileId"), rbac.BillingProfileUpdate)
	if !ok {
		return
	}
	var input billing.BillingProfile
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		fail(w, httpx.BadRequest("Invalid request body"))
		return
	}
	saved, err := h.billing.PopulateBillingData(r.Context(), bp, &input)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, billing.ToSummary(saved))
}

// create makes a fresh billing profile for an org the caller belongs to:
// resolve the org for the caller (404 org / 400
// non-member) → BILLING_PROFILE_CREATE (403) → create the profile for the org. The
// profile is built from the org's OWNER (not the caller) and does NOT update
// organization.billingProfileId (a repeat call orphans a profile). Returns the
// new profile's summary. NOTE: a blank organizationId validation returns `errors`
// as an ARRAY (not the single-object envelope) — that edge is not tested here
// (same class as the malformed-body 500), only the valid create is.
func (h *BillingHandler) create(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	var req struct {
		OrganizationID string `json:"organizationId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, httpx.BadRequest("Invalid request body"))
		return
	}
	o, err := h.svc.GetOrganizationForUser(r.Context(), req.OrganizationID, u.Sub)
	if err != nil {
		fail(w, err)
		return
	}
	if !h.policy.HasPermission(r.Context(), u.Sub, o.ID, rbac.BillingProfileCreate) {
		fail(w, httpx.Forbidden("You do not have permission to perform this action: "+rbac.Description(rbac.BillingProfileCreate)))
		return
	}
	// Create the profile for the org: build from the org OWNER member's user.
	members, err := h.svc.Members(r.Context(), o.ID)
	if err != nil {
		fail(w, err)
		return
	}
	var owner *user.User
	for i := range members {
		if members[i].Role() == rbac.RoleOwner {
			owner, err = h.users.FindBySub(r.Context(), members[i].Sub)
			if err != nil {
				fail(w, err)
				return
			}
			break
		}
	}
	if owner == nil {
		fail(w, httpx.BadRequest("Organization has no owner"))
		return
	}
	bpID, err := h.billing.CreateForOrganization(r.Context(), o.ID, billing.Owner{
		Sub: owner.Sub, Email: owner.Email, FirstName: owner.FirstName,
		LastName: owner.LastName, FullName: owner.FullName(),
	})
	if err != nil {
		fail(w, err)
		return
	}
	bp, err := h.billing.FindByID(r.Context(), bpID)
	if err != nil {
		fail(w, err)
		return
	}
	if bp == nil {
		fail(w, httpx.NotFound(fmt.Sprintf("Billing profile with id %s not found. ", bpID)))
		return
	}
	httpx.OK(w, billing.ToSummary(bp).WithFinancials(r.Context(), h.billing, time.Now().UTC()))
}

// list returns the summaries of every billing profile the user can read
// (orgs with a profile + BILLING_PROFILE_READ, deduped).
func (h *BillingHandler) list(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	orgs, err := h.svc.GetOrganizationsForUser(r.Context(), u.Sub)
	if err != nil {
		fail(w, err)
		return
	}
	seen := map[string]bool{}
	summaries := make([]billing.Summary, 0, len(orgs))
	for i := range orgs {
		o := &orgs[i]
		if o.BillingProfileID == "" || seen[o.BillingProfileID] {
			continue
		}
		if !h.policy.HasPermission(r.Context(), u.Sub, o.ID, rbac.BillingProfileRead) {
			continue
		}
		bp, _ := h.billing.FindByID(r.Context(), o.BillingProfileID)
		if bp == nil {
			continue
		}
		seen[o.BillingProfileID] = true
		summaries = append(summaries, billing.ToSummary(bp).WithFinancials(r.Context(), h.billing, time.Now().UTC()))
	}
	httpx.List(w, summaries)
}

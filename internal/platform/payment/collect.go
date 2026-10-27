package payment

import (
	"context"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// collect.go charges a saved card. Stripe confirm=true is SYNCHRONOUS →
// no redirect callback; the flow creates a PENDING CollectTransaction, charges,
// then processes the result inline. Two entry points:
//   - CollectByCard       — client deposit-by-card → AccountCredit.
//   - CollectBillingProfile/CollectAll — the monthlyCollect cron: collect each SENT bill via the
//     profile's current card → applyPaidCollectOnBill (→ bill PAID).
// Invoice/affiliate/suspension/email downstream are deferred (gated subsystems).

// CollectRequest is the collect request {cardId, orderId, amount}.
type CollectRequest struct {
	CardID  string           `json:"cardId"`
	OrderID string           `json:"orderId"`
	Amount  *decimal.Decimal `json:"amount"`
}

// CollectService orchestrates card collection. The gateway is built per-integration via gatewayFor
// (NewStripeGateway in prod; a fake in tests) so the flows are testable.
type CollectService struct {
	billing     *billing.Repo
	pricing     *pricing.Repo
	gatewayFor  func(secretKey string) Gateway
	notifier    billing.Notifier
	reviewer    billing.ProfileReviewer
	orderStatus func(ctx context.Context, orderID, billingProfileID string, gross decimal.Decimal, status string) error
}

func NewCollectService(b *billing.Repo, p *pricing.Repo, gatewayFor func(secretKey string) Gateway) *CollectService {
	return &CollectService{billing: b, pricing: p, gatewayFor: gatewayFor}
}

// SetReviewer wires the suspension auto-resume hook (re-review the billing profile
// after a successful collect). Nil → no-op.
func (s *CollectService) SetReviewer(r billing.ProfileReviewer) { s.reviewer = r }

// SetOrderStatusUpdater wires the order flip (mark the order PAID when a collect
// paid an order). The updater binds the flip to the paying profile + gross amount. Nil → no-op.
func (s *CollectService) SetOrderStatusUpdater(f func(ctx context.Context, orderID, billingProfileID string, gross decimal.Decimal, status string) error) {
	s.orderStatus = f
}

// SetNotifier wires the email hook (collect-by-card thank-you). Nil → no-op.
func (s *CollectService) SetNotifier(n billing.Notifier) { s.notifier = n }

// CollectByCard handles client deposit-by-card: load the card + its gateway, create a
// PENDING CollectTransaction (FX→tax to a gross amount), charge the card, then process the result
// inline. No billId → SUCCESS creates a spendable AccountCredit. Returns the terminal transaction.
func (s *CollectService) CollectByCard(ctx context.Context, profile *billing.BillingProfile, req CollectRequest) (*pricing.CollectTransaction, error) {
	if req.CardID == "" {
		return nil, httpx.BadRequest("Card id is required.")
	}
	if req.Amount == nil {
		return nil, httpx.BadRequest("Amount is required.")
	}
	// Bind the card to the CALLER's billing profile: a card id owned by another
	// profile must not be chargeable by this caller (deposit-by-card IDOR).
	card, err := s.billing.CreditCardByIDAndBillingProfile(ctx, req.CardID, profile.ID)
	if err != nil {
		return nil, err
	}
	if card == nil {
		return nil, httpx.NotFound("Credit card not found")
	}
	gw, err := s.billing.GetGateway(ctx, card.PaymentGatewayID)
	if err != nil {
		return nil, err
	}
	if gw == nil {
		return nil, httpx.NotFound("Payment gateway not found")
	}

	now := time.Now().UTC()
	amount, gross, rate, err := s.fxTax(ctx, profile, *req.Amount, now)
	if err != nil {
		return nil, err
	}
	txn := &pricing.CollectTransaction{
		Currency:         profile.Currency,
		Amount:           &amount,
		OrderID:          req.OrderID,
		ExchangeRate:     &rate,
		InvoiceGatewayID: gw.ID, // default-invoice-gateway resolution deferred → payment gw
		PaymentGatewayID: card.PaymentGatewayID,
		BillingProfileID: profile.ID,
		Status:           pricing.CollectTransactionStatusPending,
		CreditCardID:     card.ID,
		GrossAmount:      &gross,
		Metadata:         map[string]any{},
	}
	if txn, err = s.billing.SaveCollectTransaction(ctx, txn); err != nil {
		return nil, err
	}
	return s.runCollect(ctx, txn, card.TokenID)
}

// CollectAll is the monthly collect job (per-profile fan-out):
// collect every profile's SENT bills via its current card. Best-effort per profile/bill (a single
// failure does not abort the run). Returns the number of bills that ended up PAID.
func (s *CollectService) CollectAll(ctx context.Context) (int, error) {
	profiles, err := s.billing.AllBillingProfiles(ctx)
	if err != nil {
		return 0, err
	}
	paid := 0
	for i := range profiles {
		n, _ := s.CollectBillingProfile(ctx, &profiles[i])
		paid += n
	}
	return paid, nil
}

// CollectBillingProfile collects each SENT bill via the
// profile's current card. SUSPENDED → skip. No card → skip (the no-card-notify email is deferred).
// Returns the number of bills that ended PAID. Best-effort per bill.
func (s *CollectService) CollectBillingProfile(ctx context.Context, profile *billing.BillingProfile) (int, error) {
	if profile.Status == billing.StatusSuspended {
		return 0, nil
	}
	bills, err := s.billing.SentBills(ctx, profile.ID)
	if err != nil {
		return 0, err
	}
	if len(bills) == 0 {
		return 0, nil
	}
	now := time.Now().UTC()
	card, err := s.billing.CurrentCreditCard(ctx, profile.ID, profile.DefaultCardID, now)
	if err != nil {
		return 0, err
	}
	if card == nil {
		// the customer has SENT bills but no usable card — warn them
		// (template notify_customer_has_no_card: fullName/balance/currency). Best-effort.
		s.notifyNoCard(ctx, profile)
		return 0, nil
	}
	paid := 0
	for i := range bills {
		ok, err := s.collectBill(ctx, profile, &bills[i], card, now)
		if err != nil {
			continue // best-effort: a per-bill gateway error must not abort the rest
		}
		if ok {
			paid++
		}
	}
	return paid, nil
}

// collectBill collects one SENT bill: CollectTransaction(billId, unpaidAmount) → charge → process.
// Returns whether the bill ended PAID.
func (s *CollectService) collectBill(ctx context.Context, profile *billing.BillingProfile, bill *pricing.Bill, card *billing.CreditCard, now time.Time) (bool, error) {
	baseCcy, _ := s.billing.BaseCurrency(ctx)
	x := pricing.NewExchanger(nil)
	rate, err := x.GetExchangeRate(baseCcy, profile.Currency, now)
	if err != nil {
		return false, err
	}
	// unpaid is in product (base) currency; collect amount is in the profile/invoice currency.
	unpaid := pricing.GetUnpaidAmountBillProductCurrency(bill)
	amount := rate.Mul(unpaid)
	var rates []pricing.TaxRate
	if s.pricing != nil {
		all, _ := s.pricing.AllTaxRates(ctx)
		rates = pricing.SelectTaxRates(all, profile.Country, profile.Company, now)
	}
	gross := pricing.CalculateGrossAmount(amount, rates)
	txn := &pricing.CollectTransaction{
		BillID:           bill.ID,
		Currency:         profile.Currency,
		Amount:           &amount,
		ExchangeRate:     &rate,
		InvoiceGatewayID: card.PaymentGatewayID,
		PaymentGatewayID: card.PaymentGatewayID,
		BillingProfileID: profile.ID,
		Status:           pricing.CollectTransactionStatusPending,
		CreditCardID:     card.ID,
		GrossAmount:      &gross,
		Metadata:         map[string]any{},
	}
	if txn, err = s.billing.SaveCollectTransaction(ctx, txn); err != nil {
		return false, err
	}
	txn, err = s.runCollect(ctx, txn, card.TokenID)
	if err != nil {
		return false, err
	}
	return txn.Status == pricing.CollectTransactionStatusSuccess, nil
}

// runCollect charges the card for an already-persisted PENDING CollectTransaction (Stripe
// confirm=true) then processes the result inline. Shared by deposit-by-card + the bill cron.
func (s *CollectService) runCollect(ctx context.Context, txn *pricing.CollectTransaction, cardTokenID string) (*pricing.CollectTransaction, error) {
	gw, err := s.billing.GetGateway(ctx, txn.PaymentGatewayID)
	if err != nil {
		return nil, err
	}
	if gw == nil {
		return nil, httpx.NotFound("Payment gateway not found")
	}
	if gw.ThirdParty != "Stripe" {
		return nil, httpx.BadRequest("Unsupported payment gateway: " + gw.ThirdParty)
	}
	g := s.gatewayFor(gw.SecretString("privateKey"))
	pi, err := g.CollectPaymentIntent(ctx, CollectInput{
		CardTokenID: cardTokenID,
		AmountCents: centsHalfDown(txn.GrossAmount),
		Currency:    txn.Currency,
	})
	if err != nil {
		return nil, err
	}
	// collectBillTransaction: FAILED/REFUSED → FAILED+errorMessage; else set externalId, stay PENDING.
	txn.ExternalID = pi.ID
	if mapStatus(pi.Status) == "FAILED" {
		txn.Status = pricing.CollectTransactionStatusFailed
		txn.ErrorMessage = gatewayErrorMessage(pi.ErrorMessage, pi.ErrorCode)
	}
	if txn, err = s.billing.SaveCollectTransaction(ctx, txn); err != nil {
		return nil, err
	}
	if txn.Status == pricing.CollectTransactionStatusPending {
		return s.processCollect(ctx, txn)
	}
	return txn, nil
}

// processCollect retrieves the PaymentIntent, and on
// SUCCESS settle it — billId → applyPaidCollectOnBill (→ bill PAID); else (deposit) → create a
// spendable AccountCredit. FAILED marks it failed. (order branch + invoice/affiliate/suspension/
// email deferred.)
func (s *CollectService) processCollect(ctx context.Context, txn *pricing.CollectTransaction) (*pricing.CollectTransaction, error) {
	gw, err := s.billing.GetGateway(ctx, txn.PaymentGatewayID)
	if err != nil {
		return nil, err
	}
	if gw == nil {
		return nil, httpx.NotFound("Payment gateway not found")
	}
	g := s.gatewayFor(gw.SecretString("privateKey"))
	pi, err := g.RetrievePaymentIntent(ctx, txn.ExternalID)
	if err != nil {
		return nil, err
	}
	switch mapStatus(pi.Status) {
	case "SUCCESS":
		txn.Status = pricing.CollectTransactionStatusSuccess
		switch {
		case txn.BillID != "":
			if err := s.applyPaidCollectOnBill(ctx, txn); err != nil {
				return nil, err
			}
		case txn.OrderID != "":
			// a collect that paid an order flips it PAID — bound to the paying profile + gross amount.
			if s.orderStatus != nil {
				if err := s.orderStatus(ctx, txn.OrderID, txn.BillingProfileID, grossOrZero(txn.GrossAmount), "PAID"); err != nil {
					return nil, err
				}
			}
		default:
			ac, err := newAccountCredit(ctx, s.billing, txn.BillingProfileID, txn.Currency, *txn.Amount)
			if err != nil {
				return nil, err
			}
			if err := s.billing.CreateAccountCredit(ctx, ac); err != nil {
				return nil, err
			}
			if s.notifier != nil {
				if profile, _ := s.billing.FindByID(ctx, txn.BillingProfileID); profile != nil {
					_ = s.notifier.SendTemplate(ctx, "send_thank_you_to_customer", []string{profile.Email}, map[string]any{
						"fullName": fullName(profile), "grossAmount": decStr(txn.GrossAmount), "currency": txn.Currency,
					})
				}
			}
		}
		saved, err := s.billing.SaveCollectTransaction(ctx, txn)
		if err != nil {
			return nil, err
		}
		// re-review the billing profile after every successful collect (a settled
		// bill may auto-resume a suspended profile). Best-effort. (invoice/affiliate = ACCEPT.)
		if s.reviewer != nil {
			if profile, _ := s.billing.FindByID(ctx, txn.BillingProfileID); profile != nil {
				_ = s.reviewer.ReviewBillingProfile(ctx, profile)
			}
		}
		return saved, nil
	case "FAILED":
		// FAILED/REFUSED: status FAILED + errorMessage = message + " (code)".
		txn.Status = pricing.CollectTransactionStatusFailed
		txn.ErrorMessage = gatewayErrorMessage(pi.ErrorMessage, pi.ErrorCode)
	default: // still pending (a CANCELLED PI is not terminal here — the collect switch has no CANCELLED case)
		return txn, nil
	}
	return s.billing.SaveCollectTransaction(ctx, txn)
}

// notifyNoCard sends the no-card warning email: fullName + current balance + currency
// (the template's fields; suspendAt values are also added when suspension is enabled — the seeded
// template body reads only fullName/balance/currency, so those are included when available).
func (s *CollectService) notifyNoCard(ctx context.Context, profile *billing.BillingProfile) {
	if s.notifier == nil || profile.Email == "" {
		return
	}
	balance, err := billing.NewBalanceService(s.billing).CurrentBalance(ctx, profile.ID, time.Now().UTC())
	if err != nil {
		return
	}
	vars := map[string]any{
		"fullName": fullName(profile), "balance": balance.StringFixed(2), "currency": profile.Currency,
	}
	if cfg, _ := s.billing.SuspensionConfiguration(ctx); cfg != nil && cfg.Enabled && cfg.SuspendedAt != nil {
		if cfg.SuspendedAt.Balance != nil {
			vars["suspendAtBalance"] = cfg.SuspendedAt.Balance.StringFixed(2)
		}
		vars["suspendAtDueDays"] = cfg.SuspendedAt.Days
	}
	_ = s.notifier.SendTemplate(ctx, "notify_customer_has_no_card", []string{profile.Email}, vars)
}

// applyPaidCollectOnBill appends the AppliedCollectedCredit
// + flip the bill PAID when nothing is left unpaid (the golden pricing.ApplyPaidCollectOnBill).
func (s *CollectService) applyPaidCollectOnBill(ctx context.Context, txn *pricing.CollectTransaction) error {
	bill, err := s.billing.BillByID(ctx, txn.BillID)
	if err != nil {
		return err
	}
	if bill == nil {
		return nil // bill vanished — nothing to settle
	}
	baseCcy, _ := s.billing.BaseCurrency(ctx)
	pricing.ApplyPaidCollectOnBill(bill, txn, baseCcy)
	return s.billing.SaveBillDoc(ctx, bill)
}

// fxTax converts a requested amount to the profile currency (rate) + computes the tax-gross.
func (s *CollectService) fxTax(ctx context.Context, profile *billing.BillingProfile, requested decimal.Decimal, now time.Time) (amount, gross, rate decimal.Decimal, err error) {
	baseCcy, _ := s.billing.BaseCurrency(ctx)
	x := pricing.NewExchanger(nil)
	rate, err = x.GetExchangeRate(baseCcy, profile.Currency, now)
	if err != nil {
		return
	}
	amount = rate.Mul(requested)
	var rates []pricing.TaxRate
	if s.pricing != nil {
		all, _ := s.pricing.AllTaxRates(ctx)
		rates = pricing.SelectTaxRates(all, profile.Country, profile.Company, now)
	}
	gross = pricing.CalculateGrossAmount(amount, rates)
	return
}

// centsHalfDown converts a money amount to integer cents using ROUND_HALF_DOWN
// (grossAmount × 100, scale 0). For a non-negative
// amount HALF_DOWN(v) = ceil(v − 0.5) (ties round toward zero); collect/deposit amounts are ≥ 0.
func centsHalfDown(amount *decimal.Decimal) int64 {
	if amount == nil {
		return 0
	}
	cents := amount.Mul(decimal.NewFromInt(100))
	if cents.IsNegative() {
		return cents.IntPart()
	}
	return cents.Sub(decimal.NewFromFloat(0.5)).Ceil().IntPart()
}

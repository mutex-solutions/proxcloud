package payment

import (
	"context"
	"errors"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// collect.go charges a saved card. The gateway confirm is SYNCHRONOUS (Stripe confirm=true /
// Airwallex confirm with a payment consent) → no redirect callback; the flow creates a PENDING
// CollectTransaction, charges, then processes the result inline. Entry points:
//   - CollectByCard       — client deposit-by-card → AccountCredit.
//   - CollectBill         — the customer retries an unpaid bill from the bill page.
//   - CollectBillingProfile/CollectAll — the monthlyCollect cron AND the pay-as-you-go hook the
//     bill send runs for POSTPAID profiles: collect each SENT bill via the profile's current card
//     → applyPaidCollectOnBill (→ bill PAID + bill_is_paid mail).
//
// A declined / errored bill charge ends the transaction FAILED with the gateway message and mails
// notify_customer_payment_failed to the billing recipients (the profile e-mail = the organization
// creator, plus the invoice contacts) with the bill link so they can fix the card and retry.

// CollectRequest is the collect request {cardId, orderId, amount}.
type CollectRequest struct {
	CardID  string           `json:"cardId"`
	OrderID string           `json:"orderId"`
	Amount  *decimal.Decimal `json:"amount"`
}

// CollectBillRequest is the pay-a-bill-with-a-saved-card request; a blank cardId uses the
// profile's current (default) card.
type CollectBillRequest struct {
	CardID string `json:"cardId"`
}

// CollectService orchestrates card collection. The gateway is built per-integration via gatewayFor
// (payment.NewGateway in prod; a fake in tests) so the flows are testable.
type CollectService struct {
	billing     *billing.Repo
	pricing     *pricing.Repo
	gatewayFor  GatewayFactory
	notifier    billing.Notifier
	reviewer    billing.ProfileReviewer
	billLink    billing.BillLinkFunc
	orderStatus func(ctx context.Context, orderID, billingProfileID string, gross decimal.Decimal, status string) error
}

func NewCollectService(b *billing.Repo, p *pricing.Repo, gatewayFor GatewayFactory) *CollectService {
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

// SetNotifier wires the email hook (thank-you, bill paid, payment failed, no card). Nil → no-op.
func (s *CollectService) SetNotifier(n billing.Notifier) { s.notifier = n }

// SetBillLink wires the console deep-link resolver for {{billUrl}} in the bill mails. Nil → "".
func (s *CollectService) SetBillLink(fn billing.BillLinkFunc) { s.billLink = fn }

// CollectByCard handles client deposit-by-card: load the card + its gateway, create a
// PENDING CollectTransaction (FX→tax to a gross amount), charge the card, then process the result
// inline. No billId → SUCCESS creates a spendable AccountCredit. Returns the terminal transaction.
func (s *CollectService) CollectByCard(ctx context.Context, profile *billing.BillingProfile, req CollectRequest) (*pricing.CollectTransaction, error) {
	if req.CardID == "" {
		return nil, httpx.BadRequest("Card id is required.")
	}
	if req.Amount == nil || !req.Amount.IsPositive() {
		return nil, httpx.BadRequest("Amount is required and must be positive.")
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

// CollectBill charges one SENT bill of the profile on a saved card (the given card, else the
// profile's current one) — the customer's retry after a failed automatic charge. Returns the
// terminal transaction (FAILED carries the gateway message).
func (s *CollectService) CollectBill(ctx context.Context, profile *billing.BillingProfile, billID string, req CollectBillRequest) (*pricing.CollectTransaction, error) {
	bill, err := s.billing.BillByID(ctx, billID)
	if err != nil {
		return nil, err
	}
	if bill == nil || bill.BillingProfileID != profile.ID {
		return nil, httpx.NotFound("Bill not found")
	}
	if bill.Status != pricing.BillStatusSent {
		return nil, httpx.BadRequest("Only an issued (SENT) bill can be paid.")
	}
	if !pricing.GetUnpaidAmountBillProductCurrency(bill).IsPositive() {
		return nil, httpx.BadRequest("The bill has no unpaid amount.")
	}
	now := time.Now().UTC()
	var card *billing.CreditCard
	if req.CardID != "" {
		if card, err = s.billing.CreditCardByIDAndBillingProfile(ctx, req.CardID, profile.ID); err != nil {
			return nil, err
		}
	} else if card, err = s.billing.CurrentCreditCard(ctx, profile.ID, profile.DefaultCardID, now); err != nil {
		return nil, err
	}
	if card == nil {
		return nil, httpx.BadRequest("No saved card to charge — add a card first.")
	}
	// The customer is watching this attempt: a decline is shown inline, no failure mail.
	ctx = context.WithValue(ctx, ctxKeyManual{}, true)
	// An earlier charge still in flight: ask the gateway about it right now instead of charging
	// again — a settled one answers the customer, a still-pending one is a 409.
	if pending, err := s.billing.PendingCollectForBill(ctx, billID); err != nil {
		return nil, err
	} else if len(pending) > 0 {
		if pending[0].ExternalID != "" {
			done, err := s.processCollect(ctx, &pending[0])
			if err == nil && done.Status == pricing.CollectTransactionStatusCancelled {
				// It took no money and leaves nothing to answer with, and the customer came to pay.
				return s.collectBill(ctx, profile, bill, card, now)
			}
			if err == nil && done.Status != pricing.CollectTransactionStatusPending {
				return done, nil
			}
		}
		return nil, chargeBusy(pending, now)
	}
	return s.collectBill(ctx, profile, bill, card, now)
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
// profile's current card. SUSPENDED → skip. No card → the no-card warning mail.
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
		txn, err := s.collectBill(ctx, profile, &bills[i], card, now)
		if err != nil {
			continue // best-effort: a per-bill gateway error must not abort the rest
		}
		if txn.Status == pricing.CollectTransactionStatusSuccess {
			paid++
		}
	}
	return paid, nil
}

// ErrCollectInFlight is returned when a charge for the bill is still being processed (a PENDING
// collect transaction) — starting another would charge twice.
var ErrCollectInFlight = httpx.NewError(409, 409, "A payment for this bill is still being processed — please wait a few minutes.")

// ErrCollectStuck is the same refusal past the scanner's window: nothing re-checks the charge by itself any more, so
// waiting will not help, and until an operator resolves it a new charge could be a second one.
var ErrCollectStuck = httpx.NewError(409, 409, "An earlier payment for this bill could not be confirmed with the payment gateway, so the bill cannot be charged again yet. Please contact support.")

// chargeBusy is the refusal for a bill with these PENDING charges.
func chargeBusy(pending []pricing.CollectTransaction, now time.Time) error {
	for i := range pending {
		if c := pending[i].CreatedAt; c != nil && now.Sub(*c) > scanMaxAge {
			return ErrCollectStuck
		}
	}
	return ErrCollectInFlight
}

// collectBill collects one SENT bill: CollectTransaction(billId, unpaidAmount) → charge → process.
// Refused (chargeBusy) while an earlier charge for the bill is still PENDING, however old it is.
func (s *CollectService) collectBill(ctx context.Context, profile *billing.BillingProfile, bill *pricing.Bill, card *billing.CreditCard, now time.Time) (*pricing.CollectTransaction, error) {
	if pending, err := s.billing.PendingCollectForBill(ctx, bill.ID); err != nil {
		return nil, err
	} else if len(pending) > 0 {
		return nil, chargeBusy(pending, now)
	}
	baseCcy, _ := s.billing.BaseCurrency(ctx)
	x := pricing.NewExchanger(nil)
	rate, err := x.GetExchangeRate(baseCcy, profile.Currency, now)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	// Insert-then-check duplicate guard (no lock needed): if another PENDING charge for the bill
	// was created before ours, ours yields — two concurrent requests can at worst both yield,
	// never both charge. Ids are time-ordered, so "older" is a total order.
	if pending, err := s.billing.PendingCollectForBill(ctx, bill.ID); err != nil {
		return nil, err
	} else {
		for i := range pending {
			if pending[i].ID != txn.ID && pending[i].ID < txn.ID {
				txn.Status = pricing.CollectTransactionStatusCancelled
				txn.ErrorMessage = "another payment for this bill is in progress"
				_, _ = s.billing.SaveCollectTransaction(ctx, txn)
				return nil, ErrCollectInFlight
			}
		}
	}
	return s.runCollect(ctx, txn, card.TokenID)
}

// runCollect charges the card for an already-persisted PENDING CollectTransaction (synchronous
// gateway confirm) then processes the result inline. Shared by deposit-by-card + the bill paths.
// A gateway error or decline ends the transaction FAILED (never a PENDING orphan with no
// externalId, which the scanner would skip forever).
func (s *CollectService) runCollect(ctx context.Context, txn *pricing.CollectTransaction, cardTokenID string) (*pricing.CollectTransaction, error) {
	gw, err := s.billing.GetGateway(ctx, txn.PaymentGatewayID)
	if err != nil {
		return nil, err
	}
	if gw == nil {
		// The card's gateway integration is gone: nothing can be charged — close the transaction
		// (no customer mail: this is an operator problem, not the card's).
		return s.failCollect(ctx, txn, "payment gateway "+txn.PaymentGatewayID+" is not configured", false)
	}
	g, err := cardGateway(s.gatewayFor, gw)
	if err != nil {
		return s.failCollect(ctx, txn, err.Error(), false)
	}
	pi, err := g.CollectPaymentIntent(ctx, CollectInput{
		CardTokenID: cardTokenID,
		AmountCents: centsHalfDown(txn.GrossAmount),
		Currency:    txn.Currency,
		Reference:   txn.ID,
	})
	if err != nil {
		if pi.ID == "" {
			// Nothing was charged (the consent lookup / intent creation failed): terminal FAILED.
			// A transport / configuration error, not a decline — the customer gets a generic reason.
			return s.failCollect(ctx, txn, err.Error(), true)
		}
		// An intent exists but its outcome is unknown (e.g. a timeout after the confirm was sent):
		// keep it PENDING with the externalId so the scanner reconciles it against the gateway.
		// Marking it FAILED here could double-charge a payment that actually went through.
		txn.ExternalID = pi.ID
		txn.ErrorMessage = err.Error()
		return s.billing.SaveCollectTransaction(ctx, txn)
	}
	// FAILED/REFUSED → FAILED + errorMessage; else set externalId, stay PENDING and settle.
	txn.ExternalID = pi.ID
	if mapStatus(pi.Status) == "FAILED" {
		markDecline(txn)
		return s.failCollect(ctx, txn, gatewayErrorMessage(pi.ErrorMessage, pi.ErrorCode), true)
	}
	if txn, err = s.billing.SaveCollectTransaction(ctx, txn); err != nil {
		return nil, err
	}
	if txn.Status == pricing.CollectTransactionStatusPending {
		return s.processCollect(ctx, txn)
	}
	return txn, nil
}

// failCollect marks the transaction FAILED with the gateway message, persists it, and — for a
// bill charge, when notify — mails notify_customer_payment_failed so the customer can fix the card
// and retry.
func (s *CollectService) failCollect(ctx context.Context, txn *pricing.CollectTransaction, msg string, notify bool) (*pricing.CollectTransaction, error) {
	txn.Status = pricing.CollectTransactionStatusFailed
	txn.ErrorMessage = msg
	saved, err := s.billing.SaveCollectTransaction(ctx, txn)
	if err != nil {
		return nil, err
	}
	if manual, _ := ctx.Value(ctxKeyManual{}).(bool); notify && txn.BillID != "" && !manual {
		s.notifyPaymentFailed(ctx, saved)
	}
	return saved, nil
}

// ctxKeyManual marks a collect started by the customer from the console (CollectBill): its
// outcome is shown on the page, so a decline does not also send the failure e-mail.
type ctxKeyManual struct{}

// retrieveCharge asks the charge's gateway about its intent.
func (s *CollectService) retrieveCharge(ctx context.Context, txn *pricing.CollectTransaction) (PaymentIntentResult, error) {
	gw, err := s.billing.GetGateway(ctx, txn.PaymentGatewayID)
	if err != nil {
		return PaymentIntentResult{}, err
	}
	if gw == nil {
		return PaymentIntentResult{}, httpx.NotFound("Payment gateway not found")
	}
	g, err := cardGateway(s.gatewayFor, gw)
	if err != nil {
		return PaymentIntentResult{}, err
	}
	return g.RetrievePaymentIntent(ctx, txn.ExternalID)
}

// chargeDidNotSucceed reports a gateway answer that says no money moved and none will without a new confirm, which only
// this portal could send: declined or never confirmed. Everything else that is not terminal (processing, requires_action,
// requires_capture, Airwallex's pending) may still capture.
func chargeDidNotSucceed(status string) bool {
	return status == "requires_payment_method" || status == "requires_confirmation"
}

// chargeNote says what the gateway answered about a charge that stays PENDING.
func chargeNote(pi PaymentIntentResult) string {
	if chargeDidNotSucceed(pi.Status) {
		note := "The gateway reports " + pi.Status + ": the charge did not succeed"
		if m := gatewayErrorMessage(pi.ErrorMessage, pi.ErrorCode); m != "" {
			note += " (" + m + ")"
		}
		return note + "."
	}
	return "The gateway reports " + pi.Status + ": the charge may still complete."
}

// processCollect retrieves the PaymentIntent, and on
// SUCCESS settle it — billId → applyPaidCollectOnBill (→ bill PAID + bill_is_paid mail); else
// (deposit) → create a spendable AccountCredit. FAILED marks it failed (+ the failure mail for a
// bill charge). CANCELLED ends it: no money moved and none can, and the bill can be charged again. Anything
// else stays PENDING, with the answer in txn.ErrorMessage for the caller to keep or not.
func (s *CollectService) processCollect(ctx context.Context, txn *pricing.CollectTransaction) (*pricing.CollectTransaction, error) {
	pi, err := s.retrieveCharge(ctx, txn)
	if err != nil {
		return nil, err
	}
	switch mapStatus(pi.Status) {
	case "SUCCESS":
		txn.Status = pricing.CollectTransactionStatusSuccess
		txn.ErrorMessage = "" // an unknown-outcome error recorded earlier is resolved now
		var paidBill *pricing.Bill
		switch {
		case txn.BillID != "":
			var closed bool
			if paidBill, closed, err = s.applyPaidCollectOnBill(ctx, txn); err != nil {
				return nil, err
			}
			if closed {
				return s.creditSurplus(ctx, txn)
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
			if err := s.billing.CreateAccountCreditForTxn(ctx, txn.ID, ac); err != nil {
				return nil, err
			}
			if s.notifier != nil {
				if profile, _ := s.billing.FindByID(ctx, txn.BillingProfileID); profile != nil {
					_ = s.notifier.SendTemplate(ctx, "send_thank_you_to_customer", billing.Recipients(profile), map[string]any{
						"fullName": fullName(profile), "grossAmount": decStr(txn.GrossAmount), "currency": txn.Currency,
					})
				}
			}
		}
		saved, err := s.billing.SaveCollectTransaction(ctx, txn)
		if err != nil {
			return nil, err
		}
		// Post-success side-effects, best-effort: the bill_is_paid mail (with the invoice PDF) when
		// the charge settled the bill, and the suspension re-review (a settled bill may auto-resume a
		// suspended profile).
		if profile, _ := s.billing.FindByID(ctx, txn.BillingProfileID); profile != nil {
			if paidBill != nil && paidBill.Status == pricing.BillStatusPaid {
				billing.NotifyBillPaid(ctx, s.notifier, s.billing, s.pricing, s.billLink, profile, paidBill)
			}
			if s.reviewer != nil {
				_ = s.reviewer.ReviewBillingProfile(ctx, profile)
			}
		}
		return saved, nil
	case "FAILED":
		markDecline(txn)
		return s.failCollect(ctx, txn, gatewayErrorMessage(pi.ErrorMessage, pi.ErrorCode), true)
	case "CANCELLED":
		txn.Status = pricing.CollectTransactionStatusCancelled
		txn.ErrorMessage = "The gateway reports the charge was cancelled"
		if pi.CancellationReason != "" {
			txn.ErrorMessage += " (" + pi.CancellationReason + ")"
		}
		return s.billing.SaveCollectTransaction(ctx, txn)
	default:
		txn.ErrorMessage = chargeNote(pi)
		return txn, nil
	}
}

// settleSurplus is the scanner's leg for a stuck bill charge whose bill is already PAID: when the
// gateway reports the charge succeeded, the money becomes a spendable account credit. Reports whether it
// settled that way; a charge that did not succeed changes nothing, and an error means the gateway could
// not say, or says the charge may still complete.
func (s *CollectService) settleSurplus(ctx context.Context, txn *pricing.CollectTransaction) (bool, error) {
	pi, err := s.retrieveCharge(ctx, txn)
	if err != nil {
		return false, err
	}
	switch st := mapStatus(pi.Status); {
	case st == "SUCCESS":
		_, err = s.creditSurplus(ctx, txn)
		return err == nil, err
	case st != "PENDING", chargeDidNotSucceed(pi.Status):
		return false, nil
	}
	return false, errors.New(chargeNote(pi))
}

// creditSurplus closes a succeeded charge whose bill was settled by something else (credits, a bank
// transfer, another charge): the money the customer paid twice must not vanish from the books, so it
// becomes a spendable account credit (SUCCESS txn, no bill settlement) the operator can also refund.
func (s *CollectService) creditSurplus(ctx context.Context, txn *pricing.CollectTransaction) (*pricing.CollectTransaction, error) {
	ac, err := newAccountCredit(ctx, s.billing, txn.BillingProfileID, txn.Currency, *txn.Amount)
	if err != nil {
		return nil, err
	}
	if err := s.billing.CreateAccountCreditForTxn(ctx, txn.ID, ac); err != nil {
		return nil, err
	}
	txn.Status = pricing.CollectTransactionStatusSuccess
	if txn.Metadata == nil {
		txn.Metadata = map[string]any{}
	}
	txn.Metadata["surplusAccountCreditId"] = ac.ID
	txn.ErrorMessage = "bill was already paid; the charge was credited to the account balance"
	return s.billing.SaveCollectTransaction(ctx, txn)
}

// notifyPaymentFailed mails notify_customer_payment_failed for a declined bill charge: who, which
// invoice, how much, the card, the gateway's reason and the console link to retry. Best-effort.
func (s *CollectService) notifyPaymentFailed(ctx context.Context, txn *pricing.CollectTransaction) {
	if s.notifier == nil {
		return
	}
	profile, _ := s.billing.FindByID(ctx, txn.BillingProfileID)
	bill, _ := s.billing.BillByID(ctx, txn.BillID)
	if profile == nil || bill == nil {
		return
	}
	to := billing.Recipients(profile)
	if len(to) == 0 {
		return
	}
	vars := billing.BillMailVars(ctx, s.billing, s.billLink, profile, bill, nil)
	vars["amountDue"] = decStr(txn.GrossAmount) // the charged gross in the invoice currency
	// Only a gateway decline is quoted to the customer; a transport / configuration error stays
	// in the transaction for the operator and the customer gets a neutral reason.
	vars["errorMessage"] = "The payment provider could not process the charge. Please try again or use another card."
	if isDecline(txn) {
		vars["errorMessage"] = txn.ErrorMessage
	}
	vars["panMasked"] = ""
	if card, _ := s.billing.CreditCardByID(ctx, txn.CreditCardID); card != nil {
		vars["panMasked"] = card.PanMasked
	}
	_ = s.notifier.SendTemplate(ctx, "notify_customer_payment_failed", to, vars)
}

// markDecline tags a transaction whose failure is the card being refused by the gateway (as
// opposed to an error on our side) — the only failure reason quoted to the customer.
func markDecline(txn *pricing.CollectTransaction) {
	if txn.Metadata == nil {
		txn.Metadata = map[string]any{}
	}
	txn.Metadata["failure"] = "decline"
}

func isDecline(txn *pricing.CollectTransaction) bool {
	v, _ := txn.Metadata["failure"].(string)
	return v == "decline"
}

// notifyNoCard sends the no-card warning email: fullName + current balance + currency
// (the template's fields; suspendAt values are also added when suspension is enabled — the seeded
// template body reads only fullName/balance/currency, so those are included when available).
func (s *CollectService) notifyNoCard(ctx context.Context, profile *billing.BillingProfile) {
	to := billing.Recipients(profile)
	if s.notifier == nil || len(to) == 0 {
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
	_ = s.notifier.SendTemplate(ctx, "notify_customer_has_no_card", to, vars)
}

// applyPaidCollectOnBill records a succeeded charge on its bill under the profile's credit lock, flipping
// it PAID when nothing is left unpaid (the golden pricing.ApplyPaidCollectOnBill). It returns the bill when
// this call changed it. A charge the bill already holds is a race with another pass over the same
// transaction and changes nothing. closed reports a bill paid some other way meanwhile, which takes no more
// money.
func (s *CollectService) applyPaidCollectOnBill(ctx context.Context, txn *pricing.CollectTransaction) (changed *pricing.Bill, closed bool, err error) {
	baseCcy, _ := s.billing.BaseCurrency(ctx) // tolerated failure: read outside the lock's transaction
	err = s.billing.WithProfileLock(ctx, txn.BillingProfileID, func(ctx context.Context) error {
		bill, err := s.billing.BillByID(ctx, txn.BillID)
		if err != nil || bill == nil { // a vanished bill has nothing to settle
			return err
		}
		if billHolds(bill, txn.ID) {
			return nil
		}
		if bill.Status == pricing.BillStatusPaid {
			closed = true
			return nil
		}
		pricing.ApplyPaidCollectOnBill(bill, txn, baseCcy)
		if err := s.billing.SaveBillDoc(ctx, bill); err != nil {
			return err
		}
		changed = bill
		return nil
	})
	return changed, closed, err
}

// billHolds reports whether the charge is already recorded on the bill.
func billHolds(bill *pricing.Bill, txnID string) bool {
	for i := range bill.CollectedAmounts {
		if bill.CollectedAmounts[i].CollectTransactionID == txnID {
			return true
		}
	}
	return false
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

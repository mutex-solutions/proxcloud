package payment

import (
	"context"
	"encoding/json"
	"fmt"
	mrand "math/rand/v2"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// addfunds.go implements the deposit flow: validate KYC + amount/gateway + minDeposit,
// FX→tax to a gross amount, persist a PENDING AccountCreditTransaction, then create the gateway
// PaymentIntent and return its client secret for the frontend to confirm (the redirect callback
// finalizes it later). Stripe and Airwallex are the card gateways; BankTransfer is manual.

// AddFundsRequest is the add-funds request payload.
type AddFundsRequest struct {
	BillID            string           `json:"billId"`
	OrderID           string           `json:"orderId"`
	Amount            *decimal.Decimal `json:"amount"`
	PaymentGatewayID  string           `json:"paymentGatewayId"`
	SavingsContractID string           `json:"savingsContractId"`
}

// AddFundsResponse is the add-funds response (money as JSON numbers).
type AddFundsResponse struct {
	TransactionID       string      `json:"transactionId,omitempty"`
	ExternalPaymentID   string      `json:"externalPaymentId,omitempty"`
	ExternalInvoiceID   string      `json:"externalInvoiceId,omitempty"`
	ThirdParty          string      `json:"thirdParty,omitempty"`
	PaymentFormURL      string      `json:"paymentFormUrl,omitempty"`
	Metadata            any         `json:"metadata,omitempty"`
	NetAmount           json.Number `json:"netAmount"`
	GrossAmount         json.Number `json:"grossAmount"`
	InvoiceCurrency     string      `json:"invoiceCurrency,omitempty"`
	IsNeededToBeHandled bool        `json:"isNeededToBeHandled"`
}

// AddFundsService orchestrates a deposit. The gateway is built per-integration via gatewayFor
// (payment.NewGateway in prod; a fake in tests) so the flow is testable without calling a gateway.
type AddFundsService struct {
	billing     *billing.Repo
	pricing     *pricing.Repo
	gatewayFor  GatewayFactory
	notifier    billing.Notifier
	reviewer    billing.ProfileReviewer
	orderStatus func(ctx context.Context, orderID, billingProfileID string, gross decimal.Decimal, status string) error
	billPayer   func(ctx context.Context, profile *billing.BillingProfile, billID string) error
	adminURL    string // admin console base URL ({{adminUrl}} in operator notifications)
}

func NewAddFundsService(b *billing.Repo, p *pricing.Repo, gatewayFor GatewayFactory) *AddFundsService {
	return &AddFundsService{billing: b, pricing: p, gatewayFor: gatewayFor}
}

// SetNotifier wires the email hook (deposit thank-you + refunded-invoice). Nil → no-op.
func (s *AddFundsService) SetNotifier(n billing.Notifier) { s.notifier = n }

// SetReviewer wires the suspension auto-resume hook (re-review the billing profile
// after a successful deposit). Nil → no-op.
func (s *AddFundsService) SetReviewer(r billing.ProfileReviewer) { s.reviewer = r }

// SetOrderStatusUpdater wires the order flip (mark the order PAID when the deposit
// paid an order). The updater binds the flip to the paying profile + gross amount. Nil → no-op.
func (s *AddFundsService) SetOrderStatusUpdater(f func(ctx context.Context, orderID, billingProfileID string, gross decimal.Decimal, status string) error) {
	s.orderStatus = f
}

// grossOrZero derefs a gross-amount pointer (nil → zero) for the order-binding seam.
func grossOrZero(d *decimal.Decimal) decimal.Decimal {
	if d == nil {
		return decimal.Zero
	}
	return *d
}

// SetBillPayer wires the pay-bill leg (when the deposit targeted a specific bill —
// the freshly-minted credit settles it). Nil → no-op.
func (s *AddFundsService) SetBillPayer(f func(ctx context.Context, profile *billing.BillingProfile, billID string) error) {
	s.billPayer = f
}

// fullName returns the profile's trimmed first + last name.
func fullName(p *billing.BillingProfile) string {
	return strings.TrimSpace(p.FirstName + " " + p.LastName)
}

// decStr renders a money pointer at 2dp for email display ("" when nil).
func decStr(d *decimal.Decimal) string {
	if d == nil {
		return ""
	}
	return d.StringFixed(2)
}

// notify is a nil-safe best-effort templated send to the profile's billing recipients (email
// failure never breaks the payment flow).
func (s *AddFundsService) notify(ctx context.Context, key string, profile *billing.BillingProfile, vars map[string]any) {
	to := billing.Recipients(profile)
	if s.notifier == nil || len(to) == 0 {
		return
	}
	_ = s.notifier.SendTemplate(ctx, key, to, vars)
}

func (s *AddFundsService) AddFunds(ctx context.Context, profile *billing.BillingProfile, req AddFundsRequest) (*AddFundsResponse, error) {
	// KYC: if the profile has verifications, ALL must be verified.
	if !allVerified(profile.Verifications) {
		return nil, httpx.BadRequest("Your account is not verified.")
	}
	if req.PaymentGatewayID == "" {
		return nil, httpx.BadRequest("Payment gateway id is required.")
	}
	if req.Amount == nil && req.BillID == "" {
		return nil, httpx.BadRequest("Amount is required.")
	}

	gw, err := s.billing.GetGateway(ctx, req.PaymentGatewayID)
	if err != nil {
		return nil, err
	}
	if gw == nil {
		return nil, httpx.NotFound("Payment gateway not found")
	}
	// minDeposit guard (skipped when paying a specific bill).
	if req.BillID == "" {
		if min := gw.MinDeposit(); min > 0 && req.Amount.LessThan(decimal.NewFromFloat(min)) {
			return nil, httpx.BadRequest("The amount is less than the minimum deposit.")
		}
	}

	now := time.Now().UTC()
	baseCcy, _ := s.billing.BaseCurrency(ctx)
	x := pricing.NewExchanger(nil) // live FX deferred; same-currency → rate 1
	rate, err := x.GetExchangeRate(baseCcy, profile.Currency, now)
	if err != nil {
		return nil, err
	}
	var amount decimal.Decimal
	if req.BillID != "" {
		// Paying a specific bill: the deposit NET is the bill's unpaid net (product currency), so the
		// gross computed below is exactly the invoice's unpaid gross. The client-supplied amount is
		// ignored — it is tax-inclusive and would be taxed a second time.
		bill, err := s.billing.BillByID(ctx, req.BillID)
		if err != nil {
			return nil, err
		}
		if bill == nil || bill.BillingProfileID != profile.ID {
			return nil, httpx.NotFound("Bill not found")
		}
		if bill.Status != pricing.BillStatusSent {
			return nil, httpx.BadRequest("Only an issued (SENT) bill can be paid.")
		}
		amount = pricing.GetUnpaidAmountBillProductCurrency(bill).Round(2)
		if !amount.IsPositive() {
			return nil, httpx.BadRequest("The bill has no unpaid amount.")
		}
	} else {
		amount = rate.Mul(*req.Amount).Round(2) // scaleHalfUp(rate × requested)
	}

	var rates []pricing.TaxRate
	if s.pricing != nil {
		all, _ := s.pricing.AllTaxRates(ctx)
		rates = pricing.SelectTaxRates(all, profile.Country, profile.Company, now)
	}
	gross := pricing.CalculateGrossAmount(amount, rates)

	// Persist the PENDING transaction (invoiceGatewayId falls back to the payment gateway until
	// a default invoice gateway is configured — invoice generation lands with the callback).
	txn := &billing.AccountCreditTransaction{
		BillingProfileID: profile.ID,
		Status:           "PENDING",
		OrderID:          req.OrderID,
		PaymentGatewayID: req.PaymentGatewayID,
		InvoiceGatewayID: req.PaymentGatewayID,
		Currency:         profile.Currency,
		Amount:           &amount,
		GrossAmount:      &gross,
		BillID:           req.BillID,
		ExchangeRate:     &rate,
		Metadata:         pgdoc.M{},
	}
	txn, err = s.billing.SaveAccountCreditTransaction(ctx, txn)
	if err != nil {
		return nil, err
	}

	if gw.ThirdParty == "BankTransfer" {
		// BankTransferOperations.addFunds: create the PENDING manual transfer keyed to the txn and
		// hand the customer a reference number — NO external gateway. The admin approve/reject
		// endpoints later settle it through ProcessBankTransfer.
		ref := mrand.IntN(999999) // random reference number in [0,999999)
		bt := pgdoc.M{
			"status":                     "PENDING",
			"paymentGatewayId":           gw.ID,
			"referenceNumber":            ref,
			"accountCreditTransactionId": txn.ID,
			"billingProfileId":           profile.ID,
			"fullName":                   fullName(profile),
			"grossAmount":                gross, // decimal.Decimal → stored as a decimal string by the codec
			"currency":                   profile.Currency,
			"createdAt":                  now,
		}
		if req.BillID != "" {
			bt["billId"] = req.BillID // the bill this transfer pays (approval settles it) — shown to the operator
			if b, _ := s.billing.BillByID(ctx, req.BillID); b != nil && b.InvoiceNumber != "" {
				bt["invoiceNumber"] = b.InvoiceNumber // shown to the operator + {{invoiceNumber}} on submit
			}
		}
		btID, err := s.billing.CreateBankTransfer(ctx, bt)
		if err != nil {
			return nil, err
		}
		txn.ExternalID = btID
		if req.SavingsContractID != "" {
			txn.Metadata["savingsContractId"] = req.SavingsContractID
		}
		if _, err := s.billing.SaveAccountCreditTransaction(ctx, txn); err != nil {
			return nil, err
		}
		// Hand the customer the transfer instructions (operator-entered on the integration) + the
		// gross amount and reference number by email. Best-effort, like the thank-you mail.
		vars := map[string]any{
			"fullName": fullName(profile), "instructions": gw.ConfigString("bankTransferInstructions"),
			"amount": gross.StringFixed(2), "currency": profile.Currency, "referenceNumber": ref,
		}
		if req.BillID != "" {
			vars["billId"] = req.BillID
			vars["billReference"] = " (payment for bill " + req.BillID + ")" // inline suffix: the renderer has no sections
		}
		s.notify(ctx, "send_bank_transfer_instructions", profile, vars)
		return &AddFundsResponse{
			TransactionID:     txn.ID,
			ExternalPaymentID: btID,
			ThirdParty:        gw.ThirdParty,
			Metadata:          map[string]any{"referenceNumber": ref},
			NetAmount:         json.Number(amount.String()),
			GrossAmount:       json.Number(gross.String()),
			InvoiceCurrency:   profile.Currency,
		}, nil
	}
	g, err := cardGateway(s.gatewayFor, gw)
	if err != nil {
		return nil, err
	}
	custID, err := g.GetOrCreateCustomer(ctx, CustomerInput{
		BillingProfileID: profile.ID,
		Email:            profile.Email,
		Name:             strings.TrimSpace(profile.FirstName + " " + profile.LastName),
		Country:          profile.Country,
	})
	if err != nil {
		return nil, err
	}
	pi, err := g.CreatePaymentIntent(ctx, PaymentIntentInput{
		CustomerID:  custID,
		AmountCents: gross.Mul(decimal.NewFromInt(100)).IntPart(),
		Currency:    profile.Currency,
		Description: "Add funds to account",
		OffSession:  true,
	})
	if err != nil {
		return nil, err
	}

	txn.ExternalID = pi.ID
	txn.Metadata["saveCardForFutureUse"] = true
	if req.SavingsContractID != "" {
		txn.Metadata["savingsContractId"] = req.SavingsContractID
	}
	if _, err := s.billing.SaveAccountCreditTransaction(ctx, txn); err != nil {
		return nil, err
	}

	return &AddFundsResponse{
		TransactionID:     txn.ID,
		ExternalPaymentID: pi.ID,
		ThirdParty:        gw.ThirdParty,
		Metadata:          pi.ClientSecret,
		NetAmount:         json.Number(amount.String()),
		GrossAmount:       json.Number(gross.String()),
		InvoiceCurrency:   profile.Currency,
	}, nil
}

// ProcessAddFunds is the redirect-callback confirm —
// retrieve the PaymentIntent, map its status, and on SUCCESS create the spendable AccountCredit
// + mark the transaction SUCCESS (idempotent). PENDING leaves it; FAILED marks it failed.
// (order/savingsContract branches + invoice/affiliate/suspension/saveCard downstream deferred.)
func (s *AddFundsService) ProcessAddFunds(ctx context.Context, txnID string) (*billing.AccountCreditTransaction, error) {
	txn, err := s.billing.AccountCreditTransactionByID(ctx, txnID)
	if err != nil {
		return nil, err
	}
	if txn == nil {
		return nil, httpx.NotFound("Transaction not found")
	}
	gw, err := s.billing.GetGateway(ctx, txn.PaymentGatewayID)
	if err != nil {
		return nil, err
	}
	if gw == nil {
		return nil, httpx.NotFound("Payment gateway not found")
	}
	if gw.ThirdParty == "BankTransfer" {
		// paymentFactory dispatch: the manual gateway's processAddFunds reads the transfer doc
		// (BankTransferOperations) — no external call. Shared with the admin approve/reject settle.
		bt, err := s.billing.BankTransferByTxnID(ctx, txn.ID)
		if err != nil {
			return nil, err
		}
		if bt == nil {
			return nil, httpx.NotFound(fmt.Sprintf("Bank transfer with account credit transaction id %s not found", txn.ID))
		}
		status, _ := bt["status"].(string)
		comments, _ := bt["comments"].(string)
		return s.ProcessBankTransfer(ctx, txnID, status, comments)
	}
	g, err := cardGateway(s.gatewayFor, gw)
	if err != nil {
		return nil, err
	}
	pi, err := g.RetrievePaymentIntent(ctx, txn.ExternalID)
	if err != nil {
		return nil, err
	}

	switch mapStatus(pi.Status) {
	case "SUCCESS":
		return s.settleSuccess(ctx, txn)
	case "FAILED":
		// failed transaction: status FAILED + gatewayMessage = gatewayStatus.message.
		txn.Status = "FAILED"
		txn.GatewayMessage = gatewayErrorMessage(pi.ErrorMessage, pi.ErrorCode)
	case "CANCELLED":
		// cancellation: status CANCELLED + gatewayMessage (NOT FAILED).
		txn.Status = "CANCELLED"
		txn.GatewayMessage = pi.CancellationReason
	default: // PENDING (requires_*/processing) → no state change
		return txn, nil
	}
	return s.billing.SaveAccountCreditTransaction(ctx, txn)
}

// settleSuccess handles a successful transaction (+ the not-already-processed guard):
// order-PAID / savings-contract-activate / spendable-credit branch, thank-you mail, txn SUCCESS,
// then the best-effort suspension re-review + targeted-bill settle. Shared by the Stripe confirm
// (ProcessAddFunds) and the manual bank-transfer resolution (ProcessBankTransfer).
func (s *AddFundsService) settleSuccess(ctx context.Context, txn *billing.AccountCreditTransaction) (*billing.AccountCreditTransaction, error) {
	if txn.Status == "SUCCESS" { // repeated callback for an already-processed txn: idempotent
		return txn, nil
	}
	// only a PENDING txn may be processed — re-driving a
	// FAILED/CANCELLED/REFUNDED txn would mint a duplicate credit (400 "already processed").
	if txn.Status != "PENDING" {
		return nil, httpx.BadRequest(fmt.Sprintf("Transaction %s already processed", txn.ID))
	}
	// Atomic claim: concurrent callbacks (two tabs, a retry racing the scanner) all read PENDING;
	// only the one that flips the row to PROCESSING may mint the credit.
	won, err := s.billing.ClaimAccountCreditTransaction(ctx, txn.ID)
	if err != nil {
		return nil, err
	}
	if !won {
		if cur, _ := s.billing.AccountCreditTransactionByID(ctx, txn.ID); cur != nil && cur.Status == "SUCCESS" {
			return cur, nil
		}
		return nil, httpx.BadRequest(fmt.Sprintf("Transaction %s is being processed", txn.ID))
	}
	saved, err := s.settleClaimed(ctx, txn)
	if err != nil {
		_ = s.billing.ReleaseAccountCreditTransaction(ctx, txn.ID) // let the callback / scanner retry
		return nil, err
	}
	return saved, nil
}

// settleClaimed is the settle body once the PENDING→PROCESSING claim is won (see settleSuccess).
func (s *AddFundsService) settleClaimed(ctx context.Context, txn *billing.AccountCreditTransaction) (*billing.AccountCreditTransaction, error) {
	profile, err := s.billing.FindByID(ctx, txn.BillingProfileID)
	if err != nil {
		return nil, err
	}
	if profile != nil {
		// success branches: orderId → order PAID; savingsContractId
		// (stashed in metadata by AddFunds) → activate the contract; else → spendable credit.
		contractID, _ := txn.Metadata["savingsContractId"].(string)
		switch {
		case txn.OrderID != "":
			if s.orderStatus != nil {
				if err := s.orderStatus(ctx, txn.OrderID, txn.BillingProfileID, grossOrZero(txn.GrossAmount), "PAID"); err != nil {
					return nil, err
				}
			}
		case contractID != "":
			if err := s.activateSavingsContract(ctx, profile, contractID); err != nil {
				return nil, err
			}
		default:
			if err := s.createAccountCredit(ctx, profile, txn); err != nil {
				return nil, err
			}
		}
		s.notify(ctx, "send_thank_you_to_customer", profile, map[string]any{
			"fullName": fullName(profile), "grossAmount": decStr(txn.GrossAmount), "currency": txn.Currency,
		})
	}
	txn.Status = "SUCCESS"
	saved, err := s.billing.SaveAccountCreditTransaction(ctx, txn)
	if err != nil {
		return nil, err
	}
	// Post-success side-effects: re-review the suspension (a top-up may auto-resume) +
	// settle the targeted bill with the fresh credit. Best-effort — the deposit already stands.
	if profile != nil {
		if s.reviewer != nil {
			_ = s.reviewer.ReviewBillingProfile(ctx, profile)
		}
		if txn.BillID != "" && s.billPayer != nil {
			_ = s.billPayer(ctx, profile, txn.BillID)
		}
	}
	return saved, nil
}

// ProcessBankTransfer resolves a deposit when the gateway is the manual
// BankTransfer integration: the provider status comes
// from the resolved bankTransfer doc — APPROVED→SUCCESS (settle: credit the account), REJECTED→
// REFUSED (txn FAILED, gatewayMessage = the transfer's comments), anything else→PENDING (no-op).
// No external gateway call is involved.
func (s *AddFundsService) ProcessBankTransfer(ctx context.Context, txnID, bankStatus, comments string) (*billing.AccountCreditTransaction, error) {
	txn, err := s.billing.AccountCreditTransactionByID(ctx, txnID)
	if err != nil {
		return nil, err
	}
	if txn == nil {
		return nil, httpx.NotFound("Transaction not found")
	}
	switch bankStatus {
	case "APPROVED":
		return s.settleSuccess(ctx, txn)
	case "REJECTED":
		// REFUSED → failed transaction: status FAILED + gatewayMessage.
		txn.Status = "FAILED"
		txn.GatewayMessage = comments
		return s.billing.SaveAccountCreditTransaction(ctx, txn)
	default: // PENDING → no state change
		return txn, nil
	}
}

// activateSavingsContract activates the savings contract (a deposit tied to a savings
// contract activates the contract instead of minting spendable credit): status → ACTIVE + the
// savings_contract_activated email. A missing contract errors (404).
func (s *AddFundsService) activateSavingsContract(ctx context.Context, profile *billing.BillingProfile, contractID string) error {
	c, err := s.billing.SavingsContractByID(ctx, contractID)
	if err != nil {
		return err
	}
	if c == nil {
		return httpx.NotFound("Savings contract not found")
	}
	c.Status = billing.SavingsStatusActive
	if err := s.billing.SaveSavingsContract(ctx, c); err != nil {
		return err
	}
	vars := map[string]any{
		"fullName": fullName(profile), "savingsPlanName": c.SavingsPlanName, "currency": profile.Currency,
	}
	if c.StartDate != nil {
		vars["startDate"] = c.StartDate.Format("2006-01-02")
	}
	if c.EndDate != nil {
		vars["endDate"] = c.EndDate.Format("2006-01-02")
	}
	if c.MonthlyCommittedAmount != nil {
		vars["monthlyCommittedAmount"] = c.MonthlyCommittedAmount.StringFixed(2)
	}
	if c.DiscountRate != nil {
		vars["discountRate"] = c.DiscountRate.String()
	}
	s.notify(ctx, "savings_contract_activated", profile, vars)
	return nil
}

// createAccountCredit creates the spendable AccountCredit balance from
// the (product-currency) transaction amount and link it (with its id) on the transaction — the
// id lets refundFunds find + void the credit later.
func (s *AddFundsService) createAccountCredit(ctx context.Context, profile *billing.BillingProfile, txn *billing.AccountCreditTransaction) error {
	ac, err := newAccountCredit(ctx, s.billing, txn.BillingProfileID, txn.Currency, *txn.Amount)
	if err != nil {
		return err
	}
	if err := s.billing.CreateAccountCredit(ctx, ac); err != nil {
		return err
	}
	txn.AccountCredit = pgdoc.M{"id": ac.ID, "billingProfileId": ac.BillingProfileID, "amount": ac.Amount.String(), "currency": ac.Currency}
	return nil
}

// newAccountCredit builds a spendable AccountCredit from a (PENDING→SUCCESS) deposit amount,
// converting the invoice-currency amount to the product (base) currency.
// Shared by add-funds + collect-by-card.
func newAccountCredit(ctx context.Context, b *billing.Repo, billingProfileID, currency string, amount decimal.Decimal) (*pricing.AccountCredit, error) {
	now := time.Now().UTC()
	baseCcy, _ := b.BaseCurrency(ctx)
	amt := amount
	rate := decimal.NewFromInt(1)
	if currency != baseCcy {
		x := pricing.NewExchanger(nil)
		a, err := x.ExchangeToProductCurrency(amount, currency, baseCcy, now)
		if err != nil {
			return nil, err
		}
		amt = a
		r, err := x.GetExchangeRate(baseCcy, currency, now)
		if err != nil {
			return nil, err
		}
		rate = r
	}
	// invoiceExchangeRate must be stamped on every minted credit: the bill-settle leg
	// (settleAccountCreditLeg) multiplies by it when the credit pays a bill.
	return &pricing.AccountCredit{
		BillingProfileID:    billingProfileID,
		InitialAmount:       &amt,
		Amount:              &amt,
		Currency:            baseCcy,
		InvoiceCurrency:     currency,
		InvoiceExchangeRate: &rate,
		CreatedAt:           &now,
	}, nil
}

// RefundFunds is the admin refund endpoint: full-refund a SUCCESS deposit's PaymentIntent. When the
// gateway accepts the refund (succeeded, or still processing — Airwallex settles refunds
// asynchronously) the spendable AccountCredit is voided right away (zero balance + refundedAt,
// never deleted — the money is on its way back and must not be spendable meanwhile) and the txn is
// REFUNDED with metadata.refund = {id, status, creditAmount}. A refund the gateway refuses is a
// 400 with the gateway's reason and changes nothing. The credit note mail (send_refunded_invoice,
// billing.CreditNoteNumber) goes out when the refund is confirmed: immediately for a succeeded
// one, from the scanner (ReconcileRefund) for a pending one. Non-refund-capable gateways → 400.
func (s *AddFundsService) RefundFunds(ctx context.Context, txnID string) (*billing.AccountCreditTransaction, error) {
	txn, err := s.billing.AccountCreditTransactionByID(ctx, txnID)
	if err != nil {
		return nil, err
	}
	if txn == nil {
		return nil, httpx.NotFound(fmt.Sprintf("Transaction %s not found ", txnID))
	}
	if txn.Status != "SUCCESS" {
		return nil, httpx.BadRequest(fmt.Sprintf("Transaction %s is %s — only a SUCCESS deposit can be refunded.", txn.ID, txn.Status))
	}
	gw, err := s.billing.GetGateway(ctx, txn.PaymentGatewayID)
	if err != nil {
		return nil, err
	}
	if gw == nil {
		return nil, httpx.NotFound("Payment gateway not found")
	}
	if !gw.IsCard() { // only refund-capable (card) gateways
		return nil, httpx.BadRequest("Cannot refund the transaction.")
	}
	g, err := cardGateway(s.gatewayFor, gw)
	if err != nil {
		return nil, err
	}
	rf, err := g.Refund(ctx, txn.ExternalID)
	if err != nil {
		return nil, err
	}
	if rf.Status == "failed" {
		return nil, httpx.BadRequest("Refund refused by the payment provider: " + gatewayErrorMessage(rf.ErrorMessage, ""))
	}
	now := time.Now().UTC()
	refund := pgdoc.M{"id": rf.ID, "status": "PENDING", "requestedAt": now}
	if rf.Status == "succeeded" {
		refund["status"] = "SUCCEEDED"
	}
	if id, _ := txn.AccountCredit["id"].(string); id != "" {
		ac, err := s.billing.AccountCreditByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if ac != nil {
			if ac.Amount != nil {
				refund["creditAmount"] = ac.Amount.String() // restored if the refund fails later
			}
			billing.MarkAccountCreditRefunded(ac, now)
			if err := s.billing.SaveAccountCredit(ctx, ac); err != nil {
				return nil, err
			}
		}
	}
	txn.Status = "REFUNDED"
	if txn.Metadata == nil {
		txn.Metadata = pgdoc.M{}
	}
	txn.Metadata["refund"] = refund
	saved, err := s.billing.SaveAccountCreditTransaction(ctx, txn)
	if err != nil {
		return nil, err
	}
	if rf.Status == "succeeded" {
		s.notifyRefunded(ctx, saved)
	}
	return saved, nil
}

// notifyRefunded mails the customer the credit note (send_refunded_invoice).
func (s *AddFundsService) notifyRefunded(ctx context.Context, txn *billing.AccountCreditTransaction) {
	if profile, _ := s.billing.FindByID(ctx, txn.BillingProfileID); profile != nil {
		s.notify(ctx, "send_refunded_invoice", profile, map[string]any{
			"firstName": profile.FirstName, "lastName": profile.LastName,
			"invoiceNumber": billing.CreditNoteNumber(txn.ID), "amount": decStr(txn.GrossAmount), "currency": txn.Currency,
		})
	}
}

// refundState reads metadata.refund off a REFUNDED transaction (nil when the refund settled
// synchronously before this tracking existed).
func refundState(txn *billing.AccountCreditTransaction) pgdoc.M {
	if txn == nil || txn.Metadata == nil {
		return nil
	}
	if v, ok := txn.Metadata["refund"].(map[string]any); ok { // pgdoc.M is this map type
		return v
	}
	return nil
}

// ReconcileRefund re-reads a still-pending refund at the gateway (the scanner's leg):
//   - SUCCEEDED → the refund is confirmed: metadata.refund.status = SUCCEEDED + the credit note mail.
//   - FAILED → the money did not go back: the voided credit is restored to the balance it had, the
//     txn returns to SUCCESS (metadata.refund.status = FAILED + the gateway reason), the customer is
//     told (refund_failed) and so is the operator's finance mailbox (refund_failed_operator).
//   - anything else → still pending, checked again next pass.
func (s *AddFundsService) ReconcileRefund(ctx context.Context, txn *billing.AccountCreditTransaction) error {
	st := refundState(txn)
	if txn.Status != "REFUNDED" || st == nil || st["status"] != "PENDING" {
		return nil
	}
	refundID, _ := st["id"].(string)
	if refundID == "" {
		return nil
	}
	gw, err := s.billing.GetGateway(ctx, txn.PaymentGatewayID)
	if err != nil || gw == nil {
		return err
	}
	g, err := cardGateway(s.gatewayFor, gw)
	if err != nil {
		return err
	}
	rf, err := g.RetrieveRefund(ctx, refundID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	switch rf.Status {
	case "succeeded":
		st["status"], st["settledAt"] = "SUCCEEDED", now
		txn.Metadata["refund"] = st
		if _, err := s.billing.SaveAccountCreditTransaction(ctx, txn); err != nil {
			return err
		}
		s.notifyRefunded(ctx, txn)
	case "failed":
		st["status"], st["failedAt"], st["failureReason"] = "FAILED", now, rf.ErrorMessage
		txn.Metadata["refund"] = st
		restored := ""
		if id, _ := txn.AccountCredit["id"].(string); id != "" {
			if ac, _ := s.billing.AccountCreditByID(ctx, id); ac != nil {
				amount := decimal.Zero
				if v, _ := st["creditAmount"].(string); v != "" {
					amount, _ = decimal.NewFromString(v)
				}
				billing.RestoreAccountCredit(ac, amount, now)
				if err := s.billing.SaveAccountCredit(ctx, ac); err != nil {
					return err
				}
				restored = amount.StringFixed(2)
			}
		}
		txn.Status = "SUCCESS"
		if _, err := s.billing.SaveAccountCreditTransaction(ctx, txn); err != nil {
			return err
		}
		s.notifyRefundFailed(ctx, txn, rf.ErrorMessage, restored)
	}
	return nil
}

// notifyRefundFailed tells the customer (refund_failed) and the finance mailbox
// (refund_failed_operator) that the gateway could not complete a refund and the credit is back.
func (s *AddFundsService) notifyRefundFailed(ctx context.Context, txn *billing.AccountCreditTransaction, reason, restored string) {
	profile, _ := s.billing.FindByID(ctx, txn.BillingProfileID)
	if profile == nil {
		return
	}
	if reason == "" {
		reason = "no reason given by the payment provider"
	}
	vars := map[string]any{
		"fullName": fullName(profile), "companyName": profile.CompanyName, "amount": decStr(txn.GrossAmount), "currency": txn.Currency,
		"restoredCredit": restored, "reason": reason, "transactionId": txn.ID, "externalId": txn.ExternalID,
		"adminUrl": s.adminURL + "/clients/billing-profiles/" + profile.ID + "?tab=transactions",
	}
	s.notify(ctx, "refund_failed", profile, vars)
	if to, _ := s.billing.FinanceEmail(ctx); to != "" && s.notifier != nil {
		_ = s.notifier.SendTemplate(ctx, "refund_failed_operator", []string{to}, vars)
	}
}

// mapStatus maps the gateway status: succeeded→SUCCESS, failed→FAILED, canceled→CANCELLED (a
// DISTINCT terminal state — CANCELLED is routed to the cancellation path,
// not the failure path; collapsing it into FAILED mislabels the terminal state), else PENDING.
func mapStatus(stripeStatus string) string {
	switch stripeStatus {
	case "succeeded":
		return "SUCCESS"
	case "failed":
		return "FAILED"
	case "canceled":
		return "CANCELLED"
	default:
		return "PENDING"
	}
}

// gatewayErrorMessage assembles the gateway error message: the gateway message, with the
// code appended as " (code)" when present.
func gatewayErrorMessage(message, code string) string {
	if code != "" {
		return message + " (" + code + ")"
	}
	return message
}

// allVerified: empty verifications pass; otherwise every entry must be verified.
func allVerified(verifications []any) bool {
	for _, v := range verifications {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		verified, _ := m["verified"].(bool)
		if !verified {
			if iv, _ := m["isVerified"].(bool); iv {
				continue
			}
			return false
		}
	}
	return true
}

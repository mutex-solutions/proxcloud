package payment

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// registercard.go registers a card the way the big clouds do: a 1.00 authorization hold on the
// new card (which also saves it at the gateway for later merchant-initiated charges), confirmed in
// the browser, then — in the confirm callback — the card is stored and the hold is released. The
// customer sees a pending 1.00 that disappears; nothing is captured.
//
// RegisterCard creates a PENDING CreditCardTransaction + the verification intent and returns its
// client secret; ProcessRegisterCard (the card-confirm callback) retrieves the intent and, on
// success, stores the verified card as a CreditCard and voids the hold.

// CardVerificationAmountCents is the verification hold: 1.00 in the profile's currency.
const CardVerificationAmountCents int64 = 100

// RegisterCardRequest is the register-card request.
type RegisterCardRequest struct {
	PaymentGatewayID string `json:"paymentGatewayId"`
}

// RegisterCardResponse is the register-card response. Metadata carries what the browser needs
// to confirm the verification: client_secret (Stripe + Airwallex), customerId + currency
// (Airwallex Elements) and the hold amount for the UI copy.
type RegisterCardResponse struct {
	TransactionID     string `json:"transactionId,omitempty"`
	ExternalPaymentID string `json:"externalPaymentId,omitempty"`
	ThirdParty        string `json:"thirdParty,omitempty"`
	Metadata          any    `json:"metadata,omitempty"`
}

// RegisterCardService orchestrates card registration.
type RegisterCardService struct {
	billing    *billing.Repo
	gatewayFor GatewayFactory
	log        *slog.Logger
}

func NewRegisterCardService(b *billing.Repo, gatewayFor GatewayFactory) *RegisterCardService {
	return &RegisterCardService{billing: b, gatewayFor: gatewayFor, log: slog.Default()}
}

func (s *RegisterCardService) RegisterCard(ctx context.Context, profile *billing.BillingProfile, req RegisterCardRequest) (*RegisterCardResponse, error) {
	if !allVerified(profile.Verifications) {
		return nil, httpx.BadRequest("Your account is not verified.")
	}
	if req.PaymentGatewayID == "" {
		return nil, httpx.BadRequest("Payment gateway id is required.")
	}
	gw, err := s.billing.GetGateway(ctx, req.PaymentGatewayID)
	if err != nil {
		return nil, err
	}
	if gw == nil {
		return nil, httpx.NotFound("Payment gateway not found")
	}
	g, err := cardGateway(s.gatewayFor, gw)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(profile.Currency) == "" {
		return nil, httpx.BadRequest("The billing profile has no currency yet — fill in the billing details first.")
	}
	zero, one := decimal.Zero, decimal.NewFromInt(1)
	txn := &billing.CreditCardTransaction{
		Status: "PENDING", PaymentGatewayID: gw.ID, BillingProfileID: profile.ID,
		Amount: &zero, InvoiceGatewayID: gw.ID, Currency: profile.Currency, ExchangeRate: &one,
	}
	txn, err = s.billing.SaveCreditCardTransaction(ctx, txn)
	if err != nil {
		return nil, err
	}
	cust, err := g.GetOrCreateCustomer(ctx, customerInput(profile))
	if err != nil {
		return nil, err
	}
	si, err := g.CreateSetupIntent(ctx, SetupIntentInput{
		CustomerID: cust, Description: "Card verification", AmountCents: CardVerificationAmountCents, Currency: profile.Currency,
	})
	if err != nil {
		return nil, err
	}
	txn.ExternalID = si.ID
	if _, err := s.billing.SaveCreditCardTransaction(ctx, txn); err != nil {
		return nil, err
	}
	// The FE reads metadata.client_secret to confirm the verification intent with the card element.
	return &RegisterCardResponse{TransactionID: txn.ID, ExternalPaymentID: si.ID, ThirdParty: gw.ThirdParty, Metadata: map[string]any{
		"client_secret":      si.ClientSecret,
		"customerId":         cust,
		"currency":           profile.Currency,
		"verificationAmount": decimal.New(CardVerificationAmountCents, -2).StringFixed(2),
	}}, nil
}

// ProcessRegisterCard handles the card-confirm callback: retrieve the verification intent and, on
// success, store the verified card as a CreditCard, release the hold and mark the transaction
// SUCCESS (idempotent). FAILED/CANCELLED set the status; PENDING leaves it.
func (s *RegisterCardService) ProcessRegisterCard(ctx context.Context, txnID string) (*billing.CreditCardTransaction, error) {
	txn, err := s.billing.CreditCardTransactionByID(ctx, txnID)
	if err != nil {
		return nil, err
	}
	if txn == nil {
		return nil, httpx.NotFound("Transaction not found")
	}
	if txn.Status == "SUCCESS" { // idempotent (a repeated callback must not store the card twice)
		return txn, nil
	}
	gw, err := s.billing.GetGateway(ctx, txn.PaymentGatewayID)
	if err != nil {
		return nil, err
	}
	if gw == nil {
		return nil, httpx.NotFound("Payment gateway not found")
	}
	g, err := cardGateway(s.gatewayFor, gw)
	if err != nil {
		return nil, err
	}
	si, err := g.RetrieveSetupIntent(ctx, txn.ExternalID)
	if err != nil {
		return nil, err
	}
	switch mapStatus(si.Status) {
	case "SUCCESS":
		exp := time.Date(int(si.Card.ExpYear), time.Month(si.Card.ExpMonth), 1, 0, 0, 0, 0, time.UTC)
		// Duplicate guard for a repeated / concurrent callback: the same gateway token is stored once.
		if existing, err := s.billing.CreditCardByToken(ctx, txn.BillingProfileID, si.Card.TokenID); err != nil {
			return nil, err
		} else if existing != nil {
			txn.Status = "SUCCESS"
			return s.billing.SaveCreditCardTransaction(ctx, txn)
		}
		if err := s.billing.CreateCreditCard(ctx, &billing.CreditCard{
			BillingProfileID:    txn.BillingProfileID,
			TokenID:             si.Card.TokenID,
			PanMasked:           si.Card.PanMasked,
			TokenExpirationDate: &exp,
			PaymentGatewayID:    txn.PaymentGatewayID,
			Metadata:            map[string]any{}, // stores an EMPTY map → "metadata":{} on the wire
		}); err != nil {
			return nil, err
		}
		txn.Status = "SUCCESS"
		// Release the 1.00 hold now that the card is stored. Best-effort with one retry: an
		// unreleased authorization expires at the gateway on its own (7 days), so a transient error
		// here must not fail a registration that already succeeded.
		if rerr := g.ReleaseSetupIntent(ctx, txn.ExternalID); rerr != nil {
			if rerr = g.ReleaseSetupIntent(ctx, txn.ExternalID); rerr != nil {
				s.log.Error("register-card: release verification hold", "txn", txn.ID, "intent", txn.ExternalID, "err", rerr)
				txn.ErrorMessage = "verification hold not released: " + rerr.Error()
			}
		}
	case "FAILED", "CANCELLED":
		txn.Status = "FAILED"
		txn.ErrorMessage = si.ErrorMessage
		// A hold may have been authorized even though the card could not be saved: release it.
		if rerr := g.ReleaseSetupIntent(ctx, txn.ExternalID); rerr != nil {
			s.log.Error("register-card: release verification hold after failure", "txn", txn.ID, "intent", txn.ExternalID, "err", rerr)
		}
	default: // PENDING
		return txn, nil
	}
	return s.billing.SaveCreditCardTransaction(ctx, txn)
}

func customerInput(p *billing.BillingProfile) CustomerInput {
	return CustomerInput{
		BillingProfileID: p.ID, Email: p.Email,
		Name: strings.TrimSpace(p.FirstName + " " + p.LastName), Country: p.Country,
	}
}

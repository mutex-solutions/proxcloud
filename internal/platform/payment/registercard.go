package payment

import (
	"context"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// registercard.go registers a card via a Stripe SetupIntent (no
// charge). RegisterCard creates a PENDING CreditCardTransaction + a SetupIntent and returns its
// client secret; ProcessRegisterCard (the card-confirm callback) retrieves the SetupIntent and,
// on success, stores the resulting card PaymentMethod as a CreditCard.

// RegisterCardRequest is the register-card request.
type RegisterCardRequest struct {
	PaymentGatewayID string `json:"paymentGatewayId"`
}

// RegisterCardResponse is the register-card response.
type RegisterCardResponse struct {
	TransactionID     string `json:"transactionId,omitempty"`
	ExternalPaymentID string `json:"externalPaymentId,omitempty"`
	ThirdParty        string `json:"thirdParty,omitempty"`
	Metadata          any    `json:"metadata,omitempty"`
}

// RegisterCardService orchestrates card registration.
type RegisterCardService struct {
	billing    *billing.Repo
	gatewayFor func(secretKey string) Gateway
}

func NewRegisterCardService(b *billing.Repo, gatewayFor func(secretKey string) Gateway) *RegisterCardService {
	return &RegisterCardService{billing: b, gatewayFor: gatewayFor}
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
	zero, one := decimal.Zero, decimal.NewFromInt(1)
	txn := &billing.CreditCardTransaction{
		Status: "PENDING", PaymentGatewayID: gw.ID, BillingProfileID: profile.ID,
		Amount: &zero, InvoiceGatewayID: gw.ID, Currency: profile.Currency, ExchangeRate: &one,
	}
	txn, err = s.billing.SaveCreditCardTransaction(ctx, txn)
	if err != nil {
		return nil, err
	}
	if gw.ThirdParty != "Stripe" {
		return nil, httpx.BadRequest("Unsupported payment gateway: " + gw.ThirdParty)
	}
	g := s.gatewayFor(gw.SecretString("privateKey"))
	cust, err := g.GetOrCreateCustomer(ctx, customerInput(profile))
	if err != nil {
		return nil, err
	}
	si, err := g.CreateSetupIntent(ctx, SetupIntentInput{CustomerID: cust, Description: "Register card"})
	if err != nil {
		return nil, err
	}
	txn.ExternalID = si.ID
	if _, err := s.billing.SaveCreditCardTransaction(ctx, txn); err != nil {
		return nil, err
	}
	// registerCard returns metadata as a Map {"client_secret": <secret>}
	// (NOT a bare string) — the FE reads metadata.client_secret to init stripe.elements.
	return &RegisterCardResponse{TransactionID: txn.ID, ExternalPaymentID: si.ID, ThirdParty: gw.ThirdParty, Metadata: map[string]string{"client_secret": si.ClientSecret}}, nil
}

// ProcessRegisterCard handles the card-confirm callback: retrieve the
// SetupIntent, and on success store the card PaymentMethod as a CreditCard + mark SUCCESS
// (idempotent). FAILED/CANCELLED set the status; PENDING leaves it.
func (s *RegisterCardService) ProcessRegisterCard(ctx context.Context, txnID string) (*billing.CreditCardTransaction, error) {
	txn, err := s.billing.CreditCardTransactionByID(ctx, txnID)
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
	g := s.gatewayFor(gw.SecretString("privateKey"))
	si, err := g.RetrieveSetupIntent(ctx, txn.ExternalID)
	if err != nil {
		return nil, err
	}
	switch mapStatus(si.Status) {
	case "SUCCESS":
		if txn.Status == "SUCCESS" { // idempotent (checkTransactionIsNotProcessed)
			return txn, nil
		}
		profile, err := s.billing.FindByID(ctx, txn.BillingProfileID)
		if err != nil {
			return nil, err
		}
		if profile != nil {
			cust, err := g.GetOrCreateCustomer(ctx, customerInput(profile))
			if err != nil {
				return nil, err
			}
			card, err := g.LatestCardForCustomer(ctx, cust)
			if err != nil {
				return nil, err
			}
			exp := time.Date(int(card.ExpYear), time.Month(card.ExpMonth), 1, 0, 0, 0, 0, time.UTC)
			if err := s.billing.CreateCreditCard(ctx, &billing.CreditCard{
				BillingProfileID:    txn.BillingProfileID,
				TokenID:             card.TokenID,
				PanMasked:           card.PanMasked,
				TokenExpirationDate: &exp,
				PaymentGatewayID:    txn.PaymentGatewayID,
				Metadata:            map[string]any{}, // stores an EMPTY map → "metadata":{} on the wire
			}); err != nil {
				return nil, err
			}
		}
		txn.Status = "SUCCESS"
	case "FAILED":
		txn.Status = "FAILED"
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

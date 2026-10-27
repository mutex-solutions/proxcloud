// Package payment implements the Stripe payment gateway operations. The Gateway interface is the
// testable boundary over a Stripe API key; StripeGateway is the concrete stripe-go (v82) impl,
// constructed per thirdPartyIntegration secret (the API key is set per call).
package payment

import (
	"context"
	"fmt"
	"strings"

	stripe "github.com/stripe/stripe-go/v82"
	scl "github.com/stripe/stripe-go/v82/client"
)

// CustomerInput resolves/creates the Stripe Customer for a billing profile
// (search by metadata billingProfileId, else create).
type CustomerInput struct {
	BillingProfileID string
	Email            string
	Name             string
	Country          string
	City             string
	Line1            string
}

// PaymentIntentInput carries the add-funds PaymentIntent.create params (amount already in
// cents). OffSession sets setup_future_usage=off_session (store the card for later).
type PaymentIntentInput struct {
	CustomerID  string
	AmountCents int64
	Currency    string
	Description string
	OffSession  bool
}

// PaymentIntentResult is the created/retrieved PaymentIntent (the fields ProxCloud persists/returns).
type PaymentIntentResult struct {
	ID                 string
	ClientSecret       string
	Status             string
	ErrorMessage       string // pi.LastPaymentError.Msg (gatewayStatus.message) — for a FAILED collect/deposit
	ErrorCode          string // pi.LastPaymentError DeclineCode (preferred) or Code (gatewayStatus.code)
	CancellationReason string // pi.CancellationReason — for a CANCELLED deposit
}

// CollectInput carries the collect (charge-a-saved-card) PaymentIntent params:
// charge the stored card token immediately (confirm=true). The Stripe
// customer is resolved from the PaymentMethod itself, so only the card token is needed.
type CollectInput struct {
	CardTokenID string
	AmountCents int64
	Currency    string
}

// RefundResult is the created Refund (the fields ProxCloud maps: gateway status + a failure reason).
type RefundResult struct {
	Status       string
	ErrorMessage string
}

// SetupIntentInput / Result carry the register-card SetupIntent.create params (no charge — just
// stores a card for future off-session use).
type SetupIntentInput struct {
	CustomerID  string
	Description string
}
type SetupIntentResult struct {
	ID           string
	ClientSecret string
	Status       string
}

// CardDetails is the stored-card info pulled from the customer's latest card PaymentMethod:
// tokenId = pm_id, panMasked = "*.<last4>", expiry.
type CardDetails struct {
	TokenID   string
	PanMasked string
	ExpMonth  int64
	ExpYear   int64
}

// Gateway is the Stripe operation boundary (so the payment flows are unit-testable with a fake and
// live-testable with the sandbox).
type Gateway interface {
	GetOrCreateCustomer(ctx context.Context, in CustomerInput) (string, error)
	CreatePaymentIntent(ctx context.Context, in PaymentIntentInput) (PaymentIntentResult, error)
	RetrievePaymentIntent(ctx context.Context, id string) (PaymentIntentResult, error)
	CollectPaymentIntent(ctx context.Context, in CollectInput) (PaymentIntentResult, error)
	Refund(ctx context.Context, paymentIntentID string) (RefundResult, error)
	CreateSetupIntent(ctx context.Context, in SetupIntentInput) (SetupIntentResult, error)
	RetrieveSetupIntent(ctx context.Context, id string) (SetupIntentResult, error)
	LatestCardForCustomer(ctx context.Context, customerID string) (CardDetails, error)
}

// StripeGateway is the concrete stripe-go impl, keyed by one integration's secret key.
type StripeGateway struct{ sc *scl.API }

// NewStripeGateway builds a gateway bound to a Stripe secret key (sk_*).
func NewStripeGateway(secretKey string) *StripeGateway {
	api := &scl.API{}
	api.Init(secretKey, nil)
	return &StripeGateway{sc: api}
}

// escapeSearchValue escapes a value for interpolation into a Stripe Search Query Language string
// literal.
//
// Stripe's search grammar is SQL-like: clauses can be combined with AND/OR and can reference other
// fields. A raw double quote in an interpolated value therefore closes the literal and everything
// after it is parsed as query syntax, so an id like `x" OR metadata["k"]:"y` would silently widen
// the search and can resolve the WRONG CUSTOMER — which this function then charges or credits.
//
// Every caller today passes a pgdoc-generated hex id, so nothing can currently reach it. But
// BillingProfileID is a plain exported string with no validation at the type boundary, and "no
// caller does that yet" is not a control. Backslash is escaped first so it cannot be used to
// re-escape the quote we add.
func escapeSearchValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	return strings.ReplaceAll(v, `"`, `\"`)
}

// GetOrCreateCustomer finds a Customer by the billingProfileId
// metadata, else create one with that metadata (so the lookup is idempotent).
func (g *StripeGateway) GetOrCreateCustomer(_ context.Context, in CustomerInput) (string, error) {
	// Refuse an empty id outright: an empty search value matches broadly, and the customer we would
	// then return belongs to somebody else.
	if strings.TrimSpace(in.BillingProfileID) == "" {
		return "", fmt.Errorf("stripe: refusing to look up a customer with an empty billingProfileId")
	}
	q := fmt.Sprintf(`metadata["billingProfileId"]:"%s"`, escapeSearchValue(in.BillingProfileID))
	iter := g.sc.Customers.Search(&stripe.CustomerSearchParams{SearchParams: stripe.SearchParams{Query: q}})
	for iter.Next() {
		return iter.Customer().ID, nil
	}
	if err := iter.Err(); err != nil {
		return "", err
	}
	params := &stripe.CustomerParams{
		Email:    stripe.String(in.Email),
		Name:     stripe.String(in.Name),
		Metadata: map[string]string{"billingProfileId": in.BillingProfileID},
	}
	if in.Country != "" || in.City != "" || in.Line1 != "" {
		params.Address = &stripe.AddressParams{
			Country: stripe.String(in.Country), City: stripe.String(in.City), Line1: stripe.String(in.Line1),
		}
	}
	c, err := g.sc.Customers.New(params)
	if err != nil {
		return "", err
	}
	return c.ID, nil
}

// CreatePaymentIntent creates the add-funds PaymentIntent (automatic payment methods on).
func (g *StripeGateway) CreatePaymentIntent(_ context.Context, in PaymentIntentInput) (PaymentIntentResult, error) {
	params := &stripe.PaymentIntentParams{
		Customer:                stripe.String(in.CustomerID),
		Amount:                  stripe.Int64(in.AmountCents),
		Currency:                stripe.String(in.Currency),
		Description:             stripe.String(in.Description),
		AutomaticPaymentMethods: &stripe.PaymentIntentAutomaticPaymentMethodsParams{Enabled: stripe.Bool(true)},
	}
	if in.OffSession {
		params.SetupFutureUsage = stripe.String("off_session")
	}
	pi, err := g.sc.PaymentIntents.New(params)
	if err != nil {
		return PaymentIntentResult{}, err
	}
	return PaymentIntentResult{ID: pi.ID, ClientSecret: pi.ClientSecret, Status: string(pi.Status)}, nil
}

// RetrievePaymentIntent re-reads a PaymentIntent (the redirect-callback confirm path).
func (g *StripeGateway) RetrievePaymentIntent(_ context.Context, id string) (PaymentIntentResult, error) {
	pi, err := g.sc.PaymentIntents.Get(id, nil)
	if err != nil {
		return PaymentIntentResult{}, err
	}
	res := PaymentIntentResult{ID: pi.ID, ClientSecret: pi.ClientSecret, Status: string(pi.Status), CancellationReason: string(pi.CancellationReason)}
	if e := pi.LastPaymentError; e != nil {
		res.ErrorMessage = e.Msg
		res.ErrorCode = string(e.Code)
		if e.DeclineCode != "" { // a card decline carries the more specific reason
			res.ErrorCode = string(e.DeclineCode)
		}
	}
	return res, nil
}

// CollectPaymentIntent retrieves the saved card's PaymentMethod
// (for its customer), then create a confirm=true PaymentIntent charging that card immediately.
func (g *StripeGateway) CollectPaymentIntent(_ context.Context, in CollectInput) (PaymentIntentResult, error) {
	pm, err := g.sc.PaymentMethods.Get(in.CardTokenID, nil)
	if err != nil {
		return PaymentIntentResult{}, err
	}
	params := &stripe.PaymentIntentParams{
		Description:   stripe.String("Collect funds"),
		PaymentMethod: stripe.String(in.CardTokenID),
		Amount:        stripe.Int64(in.AmountCents),
		Currency:      stripe.String(in.Currency),
		Confirm:       stripe.Bool(true),
		// off-session merchant-initiated charge of a saved card: disable redirect-based methods so
		// confirm=true does not require a return_url (allow_redirects=never disables redirect-based
		// methods, which is what a direct card collect needs).
		AutomaticPaymentMethods: &stripe.PaymentIntentAutomaticPaymentMethodsParams{
			Enabled:        stripe.Bool(true),
			AllowRedirects: stripe.String("never"),
		},
	}
	if pm.Customer != nil {
		params.Customer = stripe.String(pm.Customer.ID)
	}
	pi, err := g.sc.PaymentIntents.New(params)
	if err != nil {
		return PaymentIntentResult{}, err
	}
	return PaymentIntentResult{ID: pi.ID, ClientSecret: pi.ClientSecret, Status: string(pi.Status)}, nil
}

// Refund issues a full refund of the original PaymentIntent (the gateway
// status + failure reason are mapped by the caller).
func (g *StripeGateway) Refund(_ context.Context, paymentIntentID string) (RefundResult, error) {
	rf, err := g.sc.Refunds.New(&stripe.RefundParams{PaymentIntent: stripe.String(paymentIntentID)})
	if err != nil {
		return RefundResult{}, err
	}
	return RefundResult{Status: string(rf.Status), ErrorMessage: string(rf.FailureReason)}, nil
}

// CreateSetupIntent creates the register-card SetupIntent (card type, no charge).
func (g *StripeGateway) CreateSetupIntent(_ context.Context, in SetupIntentInput) (SetupIntentResult, error) {
	si, err := g.sc.SetupIntents.New(&stripe.SetupIntentParams{
		Customer:           stripe.String(in.CustomerID),
		Description:        stripe.String(in.Description),
		PaymentMethodTypes: stripe.StringSlice([]string{"card"}),
	})
	if err != nil {
		return SetupIntentResult{}, err
	}
	return SetupIntentResult{ID: si.ID, ClientSecret: si.ClientSecret, Status: string(si.Status)}, nil
}

// RetrieveSetupIntent re-reads a SetupIntent (the card-confirm callback).
func (g *StripeGateway) RetrieveSetupIntent(_ context.Context, id string) (SetupIntentResult, error) {
	si, err := g.sc.SetupIntents.Get(id, nil)
	if err != nil {
		return SetupIntentResult{}, err
	}
	return SetupIntentResult{ID: si.ID, ClientSecret: si.ClientSecret, Status: string(si.Status)}, nil
}

// LatestCardForCustomer returns the customer's first card PaymentMethod
// (PaymentMethod.list(customer, CARD), first entry).
func (g *StripeGateway) LatestCardForCustomer(_ context.Context, customerID string) (CardDetails, error) {
	iter := g.sc.PaymentMethods.List(&stripe.PaymentMethodListParams{
		Customer: stripe.String(customerID), Type: stripe.String("card"),
	})
	for iter.Next() {
		pm := iter.PaymentMethod()
		if pm.Card != nil {
			return CardDetails{
				TokenID:   pm.ID,
				PanMasked: "*." + pm.Card.Last4,
				ExpMonth:  pm.Card.ExpMonth,
				ExpYear:   pm.Card.ExpYear,
			}, nil
		}
	}
	if err := iter.Err(); err != nil {
		return CardDetails{}, err
	}
	return CardDetails{}, fmt.Errorf("no card payment method for customer %s", customerID)
}

// Package payment implements the Stripe payment gateway operations. The Gateway interface is the
// testable boundary over a Stripe API key; StripeGateway is the concrete stripe-go (v82) impl,
// constructed per thirdPartyIntegration secret (the API key is set per call).
package payment

import (
	"context"
	"errors"
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
	// Reference is the collect transaction id: the gateway idempotency key (Stripe
	// Idempotency-Key / Airwallex request_id), so a retried request never charges twice.
	Reference string
}

// RefundResult is a Refund as ProxCloud sees it: the gateway refund id (polled later while it is
// still processing), a normalized Status — succeeded | failed | pending (accepted, not settled
// yet) — and the gateway's failure reason.
type RefundResult struct {
	ID           string
	Status       string
	ErrorMessage string
}

// SetupIntentInput / Result carry the register-card verification: a small authorization hold
// (AmountCents in Currency — 1.00 by default) on the new card that also saves it for future
// off-session (merchant-initiated) charges. The hold is released (ReleaseSetupIntent) as soon as
// the card is stored, so the customer only ever sees a pending 1.00 that disappears — the same
// verification the big clouds run when a card is added.
type SetupIntentInput struct {
	CustomerID  string
	Description string
	AmountCents int64
	Currency    string
}

// SetupIntentResult is the verification intent: ClientSecret for the client-side confirm, and,
// once the customer confirmed, Status "succeeded" (authorized) with the verified Card.
type SetupIntentResult struct {
	ID           string
	ClientSecret string
	Status       string // succeeded | failed | canceled | anything else = still pending
	ErrorMessage string
	Card         CardDetails // set on succeeded — the exact card the customer just verified
}

// CardDetails is the stored-card info: TokenID = the gateway's reusable token (Stripe pm_…,
// Airwallex payment consent cst_…), PanMasked = "*.<last4>", expiry.
type CardDetails struct {
	TokenID   string
	PanMasked string
	ExpMonth  int64
	ExpYear   int64
}

// Gateway is the card-gateway operation boundary (Stripe and Airwallex implement it; the payment
// flows are unit-testable with a fake and live-testable against each sandbox).
type Gateway interface {
	GetOrCreateCustomer(ctx context.Context, in CustomerInput) (string, error)
	CreatePaymentIntent(ctx context.Context, in PaymentIntentInput) (PaymentIntentResult, error)
	RetrievePaymentIntent(ctx context.Context, id string) (PaymentIntentResult, error)
	CollectPaymentIntent(ctx context.Context, in CollectInput) (PaymentIntentResult, error)
	Refund(ctx context.Context, paymentIntentID string) (RefundResult, error)
	// RetrieveRefund re-reads a refund that was still pending when created (the scanner's poll).
	RetrieveRefund(ctx context.Context, refundID string) (RefundResult, error)
	CreateSetupIntent(ctx context.Context, in SetupIntentInput) (SetupIntentResult, error)
	RetrieveSetupIntent(ctx context.Context, id string) (SetupIntentResult, error)
	// ReleaseSetupIntent voids the verification hold: cancels the uncaptured authorization, or
	// refunds it if the gateway captured anyway. Safe to repeat.
	ReleaseSetupIntent(ctx context.Context, id string) error
	// Ping checks the credentials with a cheap authenticated call (the admin health check).
	Ping(ctx context.Context) error
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

// CollectPaymentIntent charges a saved card off-session in two steps — create the PaymentIntent
// (idempotent on the collect transaction id), then confirm it — so a transport error on the
// confirm leaves a known intent id for the scanner to reconcile instead of an unknown outcome, and
// a retried create never produces a second charge. The Stripe customer is resolved from the
// PaymentMethod itself, so only the card token is needed.
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
		// off-session merchant-initiated charge of a saved card: disable redirect-based methods so
		// the confirm does not require a return_url.
		AutomaticPaymentMethods: &stripe.PaymentIntentAutomaticPaymentMethodsParams{
			Enabled:        stripe.Bool(true),
			AllowRedirects: stripe.String("never"),
		},
	}
	if pm.Customer != nil {
		params.Customer = stripe.String(pm.Customer.ID)
	}
	if in.Reference != "" {
		params.SetIdempotencyKey("collect-create-" + in.Reference)
	}
	pi, err := g.sc.PaymentIntents.New(params)
	if err != nil {
		return PaymentIntentResult{}, err
	}
	confirm := &stripe.PaymentIntentConfirmParams{OffSession: stripe.Bool(true)}
	if in.Reference != "" {
		confirm.SetIdempotencyKey("collect-confirm-" + in.Reference)
	}
	done, err := g.sc.PaymentIntents.Confirm(pi.ID, confirm)
	if err != nil {
		// A decline comes back as a card error: report it as a failed result with the decline reason
		// so the collect ends FAILED and the customer is told. Anything else is an unknown outcome
		// on a known intent (the caller keeps it PENDING for the scanner).
		var se *stripe.Error
		if errors.As(err, &se) && se.Type == stripe.ErrorTypeCard {
			res := PaymentIntentResult{ID: pi.ID, Status: "failed", ErrorMessage: se.Msg, ErrorCode: string(se.Code)}
			if se.DeclineCode != "" {
				res.ErrorCode = string(se.DeclineCode)
			}
			return res, nil
		}
		return PaymentIntentResult{ID: pi.ID, Status: "pending"}, err
	}
	res := PaymentIntentResult{ID: done.ID, ClientSecret: done.ClientSecret, Status: string(done.Status)}
	if e := done.LastPaymentError; e != nil {
		res.ErrorMessage, res.ErrorCode = e.Msg, string(e.Code)
		if e.DeclineCode != "" {
			res.ErrorCode = string(e.DeclineCode)
		}
	}
	// A confirm that completed without succeeding (requires_action / requires_payment_method) is a
	// failed merchant-initiated charge: the customer must pay from the console.
	switch done.Status {
	case stripe.PaymentIntentStatusRequiresAction, stripe.PaymentIntentStatusRequiresPaymentMethod:
		res.Status = "failed"
		if res.ErrorMessage == "" {
			res.ErrorMessage = "The card issuer requires the cardholder to authenticate this payment"
		}
	}
	return res, nil
}

// Refund issues a full refund of the original PaymentIntent (the gateway
// status + failure reason are mapped by the caller).
func (g *StripeGateway) Refund(_ context.Context, paymentIntentID string) (RefundResult, error) {
	rf, err := g.sc.Refunds.New(&stripe.RefundParams{PaymentIntent: stripe.String(paymentIntentID)})
	if err != nil {
		return RefundResult{}, err
	}
	return stripeRefundResult(rf), nil
}

// RetrieveRefund re-reads a refund (a `pending` one the scanner keeps polling).
func (g *StripeGateway) RetrieveRefund(_ context.Context, refundID string) (RefundResult, error) {
	rf, err := g.sc.Refunds.Get(refundID, nil)
	if err != nil {
		return RefundResult{}, err
	}
	return stripeRefundResult(rf), nil
}

// stripeRefundResult normalizes a Stripe refund: succeeded / pending stay, failed and canceled are
// failed (the failure reason is the issuer's).
func stripeRefundResult(rf *stripe.Refund) RefundResult {
	res := RefundResult{ID: rf.ID, Status: string(rf.Status), ErrorMessage: string(rf.FailureReason)}
	switch rf.Status {
	case stripe.RefundStatusSucceeded:
		res.Status = "succeeded"
	case stripe.RefundStatusPending, stripe.RefundStatusRequiresAction:
		res.Status = "pending"
	default:
		res.Status = "failed"
	}
	return res
}

// CreateSetupIntent creates the register-card verification: a manual-capture PaymentIntent for the
// hold amount with setup_future_usage=off_session, so confirming it with the card both authorizes
// the hold and attaches the card to the customer for later merchant-initiated charges.
func (g *StripeGateway) CreateSetupIntent(_ context.Context, in SetupIntentInput) (SetupIntentResult, error) {
	pi, err := g.sc.PaymentIntents.New(&stripe.PaymentIntentParams{
		Customer:           stripe.String(in.CustomerID),
		Amount:             stripe.Int64(in.AmountCents),
		Currency:           stripe.String(in.Currency),
		Description:        stripe.String(in.Description),
		CaptureMethod:      stripe.String(string(stripe.PaymentIntentCaptureMethodManual)),
		SetupFutureUsage:   stripe.String(string(stripe.PaymentIntentSetupFutureUsageOffSession)),
		PaymentMethodTypes: stripe.StringSlice([]string{"card"}),
	})
	if err != nil {
		return SetupIntentResult{}, err
	}
	return SetupIntentResult{ID: pi.ID, ClientSecret: pi.ClientSecret, Status: string(pi.Status)}, nil
}

// RetrieveSetupIntent re-reads the verification PaymentIntent (the card-confirm callback) with its
// PaymentMethod expanded: requires_capture (authorized) or succeeded → "succeeded" + the card.
func (g *StripeGateway) RetrieveSetupIntent(_ context.Context, id string) (SetupIntentResult, error) {
	if strings.HasPrefix(id, "seti_") {
		return g.retrieveLegacySetupIntent(id)
	}
	params := &stripe.PaymentIntentParams{}
	params.AddExpand("payment_method")
	pi, err := g.sc.PaymentIntents.Get(id, params)
	if err != nil {
		return SetupIntentResult{}, err
	}
	res := SetupIntentResult{ID: pi.ID, ClientSecret: pi.ClientSecret, Status: string(pi.Status)}
	if e := pi.LastPaymentError; e != nil {
		res.ErrorMessage = gatewayErrorMessage(e.Msg, string(e.DeclineCode))
	}
	switch pi.Status {
	case stripe.PaymentIntentStatusRequiresCapture, stripe.PaymentIntentStatusSucceeded:
		pm := pi.PaymentMethod
		if pm == nil || pm.Card == nil {
			return res, fmt.Errorf("stripe: verification %s has no card payment method", id)
		}
		res.Status = "succeeded"
		res.Card = CardDetails{TokenID: pm.ID, PanMasked: "*." + pm.Card.Last4, ExpMonth: pm.Card.ExpMonth, ExpYear: pm.Card.ExpYear}
	case stripe.PaymentIntentStatusCanceled:
		res.Status = "canceled"
	}
	return res, nil
}

// retrieveLegacySetupIntent finishes a card registration started before the verification hold
// existed (a 0-amount SetupIntent, seti_…): succeeded → the attached PaymentMethod is the card.
func (g *StripeGateway) retrieveLegacySetupIntent(id string) (SetupIntentResult, error) {
	params := &stripe.SetupIntentParams{}
	params.AddExpand("payment_method")
	si, err := g.sc.SetupIntents.Get(id, params)
	if err != nil {
		return SetupIntentResult{}, err
	}
	res := SetupIntentResult{ID: si.ID, ClientSecret: si.ClientSecret, Status: string(si.Status)}
	if si.Status == stripe.SetupIntentStatusSucceeded && si.PaymentMethod != nil && si.PaymentMethod.Card != nil {
		pm := si.PaymentMethod
		res.Card = CardDetails{TokenID: pm.ID, PanMasked: "*." + pm.Card.Last4, ExpMonth: pm.Card.ExpMonth, ExpYear: pm.Card.ExpYear}
	}
	return res, nil
}

// ReleaseSetupIntent voids the verification hold: cancel the uncaptured authorization; refund it
// when it was captured; nothing to do otherwise (a legacy seti_ SetupIntent has no hold).
func (g *StripeGateway) ReleaseSetupIntent(_ context.Context, id string) error {
	if strings.HasPrefix(id, "seti_") {
		return nil
	}
	pi, err := g.sc.PaymentIntents.Get(id, nil)
	if err != nil {
		return err
	}
	switch pi.Status {
	case stripe.PaymentIntentStatusRequiresCapture:
		_, err = g.sc.PaymentIntents.Cancel(id, &stripe.PaymentIntentCancelParams{
			CancellationReason: stripe.String(string(stripe.PaymentIntentCancellationReasonRequestedByCustomer)),
		})
		return err
	case stripe.PaymentIntentStatusSucceeded:
		_, err = g.sc.Refunds.New(&stripe.RefundParams{PaymentIntent: stripe.String(id)})
		return err
	}
	return nil
}

// Ping verifies the secret key with a balance read.
func (g *StripeGateway) Ping(_ context.Context) error {
	_, err := g.sc.Balance.Get(nil)
	return err
}

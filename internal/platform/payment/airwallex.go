package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// airwallex.go is the Airwallex Payment Acceptance implementation of Gateway. Only the server
// side of the flows lives here — card data never reaches ProxCloud: the console mounts the
// Airwallex Elements SDK with the intent's client_secret and confirms it in the browser.
//
// Mapping onto the Gateway vocabulary:
//   - customer            → /pa/customers, keyed by merchant_customer_id = billing profile id
//   - PaymentIntent       → /pa/payment_intents (create / retrieve / confirm / cancel)
//   - saved card token    → a PaymentConsent (cst_…) with next_triggered_by=merchant, created by
//                           the browser while confirming the 1.00 verification intent
//   - collect             → create an intent for the consent's customer + confirm it with
//                           payment_consent_reference (merchant-initiated, no customer present)
//   - refund              → /pa/refunds/create
//
// Statuses are normalized to the Stripe words the services already map (succeeded / failed /
// canceled / requires_capture / pending). Amounts are decimal major units on the wire.

const (
	airwallexProdHost = "https://api.airwallex.com"
	airwallexDemoHost = "https://api-demo.airwallex.com"
)

// AirwallexError is a non-2xx Airwallex API response ({code, message}).
type AirwallexError struct {
	HTTPStatus int
	Code       string
	Message    string
}

func (e *AirwallexError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("airwallex: %s (%s)", e.Message, e.Code)
	}
	return fmt.Sprintf("airwallex: HTTP %d: %s", e.HTTPStatus, e.Message)
}

// AirwallexGateway is the Gateway bound to one Airwallex API key (client id + key) and host.
type AirwallexGateway struct {
	host     string
	clientID string
	apiKey   string
	http     *http.Client
	now      func() time.Time

	mu       sync.Mutex
	token    string
	tokenExp time.Time

	pollInterval time.Duration // refund status poll interval override (tests)
}

// NewAirwallexGateway builds a gateway; sandbox selects the demo host.
func NewAirwallexGateway(clientID, apiKey string, sandbox bool) *AirwallexGateway {
	host := airwallexProdHost
	if sandbox {
		host = airwallexDemoHost
	}
	return &AirwallexGateway{host: host, clientID: clientID, apiKey: apiKey, http: &http.Client{Timeout: 30 * time.Second}, now: time.Now}
}

// WithHost points the gateway at another base URL (tests).
func (g *AirwallexGateway) WithHost(host string) *AirwallexGateway { g.host = host; return g }

// bearer returns a cached login token, refreshing it two minutes before expiry (Airwallex tokens
// last 30 minutes).
func (g *AirwallexGateway) bearer(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token != "" && g.now().Add(2*time.Minute).Before(g.tokenExp) {
		return g.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.host+"/api/v1/authentication/login", bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-client-id", g.clientID)
	req.Header.Set("x-api-key", g.apiKey)
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := g.send(req, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", errors.New("airwallex: login returned no token")
	}
	exp := g.now().Add(25 * time.Minute)
	if t, err := time.Parse("2006-01-02T15:04:05-0700", out.ExpiresAt); err == nil {
		exp = t
	}
	g.token, g.tokenExp = out.Token, exp
	return g.token, nil
}

// call performs an authenticated JSON request; a non-2xx body is returned as *AirwallexError.
func (g *AirwallexGateway) call(ctx context.Context, method, path string, body any, out any) error {
	tok, err := g.bearer(ctx)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.host+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return g.send(req, out)
}

func (g *AirwallexGateway) send(req *http.Request, out any) error {
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e := &AirwallexError{HTTPStatus: resp.StatusCode}
		var body struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &body) == nil {
			e.Code, e.Message = body.Code, body.Message
		}
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
		}
		return e
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// --- wire shapes ------------------------------------------------------------------------------

type awxCard struct {
	Last4       string `json:"last4"`
	ExpiryMonth string `json:"expiry_month"`
	ExpiryYear  string `json:"expiry_year"`
	Brand       string `json:"brand"`
}

type awxPaymentMethod struct {
	ID   string   `json:"id"`
	Type string   `json:"type"`
	Card *awxCard `json:"card"`
}

type awxAttempt struct {
	ID            string            `json:"id"`
	Status        string            `json:"status"`
	PaymentMethod *awxPaymentMethod `json:"payment_method"`
	FailureCode   string            `json:"failure_code"`
	FailureReason string            `json:"failure_reason"`
}

type awxIntent struct {
	ID                   string          `json:"id"`
	Status               string          `json:"status"`
	ClientSecret         string          `json:"client_secret"`
	Amount               decimal.Decimal `json:"amount"`
	CapturedAmount       decimal.Decimal `json:"captured_amount"`
	Currency             string          `json:"currency"`
	CustomerID           string          `json:"customer_id"`
	PaymentConsentID     string          `json:"payment_consent_id"`
	CancellationReason   string          `json:"cancellation_reason"`
	LatestPaymentAttempt *awxAttempt     `json:"latest_payment_attempt"`
}

type awxConsent struct {
	ID            string            `json:"id"`
	CustomerID    string            `json:"customer_id"`
	Status        string            `json:"status"`
	PaymentMethod *awxPaymentMethod `json:"payment_method"`
}

// amountOf renders integer cents as the decimal major-unit amount Airwallex expects.
func amountOf(cents int64) decimal.Decimal { return decimal.New(cents, -2) }

// wireAmount is amountOf as a bare JSON number (shopspring/decimal marshals to a quoted string by
// default, and Airwallex validates `amount` as a number).
func wireAmount(cents int64) json.Number { return json.Number(amountOf(cents).String()) }

// descriptor trims a statement descriptor to Airwallex's 32-character limit.
func descriptor(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 32 {
		return s[:32]
	}
	return s
}

// GetOrCreateCustomer looks the customer up by merchant_customer_id (= the billing profile id),
// else creates it with a deterministic request_id so a retried create is idempotent.
func (g *AirwallexGateway) GetOrCreateCustomer(ctx context.Context, in CustomerInput) (string, error) {
	bp := strings.TrimSpace(in.BillingProfileID)
	if bp == "" {
		return "", fmt.Errorf("airwallex: refusing to look up a customer with an empty billingProfileId")
	}
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := g.call(ctx, http.MethodGet, "/api/v1/pa/customers?merchant_customer_id="+url.QueryEscape(bp), nil, &list); err != nil {
		return "", err
	}
	if len(list.Items) > 0 {
		return list.Items[0].ID, nil
	}
	first, last := splitName(in.Name)
	body := map[string]any{
		"request_id":           "proxcloud-customer-" + bp,
		"merchant_customer_id": bp,
		"email":                in.Email,
		"first_name":           first,
		"last_name":            last,
		"metadata":             map[string]any{"billingProfileId": bp},
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := g.call(ctx, http.MethodPost, "/api/v1/pa/customers/create", body, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func splitName(name string) (string, string) {
	name = strings.TrimSpace(name)
	if i := strings.LastIndex(name, " "); i > 0 {
		return strings.TrimSpace(name[:i]), strings.TrimSpace(name[i+1:])
	}
	return name, ""
}

func (g *AirwallexGateway) createIntent(ctx context.Context, customerID string, cents int64, currency, desc string, meta map[string]any, requestID string) (awxIntent, error) {
	rid := requestID
	if rid == "" {
		rid = uuid.NewString()
	}
	body := map[string]any{
		"request_id":        rid,
		"amount":            wireAmount(cents),
		"currency":          strings.ToUpper(currency),
		"merchant_order_id": rid,
		"descriptor":        descriptor(desc),
	}
	if customerID != "" {
		body["customer_id"] = customerID
	}
	if meta != nil {
		body["metadata"] = meta
	}
	var out awxIntent
	err := g.call(ctx, http.MethodPost, "/api/v1/pa/payment_intents/create", body, &out)
	return out, err
}

// CreatePaymentIntent creates the add-funds intent; the browser confirms it with the card element.
func (g *AirwallexGateway) CreatePaymentIntent(ctx context.Context, in PaymentIntentInput) (PaymentIntentResult, error) {
	pi, err := g.createIntent(ctx, in.CustomerID, in.AmountCents, in.Currency, in.Description, map[string]any{"purpose": "add_funds"}, "")
	if err != nil {
		return PaymentIntentResult{}, err
	}
	return PaymentIntentResult{ID: pi.ID, ClientSecret: pi.ClientSecret, Status: awxStatus(pi.Status)}, nil
}

// RetrievePaymentIntent re-reads an intent (the confirm callback / the stuck-transaction scanner).
func (g *AirwallexGateway) RetrievePaymentIntent(ctx context.Context, id string) (PaymentIntentResult, error) {
	var pi awxIntent
	if err := g.call(ctx, http.MethodGet, "/api/v1/pa/payment_intents/"+url.PathEscape(id), nil, &pi); err != nil {
		return PaymentIntentResult{}, err
	}
	return intentResult(pi), nil
}

func intentResult(pi awxIntent) PaymentIntentResult {
	res := PaymentIntentResult{ID: pi.ID, ClientSecret: pi.ClientSecret, Status: awxStatus(pi.Status), CancellationReason: pi.CancellationReason}
	if a := pi.LatestPaymentAttempt; a != nil && (a.FailureReason != "" || a.FailureCode != "") {
		res.ErrorMessage, res.ErrorCode = a.FailureReason, a.FailureCode
	}
	if strings.EqualFold(pi.Status, "REQUIRES_PAYMENT_METHOD") { // not "pending": no attempt is running, see chargeDidNotSucceed
		res.Status = "requires_payment_method"
	}
	return res
}

// awxStatus normalizes an Airwallex intent status: SUCCEEDED → succeeded, CANCELLED → canceled,
// REQUIRES_CAPTURE → requires_capture, everything else (REQUIRES_PAYMENT_METHOD — also the state
// after a declined browser attempt, REQUIRES_CUSTOMER_ACTION, PENDING) → pending.
func awxStatus(s string) string {
	switch strings.ToUpper(s) {
	case "SUCCEEDED":
		return "succeeded"
	case "CANCELLED":
		return "canceled"
	case "REQUIRES_CAPTURE":
		return "requires_capture"
	}
	return "pending"
}

func (g *AirwallexGateway) consent(ctx context.Context, id string) (awxConsent, error) {
	var c awxConsent
	err := g.call(ctx, http.MethodGet, "/api/v1/pa/payment_consents/"+url.PathEscape(id), nil, &c)
	return c, err
}

// CollectPaymentIntent charges a saved consent off-session: an intent for the consent's customer
// (request_id derived from the collect transaction, so a retried create returns the same intent),
// confirmed server-side with payment_consent_reference. A decline comes back as a 4xx and is
// reported as a failed result (message + code); auth / rate-limit / validation / 5xx responses are
// returned as errors on a known intent so the caller keeps the transaction PENDING for the scanner.
func (g *AirwallexGateway) CollectPaymentIntent(ctx context.Context, in CollectInput) (PaymentIntentResult, error) {
	c, err := g.consent(ctx, in.CardTokenID)
	if err != nil {
		return PaymentIntentResult{}, err
	}
	createID, confirmID := "", ""
	if in.Reference != "" {
		createID, confirmID = "collect-"+in.Reference, "collect-confirm-"+in.Reference
	}
	pi, err := g.createIntent(ctx, c.CustomerID, in.AmountCents, in.Currency, "ProxCloud bill", map[string]any{"purpose": "collect"}, createID)
	if err != nil {
		return PaymentIntentResult{}, err
	}
	if confirmID == "" {
		confirmID = uuid.NewString()
	}
	// payment_consent_id (top-level) is what the current API accepts for a merchant-initiated
	// confirm; the older payment_consent_reference shape is rejected with a validation_error.
	body := map[string]any{
		"request_id":         confirmID,
		"customer_id":        c.CustomerID,
		"payment_consent_id": c.ID,
	}
	var confirmed awxIntent
	if err := g.call(ctx, http.MethodPost, "/api/v1/pa/payment_intents/"+url.PathEscape(pi.ID)+"/confirm", body, &confirmed); err != nil {
		var ae *AirwallexError
		if errors.As(err, &ae) && ae.isDecline() {
			return PaymentIntentResult{ID: pi.ID, Status: "failed", ErrorMessage: ae.Message, ErrorCode: ae.Code}, nil
		}
		return PaymentIntentResult{ID: pi.ID, Status: "pending"}, err
	}
	res := intentResult(confirmed)
	// The confirm completed, so the outcome is known: anything but SUCCEEDED (or an authorization
	// awaiting capture) means the merchant-initiated charge did not go through — the issuer wants
	// the cardholder (3-D Secure) or rejected the consent. Only PENDING stays pending.
	switch strings.ToUpper(confirmed.Status) {
	case "SUCCEEDED", "REQUIRES_CAPTURE", "PENDING":
	default:
		res.Status = "failed"
		if res.ErrorMessage == "" {
			res.ErrorMessage = "The card issuer did not accept the charge (" + strings.ToLower(confirmed.Status) + ")"
			if a := confirmed.LatestPaymentAttempt; a != nil && a.Status != "" {
				res.ErrorMessage = "The card issuer did not accept the charge (" + strings.ToLower(a.Status) + ")"
			}
		}
	}
	return res, nil
}

// isDecline reports whether a 4xx from /confirm is the card being refused (a terminal decline
// the customer must act on) rather than a problem on our side (credentials, rate limit, a request
// Airwallex could not validate, a vanished resource) — those are surfaced as errors.
func (e *AirwallexError) isDecline() bool {
	if e.HTTPStatus >= 500 || e.HTTPStatus == 401 || e.HTTPStatus == 403 || e.HTTPStatus == 429 {
		return false
	}
	switch e.Code {
	case "validation_error", "resource_not_found", "unauthorized", "credentials_invalid", "token_expired", "request_id_duplicate", "not_found":
		return false
	}
	return true
}

// Refund refunds the full intent. Airwallex processes refunds asynchronously (RECEIVED → ACCEPTED
// → SUCCEEDED | FAILED): the call waits a few seconds for a terminal state and otherwise reports
// the refund as `pending` with its id, which the stuck-transaction scanner keeps polling
// (RetrieveRefund) until it settles or fails.
func (g *AirwallexGateway) Refund(ctx context.Context, paymentIntentID string) (RefundResult, error) {
	body := map[string]any{"request_id": uuid.NewString(), "payment_intent_id": paymentIntentID, "reason": "Refund requested by the operator"}
	var out awxRefund
	if err := g.call(ctx, http.MethodPost, "/api/v1/pa/refunds/create", body, &out); err != nil {
		return RefundResult{}, err
	}
	for i := 0; i < 5 && out.ID != "" && !isTerminalRefund(out.Status); i++ {
		if !g.sleep(ctx, time.Second) {
			break
		}
		cur, err := g.refund(ctx, out.ID)
		if err != nil {
			break
		}
		out = cur
	}
	return out.result(), nil
}

// RetrieveRefund re-reads a refund by id.
func (g *AirwallexGateway) RetrieveRefund(ctx context.Context, refundID string) (RefundResult, error) {
	rf, err := g.refund(ctx, refundID)
	if err != nil {
		return RefundResult{}, err
	}
	return rf.result(), nil
}

type awxRefund struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	FailureReason string `json:"failure_reason"`
}

// result normalizes an Airwallex refund: RECEIVED / ACCEPTED are queued (pending), SUCCEEDED is
// the refund being paid out and SETTLED the money having left (both succeeded — the sandbox
// reports SETTLED about 25 minutes after acceptance), FAILED is failed.
func (r awxRefund) result() RefundResult {
	res := RefundResult{ID: r.ID, Status: "pending", ErrorMessage: r.FailureReason}
	switch strings.ToUpper(r.Status) {
	case "SUCCEEDED", "SETTLED":
		res.Status = "succeeded"
	case "FAILED":
		res.Status = "failed"
	}
	return res
}

func (g *AirwallexGateway) refund(ctx context.Context, id string) (awxRefund, error) {
	var rf awxRefund
	err := g.call(ctx, http.MethodGet, "/api/v1/pa/refunds/"+url.PathEscape(id), nil, &rf)
	return rf, err
}

func isTerminalRefund(status string) bool {
	switch strings.ToUpper(status) {
	case "SUCCEEDED", "SETTLED", "FAILED":
		return true
	}
	return false
}

// sleep waits d unless the context ends first; tests shorten it through pollInterval.
func (g *AirwallexGateway) sleep(ctx context.Context, d time.Duration) bool {
	if g.pollInterval > 0 {
		d = g.pollInterval
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// CreateSetupIntent creates the 1.00 verification intent for the customer. The browser confirms
// it with the card element, auto_capture=false and payment_consent{next_triggered_by: merchant},
// which authorizes the hold and creates the reusable consent in one step.
func (g *AirwallexGateway) CreateSetupIntent(ctx context.Context, in SetupIntentInput) (SetupIntentResult, error) {
	pi, err := g.createIntent(ctx, in.CustomerID, in.AmountCents, in.Currency, in.Description, map[string]any{"purpose": "card_verification"}, "")
	if err != nil {
		return SetupIntentResult{}, err
	}
	return SetupIntentResult{ID: pi.ID, ClientSecret: pi.ClientSecret, Status: awxStatus(pi.Status)}, nil
}

// RetrieveSetupIntent reads the verification intent: authorized (REQUIRES_CAPTURE, or SUCCEEDED
// if captured) with a consent attached → succeeded + the consent's card.
func (g *AirwallexGateway) RetrieveSetupIntent(ctx context.Context, id string) (SetupIntentResult, error) {
	var pi awxIntent
	if err := g.call(ctx, http.MethodGet, "/api/v1/pa/payment_intents/"+url.PathEscape(id), nil, &pi); err != nil {
		return SetupIntentResult{}, err
	}
	res := SetupIntentResult{ID: pi.ID, ClientSecret: pi.ClientSecret, Status: awxStatus(pi.Status)}
	if a := pi.LatestPaymentAttempt; a != nil && (a.FailureReason != "" || a.FailureCode != "") {
		res.ErrorMessage = gatewayErrorMessage(a.FailureReason, a.FailureCode)
	}
	switch res.Status {
	case "requires_capture", "succeeded":
		if pi.PaymentConsentID == "" {
			// Authorized but no reusable consent was created: the card cannot be charged later, so
			// the registration fails and the caller releases the hold.
			res.Status = "failed"
			res.ErrorMessage = "the card was verified but could not be saved for future payments"
			return res, nil
		}
		c, err := g.consent(ctx, pi.PaymentConsentID)
		if err != nil {
			return res, err
		}
		card := CardDetails{TokenID: c.ID, PanMasked: "*.????"}
		if c.PaymentMethod != nil && c.PaymentMethod.Card != nil {
			card.PanMasked = "*." + c.PaymentMethod.Card.Last4
			card.ExpMonth = atoi(c.PaymentMethod.Card.ExpiryMonth)
			card.ExpYear = atoi(c.PaymentMethod.Card.ExpiryYear)
		} else if a := pi.LatestPaymentAttempt; a != nil && a.PaymentMethod != nil && a.PaymentMethod.Card != nil {
			card.PanMasked = "*." + a.PaymentMethod.Card.Last4
			card.ExpMonth = atoi(a.PaymentMethod.Card.ExpiryMonth)
			card.ExpYear = atoi(a.PaymentMethod.Card.ExpiryYear)
		}
		res.Status = "succeeded"
		res.Card = card
	}
	return res, nil
}

func atoi(s string) int64 {
	var n int64
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

// ReleaseSetupIntent voids the verification hold (cancel an authorized-only intent; refund a
// captured one).
func (g *AirwallexGateway) ReleaseSetupIntent(ctx context.Context, id string) error {
	var pi awxIntent
	if err := g.call(ctx, http.MethodGet, "/api/v1/pa/payment_intents/"+url.PathEscape(id), nil, &pi); err != nil {
		return err
	}
	switch awxStatus(pi.Status) {
	case "requires_capture":
		body := map[string]any{"request_id": uuid.NewString(), "cancellation_reason": "Card verification hold released"}
		return g.call(ctx, http.MethodPost, "/api/v1/pa/payment_intents/"+url.PathEscape(id)+"/cancel", body, nil)
	case "succeeded":
		_, err := g.Refund(ctx, id)
		return err
	}
	return nil
}

// Ping verifies the API key by logging in.
func (g *AirwallexGateway) Ping(ctx context.Context) error {
	g.mu.Lock()
	g.token, g.tokenExp = "", time.Time{}
	g.mu.Unlock()
	_, err := g.bearer(ctx)
	return err
}

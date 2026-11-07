package payment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAirwallex is an httptest stand-in for the Airwallex Payment Acceptance API: enough of
// login / customers / payment intents / consents / refunds to drive every Gateway method.
type fakeAirwallex struct {
	t           *testing.T
	logins      atomic.Int32
	customers   map[string]string // merchant_customer_id → customer id
	intents     map[string]map[string]any
	consents    map[string]map[string]any
	declineNext bool   // decline the next confirm (card_declined)
	confirmAs   string // when set, the next confirm answers with this intent status instead of SUCCEEDED
	rateLimit   bool   // when set, the next confirm answers 429
	byRequest   map[string]string
	seq         int
	refundPolls int
	refundFinal string // terminal status the refund poll reports (default SUCCEEDED)
	requests    []string
}

func newFakeAirwallex(t *testing.T) (*fakeAirwallex, *httptest.Server) {
	f := &fakeAirwallex{t: t, customers: map[string]string{}, intents: map[string]map[string]any{}, consents: map[string]map[string]any{}, byRequest: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeAirwallex) handle(w http.ResponseWriter, r *http.Request) {
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	writeJSON := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	p := r.URL.Path
	switch {
	case p == "/api/v1/authentication/login":
		if r.Header.Get("x-client-id") != "cid" || r.Header.Get("x-api-key") != "key" {
			writeJSON(401, map[string]any{"code": "credentials_invalid", "message": "UNAUTHORIZED"})
			return
		}
		f.logins.Add(1)
		writeJSON(201, map[string]any{"token": "tok-1", "expires_at": time.Now().UTC().Add(30 * time.Minute).Format("2006-01-02T15:04:05-0700")})
		return
	}
	if r.Header.Get("Authorization") != "Bearer tok-1" {
		writeJSON(401, map[string]any{"code": "unauthorized", "message": "no token"})
		return
	}
	switch {
	case p == "/api/v1/pa/customers" && r.Method == http.MethodGet:
		items := []any{}
		if id, ok := f.customers[r.URL.Query().Get("merchant_customer_id")]; ok {
			items = append(items, map[string]any{"id": id})
		}
		writeJSON(200, map[string]any{"items": items, "has_more": false})
	case p == "/api/v1/pa/customers/create":
		id := "cus_" + body["merchant_customer_id"].(string)
		f.customers[body["merchant_customer_id"].(string)] = id
		writeJSON(201, map[string]any{"id": id, "client_secret": "cs_customer"})
	case p == "/api/v1/pa/payment_intents/create":
		if existing, ok := f.byRequest[body["request_id"].(string)]; ok { // idempotent create
			writeJSON(201, f.intents[existing])
			return
		}
		f.seq++
		id := "int_" + strconv.Itoa(f.seq)
		f.byRequest[body["request_id"].(string)] = id
		pi := map[string]any{"id": id, "status": "REQUIRES_PAYMENT_METHOD", "client_secret": "cs_" + id,
			"amount": body["amount"], "currency": body["currency"], "customer_id": body["customer_id"], "captured_amount": 0}
		f.intents[id] = pi
		writeJSON(201, pi)
	case strings.HasPrefix(p, "/api/v1/pa/payment_intents/") && strings.HasSuffix(p, "/confirm"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/api/v1/pa/payment_intents/"), "/confirm")
		pi, ok := f.intents[id]
		if !ok {
			writeJSON(404, map[string]any{"code": "resource_not_found", "message": "intent not found"})
			return
		}
		if f.declineNext {
			f.declineNext = false
			writeJSON(400, map[string]any{"code": "card_declined", "message": "The card was declined"})
			return
		}
		if f.rateLimit {
			f.rateLimit = false
			writeJSON(429, map[string]any{"code": "too_many_requests", "message": "slow down"})
			return
		}
		cid, _ := body["payment_consent_id"].(string)
		if cid == "" || f.consents[cid] == nil {
			writeJSON(400, map[string]any{"code": "validation_error", "message": "Either payment_method or payment_consent_id should be provided"})
			return
		}
		ref := map[string]any{"id": cid}
		pi["status"] = "SUCCEEDED"
		if f.confirmAs != "" {
			pi["status"] = f.confirmAs
			f.confirmAs = ""
		}
		pi["payment_consent_id"] = ref["id"]
		pi["latest_payment_attempt"] = map[string]any{"status": "SETTLED"}
		writeJSON(200, pi)
	case strings.HasPrefix(p, "/api/v1/pa/payment_intents/") && strings.HasSuffix(p, "/cancel"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/api/v1/pa/payment_intents/"), "/cancel")
		pi := f.intents[id]
		if pi["status"] != "REQUIRES_CAPTURE" {
			writeJSON(400, map[string]any{"code": "validation_error", "message": "cannot cancel"})
			return
		}
		pi["status"] = "CANCELLED"
		pi["cancellation_reason"] = body["cancellation_reason"]
		writeJSON(200, pi)
	case strings.HasPrefix(p, "/api/v1/pa/payment_intents/"):
		id := strings.TrimPrefix(p, "/api/v1/pa/payment_intents/")
		pi, ok := f.intents[id]
		if !ok {
			writeJSON(404, map[string]any{"code": "resource_not_found", "message": "intent not found"})
			return
		}
		writeJSON(200, pi)
	case strings.HasPrefix(p, "/api/v1/pa/payment_consents/"):
		id := strings.TrimPrefix(p, "/api/v1/pa/payment_consents/")
		c, ok := f.consents[id]
		if !ok {
			writeJSON(404, map[string]any{"code": "resource_not_found", "message": "consent not found"})
			return
		}
		writeJSON(200, c)
	case p == "/api/v1/pa/refunds/create":
		f.refundPolls = 0
		writeJSON(201, map[string]any{"id": "rfd_1", "status": "RECEIVED", "payment_intent_id": body["payment_intent_id"]})
	case strings.HasPrefix(p, "/api/v1/pa/refunds/"):
		// still queued on the first read, then whatever the test scripted (default SUCCEEDED).
		f.refundPolls++
		st := "ACCEPTED"
		if f.refundPolls > 1 {
			st = "SETTLED" // the sandbox's terminal state for a paid-out refund
			if f.refundFinal != "" {
				st = f.refundFinal
			}
		}
		writeJSON(200, map[string]any{"id": "rfd_1", "status": st, "failure_reason": map[bool]string{true: "insufficient balance", false: ""}[st == "FAILED"]})
	default:
		writeJSON(404, map[string]any{"code": "not_found", "message": p})
	}
}

// browserVerifies simulates the console confirming a verification intent with the card element:
// the hold is authorized (REQUIRES_CAPTURE) and a merchant-triggered consent is attached.
func (f *fakeAirwallex) browserVerifies(intentID string) string {
	cst := "cst_" + intentID
	f.consents[cst] = map[string]any{"id": cst, "customer_id": f.intents[intentID]["customer_id"], "status": "VERIFIED",
		"payment_method": map[string]any{"type": "card", "card": map[string]any{"last4": "0008", "expiry_month": "12", "expiry_year": "2030", "brand": "visa"}}}
	f.intents[intentID]["status"] = "REQUIRES_CAPTURE"
	f.intents[intentID]["payment_consent_id"] = cst
	return cst
}

// An intent nobody is trying to charge must read differently from one being charged: only the first can be closed by hand.
func TestAirwallexIntentResultKeepsNoAttemptApart(t *testing.T) {
	if got := intentResult(awxIntent{Status: "REQUIRES_PAYMENT_METHOD"}).Status; !chargeDidNotSucceed(got) {
		t.Errorf("REQUIRES_PAYMENT_METHOD reads %q", got)
	}
	for _, s := range []string{"PENDING", "REQUIRES_CUSTOMER_ACTION", "REQUIRES_CAPTURE"} {
		if got := intentResult(awxIntent{Status: s}).Status; chargeDidNotSucceed(got) {
			t.Errorf("%s reads %q, which could be closed", s, got)
		}
	}
}

func TestAirwallexStatusMapping(t *testing.T) {
	for in, want := range map[string]string{"SUCCEEDED": "succeeded", "CANCELLED": "canceled", "REQUIRES_CAPTURE": "requires_capture",
		"REQUIRES_PAYMENT_METHOD": "pending", "REQUIRES_CUSTOMER_ACTION": "pending", "PENDING": "pending", "": "pending"} {
		if got := awxStatus(in); got != want {
			t.Errorf("awxStatus(%q) = %q, want %q", in, got, want)
		}
	}
	// the services' mapStatus sees the normalized words: requires_capture is NOT a terminal state.
	if mapStatus("requires_capture") != "PENDING" || mapStatus("succeeded") != "SUCCESS" {
		t.Fatal("normalized statuses must flow through mapStatus")
	}
	if amountOf(1234).String() != "12.34" || amountOf(100).String() != "1" {
		t.Fatalf("amountOf: %s / %s", amountOf(1234), amountOf(100))
	}
}

func TestAirwallexGatewayFlows(t *testing.T) {
	f, srv := newFakeAirwallex(t)
	g := NewAirwallexGateway("cid", "key", true).WithHost(srv.URL)
	ctx := context.Background()

	// customer: created once, then found by merchant_customer_id.
	cust, err := g.GetOrCreateCustomer(ctx, CustomerInput{BillingProfileID: "bp1", Email: "a@b.c", Name: "Ada Lovelace"})
	if err != nil || cust != "cus_bp1" {
		t.Fatalf("create customer: %s %v", cust, err)
	}
	if again, _ := g.GetOrCreateCustomer(ctx, CustomerInput{BillingProfileID: "bp1"}); again != cust {
		t.Fatalf("customer lookup = %s, want %s", again, cust)
	}
	if _, err := g.GetOrCreateCustomer(ctx, CustomerInput{}); err == nil {
		t.Fatal("empty billing profile id must be refused")
	}

	// add-funds intent: amount in major units, client secret returned, pending.
	pi, err := g.CreatePaymentIntent(ctx, PaymentIntentInput{CustomerID: cust, AmountCents: 1234, Currency: "usd", Description: "Add funds"})
	if err != nil || pi.ID == "" || pi.ClientSecret == "" || pi.Status != "pending" {
		t.Fatalf("create PI: %+v %v", pi, err)
	}
	if got := f.intents[pi.ID]["amount"]; got != 12.34 || f.intents[pi.ID]["currency"] != "USD" {
		t.Fatalf("intent amount/currency on the wire: %v %v", got, f.intents[pi.ID]["currency"])
	}
	if r, _ := g.RetrievePaymentIntent(ctx, pi.ID); r.Status != "requires_payment_method" {
		t.Fatalf("retrieve pending: %+v", r)
	}
	f.intents[pi.ID]["status"] = "SUCCEEDED"
	if r, _ := g.RetrievePaymentIntent(ctx, pi.ID); r.Status != "succeeded" {
		t.Fatalf("retrieve succeeded: %+v", r)
	}

	// card verification: 1.00 hold → browser authorizes + consent → retrieve yields the card → release cancels.
	si, err := g.CreateSetupIntent(ctx, SetupIntentInput{CustomerID: cust, Description: "Card verification", AmountCents: 100, Currency: "USD"})
	if err != nil || si.ClientSecret == "" {
		t.Fatalf("setup intent: %+v %v", si, err)
	}
	if r, _ := g.RetrieveSetupIntent(ctx, si.ID); r.Status != "pending" {
		t.Fatalf("verification before confirm: %+v", r)
	}
	cst := f.browserVerifies(si.ID)
	verified, err := g.RetrieveSetupIntent(ctx, si.ID)
	if err != nil || verified.Status != "succeeded" || verified.Card.TokenID != cst || verified.Card.PanMasked != "*.0008" || verified.Card.ExpMonth != 12 || verified.Card.ExpYear != 2030 {
		t.Fatalf("verification after confirm: %+v %v", verified, err)
	}
	if err := g.ReleaseSetupIntent(ctx, si.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if f.intents[si.ID]["status"] != "CANCELLED" {
		t.Fatalf("hold not cancelled: %v", f.intents[si.ID]["status"])
	}
	if err := g.ReleaseSetupIntent(ctx, si.ID); err != nil { // idempotent on a cancelled intent
		t.Fatalf("second release: %v", err)
	}

	// collect on the consent: a new intent for the consent's customer, confirmed merchant-initiated.
	col, err := g.CollectPaymentIntent(ctx, CollectInput{CardTokenID: cst, AmountCents: 2000, Currency: "USD"})
	if err != nil || col.Status != "succeeded" || col.ID == "" {
		t.Fatalf("collect: %+v %v", col, err)
	}
	if f.intents[col.ID]["customer_id"] != cust {
		t.Fatalf("collect intent customer = %v", f.intents[col.ID]["customer_id"])
	}

	// a declined confirm is a FAILED result with the gateway's message, not an error.
	f.declineNext = true
	dec, err := g.CollectPaymentIntent(ctx, CollectInput{CardTokenID: cst, AmountCents: 500, Currency: "USD"})
	if err != nil || dec.Status != "failed" || dec.ErrorMessage != "The card was declined" || dec.ErrorCode != "card_declined" || dec.ID == "" {
		t.Fatalf("declined collect: %+v %v", dec, err)
	}
	// an unknown consent is a transport-level error (nothing was charged, nothing to mark failed).
	if _, err := g.CollectPaymentIntent(ctx, CollectInput{CardTokenID: "cst_missing", AmountCents: 500, Currency: "USD"}); err == nil {
		t.Fatal("unknown consent must error")
	}

	// a confirm that completes with REQUIRES_CUSTOMER_ACTION (issuer wants 3-D Secure) is a failed
	// merchant-initiated charge, not a pending one.
	f.confirmAs = "REQUIRES_CUSTOMER_ACTION"
	act, err := g.CollectPaymentIntent(ctx, CollectInput{CardTokenID: cst, AmountCents: 700, Currency: "USD", Reference: "txn-3ds"})
	if err != nil || act.Status != "failed" || !strings.Contains(act.ErrorMessage, "did not accept") {
		t.Fatalf("3ds collect: %+v %v", act, err)
	}
	// a rate limit on the confirm is an error on a known intent (the caller keeps it PENDING).
	f.rateLimit = true
	rl, err := g.CollectPaymentIntent(ctx, CollectInput{CardTokenID: cst, AmountCents: 700, Currency: "USD", Reference: "txn-429"})
	if err == nil || rl.ID == "" || rl.Status != "pending" {
		t.Fatalf("rate-limited collect: %+v %v", rl, err)
	}
	// the create is idempotent on the collect transaction id: a retry reuses the same intent.
	again, _ := g.CollectPaymentIntent(ctx, CollectInput{CardTokenID: cst, AmountCents: 700, Currency: "USD", Reference: "txn-429"})
	if again.ID != rl.ID {
		t.Fatalf("retried collect created a new intent: %s vs %s", again.ID, rl.ID)
	}

	// refund: queued (RECEIVED/ACCEPTED) is polled until it settles; here it SUCCEEDs on the second read.
	g.pollInterval = time.Millisecond
	rf, err := g.Refund(ctx, col.ID)
	if err != nil || rf.Status != "succeeded" || rf.ID != "rfd_1" {
		t.Fatalf("refund: %+v %v", rf, err)
	}
	// a refund the gateway fails after accepting it is reported failed with the reason …
	f.refundFinal = "FAILED"
	if rf, err := g.Refund(ctx, col.ID); err != nil || rf.Status != "failed" || rf.ErrorMessage != "insufficient balance" {
		t.Fatalf("failed refund: %+v %v", rf, err)
	}
	// … and one still queued after the poll budget stays pending (the scanner re-reads it later).
	f.refundFinal = "ACCEPTED"
	if rf, err := g.Refund(ctx, col.ID); err != nil || rf.Status != "pending" || rf.ID != "rfd_1" {
		t.Fatalf("pending refund: %+v %v", rf, err)
	}
	if again, err := g.RetrieveRefund(ctx, "rfd_1"); err != nil || again.Status != "pending" {
		t.Fatalf("retrieve pending refund: %+v %v", again, err)
	}

	// token cache: one login served the whole conversation; Ping forces a fresh login.
	if n := f.logins.Load(); n != 1 {
		t.Fatalf("logins = %d, want 1 (token must be cached)", n)
	}
	if err := g.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if n := f.logins.Load(); n != 2 {
		t.Fatalf("logins after ping = %d, want 2", n)
	}
	bad := NewAirwallexGateway("cid", "wrong", true).WithHost(srv.URL)
	if err := bad.Ping(ctx); err == nil || !strings.Contains(err.Error(), "UNAUTHORIZED") {
		t.Fatalf("bad credentials must fail the health check: %v", err)
	}
}

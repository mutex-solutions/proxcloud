//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/payment"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// awxServer is a minimal httptest Airwallex Payment Acceptance API: login (credential check),
// customers, payment-intent create / retrieve. The intent status is flipped by the test.
type awxServer struct {
	mu      sync.Mutex
	status  string // the status retrieve reports
	created int
	amount  string // the amount the create call carried
}

func (a *awxServer) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	write := func(code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case r.URL.Path == "/api/v1/authentication/login":
		if r.Header.Get("x-client-id") != "cid" || r.Header.Get("x-api-key") != "k" {
			write(401, map[string]any{"code": "credentials_invalid", "message": "UNAUTHORIZED"})
			return
		}
		write(201, map[string]any{"token": "tok", "expires_at": "2099-01-01T00:00:00+0000"})
	case r.Header.Get("Authorization") != "Bearer tok":
		write(401, map[string]any{"code": "unauthorized", "message": "no bearer"})
	case r.URL.Path == "/api/v1/pa/customers" && r.Method == http.MethodGet:
		write(200, map[string]any{"items": []any{}})
	case r.URL.Path == "/api/v1/pa/customers/create":
		write(201, map[string]any{"id": "cus_1"})
	case r.URL.Path == "/api/v1/pa/payment_intents/create":
		a.created++
		a.amount = string(mustJSON(body["amount"]))
		write(201, map[string]any{"id": "int_1", "status": "REQUIRES_PAYMENT_METHOD", "client_secret": "cs_1", "currency": "USD"})
	case r.URL.Path == "/api/v1/pa/payment_intents/int_1":
		write(200, map[string]any{"id": "int_1", "status": a.status, "currency": "USD"})
	default:
		write(404, map[string]any{"code": "not_found", "message": r.URL.Path})
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func (a *awxServer) set(status string) { a.mu.Lock(); a.status = status; a.mu.Unlock() }

// TestAirwallexDeposit drives the real AirwallexGateway (HTTP against the fake) through the
// add-funds flow: a wrong API key is refused before any transaction exists, an unpaid intent
// credits nothing, and once the gateway reports SUCCEEDED any number of concurrent confirm
// callbacks credit exactly once. The callback carries no gateway data of its own: the amount and
// the outcome always come from re-reading the intent, so a forged redirect cannot credit.
func TestAirwallexDeposit(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)
	profile, _ := seedPostpaid(t, db, billing.PaymentModePrepaid) // seeds gateway "gw-awx" (cid / k)
	srv := &awxServer{status: "REQUIRES_PAYMENT_METHOD"}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	t.Cleanup(ts.Close)

	factory := func(gw *billing.Gateway) payment.Gateway {
		if gw == nil || gw.ThirdParty != billing.ThirdPartyAirwallex {
			return nil
		}
		return payment.NewAirwallexGateway(gw.ConfigString("clientId"), gw.SecretString("apiKey"), gw.Sandbox()).WithHost(ts.URL)
	}
	svc := payment.NewAddFundsService(repo, pricingRepo, factory)
	ten := decimal.NewFromInt(10)

	// wrong API key on the integration: the deposit is refused and nothing is credited.
	if _, err := db.C("thirdPartyIntegration").SetByID(ctx, "gw-awx", pgdoc.M{"secret": pgdoc.M{"apiKey": "wrong"}}, nil); err != nil {
		t.Fatalf("break key: %v", err)
	}
	if _, err := svc.AddFunds(ctx, profile, payment.AddFundsRequest{Amount: &ten, PaymentGatewayID: "gw-awx"}); err == nil {
		t.Fatal("a rejected API key must fail the deposit")
	}
	// the PENDING row written before the gateway call carries no gateway reference and settles nothing.
	if n, _ := db.C("accountCredit").Count(ctx, pgdoc.M{"billingProfileId": profile.ID}); n != 0 {
		t.Fatalf("credits after a refused deposit = %d, want 0", n)
	}
	if n, _ := db.C("accountCreditTransaction").Count(ctx, pgdoc.M{"billingProfileId": profile.ID, "status": "SUCCESS"}); n != 0 {
		t.Fatalf("SUCCESS transactions after a refused deposit = %d, want 0", n)
	}
	if _, err := db.C("thirdPartyIntegration").SetByID(ctx, "gw-awx", pgdoc.M{"secret": pgdoc.M{"apiKey": "k"}}, nil); err != nil {
		t.Fatalf("restore key: %v", err)
	}

	resp, err := svc.AddFunds(ctx, profile, payment.AddFundsRequest{Amount: &ten, PaymentGatewayID: "gw-awx"})
	if err != nil {
		t.Fatalf("addFunds: %v", err)
	}
	if resp.ExternalPaymentID != "int_1" || resp.Metadata != "cs_1" || resp.ThirdParty != "Airwallex" {
		t.Fatalf("response: %+v", resp)
	}
	if got := strings.Trim(srv.amount, `"`); got != "10" {
		t.Fatalf("amount sent to the gateway = %s, want 10 (decimal major units, not cents)", got)
	}

	// the browser has not paid yet: confirm leaves the transaction PENDING and credits nothing.
	if txn, err := svc.ProcessAddFunds(ctx, resp.TransactionID); err != nil || txn.Status != "PENDING" {
		t.Fatalf("unpaid confirm: %+v %v", txn, err)
	}
	if n, _ := db.C("accountCredit").Count(ctx, pgdoc.M{"billingProfileId": profile.ID}); n != 0 {
		t.Fatalf("credits for an unpaid intent = %d", n)
	}

	// paid: eight concurrent callbacks (two tabs, a retry, the scanner) settle exactly once.
	srv.set("SUCCEEDED")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.ProcessAddFunds(ctx, resp.TransactionID)
		}()
	}
	wg.Wait()
	if txn, err := svc.ProcessAddFunds(ctx, resp.TransactionID); err != nil || txn.Status != "SUCCESS" {
		t.Fatalf("final confirm: %+v %v", txn, err)
	}
	if n, _ := db.C("accountCredit").Count(ctx, pgdoc.M{"billingProfileId": profile.ID}); n != 1 {
		t.Fatalf("credits after concurrent callbacks = %d, want exactly 1", n)
	}
}

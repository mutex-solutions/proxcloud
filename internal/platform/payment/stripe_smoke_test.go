package payment

import (
	"context"
	"os"
	"testing"

	stripe "github.com/stripe/stripe-go/v82"
)

// TestStripeSmoke is a LIVE smoke against the Stripe TEST sandbox: get-or-create a Customer,
// create a PaymentIntent (off_session, automatic payment methods), retrieve it. Gated on
// STRIPE_SECRET_KEY (a sk_test_* key) so normal `go test ./...` never calls Stripe.
//
//	STRIPE_SECRET_KEY=sk_test_... go test ./internal/platform/payment -run TestStripeSmoke -v
func TestStripeSmoke(t *testing.T) {
	key := os.Getenv("STRIPE_SECRET_KEY")
	if key == "" {
		t.Skip("STRIPE_SECRET_KEY not set — skipping live Stripe smoke")
	}
	g := NewStripeGateway(key)
	ctx := context.Background()

	cust, err := g.GetOrCreateCustomer(ctx, CustomerInput{
		BillingProfileID: "proxcloud-go-smoke-bp", Email: "proxcloud-go-smoke@example.com", Name: "ProxCloud Go Smoke",
		Country: "RO", City: "Bucharest", Line1: "1 Test St",
	})
	if err != nil {
		t.Fatalf("get/create customer: %v", err)
	}
	if cust == "" {
		t.Fatal("empty customer id")
	}
	t.Logf("customer %s", cust)
	// NOTE: search-by-metadata is eventually consistent on Stripe (a just-created customer is
	// not immediately indexed), so we do NOT assert an immediate second call returns the same
	// id — real idempotency is the service-layer billingProfile.customInfo customer-id cache
	// (the customer id is cached as STRIPE_CUSTOMER_ID_{integrationId}). The search
	// here is the cold-start fallback.

	pi, err := g.CreatePaymentIntent(ctx, PaymentIntentInput{
		CustomerID: cust, AmountCents: 500, Currency: "usd", Description: "Add funds to account", OffSession: true,
	})
	if err != nil {
		t.Fatalf("create payment intent: %v", err)
	}
	if pi.ID == "" || pi.ClientSecret == "" {
		t.Fatalf("PI missing id/secret: %+v", pi)
	}
	t.Logf("payment intent %s status=%s (clientSecret %d chars)", pi.ID, pi.Status, len(pi.ClientSecret))

	got, err := g.RetrievePaymentIntent(ctx, pi.ID)
	if err != nil {
		t.Fatalf("retrieve PI: %v", err)
	}
	if got.ID != pi.ID {
		t.Fatalf("retrieve mismatch: %s vs %s", got.ID, pi.ID)
	}
	t.Logf("retrieved %s status=%s", got.ID, got.Status)

	// cleanup: cancel the uncaptured PI + delete the test customer (keep the sandbox tidy).
	if _, err := g.sc.PaymentIntents.Cancel(pi.ID, nil); err != nil {
		t.Logf("cleanup cancel PI (non-fatal): %v", err)
	}
	if _, err := g.sc.Customers.Del(cust, nil); err != nil {
		t.Logf("cleanup del customer (non-fatal): %v", err)
	} else {
		t.Logf("cleaned up customer %s + PI %s", cust, pi.ID)
	}
}

// TestStripeCardSmoke is a LIVE add-card smoke vs the sandbox: customer → SetupIntent → confirm
// with a test card → retrieve (succeeded) → pull the stored card. Gated on STRIPE_SECRET_KEY.
func TestStripeCardSmoke(t *testing.T) {
	key := os.Getenv("STRIPE_SECRET_KEY")
	if key == "" {
		t.Skip("STRIPE_SECRET_KEY not set")
	}
	g := NewStripeGateway(key)
	ctx := context.Background()
	cust, err := g.GetOrCreateCustomer(ctx, CustomerInput{BillingProfileID: "proxcloud-go-card-smoke", Email: "card@example.com", Name: "Card Smoke"})
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	si, err := g.CreateSetupIntent(ctx, SetupIntentInput{CustomerID: cust, Description: "Register card"})
	if err != nil {
		t.Fatalf("create setup intent: %v", err)
	}
	t.Logf("setup intent %s status=%s", si.ID, si.Status)
	// confirm with a test card (attaches a card pm to the customer).
	if _, err := g.sc.SetupIntents.Confirm(si.ID, &stripe.SetupIntentConfirmParams{PaymentMethod: stripe.String("pm_card_visa"), ReturnURL: stripe.String("https://example.com/return")}); err != nil {
		t.Fatalf("confirm setup intent: %v", err)
	}
	got, err := g.RetrieveSetupIntent(ctx, si.ID)
	if err != nil || got.Status != "succeeded" {
		t.Fatalf("retrieve SI: %v status=%s", err, got.Status)
	}
	card, err := g.LatestCardForCustomer(ctx, cust)
	if err != nil {
		t.Fatalf("latest card: %v", err)
	}
	if card.TokenID == "" || card.PanMasked == "" {
		t.Fatalf("card details empty: %+v", card)
	}
	t.Logf("stored card token=%s pan=%s exp=%d/%d", card.TokenID, card.PanMasked, card.ExpMonth, card.ExpYear)
	if _, err := g.sc.Customers.Del(cust, nil); err == nil {
		t.Logf("cleaned customer %s", cust)
	}
}

// TestStripeCollectRefundSmoke is a LIVE collect-then-refund smoke vs the sandbox: customer →
// SetupIntent → confirm a test card (attaches a saved pm) → CollectPaymentIntent (confirm=true,
// immediate charge) → Refund that PaymentIntent. Gated on STRIPE_SECRET_KEY.
func TestStripeCollectRefundSmoke(t *testing.T) {
	key := os.Getenv("STRIPE_SECRET_KEY")
	if key == "" {
		t.Skip("STRIPE_SECRET_KEY not set")
	}
	g := NewStripeGateway(key)
	ctx := context.Background()
	cust, err := g.GetOrCreateCustomer(ctx, CustomerInput{BillingProfileID: "proxcloud-go-collect-smoke", Email: "collect@example.com", Name: "Collect Smoke"})
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	si, err := g.CreateSetupIntent(ctx, SetupIntentInput{CustomerID: cust, Description: "Register card"})
	if err != nil {
		t.Fatalf("setup intent: %v", err)
	}
	if _, err := g.sc.SetupIntents.Confirm(si.ID, &stripe.SetupIntentConfirmParams{PaymentMethod: stripe.String("pm_card_visa"), ReturnURL: stripe.String("https://example.com/return")}); err != nil {
		t.Fatalf("confirm setup intent: %v", err)
	}
	card, err := g.LatestCardForCustomer(ctx, cust)
	if err != nil {
		t.Fatalf("latest card: %v", err)
	}

	// collect: charge the saved card immediately.
	pi, err := g.CollectPaymentIntent(ctx, CollectInput{CardTokenID: card.TokenID, AmountCents: 1200, Currency: "usd"})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if pi.ID == "" || pi.Status != "succeeded" {
		t.Fatalf("collect PI not succeeded: %+v", pi)
	}
	t.Logf("collected %s status=%s", pi.ID, pi.Status)

	// refund it.
	rf, err := g.Refund(ctx, pi.ID)
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if rf.Status != "succeeded" {
		t.Fatalf("refund status=%s err=%s", rf.Status, rf.ErrorMessage)
	}
	t.Logf("refunded %s status=%s", pi.ID, rf.Status)

	if _, err := g.sc.Customers.Del(cust, nil); err == nil {
		t.Logf("cleaned customer %s", cust)
	}
}

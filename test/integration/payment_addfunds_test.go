//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/payment"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// fakeGW stands in for Stripe so the add-funds orchestration is tested against real Postgres
// without calling Stripe (the live gateway path is covered by payment.TestStripeSmoke).
type fakeGW struct {
	lastAmountCents int64
	released        []string // ReleaseSetupIntent calls (the 1.00 verification hold voids)
}

func (g *fakeGW) GetOrCreateCustomer(context.Context, payment.CustomerInput) (string, error) {
	return "cus_fake", nil
}
func (g *fakeGW) CreatePaymentIntent(_ context.Context, in payment.PaymentIntentInput) (payment.PaymentIntentResult, error) {
	g.lastAmountCents = in.AmountCents
	return payment.PaymentIntentResult{ID: "pi_fake", ClientSecret: "cs_fake", Status: "requires_payment_method"}, nil
}
func (g *fakeGW) RetrievePaymentIntent(context.Context, string) (payment.PaymentIntentResult, error) {
	return payment.PaymentIntentResult{ID: "pi_fake", Status: "succeeded"}, nil
}
func (g *fakeGW) CreateSetupIntent(context.Context, payment.SetupIntentInput) (payment.SetupIntentResult, error) {
	return payment.SetupIntentResult{ID: "seti_fake", ClientSecret: "seti_cs", Status: "requires_payment_method"}, nil
}
func (g *fakeGW) RetrieveSetupIntent(context.Context, string) (payment.SetupIntentResult, error) {
	return payment.SetupIntentResult{ID: "seti_fake", Status: "succeeded",
		Card: payment.CardDetails{TokenID: "pm_fake", PanMasked: "*.4242", ExpMonth: 12, ExpYear: 2030}}, nil
}
func (g *fakeGW) ReleaseSetupIntent(_ context.Context, id string) error {
	g.released = append(g.released, id)
	return nil
}
func (g *fakeGW) Ping(context.Context) error { return nil }
func (g *fakeGW) CollectPaymentIntent(_ context.Context, in payment.CollectInput) (payment.PaymentIntentResult, error) {
	g.lastAmountCents = in.AmountCents
	return payment.PaymentIntentResult{ID: "pi_collect", Status: "succeeded"}, nil
}
func (g *fakeGW) Refund(context.Context, string) (payment.RefundResult, error) {
	return payment.RefundResult{ID: "rf_fake", Status: "succeeded"}, nil
}
func (g *fakeGW) RetrieveRefund(context.Context, string) (payment.RefundResult, error) {
	return payment.RefundResult{ID: "rf_fake", Status: "succeeded"}, nil
}

// TestRegisterCardDispatch exercises payment.RegisterCardService against real Postgres with a fake
// gateway: PENDING CreditCardTransaction + SetupIntent, then confirm → store CreditCard + SUCCESS
// (idempotent).
func TestRegisterCardDispatch(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	const bpID = "6a37e2f547f3b4378ba78549"
	if _, err := db.C("billingProfile").InsertOne(ctx, pgdoc.M{"_id": bpID, "currency": "USD", "country": "US", "email": "a@b.c", "firstName": "Ada"}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	if _, err := db.C("thirdPartyIntegration").InsertOne(ctx, pgdoc.M{"_id": "gw1", "name": "Stripe", "thirdParty": "Stripe", "config": pgdoc.M{"publicKey": "pk_x"}, "secret": pgdoc.M{"privateKey": "sk_x"}}); err != nil {
		t.Fatalf("seed gw: %v", err)
	}
	svc := payment.NewRegisterCardService(repo, func(*billing.Gateway) payment.Gateway { return &fakeGW{} })
	profile := &billing.BillingProfile{ID: bpID, Currency: "USD", Country: "US", Email: "a@b.c", FirstName: "Ada"}

	resp, err := svc.RegisterCard(ctx, profile, payment.RegisterCardRequest{PaymentGatewayID: "gw1"})
	if err != nil {
		t.Fatalf("registerCard: %v", err)
	}
	// register-card metadata is a map {"client_secret": <secret>} (Stripe
	// Elements requires it), NOT a bare string.
	md, _ := resp.Metadata.(map[string]any)
	if resp.ExternalPaymentID != "seti_fake" || md["client_secret"] != "seti_cs" || resp.ThirdParty != "Stripe" || resp.TransactionID == "" {
		t.Fatalf("response: %+v", resp)
	}

	done, err := svc.ProcessRegisterCard(ctx, resp.TransactionID)
	if err != nil {
		t.Fatalf("processRegisterCard: %v", err)
	}
	if done.Status != "SUCCESS" {
		t.Fatalf("confirm status = %s", done.Status)
	}
	n, _ := db.C("creditCard").Count(ctx, pgdoc.M{"billingProfileId": bpID})
	if n != 1 {
		t.Fatalf("creditCard count = %d, want 1", n)
	}
	var card pgdoc.M
	_, _ = db.C("creditCard").FindOne(ctx, pgdoc.M{"billingProfileId": bpID}, &card)
	if card["tokenId"] != "pm_fake" || card["panMasked"] != "*.4242" {
		t.Fatalf("card fields: %v", card)
	}
	// idempotent: a second confirm must NOT store a duplicate card.
	if _, err := svc.ProcessRegisterCard(ctx, resp.TransactionID); err != nil {
		t.Fatalf("idempotent: %v", err)
	}
	if n2, _ := db.C("creditCard").Count(ctx, pgdoc.M{"billingProfileId": bpID}); n2 != 1 {
		t.Fatalf("idempotent stored duplicate card: %d", n2)
	}
}

// TestAddFundsDispatch exercises payment.AddFundsService.AddFunds against real Postgres: it
// persists a PENDING AccountCreditTransaction with the gateway's PaymentIntent id and returns
// the client secret.
func TestAddFundsDispatch(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)

	// Seed the base currency + a Stripe gateway integration.
	if _, err := db.C("billingConfiguration").InsertOne(ctx, pgdoc.M{"baseCurrency": "USD"}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if _, err := db.C("thirdPartyIntegration").InsertOne(ctx, pgdoc.M{
		"_id": "gw1", "name": "Stripe", "thirdParty": "Stripe",
		"config": pgdoc.M{"publicKey": "pk_test_x", "minDeposit": int32(5)},
		"secret": pgdoc.M{"privateKey": "sk_test_x"},
	}); err != nil {
		t.Fatalf("seed gw: %v", err)
	}

	// the profile must be persisted (ProcessAddFunds re-loads it by id via FindByID).
	const bpID = "6a37e2f547f3b4378ba78549"
	if _, err := db.C("billingProfile").InsertOne(ctx, pgdoc.M{"_id": bpID, "currency": "USD", "country": "US", "email": "a@b.c", "firstName": "Ada"}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	fg := &fakeGW{}
	svc := payment.NewAddFundsService(repo, pricingRepo, func(*billing.Gateway) payment.Gateway { return fg })
	profile := &billing.BillingProfile{ID: bpID, Currency: "USD", Country: "US", Email: "a@b.c", FirstName: "Ada"}

	// below minDeposit (5) → 400.
	small := decimal.NewFromInt(2)
	if _, err := svc.AddFunds(ctx, profile, payment.AddFundsRequest{Amount: &small, PaymentGatewayID: "gw1"}); err == nil {
		t.Fatal("expected minDeposit error for amount 2")
	}

	// valid deposit → PENDING txn + PaymentIntent.
	amt := decimal.NewFromInt(10)
	resp, err := svc.AddFunds(ctx, profile, payment.AddFundsRequest{Amount: &amt, PaymentGatewayID: "gw1"})
	if err != nil {
		t.Fatalf("addFunds: %v", err)
	}
	if resp.ExternalPaymentID != "pi_fake" || resp.Metadata != "cs_fake" || resp.TransactionID == "" {
		t.Fatalf("response wrong: %+v", resp)
	}
	if resp.ThirdParty != "Stripe" || resp.InvoiceCurrency != "USD" {
		t.Fatalf("response fields: %+v", resp)
	}
	if fg.lastAmountCents != 1000 { // 10.00 → 1000 cents (no tax seeded)
		t.Fatalf("amount cents = %d, want 1000", fg.lastAmountCents)
	}

	// the PENDING transaction is persisted with the PaymentIntent id.
	tx, err := repo.AccountCreditTransactionByID(ctx, resp.TransactionID)
	if err != nil || tx == nil {
		t.Fatalf("txn not persisted: %v / %v", err, tx)
	}
	if tx.Status != "PENDING" || tx.ExternalID != "pi_fake" || tx.PaymentGatewayID != "gw1" {
		t.Fatalf("txn fields: %+v", tx)
	}
	if tx.Amount == nil || !tx.Amount.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("txn amount: %v", tx.Amount)
	}

	// confirm callback (fakeGW retrieve → "succeeded"): txn → SUCCESS + one AccountCredit balance.
	done, err := svc.ProcessAddFunds(ctx, resp.TransactionID)
	if err != nil {
		t.Fatalf("processAddFunds: %v", err)
	}
	if done.Status != "SUCCESS" {
		t.Fatalf("confirm status = %s, want SUCCESS", done.Status)
	}
	n, _ := db.C("accountCredit").Count(ctx, pgdoc.M{"billingProfileId": bpID})
	if n != 1 {
		t.Fatalf("accountCredit count = %d, want 1", n)
	}
	// the minted credit must carry invoiceExchangeRate (=1 same-currency): a rate-less credit
	// nil-panics the bill-settle leg (the prod midnight monthlyBill outage shape).
	var credit struct {
		InvoiceCurrency     string           `json:"invoiceCurrency"`
		InvoiceExchangeRate *decimal.Decimal `json:"invoiceExchangeRate"`
	}
	if found, err := db.C("accountCredit").FindOne(ctx, pgdoc.M{"billingProfileId": bpID}, &credit); err != nil || !found {
		t.Fatalf("minted credit not readable: %v / %v", found, err)
	}
	if credit.InvoiceCurrency != "USD" || credit.InvoiceExchangeRate == nil || !credit.InvoiceExchangeRate.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("minted credit invoice fields: ccy=%q rate=%v, want USD @ 1", credit.InvoiceCurrency, credit.InvoiceExchangeRate)
	}
	// idempotent: a second confirm must NOT create a duplicate credit.
	if _, err := svc.ProcessAddFunds(ctx, resp.TransactionID); err != nil {
		t.Fatalf("idempotent confirm: %v", err)
	}
	if n2, _ := db.C("accountCredit").Count(ctx, pgdoc.M{"billingProfileId": bpID}); n2 != 1 {
		t.Fatalf("idempotent confirm created duplicate credit: %d", n2)
	}
}

// TestProcessAddFundsBranches covers the processSuccessTransaction branches added for the
// audit §7 gaps: a deposit tied to an order flips the order PAID (no credit minted); a deposit
// tied to a savings contract ACTIVATES the contract (no credit minted); the already-processed
// guard rejects re-driving a terminal txn.
func TestProcessAddFundsBranches(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)

	if _, err := db.C("billingConfiguration").InsertOne(ctx, pgdoc.M{"baseCurrency": "USD"}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if _, err := db.C("thirdPartyIntegration").InsertOne(ctx, pgdoc.M{
		"_id": "gw1", "name": "Stripe", "thirdParty": "Stripe",
		"config": pgdoc.M{"publicKey": "pk_x"}, "secret": pgdoc.M{"privateKey": "sk_x"},
	}); err != nil {
		t.Fatalf("seed gw: %v", err)
	}
	const bpID = "6a37e2f547f3b4378ba78549"
	if _, err := db.C("billingProfile").InsertOne(ctx, pgdoc.M{
		"_id": bpID, "currency": "USD", "country": "US", "email": "a@b.c", "firstName": "Ada",
	}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	tenD128 := decimalOf(t, "10")

	svc := payment.NewAddFundsService(repo, pricingRepo, func(*billing.Gateway) payment.Gateway { return &fakeGW{} })
	orderFlips := map[string]string{}
	svc.SetOrderStatusUpdater(func(_ context.Context, orderID, _ string, _ decimal.Decimal, status string) error {
		orderFlips[orderID] = status
		return nil
	})

	// (1) order-tied deposit → order PAID, NO account credit.
	ordTxn := pgdoc.NewID()
	if _, err := db.C("accountCreditTransaction").InsertOne(ctx, pgdoc.M{
		"_id": ordTxn, "status": "PENDING", "billingProfileId": bpID, "paymentGatewayId": "gw1",
		"externalId": "pi_fake", "currency": "USD", "amount": tenD128, "grossAmount": tenD128,
		"orderId": "ord-1",
	}); err != nil {
		t.Fatalf("seed order txn: %v", err)
	}
	if _, err := svc.ProcessAddFunds(ctx, ordTxn); err != nil {
		t.Fatalf("process order txn: %v", err)
	}
	if orderFlips["ord-1"] != "PAID" {
		t.Errorf("order not flipped PAID: %v", orderFlips)
	}
	if cn, _ := db.C("accountCredit").Count(ctx, pgdoc.M{}); cn != 0 {
		t.Errorf("order deposit must NOT mint credit, got %d", cn)
	}

	// (2) savings-contract deposit → contract ACTIVE, NO account credit.
	contractID := pgdoc.NewID()
	if _, err := db.C("savingsContract").InsertOne(ctx, pgdoc.M{
		"_id": contractID, "status": "PENDING_PAYMENT", "billingProfileId": bpID, "savingsPlanName": "Saver", "paidUpfront": true, "upfrontAmount": tenD128,
	}); err != nil {
		t.Fatalf("seed contract: %v", err)
	}
	savTxn := pgdoc.NewID()
	if _, err := db.C("accountCreditTransaction").InsertOne(ctx, pgdoc.M{
		"_id": savTxn, "status": "PENDING", "billingProfileId": bpID, "paymentGatewayId": "gw1",
		"externalId": "pi_fake", "currency": "USD", "amount": tenD128, "grossAmount": tenD128,
		"metadata": pgdoc.M{"savingsContractId": contractID},
	}); err != nil {
		t.Fatalf("seed savings txn: %v", err)
	}
	if _, err := svc.ProcessAddFunds(ctx, savTxn); err != nil {
		t.Fatalf("process savings txn: %v", err)
	}
	var contract pgdoc.M
	_, _ = db.C("savingsContract").FindOne(ctx, pgdoc.M{"_id": contractID}, &contract)
	if contract["status"] != "ACTIVE" {
		t.Errorf("contract status = %v, want ACTIVE", contract["status"])
	}
	if cn, _ := db.C("accountCredit").Count(ctx, pgdoc.M{}); cn != 0 {
		t.Errorf("savings deposit must NOT mint credit, got %d", cn)
	}

	// (3) already-processed guard: a FAILED txn must be rejected, not re-driven into a credit.
	failedTxn := pgdoc.NewID()
	if _, err := db.C("accountCreditTransaction").InsertOne(ctx, pgdoc.M{
		"_id": failedTxn, "status": "FAILED", "billingProfileId": bpID, "paymentGatewayId": "gw1",
		"externalId": "pi_fake", "currency": "USD", "amount": tenD128, "grossAmount": tenD128,
	}); err != nil {
		t.Fatalf("seed failed txn: %v", err)
	}
	if _, err := svc.ProcessAddFunds(ctx, failedTxn); err == nil {
		t.Error("re-processing a FAILED txn must 400 (already processed)")
	}
	if cn, _ := db.C("accountCredit").Count(ctx, pgdoc.M{}); cn != 0 {
		t.Errorf("guard leaked a credit: %d", cn)
	}
}

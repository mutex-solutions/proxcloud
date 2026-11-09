//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/payment"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// decliningGW is a fakeGW whose card charges are declined (a synchronous gateway decline).
type decliningGW struct {
	fakeGW
	declines int
}

func (g *decliningGW) CollectPaymentIntent(_ context.Context, in payment.CollectInput) (payment.PaymentIntentResult, error) {
	g.declines++
	g.lastAmountCents = in.AmountCents
	return payment.PaymentIntentResult{ID: "pi_declined", Status: "failed", ErrorMessage: "Your card has insufficient funds.", ErrorCode: "insufficient_funds"}, nil
}

// seedPostpaid seeds the config, a card gateway, a POSTPAID profile with a stored card, and
// returns the profile + the ids.
func seedPostpaid(t *testing.T, db *pgdoc.DB, mode string) (*billing.BillingProfile, string) {
	t.Helper()
	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD"})
	mustInsert(t, db, "thirdPartyIntegration", pgdoc.M{"_id": "gw-awx", "name": "Airwallex", "thirdParty": "Airwallex",
		"config": pgdoc.M{"clientId": "cid", "sandbox": true}, "secret": pgdoc.M{"apiKey": "k"}})
	bpID := pgdoc.NewID()
	mustInsert(t, db, "billingProfile", pgdoc.M{"_id": bpID, "currency": "USD", "country": "US", "status": "ACTIVE",
		"email": "owner@acme.test", "firstName": "Ann", "lastName": "Lee", "paymentMode": mode,
		"contacts": []any{pgdoc.M{"name": "Finance", "email": "finance@acme.test"}}})
	cardID := pgdoc.NewID()
	mustInsert(t, db, "creditCard", pgdoc.M{"_id": cardID, "billingProfileId": bpID, "tokenId": "cst_saved", "panMasked": "*.0008",
		"paymentGatewayId": "gw-awx", "tokenExpirationDate": time.Now().UTC().AddDate(1, 0, 0)})
	return &billing.BillingProfile{ID: bpID, Currency: "USD", Country: "US", Status: billing.StatusActive,
		Email: "owner@acme.test", FirstName: "Ann", LastName: "Lee", PaymentMode: mode,
		Contacts: []billing.Contact{{Name: "Finance", Email: "finance@acme.test"}}}, cardID
}

func lastMonthOpenBill(t *testing.T, db *pgdoc.DB, bpID string, net string) string {
	t.Helper()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return mustInsertID(t, db, "bill", pgdoc.M{
		"billingProfileId": bpID, "status": "OPEN", "invoiceCurrency": "USD",
		"billingCycle": pgdoc.M{"startDate": start, "endDate": end},
		"items":        []any{pgdoc.M{"resourceType": "instance", "currency": "USD", "netAmount": decimalOf(t, net)}},
	})
}

// TestPostpaidAutoCollectOnSend: a POSTPAID profile's bill is charged to its card the moment the
// monthlyBill send issues it → bill PAID, a SUCCESS collectTransaction, and ONE mail to the owner +
// contacts: bill_is_paid with the invoice PDF (no bill_is_generated asking for a bank transfer). A
// PREPAID profile with a saved card is charged the same way; one without a card stays SENT.
func TestPostpaidAutoCollectOnSend(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)
	profile, _ := seedPostpaid(t, db, billing.PaymentModePostpaid)
	billID := lastMonthOpenBill(t, db, profile.ID, "20")

	fg := &fakeGW{}
	collect := payment.NewCollectService(repo, pricingRepo, func(*billing.Gateway) payment.Gateway { return fg })
	mails := &captureNotifier{}
	collect.SetNotifier(mails)
	collect.SetBillLink(func(_ context.Context, p *billing.BillingProfile, b *pricing.Bill) string {
		return "https://console/bills/" + b.ID
	})
	send := billing.NewBillSendService(repo, pricingRepo)
	send.SetNotifier(mails)
	send.SetAutoCollector(func(ctx context.Context, p *billing.BillingProfile) error {
		_, err := collect.CollectBillingProfile(ctx, p)
		return err
	})

	now := time.Date(2026, 9, 1, 0, 0, 30, 0, time.UTC)
	if ok, err := send.SendBillForProfile(ctx, profile, now); err != nil || !ok {
		t.Fatalf("send: ok=%v err=%v", ok, err)
	}
	var b pricing.Bill
	_, _ = db.C("bill").Get(ctx, billID, &b)
	if b.Status != pricing.BillStatusPaid || len(b.CollectedAmounts) != 1 {
		t.Fatalf("bill after auto-collect: status=%s collected=%d", b.Status, len(b.CollectedAmounts))
	}
	if fg.lastAmountCents != 2000 {
		t.Fatalf("charged cents = %d, want 2000", fg.lastAmountCents)
	}
	n, _ := db.C("collectTransaction").Count(ctx, pgdoc.M{"billingProfileId": profile.ID, "status": "SUCCESS", "billId": billID})
	if n != 1 {
		t.Fatalf("SUCCESS collect txns = %d, want 1", n)
	}
	keys := []string{}
	for _, c := range mails.calls {
		keys = append(keys, c.key)
		if len(c.to) != 2 || c.to[0] != "owner@acme.test" || c.to[1] != "finance@acme.test" {
			t.Fatalf("%s recipients: %v", c.key, c.to)
		}
	}
	if strings.Join(keys, ",") != "bill_is_paid" {
		t.Fatalf("mails: %v", keys)
	}
	if v := mails.calls[0].vars; v["billUrl"] != "https://console/bills/"+billID || v["amountDue"] != "0.00" {
		t.Fatalf("bill_is_paid vars: %v", v)
	}
	if atts := mails.calls[0].atts; len(atts) != 1 || atts[0].ContentType != "application/pdf" ||
		!strings.Contains(atts[0].Filename, "INV-2026-000001") || !strings.HasPrefix(string(atts[0].Data), "%PDF") {
		t.Fatalf("bill_is_paid must carry the invoice PDF, got %d attachment(s)", len(atts))
	}

	// A PREPAID profile with a saved card is charged at send too (the collect cron would otherwise
	// charge that card hours after a bank-transfer request), so it also gets only bill_is_paid.
	prepaid, _ := seedPostpaidLike(t, db, billing.PaymentModePrepaid)
	pb := lastMonthOpenBill(t, db, prepaid.ID, "5")
	fg.lastAmountCents = 0
	mails.calls = nil
	if ok, err := send.SendBillForProfile(ctx, prepaid, now); err != nil || !ok {
		t.Fatalf("send prepaid: ok=%v err=%v", ok, err)
	}
	var pbill pricing.Bill
	_, _ = db.C("bill").Get(ctx, pb, &pbill)
	if pbill.Status != pricing.BillStatusPaid || fg.lastAmountCents != 500 || len(mails.calls) != 1 || mails.calls[0].key != "bill_is_paid" {
		t.Fatalf("prepaid with a card: status=%s cents=%d mails=%+v", pbill.Status, fg.lastAmountCents, mails.calls)
	}

	// A PREPAID profile WITHOUT a card is not charged: the bill stays SENT and bill_is_generated
	// carries the bank-transfer details (no no-card warning — a transfer is its normal way to pay).
	noCardID := pgdoc.NewID()
	mustInsert(t, db, "billingProfile", pgdoc.M{"_id": noCardID, "currency": "USD", "country": "US", "status": "ACTIVE",
		"email": "three@acme.test", "firstName": "Cy", "lastName": "Ho", "paymentMode": billing.PaymentModePrepaid})
	noCard := &billing.BillingProfile{ID: noCardID, Currency: "USD", Country: "US", Status: billing.StatusActive,
		Email: "three@acme.test", FirstName: "Cy", LastName: "Ho", PaymentMode: billing.PaymentModePrepaid}
	nb := lastMonthOpenBill(t, db, noCardID, "7")
	fg.lastAmountCents = 0
	mails.calls = nil
	if ok, err := send.SendBillForProfile(ctx, noCard, now); err != nil || !ok {
		t.Fatalf("send prepaid without a card: ok=%v err=%v", ok, err)
	}
	var nbill pricing.Bill
	_, _ = db.C("bill").Get(ctx, nb, &nbill)
	if nbill.Status != pricing.BillStatusSent || fg.lastAmountCents != 0 || len(mails.calls) != 1 || mails.calls[0].key != "bill_is_generated" {
		t.Fatalf("prepaid without a card: status=%s cents=%d mails=%+v", nbill.Status, fg.lastAmountCents, mails.calls)
	}
}

// seedPostpaidLike seeds a second profile (with a card) in an already-seeded database.
func seedPostpaidLike(t *testing.T, db *pgdoc.DB, mode string) (*billing.BillingProfile, string) {
	t.Helper()
	bpID := pgdoc.NewID()
	mustInsert(t, db, "billingProfile", pgdoc.M{"_id": bpID, "currency": "USD", "country": "US", "status": "ACTIVE",
		"email": "two@acme.test", "firstName": "Bo", "lastName": "Ng", "paymentMode": mode})
	cardID := pgdoc.NewID()
	mustInsert(t, db, "creditCard", pgdoc.M{"_id": cardID, "billingProfileId": bpID, "tokenId": "cst_two", "panMasked": "*.1111",
		"paymentGatewayId": "gw-awx", "tokenExpirationDate": time.Now().UTC().AddDate(1, 0, 0)})
	return &billing.BillingProfile{ID: bpID, Currency: "USD", Country: "US", Status: billing.StatusActive, Email: "two@acme.test", FirstName: "Bo", LastName: "Ng", PaymentMode: mode}, cardID
}

// TestPostpaidDeclineMailsAndRetry: a declined automatic charge leaves the bill SENT with a FAILED
// collectTransaction carrying the gateway message, mails notify_customer_payment_failed to the
// owner + contacts with the reason / amount / card / link, then bill_is_generated (the invoice with
// the bank-transfer details); the customer's retry from the bill page (CollectBill) then succeeds →
// PAID.
func TestPostpaidDeclineMailsAndRetry(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)
	profile, cardID := seedPostpaid(t, db, billing.PaymentModePostpaid)
	billID := lastMonthOpenBill(t, db, profile.ID, "80.05")

	dg := &decliningGW{}
	var gw payment.Gateway = dg
	collect := payment.NewCollectService(repo, pricingRepo, func(*billing.Gateway) payment.Gateway { return gw })
	mails := &captureNotifier{}
	collect.SetNotifier(mails)
	collect.SetBillLink(func(_ context.Context, p *billing.BillingProfile, b *pricing.Bill) string {
		return "https://console/bills/" + b.ID
	})
	send := billing.NewBillSendService(repo, pricingRepo)
	send.SetNotifier(mails)
	send.SetAutoCollector(func(ctx context.Context, p *billing.BillingProfile) error {
		_, err := collect.CollectBillingProfile(ctx, p)
		return err
	})
	now := time.Date(2026, 9, 1, 0, 0, 30, 0, time.UTC)
	if ok, err := send.SendBillForProfile(ctx, profile, now); err != nil || !ok {
		t.Fatalf("send: ok=%v err=%v", ok, err)
	}
	var b pricing.Bill
	_, _ = db.C("bill").Get(ctx, billID, &b)
	if b.Status != pricing.BillStatusSent || len(b.CollectedAmounts) != 0 {
		t.Fatalf("declined bill must stay SENT and unsettled: status=%s collected=%d", b.Status, len(b.CollectedAmounts))
	}
	var failed []pricing.CollectTransaction
	_ = db.C("collectTransaction").Find(ctx, pgdoc.M{"billId": billID}, &failed)
	if len(failed) != 1 || failed[0].Status != pricing.CollectTransactionStatusFailed || failed[0].ExternalID != "pi_declined" ||
		failed[0].ErrorMessage != "Your card has insufficient funds. (insufficient_funds)" {
		t.Fatalf("collect txn after decline: %+v", failed)
	}
	if len(mails.calls) != 2 || mails.calls[0].key != "notify_customer_payment_failed" || mails.calls[1].key != "bill_is_generated" {
		t.Fatalf("mails: %+v", mails.calls)
	}
	if v := mails.calls[1].vars; v["amountDue"] != "80.05" || v["dueDate"] != "2026-09-08" || len(mails.calls[1].atts) != 1 {
		t.Fatalf("bill_is_generated after the decline: vars=%v atts=%d", v, len(mails.calls[1].atts))
	}
	v := mails.calls[0].vars
	if v["fullName"] != "Ann Lee" || v["amountDue"] != "80.05" || v["currency"] != "USD" || v["panMasked"] != "*.0008" ||
		v["errorMessage"] != "Your card has insufficient funds. (insufficient_funds)" || v["billUrl"] != "https://console/bills/"+billID ||
		v["invoiceNumber"] != "INV-2026-000001" || v["dueDate"] != "2026-09-08" {
		t.Fatalf("payment-failed vars: %v", v)
	}
	if to := mails.calls[0].to; len(to) != 2 || to[0] != "owner@acme.test" || to[1] != "finance@acme.test" {
		t.Fatalf("payment-failed recipients: %v", to)
	}

	// The cron retry declines again → a second FAILED txn + a second mail (still SENT).
	if paid, err := collect.CollectBillingProfile(ctx, profile); err != nil || paid != 0 {
		t.Fatalf("cron retry: paid=%d err=%v", paid, err)
	}
	if n, _ := db.C("collectTransaction").Count(ctx, pgdoc.M{"billId": billID, "status": "FAILED"}); n != 2 {
		t.Fatalf("FAILED txns after retry = %d, want 2", n)
	}
	if dg.declines != 2 || len(mails.calls) != 3 {
		t.Fatalf("declines=%d mails=%d", dg.declines, len(mails.calls))
	}

	// Validation of the manual retry: an unknown bill 404s, an OPEN bill 400s, a foreign card 400s.
	if _, err := collect.CollectBill(ctx, profile, pgdoc.NewID(), payment.CollectBillRequest{}); err == nil {
		t.Fatal("unknown bill must fail")
	}
	openID := mustInsertID(t, db, "bill", pgdoc.M{"billingProfileId": profile.ID, "status": "OPEN", "invoiceCurrency": "USD", "items": []any{pgdoc.M{"netAmount": decimalOf(t, "1")}}})
	if _, err := collect.CollectBill(ctx, profile, openID, payment.CollectBillRequest{}); err == nil || !strings.Contains(err.Error(), "SENT") {
		t.Fatalf("open bill: %v", err)
	}
	if _, err := collect.CollectBill(ctx, profile, billID, payment.CollectBillRequest{CardID: pgdoc.NewID()}); err == nil {
		t.Fatal("a card of another profile / unknown card must fail")
	}

	// The customer fixed the card: the retry from the bill page charges the saved card → PAID +
	// bill_is_paid. The web layer maps the SUCCESS/FAILED txn straight to the response.
	ok := &fakeGW{}
	gw = ok
	txn, err := collect.CollectBill(ctx, profile, billID, payment.CollectBillRequest{CardID: cardID})
	if err != nil || txn.Status != pricing.CollectTransactionStatusSuccess {
		t.Fatalf("retry: %+v %v", txn, err)
	}
	if ok.lastAmountCents != 8005 {
		t.Fatalf("retry charged cents = %d, want 8005", ok.lastAmountCents)
	}
	_, _ = db.C("bill").Get(ctx, billID, &b)
	if b.Status != pricing.BillStatusPaid {
		t.Fatalf("bill after retry = %s, want PAID", b.Status)
	}
	if last := mails.calls[len(mails.calls)-1]; last.key != "bill_is_paid" {
		t.Fatalf("last mail = %s, want bill_is_paid", last.key)
	}
	// A settled bill cannot be charged twice.
	if _, err := collect.CollectBill(ctx, profile, billID, payment.CollectBillRequest{}); err == nil {
		t.Fatal("a PAID bill must not be collectable")
	}
}

// TestPostpaidUnknownOutcomeAtSendMailsNothing: when the automatic charge's outcome is unknown (the
// collect stays PENDING for the scanner), the send mails nothing — a bank-transfer request now could
// be paid on top of the card; the scanner's paid / failed mail follows once the gateway answers.
func TestPostpaidUnknownOutcomeAtSendMailsNothing(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)
	profile, _ := seedPostpaid(t, db, billing.PaymentModePostpaid)
	billID := lastMonthOpenBill(t, db, profile.ID, "20")

	collect := payment.NewCollectService(repo, pricingRepo, func(*billing.Gateway) payment.Gateway { return &unknownOutcomeGW{} })
	mails := &captureNotifier{}
	collect.SetNotifier(mails)
	send := billing.NewBillSendService(repo, pricingRepo)
	send.SetNotifier(mails)
	send.SetAutoCollector(func(ctx context.Context, p *billing.BillingProfile) error {
		_, err := collect.CollectBillingProfile(ctx, p)
		return err
	})
	if ok, err := send.SendBillForProfile(ctx, profile, time.Date(2026, 9, 1, 0, 0, 30, 0, time.UTC)); err != nil || !ok {
		t.Fatalf("send: ok=%v err=%v", ok, err)
	}
	var b pricing.Bill
	_, _ = db.C("bill").Get(ctx, billID, &b)
	n, _ := db.C("collectTransaction").Count(ctx, pgdoc.M{"billId": billID, "status": "PENDING"})
	if b.Status != pricing.BillStatusSent || n != 1 {
		t.Fatalf("bill=%s pending charges=%d, want SENT + 1", b.Status, n)
	}
	if len(mails.calls) != 0 {
		t.Fatalf("no mail while the charge is pending, got %+v", mails.calls)
	}
}

// TestCollectGatewayErrorEndsFailed: a transport error from the gateway during a bill collect no
// longer leaves a PENDING orphan (which the scanner skips forever) — the txn ends FAILED with the
// error and the customer is mailed.
func TestCollectGatewayErrorEndsFailed(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	profile, _ := seedPostpaid(t, db, billing.PaymentModePostpaid)
	billID := mustInsertID(t, db, "bill", pgdoc.M{"billingProfileId": profile.ID, "status": "SENT", "invoiceCurrency": "USD",
		"items": []any{pgdoc.M{"netAmount": decimalOf(t, "9")}}})
	collect := payment.NewCollectService(repo, pricing.NewRepo(db), func(*billing.Gateway) payment.Gateway { return &erroringGW{} })
	mails := &captureNotifier{}
	collect.SetNotifier(mails)
	paid, err := collect.CollectBillingProfile(ctx, profile)
	if err != nil || paid != 0 {
		t.Fatalf("collect: paid=%d err=%v", paid, err)
	}
	var txns []pricing.CollectTransaction
	_ = db.C("collectTransaction").Find(ctx, pgdoc.M{"billId": billID}, &txns)
	if len(txns) != 1 || txns[0].Status != pricing.CollectTransactionStatusFailed || !strings.Contains(txns[0].ErrorMessage, "deadline exceeded") {
		t.Fatalf("txn after gateway error: %+v", txns)
	}
	if len(mails.calls) != 1 || mails.calls[0].key != "notify_customer_payment_failed" {
		t.Fatalf("mails: %+v", mails.calls)
	}

	// An error AFTER an intent exists (unknown outcome, e.g. a timeout on the confirm) must NOT be
	// marked FAILED — a retry could double-charge — it stays PENDING with the externalId for the
	// scanner to reconcile, and no failure mail goes out.
	unknown := payment.NewCollectService(repo, pricing.NewRepo(db), func(*billing.Gateway) payment.Gateway { return &unknownOutcomeGW{} })
	unknown.SetNotifier(mails)
	if _, err := unknown.CollectBillingProfile(ctx, profile); err != nil {
		t.Fatalf("collect unknown: %v", err)
	}
	var pending []pricing.CollectTransaction
	_ = db.C("collectTransaction").Find(ctx, pgdoc.M{"billId": billID, "status": "PENDING"}, &pending)
	if len(pending) != 1 || pending[0].ExternalID != "pi_unknown" || !strings.Contains(pending[0].ErrorMessage, "deadline exceeded") {
		t.Fatalf("unknown-outcome txn: %+v", pending)
	}
	if len(mails.calls) != 1 {
		t.Fatalf("no failure mail expected for an unknown outcome, got %d", len(mails.calls))
	}
}

type unknownOutcomeGW struct{ fakeGW }

func (g *unknownOutcomeGW) CollectPaymentIntent(context.Context, payment.CollectInput) (payment.PaymentIntentResult, error) {
	return payment.PaymentIntentResult{ID: "pi_unknown", Status: "pending"}, context.DeadlineExceeded
}

type erroringGW struct{ fakeGW }

func (g *erroringGW) CollectPaymentIntent(context.Context, payment.CollectInput) (payment.PaymentIntentResult, error) {
	return payment.PaymentIntentResult{}, context.DeadlineExceeded
}

func (g *erroringGW) RetrievePaymentIntent(context.Context, string) (payment.PaymentIntentResult, error) {
	return payment.PaymentIntentResult{}, context.DeadlineExceeded
}

// TestRegisterCardReleasesHold: the card-confirm callback stores the verified card and releases the
// 1.00 hold exactly once (idempotent on a repeated callback); the register response carries what
// the browser needs.
func TestRegisterCardReleasesHold(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	profile, _ := seedPostpaid(t, db, billing.PaymentModePostpaid)
	fg := &fakeGW{}
	svc := payment.NewRegisterCardService(repo, func(*billing.Gateway) payment.Gateway { return fg })
	resp, err := svc.RegisterCard(ctx, profile, payment.RegisterCardRequest{PaymentGatewayID: "gw-awx"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	md, _ := resp.Metadata.(map[string]any)
	if resp.ThirdParty != "Airwallex" || md["client_secret"] != "seti_cs" || md["customerId"] != "cus_fake" || md["currency"] != "USD" || md["verificationAmount"] != "1.00" {
		t.Fatalf("register response: %+v", resp)
	}
	if _, err := svc.ProcessRegisterCard(ctx, resp.TransactionID); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, err := svc.ProcessRegisterCard(ctx, resp.TransactionID); err != nil {
		t.Fatalf("second confirm: %v", err)
	}
	if len(fg.released) != 1 || fg.released[0] != "seti_fake" {
		t.Fatalf("hold releases = %v, want exactly [seti_fake]", fg.released)
	}
	if n, _ := db.C("creditCard").Count(ctx, pgdoc.M{"billingProfileId": profile.ID, "tokenId": "pm_fake"}); n != 1 {
		t.Fatalf("stored cards = %d, want 1", n)
	}
	// a profile without a currency cannot verify a card (the hold needs one).
	if _, err := svc.RegisterCard(ctx, &billing.BillingProfile{ID: profile.ID}, payment.RegisterCardRequest{PaymentGatewayID: "gw-awx"}); err == nil {
		t.Fatal("missing currency must be refused")
	}
	// the manual BankTransfer gateway cannot register a card.
	mustInsert(t, db, "thirdPartyIntegration", pgdoc.M{"_id": "gw-bank", "thirdParty": "BankTransfer"})
	if _, err := svc.RegisterCard(ctx, profile, payment.RegisterCardRequest{PaymentGatewayID: "gw-bank"}); err == nil {
		t.Fatal("bank transfer must be refused for cards")
	}
}

// TestPaymentSettingsUpdate: the tenant payment-settings partial update validates the mode and the
// gateway, persists only those fields, and the summary reports them (PREPAID by default).
func TestPaymentSettingsUpdate(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	profile, _ := seedPostpaid(t, db, "")
	mustInsert(t, db, "thirdPartyIntegration", pgdoc.M{"_id": "gw-smtp", "thirdParty": "SMTP"})

	raw, _ := json.Marshal(billing.ToSummary(profile))
	if !strings.Contains(string(raw), `"paymentMode":"PREPAID"`) || strings.Contains(string(raw), "paymentGatewayId") {
		t.Fatalf("default summary: %s", raw)
	}
	str := func(s string) *string { return &s }
	if _, err := repo.UpdatePaymentSettings(ctx, profile, billing.PaymentSettings{PaymentMode: str("weekly")}); err == nil {
		t.Fatal("bad mode must be refused")
	}
	if _, err := repo.UpdatePaymentSettings(ctx, profile, billing.PaymentSettings{PaymentGatewayID: str("nope")}); err == nil {
		t.Fatal("unknown gateway must be refused")
	} else if he, ok := err.(*httpx.HTTPError); !ok || he.Status != 404 {
		t.Fatalf("unknown gateway error: %v", err)
	}
	if _, err := repo.UpdatePaymentSettings(ctx, profile, billing.PaymentSettings{PaymentGatewayID: str("gw-smtp")}); err == nil {
		t.Fatal("a non-payment integration must be refused")
	}
	saved, err := repo.UpdatePaymentSettings(ctx, profile, billing.PaymentSettings{PaymentMode: str("postpaid"), PaymentGatewayID: str("gw-awx")})
	if err != nil || saved.PaymentMode != billing.PaymentModePostpaid || saved.PaymentGatewayID != "gw-awx" {
		t.Fatalf("update: %+v %v", saved, err)
	}
	reloaded, _ := repo.FindByID(ctx, profile.ID)
	if reloaded.PaymentMode != "POSTPAID" || reloaded.PaymentGatewayID != "gw-awx" || reloaded.Email != "owner@acme.test" || len(reloaded.Contacts) != 1 {
		t.Fatalf("persisted: %+v", reloaded)
	}
	if _, err := repo.UpdatePaymentSettings(ctx, reloaded, billing.PaymentSettings{PaymentGatewayID: str("")}); err != nil {
		t.Fatalf("clear gateway: %v", err)
	}
	reloaded, _ = repo.FindByID(ctx, profile.ID)
	if reloaded.PaymentGatewayID != "" || reloaded.PaymentMode != "POSTPAID" {
		t.Fatalf("after clear: %+v", reloaded)
	}
}

// TestCollectInFlightGuardAndScannerSurplus: a bill with a PENDING (unknown-outcome) charge is
// not charged again — the cron skips it and the manual retry answers 409; when the bill gets PAID
// by other means the scanner asks the gateway before closing the stuck charge: a succeeded charge
// becomes a spendable account credit, a non-succeeded one is CANCELLED. A deposit-by-card with an
// unknown outcome is re-driven through the gateway, not failed blind.
func TestCollectInFlightGuardAndScannerSurplus(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)
	profile, cardID := seedPostpaid(t, db, billing.PaymentModePostpaid)
	billID := mustInsertID(t, db, "bill", pgdoc.M{"billingProfileId": profile.ID, "status": "SENT", "invoiceCurrency": "USD",
		"items": []any{pgdoc.M{"netAmount": decimalOf(t, "30")}}})

	// 1) an unknown-outcome charge leaves a PENDING txn with an externalId …
	fg := &fakeGW{}
	var gw payment.Gateway = &unknownOutcomeGW{}
	collect := payment.NewCollectService(repo, pricingRepo, func(*billing.Gateway) payment.Gateway { return gw })
	mails := &captureNotifier{}
	collect.SetNotifier(mails)
	if _, err := collect.CollectBillingProfile(ctx, profile); err != nil {
		t.Fatalf("first collect: %v", err)
	}
	if n, _ := db.C("collectTransaction").Count(ctx, pgdoc.M{"billId": billID, "status": "PENDING"}); n != 1 {
		t.Fatalf("pending txns = %d, want 1", n)
	}
	// … so neither the cron nor the customer may start a second charge.
	gw = fg
	if paid, err := collect.CollectBillingProfile(ctx, profile); err != nil || paid != 0 || fg.lastAmountCents != 0 {
		t.Fatalf("cron must skip an in-flight bill: paid=%d cents=%d err=%v", paid, fg.lastAmountCents, err)
	}
	// The manual retry first asks the gateway about the in-flight charge: still pending → 409.
	gw = &statusGW{status: "requires_payment_method"}
	if _, err := collect.CollectBill(ctx, profile, billID, payment.CollectBillRequest{CardID: cardID}); err == nil || !strings.Contains(err.Error(), "still being processed") {
		t.Fatalf("manual retry must be refused while in flight: %v", err)
	}
	if n, _ := db.C("collectTransaction").Count(ctx, pgdoc.M{"billId": billID}); n != 1 {
		t.Fatalf("txns = %d, want 1 (no second charge)", n)
	}
	gw = fg

	// 2) the scanner reconciles it against the gateway (fakeGW: succeeded) → bill PAID.
	scanner := payment.NewTransactionScanner(repo, payment.NewAddFundsService(repo, pricingRepo, func(*billing.Gateway) payment.Gateway { return gw }), collect, nil).
		WithNow(func() time.Time { return time.Now().UTC().Add(30 * time.Minute) })
	if _, err := scanner.Scan(ctx); err != nil {
		t.Fatalf("scan: %v", err)
	}
	var b pricing.Bill
	_, _ = db.C("bill").Get(ctx, billID, &b)
	if b.Status != pricing.BillStatusPaid {
		t.Fatalf("bill after scan = %s, want PAID", b.Status)
	}

	// 3) surplus: another PENDING charge for the now-PAID bill whose gateway status is succeeded
	//    becomes an account credit (SUCCESS txn), one that is not succeeded is CANCELLED.
	mk := func(ext string) string {
		return mustInsertID(t, db, "collectTransaction", pgdoc.M{"billId": billID, "billingProfileId": profile.ID, "status": "PENDING",
			"externalId": ext, "paymentGatewayId": "gw-awx", "currency": "USD", "amount": decimalOf(t, "30"), "grossAmount": decimalOf(t, "30"),
			"exchangeRate": decimalOf(t, "1"), "creditCardId": cardID, "createdAt": time.Now().UTC().Add(-2 * time.Hour)})
	}
	okTxn := mk("pi_paid_twice")
	gw = fg
	if _, err := scanner.Scan(ctx); err != nil {
		t.Fatalf("scan surplus: %v", err)
	}
	var ok pricing.CollectTransaction
	_, _ = db.C("collectTransaction").Get(ctx, okTxn, &ok)
	if ok.Status != pricing.CollectTransactionStatusSuccess || ok.Metadata["surplusAccountCreditId"] == nil {
		t.Fatalf("surplus txn: %+v", ok)
	}
	if n, _ := db.C("accountCredit").Count(ctx, pgdoc.M{"billingProfileId": profile.ID}); n != 1 {
		t.Fatalf("surplus credits = %d, want 1", n)
	}
	notCharged := mk("pi_never_charged")
	gw = &statusGW{status: "requires_payment_method"}
	if _, err := scanner.Scan(ctx); err != nil {
		t.Fatalf("scan cancel: %v", err)
	}
	var nc pricing.CollectTransaction
	_, _ = db.C("collectTransaction").Get(ctx, notCharged, &nc)
	if nc.Status != pricing.CollectTransactionStatusCancelled {
		t.Fatalf("not-charged surplus txn: %+v", nc)
	}

	// 4) a deposit-by-card (no bill) stuck PENDING is re-driven: succeeded → credit minted.
	gw = fg
	dep := mustInsertID(t, db, "collectTransaction", pgdoc.M{"billingProfileId": profile.ID, "status": "PENDING", "externalId": "pi_dep",
		"paymentGatewayId": "gw-awx", "currency": "USD", "amount": decimalOf(t, "12"), "grossAmount": decimalOf(t, "12"), "exchangeRate": decimalOf(t, "1"),
		"creditCardId": cardID, "createdAt": time.Now().UTC().Add(-2 * time.Hour)})
	if _, err := scanner.Scan(ctx); err != nil {
		t.Fatalf("scan deposit: %v", err)
	}
	var d pricing.CollectTransaction
	_, _ = db.C("collectTransaction").Get(ctx, dep, &d)
	if d.Status != pricing.CollectTransactionStatusSuccess {
		t.Fatalf("deposit txn after scan: %+v", d)
	}
	if n, _ := db.C("accountCredit").Count(ctx, pgdoc.M{"billingProfileId": profile.ID}); n != 2 {
		t.Fatalf("credits after deposit scan = %d, want 2", n)
	}
}

type statusGW struct {
	fakeGW
	status string
}

func (g *statusGW) RetrievePaymentIntent(context.Context, string) (payment.PaymentIntentResult, error) {
	return payment.PaymentIntentResult{ID: "pi_x", Status: g.status}, nil
}

// TestAddFundsSettleClaim: the deposit confirm claims the PENDING transaction atomically — a
// transaction another caller is already settling (PROCESSING) is refused, a settle error hands it
// back to PENDING for the retry, and the winner mints exactly one credit.
func TestAddFundsSettleClaim(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)
	profile, _ := seedPostpaid(t, db, billing.PaymentModePrepaid)
	ten := decimalOf(t, "10")
	seed := func(status string) string {
		return mustInsertID(t, db, "accountCreditTransaction", pgdoc.M{"status": status, "billingProfileId": profile.ID, "paymentGatewayId": "gw-awx",
			"externalId": "pi_fake", "currency": "USD", "amount": ten, "grossAmount": ten})
	}
	svc := payment.NewAddFundsService(repo, pricingRepo, func(*billing.Gateway) payment.Gateway { return &fakeGW{} })

	// a txn a concurrent caller is settling right now → refused, no credit minted.
	busy := seed("PROCESSING")
	if _, err := svc.ProcessAddFunds(ctx, busy); err == nil {
		t.Fatal("a PROCESSING transaction must be refused")
	}
	if n, _ := db.C("accountCredit").Count(ctx, pgdoc.M{"billingProfileId": profile.ID}); n != 0 {
		t.Fatalf("credits after refused claim = %d, want 0", n)
	}
	// the winner settles once; the repeated callback is idempotent.
	ok := seed("PENDING")
	if done, err := svc.ProcessAddFunds(ctx, ok); err != nil || done.Status != "SUCCESS" {
		t.Fatalf("settle: %+v %v", done, err)
	}
	if done, err := svc.ProcessAddFunds(ctx, ok); err != nil || done.Status != "SUCCESS" {
		t.Fatalf("repeat: %+v %v", done, err)
	}
	if n, _ := db.C("accountCredit").Count(ctx, pgdoc.M{"billingProfileId": profile.ID}); n != 1 {
		t.Fatalf("credits = %d, want exactly 1", n)
	}
	// a settle error releases the claim back to PENDING (the order leg fails).
	svc.SetOrderStatusUpdater(func(context.Context, string, string, decimal.Decimal, string) error {
		return errors.New("order store down")
	})
	bad := mustInsertID(t, db, "accountCreditTransaction", pgdoc.M{"status": "PENDING", "billingProfileId": profile.ID, "paymentGatewayId": "gw-awx",
		"externalId": "pi_fake", "currency": "USD", "amount": ten, "grossAmount": ten, "orderId": pgdoc.NewID(), "metadata": pgdoc.M{}})
	if _, err := svc.ProcessAddFunds(ctx, bad); err == nil {
		t.Fatal("a failing order leg must fail the settle")
	}
	cur, _ := repo.AccountCreditTransactionByID(ctx, bad)
	if cur == nil || cur.Status != "PENDING" {
		t.Fatalf("failed settle must release the claim: %+v", cur)
	}
}

// pendingRefundGW accepts refunds asynchronously (pending) and answers the poll with `final`.
type pendingRefundGW struct {
	fakeGW
	final string
}

func (g *pendingRefundGW) Refund(context.Context, string) (payment.RefundResult, error) {
	return payment.RefundResult{ID: "rf_async", Status: "pending"}, nil
}

func (g *pendingRefundGW) RetrieveRefund(context.Context, string) (payment.RefundResult, error) {
	return payment.RefundResult{ID: "rf_async", Status: g.final, ErrorMessage: "insufficient balance"}, nil
}

// TestRefundPendingReconcile: an asynchronous refund voids the credit immediately but sends the
// credit note only once the gateway confirms it; a refund the gateway later FAILS restores the
// credit, returns the deposit to SUCCESS and mails the customer + the finance mailbox.
func TestRefundPendingReconcile(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	pricingRepo := pricing.NewRepo(db)
	profile, _ := seedPostpaid(t, db, billing.PaymentModePrepaid)
	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD", "company": pgdoc.M{"financeEmail": "finance@operator.test"}})
	_ = db.C("billingConfiguration").Find(ctx, pgdoc.M{}, &[]pgdoc.M{}) // two config docs: FinanceEmail reads the first found — keep one
	_, _ = db.C("billingConfiguration").DeleteOne(ctx, pgdoc.M{"company": nil})
	seed := func(credit string) (string, string) {
		cid := mustInsertID(t, db, "accountCredit", pgdoc.M{"billingProfileId": profile.ID, "currency": "USD", "initialAmount": decimalOf(t, credit), "amount": decimalOf(t, credit)})
		tid := mustInsertID(t, db, "accountCreditTransaction", pgdoc.M{"billingProfileId": profile.ID, "status": "SUCCESS", "externalId": "pi_done",
			"paymentGatewayId": "gw-awx", "currency": "USD", "grossAmount": decimalOf(t, "40"), "amount": decimalOf(t, "40"), "accountCredit": pgdoc.M{"id": cid}})
		return tid, cid
	}
	gw := &pendingRefundGW{final: "pending"}
	svc := payment.NewAddFundsService(repo, pricingRepo, func(*billing.Gateway) payment.Gateway { return gw })
	mails := &captureNotifier{}
	svc.SetNotifier(mails)
	svc.SetAdminURL("https://admin.example")
	scanner := payment.NewTransactionScanner(repo, svc, payment.NewCollectService(repo, pricingRepo, func(*billing.Gateway) payment.Gateway { return gw }), nil)

	// 1) pending refund: credit voided now, txn REFUNDED with the pending refund, NO mail yet.
	txnID, creditID := seed("25")
	done, err := svc.RefundFunds(ctx, txnID)
	if err != nil || done.Status != "REFUNDED" {
		t.Fatalf("refund: %+v %v", done, err)
	}
	if st, _ := done.Metadata["refund"].(map[string]any); st == nil || st["status"] != "PENDING" || st["id"] != "rf_async" || st["creditAmount"] != "25" {
		t.Fatalf("refund metadata: %v", done.Metadata)
	}
	ac, _ := repo.AccountCreditByID(ctx, creditID)
	if ac == nil || !ac.Amount.IsZero() || ac.RefundedAt == nil {
		t.Fatalf("credit not voided: %+v", ac)
	}
	if len(mails.calls) != 0 {
		t.Fatalf("no mail expected while the refund is pending, got %+v", mails.calls)
	}
	// still pending → the scanner leaves it alone.
	if _, err := scanner.Scan(ctx); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(mails.calls) != 0 {
		t.Fatalf("still pending must not mail: %+v", mails.calls)
	}
	// 2) the gateway fails it → credit restored, txn back to SUCCESS, customer + finance mailed.
	gw.final = "failed"
	if _, err := scanner.Scan(ctx); err != nil {
		t.Fatalf("scan failed refund: %v", err)
	}
	cur, _ := repo.AccountCreditTransactionByID(ctx, txnID)
	st, _ := cur.Metadata["refund"].(map[string]any)
	if cur.Status != "SUCCESS" || st["status"] != "FAILED" || st["failureReason"] != "insufficient balance" {
		t.Fatalf("txn after failed refund: %+v", cur)
	}
	ac, _ = repo.AccountCreditByID(ctx, creditID)
	if ac == nil || !ac.Amount.Equal(decimalOf(t, "25")) || ac.RefundedAt != nil {
		t.Fatalf("credit not restored: %+v", ac)
	}
	keys := map[string][]string{}
	for _, c := range mails.calls {
		keys[c.key] = c.to
	}
	if to := keys["refund_failed"]; len(to) != 2 || to[0] != "owner@acme.test" {
		t.Fatalf("customer refund_failed mail: %v", keys)
	}
	if to := keys["refund_failed_operator"]; len(to) != 1 || to[0] != "finance@operator.test" {
		t.Fatalf("finance refund_failed_operator mail: %v", keys)
	}
	if v := mails.calls[0].vars; v["restoredCredit"] != "25.00" || v["amount"] != "40.00" || v["reason"] != "insufficient balance" {
		t.Fatalf("refund_failed vars: %v", v)
	}
	// a second scan changes nothing (the refund is terminal now).
	n := len(mails.calls)
	if _, err := scanner.Scan(ctx); err != nil || len(mails.calls) != n {
		t.Fatalf("terminal refund re-processed: err=%v mails=%d", err, len(mails.calls))
	}

	// 3) a pending refund that the gateway confirms → credit note mail, credit stays voided.
	txn2, credit2 := seed("40")
	gw.final = "pending"
	if _, err := svc.RefundFunds(ctx, txn2); err != nil {
		t.Fatalf("refund 2: %v", err)
	}
	gw.final = "succeeded"
	mails.calls = nil
	if _, err := scanner.Scan(ctx); err != nil {
		t.Fatalf("scan succeeded refund: %v", err)
	}
	cur2, _ := repo.AccountCreditTransactionByID(ctx, txn2)
	st2, _ := cur2.Metadata["refund"].(map[string]any)
	if cur2.Status != "REFUNDED" || st2["status"] != "SUCCEEDED" {
		t.Fatalf("txn after confirmed refund: %+v", cur2)
	}
	if ac2, _ := repo.AccountCreditByID(ctx, credit2); ac2 == nil || !ac2.Amount.IsZero() {
		t.Fatalf("confirmed refund must keep the credit voided: %+v", ac2)
	}
	if len(mails.calls) != 1 || mails.calls[0].key != "send_refunded_invoice" {
		t.Fatalf("credit note mail: %+v", mails.calls)
	}
	// 4) a refund the gateway refuses outright is a 400 and changes nothing.
	txn3, credit3 := seed("10")
	refusing := payment.NewAddFundsService(repo, pricingRepo, func(*billing.Gateway) payment.Gateway { return &refusingRefundGW{} })
	if _, err := refusing.RefundFunds(ctx, txn3); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("refused refund: %v", err)
	}
	if cur3, _ := repo.AccountCreditTransactionByID(ctx, txn3); cur3.Status != "SUCCESS" {
		t.Fatalf("refused refund must not change the txn: %+v", cur3)
	}
	if ac3, _ := repo.AccountCreditByID(ctx, credit3); !ac3.Amount.Equal(decimalOf(t, "10")) {
		t.Fatalf("refused refund must not void the credit: %+v", ac3)
	}
}

type refusingRefundGW struct{ fakeGW }

func (g *refusingRefundGW) Refund(context.Context, string) (payment.RefundResult, error) {
	return payment.RefundResult{Status: "failed", ErrorMessage: "charge already refunded"}, nil
}

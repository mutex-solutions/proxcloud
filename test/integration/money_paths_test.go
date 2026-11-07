//go:build integration

package integration

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/payment"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// refundCountingGW counts the money sent back.
type refundCountingGW struct {
	fakeGW
	refunds   atomic.Int32
	onCollect func() // runs while the gateway is taking the charge
}

func (g *refundCountingGW) CollectPaymentIntent(ctx context.Context, in payment.CollectInput) (payment.PaymentIntentResult, error) {
	if g.onCollect != nil {
		g.onCollect()
	}
	return g.fakeGW.CollectPaymentIntent(ctx, in)
}

func (g *refundCountingGW) Refund(context.Context, string) (payment.RefundResult, error) {
	g.refunds.Add(1)
	return payment.RefundResult{ID: "rf_fake", Status: "succeeded"}, nil
}

type moneyFixture struct {
	db      *pgdoc.DB
	repo    *billing.Repo
	gw      *refundCountingGW
	svc     *payment.AddFundsService
	profile *billing.BillingProfile
}

func newMoneyFixture(t *testing.T) *moneyFixture {
	t.Helper()
	db := freshPG(t)
	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD"})
	mustInsert(t, db, "billingProfile", pgdoc.M{"_id": "bp-a", "currency": "USD", "country": "US", "email": "a@b.c", "firstName": "Ada"})
	mustInsert(t, db, "thirdPartyIntegration", pgdoc.M{"_id": "gw1", "name": "Stripe", "thirdParty": "Stripe", "config": pgdoc.M{"publicKey": "pk_x", "addFunds": true}, "secret": pgdoc.M{"privateKey": "sk_x"}})
	repo := billing.NewRepo(db)
	gw := &refundCountingGW{}
	svc := payment.NewAddFundsService(repo, pricing.NewRepo(db), func(*billing.Gateway) payment.Gateway { return gw })
	return &moneyFixture{db: db, repo: repo, gw: gw, svc: svc,
		profile: &billing.BillingProfile{ID: "bp-a", Currency: "USD", Country: "US", Email: "a@b.c", FirstName: "Ada"}}
}

func (f *moneyFixture) status(t *testing.T, id string) string {
	t.Helper()
	c, err := f.repo.SavingsContractByID(context.Background(), id)
	if err != nil || c == nil {
		t.Fatalf("contract %s: %v", id, err)
	}
	return c.Status
}

func (f *moneyFixture) credits(t *testing.T) int64 {
	t.Helper()
	n, err := f.db.C("accountCredit").Count(context.Background(), pgdoc.M{})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// seedContract inserts an upfront contract waiting for a 120.00 payment.
func (f *moneyFixture) seedContract(t *testing.T, bp string) string {
	t.Helper()
	return mustInsertID(t, f.db, "savingsContract", pgdoc.M{
		"status": billing.SavingsStatusPendingPayment, "billingProfileId": bp, "paidUpfront": true,
		"savingsPlanName": "Saver", "durationMonths": 12, "upfrontAmount": decimalOf(t, "120"),
	})
}

func (f *moneyFixture) deposit(t *testing.T, contractID string, amount *decimal.Decimal) (*payment.AddFundsResponse, error) {
	t.Helper()
	return f.svc.AddFunds(context.Background(), f.profile, payment.AddFundsRequest{
		PaymentGatewayID: "gw1", Amount: amount, SavingsContractID: contractID,
	})
}

func TestCreateUpfrontContractWaitsForPayment(t *testing.T) {
	ctx := context.Background()
	f := newMoneyFixture(t)
	planID := mustInsertID(t, f.db, "savingsPlan", pgdoc.M{"name": "Saver", "available": true, "accessMode": "PUBLIC",
		"savingSchedule": []any{pgdoc.M{"durationMonths": 12,
			"noUpfrontTiers": []any{pgdoc.M{"startAmount": decimalOf(t, "1"), "discount": decimalOf(t, "10")}},
			"upfrontTiers":   []any{pgdoc.M{"startAmount": decimalOf(t, "1"), "discount": decimalOf(t, "20")}},
		}}})
	now := time.Now().UTC()
	c, err := f.repo.CreateSavingsContract(ctx, "bp-a", billing.CreateSavingsContractInput{
		SavingsPlanID: planID, DurationMonths: 12, MonthlyCommittedAmount: decimalOf(t, "100"), PaidUpfront: true,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != billing.SavingsStatusPendingPayment {
		t.Fatalf("status = %s", c.Status)
	}
	if c.UpfrontAmount == nil || !c.UpfrontAmount.Equal(decimalOf(t, "960")) { // 100 x 12 less 20%
		t.Fatalf("upfront amount = %v", c.UpfrontAmount)
	}
	// A second one for the plan would stack discounts once both are paid.
	if _, err := f.repo.CreateSavingsContract(ctx, "bp-a", billing.CreateSavingsContractInput{
		SavingsPlanID: planID, DurationMonths: 12, MonthlyCommittedAmount: decimalOf(t, "100"), PaidUpfront: true,
	}, now); err == nil {
		t.Fatal("second contract for the plan accepted")
	}
	if _, err := f.repo.CreateSavingsContract(ctx, "bp-b", billing.CreateSavingsContractInput{
		SavingsPlanID: planID, DurationMonths: 12, MonthlyCommittedAmount: decimalOf(t, "-5"),
	}, now); err == nil {
		t.Fatal("negative commitment accepted")
	}
	noUp, err := f.repo.CreateSavingsContract(ctx, "bp-b", billing.CreateSavingsContractInput{
		SavingsPlanID: planID, DurationMonths: 12, MonthlyCommittedAmount: decimalOf(t, "100"),
	}, now)
	if err != nil || noUp.Status != billing.SavingsStatusActive {
		t.Fatalf("no-upfront = %+v, %v", noUp, err)
	}
}

func TestAddFundsBindsTheContract(t *testing.T) {
	f := newMoneyFixture(t)
	one := decimalOf(t, "1")

	t.Run("another profile's contract", func(t *testing.T) {
		if _, err := f.deposit(t, f.seedContract(t, "bp-other"), &one); err == nil {
			t.Fatal("accepted")
		}
	})
	t.Run("a contract already active", func(t *testing.T) {
		id := mustInsertID(t, f.db, "savingsContract", pgdoc.M{"status": "ACTIVE", "billingProfileId": "bp-a", "paidUpfront": true, "upfrontAmount": decimalOf(t, "120")})
		if _, err := f.deposit(t, id, &one); err == nil {
			t.Fatal("accepted")
		}
	})
	t.Run("the price is the contract's, not the client's", func(t *testing.T) {
		id := f.seedContract(t, "bp-a")
		resp, err := f.deposit(t, id, &one)
		if err != nil {
			t.Fatal(err)
		}
		if resp.NetAmount.String() != "120" {
			t.Fatalf("net = %s", resp.NetAmount)
		}
		if f.status(t, id) != billing.SavingsStatusPendingPayment {
			t.Fatal("a deposit alone must not activate")
		}
		if _, err := f.svc.ProcessAddFunds(context.Background(), resp.TransactionID); err != nil {
			t.Fatal(err)
		}
		if f.status(t, id) != billing.SavingsStatusActive {
			t.Fatalf("settled: status = %s", f.status(t, id))
		}
		if f.credits(t) != 0 {
			t.Fatal("a contract deposit must not mint credit")
		}
	})
	t.Run("zero and negative amounts", func(t *testing.T) {
		for _, a := range []string{"0", "-5"} {
			d := decimalOf(t, a)
			if _, err := f.deposit(t, "", &d); err == nil {
				t.Fatalf("deposit of %s accepted", a)
			}
		}
	})
}

// The contract changed between deposit and settle: the money stands as credit, the contract stays as it is.
func TestSettleNeverActivatesAContractItCannotPay(t *testing.T) {
	ctx := context.Background()
	f := newMoneyFixture(t)
	seed := func(amount string, contract string) string {
		return mustInsertID(t, f.db, "accountCreditTransaction", pgdoc.M{"status": "PENDING", "billingProfileId": "bp-a", "paymentGatewayId": "gw1",
			"externalId": "pi_fake", "currency": "USD", "amount": decimalOf(t, amount), "grossAmount": decimalOf(t, amount),
			"metadata": pgdoc.M{"savingsContractId": contract}})
	}
	cancelled := mustInsertID(t, f.db, "savingsContract", pgdoc.M{"status": "CANCELLED", "billingProfileId": "bp-a", "paidUpfront": true, "upfrontAmount": decimalOf(t, "120")})
	if _, err := f.svc.ProcessAddFunds(ctx, seed("120", cancelled)); err != nil {
		t.Fatal(err)
	}
	if f.status(t, cancelled) != "CANCELLED" {
		t.Fatal("a cancelled contract came back to life")
	}
	if f.credits(t) != 1 {
		t.Fatalf("credits = %d: the money must not vanish", f.credits(t))
	}

	wrong := f.seedContract(t, "bp-a")
	if _, err := f.svc.ProcessAddFunds(ctx, seed("1", wrong)); err != nil {
		t.Fatal(err)
	}
	if f.status(t, wrong) != billing.SavingsStatusPendingPayment {
		t.Fatal("a 1.00 deposit activated a 120.00 contract")
	}

	foreign := f.seedContract(t, "bp-other")
	if _, err := f.svc.ProcessAddFunds(ctx, seed("120", foreign)); err != nil {
		t.Fatal(err)
	}
	if f.status(t, foreign) != billing.SavingsStatusPendingPayment {
		t.Fatal("another profile's deposit activated the contract")
	}
}

// Many payers, one balance: credit that covers one bill pays one bill.
func TestConcurrentPayBillsCannotDoubleSpend(t *testing.T) {
	ctx := context.Background()
	f := newMoneyFixture(t)
	pay := billing.NewPayService(f.repo, pricing.NewRepo(f.db))
	credit := mustInsertID(t, f.db, "accountCredit", pgdoc.M{"billingProfileId": "bp-a", "currency": "USD", "invoiceCurrency": "USD",
		"amount": decimalOf(t, "10"), "initialAmount": decimalOf(t, "10"), "invoiceExchangeRate": decimalOf(t, "1")})
	bills := make([]string, 6)
	for i := range bills {
		bills[i] = mustInsertID(t, f.db, "bill", pgdoc.M{"billingProfileId": "bp-a", "status": "SENT", "invoiceCurrency": "USD",
			"items": []any{pgdoc.M{"resourceType": "instance", "currency": "USD", "netAmount": decimalOf(t, "10")}}})
	}
	now := time.Now().UTC()
	var wg sync.WaitGroup
	var paid, short atomic.Int32
	start := make(chan struct{})
	for _, id := range append(bills, bills...) { // each bill twice: also the same bill racing itself
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := pay.PayBillWithCredits(ctx, f.profile, id, now)
			switch {
			case err == nil:
				paid.Add(1)
			case errors.Is(err, billing.ErrNotEnoughCredit), errors.Is(err, billing.ErrBillAlreadyPaid):
				short.Add(1)
			default:
				t.Errorf("pay: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if paid.Load() != 1 {
		t.Fatalf("%d payments went through on credit for one bill", paid.Load())
	}
	ac, err := f.repo.AccountCreditByID(ctx, credit)
	if err != nil || ac == nil || !ac.Amount.IsZero() {
		t.Fatalf("credit left = %+v, %v", ac, err)
	}
}

func TestRefundRefusesSpentCredit(t *testing.T) {
	ctx := context.Background()
	f := newMoneyFixture(t)
	seed := func(left string) string {
		txn := pgdoc.NewID()
		mustInsert(t, f.db, "accountCredit", pgdoc.M{"_id": txn, "billingProfileId": "bp-a", "currency": "USD", "amount": decimalOf(t, left), "initialAmount": decimalOf(t, "100")})
		mustInsert(t, f.db, "accountCreditTransaction", pgdoc.M{"_id": txn, "status": "SUCCESS", "billingProfileId": "bp-a", "paymentGatewayId": "gw1",
			"externalId": "pi_fake", "currency": "USD", "amount": decimalOf(t, "100"), "accountCredit": pgdoc.M{"id": txn}})
		return txn
	}
	spent := seed("40")
	if _, err := f.svc.RefundFunds(ctx, spent); err == nil {
		t.Fatal("refunded a deposit 60% spent")
	}
	if f.gw.refunds.Load() != 0 {
		t.Fatal("the gateway was called")
	}
	txn, _ := f.repo.AccountCreditTransactionByID(ctx, spent)
	if txn.Status != "SUCCESS" {
		t.Fatalf("status = %s", txn.Status)
	}

	unspent := seed("100")
	got, err := f.svc.RefundFunds(ctx, unspent)
	if err != nil || got.Status != "REFUNDED" || f.gw.refunds.Load() != 1 {
		t.Fatalf("unspent: %+v, %v, %d refunds", got, err, f.gw.refunds.Load())
	}

	// A contract still running on the deposit: its discount is what the money bought.
	running := pgdoc.NewID()
	contract := mustInsertID(t, f.db, "savingsContract", pgdoc.M{"status": "ACTIVE", "billingProfileId": "bp-a", "paidUpfront": true, "paymentTransactionId": running})
	mustInsert(t, f.db, "accountCreditTransaction", pgdoc.M{"_id": running, "status": "SUCCESS", "billingProfileId": "bp-a", "paymentGatewayId": "gw1",
		"externalId": "pi_fake", "currency": "USD", "amount": decimalOf(t, "100"), "metadata": pgdoc.M{"savingsContractId": contract}})
	if _, err := f.svc.RefundFunds(ctx, running); err == nil {
		t.Fatal("refunded the deposit of a running contract")
	}
}

// A settle that failed after minting, or died holding the claim: the retry finds the same credit.
func TestDepositSettleIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newMoneyFixture(t)
	seed := func(status string, updated time.Time) string {
		return mustInsertID(t, f.db, "accountCreditTransaction", pgdoc.M{"status": status, "billingProfileId": "bp-a", "paymentGatewayId": "gw1",
			"externalId": "pi_fake", "currency": "USD", "amount": decimalOf(t, "10"), "grossAmount": decimalOf(t, "10"),
			"createdAt": updated, "updatedAt": updated, "metadata": pgdoc.M{}})
	}
	id := seed("PENDING", time.Now().UTC())
	if _, err := f.svc.ProcessAddFunds(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.C("accountCreditTransaction").SetByID(ctx, id, pgdoc.M{"status": "PENDING"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ProcessAddFunds(ctx, id); err != nil {
		t.Fatal(err)
	}
	if n := f.credits(t); n != 1 {
		t.Fatalf("%d credits for one deposit", n)
	}

	// The scanner picks up what a dead pod left PROCESSING. A fresh claim is left alone.
	scanner := payment.NewTransactionScanner(f.repo, f.svc, nil, nil)
	stuck := seed("PROCESSING", time.Now().UTC().Add(-time.Hour))
	inFlight := seed("PROCESSING", time.Now().UTC())
	if _, err := scanner.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if txn, _ := f.repo.AccountCreditTransactionByID(ctx, stuck); txn.Status != "SUCCESS" {
		t.Fatalf("stuck txn = %s", txn.Status)
	}
	if txn, _ := f.repo.AccountCreditTransactionByID(ctx, inFlight); txn.Status != "PROCESSING" {
		t.Fatalf("in-flight txn = %s", txn.Status)
	}
	if n := f.credits(t); n != 2 {
		t.Fatalf("%d credits", n)
	}
}

func TestBankTransferResolvesOnce(t *testing.T) {
	ctx := context.Background()
	f := newMoneyFixture(t)
	id := mustInsertID(t, f.db, "bankTransfer", pgdoc.M{"status": "PENDING", "billingProfileId": "bp-a"})
	won, err := f.repo.ResolveBankTransfer(ctx, id, pgdoc.M{"status": "APPROVED"})
	if err != nil || !won {
		t.Fatalf("approve: %v, %v", won, err)
	}
	if won, _ := f.repo.ResolveBankTransfer(ctx, id, pgdoc.M{"status": "REJECTED"}); won {
		t.Fatal("rejected an approved transfer")
	}
	bt, _ := f.repo.BankTransferByID(ctx, id)
	if bt["status"] != "APPROVED" {
		t.Fatalf("status = %v", bt["status"])
	}

	// A settled deposit does not go FAILED, which would keep the credit and lose the record.
	txn := mustInsertID(t, f.db, "accountCreditTransaction", pgdoc.M{"status": "SUCCESS", "billingProfileId": "bp-a", "currency": "USD", "amount": decimalOf(t, "10")})
	if _, err := f.svc.ProcessBankTransfer(ctx, txn, "REJECTED", "no"); err == nil {
		t.Fatal("rejected a settled deposit")
	}
	if cur, _ := f.repo.AccountCreditTransactionByID(ctx, txn); cur.Status != "SUCCESS" {
		t.Fatalf("status = %s", cur.Status)
	}
}

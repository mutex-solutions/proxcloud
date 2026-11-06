package billing

import (
	"context"
	"errors"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// PayService pays a SENT bill from the profile's
// credit balance (account + promotional credits), NO external gateway. Errors are sentinels
// the handler maps to 400 messages.
type PayService struct {
	repo     *Repo
	pricing  *pricing.Repo // tax rates for the account-credit gross legs (nil-safe → no tax)
	notifier Notifier
	reviewer ProfileReviewer
	billLink BillLinkFunc // {{billUrl}} in the bill_is_paid mail (nil → "")
}

// ProfileReviewer re-evaluates a profile's suspension after its balance changes.
// *SuspensionJob implements it.
//
// EXTRACTION SEAM: unlike the other three hooks this one does NOT become a network call — both
// sides (PayService and SuspensionJob) move into the billing service together, so it stays an
// in-process call. That is why it can keep taking a *BillingProfile rather than an id.
type ProfileReviewer interface {
	ReviewBillingProfile(ctx context.Context, profile *BillingProfile) error
}

func NewPayService(repo *Repo, pricingRepo *pricing.Repo) *PayService {
	return &PayService{repo: repo, pricing: pricingRepo}
}

// SetNotifier wires the email hook (bill-paid notification). Nil → no-op.
func (s *PayService) SetNotifier(n Notifier) { s.notifier = n }

// SetReviewer wires the suspension auto-resume hook (called after a bill flips PAID). Nil → no-op.
func (s *PayService) SetReviewer(r ProfileReviewer) { s.reviewer = r }

// SetBillLink wires the console deep-link resolver for {{billUrl}}. Nil → empty link.
func (s *PayService) SetBillLink(fn BillLinkFunc) { s.billLink = fn }

// Sentinel errors (the canonical messages).
var (
	ErrBillNotFound      = errors.New("Bill not found")
	ErrBillAlreadyPaid   = errors.New("Bill is already paid")
	ErrCannotPayOpenBill = errors.New("Cannot pay an open bill")
	ErrNotEnoughCredit   = errors.New("Not enough credit")
	ErrBillNotIssued     = errors.New("bill is not issued yet")
	ErrInvalidAmount     = errors.New("amount must be positive")
)

// PayBillWithCredits pays a bill from the credit balance:
// the bill must be SENT (PAID → already-paid, OPEN → cannot-pay-open); the credit balance must
// cover the FULL unpaid amount (no partial); settle promos-first-then-account-credits, persist
// the consumed credits + bill, and flip → PAID when nothing is left unpaid. The mail + the
// suspension review (side-effects) are out of scope for this slice (see TODO).
func (s *PayService) PayBillWithCredits(ctx context.Context, profile *BillingProfile, billID string, now time.Time) (*pricing.Bill, error) {
	// Read before the lock's transaction: a failed read would abort it, and these tolerate failure.
	baseCurrency, _ := s.repo.BaseCurrency(ctx)
	var rates []pricing.TaxRate
	if s.pricing != nil {
		all, _ := s.pricing.AllTaxRates(ctx)
		rates = pricing.SelectTaxRates(all, profile.Country, profile.Company, now)
	}
	var bill *pricing.Bill
	paid := false
	err := s.repo.WithProfileLock(ctx, profile.ID, func(ctx context.Context) error {
		var err error
		bill, err = s.repo.BillByID(ctx, billID)
		if err != nil {
			return err
		}
		if bill == nil {
			return ErrBillNotFound
		}
		// Owner guard (mirrors the by-id lookup's owner check): a bill of another profile
		// must be invisible on the pay path, else a member of profile A could settle profile B's bill.
		if !billBelongsToProfile(bill, profile.ID) {
			return ErrBillNotFound
		}
		switch bill.Status {
		case pricing.BillStatusPaid:
			return ErrBillAlreadyPaid
		case pricing.BillStatusOpen:
			return ErrCannotPayOpenBill
		}

		x := pricing.NewExchanger(nil) // same-currency identity; live FX deferred

		promoCredits, err := s.repo.PromotionalCreditCandidates(ctx, profile.ID, now)
		if err != nil {
			return err
		}
		accountCredits, err := s.repo.AccountCreditsByProfile(ctx, profile.ID)
		if err != nil {
			return err
		}

		// unpaid amount converted into the product currency.
		unpaid, err := x.ExchangeToProductCurrency(pricing.GetUnpaidAmountBillProductCurrency(bill), bill.InvoiceCurrency, baseCurrency, now)
		if err != nil {
			return err
		}
		if creditBalance(promoCredits, accountCredits).Cmp(unpaid) < 0 {
			return ErrNotEnoughCredit
		}

		appliedPromo, acctSettlements, err := pricing.SettleBillAmount(bill, unpaid, promoCredits, accountCredits, rates, baseCurrency, profile.Currency, x, now)
		if err != nil {
			return err
		}

		// persist the consumed credits (their balances were mutated in place by the settlement)
		touched := map[string]bool{}
		for _, ap := range appliedPromo {
			touched[ap.PromotionalCreditID] = true
		}
		for _, pc := range promoCredits {
			if touched[pc.ID] {
				if err := s.repo.SavePromotionalCredit(ctx, pc); err != nil {
					return err
				}
			}
		}
		for _, as := range acctSettlements {
			if as.AccountCredit != nil {
				if err := s.repo.SaveAccountCredit(ctx, as.AccountCredit); err != nil {
					return err
				}
			}
		}

		if pricing.GetUnpaidAmountBillProductCurrency(bill).Cmp(decimal.Zero) <= 0 {
			bill.Status = pricing.BillStatusPaid
			paid = true
		}
		return s.repo.SaveBillDoc(ctx, bill)
	})
	if err != nil {
		return nil, err
	}
	if paid {
		s.afterBillPaid(ctx, profile, bill)
	}
	return bill, nil
}

// afterBillPaid runs the side-effects of a bill flipping PAID: the bill_is_paid mail with the
// invoice PDF and the suspension re-review → auto-resume a SUSPENDED profile once its balance
// clears. Both best-effort (a failure must not fail the pay).
func (s *PayService) afterBillPaid(ctx context.Context, profile *BillingProfile, bill *pricing.Bill) {
	NotifyBillPaid(ctx, s.notifier, s.repo, s.pricing, s.billLink, profile, bill)
	if s.reviewer != nil {
		_ = s.reviewer.ReviewBillingProfile(ctx, profile)
	}
}

// ManualPayment is an offline payment an operator confirms against a SENT bill (a bank
// transfer that showed up on the statement, cash, …). Amount is the GROSS amount received in
// the bill's invoice (profile) currency — what the bank statement shows; nil → the bill's
// full unpaid gross amount. PaidAt zero → now.
type ManualPayment struct {
	Amount     *decimal.Decimal
	Method     string // BANK_TRANSFER | OTHER
	Reference  string // free text (bank reference, receipt number)
	PaidAt     time.Time
	RecordedBy string // operator sub / key id (audit trail on the transaction metadata)
}

// RecordManualPayment records an offline payment on a SENT bill: a SUCCESS CollectTransaction
// (paymentGatewayId = the BankTransfer integration when one is configured) is applied to the bill
// through the golden settle helper so CollectedAmounts is recorded; when nothing is left unpaid
// the bill flips PAID with the same mail + suspension review as PayBillWithCredits. A partial
// amount leaves the bill SENT with the collected amount on it.
//
// The received gross maps back to the net product-currency amount the bill tracks
// proportionally: net = unpaidNet × received ÷ unpaidGross — exact for the full-payment
// default, and an overpayment still settles the bill.
// ponytail: proportional gross→net; per-rate reverse tax if mixed-rate bills ever need cent-exact partials.
func (s *PayService) RecordManualPayment(ctx context.Context, billID string, p ManualPayment) (*pricing.Bill, *pricing.CollectTransaction, error) {
	bill, err := s.repo.BillByID(ctx, billID)
	if err != nil {
		return nil, nil, err
	}
	if bill == nil {
		return nil, nil, ErrBillNotFound
	}
	switch bill.Status {
	case pricing.BillStatusPaid:
		return nil, nil, ErrBillAlreadyPaid
	case pricing.BillStatusOpen:
		return nil, nil, ErrBillNotIssued
	}
	now := time.Now().UTC()
	if p.PaidAt.IsZero() {
		p.PaidAt = now
	}
	baseCurrency, _ := s.repo.BaseCurrency(ctx)
	profile, err := s.repo.FindByID(ctx, bill.BillingProfileID)
	if err != nil {
		return nil, nil, err
	}
	if profile == nil { // orphan bill: still settle it, in the base currency
		profile = &BillingProfile{ID: bill.BillingProfileID, Currency: baseCurrency}
	}
	if profile.Currency == "" {
		profile.Currency = baseCurrency
	}
	var rates []pricing.TaxRate
	if s.pricing != nil {
		all, _ := s.pricing.AllTaxRates(ctx)
		rates = pricing.SelectTaxRates(all, profile.Country, profile.Company, now)
	}
	rate, err := pricing.NewExchanger(nil).GetExchangeRate(baseCurrency, profile.Currency, now)
	if err != nil {
		return nil, nil, err
	}
	unpaidNet := pricing.GetUnpaidAmountBillProductCurrency(bill)
	unpaidGross := rate.Mul(pricing.CalculateGrossAmount(unpaidNet, rates))

	received := unpaidGross
	if p.Amount != nil {
		received = *p.Amount
	}
	if !received.IsPositive() {
		return nil, nil, ErrInvalidAmount
	}
	netBase := received
	if unpaidGross.IsPositive() {
		netBase = unpaidNet.Mul(received).Div(unpaidGross).Round(2)
	}
	amount := rate.Mul(netBase) // net in the profile currency (ApplyPaidCollectOnBill converts it back)

	gatewayID, err := s.repo.BankTransferGatewayID(ctx)
	if err != nil {
		return nil, nil, err
	}
	paidAt := p.PaidAt.UTC()
	txn := &pricing.CollectTransaction{
		BillID:           bill.ID,
		BillingProfileID: bill.BillingProfileID,
		Currency:         profile.Currency,
		Amount:           &amount,
		GrossAmount:      &received,
		ExchangeRate:     &rate,
		PaymentGatewayID: gatewayID,
		Status:           pricing.CollectTransactionStatusSuccess,
		Metadata: map[string]any{
			"manual": true, "method": p.Method, "reference": p.Reference,
			"paidAt": paidAt.Format(time.RFC3339), "recordedBy": p.RecordedBy,
		},
		CreatedAt: &paidAt,
	}
	if txn, err = s.repo.SaveCollectTransaction(ctx, txn); err != nil {
		return nil, nil, err
	}
	pricing.ApplyPaidCollectOnBill(bill, txn, baseCurrency)
	if err := s.repo.SaveBillDoc(ctx, bill); err != nil {
		return nil, nil, err
	}
	if bill.Status == pricing.BillStatusPaid {
		s.afterBillPaid(ctx, profile, bill)
	}
	return bill, txn, nil
}

// billBelongsToProfile is the pay-path owner guard (mirrors the by-id lookup): a bill of another
// profile is invisible → ErrBillNotFound, so a member of one profile can't settle another's bill.
func billBelongsToProfile(bill *pricing.Bill, profileID string) bool {
	return bill != nil && bill.BillingProfileID == profileID
}

// creditBalance sums credit + promotional credit: Σ remaining
// promotional credit + Σ account-credit balance (base currency; the multi-currency
// account-credit conversion is a refinement — exact for the same-currency case).
func creditBalance(promo []*pricing.PromotionalCredit, account []*pricing.AccountCredit) decimal.Decimal {
	total := decimal.Zero
	for _, pc := range promo {
		if pc.RemainingAmount != nil {
			total = total.Add(*pc.RemainingAmount)
		}
	}
	for _, ac := range account {
		if ac.Amount != nil {
			total = total.Add(*ac.Amount)
		}
	}
	return total
}

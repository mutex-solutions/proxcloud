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

// Sentinel errors (the canonical messages).
var (
	ErrBillNotFound      = errors.New("Bill not found")
	ErrBillAlreadyPaid   = errors.New("Bill is already paid")
	ErrCannotPayOpenBill = errors.New("Cannot pay an open bill")
	ErrNotEnoughCredit   = errors.New("Not enough credit")
)

// PayBillWithCredits pays a bill from the credit balance:
// the bill must be SENT (PAID → already-paid, OPEN → cannot-pay-open); the credit balance must
// cover the FULL unpaid amount (no partial); settle promos-first-then-account-credits, persist
// the consumed credits + bill, and flip → PAID when nothing is left unpaid. The mail + the
// suspension review (side-effects) are out of scope for this slice (see TODO).
func (s *PayService) PayBillWithCredits(ctx context.Context, profile *BillingProfile, billID string, now time.Time) (*pricing.Bill, error) {
	bill, err := s.repo.BillByID(ctx, billID)
	if err != nil {
		return nil, err
	}
	if bill == nil {
		return nil, ErrBillNotFound
	}
	// Owner guard (mirrors the by-id lookup's owner check): a bill of another profile
	// must be invisible on the pay path, else a member of profile A could settle profile B's bill.
	if !billBelongsToProfile(bill, profile.ID) {
		return nil, ErrBillNotFound
	}
	switch bill.Status {
	case pricing.BillStatusPaid:
		return nil, ErrBillAlreadyPaid
	case pricing.BillStatusOpen:
		return nil, ErrCannotPayOpenBill
	}

	baseCurrency, _ := s.repo.BaseCurrency(ctx)
	x := pricing.NewExchanger(nil) // same-currency identity; live FX deferred

	promoCredits, err := s.repo.PromotionalCreditCandidates(ctx, profile.ID, now)
	if err != nil {
		return nil, err
	}
	accountCredits, err := s.repo.AccountCreditsByProfile(ctx, profile.ID)
	if err != nil {
		return nil, err
	}

	// unpaid amount converted into the product currency.
	unpaid, err := x.ExchangeToProductCurrency(pricing.GetUnpaidAmountBillProductCurrency(bill), bill.InvoiceCurrency, baseCurrency, now)
	if err != nil {
		return nil, err
	}
	if creditBalance(promoCredits, accountCredits).Cmp(unpaid) < 0 {
		return nil, ErrNotEnoughCredit
	}

	var rates []pricing.TaxRate
	if s.pricing != nil {
		all, _ := s.pricing.AllTaxRates(ctx)
		rates = pricing.SelectTaxRates(all, profile.Country, profile.Company, now)
	}

	appliedPromo, acctSettlements, err := pricing.SettleBillAmount(bill, unpaid, promoCredits, accountCredits, rates, baseCurrency, profile.Currency, x, now)
	if err != nil {
		return nil, err
	}

	// persist the consumed credits (their balances were mutated in place by the settlement)
	touched := map[string]bool{}
	for _, ap := range appliedPromo {
		touched[ap.PromotionalCreditID] = true
	}
	for _, pc := range promoCredits {
		if touched[pc.ID] {
			if err := s.repo.SavePromotionalCredit(ctx, pc); err != nil {
				return nil, err
			}
		}
	}
	for _, as := range acctSettlements {
		if as.AccountCredit != nil {
			if err := s.repo.SaveAccountCredit(ctx, as.AccountCredit); err != nil {
				return nil, err
			}
		}
	}

	if err := s.repo.SaveBillDoc(ctx, bill); err != nil {
		return nil, err
	}
	if pricing.GetUnpaidAmountBillProductCurrency(bill).Cmp(decimal.Zero) <= 0 {
		bill.Status = pricing.BillStatusPaid
		if err := s.repo.SaveBillDoc(ctx, bill); err != nil {
			return nil, err
		}
		vars := map[string]any{"fullName": profileFullName(profile)}
		if bill.BillingCycle != nil {
			if bill.BillingCycle.StartDate != nil {
				vars["startDate"] = bill.BillingCycle.StartDate.Format("2006-01-02")
			}
			if bill.BillingCycle.EndDate != nil {
				vars["endDate"] = bill.BillingCycle.EndDate.Format("2006-01-02")
			}
		}
		notify(ctx, s.notifier, "bill_is_paid", profile.Email, vars)
		// re-review suspension after a bill is paid → auto-resume a
		// SUSPENDED profile once its balance clears. Best-effort (a review failure must not fail the pay).
		if s.reviewer != nil {
			_ = s.reviewer.ReviewBillingProfile(ctx, profile)
		}
	}
	return bill, nil
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

package billing

import (
	"context"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// BalanceService provides the balance/due-bill reads the
// suspension/dunning flow needs. Money is shopspring decimal end to end; scaleTotal =
// setScale(4, HALF_UP) (decimal.Round(4) rounds half away
// from zero, matching HALF_UP for both signs).
type BalanceService struct{ repo *Repo }

func NewBalanceService(repo *Repo) *BalanceService { return &BalanceService{repo: repo} }

func scaleTotal(d decimal.Decimal) decimal.Decimal { return d.Round(4) }

// CurrentBalance computes the current balance:
// scaleTotal( accountCreditTotal + availablePromotionalTotal − Σ scaleTotal(unpaidAmount) )
// over the profile's SENT/OPEN bills. A positive balance = credit on hand; negative = owed.
func (s *BalanceService) CurrentBalance(ctx context.Context, bpID string, now time.Time) (decimal.Decimal, error) {
	acct, err := s.repo.AccountCreditTotal(ctx, bpID)
	if err != nil {
		return decimal.Zero, err
	}
	promo, err := s.repo.AvailablePromotionalTotal(ctx, bpID, now)
	if err != nil {
		return decimal.Zero, err
	}
	bills, err := s.repo.BillsByBillingProfile(ctx, bpID)
	if err != nil {
		return decimal.Zero, err
	}
	billTotals := decimal.Zero
	for i := range bills {
		b := &bills[i]
		if b.Status == pricing.BillStatusSent || b.Status == pricing.BillStatusOpen {
			billTotals = billTotals.Add(scaleTotal(pricing.GetUnpaidAmountBillProductCurrency(b)))
		}
	}
	return scaleTotal(acct.Add(promo).Sub(billTotals)), nil
}

// CurrentDue computes the due amount:
// Σ scaleTotal(unpaidAmount) over the profile's SENT/OPEN bills (the same set CurrentBalance
// subtracts, so balance = credits − due reconciles exactly).
func (s *BalanceService) CurrentDue(ctx context.Context, bpID string) (decimal.Decimal, error) {
	bills, err := s.repo.BillsByBillingProfile(ctx, bpID)
	if err != nil {
		return decimal.Zero, err
	}
	due := decimal.Zero
	for i := range bills {
		b := &bills[i]
		if b.Status == pricing.BillStatusSent || b.Status == pricing.BillStatusOpen {
			due = due.Add(scaleTotal(pricing.GetUnpaidAmountBillProductCurrency(b)))
		}
	}
	return scaleTotal(due), nil
}

// DueBills returns the due bills: SENT bills whose dueAt is set and at or before
// now. Returns their dueAt instants (the suspension decision layer consumes dueAts).
func (s *BalanceService) DueBills(ctx context.Context, bpID string, now time.Time) ([]time.Time, error) {
	bills, err := s.repo.BillsByBillingProfile(ctx, bpID)
	if err != nil {
		return nil, err
	}
	out := []time.Time{}
	for i := range bills {
		b := &bills[i]
		if b.Status == pricing.BillStatusSent && b.DueAt != nil && !b.DueAt.After(now) {
			out = append(out, *b.DueAt)
		}
	}
	return out, nil
}

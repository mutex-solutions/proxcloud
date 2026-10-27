package billing

import (
	"context"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// sendbill.go handles sendBill + the monthlyBill cron (sendBills): finalize
// each profile's PREVIOUS-month OPEN bill — delete-if-zero, skip SKIP profiles, else scale up, settle
// against the credit balance, and flip OPEN→PAID (fully covered) or OPEN→SENT (+dueAt 3 days). This is
// the keystone the charge cron's OPEN bills depend on: without it they never become SENT, so
// monthlyCollect (collects SENT) and dunning (DueBills = SENT) have nothing to act on.
//
// NOTE: the savings-contract bill adjustment applied in sendBill (savingsContractAdjustmentsService
// .addBillAdjustments) is the separate Theme-B charge-adjustment item, not yet wired here.
type BillSendService struct {
	repo     *Repo
	pricing  *pricing.Repo // tax rates for the settle legs (nil-safe → no tax)
	notifier Notifier
}

func NewBillSendService(repo *Repo, pricingRepo *pricing.Repo) *BillSendService {
	return &BillSendService{repo: repo, pricing: pricingRepo}
}

// SetNotifier wires the bill-generated/paid email hook. Nil → no-op.
func (s *BillSendService) SetNotifier(n Notifier) { s.notifier = n }

// SendAllBills finalizes every profile's last-month OPEN bill. Best-effort per profile (a single
// failure does not abort the run). Returns the number of bills finalized (SENT or PAID).
func (s *BillSendService) SendAllBills(ctx context.Context, now time.Time) (int, error) {
	profiles, err := s.repo.AllBillingProfiles(ctx)
	if err != nil {
		return 0, err
	}
	done := 0
	for i := range profiles {
		ok, _ := s.SendBillForProfile(ctx, &profiles[i], now)
		if ok {
			done++
		}
	}
	return done, nil
}

// SendBillForProfile sends the profile's bill: finalize the profile's previous-month
// OPEN bill. Returns whether a bill was finalized.
func (s *BillSendService) SendBillForProfile(ctx context.Context, profile *BillingProfile, now time.Time) (bool, error) {
	bill, err := s.lastMonthOpenBill(ctx, profile.ID, now)
	if err != nil || bill == nil {
		return false, err
	}
	// delete-if-zero: total amount 0 AND no positive item (an empty/zero accrual bill).
	positive := false
	for i := range bill.Items {
		if bill.Items[i].NetAmount.IsPositive() {
			positive = true
			break
		}
	}
	if bill.CalculateTotalAmount().IsZero() && !positive {
		return false, s.repo.DeleteBill(ctx, bill.ID)
	}
	if profile.Status == StatusSkip {
		return false, nil // SKIP profiles' invoices are not sent
	}
	return true, s.sendBill(ctx, profile, bill, now)
}

// sendBill finalizes one bill: scale up, settle against credits, then
// flip OPEN→PAID (fully covered) or OPEN→SENT (+dueAt = sentAt + 3 days).
func (s *BillSendService) sendBill(ctx context.Context, profile *BillingProfile, bill *pricing.Bill, now time.Time) error {
	bill.InvoiceCurrency = profile.Currency
	pricing.ScaleUpItems(bill)

	grossTotal := pricing.GetUnpaidAmountBillProductCurrency(bill)
	if grossTotal.IsZero() {
		bill.Status = pricing.BillStatusPaid
		bill.SentAt = &now
		return s.repo.SaveBillDoc(ctx, bill)
	}

	// Settle the gross against the profile's credit balance (settleBillAmount); persist the
	// consumed credits, then decide PAID vs SENT on the remaining unpaid amount.
	baseCcy, _ := s.repo.BaseCurrency(ctx)
	x := pricing.NewExchanger(nil)
	promoCredits, err := s.repo.PromotionalCreditCandidates(ctx, profile.ID, now)
	if err != nil {
		return err
	}
	accountCredits, err := s.repo.AccountCreditsByProfile(ctx, profile.ID)
	if err != nil {
		return err
	}
	var rates []pricing.TaxRate
	if s.pricing != nil {
		all, _ := s.pricing.AllTaxRates(ctx)
		rates = pricing.SelectTaxRates(all, profile.Country, profile.Company, now)
	}
	appliedPromo, acctSettlements, err := pricing.SettleBillAmount(bill, grossTotal, promoCredits, accountCredits, rates, baseCcy, profile.Currency, x, now)
	if err != nil {
		return err
	}
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

	vars := map[string]any{"fullName": profileFullName(profile)}
	if pricing.GetUnpaidAmountBillProductCurrency(bill).Cmp(decimal.Zero) <= 0 {
		bill.Status = pricing.BillStatusPaid
		notify(ctx, s.notifier, "bill_is_paid", profile.Email, vars)
	} else {
		bill.Status = pricing.BillStatusSent
		bill.SentAt = &now
		due := now.Add(259200 * time.Second) // sentAt + 259200s (3 days)
		bill.DueAt = &due
		notify(ctx, s.notifier, "bill_is_generated", profile.Email, vars)
	}
	return s.repo.SaveBillDoc(ctx, bill)
}

// lastMonthOpenBill returns the last-month OPEN bill: the OPEN bill whose billing cycle started
// in the previous calendar month (nil if none).
func (s *BillSendService) lastMonthOpenBill(ctx context.Context, bpID string, now time.Time) (*pricing.Bill, error) {
	bills, err := s.repo.BillsByBillingProfile(ctx, bpID)
	if err != nil {
		return nil, err
	}
	lastT := now.AddDate(0, -1, 0)
	ly, lm, _ := lastT.Date()
	for i := range bills {
		b := &bills[i]
		if b.Status != pricing.BillStatusOpen {
			continue
		}
		cs := now
		if b.BillingCycle != nil && b.BillingCycle.StartDate != nil {
			cs = b.BillingCycle.StartDate.UTC()
		}
		y, m, _ := cs.Date()
		if y == ly && m == lm {
			return b, nil
		}
	}
	return nil, nil
}

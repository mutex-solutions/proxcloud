package billing

import (
	"context"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// sendbill.go handles sendBill + the monthlyBill cron (sendBills): finalize
// each profile's PREVIOUS-month OPEN bill — delete-if-zero, skip SKIP profiles, else scale up, settle
// against the credit balance, and flip OPEN→PAID (fully covered) or OPEN→SENT (+dueAt 7 days). This is
// the linchpin the charge cron's OPEN bills depend on: without it they never become SENT, so
// monthlyCollect (collects SENT) and dunning (DueBills = SENT) have nothing to act on.
//
// NOTE: the savings-contract bill adjustment applied in sendBill (savingsContractAdjustmentsService
// .addBillAdjustments) is the separate Theme-B charge-adjustment item, not yet wired here.
type BillSendService struct {
	repo        *Repo
	pricing     *pricing.Repo // tax rates for the settle legs (nil-safe → no tax)
	notifier    Notifier
	billLink    BillLinkFunc  // {{billUrl}} in the bill mails (nil → "")
	autoCollect AutoCollector // charge the card right after the bill is issued (chargesAtIssue; nil → cron only)
}

// AutoCollector charges a profile's SENT bills on its current card — wired from cmd/api
// to payment.CollectService.CollectBillingProfile (the same code the monthlyCollect cron runs, so
// a failure is e-mailed and retried the same way). Best-effort: the bill is already issued; the bill
// mail waits for the charge's outcome.
type AutoCollector func(ctx context.Context, profile *BillingProfile) error

// SetAutoCollector wires the pay-as-you-go charge hook. Nil → bills wait for the collect cron.
func (s *BillSendService) SetAutoCollector(fn AutoCollector) { s.autoCollect = fn }

func NewBillSendService(repo *Repo, pricingRepo *pricing.Repo) *BillSendService {
	return &BillSendService{repo: repo, pricing: pricingRepo}
}

// SetNotifier wires the bill-generated/paid email hook. Nil → no-op.
func (s *BillSendService) SetNotifier(n Notifier) { s.notifier = n }

// SetBillLink wires the console deep-link resolver for {{billUrl}}. Nil → empty link.
func (s *BillSendService) SetBillLink(fn BillLinkFunc) { s.billLink = fn }

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
// flip OPEN→PAID (fully covered) or OPEN→SENT (+dueAt = sentAt + 7 days).
func (s *BillSendService) sendBill(ctx context.Context, profile *BillingProfile, bill *pricing.Bill, now time.Time) error {
	bill.InvoiceCurrency = profile.Currency
	pricing.ScaleUpItems(bill)

	grossTotal := pricing.GetUnpaidAmountBillProductCurrency(bill)
	if grossTotal.IsZero() {
		bill.Status = pricing.BillStatusPaid
		bill.SentAt = &now
		if err := s.stampInvoiceNumber(ctx, bill, now); err != nil {
			return err
		}
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
	var rates, allRates []pricing.TaxRate
	if s.pricing != nil {
		allRates, _ = s.pricing.AllTaxRates(ctx)
		rates = pricing.SelectTaxRates(allRates, profile.Country, profile.Company, now)
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

	// The bill is issued now either way (sentAt = the invoice issue date); the number is stamped
	// before the mail so the template can print it.
	bill.SentAt = &now
	if err := s.stampInvoiceNumber(ctx, bill, now); err != nil {
		return err
	}
	key := "bill_is_paid"
	if pricing.GetUnpaidAmountBillProductCurrency(bill).Cmp(decimal.Zero) <= 0 {
		bill.Status = pricing.BillStatusPaid
	} else {
		bill.Status = pricing.BillStatusSent
		due := now.AddDate(0, 0, 7) // sentAt + 7 days
		bill.DueAt = &due
		key = "bill_is_generated"
	}
	if err := s.repo.SaveBillDoc(ctx, bill); err != nil {
		return err
	}
	// The remainder is charged to the saved card BEFORE the mail (chargesAtIssue), so the customer
	// gets one mail for the outcome, never a bank-transfer request for a bill the card pays later.
	// The bill is persisted SENT first, so a gateway failure leaves a normal unpaid bill for the cron
	// + the customer. Settled → the collect already mailed bill_is_paid; outcome unknown (PENDING) →
	// the scanner mails it once known; declined / no card → bill_is_generated below.
	if bill.Status == pricing.BillStatusSent && s.autoCollect != nil && s.chargesAtIssue(ctx, profile, now) {
		_ = s.autoCollect(ctx, profile)
		if fresh, _ := s.repo.BillByID(ctx, bill.ID); fresh != nil {
			bill = fresh
		}
		if bill.Status == pricing.BillStatusPaid {
			return nil
		}
		// The bill was issued just now, so any PENDING charge on it is this one.
		if pending, _ := s.repo.PendingCollectForBill(ctx, bill.ID, time.Time{}); len(pending) > 0 {
			return nil
		}
	}
	// Vars are built after the status/dueAt decision so the mail prints the due date, the unpaid
	// gross, the bank details and the console link the customer needs to pay without logging in.
	// The invoice PDF rides along (same document as the console download); a render failure only
	// drops the attachment — a notification must never break the billing state machine.
	notifyWith(ctx, s.notifier, key, profile, billMailVarsFull(ctx, s.repo, s.billLink, profile, bill, rates), invoiceAttachment(ctx, s.repo, s.pricing, profile, bill, now))
	return nil
}

// chargesAtIssue reports whether the freshly issued remainder is charged right away: always for a
// POSTPAID profile (no card → the collect's no-card mail); for a PREPAID one only with a usable saved
// card — the collect cron would charge that card a few hours later anyway.
func (s *BillSendService) chargesAtIssue(ctx context.Context, profile *BillingProfile, now time.Time) bool {
	if profile.IsPostpaid() {
		return true
	}
	card, _ := s.repo.CurrentCreditCard(ctx, profile.ID, profile.DefaultCardID, now)
	return card != nil
}

// stampInvoiceNumber assigns the sequential invoice number exactly once — on the transition out
// of OPEN. An already-numbered bill is never renumbered.
func (s *BillSendService) stampInvoiceNumber(ctx context.Context, bill *pricing.Bill, now time.Time) error {
	if bill.InvoiceNumber != "" {
		return nil
	}
	n, err := s.repo.NextInvoiceNumber(ctx, now)
	if err != nil {
		return err
	}
	bill.InvoiceNumber = n
	return nil
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

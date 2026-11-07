package billing

import (
	"context"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// CreateSavingsContractInput is the client create body for a savings contract.
type CreateSavingsContractInput struct {
	SavingsPlanID          string
	DurationMonths         int
	MonthlyCommittedAmount decimal.Decimal
	PaidUpfront            bool
	StartDate              string // "CURRENT_MONTH" | "NEXT_MONTH"
}

// InsertSavingsContract inserts a new contract (SaveSavingsContract is replace-by-id; a fresh
// contract has no _id yet). Sets created/updated timestamps + the generated id.
func (r *Repo) InsertSavingsContract(ctx context.Context, c *SavingsContract) error {
	now := time.Now().UTC()
	if c.CreatedAt == nil {
		c.CreatedAt = &now
	}
	c.UpdatedAt = &now
	c.ID = ""
	id, err := r.savingCtrs.InsertOne(ctx, c)
	if err != nil {
		return err
	}
	c.ID = id
	return nil
}

// CreateSavingsContract creates a savings contract: resolve the available plan, reject a
// duplicate contract, pick the schedule by duration + the discount tier by committed amount, compute
// start/end dates, persist. No-upfront is ACTIVE at once, upfront waits for its payment
// (ActivatePaidSavingsContract). Returns httpx errors (400/404) for the guards.
func (r *Repo) CreateSavingsContract(ctx context.Context, bpID string, in CreateSavingsContractInput, now time.Time) (*SavingsContract, error) {
	if !in.MonthlyCommittedAmount.IsPositive() {
		return nil, httpx.BadRequest("The monthly committed amount must be positive")
	}
	plan, err := r.AvailableSavingsPlanByID(ctx, in.SavingsPlanID)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, httpx.NotFound("Savings plan not found")
	}
	exists, err := r.ExistsActiveSavingsContract(ctx, plan.ID, bpID)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, httpx.BadRequest("You already have a savings contract for this savings plan")
	}
	var schedule *SavingsPlanSchedule
	for i := range plan.SavingSchedule {
		if plan.SavingSchedule[i].DurationMonths == in.DurationMonths {
			schedule = &plan.SavingSchedule[i]
			break
		}
	}
	if schedule == nil {
		return nil, httpx.BadRequest("No schedule found for the given duration")
	}
	tiers := schedule.NoUpfrontTiers
	if in.PaidUpfront {
		tiers = schedule.UpfrontTiers
	}
	var discount *decimal.Decimal
	for i := range tiers {
		t := &tiers[i]
		if t.StartAmount == nil || t.Discount == nil {
			continue
		}
		if t.StartAmount.Cmp(in.MonthlyCommittedAmount) <= 0 {
			if discount == nil || t.Discount.Cmp(*discount) > 0 {
				d := *t.Discount
				discount = &d
			}
		}
	}
	if discount == nil {
		return nil, httpx.BadRequest("No savings plan found for the given monthly commited amount")
	}
	start := firstOfMonth(now, 0)
	if in.StartDate == "NEXT_MONTH" {
		start = firstOfMonth(now, 1)
	}
	end := start.AddDate(0, in.DurationMonths, 0)
	monthly := in.MonthlyCommittedAmount
	c := &SavingsContract{
		BillingProfileID: bpID, SavingsPlanID: plan.ID, Status: SavingsStatusActive,
		SavingsPlanName: plan.Name, Targets: plan.Targets, StartDate: &start, EndDate: &end,
		DurationMonths: in.DurationMonths, DiscountRate: discount, MonthlyCommittedAmount: &monthly,
		PaidUpfront: in.PaidUpfront,
	}
	if in.PaidUpfront {
		c.Status = SavingsStatusPendingPayment
		price := upfrontPrice(monthly, in.DurationMonths, *discount)
		c.UpfrontAmount = &price
	}
	if err := r.InsertSavingsContract(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// upfrontPrice is the term's commitment less the contract's discount, in the base currency.
func upfrontPrice(monthly decimal.Decimal, months int, discountRate decimal.Decimal) decimal.Decimal {
	full := monthly.Mul(decimal.NewFromInt(int64(months)))
	return full.Sub(full.Mul(discountRate).Div(decimal.NewFromInt(100))).Round(2)
}

// ActivatePaidSavingsContract flips a contract from PENDING_PAYMENT to ACTIVE, once: a concurrent
// settle or cancel loses. Reports whether this caller won.
func (r *Repo) ActivatePaidSavingsContract(ctx context.Context, c *SavingsContract, txnID string, now time.Time) (bool, error) {
	return r.activateUpfront(ctx, c, map[string]any{"paymentTransactionId": txnID}, now)
}

// RecordUpfrontPayment is ActivatePaidSavingsContract for money an operator took outside the portal: they
// answer for it, where a deposit would be the record.
func (r *Repo) RecordUpfrontPayment(ctx context.Context, c *SavingsContract, reference, recordedBy string, now time.Time) (bool, error) {
	return r.activateUpfront(ctx, c, map[string]any{"paymentReference": reference, "paymentRecordedBy": recordedBy}, now)
}

func (r *Repo) activateUpfront(ctx context.Context, c *SavingsContract, set map[string]any, now time.Time) (bool, error) {
	set["status"], set["updatedAt"] = SavingsStatusActive, now
	// A term whose start went by while the contract waited for its money starts now: the customer
	// paid for all of it.
	if c.StartDate != nil && c.StartDate.Before(firstOfMonth(now, 0)) {
		start := firstOfMonth(now, 0)
		end := start.AddDate(0, c.DurationMonths, 0)
		set["startDate"], set["endDate"] = start, end
		c.StartDate, c.EndDate = &start, &end
	}
	return r.savingCtrs.SetFieldsOne(ctx, map[string]any{"_id": c.ID, "status": SavingsStatusPendingPayment}, set, nil)
}

// HoldUnpaidUpfrontContracts moves the upfront contracts that are ACTIVE with no record of their money to
// PENDING_PAYMENT, priced, where the customer can pay them and an operator can record a payment taken
// elsewhere. Those exist because a customer's upfront purchase used to go ACTIVE at once, and they would
// otherwise discount every bill for nothing. What they already discounted stays on those bills. Runs at
// boot: a held contract no longer matches, so a second run changes nothing. Returns the ids it moved.
func (r *Repo) HoldUnpaidUpfrontContracts(ctx context.Context, now time.Time) ([]string, error) {
	contracts, err := findTyped[SavingsContract](ctx, r.savingCtrs, map[string]any{"status": SavingsStatusActive, "paidUpfront": true})
	if err != nil {
		return nil, err
	}
	held := []string{}
	for i := range contracts {
		c := &contracts[i]
		if !c.UpfrontUnpaid() || (c.EndDate != nil && c.EndDate.Before(now)) { // the expiry job closes those
			continue
		}
		set := map[string]any{"status": SavingsStatusPendingPayment, "updatedAt": now}
		if c.MonthlyCommittedAmount != nil && c.DiscountRate != nil {
			set["upfrontAmount"] = upfrontPrice(*c.MonthlyCommittedAmount, c.DurationMonths, *c.DiscountRate)
		}
		won, err := r.savingCtrs.SetFieldsOne(ctx, map[string]any{"_id": c.ID, "status": SavingsStatusActive}, set, nil)
		if err != nil {
			return held, err
		}
		if won {
			held = append(held, c.ID)
		}
	}
	return held, nil
}

// firstOfMonth returns the first day of the month addMonths from now (UTC midnight).
func firstOfMonth(now time.Time, addMonths int) time.Time {
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, addMonths, 0)
}

// SavingsService handles the cron-driven parts of the savings-contract service. It covers the
// pure state transition (expiring contracts); the notification downstream it also fires —
// cancelling pending reminders + sending the contract-expired email — belongs to the
// reminder / email subsystems, which are NOT wired (gated like the other notification jobs),
// so it is intentionally skipped here. The contract status flip is the persisted,
// billing-relevant effect (only ACTIVE contracts count toward a profile's available contracts).
type SavingsService struct {
	repo     *Repo
	now      func() time.Time
	notifier Notifier
}

func NewSavingsService(repo *Repo) *SavingsService {
	return &SavingsService{repo: repo, now: func() time.Time { return time.Now().UTC() }}
}

// SetNotifier wires the email hook (the contract-expired notification). Nil → skipped.
func (s *SavingsService) SetNotifier(n Notifier) { s.notifier = n }

// ExpireContracts flips expired contracts: every ACTIVE contract whose
// endDate is strictly before now flips to EXPIRED and is persisted. Returns the count flipped.
// Per-contract save errors stop the batch (the loop propagates).
func (s *SavingsService) ExpireContracts(ctx context.Context) (int, error) {
	now := s.now()
	contracts, err := s.repo.AllSavingsContracts(ctx)
	if err != nil {
		return 0, err
	}
	expired := 0
	for i := range contracts {
		c := &contracts[i]
		if c.Status != SavingsStatusActive || c.EndDate == nil || !c.EndDate.Before(now) {
			continue
		}
		c.Status = SavingsStatusExpired
		if err := s.repo.SaveSavingsContract(ctx, c); err != nil {
			return expired, err
		}
		// Cancel any pending expiry reminders. Best-effort.
		_ = s.repo.CancelReminders(ctx, ResourceTypeSavingsContract, c.ID)
		expired++
		s.notifyExpired(ctx, c)
	}
	return expired, nil
}

// notifyExpired sends the savings_contract_expired email (best-effort; gated → no-op when no
// notifier).
func (s *SavingsService) notifyExpired(ctx context.Context, c *SavingsContract) {
	if s.notifier == nil {
		return
	}
	profile, err := s.repo.FindByID(ctx, c.BillingProfileID)
	if err != nil || profile == nil {
		return
	}
	vars := map[string]any{
		"fullName": profileFullName(profile), "savingsPlanName": c.SavingsPlanName, "currency": profile.Currency,
	}
	if c.StartDate != nil {
		vars["startDate"] = c.StartDate.Format("2006-01-02")
	}
	if c.EndDate != nil {
		vars["endDate"] = c.EndDate.Format("2006-01-02")
	}
	if c.MonthlyCommittedAmount != nil {
		vars["monthlyCommittedAmount"] = c.MonthlyCommittedAmount.StringFixed(2)
	}
	notify(ctx, s.notifier, "savings_contract_expired", profile, vars)
}

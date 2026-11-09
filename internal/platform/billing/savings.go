package billing

import (
	"context"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"

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

func (r *Repo) activateUpfront(ctx context.Context, c *SavingsContract, set map[string]any, now time.Time) (won bool, err error) {
	var activated *SavingsContract
	err = r.withReminderContract(ctx, c.ID, func(tc context.Context, current *SavingsContract) error {
		if current == nil || current.Status != SavingsStatusPendingPayment {
			return nil
		}
		// The caller checked price/profile before its gateway work. Refuse a
		// changed eligibility snapshot; never activate a different-priced term.
		if current.BillingProfileID != c.BillingProfileID || current.PaidUpfront != c.PaidUpfront || !sameSavingsAmount(current.UpfrontAmount, c.UpfrontAmount) {
			return nil
		}
		set["status"], set["updatedAt"] = SavingsStatusActive, now
		// Rebase from LOCKED current dates/duration, not a stale caller snapshot.
		if current.StartDate != nil && current.StartDate.Before(firstOfMonth(now, 0)) {
			start := firstOfMonth(now, 0)
			end := start.AddDate(0, current.DurationMonths, 0)
			set["startDate"], set["endDate"] = start, end
		}
		var e error
		won, e = r.savingCtrs.SetFieldsOne(tc, map[string]any{"_id": current.ID, "status": SavingsStatusPendingPayment}, set, nil)
		if e != nil || !won {
			return e
		}
		if e = r.cancelReminderRowsTx(tc, current.ID, current.EndDate, now); e != nil {
			return e
		}
		if e = r.cancelSavingsExpiredTx(tc, current.ID); e != nil {
			return e
		}
		activated, e = r.SavingsContractByID(tc, current.ID)
		return e
	})
	if err != nil {
		return false, err
	}
	if won && activated != nil {
		*c = *activated
	}
	return won, nil
}

func sameSavingsAmount(a, b *decimal.Decimal) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// HoldUnpaidUpfrontContracts moves the upfront contracts that are ACTIVE with no record of their money to
// PENDING_PAYMENT, priced, where the customer can pay them and an operator can record a payment taken
// elsewhere. Those exist because a customer's upfront purchase used to go ACTIVE at once, and they would
// otherwise discount every bill for nothing. What they already discounted stays on those bills. Runs at
// boot: a held contract no longer matches, so a second run changes nothing. Returns the ids it moved.
func (r *Repo) HoldUnpaidUpfrontContracts(ctx context.Context, now time.Time) ([]string, error) {
	if err := pgdoc.RejectInheritedTransaction(ctx); err != nil {
		return nil, err
	}
	contracts, err := findTyped[SavingsContract](ctx, r.savingCtrs, map[string]any{"status": SavingsStatusActive, "paidUpfront": true})
	if err != nil {
		return nil, err
	}
	held := []string{}
	for i := range contracts {
		id := contracts[i].ID
		won := false
		err := r.withReminderContract(ctx, id, func(tc context.Context, c *SavingsContract) error {
			if c == nil || c.Status != SavingsStatusActive || !c.UpfrontUnpaid() || (c.EndDate != nil && c.EndDate.Before(now)) {
				return nil
			}
			set := map[string]any{"status": SavingsStatusPendingPayment, "updatedAt": now}
			if c.MonthlyCommittedAmount != nil && c.DiscountRate != nil {
				set["upfrontAmount"] = upfrontPrice(*c.MonthlyCommittedAmount, c.DurationMonths, *c.DiscountRate)
			}
			var e error
			won, e = r.savingCtrs.SetFieldsOne(tc, map[string]any{"_id": id, "status": SavingsStatusActive}, set, nil)
			if e != nil || !won {
				return e
			}
			if e = r.cancelReminderRowsTx(tc, id, c.EndDate, now); e != nil {
				return e
			}
			return r.cancelSavingsExpiredTx(tc, id)
		})
		if err != nil {
			return held, err
		}
		if won {
			held = append(held, id)
		}
	}
	return held, nil
}

// firstOfMonth returns the first day of the month addMonths from now (UTC midnight).
func firstOfMonth(now time.Time, addMonths int) time.Time {
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, addMonths, 0)
}

// SavingsService owns DB-only expiration decisions. Status affects future discount
// eligibility, never payment/credit/provision callbacks. A winning decision selects
// the fixed expired-notice outbox atomically when opted in, otherwise persists a
// legacy/no-mail tombstone before any postcommit best-effort notifier.
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
	if err := pgdoc.RejectInheritedTransaction(ctx); err != nil {
		return 0, err
	}
	now := s.now()
	contracts, err := s.repo.AllSavingsContracts(ctx)
	if err != nil {
		return 0, err
	}
	expired := 0
	for i := range contracts {
		var changed *SavingsContract
		var legacy bool
		err := s.repo.withReminderContract(ctx, contracts[i].ID, func(tc context.Context, c *SavingsContract) error {
			if c == nil || c.Status != SavingsStatusActive || c.EndDate == nil || c.EndDate.IsZero() || c.EndDate.Year() < 1 || c.EndDate.Year() > 9999 || !c.EndDate.Before(now) {
				return nil
			}
			var err error
			legacy, err = s.expireSavingsTermTx(tc, c)
			if err != nil {
				return err
			}
			changed = c
			return nil
		})
		if err != nil {
			return expired, err
		}
		if changed != nil {
			expired++
			// Only the FIRST OFF decision may invoke best-effort SMTP, AFTER
			// its tombstone commits. Crash loses availability, never redrives.
			if legacy {
				s.notifyExpired(ctx, changed)
			}
		}
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

package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// ReminderOutbox is intentionally separate from best-effort Notifier. Methods
// must join the caller's primary transaction and MUST NOT perform network I/O.
// The composition adapter owns rendering/sealing and maps the mail package DTO.
type ReminderOutbox interface {
	EnqueueReminderTx(context.Context, ReminderRequest) (ReminderQueueResult, error)
}

type ReminderCanceler interface {
	CancelPendingTx(context.Context, []string) error
}

type ReminderRequest struct {
	ContractID, ExpiryGeneration string
	WindowDays                   int
	EndDate, Date                time.Time
	Recipients                   []string
	Vars                         map[string]any
}

func (ReminderRequest) LogValue() slog.Value { return slog.StringValue("[reminder redacted]") }

type ReminderQueueResult struct {
	IDs   []string
	State string // selection state only; NEVER an SMTP delivery acknowledgement
}

// SetReminderOutbox opts into atomic reminder decisions ONLY, not SMTP dispatch.
// Set once during startup, before scheduler/routes serve. Nil preserves legacy
// behavior for legacy records; already-durable records never fall back to SMTP.
func (r *Repo) SetReminderOutbox(o ReminderOutbox) { r.reminderOutbox = o }

func (s *SavingsService) DurableRemindersEnabled() bool { return s.repo.reminderOutbox != nil }

// SetReminderCanceler is wired even with the producer OFF or payload keys absent:
// canceling already-queued work needs no decryption or SMTP capability.
func (r *Repo) SetReminderCanceler(c ReminderCanceler) { r.reminderCanceler = c }

func reminderGeneration(end time.Time) string {
	sum := sha256.Sum256([]byte(end.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)))
	return hex.EncodeToString(sum[:])
}
func reminderID(contract, generation string) string {
	sum := sha256.Sum256([]byte(contract + "\x00" + generation))
	return "srem_" + hex.EncodeToString(sum[:])
}

// All local mutations use contract -> reminder -> outbox locking order. The
// advisory key also serializes a missing row; no second pool/transaction bridge.
func (r *Repo) withReminderContract(ctx context.Context, id string, fn func(context.Context, *SavingsContract) error) error {
	for _, col := range []*pgdoc.Store{r.savingCtrs, r.reminders, r.profiles} {
		if err := col.Ensure(ctx); err != nil {
			return err
		}
	}
	db := r.savingCtrs.DB()
	return db.WithTx(ctx, func(tc context.Context) error {
		if err := db.LockXact(tc, "savings-reminder:"+id); err != nil {
			return err
		}
		var c SavingsContract
		found, err := r.savingCtrs.GetForUpdate(tc, id, &c)
		if err != nil {
			return err
		}
		if !found {
			return fn(tc, nil)
		}
		return fn(tc, &c)
	})
}

func (r *Repo) reminderRowsTx(ctx context.Context, id string) ([]ReminderNotification, error) {
	rows, err := findTyped[ReminderNotification](ctx, r.reminders, pgdoc.M{
		"resourceType": ResourceTypeSavingsContract, "targetResourceId": id,
	}, pgdoc.Sort(pgdoc.Asc("_id")))
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if _, err := r.reminders.GetForUpdate(ctx, rows[i].ID, &rows[i]); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// scheduleReminderTx uses a deterministic primary key per contract+expiry, not a
// check-then-insert random ID. Legacy processed evidence is conservatively merged
// and never backfilled; duplicate legacy docs cannot schedule duplicate windows.
func (r *Repo) scheduleReminderTx(ctx context.Context, c *SavingsContract, days []int, now time.Time) (*ReminderNotification, bool, error) {
	generation := reminderGeneration(*c.EndDate)
	id := reminderID(c.ID, generation)
	rows, err := r.reminderRowsTx(ctx, c.ID)
	if err != nil {
		return nil, false, err
	}
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i], false, nil
		}
	}
	var legacy []ReminderNotification
	for _, n := range rows {
		if n.ExpiryGeneration == "" {
			legacy = append(legacy, n)
		}
	}
	if len(legacy) > 0 {
		// Adopt the stored schedule, not new config windows that would backfill it.
		days = nil
		for _, d := range legacy[0].Notifications {
			days = append(days, d.DaysBefore)
		}
	}
	if len(days) == 0 {
		return nil, false, errors.New("reminder schedule has no windows")
	}
	unique := map[int]bool{}
	for _, day := range days {
		if day < 0 || day > 366 {
			return nil, false, errors.New("invalid reminder window")
		}
		unique[day] = true
	}
	days = nil
	for day := range unique {
		days = append(days, day)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(days)))
	n := &ReminderNotification{ID: id, ResourceType: ResourceTypeSavingsContract, TargetResourceID: c.ID,
		MessageTemplateID: savingsExpiryReminderTemplate, ExpiryGeneration: generation,
		Status: ReminderStatusInProgress, CreatedAt: &now, UpdatedAt: &now}
	for _, day := range days {
		detail := ReminderNotificationDetail{DaysBefore: day}
		for _, old := range legacy {
			if old.Status == ReminderStatusDone {
				detail.SkippedAt, detail.SkipReason = &now, "legacy_processed"
			}
			for _, d := range old.Notifications {
				if d.DaysBefore == day && d.SentAt != nil {
					detail.SentAt = d.SentAt
				}
			}
		}
		n.Notifications = append(n.Notifications, detail)
	}
	if allDetailsSent(n.Notifications) {
		n.Status = ReminderStatusDone
	}
	for i := range legacy {
		legacy[i].Status, legacy[i].ExpiryGeneration = ReminderStatusDone, generation
		if err := r.SaveReminderNotification(ctx, &legacy[i]); err != nil {
			return nil, false, err
		}
	}
	if _, err := r.reminders.InsertOne(ctx, n); err != nil {
		return nil, false, err
	}
	return n, true, nil
}

// Producer rollback is not a migration back to immediate SMTP: any durable
// history (even DONE or an older expiry) prevents a new untagged legacy schedule.
func (r *Repo) scheduleLegacyReminder(ctx context.Context, id, template string, days []int) (created bool, err error) {
	err = r.withReminderContract(ctx, id, func(tc context.Context, _ *SavingsContract) error {
		rows, e := r.reminderRowsTx(tc, id)
		if e != nil {
			return e
		}
		for _, n := range rows {
			if n.ExpiryGeneration != "" {
				return nil
			}
		}
		created, e = r.scheduleLegacyReminderRow(tc, ResourceTypeSavingsContract, id, template, days)
		return e
	})
	return created && err == nil, err
}

func (r *Repo) scheduleDurableReminder(ctx context.Context, id string, days []int, now, threshold time.Time) (created bool, err error) {
	err = r.withReminderContract(ctx, id, func(tc context.Context, c *SavingsContract) error {
		if c == nil || c.Status != SavingsStatusActive || c.EndDate == nil || !c.EndDate.Before(threshold) {
			return nil
		}
		_, created, err = r.scheduleReminderTx(tc, c, days, now)
		return err
	})
	return created && err == nil, err
}

func (s *SavingsService) queueReminder(ctx context.Context, candidate *ReminderNotification, now time.Time) (queued bool, err error) {
	err = s.repo.withReminderContract(ctx, candidate.TargetResourceID, func(tc context.Context, c *SavingsContract) error {
		if c == nil || c.Status != SavingsStatusActive || c.EndDate == nil {
			return nil
		}
		// The batch candidate is only an identity hint. Extension/admin edits may
		// have completed or adopted it before this contract lock was acquired.
		n := new(ReminderNotification)
		found, e := s.repo.reminders.GetForUpdate(tc, candidate.ID, n)
		if e != nil || !found {
			return e
		}
		if n.Status != ReminderStatusInProgress || n.ResourceType != ResourceTypeSavingsContract || n.TargetResourceID != c.ID {
			return nil
		}
		if n.ExpiryGeneration == "" {
			n, _, e = s.repo.scheduleReminderTx(tc, c, nil, now)
			if e != nil {
				return e
			}
		}
		if n.Status != ReminderStatusInProgress || n.ExpiryGeneration != reminderGeneration(*c.EndDate) {
			return nil
		}
		du := daysUntil(now, *c.EndDate)
		chosen, skipped := reminderDecision(n.Notifications, du)
		if chosen == -1 {
			return nil
		}
		profile, e := s.repo.FindByID(tc, c.BillingProfileID)
		if e != nil {
			return e
		}
		if profile == nil {
			return errors.New("reminder profile unavailable")
		}
		result, e := s.repo.reminderOutbox.EnqueueReminderTx(tc, ReminderRequest{
			ContractID: c.ID, ExpiryGeneration: n.ExpiryGeneration, WindowDays: n.Notifications[chosen].DaysBefore,
			EndDate: *c.EndDate, Date: now, Recipients: Recipients(profile), Vars: reminderVars(c, profile, du),
		})
		if e != nil {
			return e
		}
		if len(result.IDs) == 0 || len(result.IDs) > 20 || result.State == "" {
			return errors.New("invalid reminder queue result")
		}
		for _, i := range skipped {
			n.Notifications[i].SkippedAt, n.Notifications[i].SkipReason = &now, "superseded"
		}
		d := &n.Notifications[chosen]
		d.DeliveryQueuedAt, d.OutboxIDs, d.QueueState = &now, append([]string(nil), result.IDs...), result.State
		if allDetailsSent(n.Notifications) {
			n.Status = ReminderStatusDone
		}
		queued = true
		return s.repo.SaveReminderNotification(tc, n)
	})
	return queued && err == nil, err
}

func (r *Repo) cancelReminderRowsTx(ctx context.Context, id string, end *time.Time, now time.Time) error {
	rows, err := r.reminderRowsTx(ctx, id)
	if err != nil {
		return err
	}
	for i := range rows {
		n := &rows[i]
		for _, d := range n.Notifications {
			if len(d.OutboxIDs) > 0 {
				if r.reminderCanceler == nil {
					return errors.New("durable reminder cancellation unavailable")
				}
				if err := r.reminderCanceler.CancelPendingTx(ctx, d.OutboxIDs); err != nil {
					return err
				}
			}
		}
		if n.ExpiryGeneration == "" && r.reminderOutbox != nil && end != nil {
			n.ExpiryGeneration = reminderGeneration(*end)
		}
		n.Status = ReminderStatusDone
		for j := range n.Notifications {
			if !reminderProcessed(n.Notifications[j]) {
				n.Notifications[j].SkippedAt, n.Notifications[j].SkipReason = &now, "cancelled"
			}
		}
		if err := r.SaveReminderNotification(ctx, n); err != nil {
			return err
		}
	}
	return nil
}

// MutateSavingsContract re-reads/authorizes local cancel/extend under the SAME
// contract boundary as scheduling/enqueue. It never invokes payment/cloud/SMTP.
func (r *Repo) MutateSavingsContract(ctx context.Context, profileID, id string, extend bool) (result *SavingsContract, err error) {
	err = r.withReminderContract(ctx, id, func(tc context.Context, c *SavingsContract) error {
		if c == nil || c.BillingProfileID != profileID {
			return httpx.NotFound("Savings contract not found")
		}
		oldEnd := c.EndDate
		if extend {
			if c.Status != SavingsStatusActive {
				return httpx.BadRequest("Only active contracts can be extended")
			}
			if c.PaidUpfront {
				return httpx.BadRequest("An upfront contract cannot be extended: buy a new one when it ends")
			}
			if c.EndDate != nil {
				end := c.EndDate.AddDate(0, c.DurationMonths, 0)
				c.EndDate = &end
			}
		} else {
			if c.Status != SavingsStatusPendingPayment {
				if c.PaidUpfront {
					return httpx.BadRequest("Cannot cancel upfront contracts")
				}
				if c.Status != SavingsStatusActive {
					return httpx.BadRequest("Only active contracts can be cancelled")
				}
			}
			c.Status = SavingsStatusCancelled
		}
		if err := r.SaveSavingsContract(tc, c); err != nil {
			return err
		}
		if err := r.cancelReminderRowsTx(tc, id, oldEnd, time.Now().UTC()); err != nil {
			return err
		}
		result = c
		return nil
	})
	return result, err
}

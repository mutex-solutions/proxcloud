package billing

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// reminder_repo.go = the DB access for the savings-contract expiry-reminder pipeline (the
// reminder-notification repository + the config/contract queries). Collection
// `reminderNotification`, config path billingConfiguration.savingsContractNotificationConfig.
// reminderDaysBeforeExpiry.

// SavingsContractReminderDays reads the global config's reminderDaysBeforeExpiry
// (savingsContractNotificationConfig.reminderDaysBeforeExpiry); nil
// when the config doc / subdoc / list is absent (→ reminders disabled).
func (r *Repo) SavingsContractReminderDays(ctx context.Context) ([]int, error) {
	var cfg struct {
		Notif *struct {
			ReminderDaysBeforeExpiry []int `json:"reminderDaysBeforeExpiry"`
		} `json:"savingsContractNotificationConfig"`
	}
	found, err := r.configs.FindOne(ctx, pgdoc.M{}, &cfg)
	if err != nil || !found {
		return nil, err
	}
	if cfg.Notif == nil {
		return nil, nil
	}
	return cfg.Notif.ReminderDaysBeforeExpiry, nil
}

// ActiveSavingsContractsEndingBefore returns the ACTIVE contracts whose endDate is strictly before
// the threshold (the send-expiry-reminders window).
func (r *Repo) ActiveSavingsContractsEndingBefore(ctx context.Context, threshold time.Time) ([]SavingsContract, error) {
	return findTyped[SavingsContract](ctx, r.savingCtrs, pgdoc.M{
		"status":  SavingsStatusActive,
		"endDate": pgdoc.M{"$lt": threshold},
	})
}

// ExistsInProgressReminder reports whether an IN_PROGRESS reminder already exists for the given
// resource — the dedup gate for scheduling.
func (r *Repo) ExistsInProgressReminder(ctx context.Context, resourceType, targetID string) (bool, error) {
	return r.reminders.Exists(ctx, pgdoc.M{
		"resourceType": resourceType, "targetResourceId": targetID, "status": ReminderStatusInProgress,
	})
}

// ScheduleReminder schedules a reminder: no-op (returns created=false)
// when an IN_PROGRESS doc already exists; else insert an IN_PROGRESS doc with one detail per day,
// sorted DESC by daysBefore (the dispatcher's tightest-window pick depends on this order).
func (r *Repo) ScheduleReminder(ctx context.Context, resourceType, targetID, templateID string, days []int) (bool, error) {
	exists, err := r.ExistsInProgressReminder(ctx, resourceType, targetID)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	sorted := append([]int(nil), days...)
	sort.Sort(sort.Reverse(sort.IntSlice(sorted)))
	details := make([]ReminderNotificationDetail, 0, len(sorted))
	for _, d := range sorted {
		details = append(details, ReminderNotificationDetail{DaysBefore: d})
	}
	now := time.Now().UTC()
	doc := ReminderNotification{
		ID:               pgdoc.NewID(),
		TargetResourceID: targetID, MessageTemplateID: templateID, ResourceType: resourceType,
		Status: ReminderStatusInProgress, Notifications: details, CreatedAt: &now, UpdatedAt: &now,
	}
	if _, err := r.reminders.InsertOne(ctx, doc); err != nil {
		return false, err
	}
	return true, nil
}

// FindInProgressReminders returns every reminder still in the IN_PROGRESS status.
func (r *Repo) FindInProgressReminders(ctx context.Context) ([]ReminderNotification, error) {
	return findTyped[ReminderNotification](ctx, r.reminders, pgdoc.M{"status": ReminderStatusInProgress})
}

// SaveReminderNotification persists a reminder doc (upsert-by-id; stamps updatedAt).
func (r *Repo) SaveReminderNotification(ctx context.Context, n *ReminderNotification) error {
	if n.ID == "" {
		return errors.New("reminder notification has no id")
	}
	now := time.Now().UTC()
	n.UpdatedAt = &now
	return r.reminders.Upsert(ctx, n.ID, n)
}

// CancelReminders cancels reminders: an IN_PROGRESS doc for
// (resourceType, targetID) is flipped to DONE so no further reminders fire (called on contract
// cancel / extend / expire). No-op when absent or already DONE.
func (r *Repo) CancelReminders(ctx context.Context, resourceType, targetID string) error {
	var n ReminderNotification
	found, err := r.reminders.FindOne(ctx, pgdoc.M{"resourceType": resourceType, "targetResourceId": targetID}, &n)
	if err != nil || !found {
		return err
	}
	if n.Status != ReminderStatusInProgress {
		return nil
	}
	n.Status = ReminderStatusDone
	return r.SaveReminderNotification(ctx, &n)
}

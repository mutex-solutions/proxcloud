package billing

import (
	"context"
	"time"
)

// reminder.go implements the savings-contract expiry-REMINDER pipeline. It is a two-phase
// flow:
//   - SCHEDULER (daily): SendExpiryReminders finds ACTIVE contracts nearing expiry and creates one
//     IN_PROGRESS reminderNotification doc per contract (idempotent via the exists-gate).
//   - DISPATCHER (hourly): ProcessReminderNotifications walks IN_PROGRESS docs, re-evaluates each
//     contract's days-until-expiry, sends the single tightest due reminder (marking earlier missed
//     windows as sent without emailing), stamps sentAt, and flips the doc to DONE when all details
//     are sent.
// The generic ReminderNotification framework (a Map<ResourceType,Handler>) collapses here to the
// only ResourceType that exists — SAVINGS_CONTRACT — but the doc shape, dedup semantics, detail
// ordering, and send/skip logic are preserved. Collection name is `reminderNotification`.
//
// The mail send goes through the nil-safe notify
// hook (no-op without a mail gateway), so the state machine advances even when email is unconfigured
// (the send always runs, and may itself be a no-op).

const (
	ResourceTypeSavingsContract = "SAVINGS_CONTRACT"

	ReminderStatusPending    = "PENDING"
	ReminderStatusInProgress = "IN_PROGRESS"
	ReminderStatusDone       = "DONE"

	// savingsExpiryReminderTemplate is the hardcoded messageTemplateId.
	savingsExpiryReminderTemplate = "savings_contract_expiry_reminder"
)

// ReminderNotificationDetail is one scheduled reminder window:
// daysBefore = how many days before expiry to remind; sentAt nil = not yet fired.
type ReminderNotificationDetail struct {
	DaysBefore int        `json:"daysBefore"`
	SentAt     *time.Time `json:"sentAt,omitempty"`
}

// ReminderNotification is the per-target reminder doc (collection "reminderNotification").
type ReminderNotification struct {
	ID                string                       `json:"id,omitempty"`
	TargetResourceID  string                       `json:"targetResourceId,omitempty"`
	MessageTemplateID string                       `json:"messageTemplateId,omitempty"`
	ResourceType      string                       `json:"resourceType,omitempty"`
	Status            string                       `json:"status,omitempty"`
	Notifications     []ReminderNotificationDetail `json:"notifications,omitempty"`
	CreatedAt         *time.Time                   `json:"createdAt,omitempty"`
	UpdatedAt         *time.Time                   `json:"updatedAt,omitempty"`
}

// reminderDecision determines which reminders to process: iterate the
// details in stored (DESC-by-daysBefore) order; among UNSENT details whose window is open
// (daysUntilExpiry <= daysBefore), pick the LAST one reached = the SMALLEST daysBefore (tightest
// window). Every earlier open-unsent detail becomes a "skip" (marked sent, no email). Returns
// sendIdx = -1 when nothing is due.
func reminderDecision(details []ReminderNotificationDetail, daysUntilExpiry int64) (sendIdx int, skipIdxs []int) {
	sendIdx = -1
	for i := range details {
		d := details[i]
		if d.SentAt == nil && daysUntilExpiry <= int64(d.DaysBefore) {
			if sendIdx != -1 {
				skipIdxs = append(skipIdxs, sendIdx)
			}
			sendIdx = i
		}
	}
	return sendIdx, skipIdxs
}

// daysUntil returns whole days between now and endDate — truncation toward zero (negative when
// the contract is already past its endDate but still ACTIVE).
func daysUntil(now, endDate time.Time) int64 {
	return int64(endDate.Sub(now).Hours() / 24)
}

func allDetailsSent(details []ReminderNotificationDetail) bool {
	for i := range details {
		if details[i].SentAt == nil {
			return false
		}
	}
	return true
}

// SendExpiryReminders is the DAILY scheduler half:
// read the config's reminderDaysBeforeExpiry; for every ACTIVE contract ending before now +
// (maxDays+1) days, schedule its reminders (idempotent). Returns the number of contracts newly
// scheduled. A nil/empty config disables reminders (no-op).
func (s *SavingsService) SendExpiryReminders(ctx context.Context) (int, error) {
	days, err := s.repo.SavingsContractReminderDays(ctx)
	if err != nil {
		return 0, err
	}
	if len(days) == 0 {
		return 0, nil // guard: no savingsContractNotificationConfig.reminderDaysBeforeExpiry
	}
	maxDays := 0
	for _, d := range days {
		if d > maxDays {
			maxDays = d
		}
	}
	// threshold = now + (maxDays + 1) days — the +1 buffers a contract entering the window between
	// daily runs (now + (maxReminderDays + 1) days).
	threshold := s.now().Add(time.Duration(maxDays+1) * 24 * time.Hour)
	contracts, err := s.repo.ActiveSavingsContractsEndingBefore(ctx, threshold)
	if err != nil {
		return 0, err
	}
	scheduled := 0
	for i := range contracts {
		created, err := s.repo.ScheduleReminder(ctx, ResourceTypeSavingsContract, contracts[i].ID, savingsExpiryReminderTemplate, days)
		if err != nil {
			return scheduled, err
		}
		if created {
			scheduled++
		}
	}
	return scheduled, nil
}

// ProcessReminderNotifications is the HOURLY dispatcher half:
// walk IN_PROGRESS docs, send each one's due
// reminder. Best-effort + resilient — a per-doc error is logged and skipped (never aborts the
// batch). Returns the number of reminders emailed.
func (s *SavingsService) ProcessReminderNotifications(ctx context.Context) (int, error) {
	docs, err := s.repo.FindInProgressReminders(ctx)
	if err != nil {
		return 0, err
	}
	now := s.now()
	sent := 0
	for i := range docs {
		emailed, err := s.processReminder(ctx, &docs[i], now)
		if err != nil {
			// catch per-doc + log + continue.
			continue
		}
		if emailed {
			sent++
		}
	}
	return sent, nil
}

// processReminder handles one doc. Returns whether an email was sent.
func (s *SavingsService) processReminder(ctx context.Context, n *ReminderNotification, now time.Time) (bool, error) {
	// Only SAVINGS_CONTRACT exists; a doc of another type has no handler → skip (warn+skip).
	if n.ResourceType != ResourceTypeSavingsContract {
		return false, nil
	}
	contract, err := s.repo.SavingsContractByID(ctx, n.TargetResourceID)
	if err != nil {
		return false, err
	}
	// determineRemindersToProcess: missing OR non-ACTIVE contract → none.
	if contract == nil || contract.Status != SavingsStatusActive || contract.EndDate == nil {
		return false, nil
	}
	du := daysUntil(now, *contract.EndDate)
	sendIdx, skipIdxs := reminderDecision(n.Notifications, du)
	if sendIdx == -1 {
		return false, nil
	}
	// buildMailContext: resolve the profile; a nil profile → nil context → skip (no state change).
	profile, err := s.repo.FindByID(ctx, contract.BillingProfileID)
	if err != nil {
		return false, err
	}
	if profile == nil {
		return false, nil
	}
	// Mark the missed/superseded windows as sent WITHOUT emailing.
	for _, si := range skipIdxs {
		t := now
		n.Notifications[si].SentAt = &t
	}
	// Send ONE email (nil-safe; no-op when no mail gateway).
	s.sendReminderEmail(ctx, contract, profile, du)
	// Stamp the chosen detail's sentAt (first detail whose daysBefore == chosen.daysBefore).
	chosenDays := n.Notifications[sendIdx].DaysBefore
	for i := range n.Notifications {
		if n.Notifications[i].DaysBefore == chosenDays {
			t := now
			n.Notifications[i].SentAt = &t
			break
		}
	}
	if allDetailsSent(n.Notifications) {
		n.Status = ReminderStatusDone
	}
	if err := s.repo.SaveReminderNotification(ctx, n); err != nil {
		return true, err
	}
	return true, nil
}

// sendReminderEmail builds the mail context + sends. displayDays =
// max(1, daysUntilExpiry) so the customer never sees 0/negative; endDate is "dd MMM yyyy".
func (s *SavingsService) sendReminderEmail(ctx context.Context, c *SavingsContract, profile *BillingProfile, du int64) {
	displayDays := du
	if displayDays < 1 {
		displayDays = 1
	}
	vars := map[string]any{
		// fullName is not in the base values map (it's supplied via the MailContext shell) but the
		// seeded Go template greets with {{fullName}} — include it so the email renders.
		"fullName":        profileFullName(profile),
		"savingsPlanName": c.SavingsPlanName,
		"daysUntilExpiry": displayDays,
		"currency":        profile.Currency,
	}
	if c.EndDate != nil {
		vars["endDate"] = c.EndDate.Format("02 Jan 2006") // "dd MMM yyyy"
	}
	if c.MonthlyCommittedAmount != nil {
		vars["monthlyCommittedAmount"] = c.MonthlyCommittedAmount.StringFixed(2)
	}
	if c.DiscountRate != nil {
		vars["discountRate"] = c.DiscountRate.String()
	}
	notify(ctx, s.notifier, savingsExpiryReminderTemplate, profile.Email, vars)
}

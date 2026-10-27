//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
)

// TestSavingsExpiryReminderPipeline exercises the full schedule → dispatch → dedup → DONE flow
// (reminder notification + savings-contract reminder handling) against the real store.
func TestSavingsExpiryReminderPipeline(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	now := time.Now().UTC()

	// Config: remind 30 + 7 days before expiry.
	if _, err := db.C("billingConfiguration").InsertOne(ctx, pgdoc.M{
		"baseCurrency": "USD", "defaultConfiguration": true,
		"savingsContractNotificationConfig": pgdoc.M{"reminderDaysBeforeExpiry": []int{7, 30}},
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	// Profile (email target).
	const bpID = "6a37e2f547f3b4378ba78550"
	if _, err := db.C("billingProfile").InsertOne(ctx, pgdoc.M{
		"_id": bpID, "email": "ada@example.com", "firstName": "Ada", "lastName": "Lovelace", "currency": "USD",
	}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	// ACTIVE contract expiring in ~7 days → inside the (max 30 + 1) window. The +1h cushions the
	// sub-second test elapse so Duration.toDays()-style truncation gives a stable 7 (not 6).
	contractID := mustInsertID(t, db, "savingsContract", pgdoc.M{
		"status": billing.SavingsStatusActive, "billingProfileId": bpID,
		"savingsPlanName": "PW Saver", "endDate": now.Add(7*24*time.Hour + time.Hour),
	})

	cap := &captureNotifier{}
	svc := billing.NewSavingsService(repo)
	svc.SetNotifier(cap)

	// SCHEDULE — creates one IN_PROGRESS doc with details [30,7] (DESC), both unsent.
	n, err := svc.SendExpiryReminders(ctx)
	if err != nil || n != 1 {
		t.Fatalf("SendExpiryReminders: n=%d err=%v (want 1)", n, err)
	}
	docs, _ := repo.FindInProgressReminders(ctx)
	if len(docs) != 1 {
		t.Fatalf("in-progress docs = %d, want 1", len(docs))
	}
	if got := docs[0].Notifications; len(got) != 2 || got[0].DaysBefore != 30 || got[1].DaysBefore != 7 {
		t.Fatalf("details not [30,7] DESC: %+v", got)
	}
	if docs[0].TargetResourceID != contractID {
		t.Fatalf("target = %s, want %s", docs[0].TargetResourceID, contractID)
	}

	// SCHEDULE again while IN_PROGRESS → idempotent no-op (exists-gate).
	if n, err := svc.SendExpiryReminders(ctx); err != nil || n != 0 {
		t.Fatalf("2nd SendExpiryReminders: n=%d err=%v (want 0, dedup)", n, err)
	}

	// DISPATCH — du=7: both 30 & 7 open, picks the tightest (7) to email, marks 30 skipped (no email).
	sent, err := svc.ProcessReminderNotifications(ctx)
	if err != nil || sent != 1 {
		t.Fatalf("ProcessReminderNotifications: sent=%d err=%v (want 1)", sent, err)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("emails sent = %d, want 1", len(cap.calls))
	}
	if cap.calls[0].key != "savings_contract_expiry_reminder" || cap.calls[0].to[0] != "ada@example.com" {
		t.Fatalf("wrong email: %+v", cap.calls[0])
	}
	if cap.calls[0].vars["daysUntilExpiry"] != int64(7) || cap.calls[0].vars["savingsPlanName"] != "PW Saver" {
		t.Fatalf("wrong vars: %+v", cap.calls[0].vars)
	}
	// Both details now sent (7 emailed, 30 skip-marked) → doc flipped to DONE.
	done, _ := repo.FindInProgressReminders(ctx)
	if len(done) != 0 {
		t.Fatalf("doc should be DONE (no IN_PROGRESS), got %d", len(done))
	}

	// DISPATCH again → nothing (doc is DONE, not IN_PROGRESS) — no double-send.
	if sent, err := svc.ProcessReminderNotifications(ctx); err != nil || sent != 0 {
		t.Fatalf("2nd dispatch: sent=%d err=%v (want 0)", sent, err)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("no second email expected, got %d", len(cap.calls))
	}
}

// TestSavingsReminderCancel verifies cancelReminders flips an IN_PROGRESS doc to DONE (contract
// cancel/extend/expire path).
func TestSavingsReminderCancel(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)

	created, err := repo.ScheduleReminder(ctx, billing.ResourceTypeSavingsContract, "contract-x", "savings_contract_expiry_reminder", []int{7})
	if err != nil || !created {
		t.Fatalf("ScheduleReminder: created=%v err=%v", created, err)
	}
	if docs, _ := repo.FindInProgressReminders(ctx); len(docs) != 1 {
		t.Fatalf("want 1 in-progress before cancel, got %d", len(docs))
	}
	if err := repo.CancelReminders(ctx, billing.ResourceTypeSavingsContract, "contract-x"); err != nil {
		t.Fatalf("CancelReminders: %v", err)
	}
	if docs, _ := repo.FindInProgressReminders(ctx); len(docs) != 0 {
		t.Fatalf("want 0 in-progress after cancel (flipped DONE), got %d", len(docs))
	}
	// Idempotent: cancelling again (no IN_PROGRESS) is a no-op.
	if err := repo.CancelReminders(ctx, billing.ResourceTypeSavingsContract, "contract-x"); err != nil {
		t.Fatalf("2nd CancelReminders: %v", err)
	}
}

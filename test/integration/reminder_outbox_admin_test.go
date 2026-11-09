//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func TestReminderOutboxAdminNoOpPreservesUnselectedAndFrozenPending(t *testing.T) {
	db, store, repo, svc, _, contract := reminderFixture(t)
	// Thirty-day window queued, seven-day window remains eligible.
	if _, err := db.C("savingsContract").SetByID(t.Context(), contract, pgdoc.M{"endDate": time.Now().UTC().Add(20 * 24 * time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	queueReminderFixture(t, svc)
	id := reminderOutboxIDs(t, db)[0]
	var before billing.ReminderNotification
	if ok, err := db.C("reminderNotification").FindOne(t.Context(), nil, &before); err != nil || !ok {
		t.Fatal(err)
	}
	if before.Status != billing.ReminderStatusInProgress || before.Notifications[1].DeliveryQueuedAt != nil {
		t.Fatal("fixture did not leave future window")
	}
	var c pgdoc.M
	if ok, err := db.C("savingsContract").Get(t.Context(), contract, &c); err != nil || !ok {
		t.Fatal(err)
	}
	body, _ := json.Marshal(c)
	rc := &httpx.RequestContext{Sub: "fixture-op", Issuer: "fixture-iss", Azp: "fixture-cid"}
	if w := doAdmin(reminderAdmin(db, repo), rc, "PUT", "/admin/savings-contracts/"+contract, string(body)); w.Code != 200 {
		t.Fatalf("noop=%d %s", w.Code, w.Body)
	}
	var after billing.ReminderNotification
	_, err := db.C("reminderNotification").Get(t.Context(), before.ID, &after)
	if err != nil || after.Status != billing.ReminderStatusInProgress || after.Notifications[1].SkippedAt != nil || after.Notifications[1].SentAt != nil {
		t.Fatal("no-op suppressed future window")
	}
	wantMailState(t, store, id, mailoutbox.Pending)
	// A new expiry, in contrast, invalidates old selected and unselected windows.
	c["endDate"] = time.Now().UTC().Add(25 * 24 * time.Hour)
	body, _ = json.Marshal(c)
	if w := doAdmin(reminderAdmin(db, repo), rc, "PUT", "/admin/savings-contracts/"+contract, string(body)); w.Code != 200 {
		t.Fatalf("extend=%d %s", w.Code, w.Body)
	}
	wantMailState(t, store, id, mailoutbox.Expired)
	if n, err := svc.SendExpiryReminders(t.Context()); err != nil || n != 1 {
		t.Fatalf("new generation=%d %v", n, err)
	}
}

func TestReminderOutboxCancelTransactionBlocksIntentUntilCommit(t *testing.T) {
	db, store := outboxFixture(t)
	id := enqueueMail(t, db, store, mailReminder("cancel-before-intent"))
	claim := claimMail(t, store)
	entered, release := make(chan struct{}), make(chan struct{})
	cancelResult := make(chan error, 1)
	go func() {
		cancelResult <- db.WithTx(t.Context(), func(tc context.Context) error {
			if err := store.CancelPendingTx(tc, []string{id}); err != nil {
				return err
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	intent := make(chan error, 1)
	go func() { _, err := store.BeginDispatch(t.Context(), claim); intent <- err }()
	close(release)
	if err := <-cancelResult; err != nil {
		t.Fatal(err)
	}
	if err := <-intent; !errors.Is(err, mailoutbox.ErrLeaseLost) {
		t.Fatalf("canceled owner reached intent: %v", err)
	}
	wantMailState(t, store, id, mailoutbox.Expired)
	var attempts int
	if err := db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM mail_outbox_attempt`).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatal("cancellation invented dispatch audit")
	}
}

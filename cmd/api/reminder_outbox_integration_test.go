//go:build integration

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/mail"
	"github.com/mutex-solutions/proxcloud/internal/platform/message"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Exercise REAL composition on a pristine local PG: no SMTP configuration table
// exists initially, no real network mail resolver is called, all To are .invalid.
func TestReminderCompositionFreshNoSMTPQueuesBlocked(t *testing.T) {
	ctx := t.Context()
	pg, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("reminder_compose"), tcpostgres.WithUsername("fixture"), tcpostgres.WithPassword("fixture"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(pg) })
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	db, err := pgdoc.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	t.Setenv("PROXCLOUD_MAIL_SMTP_HOST", "")
	t.Setenv("PROXCLOUD_MAIL_FROM", "")
	var existed bool
	if err := db.Pool.QueryRow(ctx, `SELECT to_regclass('"thirdPartyIntegration"') IS NOT NULL`).Scan(&existed); err != nil || existed {
		t.Fatal("fixture was not fresh")
	}
	if err := prepareReminderDependencies(ctx, db); err != nil {
		t.Fatal(err)
	}
	keys, err := textcrypt.NewBlobKeys("fixture", map[string]string{"fixture": strings.Repeat("local-only-", 4)})
	if err != nil {
		t.Fatal(err)
	}
	store := mailoutbox.New(db, keys)
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	messages := message.NewRepo(db)
	if err := messages.Create(ctx, &message.MessageTemplate{Key: "savings_contract_expiry_reminder", MessageTitle: "Expiry", MessageBody: "<p>{{fullName}}</p>"}); err != nil {
		t.Fatal(err)
	}
	service := mail.NewService(func(context.Context, string, map[string]any) (string, string, bool, error) {
		t.Fatal("unexpected second template read")
		return "", "", false, nil
	}, func(context.Context) (mail.Mailer, string) { t.Fatal("legacy SMTP invoked"); return nil, "" }, "Fixture", nil)
	repo := billing.NewRepo(db)
	repo.SetReminderCanceler(store)
	repo.SetReminderOutbox(reminderOutboxAdapter{store: store, service: service, message: messages, config: mail.DurableConfigFromStore(db.C("thirdPartyIntegration"), nil)})
	for col, doc := range map[string]pgdoc.M{
		"billingConfiguration": {"savingsContractNotificationConfig": pgdoc.M{"reminderDaysBeforeExpiry": []int{7}}},
		"billingProfile":       {"_id": "fixture-profile", "email": "one@example.invalid", "currency": "USD"},
		"savingsContract":      {"_id": "fixture-contract", "billingProfileId": "fixture-profile", "status": "ACTIVE", "endDate": time.Now().UTC().Add(7*24*time.Hour + time.Hour)},
	} {
		if _, err := db.C(col).InsertOne(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	svc := billing.NewSavingsService(repo)
	if n, err := svc.SendExpiryReminders(ctx); err != nil || n != 1 {
		t.Fatalf("schedule=%d %v", n, err)
	}
	if n, err := svc.ProcessReminderNotifications(ctx); err != nil || n != 1 {
		t.Fatalf("queue=%d %v", n, err)
	}
	var state string
	if err := db.Pool.QueryRow(ctx, `SELECT state FROM mail_outbox`).Scan(&state); err != nil || state != mailoutbox.Blocked {
		t.Fatalf("state=%s err=%v", state, err)
	}
	var n billing.ReminderNotification
	if ok, err := db.C("reminderNotification").FindOne(ctx, nil, &n); err != nil || !ok || n.Notifications[0].DeliveryQueuedAt == nil || n.Notifications[0].SentAt != nil {
		t.Fatal("blocked decision claimed delivery")
	}
}

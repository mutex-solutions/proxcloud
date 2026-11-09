//go:build integration

package billing

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

type generationQueue struct{ calls int }

func (q *generationQueue) EnqueueReminderTx(context.Context, ReminderRequest) (ReminderQueueResult, error) {
	q.calls++
	return ReminderQueueResult{IDs: []string{"efcbe267-3335-46b2-a541-1d99a65f6d87"}, State: "pending"}, nil
}

// Replay the precise ordering without timing assumptions: batch snapshots an
// untagged legacy row, then a client/admin expiry mutation commits, then that
// stale candidate is processed. It must not poison the NEW generation with a
// deterministic zero-window DONE record before the next daily scheduler.
func TestReminderGenerationStaleLegacySnapshotAfterExpiryMutation(t *testing.T) {
	ctx := t.Context()
	pg, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("generation"), tcpostgres.WithUsername("fixture"), tcpostgres.WithPassword("fixture"), tcpostgres.BasicWaitStrategies())
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
	for _, mode := range []string{"client-extend", "admin-expiry"} {
		t.Run(mode, func(t *testing.T) {
			repo := NewRepo(db)
			queue := new(generationQueue)
			repo.SetReminderOutbox(queue)
			now := time.Now().UTC().Truncate(time.Second)
			oldEnd := now.Add(7 * 24 * time.Hour)
			id := mode
			for col, doc := range map[string]pgdoc.M{
				"billingProfile":  {"_id": "profile-" + id, "email": "fixture@example.invalid", "currency": "USD"},
				"savingsContract": {"_id": id, "billingProfileId": "profile-" + id, "status": SavingsStatusActive, "endDate": oldEnd, "durationMonths": 1},
			} {
				if _, err := db.C(col).InsertOne(ctx, doc); err != nil {
					t.Fatal(err)
				}
			}
			legacy := ReminderNotification{ID: "legacy-" + id, ResourceType: ResourceTypeSavingsContract, TargetResourceID: id, Status: ReminderStatusInProgress, Notifications: []ReminderNotificationDetail{{DaysBefore: 30}, {DaysBefore: 7}}}
			if _, err := db.C("reminderNotification").InsertOne(ctx, legacy); err != nil {
				t.Fatal(err)
			}
			candidate := legacy // snapshot BEFORE mutation (never refreshed by test)
			if mode == "client-extend" {
				if _, err := repo.MutateSavingsContract(ctx, "profile-"+id, id, true); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := repo.WithSavingsContractMutation(ctx, id, func(tc context.Context) error {
					_, err := db.C("savingsContract").SetByID(tc, id, pgdoc.M{"endDate": oldEnd.AddDate(0, 1, 0)}, nil)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			c, err := repo.SavingsContractByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			newID := reminderID(id, reminderGeneration(*c.EndDate))
			svc := NewSavingsService(repo)
			if queued, err := svc.queueReminder(ctx, &candidate, now); err != nil || queued || queue.calls != 0 {
				t.Fatalf("stale process queued=%v calls=%d err=%v", queued, queue.calls, err)
			}
			var got ReminderNotification
			if exists, err := db.C("reminderNotification").Get(ctx, newID, &got); err != nil || exists {
				t.Fatal("stale candidate created/poisoned new generation")
			}
			// An empty schedule is rejected even without a stale batch candidate.
			if err := repo.withReminderContract(ctx, id, func(tc context.Context, c *SavingsContract) error {
				_, _, err := repo.scheduleReminderTx(tc, c, nil, now)
				return err
			}); err == nil {
				t.Fatal("zero-window canonical schedule accepted")
			}
			future := c.EndDate.Add(-7 * 24 * time.Hour)
			if created, err := repo.scheduleDurableReminder(ctx, id, []int{30, 7}, future, future.Add(31*24*time.Hour)); err != nil || !created {
				t.Fatalf("daily schedule=%v err=%v", created, err)
			}
			if exists, err := db.C("reminderNotification").Get(ctx, newID, &got); err != nil || !exists || len(got.Notifications) != 2 || got.Status != ReminderStatusInProgress {
				t.Fatal("new generation lost valid configured windows")
			}
			if queued, err := svc.queueReminder(ctx, &got, future); err != nil || !queued || queue.calls != 1 {
				t.Fatalf("new generation queue=%v calls=%d err=%v", queued, queue.calls, err)
			}
		})
	}
}

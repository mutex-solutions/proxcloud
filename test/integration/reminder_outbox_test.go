//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// This hermetic adapter exercises actual EnqueueTx+AEAD+SQL, not SMTP. The real
// cmd composition's rendering/disposition logic is separately unit-tested.
type reminderEnqueuer struct {
	store  *mailoutbox.Store
	before func(context.Context)
	after  func(context.Context) error
}

func (e reminderEnqueuer) EnqueueReminderTx(ctx context.Context, r billing.ReminderRequest) (billing.ReminderQueueResult, error) {
	if e.before != nil {
		e.before(ctx)
	}
	m := mailoutbox.Reminder{ContractID: r.ContractID, ExpiryGeneration: r.ExpiryGeneration, WindowDays: r.WindowDays,
		EndDate: r.EndDate, Date: r.Date, From: "sender@example.invalid", Subject: "private fixture subject", TextBody: "private fixture body"}
	for i, a := range r.Recipients {
		m.Recipients = append(m.Recipients, mailoutbox.Recipient{Key: fmt.Sprintf("recipient-%02d", i), Address: a})
	}
	q, err := e.store.EnqueueTx(ctx, m)
	if err == nil && e.after != nil {
		err = e.after(ctx)
	}
	return billing.ReminderQueueResult{IDs: q.IDs, State: q.State}, err
}
func reminderFixture(t *testing.T) (*pgdoc.DB, *mailoutbox.Store, *billing.Repo, *billing.SavingsService, string, string) {
	t.Helper()
	db, store := outboxFixture(t)
	repo := billing.NewRepo(db)
	repo.SetReminderCanceler(store)
	repo.SetReminderOutbox(reminderEnqueuer{store: store})
	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD", "savingsContractNotificationConfig": pgdoc.M{"reminderDaysBeforeExpiry": []int{30, 7}}})
	profile := mustInsertID(t, db, "billingProfile", pgdoc.M{"email": "recipient@example.invalid", "currency": "USD"})
	contract := mustInsertID(t, db, "savingsContract", pgdoc.M{"status": billing.SavingsStatusActive, "billingProfileId": profile, "savingsPlanName": "fixture", "durationMonths": 1, "endDate": time.Now().UTC().Add(7*24*time.Hour + time.Hour)})
	svc := billing.NewSavingsService(repo)
	return db, store, repo, svc, profile, contract
}
func queueReminderFixture(t *testing.T, svc *billing.SavingsService) {
	t.Helper()
	if n, err := svc.SendExpiryReminders(t.Context()); err != nil || n != 1 {
		t.Fatalf("schedule=%d err=%v", n, err)
	}
	if n, err := svc.ProcessReminderNotifications(t.Context()); err != nil || n != 1 {
		t.Fatalf("queue=%d err=%v", n, err)
	}
}
func reminderOutboxIDs(t *testing.T, db *pgdoc.DB) []string {
	t.Helper()
	rows, err := db.Pool.Query(t.Context(), `SELECT id::text FROM mail_outbox ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
func reminderAdmin(db *pgdoc.DB, repo *billing.Repo) http.Handler {
	h := admin.NewHandler(admin.NewRepo(db), nil, user.NewRepo(db), nil, nil, repo, nil, nil, nil, nil, "", "fixture-iss", "fixture-cid")
	r := chi.NewRouter()
	h.Routes(r)
	return r
}

func TestReminderOutboxTwoSchedulersAtomicQueueAndProducerRollback(t *testing.T) {
	db, store, repo, svc, profile, contract := reminderFixture(t)
	cap := &captureNotifier{}
	svc.SetNotifier(cap)
	results := make(chan int, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); n, err := svc.SendExpiryReminders(t.Context()); results <- n; errs <- err }()
	}
	wg.Wait()
	total := <-results + <-results
	if total != 1 || <-errs != nil || <-errs != nil {
		t.Fatal("two schedulers duplicated schedule")
	}
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := svc.ProcessReminderNotifications(t.Context())
			results <- n
			errs <- err
		}()
	}
	wg.Wait()
	total = <-results + <-results
	if total != 1 || <-errs != nil || <-errs != nil {
		t.Fatal("two dispatchers duplicated decision")
	}
	ids := reminderOutboxIDs(t, db)
	if len(ids) != 1 || len(cap.calls) != 0 {
		t.Fatal("durable queue called legacy notifier or duplicated")
	}
	wantMailState(t, store, ids[0], mailoutbox.Pending)
	var n billing.ReminderNotification
	if ok, err := db.C("reminderNotification").FindOne(t.Context(), nil, &n); !ok || err != nil {
		t.Fatal(err)
	}
	if n.Status != billing.ReminderStatusDone || n.Notifications[0].SkippedAt == nil || n.Notifications[1].DeliveryQueuedAt == nil || n.Notifications[1].SentAt != nil {
		t.Fatal("queued was marked emailed or missing skip")
	}
	var leaked bool
	if err := db.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM mail_outbox WHERE row_to_json(mail_outbox)::text LIKE '%recipient@example.invalid%' OR row_to_json(mail_outbox)::text LIKE '%private fixture%')`).Scan(&leaked); err != nil || leaked {
		t.Fatal("plaintext leaked into outbox")
	}
	// Restart with enqueue OFF and no key: durable history may NEVER become a
	// fresh untagged immediate-send schedule. Cancellation still works keylessly.
	off := billing.NewRepo(db)
	off.SetReminderCanceler(mailoutbox.New(db, nil))
	offSvc := billing.NewSavingsService(off)
	offSvc.SetNotifier(cap)
	if n, err := offSvc.SendExpiryReminders(t.Context()); err != nil || n != 0 {
		t.Fatalf("OFF reschedule=%d %v", n, err)
	}
	if n, err := offSvc.ProcessReminderNotifications(t.Context()); err != nil || n != 0 || len(cap.calls) != 0 {
		t.Fatal("OFF sent durable decision")
	}
	if _, err := off.MutateSavingsContract(t.Context(), profile, contract, true); err != nil {
		t.Fatal(err)
	}
	wantMailState(t, store, ids[0], mailoutbox.Expired)
	// Old-generation history also prevents fallback after extension.
	if n, err := offSvc.SendExpiryReminders(t.Context()); err != nil || n != 0 {
		t.Fatal("OFF extended contract fell back to SMTP")
	}
	_ = repo
}

func TestReminderOutboxRollbackMissingKeysAndPostEnqueueFailure(t *testing.T) {
	db, store, repo, svc, _, _ := reminderFixture(t)
	if _, err := svc.SendExpiryReminders(t.Context()); err != nil {
		t.Fatal(err)
	}
	repo.SetReminderOutbox(reminderEnqueuer{store: mailoutbox.New(db, nil)})
	if n, err := svc.ProcessReminderNotifications(t.Context()); err == nil || n != 0 {
		t.Fatal("missing key advanced decision")
	}
	boom := errors.New("fixture rollback")
	repo.SetReminderOutbox(reminderEnqueuer{store: store, after: func(context.Context) error { return boom }})
	if n, err := svc.ProcessReminderNotifications(t.Context()); !errors.Is(err, boom) || n != 0 {
		t.Fatal("post-enqueue failure not propagated")
	}
	if ids := reminderOutboxIDs(t, db); len(ids) != 0 {
		t.Fatal("outbox escaped rollback")
	}
	docs, err := repo.FindInProgressReminders(t.Context())
	if err != nil || len(docs) != 1 {
		t.Fatal("decision escaped rollback")
	}
	for _, d := range docs[0].Notifications {
		if d.SentAt != nil || d.DeliveryQueuedAt != nil || d.SkippedAt != nil {
			t.Fatal("failed queue left processed markers")
		}
	}
	repo.SetReminderOutbox(reminderEnqueuer{store: store})
	if n, err := svc.ProcessReminderNotifications(t.Context()); err != nil || n != 1 {
		t.Fatalf("retry=%d %v", n, err)
	}
}

func TestReminderOutboxLegacySentAtNeverBackfilled(t *testing.T) {
	db, _, _, svc, _, contract := reminderFixture(t)
	stamp := time.Now().UTC()
	mustInsert(t, db, "reminderNotification", billing.ReminderNotification{ID: "legacy", TargetResourceID: contract, ResourceType: billing.ResourceTypeSavingsContract, Status: billing.ReminderStatusDone, Notifications: []billing.ReminderNotificationDetail{{DaysBefore: 30, SentAt: &stamp}, {DaysBefore: 7, SentAt: &stamp}}})
	if _, err := svc.SendExpiryReminders(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.ProcessReminderNotifications(t.Context()); err != nil || n != 0 {
		t.Fatal("legacy processed window backfilled")
	}
	if len(reminderOutboxIDs(t, db)) != 0 {
		t.Fatal("legacy delivery invented")
	}
}

func TestReminderOutboxCancelBeforeIntentAndAfterIntent(t *testing.T) {
	for _, intent := range []bool{false, true} {
		t.Run(fmt.Sprint(intent), func(t *testing.T) {
			db, store, repo, svc, profile, contract := reminderFixture(t)
			queueReminderFixture(t, svc)
			id := reminderOutboxIDs(t, db)[0]
			claim := claimMail(t, store)
			if intent {
				if _, err := store.BeginDispatch(t.Context(), claim); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := repo.MutateSavingsContract(t.Context(), profile, contract, false); err != nil {
				t.Fatal(err)
			}
			if intent {
				wantMailState(t, store, id, mailoutbox.Dispatching)
			} else {
				wantMailState(t, store, id, mailoutbox.Expired)
				if _, err := store.BeginDispatch(t.Context(), claim); !errors.Is(err, mailoutbox.ErrLeaseLost) {
					t.Fatal("canceled claim could send")
				}
			}
		})
	}
}

func TestReminderOutboxAdminEditDeleteCancelAndRollback(t *testing.T) {
	for _, method := range []string{"PUT", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			db, store, repo, svc, _, contract := reminderFixture(t)
			queueReminderFixture(t, svc)
			id := reminderOutboxIDs(t, db)[0]
			rc := &httpx.RequestContext{Sub: "fixture-op", Issuer: "fixture-iss", Azp: "fixture-cid"}
			h := reminderAdmin(db, repo)
			body := `{"status":"CANCELLED","billingProfileId":"fixture","endDate":"2028-01-01T00:00:00Z"}`
			// Missing cancellation capability must fail before any raw admin mutation.
			repo.SetReminderCanceler(nil)
			if w := doAdmin(h, rc, method, "/admin/savings-contracts/"+contract, body); w.Code < 400 {
				t.Fatal("admin bypassed cancellation")
			}
			c, err := repo.SavingsContractByID(t.Context(), contract)
			if err != nil || c == nil || c.Status != billing.SavingsStatusActive {
				t.Fatal("failed admin edit escaped rollback")
			}
			wantMailState(t, store, id, mailoutbox.Pending)
			repo.SetReminderCanceler(mailoutbox.New(db, nil))
			if w := doAdmin(h, rc, method, "/admin/savings-contracts/"+contract, body); w.Code != 200 {
				t.Fatalf("admin=%d %s", w.Code, w.Body)
			}
			wantMailState(t, store, id, mailoutbox.Expired)
			if method == "DELETE" {
				if c, _ := repo.SavingsContractByID(t.Context(), contract); c != nil {
					t.Fatal("delete did not commit")
				}
			}
		})
	}
}

func TestReminderOutboxQueueCancelRaceSerializesBusinessCommit(t *testing.T) {
	db, store, repo, svc, profile, contract := reminderFixture(t)
	if _, err := svc.SendExpiryReminders(t.Context()); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	repo.SetReminderOutbox(reminderEnqueuer{store: store, before: func(context.Context) { close(entered); <-release }})
	queued := make(chan error, 1)
	go func() { _, err := svc.ProcessReminderNotifications(t.Context()); queued <- err }()
	<-entered
	canceled := make(chan error, 1)
	go func() { _, err := repo.MutateSavingsContract(t.Context(), profile, contract, false); canceled <- err }()
	// Cancellation cannot pass the already-held contract boundary. No sleep or
	// inferred protocol timing; the selection hook explicitly holds that lock.
	select {
	case err := <-canceled:
		t.Fatalf("cancel overtook locked decision: %v", err)
	default:
	}
	close(release)
	if err := <-queued; err != nil {
		t.Fatal(err)
	}
	if err := <-canceled; err != nil {
		t.Fatal(err)
	}
	ids := reminderOutboxIDs(t, db)
	if len(ids) != 1 {
		t.Fatal("decision count")
	}
	wantMailState(t, store, ids[0], mailoutbox.Expired)
	if c, err := store.Claim(t.Context(), time.Minute); err != nil || c != nil {
		t.Fatal("canceled decision reclaimable")
	}
}

func TestReminderOutboxCancellationOwnTransactionAndIntentGuard(t *testing.T) {
	db, store := outboxFixture(t)
	id := enqueueMail(t, db, store, mailReminder("cancel-tx"))
	if err := store.CancelPendingTx(t.Context(), []string{id}); err == nil {
		t.Fatal("cancel accepted missing tx")
	}
	boom := errors.New("rollback")
	if err := db.WithTx(t.Context(), func(tc context.Context) error {
		if err := store.CancelPendingTx(tc, []string{id}); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	wantMailState(t, store, id, mailoutbox.Pending)
	if err := db.WithTx(t.Context(), func(tc context.Context) error { return store.CancelPendingTx(tc, []string{strings.Repeat("x", 36)}) }); !errors.Is(err, mailoutbox.ErrInvalid) {
		t.Fatal("malformed cancellation accepted")
	}
}

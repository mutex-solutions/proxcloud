//go:build integration

package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
	"github.com/shopspring/decimal"
)

func expiredPG(t *testing.T) *pgdoc.DB {
	t.Helper()
	dsn := os.Getenv("SAVINGS_EXPIRED_TEST_PG")
	if dsn == "" {
		t.Skip("owned disposable loopback PG17 required")
	}
	u, e := url.Parse(dsn)
	if e != nil || u.Hostname() != "127.0.0.1" {
		t.Fatal("literal loopback fixture required")
	}
	base, e := pgdoc.Connect(t.Context(), dsn)
	if e != nil {
		t.Fatal(e)
	}
	var version int
	if e = base.Pool.QueryRow(t.Context(), `SELECT current_setting('server_version_num')::int`).Scan(&version); e != nil || version < 170000 || version >= 180000 {
		t.Fatal("PG17 required", e)
	}
	name := "savings_expired_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, e = base.Pool.Exec(t.Context(), `CREATE DATABASE "`+name+`"`); e != nil {
		t.Fatal(e)
	}
	u.Path = "/" + name
	db, e := pgdoc.Connect(t.Context(), u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_ = db.Close(context.Background())
		_, _ = base.Pool.Exec(context.Background(), `DROP DATABASE "`+name+`" WITH (FORCE)`)
		_ = base.Close(context.Background())
	})
	return db
}
func expiredPeer(t *testing.T, db *pgdoc.DB) *pgdoc.DB {
	t.Helper()
	cfg := db.Pool.Config()
	cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	p, e := pgxpool.NewWithConfig(t.Context(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	peer := &pgdoc.DB{Pool: p}
	t.Cleanup(func() { _ = peer.Close(context.Background()) })
	return peer
}

type expiredQueue struct {
	db     *pgdoc.DB
	store  *mailoutbox.Store
	before func(context.Context, SavingsExpiredRequest) error
	after  func(context.Context) error
}

func (q expiredQueue) EnqueueSavingsExpiredTx(ctx context.Context, in SavingsExpiredRequest) (ReminderQueueResult, error) {
	tx, e := q.db.Tx(ctx)
	if e != nil {
		return ReminderQueueResult{}, e
	}
	var isolation string
	if e = tx.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&isolation); e != nil || isolation != "read committed" {
		return ReminderQueueResult{}, fmt.Errorf("wrong isolation: %s", isolation)
	}
	if q.before != nil {
		if e = q.before(ctx, in); e != nil {
			return ReminderQueueResult{}, e
		}
	}
	m := mailoutbox.SavingsExpired{ContractID: in.ContractID, TermGeneration: in.TermGeneration, CanonicalEndDate: in.CanonicalEndDate, Date: in.Date, ExpiresAt: in.ExpiresAt, Recipients: in.Recipients, FullName: in.FullName, SavingsPlanName: in.SavingsPlanName, Currency: in.Currency, StartDate: in.StartDate, MonthlyCommittedAmount: in.MonthlyCommittedAmount, From: "sender@example.invalid", Subject: "private fixture", TextBody: "private fixture body"}
	out, e := q.store.EnqueueSavingsExpiredTx(ctx, m)
	if e == nil && q.after != nil {
		e = q.after(ctx)
	}
	return ReminderQueueResult{IDs: out.IDs, State: out.State}, e
}
func expiredSetup(t *testing.T) (*pgdoc.DB, *mailoutbox.Store, *Repo, *SavingsService, string, string, time.Time) {
	t.Helper()
	db := expiredPG(t)
	keys, e := textcrypt.NewBlobKeys("fixture", map[string]string{"fixture": strings.Repeat("test-only-", 4)})
	if e != nil {
		t.Fatal(e)
	}
	store := mailoutbox.New(db, keys)
	if e = store.Ensure(t.Context()); e != nil {
		t.Fatal(e)
	}
	r := NewRepo(db)
	r.SetReminderCanceler(store)
	r.SetSavingsExpiredCanceler(store)
	r.SetSavingsExpiredOutbox(expiredQueue{db: db, store: store})
	now := time.Now().UTC().Truncate(time.Microsecond)
	end := now.Add(-time.Hour)
	profile, contract := pgdoc.NewID(), pgdoc.NewID()
	if _, e = db.C("billingProfile").InsertOne(t.Context(), pgdoc.M{"_id": profile, "email": "MixedCase@EXAMPLE.invalid", "firstName": "PRIVATE_NAME", "currency": "USD", "billingContacts": []any{pgdoc.M{"email": "other@example.invalid"}}}); e != nil {
		t.Fatal(e)
	}
	if _, e = db.C("savingsContract").InsertOne(t.Context(), pgdoc.M{"_id": contract, "billingProfileId": profile, "status": SavingsStatusActive, "endDate": end, "durationMonths": 1, "savingsPlanName": "fixture", "monthlyCommittedAmount": "10.00"}); e != nil {
		t.Fatal(e)
	}
	svc := NewSavingsService(r)
	svc.now = func() time.Time { return now }
	return db, store, r, svc, profile, contract, end
}
func expiredSQLCount(t *testing.T, db *pgdoc.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if e := db.Pool.QueryRow(t.Context(), query, args...).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}
func expiredUpdate(t *testing.T, db *pgdoc.DB, id string, set pgdoc.M) {
	t.Helper()
	if _, e := db.C("savingsContract").SetByID(t.Context(), id, set, nil); e != nil {
		t.Fatal(e)
	}
}
func expiredID(t *testing.T, db *pgdoc.DB) string {
	t.Helper()
	var id string
	if e := db.Pool.QueryRow(t.Context(), `SELECT id::text FROM mail_outbox WHERE kind=$1 ORDER BY created_at LIMIT 1`, mailoutbox.SavingsExpiredKind).Scan(&id); e != nil {
		t.Fatal(e)
	}
	return id
}
func expireOne(t *testing.T, s *SavingsService) {
	t.Helper()
	if n, e := s.ExpireContracts(t.Context()); e != nil || n != 1 {
		t.Fatalf("expired decisions=%d err=%v", n, e)
	}
}
func TestSavingsExpiredContendersOneCanonicalDecisionAndDBClock(t *testing.T) {
	db, store, r, s, _, id, end := expiredSetup(t)
	peer := expiredPeer(t, db)
	other := NewRepo(peer)
	other.SetReminderCanceler(mailoutbox.New(peer, nil))
	other.SetSavingsExpiredCanceler(mailoutbox.New(peer, nil))
	other.SetSavingsExpiredOutbox(expiredQueue{db: peer, store: mailoutbox.New(peer, expiredTestKeys(t))})
	s2 := NewSavingsService(other)
	s2.now = s.now
	var calls atomic.Int32
	r.SetSavingsExpiredOutbox(expiredQueue{db: db, store: store, before: func(_ context.Context, in SavingsExpiredRequest) error {
		calls.Add(1)
		if !in.Date.After(end) || in.ExpiresAt.Sub(in.Date) != 24*time.Hour {
			return errors.New("decision not DB clock")
		}
		return nil
	}})
	var wg sync.WaitGroup
	counts := make(chan int, 8)
	errs := make(chan error, 8)
	for i := range 8 {
		which := s
		if i%2 == 1 {
			which = s2
		}
		wg.Go(func() { n, e := which.ExpireContracts(t.Context()); counts <- n; errs <- e })
	}
	wg.Wait()
	close(counts)
	close(errs)
	sum := 0
	for n := range counts {
		sum += n
	}
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if sum != 1 || expiredSQLCount(t, db, `SELECT count(*) FROM savings_expired_term`) != 1 || expiredSQLCount(t, db, `SELECT count(*) FROM mail_outbox_event`) != 1 {
		t.Fatal("duplicate expiration", sum)
	}
	c, e := r.SavingsContractByID(t.Context(), id)
	if e != nil || c.Status != SavingsStatusExpired {
		t.Fatal("status", e)
	}
	var generation string
	var gotEnd, date, expiry time.Time
	if e = db.Pool.QueryRow(t.Context(), `SELECT term_generation,source_end,decision_date,expires_at FROM savings_expired_term`).Scan(&generation, &gotEnd, &date, &expiry); e != nil {
		t.Fatal(e)
	}
	canonical := end.UTC().Truncate(time.Microsecond)
	hash := sha256.Sum256([]byte(canonical.Format(time.RFC3339Nano)))
	if generation != hex.EncodeToString(hash[:]) || !gotEnd.Equal(canonical) || expiry.Sub(date) != 24*time.Hour || date.Before(end) {
		t.Fatal("identity/date")
	}
	if n, e := s.ExpireContracts(t.Context()); e != nil || n != 0 {
		t.Fatal("re-expired", n, e)
	}
}
func expiredTestKeys(t *testing.T) *textcrypt.BlobKeys {
	t.Helper()
	k, e := textcrypt.NewBlobKeys("fixture", map[string]string{"fixture": strings.Repeat("test-only-", 4)})
	if e != nil {
		t.Fatal(e)
	}
	return k
}

func TestSavingsExpiredRollbackEveryAtomicEffectAndPanic(t *testing.T) {
	for _, mode := range []string{"missing-profile", "key", "render", "enqueue", "panic", "cancel", "status", "sidecar"} {
		t.Run(mode, func(t *testing.T) {
			db, store, r, s, profile, id, end := expiredSetup(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			var reminderID string
			if e := db.WithTx(t.Context(), func(tc context.Context) error {
				q, e := store.EnqueueTx(tc, mailoutbox.Reminder{ContractID: id, ExpiryGeneration: "old", WindowDays: 7, EndDate: now.Add(time.Hour), Date: now, Recipients: []mailoutbox.Recipient{{Key: "recipient-00", Address: "one@example.invalid"}}, From: "sender@example.invalid", TextBody: "reminder"})
				if e == nil {
					reminderID = q.IDs[0]
				}
				return e
			}); e != nil {
				t.Fatal(e)
			}
			n := ReminderNotification{ID: pgdoc.NewID(), ResourceType: ResourceTypeSavingsContract, TargetResourceID: id, Status: ReminderStatusInProgress, ExpiryGeneration: reminderGeneration(end), Notifications: []ReminderNotificationDetail{{DaysBefore: 7, OutboxIDs: []string{reminderID}}}}
			if _, e := db.C("reminderNotification").InsertOne(t.Context(), n); e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "missing-profile":
				if _, e := db.C("billingProfile").DeleteByID(t.Context(), profile); e != nil {
					t.Fatal(e)
				}
			case "key":
				r.SetSavingsExpiredOutbox(expiredQueue{db: db, store: mailoutbox.New(db, nil)})
			case "render":
				r.SetSavingsExpiredOutbox(expiredQueue{db: db, store: store, before: func(context.Context, SavingsExpiredRequest) error { return errors.New("render failed") }})
			case "enqueue":
				r.SetSavingsExpiredOutbox(expiredQueue{db: db, store: store, after: func(context.Context) error { return errors.New("enqueue failed") }})
			case "panic":
				r.SetSavingsExpiredOutbox(expiredQueue{db: db, store: store, after: func(context.Context) error { panic("fixture") }})
			case "cancel":
				r.SetReminderCanceler(failingExpiredCanceler{})
			case "status":
				if _, e := db.Pool.Exec(t.Context(), `CREATE FUNCTION fixture_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture'; END $$; CREATE TRIGGER fixture_fail BEFORE UPDATE ON "savingsContract" FOR EACH ROW EXECUTE FUNCTION fixture_fail()`); e != nil {
					t.Fatal(e)
				}
			case "sidecar":
				if e := r.EnsureSavingsExpired(t.Context()); e != nil {
					t.Fatal(e)
				}
				if _, e := db.Pool.Exec(t.Context(), `CREATE FUNCTION fixture_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture'; END $$; CREATE TRIGGER fixture_fail BEFORE INSERT ON savings_expired_term FOR EACH ROW EXECUTE FUNCTION fixture_fail()`); e != nil {
					t.Fatal(e)
				}
			}
			panicked := false
			var err error
			func() {
				defer func() {
					if recover() != nil {
						panicked = true
					}
				}()
				_, err = s.ExpireContracts(t.Context())
			}()
			if mode == "panic" {
				if !panicked {
					t.Fatal("no panic")
				}
			} else if err == nil {
				t.Fatal("no failure")
			}
			c, e := r.SavingsContractByID(t.Context(), id)
			if e != nil || c.Status != SavingsStatusActive {
				t.Fatal("status escaped rollback", e)
			}
			var got ReminderNotification
			if found, e := db.C("reminderNotification").Get(t.Context(), n.ID, &got); e != nil || !found || got.Status != ReminderStatusInProgress {
				t.Fatal("reminder cancellation escaped rollback", e)
			}
			if expiredSQLCount(t, db, `SELECT count(*) FROM savings_expired_term`) != 0 || expiredSQLCount(t, db, `SELECT count(*) FROM mail_outbox WHERE kind=$1`, mailoutbox.SavingsExpiredKind) != 0 || expiredSQLCount(t, db, `SELECT count(*) FROM mail_outbox WHERE id=$1 AND state='pending'`, reminderID) != 1 {
				t.Fatal("partial decision escaped rollback")
			}
		})
	}
}

type failingExpiredCanceler struct{}

func (failingExpiredCanceler) CancelPendingTx(context.Context, []string) error {
	return errors.New("cancel failed")
}

type expiredNotifier struct {
	db    *pgdoc.DB
	calls int
	t     *testing.T
	crash bool
}

func (n *expiredNotifier) SendTemplate(ctx context.Context, key string, _ []string, _ map[string]any) error {
	n.t.Helper()
	if pgdoc.RejectInheritedTransaction(ctx) != nil {
		n.t.Fatal("SMTP inside SQL")
	}
	if key != "savings_contract_expired" || expiredSQLCount(n.t, n.db, `SELECT count(*) FROM savings_expired_term`) != 1 {
		n.t.Fatal("legacy before durable marker")
	}
	n.calls++
	if n.crash {
		panic("postcommit crash")
	}
	return errors.New("best effort")
}
func TestSavingsExpiredLegacyCrashNoAdoptionAndDurableOFFNoFallback(t *testing.T) {
	for _, mode := range []string{"legacy-crash", "no-mail", "durable"} {
		t.Run(mode, func(t *testing.T) {
			db, _, r, s, _, id, _ := expiredSetup(t)
			fake := &expiredNotifier{db: db, t: t, crash: mode == "legacy-crash"}
			if mode != "durable" {
				r.SetSavingsExpiredOutbox(nil)
			}
			if mode != "no-mail" {
				s.SetNotifier(fake)
			}
			if mode == "legacy-crash" {
				panicked := false
				func() { defer func() { panicked = recover() != nil }(); _, _ = s.ExpireContracts(t.Context()) }()
				if !panicked || fake.calls != 1 {
					t.Fatal("postcommit crash fixture")
				}
			} else {
				expireOne(t, s)
			}
			for _, on := range []bool{false, true, false} {
				if on {
					r.SetSavingsExpiredOutbox(expiredQueue{db: db, store: mailoutbox.New(db, expiredTestKeys(t))})
				} else {
					r.SetSavingsExpiredOutbox(nil)
				}
				s.SetNotifier(fake)
				fake.crash = false
				if e := r.WithSavingsContractMutation(t.Context(), id, func(tc context.Context) error {
					_, e := db.C("savingsContract").SetByID(tc, id, pgdoc.M{"status": SavingsStatusActive}, nil)
					return e
				}); e != nil {
					t.Fatal(e)
				}
				expireOne(t, s)
			}
			want := 0
			if mode == "legacy-crash" {
				want = 1
			}
			if fake.calls != want || expiredSQLCount(t, db, `SELECT count(*) FROM savings_expired_term`) != 1 {
				t.Fatal("legacy replay/adoption", fake.calls)
			}
			wantEvents := 0
			if mode == "durable" {
				wantEvents = 1
			}
			if expiredSQLCount(t, db, `SELECT count(*) FROM mail_outbox_event`) != wantEvents {
				t.Fatal("same-term adopted")
			}
		})
	}
}
func TestSavingsExpiredHistoricalTombstoneRollbackAndNewEnd(t *testing.T) {
	db, store, r, s, _, id, end := expiredSetup(t)
	expiredUpdate(t, db, id, pgdoc.M{"status": SavingsStatusExpired})
	fake := &expiredNotifier{db: db, t: t}
	s.SetNotifier(fake)
	if n, e := s.ExpireContracts(t.Context()); e != nil || n != 0 || expiredSQLCount(t, db, `SELECT count(*) FROM savings_expired_term`) != 0 {
		t.Fatal("historical EXPIRED backfill", n, e)
	}
	sentinel := errors.New("bad edit")
	if e := r.WithSavingsContractMutation(t.Context(), id, func(context.Context) error { return sentinel }); !errors.Is(e, sentinel) || expiredSQLCount(t, db, `SELECT count(*) FROM savings_expired_term`) != 0 {
		t.Fatal("failed edit retained tombstone", e)
	}
	for _, on := range []bool{true, false} {
		if on {
			r.SetSavingsExpiredOutbox(expiredQueue{db: db, store: store})
		} else {
			r.SetSavingsExpiredOutbox(nil)
		}
		if e := r.WithSavingsContractMutation(t.Context(), id, func(tc context.Context) error {
			_, e := db.C("savingsContract").SetByID(tc, id, pgdoc.M{"status": SavingsStatusActive}, nil)
			return e
		}); e != nil {
			t.Fatal(e)
		}
		expireOne(t, s)
	}
	if fake.calls != 0 || expiredSQLCount(t, db, `SELECT count(*) FROM mail_outbox_event`) != 0 || expiredSQLCount(t, db, `SELECT count(*) FROM savings_expired_term WHERE selection='no_mail' AND cancelled_at IS NOT NULL`) != 1 {
		t.Fatal("historical replay")
	}
	r.SetSavingsExpiredOutbox(expiredQueue{db: db, store: store})
	newEnd := end.Add(time.Minute)
	if e := r.WithSavingsContractMutation(t.Context(), id, func(tc context.Context) error {
		_, e := db.C("savingsContract").SetByID(tc, id, pgdoc.M{"status": SavingsStatusActive, "endDate": newEnd}, nil)
		return e
	}); e != nil {
		t.Fatal(e)
	}
	expireOne(t, s)
	if expiredSQLCount(t, db, `SELECT count(*) FROM savings_expired_term`) != 2 || expiredSQLCount(t, db, `SELECT count(*) FROM mail_outbox_event`) != 1 {
		t.Fatal("new end lost availability")
	}
}

func TestSavingsExpiredAllFourWritersKeylessOFFAndHistoryRetention(t *testing.T) {
	for _, mode := range []string{"client-cancel", "client-extend", "admin-update", "admin-delete", "upfront", "boot"} {
		t.Run(mode, func(t *testing.T) {
			db, store, r, s, profile, id, _ := expiredSetup(t)
			expireOne(t, s)
			outID := expiredID(t, db)
			cl, e := store.Claim(t.Context(), time.Minute)
			if e != nil || cl == nil {
				t.Fatal(e)
			}
			// Model bypass/full-replace public writers, which must never erase the private history.
			c, e := r.SavingsContractByID(t.Context(), id)
			if e != nil {
				t.Fatal(e)
			}
			c.Status = SavingsStatusActive
			if mode == "upfront" {
				c.Status = SavingsStatusPendingPayment
				c.PaidUpfront = true
				c.UpfrontAmount = decimalPtr("10.00")
			}
			if mode == "boot" {
				c.PaidUpfront = true
				c.MonthlyCommittedAmount = decimalPtr("12.00")
				c.DiscountRate = decimalPtr("10")
				end := time.Now().UTC().Add(time.Hour)
				c.EndDate = &end
			}
			if e = r.SaveSavingsContract(t.Context(), c); e != nil {
				t.Fatal(e)
			}
			off := NewRepo(db)
			off.SetReminderCanceler(mailoutbox.New(db, nil))
			off.SetSavingsExpiredCanceler(mailoutbox.New(db, nil))
			switch mode {
			case "client-cancel", "client-extend":
				_, e = off.MutateSavingsContract(t.Context(), profile, id, mode == "client-extend")
			case "admin-update":
				e = off.WithSavingsContractMutation(t.Context(), id, func(tc context.Context) error {
					_, e := db.C("savingsContract").SetByID(tc, id, pgdoc.M{"billingProfileId": "new-profile", "status": SavingsStatusActive}, nil)
					return e
				})
			case "admin-delete":
				e = off.WithSavingsContractMutation(t.Context(), id, func(tc context.Context) error { _, e := db.C("savingsContract").DeleteByID(tc, id); return e })
			case "upfront":
				var won bool
				won, e = off.ActivatePaidSavingsContract(t.Context(), c, "fixture-txn", time.Now().UTC())
				if !won || c.Status != SavingsStatusActive {
					t.Fatal("activation winner/result", e)
				}
			case "boot":
				var ids []string
				ids, e = off.HoldUnpaidUpfrontContracts(t.Context(), time.Now().UTC())
				if len(ids) != 1 || ids[0] != id {
					t.Fatal("hold winner", e)
				}
			}
			if e != nil {
				t.Fatal(e)
			}
			if expiredSQLCount(t, db, `SELECT count(*) FROM mail_outbox WHERE id=$1 AND state='expired' AND reason='cancelled'`, outID) != 1 || expiredSQLCount(t, db, `SELECT count(*) FROM savings_expired_term WHERE cancelled_at IS NOT NULL AND cardinality(outbox_ids)>0`) != 1 {
				t.Fatal("writer did not cancel and retain history")
			}
			if _, e = store.BeginDispatch(t.Context(), cl); !errors.Is(e, mailoutbox.ErrLeaseLost) {
				t.Fatal("old claim survives", e)
			}
			if _, e = db.Pool.Exec(t.Context(), `DELETE FROM savings_expired_term`); e == nil {
				t.Fatal("term deletion allowed")
			}
			if _, e = db.Pool.Exec(t.Context(), `UPDATE savings_expired_term SET queue_state='tampered'`); e == nil {
				t.Fatal("selection rewritten")
			}
		})
	}
}
func decimalPtr(s string) *decimal.Decimal { d := decimal.RequireFromString(s); return &d }

func TestSavingsExpiredNoOpKeepsSelectionAndPostIntentLateReceipt(t *testing.T) {
	for _, phase := range []string{"pending", "DISPATCHING", "UNKNOWN", "accepted"} {
		t.Run(phase, func(t *testing.T) {
			db, store, r, s, _, id, _ := expiredSetup(t)
			expireOne(t, s)
			outID := expiredID(t, db)
			if e := r.WithSavingsContractMutation(t.Context(), id, func(tc context.Context) error {
				_, e := db.C("savingsContract").SetByID(tc, id, pgdoc.M{"savingsPlanName": "render-only"}, nil)
				return e
			}); e != nil {
				t.Fatal(e)
			}
			if expiredSQLCount(t, db, `SELECT count(*) FROM savings_expired_term WHERE cancelled_at IS NULL`) != 1 {
				t.Fatal("no-op cancelled")
			}
			if phase == "pending" {
				return
			}
			cl, e := store.Claim(t.Context(), time.Minute)
			if e != nil || cl == nil {
				t.Fatal(e)
			}
			if _, e = store.BeginDispatch(t.Context(), cl); e != nil {
				t.Fatal(e)
			}
			receipt := mailoutbox.Receipt{Delivery: mailoutbox.RelayAccepted, Stage: mailoutbox.StageReply, Class: mailoutbox.ClassOK}
			if phase == "UNKNOWN" {
				if _, e = db.Pool.Exec(t.Context(), `UPDATE mail_outbox SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, outID); e != nil {
					t.Fatal(e)
				}
				if _, e = store.Claim(t.Context(), time.Minute); e != nil {
					t.Fatal(e)
				}
			}
			if phase == "accepted" {
				if e = store.RecordOutcome(t.Context(), cl, receipt); e != nil {
					t.Fatal(e)
				}
			}
			off := NewRepo(db)
			off.SetReminderCanceler(mailoutbox.New(db, nil))
			off.SetSavingsExpiredCanceler(mailoutbox.New(db, nil))
			if e = off.WithSavingsContractMutation(t.Context(), id, func(tc context.Context) error { _, e := db.C("savingsContract").DeleteByID(tc, id); return e }); e != nil {
				t.Fatal(e)
			}
			if expiredSQLCount(t, db, `SELECT count(*) FROM mail_outbox WHERE id=$1 AND state=$2`, outID, phase) != 1 {
				t.Fatal("postintent state rewritten")
			}
			if e = store.RecordOutcome(t.Context(), cl, receipt); e != nil {
				t.Fatal("late exact receipt denied", e)
			}
			if expiredSQLCount(t, db, `SELECT count(*) FROM mail_outbox WHERE id=$1 AND state='accepted'`, outID) != 1 {
				t.Fatal("late ACK not durable")
			}
		})
	}
}

func TestSavingsExpiredOwnedBoundaryRejectsInheritedBothDB(t *testing.T) {
	db, _, r, s, profile, id, _ := expiredSetup(t)
	other := expiredPG(t)
	for _, outer := range []*pgdoc.DB{db, other} {
		if e := outer.WithTx(t.Context(), func(tc context.Context) error {
			for _, run := range []func() error{func() error { _, e := s.ExpireContracts(tc); return e }, func() error { _, e := r.MutateSavingsContract(tc, profile, id, true); return e }, func() error { return r.WithSavingsContractMutation(tc, id, func(context.Context) error { return nil }) }, func() error { return r.EnsureSavingsExpired(tc) }, func() error {
				c, _ := r.SavingsContractByID(t.Context(), id)
				_, e := r.ActivatePaidSavingsContract(tc, c, "fixture", time.Now())
				return e
			}} {
				if e := run(); !errors.Is(e, pgdoc.ErrInheritedTransaction) {
					return fmt.Errorf("inherited not rejected: %v", e)
				}
			}
			return nil
		}); e != nil {
			t.Fatal(e)
		}
	}
	if expiredSQLCount(t, db, `SELECT count(*) FROM mail_outbox_event`) != 0 {
		t.Fatal("inherited partial state")
	}
}

func TestSavingsExpiredQueuedRRWriterSeesCommittedEndAndProfile(t *testing.T) {
	for _, mode := range []string{"end", "profile"} {
		t.Run(mode, func(t *testing.T) {
			db, store, r, _, _, id, _ := expiredSetup(t)
			if e := r.withReminderContract(t.Context(), id, func(context.Context, *SavingsContract) error { return nil }); e != nil {
				t.Fatal(e)
			}
			peer := expiredPeer(t, db)
			other := NewRepo(peer)
			other.SetReminderCanceler(mailoutbox.New(peer, nil))
			other.SetSavingsExpiredCanceler(mailoutbox.New(peer, nil))
			other.SetSavingsExpiredOutbox(expiredQueue{db: peer, store: mailoutbox.New(peer, expiredTestKeys(t))})
			s := NewSavingsService(other)
			// Ensure all prerequisites before the deliberate writer barrier; the worker
			// waits on the SAME contract advisory under a pool defaulted to RR.
			locked := make(chan struct{})
			release := make(chan struct{})
			writerDone := make(chan error, 1)
			go func() {
				writerDone <- db.WithTx(t.Context(), func(tc context.Context) error {
					if e := db.LockXact(tc, "savings-reminder:"+id); e != nil {
						return e
					}
					close(locked)
					<-release
					set := pgdoc.M{}
					if mode == "end" {
						set["endDate"] = time.Now().UTC().Add(time.Hour)
					} else {
						set["billingProfileId"] = "new-profile"
						if _, e := db.C("billingProfile").InsertOne(tc, pgdoc.M{"_id": "new-profile", "email": "new@example.invalid", "currency": "EUR"}); e != nil {
							return e
						}
					}
					_, e := db.C("savingsContract").SetByID(tc, id, set, nil)
					return e
				})
			}()
			<-locked
			done := make(chan error, 1)
			var count int
			go func() { var e error; count, e = s.ExpireContracts(t.Context()); done <- e }()
			deadline := time.After(10 * time.Second)
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			waiting := false
			for !waiting {
				select {
				case <-deadline:
					t.Fatal("worker never queued at advisory")
				case <-tick.C:
					var n int
					if e := db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted`).Scan(&n); e != nil {
						t.Fatal(e)
					}
					waiting = n > 0
				}
			}
			close(release)
			if e := <-writerDone; e != nil {
				t.Fatal(e)
			}
			if e := <-done; e != nil {
				t.Fatal(e)
			}
			if mode == "end" {
				if count != 0 || expiredSQLCount(t, db, `SELECT count(*) FROM savings_expired_term`) != 0 {
					t.Fatal("stale candidate poisoned new end")
				}
			} else {
				if count != 1 {
					t.Fatal("new profile not expired", count)
				}
				var p string
				if e := db.Pool.QueryRow(t.Context(), `SELECT profile_id FROM savings_expired_term`).Scan(&p); e != nil || p != "new-profile" {
					t.Fatal("stale profile selected", e)
				}
				cl, e := store.Claim(t.Context(), time.Minute)
				if e != nil || cl == nil {
					t.Fatal(e)
				}
				env, e := store.BeginDispatch(t.Context(), cl)
				if e != nil || env.To() != "new@example.invalid" {
					t.Fatal("stale recipient", e)
				}
			}
		})
	}
}

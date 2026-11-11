//go:build integration

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

func validationDB(t *testing.T) *pgdoc.DB {
	t.Helper()
	dsn := os.Getenv("VALIDATION_OUTBOX_TEST_PG")
	if dsn == "" {
		t.Skip("owned disposable loopback PostgreSQL17 required")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() != "127.0.0.1" {
		t.Fatal("fixture must be literal loopback")
	}
	base, err := pgdoc.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := "validation_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = base.Pool.Exec(t.Context(), `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := pgdoc.Connect(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close(context.Background())
		_, _ = base.Pool.Exec(context.Background(), `DROP DATABASE "`+name+`" WITH (FORCE)`)
		_ = base.Close(context.Background())
	})
	return db
}
func validationPeer(t *testing.T, db *pgdoc.DB, rr bool) *pgdoc.DB {
	t.Helper()
	cfg := db.Pool.Config()
	if rr {
		cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	}
	p, e := pgxpool.NewWithConfig(t.Context(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	d := &pgdoc.DB{Pool: p}
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	return d
}
func validationKeys(t *testing.T) *textcrypt.BlobKeys {
	t.Helper()
	k, e := textcrypt.NewBlobKeys("fixture", map[string]string{"fixture": strings.Repeat("test-only-", 4)})
	if e != nil {
		t.Fatal(e)
	}
	return k
}

type validationQueueFunc func(context.Context, ValidationApprovalRequest) (mailoutbox.QueueResult, error)

func (f validationQueueFunc) EnqueueValidationTx(c context.Context, v ValidationApprovalRequest) (mailoutbox.QueueResult, error) {
	return f(c, v)
}

type validationCancelFunc func(context.Context, []string) error

func (f validationCancelFunc) CancelPendingTx(c context.Context, v []string) error { return f(c, v) }

type validationTailSpy struct {
	db              *pgdoc.DB
	id              string
	calls, notifies int
	fail            bool
	t               *testing.T
}

func (s *validationTailSpy) Activate(ctx context.Context, _ *billing.BillingProfile, source string) error {
	s.t.Helper()
	s.calls++
	if source != billing.SourceValidation || pgdoc.RejectInheritedTransaction(ctx) != nil {
		s.t.Fatal("activation inherited TX/source")
	}
	var status string
	if e := s.db.Pool.QueryRow(ctx, `SELECT doc->>'status' FROM "billingProfileValidation" WHERE id=$1`, s.id).Scan(&status); e != nil || status != "APPROVED" {
		s.t.Fatal("activation before commit", e)
	}
	if s.fail {
		return errors.New("fake activation failed")
	}
	return nil
}
func (s *validationTailSpy) NotifyValidation(context.Context, *billing.BillingProfile) { s.notifies++ }

type validationHarness struct {
	db, peer *pgdoc.DB
	h        *Handler
	store    *mailoutbox.Store
	id, bp   string
	calls    atomic.Int64
	tail     *validationTailSpy
}

func newValidationHarness(t *testing.T) *validationHarness {
	t.Helper()
	db := validationDB(t)
	peer := validationPeer(t, db, true)
	for _, n := range []string{billingProfileValidationCollection, billingProfileCollection} {
		if e := db.C(n).Ensure(t.Context()); e != nil {
			t.Fatal(e)
		}
	}
	store := mailoutbox.New(db, validationKeys(t))
	if e := store.Ensure(t.Context()); e != nil {
		t.Fatal(e)
	}
	c := &validationHarness{db: db, peer: peer, store: store, id: pgdoc.NewID(), bp: pgdoc.NewID()}
	if _, e := db.C(billingProfileCollection).InsertOne(t.Context(), pgdoc.M{"_id": c.bp, "status": "NEW", "email": "One@example.invalid", "firstName": "Private", "contacts": []pgdoc.M{{"email": "one@example.invalid"}, {"email": "two@example.invalid"}}}); e != nil {
		t.Fatal(e)
	}
	if _, e := db.C(billingProfileValidationCollection).InsertOne(t.Context(), pgdoc.M{"_id": c.id, "status": "PENDING", "billingProfileId": c.bp, "document": "PRIVATE_KYC_NEVER_MAIL"}); e != nil {
		t.Fatal(e)
	}
	c.h = &Handler{repo: NewRepo(db), billing: billing.NewRepo(db), adminIssuer: "fixture"}
	c.h.SetValidationCanceler(store)
	c.h.SetValidationOutbox(validationQueueFunc(c.enqueue))
	c.tail = &validationTailSpy{db: peer, id: c.id, t: t}
	c.h.validationActivation = c.tail
	return c
}
func (c *validationHarness) enqueue(ctx context.Context, in ValidationApprovalRequest) (mailoutbox.QueueResult, error) {
	c.calls.Add(1)
	return c.store.EnqueueValidationTx(ctx, mailoutbox.ValidationApproved{ValidationID: in.ValidationID, ApprovalGeneration: in.ApprovalGeneration, Date: in.Date, ExpiresAt: in.ExpiresAt, Recipients: in.Recipients, FullName: in.FullName, LoginURL: "https://console.example.invalid/login", From: "sender@example.invalid", Subject: "Validation approved", TextBody: "Your billing profile validation has been approved. This does not confirm account activation or service readiness."})
}
func (c *validationHarness) route(t *testing.T, status string) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/", nil)
	rc := chi.NewRouteContext()
	rc.URLParams.Add("validationId", c.id)
	rc.URLParams.Add("status", status)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rc)
	ctx = httpx.WithRC(ctx, &httpx.RequestContext{SigV4KeyID: "fixture"})
	w := httptest.NewRecorder()
	c.h.billingProfileValidationStatus(w, req.WithContext(ctx))
	return w.Code
}
func (c *validationHarness) decision(t *testing.T) (string, *validationDelivery) {
	t.Helper()
	d, e := c.h.repo.FindDoc(t.Context(), billingProfileValidationCollection, c.id)
	if e != nil {
		t.Fatal(e)
	}
	v, e := readValidationDelivery(d)
	if e != nil {
		t.Fatal(e)
	}
	s, _ := d["status"].(string)
	return s, v
}
func (c *validationHarness) counts(t *testing.T, want int) {
	t.Helper()
	for _, table := range []string{"mail_outbox_event", "mail_outbox"} {
		var n int
		expected := want
		if table == "mail_outbox" {
			expected = want * 2
		}
		if e := c.db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&n); e != nil || n != expected {
			t.Fatalf("%s=%d want%d err%v", table, n, expected, e)
		}
	}
}

func TestValidationDecisionRollbackWrongDBInheritedAndInvisibility(t *testing.T) {
	for _, mode := range []string{"queue-error", "panic", "status-sql", "wrong-db", "key", "missing-profile", "inherited", "wrong-inherited", "visibility"} {
		t.Run(mode, func(t *testing.T) {
			c := newValidationHarness(t)
			switch mode {
			case "queue-error":
				c.h.SetValidationOutbox(validationQueueFunc(func(ctx context.Context, in ValidationApprovalRequest) (mailoutbox.QueueResult, error) {
					q, e := c.enqueue(ctx, in)
					if e != nil {
						return q, e
					}
					return q, errors.New("PRIVATE_ERROR")
				}))
			case "panic":
				c.h.SetValidationOutbox(validationQueueFunc(func(ctx context.Context, in ValidationApprovalRequest) (mailoutbox.QueueResult, error) {
					_, _ = c.enqueue(ctx, in)
					panic("fixture panic")
				}))
			case "status-sql":
				if _, e := c.db.Pool.Exec(t.Context(), `CREATE FUNCTION refuse_validation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'refuse'; END $$; CREATE TRIGGER refuse BEFORE UPDATE ON "billingProfileValidation" FOR EACH ROW EXECUTE FUNCTION refuse_validation()`); e != nil {
					t.Fatal(e)
				}
			case "wrong-db":
				c.store = mailoutbox.New(c.peer, validationKeys(t))
			case "key":
				c.store = mailoutbox.New(c.db, nil)
			case "missing-profile":
				_, _ = c.db.Pool.Exec(t.Context(), `DELETE FROM "billingProfile" WHERE id=$1`, c.bp)
			case "visibility":
				c.h.SetValidationOutbox(validationQueueFunc(func(ctx context.Context, in ValidationApprovalRequest) (mailoutbox.QueueResult, error) {
					q, e := c.enqueue(ctx, in)
					if e != nil {
						return q, e
					}
					var n int
					var s string
					if e = c.peer.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM mail_outbox)+(SELECT count(*) FROM mail_outbox_event),doc->>'status' FROM "billingProfileValidation" WHERE id=$1`, c.id).Scan(&n, &s); e != nil || n != 0 || s != "PENDING" {
						t.Fatal("uncommitted decision visible", e)
					}
					return q, errors.New("rollback visibility")
				}))
			}
			var err error
			if mode == "inherited" || mode == "wrong-inherited" {
				d := c.db
				if mode == "wrong-inherited" {
					d = c.peer
				}
				err = d.WithTx(t.Context(), func(tc context.Context) error { _, e := c.h.decideValidationStatus(tc, c.id, "APPROVED"); return e })
				if !errors.Is(err, pgdoc.ErrInheritedTransaction) {
					t.Fatal("inherited accepted", err)
				}
			} else if mode == "panic" {
				func() {
					defer func() {
						if recover() == nil {
							t.Error("panic not exercised")
						}
					}()
					_, _ = c.h.decideValidationStatus(t.Context(), c.id, "APPROVED")
				}()
				err = errors.New("panicked")
			} else {
				_, err = c.h.decideValidationStatus(t.Context(), c.id, "APPROVED")
			}
			if err == nil {
				t.Fatal("failure committed")
			}
			s, d := c.decision(t)
			if s != "PENDING" || d != nil || c.tail.calls != 0 {
				t.Fatal("status/marker/tail survived failure")
			}
			c.counts(t, 0)
		})
	}
}
func TestValidationConcurrentOneGenerationSnapshotAndNoRerender(t *testing.T) {
	c := newValidationHarness(t)
	h2 := &Handler{repo: NewRepo(c.peer), billing: billing.NewRepo(c.peer)}
	h2.SetValidationCanceler(mailoutbox.New(c.peer, nil))
	h2.SetValidationOutbox(validationQueueFunc(func(ctx context.Context, in ValidationApprovalRequest) (mailoutbox.QueueResult, error) {
		c.calls.Add(1)
		return mailoutbox.New(c.peer, validationKeys(t)).EnqueueValidationTx(ctx, mailoutbox.ValidationApproved{ValidationID: in.ValidationID, ApprovalGeneration: in.ApprovalGeneration, Date: in.Date, ExpiresAt: in.ExpiresAt, Recipients: in.Recipients, FullName: in.FullName, LoginURL: "https://console.example.invalid", From: "sender@example.invalid", Subject: "Approved", TextBody: "Validation approval only"})
	}))
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, h := range []*Handler{c.h, h2} {
		wg.Go(func() { _, e := h.decideValidationStatus(t.Context(), c.id, "APPROVED"); errs <- e })
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	_, before := c.decision(t)
	c.counts(t, 1)
	if c.calls.Load() != 1 {
		t.Fatal("duplicate snapshot", c.calls.Load())
	}
	c.h.SetValidationOutbox(validationQueueFunc(func(context.Context, ValidationApprovalRequest) (mailoutbox.QueueResult, error) {
		panic("duplicate render")
	}))
	if _, e := c.h.decideValidationStatus(t.Context(), c.id, "APPROVED"); e != nil {
		t.Fatal(e)
	}
	_, after := c.decision(t)
	b, _ := json.Marshal(before)
	a, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("original generation changed")
	}
}
func TestValidationRevocationStatesKeylessHistoryAndReapproval(t *testing.T) {
	for _, state := range []string{"before-claim", "claimed", "pre-intent", "DISPATCHING", "UNKNOWN", "accepted"} {
		t.Run(state, func(t *testing.T) {
			c := newValidationHarness(t)
			if c.route(t, "APPROVED") != 200 {
				t.Fatal("approval failed")
			}
			_, first := c.decision(t)
			var claim *mailoutbox.Claim
			var err error
			if state != "before-claim" {
				claim, err = c.store.Claim(t.Context(), time.Minute)
				if err != nil || claim == nil {
					t.Fatal(err)
				}
			}
			if state == "DISPATCHING" || state == "UNKNOWN" || state == "accepted" {
				e, err := c.store.BeginDispatch(t.Context(), claim)
				if err != nil || len(e.Wire()) == 0 {
					t.Fatal(err)
				}
				if state == "UNKNOWN" {
					err = c.store.RecordOutcome(t.Context(), claim, mailoutbox.Receipt{Delivery: mailoutbox.Indeterminate, Stage: mailoutbox.StageReply, Class: mailoutbox.ClassUncertain})
				}
				if state == "accepted" {
					err = c.store.RecordOutcome(t.Context(), claim, mailoutbox.Receipt{Delivery: mailoutbox.RelayAccepted, Stage: mailoutbox.StageReply, Class: mailoutbox.ClassOK})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			c.h.SetValidationOutbox(nil)
			c.h.SetValidationCanceler(mailoutbox.New(c.db, nil))
			revocation := "REJECTED"
			if state == "pre-intent" {
				revocation = "PENDING"
			}
			if c.route(t, revocation) != 200 {
				t.Fatal("keyless revocation failed")
			}
			_, rev := c.decision(t)
			if !rev.Revoked || rev.Generation != first.Generation {
				t.Fatal("evidence erased")
			}
			if claim != nil {
				s, e := c.store.GetStatus(t.Context(), claim.ID)
				if e != nil {
					t.Fatal(e)
				}
				if state == "claimed" || state == "pre-intent" {
					if s.State != mailoutbox.Expired {
						t.Fatal("claim not fenced")
					}
					if _, e = c.store.BeginDispatch(t.Context(), claim); e == nil {
						t.Fatal("revoked preintent sent")
					}
				} else {
					if s.State != state {
						t.Fatalf("cannot unsend %s got%s", state, s.State)
					}
					if state != "accepted" {
						if e = c.store.RecordOutcome(t.Context(), claim, mailoutbox.Receipt{Delivery: mailoutbox.RelayAccepted, Stage: mailoutbox.StageReply, Class: mailoutbox.ClassOK}); e != nil {
							t.Fatal("late exact receipt denied", e)
						}
					}
				}
			}
			beforeNotify := c.tail.notifies
			if c.route(t, "APPROVED") != 200 || c.tail.notifies != beforeNotify {
				t.Fatal("durable OFF legacy fallback")
			}
			_, off := c.decision(t)
			if off.Generation != first.Generation || !off.Revoked {
				t.Fatal("OFF invented mail generation")
			}
			c.counts(t, 1)
			if c.route(t, "REJECTED") != 200 {
				t.Fatal("reject")
			}
			c.h.SetValidationOutbox(validationQueueFunc(c.enqueue))
			if c.route(t, "APPROVED") != 200 {
				t.Fatal("reapproval")
			}
			_, next := c.decision(t)
			if next.Generation == first.Generation || next.FirstGeneration != first.Generation || next.PriorGenerations != 1 || next.Revoked {
				t.Fatal("new generation/history incorrect")
			}
			c.counts(t, 2)
		})
	}
}
func TestValidationRouteHistoricalNoAdoptionLegacyOFFAndFailedActivationTruth(t *testing.T) {
	for _, mode := range []string{"historical-on", "legacy-off", "durable-failed-tail"} {
		t.Run(mode, func(t *testing.T) {
			c := newValidationHarness(t)
			switch mode {
			case "historical-on":
				_, _ = c.db.Pool.Exec(t.Context(), `UPDATE "billingProfileValidation" SET doc=jsonb_set(doc,'{status}','"APPROVED"') WHERE id=$1`, c.id)
			case "legacy-off":
				c.h.SetValidationOutbox(nil)
			case "durable-failed-tail":
				c.tail.fail = true
			}
			code := c.route(t, "APPROVED")
			want := 200
			if c.tail.fail {
				want = 500
			}
			if code != want || c.tail.calls != 1 {
				t.Fatal("activation tail result changed", code)
			}
			if mode == "legacy-off" {
				if c.tail.notifies != 1 || c.calls.Load() != 0 {
					t.Fatal("OFF legacy behavior lost")
				}
				c.counts(t, 0)
			} else if c.tail.notifies != 0 {
				t.Fatal("ON fallback to legacy")
			}
			if mode == "historical-on" {
				_, d := c.decision(t)
				if d != nil || c.calls.Load() != 0 {
					t.Fatal("adopted historical approval")
				}
				c.counts(t, 0)
			}
			if mode == "durable-failed-tail" {
				c.counts(t, 1)
				cl, e := c.store.Claim(t.Context(), time.Minute)
				if e != nil || cl == nil {
					t.Fatal(e)
				}
				env, e := c.store.BeginDispatch(t.Context(), cl)
				if e != nil || len(env.Wire()) == 0 {
					t.Fatal("truthful approval lost on activation failure", e)
				}
			}
		})
	}
}
func TestValidationCancelErrorRollsBackStatus(t *testing.T) {
	c := newValidationHarness(t)
	if c.route(t, "APPROVED") != 200 {
		t.Fatal("approve")
	}
	c.h.SetValidationCanceler(validationCancelFunc(func(context.Context, []string) error { return fmt.Errorf("fixture cancellation failure") }))
	if c.route(t, "REJECTED") != 500 {
		t.Fatal("cancel error hidden")
	}
	s, d := c.decision(t)
	if s != "APPROVED" || d.Revoked {
		t.Fatal("partial cancellation status")
	}
}

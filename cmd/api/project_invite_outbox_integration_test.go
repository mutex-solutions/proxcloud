//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/mail"
	"github.com/mutex-solutions/proxcloud/internal/platform/message"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/projectinvite"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// An explicit owned PostgreSQL DSN avoids testcontainers pulling images or using
// another actor's lab. The builder supplies its fresh loopback fixture only.
func inviteCompositionDB(t *testing.T) *pgdoc.DB {
	t.Helper()
	dsn := os.Getenv("PROJECT_INVITE_TEST_PG")
	if dsn == "" {
		t.Skip("set PROJECT_INVITE_TEST_PG to an owned disposable loopback PostgreSQL 17")
	}
	base, err := pgdoc.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := "invite_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = base.Pool.Exec(t.Context(), `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	// Rebuild the URL explicitly: pgx ConnConfig.ConnString retains the original
	// URL after changing its Database field, so using it would reuse the base DB.
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
func invitePeer(t *testing.T, db *pgdoc.DB, rr bool) *pgdoc.DB {
	t.Helper()
	cfg := db.Pool.Config()
	if rr {
		cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := &pgdoc.DB{Pool: pool}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return p
}
func inviteTestKeys(t *testing.T) *textcrypt.BlobKeys {
	t.Helper()
	k, e := textcrypt.NewBlobKeys("fixture", map[string]string{"fixture": strings.Repeat("owned-fixture-", 3)})
	if e != nil {
		t.Fatal(e)
	}
	return k
}

type inviteLegacySpy struct{ calls int }

func (s *inviteLegacySpy) SendTemplate(context.Context, string, []string, map[string]any) error {
	s.calls++
	return nil
}

type inviteComposition struct {
	db      *pgdoc.DB
	store   *mailoutbox.Store
	repo    *projectinvite.Repo
	handler *projectinvite.Handler
	adapter projectInviteOutboxAdapter
	pid     string
	actor   *user.User
	spy     *inviteLegacySpy
}

func newInviteComposition(t *testing.T) *inviteComposition {
	t.Helper()
	db := inviteCompositionDB(t)
	ctx := t.Context()
	if err := prepareProjectInviteDependencies(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := mailoutbox.New(db, inviteTestKeys(t))
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	messages := message.NewRepo(db)
	if err := messages.Create(ctx, &message.MessageTemplate{Key: "send_invite_to_project", MessageTitle: "Join {{projectName}}", MessageBody: `<a href="{{projectInviteUrl}}">Join {{email}}</a>`}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.C("thirdPartyIntegration").InsertOne(ctx, pgdoc.M{"thirdParty": "SMTP", "config": pgdoc.M{"domain": "smtp.example.invalid", "port": 587, "fromEmail": "sender@example.invalid", "noAuth": true}}); err != nil {
		t.Fatal(err)
	}
	service := mail.NewService(func(context.Context, string, map[string]any) (string, string, bool, error) {
		panic("second template read forbidden")
	}, func(context.Context) (mail.Mailer, string) { panic("legacy SMTP forbidden") }, "Fixture", nil)
	pr := project.NewRepo(db)
	p, err := pr.Insert(ctx, &project.Project{Name: "Project Fixture", Status: project.StatusEnabled})
	if err != nil {
		t.Fatal(err)
	}
	users := user.NewRepo(db)
	if err := db.C("users").Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	orgs := org.NewRepo(db)
	repo := projectinvite.NewRepo(db)
	if err := repo.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	repo.SetInviteCanceler(store)
	spy := &inviteLegacySpy{}
	h := projectinvite.NewHandler(repo, users, project.NewService(pr, orgs, nil, users, nil), orgs, nil, spy, "https://console.example.invalid")
	a := projectInviteOutboxAdapter{store: store, service: service, message: messages, config: mail.DurableConfigFromStore(db.C("thirdPartyIntegration"), nil)}
	h.SetInviteOutbox(a)
	return &inviteComposition{db: db, store: store, repo: repo, handler: h, adapter: a, pid: p.ID, actor: &user.User{Sub: "operator"}, spy: spy}
}
func inviteCounts(t *testing.T, c *inviteComposition, want int) {
	t.Helper()
	for _, table := range []string{`"projectInvite"`, `mail_outbox`, `mail_outbox_event`} {
		var n int
		if err := c.db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != want {
			t.Fatalf("count %s=%d want%d err%v", table, n, want, err)
		}
	}
}
func invitePersisted(t *testing.T, c *inviteComposition) (string, []string) {
	t.Helper()
	var token string
	var ids []string
	rows, err := c.db.Pool.Query(t.Context(), `SELECT doc->>'token',doc->'outboxIds' FROM "projectInvite" ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var tok string
		var raw []byte
		if err = rows.Scan(&tok, &raw); err != nil {
			t.Fatal(err)
		}
		var next []string
		if err = json.Unmarshal(raw, &next); err != nil {
			t.Fatal(err)
		}
		token = tok
		ids = append(ids, next...)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return token, ids
}
func inviteState(t *testing.T, c *inviteComposition, id, want string) {
	t.Helper()
	s, e := c.store.GetStatus(t.Context(), id)
	if e != nil || s.State != want {
		t.Fatalf("state %s want%s err%v", s.State, want, e)
	}
}
func waitInviteDB(t *testing.T, db *pgdoc.DB, query string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		var ready bool
		if err := db.Pool.QueryRow(ctx, query).Scan(&ready); err != nil {
			t.Fatal(err)
		}
		if ready {
			return
		}
		runtime.Gosched()
	}
}

func TestProjectInviteCompositionAtomicFailuresNoFallback(t *testing.T) {
	for _, failure := range []string{"no-keys", "missing-template", "render-error", "enqueue-sql", "final-persist", "panic"} {
		t.Run(failure, func(t *testing.T) {
			c := newInviteComposition(t)
			ctx := t.Context()
			a := c.adapter
			switch failure {
			case "no-keys":
				a.store = mailoutbox.New(c.db, nil)
			case "missing-template":
				if _, e := c.db.Pool.Exec(ctx, `DELETE FROM "messageTemplate"`); e != nil {
					t.Fatal(e)
				}
			case "render-error":
				a.service = nil
			case "enqueue-sql":
				if _, e := c.db.Pool.Exec(ctx, `CREATE FUNCTION fixture_enqueue_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'PRIVATE_TOKEN_SQL'; END $$; CREATE TRIGGER fixture_enqueue_fail BEFORE INSERT ON mail_outbox FOR EACH ROW EXECUTE FUNCTION fixture_enqueue_fail()`); e != nil {
					t.Fatal(e)
				}
			case "final-persist":
				if _, e := c.db.Pool.Exec(ctx, `CREATE FUNCTION fixture_persist_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'PRIVATE_TOKEN_PERSIST'; END $$; CREATE TRIGGER fixture_persist_fail BEFORE UPDATE ON "projectInvite" FOR EACH ROW EXECUTE FUNCTION fixture_persist_fail()`); e != nil {
					t.Fatal(e)
				}
			case "panic":
				a.config = func(context.Context) (mail.Config, error) { panic("fixture renderer panic") }
			}
			c.handler.SetInviteOutbox(a)
			var err error
			panicked := false
			func() {
				defer func() {
					if recover() != nil {
						panicked = true
					}
				}()
				err = c.handler.InviteToProject(ctx, c.actor, "one@example.invalid", c.pid)
			}()
			if failure == "panic" {
				if !panicked {
					t.Fatal("panic fixture not exercised")
				}
			} else if err == nil || strings.Contains(err.Error(), "PRIVATE_TOKEN") {
				t.Fatal("failure accepted or secret echoed", err)
			}
			inviteCounts(t, c, 0)
			if c.spy.calls != 0 {
				t.Fatal("legacy fallback invoked")
			}
		})
	}
}

func TestProjectInviteCompositionFrozenDecisionPrimaryInvisibilityAndManualEvents(t *testing.T) {
	c := newInviteComposition(t)
	ctx := t.Context()
	for _, db := range []*pgdoc.DB{c.db, invitePeer(t, c.db, false)} {
		if err := db.WithTx(ctx, func(tc context.Context) error {
			err := c.handler.InviteToProject(tc, c.actor, "one@example.invalid", c.pid)
			if !errors.Is(err, pgdoc.ErrInheritedTransaction) {
				return fmt.Errorf("inherited entry accepted: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	inviteCounts(t, c, 0)
	// Block the final business decision write AFTER enqueue; another pool cannot
	// observe either invite/token or sealed event until the common commit.
	blocker, err := c.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err = blocker.Exec(ctx, `LOCK TABLE mail_outbox_event IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.handler.InviteToProject(ctx, c.actor, "one@example.invalid", c.pid) }()
	waitInviteDB(t, c.db, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'INSERT INTO mail_outbox_event%')`)
	var n int
	if err = c.db.Pool.QueryRow(ctx, `SELECT count(*) FROM "projectInvite"`).Scan(&n); err != nil || n != 0 {
		t.Fatal("uncommitted invite/token visible", err)
	}
	if claim, err := c.store.Claim(ctx, time.Minute); err != nil || claim != nil {
		t.Fatal("uncommitted mail visible", err)
	}
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = c.handler.InviteToProject(ctx, c.actor, "one@example.invalid", c.pid); err != nil {
		t.Fatal(err)
	}
	inviteCounts(t, c, 2)
	var bad int
	if err = c.db.Pool.QueryRow(ctx, `SELECT count(*) FROM "projectInvite" i JOIN mail_outbox o ON o.id=(i.doc->'outboxIds'->>0)::uuid WHERE i.doc->>'deliveryMode'<>'project-invite-outbox/v1' OR o.kind<>'project-invite/v1' OR o.version<>1 OR o.expires_at>pgdoc_ts(i.doc->>'expiresAt') OR pgdoc_ts(i.doc->>'expiresAt')-pgdoc_ts(i.doc->>'date')<>interval '24 hours'`).Scan(&bad); err != nil || bad != 0 {
		t.Fatal("unfrozen expiry/identity", err)
	}
	_, ids := invitePersisted(t, c)
	if len(ids) != 2 || ids[0] == ids[1] || c.spy.calls != 0 {
		t.Fatal("manual events deduped or legacy called")
	}
	claim, err := c.store.Claim(ctx, time.Minute)
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	e, err := c.store.BeginDispatch(ctx, claim)
	if err != nil || len(e.Wire()) == 0 {
		t.Fatal("typed frozen invite did not dispatch", err)
	}
	var raw []byte
	if err = c.db.Pool.QueryRow(ctx, `SELECT to_jsonb(o)-'sealed'-'fingerprint' FROM mail_outbox o WHERE id=$1`, claim.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	token, _ := invitePersisted(t, c)
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), "one@example.invalid") {
		t.Fatal("credential in outbox metadata")
	}
}

func TestProjectInviteCompositionNonDeliveryAndLegacyOFF(t *testing.T) {
	for _, state := range []string{mailoutbox.Suppressed, mailoutbox.Blocked, mailoutbox.NoRecipient} {
		t.Run(state, func(t *testing.T) {
			c := newInviteComposition(t)
			ctx := t.Context()
			email := "one@example.invalid"
			if state == mailoutbox.Suppressed {
				if _, e := c.db.Pool.Exec(ctx, `UPDATE "messageTemplate" SET doc=jsonb_set(doc,'{disabled}','true')`); e != nil {
					t.Fatal(e)
				}
			}
			if state == mailoutbox.Blocked {
				if _, e := c.db.Pool.Exec(ctx, `UPDATE "thirdPartyIntegration" SET doc=jsonb_set(doc,'{config,domain}','""')`); e != nil {
					t.Fatal(e)
				}
			}
			if state == mailoutbox.NoRecipient {
				email = ""
			}
			if e := c.handler.InviteToProject(ctx, c.actor, email, c.pid); e != nil {
				t.Fatal(e)
			}
			inviteCounts(t, c, 1)
			_, ids := invitePersisted(t, c)
			inviteState(t, c, ids[0], state)
			if c.spy.calls != 0 {
				t.Fatal("non-delivery fell back")
			}
		})
	}
	c := newInviteComposition(t)
	ctx := t.Context()
	if e := c.handler.InviteToProject(ctx, c.actor, "one@example.invalid", c.pid); e != nil {
		t.Fatal(e)
	}
	c.handler.SetInviteOutbox(nil)
	c.repo.SetInviteCanceler(mailoutbox.New(c.db, nil))
	if _, e := c.repo.ByEmail(ctx, "one@example.invalid"); e != nil {
		t.Fatal(e)
	}
	if e := c.repo.DeleteByEmailAndProjectID(ctx, "one@example.invalid", c.pid); e != nil {
		t.Fatal(e)
	}
	if c.spy.calls != 0 {
		t.Fatal("existing durable invite resent when OFF")
	}
	if e := c.handler.InviteToProject(ctx, c.actor, "legacy@example.invalid", c.pid); e != nil {
		t.Fatal(e)
	}
	if c.spy.calls != 1 {
		t.Fatal("OFF did not preserve legacy new-invite notifier")
	}
	var mode string
	if e := c.db.Pool.QueryRow(ctx, `SELECT doc->>'deliveryMode' FROM "projectInvite"`).Scan(&mode); e != nil || mode != "legacy" {
		t.Fatal("legacy mode absent", e)
	}
}

func TestProjectInviteCompositionAcceptDeclineAllMatchingKeylessAndRollback(t *testing.T) {
	for _, action := range []string{"accept", "decline"} {
		t.Run(action, func(t *testing.T) {
			c := newInviteComposition(t)
			ctx := t.Context()
			// More than both the 20-cancel limit and 128-keyset page. No silent cap.
			for range 131 {
				if e := c.handler.InviteToProject(ctx, c.actor, "one@example.invalid", c.pid); e != nil {
					t.Fatal(e)
				}
			}
			token, ids := invitePersisted(t, c)
			c.handler.SetInviteOutbox(nil)
			c.repo.SetInviteCanceler(mailoutbox.New(c.db, nil))
			now := time.Now().UTC()
			if _, e := c.db.C("users").InsertOne(ctx, &user.User{Sub: "recipient", Email: "one@example.invalid", EmailConfirmedAt: &now}); e != nil {
				t.Fatal(e)
			}
			router := chi.NewRouter()
			c.handler.Routes(router)
			req := httptest.NewRequest("POST", "/project-invites/"+action+"/"+token, nil).WithContext(httpx.WithRC(ctx, &httpx.RequestContext{Sub: "recipient"}))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != 202 {
				t.Fatalf("%s=%d %s", action, w.Code, w.Body.String())
			}
			var n int
			if e := c.db.Pool.QueryRow(ctx, `SELECT count(*) FROM "projectInvite"`).Scan(&n); e != nil || n != 0 {
				t.Fatal("not all invites deleted", e)
			}
			for _, id := range ids {
				inviteState(t, c, id, mailoutbox.Expired)
			}
			if c.spy.calls != 0 {
				t.Fatal("durable existing rows resent")
			}
		})
	}
	c := newInviteComposition(t)
	ctx := t.Context()
	for range 2 {
		if e := c.handler.InviteToProject(ctx, c.actor, "one@example.invalid", c.pid); e != nil {
			t.Fatal(e)
		}
	}
	_, ids := invitePersisted(t, c)
	if _, e := c.db.Pool.Exec(ctx, `CREATE FUNCTION fixture_cancel_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'PRIVATE_CANCEL_TOKEN'; END $$; CREATE TRIGGER fixture_cancel_fail BEFORE UPDATE ON mail_outbox FOR EACH ROW EXECUTE FUNCTION fixture_cancel_fail()`); e != nil {
		t.Fatal(e)
	}
	if e := c.repo.DeleteByEmailAndProjectID(ctx, "one@example.invalid", c.pid); e == nil || strings.Contains(e.Error(), "PRIVATE_CANCEL_TOKEN") {
		t.Fatal("cancel failure ignored/leaked", e)
	}
	inviteCounts(t, c, 2)
	for _, id := range ids {
		inviteState(t, c, id, mailoutbox.Pending)
	}
}

func TestProjectInviteCompositionCancelIntentBothOrdersAndLateReceipt(t *testing.T) {
	for _, intentFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(intentFirst), func(t *testing.T) {
			c := newInviteComposition(t)
			ctx := t.Context()
			if e := c.handler.InviteToProject(ctx, c.actor, "one@example.invalid", c.pid); e != nil {
				t.Fatal(e)
			}
			_, ids := invitePersisted(t, c)
			worker := mailoutbox.New(invitePeer(t, c.db, false), inviteTestKeys(t))
			claim, e := worker.Claim(ctx, time.Minute)
			if e != nil || claim == nil {
				t.Fatal(e)
			}
			if intentFirst {
				if _, e = worker.BeginDispatch(ctx, claim); e != nil {
					t.Fatal(e)
				}
				if e = c.repo.DeleteByEmailAndProjectID(ctx, "one@example.invalid", c.pid); e != nil {
					t.Fatal(e)
				}
				inviteState(t, c, ids[0], mailoutbox.Dispatching)
				if _, e = c.db.Pool.Exec(ctx, `UPDATE mail_outbox SET lease_until=clock_timestamp()-interval '1 second'`); e != nil {
					t.Fatal(e)
				}
				if next, e := worker.Claim(ctx, time.Minute); e != nil || next != nil {
					t.Fatal("intent replayed", e)
				}
				inviteState(t, c, ids[0], mailoutbox.Unknown)
				// Expiry cleanup cannot turn UNKNOWN into released/replayable.
				if _, e = c.repo.ByEmail(ctx, "one@example.invalid"); e != nil {
					t.Fatal(e)
				}
				inviteState(t, c, ids[0], mailoutbox.Unknown)
				if e = worker.RecordOutcome(ctx, claim, mailoutbox.Receipt{Delivery: mailoutbox.RelayAccepted, Stage: mailoutbox.StageReply, Class: mailoutbox.ClassOK}); e != nil {
					t.Fatal(e)
				}
				inviteState(t, c, ids[0], mailoutbox.Accepted)
			} else {
				block, err := c.db.Pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer block.Rollback(context.Background())
				if _, err = block.Exec(ctx, `LOCK TABLE "projectInvite" IN ROW SHARE MODE`); err != nil {
					t.Fatal(err)
				}
				// repo holds the invite row and then waits on this outbox row. Worker T2
				// also waits; cancellation gets its lock first, invalidating stale epoch.
				if _, err = block.Exec(ctx, `SELECT id FROM mail_outbox WHERE id=$1 FOR UPDATE`, ids[0]); err != nil {
					t.Fatal(err)
				}
				cancel := make(chan error, 1)
				go func() { cancel <- c.repo.DeleteByEmailAndProjectID(ctx, "one@example.invalid", c.pid) }()
				waitInviteDB(t, c.db, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'WITH selected AS%')`)
				intent := make(chan error, 1)
				go func() { _, err := worker.BeginDispatch(ctx, claim); intent <- err }()
				if err = block.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if err = <-cancel; err != nil {
					t.Fatal(err)
				}
				if err = <-intent; !errors.Is(err, mailoutbox.ErrLeaseLost) {
					t.Fatal("stale claim reached SMTP envelope", err)
				}
				inviteState(t, c, ids[0], mailoutbox.Expired)
			}
		})
	}
}

func TestProjectInviteCompositionExpiryAndRepeatableReadQueuedWriter(t *testing.T) {
	c := newInviteComposition(t)
	ctx := t.Context()
	// Manufacture a frozen expired durable decision through the typed adapter,
	// not by mutating immutable outbox expiry.
	id := pgdoc.NewID()
	date := time.Now().UTC().Add(-25 * time.Hour).Truncate(time.Microsecond)
	if e := c.db.WithTx(ctx, func(tc context.Context) error {
		q, e := c.adapter.EnqueueInviteTx(tc, projectinvite.InviteRequest{InviteID: id, Email: "expired@example.invalid", ProjectName: "Expired", JoinURL: "https://console.example.invalid/?invite-token=EXPIRED_TOKEN", Date: date, ExpiresAt: date.Add(24 * time.Hour)})
		if e != nil {
			return e
		}
		_, e = c.db.C("projectInvite").InsertOne(tc, pgdoc.M{"_id": id, "email": "expired@example.invalid", "projectId": c.pid, "token": "EXPIRED_TOKEN", "date": date, "expiresAt": date.Add(24 * time.Hour), "deliveryMode": "project-invite-outbox/v1", "outboxIds": q.IDs})
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if inv, e := c.repo.ByEmailAndToken(ctx, "expired@example.invalid", "EXPIRED_TOKEN"); e != nil || inv != nil {
		t.Fatal("expired durable token still usable", e)
	}
	var n int
	if e := c.db.Pool.QueryRow(ctx, `SELECT count(*) FROM "projectInvite"`).Scan(&n); e != nil || n != 0 {
		t.Fatal("expired invite not purged", e)
	}
	peer := invitePeer(t, c.db, true)
	repo := projectinvite.NewRepo(peer)
	repo.SetInviteCanceler(mailoutbox.New(peer, nil))
	block, e := c.db.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer block.Rollback(context.Background())
	if _, e = block.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('project-invite-decisions-v1'))`); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- repo.DeleteByEmailAndProjectID(ctx, "queued@example.invalid", c.pid) }()
	waitInviteDB(t, c.db, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE 'SELECT pg_advisory_xact_lock%')`)
	manual := pgdoc.NewID()
	future := time.Now().UTC().Add(time.Hour)
	var outID string
	// The lock owner commits an invite while the queued default-RR deleter waits.
	// Reuse its actual transaction for both insert and typed core enqueue.
	// pgdoc context is deliberately NOT forged: a separate committed producer
	// under the already-held advisory lock is represented by direct fixture SQL.
	in := mailoutbox.Invite{InviteID: manual, Date: future.Add(-time.Hour), ExpiresAt: future, Recipient: mailoutbox.Recipient{Key: "recipient-00", Address: "queued@example.invalid"}, From: "sender@example.invalid", Subject: "Queued", TextBody: "JOIN"}
	// Enqueue in an owned business TX before the waiting delete's fresh snapshot.
	if e = c.db.WithTx(ctx, func(tc context.Context) error {
		q, err := c.store.EnqueueInviteTx(tc, in)
		if err == nil {
			outID = q.IDs[0]
		}
		return err
	}); e != nil {
		t.Fatal(e)
	}
	doc, _ := json.Marshal(pgdoc.M{"email": "queued@example.invalid", "projectId": c.pid, "token": "QUEUED_TOKEN", "expiresAt": future, "deliveryMode": "project-invite-outbox/v1", "outboxIds": []string{outID}})
	if _, e = block.Exec(ctx, `INSERT INTO "projectInvite" VALUES($1,$2)`, manual, doc); e != nil {
		t.Fatal(e)
	}
	if e = block.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	if e = c.db.Pool.QueryRow(ctx, `SELECT count(*) FROM "projectInvite"`).Scan(&n); e != nil || n != 0 {
		t.Fatal("queued RR delete missed freshly committed invite", e)
	}
	inviteState(t, c, outID, mailoutbox.Expired)
}

func TestProjectInviteCompositionConcurrentCreateAndDeleteSerializes(t *testing.T) {
	c := newInviteComposition(t)
	ctx := t.Context()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Go(func() { errs <- c.handler.InviteToProject(ctx, c.actor, "race@example.invalid", c.pid) })
	wg.Go(func() { errs <- c.repo.DeleteByEmailAndProjectID(ctx, "race@example.invalid", c.pid) })
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var invites, pending int
	if e := c.db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM "projectInvite"),(SELECT count(*) FROM mail_outbox WHERE state='pending')`).Scan(&invites, &pending); e != nil || invites != pending {
		t.Fatal("non-atomic decision race", e)
	}
}

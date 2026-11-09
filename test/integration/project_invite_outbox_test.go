//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

func inviteMail(id string) mailoutbox.Invite {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return mailoutbox.Invite{InviteID: id, Date: now, ExpiresAt: now.Add(24 * time.Hour), Recipient: mailoutbox.Recipient{Key: "recipient-00", Address: "one@example.invalid"}, From: "sender@example.invalid", Subject: "Join project", TextBody: "https://console.example.invalid/join-project?invite-token=PRIVATE_INVITE_TOKEN", HTMLBody: "<p>PRIVATE_INVITE_TOKEN</p>"}
}
func enqueueInvite(t *testing.T, db *pgdoc.DB, s *mailoutbox.Store, in mailoutbox.Invite) string {
	t.Helper()
	var q mailoutbox.QueueResult
	if err := db.WithTx(t.Context(), func(tc context.Context) error { var err error; q, err = s.EnqueueInviteTx(tc, in); return err }); err != nil {
		t.Fatal(err)
	}
	if len(q.IDs) != 1 {
		t.Fatal("not exactly one recipient")
	}
	return q.IDs[0]
}
func peerPG(t *testing.T, db *pgdoc.DB, repeatable bool) *pgdoc.DB {
	t.Helper()
	cfg := db.Pool.Config()
	if repeatable {
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

func TestProjectInviteOutboxExistingSavingsUpgradeTwiceConcurrentAndCiphertext(t *testing.T) {
	db := freshPG(t)
	ctx := t.Context()
	old, err := os.ReadFile("testdata/project-invite/old-schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Pool.Exec(ctx, string(old)); err != nil {
		t.Fatal(err)
	}
	keys := mailKeys(t)
	s := mailoutbox.New(db, keys)
	// Original single-kind schema is populated BEFORE new Ensure. Savings DTO,
	// preparation/sealing and its exact old AAD must remain readable after DDL.
	r := mailReminder("old-schema-savings")
	expected, err := mailoutbox.PrepareReminder(r)
	if err != nil {
		t.Fatal(err)
	}
	id := enqueueMail(t, db, s, r)
	var key string
	var sealed, fingerprint []byte
	if err = db.Pool.QueryRow(ctx, `SELECT key_id,sealed,fingerprint FROM mail_outbox WHERE id=$1`, id).Scan(&key, &sealed, &fingerprint); err != nil {
		t.Fatal(err)
	}
	pt, err := keys.Open(key, sealed, []byte(id+"\x00savings-reminder/v1\x001"))
	if err != nil {
		t.Fatal("old Savings AAD changed", err)
	}
	for range 2 {
		if err = s.Ensure(ctx); err != nil {
			t.Fatal(err)
		}
	}
	peer := peerPG(t, db, true)
	ss := mailoutbox.New(peer, keys)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, store := range []*mailoutbox.Store{s, ss} {
		wg.Go(func() { errs <- store.Ensure(ctx) })
	}
	wg.Wait()
	close(errs)
	for err = range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var after, afterFP []byte
	if err = db.Pool.QueryRow(ctx, `SELECT sealed,fingerprint FROM mail_outbox WHERE id=$1`, id).Scan(&after, &afterFP); err != nil || !bytes.Equal(after, sealed) || !bytes.Equal(afterFP, fingerprint) {
		t.Fatal("migration resealed old savings", err)
	}
	if again, err := keys.Open(key, after, []byte(id+"\x00savings-reminder/v1\x001")); err != nil || !bytes.Equal(again, pt) {
		t.Fatal("legacy plaintext changed")
	}
	claim := claimMail(t, s)
	e, err := s.BeginDispatch(ctx, claim)
	if err != nil || !bytes.Equal(e.Wire(), expected[0].Wire()) {
		t.Fatal("legacy wire changed", err)
	}
	if _, err = db.Pool.Exec(ctx, `UPDATE mail_outbox SET kind='project-invite/v1' WHERE id=$1`, id); err == nil {
		t.Fatal("immutable payload trigger lost")
	}
	for _, pair := range []struct {
		k string
		v int
	}{{"unknown/v1", 1}, {mailoutbox.InviteKind, 2}, {mailoutbox.Kind, 0}} {
		_, err = db.Pool.Exec(ctx, `INSERT INTO mail_outbox(id,event_key,recipient_key,kind,version,key_id,sealed,fingerprint,state,expires_at) SELECT $1,event_key,$2,$3,$4,key_id,sealed,fingerprint,state,expires_at FROM mail_outbox WHERE id=$5`, uuid.NewString(), strings.Repeat("c", 64), pair.k, pair.v, id)
		if err == nil {
			t.Fatal("schema admitted unknown pair")
		}
	}
	inviteID := enqueueInvite(t, db, s, inviteMail(uuid.NewString()))
	if inviteID == "" {
		t.Fatal("upgraded existing table still rejects invite")
	}
	// Controlled drift fails atomically, not a name-only trust of a weaker CHECK.
	if _, err = db.Pool.Exec(ctx, `ALTER TABLE mail_outbox DROP CONSTRAINT mail_outbox_family_v2; ALTER TABLE mail_outbox ADD CONSTRAINT mail_outbox_family_v2 CHECK(version=1)`); err != nil {
		t.Fatal(err)
	}
	if err = s.Ensure(ctx); err == nil {
		t.Fatal("weaker family check silently trusted")
	}
	if _, err = db.Pool.Exec(ctx, `UPDATE mail_outbox SET fingerprint=decode(repeat('00',32),'hex') WHERE id=$1`, id); err == nil {
		t.Fatal("failed migration removed immutable trigger")
	}
}

func TestProjectInviteOutboxAtomicRollbackWrongDBDedupAndPrivacy(t *testing.T) {
	db, s := outboxFixture(t)
	ctx := t.Context()
	in := inviteMail(uuid.NewString())
	if _, err := s.EnqueueInviteTx(ctx, in); err == nil {
		t.Fatal("standalone accepted")
	}
	other := peerPG(t, db, false)
	if err := other.WithTx(ctx, func(tc context.Context) error {
		if _, err := s.EnqueueInviteTx(tc, in); err == nil {
			return errors.New("other DB accepted")
		}
		if err := s.CancelPendingTx(tc, nil); err == nil {
			return errors.New("other DB cancellation accepted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.C("inviteBusiness").Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("rollback")
	if err := db.WithTx(ctx, func(tc context.Context) error {
		if _, err := db.C("inviteBusiness").InsertOne(tc, pgdoc.M{"_id": in.InviteID}); err != nil {
			return err
		}
		if _, err := s.EnqueueInviteTx(tc, in); err != nil {
			return err
		}
		if c, err := s.Claim(ctx, time.Minute); err != nil || c != nil {
			t.Fatal("uncommitted row visible", err)
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM mail_outbox)+(SELECT count(*) FROM mail_outbox_event)+(SELECT count(*) FROM "inviteBusiness")`).Scan(&count); err != nil || count != 0 {
		t.Fatal("rollback leaked rows", err)
	}
	var wg sync.WaitGroup
	ids := make(chan string, 2)
	errs := make(chan error, 2)
	for _, d := range []*pgdoc.DB{db, other} {
		wg.Go(func() {
			store := mailoutbox.New(d, mailKeys(t))
			var q mailoutbox.QueueResult
			err := d.WithTx(ctx, func(tc context.Context) error { var e error; q, e = store.EnqueueInviteTx(tc, in); return e })
			errs <- err
			if err == nil {
				ids <- q.IDs[0]
			}
		})
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	id := ""
	for v := range ids {
		if id != "" && id != v {
			t.Fatal("concurrent exact duplicate split")
		}
		id = v
	}
	for _, mutate := range []func(*mailoutbox.Invite){
		func(v *mailoutbox.Invite) { v.Subject += " changed" }, func(v *mailoutbox.Invite) { v.TextBody += " changed" }, func(v *mailoutbox.Invite) { v.HTMLBody += " changed" }, func(v *mailoutbox.Invite) { v.From = "other@example.invalid" }, func(v *mailoutbox.Invite) { v.Recipient.Address = "two@example.invalid" }, func(v *mailoutbox.Invite) { v.Recipient.Key += "1" }, func(v *mailoutbox.Invite) {
			v.Date = v.Date.Add(time.Nanosecond)
			v.ExpiresAt = v.ExpiresAt.Add(time.Nanosecond)
		}, func(v *mailoutbox.Invite) { v.ExpiresAt = v.ExpiresAt.Add(-time.Nanosecond) }, func(v *mailoutbox.Invite) { v.Disposition = mailoutbox.Blocked },
	} {
		changed := in
		mutate(&changed)
		if err := db.WithTx(ctx, func(tc context.Context) error { _, err := s.EnqueueInviteTx(tc, changed); return err }); !errors.Is(err, mailoutbox.ErrConflict) {
			t.Fatal("immutable change accepted", err)
		}
	}
	rotated, _ := textcrypt.NewBlobKeys("v2", map[string]string{"v1": strings.Repeat("test-only-", 4), "v2": strings.Repeat("new-test-", 4)})
	if again := enqueueInvite(t, db, mailoutbox.New(db, rotated), in); again != id {
		t.Fatal("rotation split identity")
	}
	manual := in
	manual.InviteID = uuid.NewString()
	if next := enqueueInvite(t, db, s, manual); next == id {
		t.Fatal("manual event deduped")
	}
	var event, recipient, metadata string
	if err := db.Pool.QueryRow(ctx, `SELECT event_key,recipient_key,(to_jsonb(o)-'sealed'-'fingerprint')::text FROM mail_outbox o WHERE id=$1`, id).Scan(&event, &recipient, &metadata); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(mailoutbox.InviteKind + "\x00" + in.InviteID))
	if event != hex.EncodeToString(digest[:]) {
		t.Fatal("event identity includes wrong fields")
	}
	status, _ := s.GetStatus(ctx, id)
	raw, _ := json.Marshal(status)
	for _, private := range []string{"PRIVATE_INVITE_TOKEN", "one@example.invalid", "sender@example.invalid", "Join project", "join-project"} {
		if strings.Contains(metadata+string(raw)+recipient, private) {
			t.Fatal("unsealed token/PII metadata")
		}
	}
}

func TestProjectInviteOutboxKindVersionKeyAndCrossSwapDenyBeforeIntent(t *testing.T) {
	for _, tamper := range []string{"unknown-kind", "version", "cross-kind", "key", "ciphertext", "cross-row", "event"} {
		t.Run(tamper, func(t *testing.T) {
			db, s := outboxFixture(t)
			ctx := t.Context()
			id := enqueueInvite(t, db, s, inviteMail(uuid.NewString()))
			claim := claimMail(t, s)
			// Explicitly local admin-only corruption fixture; production trigger forbids it.
			if _, err := db.Pool.Exec(ctx, `DROP TRIGGER mail_outbox_immutable_payload ON mail_outbox; ALTER TABLE mail_outbox DROP CONSTRAINT mail_outbox_family_v2; ALTER TABLE mail_outbox DROP CONSTRAINT mail_outbox_version_check`); err != nil {
				t.Fatal(err)
			}
			var sql string
			switch tamper {
			case "unknown-kind":
				sql = `UPDATE mail_outbox SET kind='caller-kind/v1' WHERE id=$1`
			case "version":
				sql = `UPDATE mail_outbox SET version=2 WHERE id=$1`
			case "cross-kind":
				sql = `UPDATE mail_outbox SET kind='savings-reminder/v1' WHERE id=$1`
			case "key":
				sql = `UPDATE mail_outbox SET key_id='missing' WHERE id=$1`
			case "ciphertext":
				sql = `UPDATE mail_outbox SET sealed=set_byte(sealed,20,get_byte(sealed,20)#1) WHERE id=$1`
			case "event":
				if _, err := db.Pool.Exec(ctx, `INSERT INTO mail_outbox_event VALUES($1,'v1',decode(repeat('00',32),'hex'))`, strings.Repeat("e", 64)); err != nil {
					t.Fatal(err)
				}
				sql = `UPDATE mail_outbox SET event_key='` + strings.Repeat("e", 64) + `' WHERE id=$1`
			case "cross-row":
				second := enqueueInvite(t, db, s, inviteMail(uuid.NewString()))
				if _, err := db.Pool.Exec(ctx, `UPDATE mail_outbox a SET sealed=b.sealed,fingerprint=b.fingerprint FROM mail_outbox b WHERE a.id=$1 AND b.id=$2`, id, second); err != nil {
					t.Fatal(err)
				}
			}
			if sql != "" {
				if _, err := db.Pool.Exec(ctx, sql, id); err != nil {
					t.Fatal(err)
				}
			}
			e, err := s.BeginDispatch(ctx, claim)
			if !errors.Is(err, mailoutbox.ErrPayload) || len(e.Wire()) != 0 {
				t.Fatalf("tamper produced plaintext/intent: %v", err)
			}
			var n int
			if err = db.Pool.QueryRow(ctx, `SELECT count(*) FROM mail_outbox_attempt`).Scan(&n); err != nil || n != 0 {
				t.Fatal("invalid payload reached intent", err)
			}
		})
	}
}

func TestProjectInviteOutboxExplicitNonDeliveryAndExpiry(t *testing.T) {
	db, s := outboxFixture(t)
	for _, state := range []string{mailoutbox.Suppressed, mailoutbox.Blocked, mailoutbox.NoRecipient} {
		in := inviteMail(uuid.NewString())
		in.Disposition = state
		in.From = ""
		if state == mailoutbox.NoRecipient {
			in.Recipient.Address = ""
		}
		id := enqueueInvite(t, db, s, in)
		wantMailState(t, s, id, state)
	}
	past := inviteMail(uuid.NewString())
	past.Date = time.Now().UTC().Add(-25 * time.Hour)
	past.ExpiresAt = past.Date.Add(24 * time.Hour)
	id := enqueueInvite(t, db, s, past)
	wantMailState(t, s, id, mailoutbox.Expired)
	if c, err := s.Claim(t.Context(), time.Minute); err != nil || c != nil {
		t.Fatal("non-delivery row claimable", err)
	}
}

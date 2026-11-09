//go:build integration

package mailoutbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

func validationPG(t *testing.T) *pgdoc.DB {
	t.Helper()
	dsn := os.Getenv("VALIDATION_OUTBOX_TEST_PG")
	if dsn == "" {
		t.Skip("owned disposable loopback PostgreSQL17 required")
	}
	u, e := url.Parse(dsn)
	if e != nil || u.Hostname() != "127.0.0.1" {
		t.Fatal("fixture must be literal loopback")
	}
	base, e := pgdoc.Connect(t.Context(), dsn)
	if e != nil {
		t.Fatal(e)
	}
	name := "validation_core_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
func validationPGPeer(t *testing.T, db *pgdoc.DB) *pgdoc.DB {
	t.Helper()
	cfg := db.Pool.Config()
	cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	p, e := pgxpool.NewWithConfig(t.Context(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	other := &pgdoc.DB{Pool: p}
	t.Cleanup(func() { _ = other.Close(context.Background()) })
	return other
}
func validationPGKeys(t *testing.T) *textcrypt.BlobKeys {
	t.Helper()
	k, e := textcrypt.NewBlobKeys("fixture", map[string]string{"fixture": strings.Repeat("test-only-", 4)})
	if e != nil {
		t.Fatal(e)
	}
	return k
}
func validationPGInput() ValidationApproved {
	v := validationFixture()
	v.Date = time.Now().UTC().Truncate(time.Microsecond)
	v.ExpiresAt = v.Date.Add(ValidationApprovalMaxAge)
	v.Recipients = []string{"one@example.invalid"}
	return v
}
func validationEnqueue(t *testing.T, db *pgdoc.DB, s *Store, v ValidationApproved) string {
	t.Helper()
	var q QueueResult
	if e := db.WithTx(t.Context(), func(tc context.Context) error { var e error; q, e = s.EnqueueValidationTx(tc, v); return e }); e != nil || len(q.IDs) != 1 {
		t.Fatal("enqueue", e, len(q.IDs))
	}
	return q.IDs[0]
}

func TestValidationMigrationTwoFamilyPopulatedCipherAADWireAndConcurrentRR(t *testing.T) {
	db := validationPG(t)
	ctx := t.Context()
	old, e := os.ReadFile("testdata/validation-approved/two-family-v1.sql")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Pool.Exec(ctx, string(old)); e != nil {
		t.Fatal(e)
	}
	keys := validationPGKeys(t)
	s := New(db, keys)
	now := time.Now().UTC().Truncate(time.Second)
	savings := Reminder{ContractID: "old-savings", ExpiryGeneration: "v1", Date: now, EndDate: now.Add(48 * time.Hour), WindowDays: 2, Recipients: []Recipient{{Key: "recipient-00", Address: "one@example.invalid"}}, From: "sender@example.invalid", Subject: "Saved savings", TextBody: "Saved"}
	invite := Invite{InviteID: uuid.NewString(), Date: now, ExpiresAt: now.Add(24 * time.Hour), Recipient: Recipient{Key: "recipient-00", Address: "two@example.invalid"}, From: "sender@example.invalid", Subject: "Saved invite", TextBody: "Saved invite credential"}
	var savingsID, inviteID string
	if e = db.WithTx(ctx, func(tc context.Context) error {
		q, e := s.EnqueueTx(tc, savings)
		if e != nil {
			return e
		}
		savingsID = q.IDs[0]
		q, e = s.EnqueueInviteTx(tc, invite)
		if e == nil {
			inviteID = q.IDs[0]
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
	type snapshot struct {
		id, kind, key        string
		sealed, fp, pt, wire []byte
	}
	expectedSavings, _ := PrepareReminder(savings)
	expectedInvite, _ := PrepareInvite(invite)
	snaps := []snapshot{{id: savingsID, kind: Kind, wire: expectedSavings[0].Wire()}, {id: inviteID, kind: InviteKind, wire: expectedInvite[0].Wire()}}
	for i := range snaps {
		v := &snaps[i]
		if e = db.Pool.QueryRow(ctx, `SELECT key_id,sealed,fingerprint FROM mail_outbox WHERE id=$1`, v.id).Scan(&v.key, &v.sealed, &v.fp); e != nil {
			t.Fatal(e)
		}
		v.pt, e = keys.Open(v.key, v.sealed, []byte(v.id+"\x00"+v.kind+"\x001"))
		if e != nil {
			t.Fatal("old AAD unreadable", e)
		}
	}
	peer := validationPGPeer(t, db)
	stores := []*Store{New(peer, keys), s}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, store := range stores {
		wg.Go(func() { errs <- store.Ensure(ctx) })
	}
	wg.Wait()
	close(errs)
	for e = range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if e = s.Ensure(ctx); e != nil {
		t.Fatal("second ensure", e)
	}
	var name string
	if e = db.Pool.QueryRow(ctx, `SELECT conname FROM pg_constraint WHERE conrelid='mail_outbox'::regclass AND conname IN ('mail_outbox_family_v1','mail_outbox_family_v2')`).Scan(&name); e != nil || name != "mail_outbox_family_v2" {
		t.Fatal("family replacement", e, name)
	}
	for _, v := range snaps {
		var sealed, fp []byte
		if e = db.Pool.QueryRow(ctx, `SELECT sealed,fingerprint FROM mail_outbox WHERE id=$1`, v.id).Scan(&sealed, &fp); e != nil || !bytes.Equal(sealed, v.sealed) || !bytes.Equal(fp, v.fp) {
			t.Fatal("migration resealed", e)
		}
		pt, e := keys.Open(v.key, sealed, []byte(v.id+"\x00"+v.kind+"\x001"))
		if e != nil || !bytes.Equal(pt, v.pt) {
			t.Fatal("plaintext changed", e)
		}
	}
	for range 2 {
		cl, e := s.Claim(ctx, time.Minute)
		if e != nil || cl == nil {
			t.Fatal(e)
		}
		env, e := s.BeginDispatch(ctx, cl)
		if e != nil {
			t.Fatal(e)
		}
		for _, v := range snaps {
			if v.id == cl.ID && !bytes.Equal(env.Wire(), v.wire) {
				t.Fatal("wire changed")
			}
		}
	}
	_ = validationEnqueue(t, db, s, validationPGInput())
	if _, e = db.Pool.Exec(ctx, `UPDATE mail_outbox SET kind='validation-approved/v1' WHERE id=$1`, savingsID); e == nil {
		t.Fatal("immutable trigger weakened")
	}
}
func TestValidationMigrationExactDriftFailsAtomically(t *testing.T) {
	for _, drift := range []string{"weak", "not-validated", "wrong-type", "missing", "v2-weak", "v2-unvalidated"} {
		t.Run(drift, func(t *testing.T) {
			db := validationPG(t)
			old, e := os.ReadFile("testdata/validation-approved/two-family-v1.sql")
			if e != nil {
				t.Fatal(e)
			}
			if _, e = db.Pool.Exec(t.Context(), string(old)); e != nil {
				t.Fatal(e)
			}
			setup := `ALTER TABLE mail_outbox DROP CONSTRAINT mail_outbox_family_v1; `
			switch drift {
			case "weak":
				setup += `ALTER TABLE mail_outbox ADD CONSTRAINT mail_outbox_family_v1 CHECK(version=1)`
			case "not-validated":
				setup += `ALTER TABLE mail_outbox ADD CONSTRAINT mail_outbox_family_v1 CHECK(kind IN ('savings-reminder/v1','project-invite/v1') AND version=1) NOT VALID`
			case "wrong-type":
				setup += `ALTER TABLE mail_outbox ADD CONSTRAINT mail_outbox_family_v1 UNIQUE(key_id,id)`
			case "missing":
			case "v2-weak":
				setup += `ALTER TABLE mail_outbox ADD CONSTRAINT mail_outbox_family_v2 CHECK(version=1)`
			case "v2-unvalidated":
				setup += `ALTER TABLE mail_outbox ADD CONSTRAINT mail_outbox_family_v2 CHECK(kind IN ('savings-reminder/v1','project-invite/v1','validation-approved/v1') AND version=1) NOT VALID`
			}
			if _, e = db.Pool.Exec(t.Context(), setup); e != nil {
				t.Fatal(e)
			}
			var before, after string
			query := `SELECT coalesce(jsonb_agg(jsonb_build_array(conname,contype,convalidated,pg_get_constraintdef(oid)) ORDER BY conname)::text,'[]') FROM pg_constraint WHERE conrelid='mail_outbox'::regclass`
			if e = db.Pool.QueryRow(t.Context(), query).Scan(&before); e != nil {
				t.Fatal(e)
			}
			if e = New(db, validationPGKeys(t)).Ensure(t.Context()); e == nil {
				t.Fatal("drift admitted")
			}
			if e = db.Pool.QueryRow(t.Context(), query).Scan(&after); e != nil || before != after {
				t.Fatal("failed migration not atomic", e)
			}
		})
	}
}
func TestValidationPayloadTamperAndImmutableEventConflicts(t *testing.T) {
	for _, tamper := range []string{"kind", "version", "cross-family", "ciphertext", "event", "recipient", "expiry", "canonical-unknown", "canonical-subject"} {
		t.Run(tamper, func(t *testing.T) {
			db := validationPG(t)
			keys := validationPGKeys(t)
			s := New(db, keys)
			if e := s.Ensure(t.Context()); e != nil {
				t.Fatal(e)
			}
			id := validationEnqueue(t, db, s, validationPGInput())
			cl, e := s.Claim(t.Context(), time.Minute)
			if e != nil || cl == nil {
				t.Fatal(e)
			}
			if _, e = db.Pool.Exec(t.Context(), `DROP TRIGGER mail_outbox_immutable_payload ON mail_outbox; ALTER TABLE mail_outbox DROP CONSTRAINT mail_outbox_family_v2; ALTER TABLE mail_outbox DROP CONSTRAINT mail_outbox_version_check`); e != nil {
				t.Fatal(e)
			}
			sql := ""
			switch tamper {
			case "kind":
				sql = `UPDATE mail_outbox SET kind='other/v1' WHERE id=$1`
			case "version":
				sql = `UPDATE mail_outbox SET version=2 WHERE id=$1`
			case "cross-family":
				sql = `UPDATE mail_outbox SET kind='project-invite/v1' WHERE id=$1`
			case "ciphertext":
				sql = `UPDATE mail_outbox SET sealed=set_byte(sealed,20,get_byte(sealed,20)#1) WHERE id=$1`
			case "recipient":
				sql = `UPDATE mail_outbox SET recipient_key=repeat('a',64) WHERE id=$1`
			case "expiry":
				sql = `UPDATE mail_outbox SET expires_at=expires_at+interval '1 second' WHERE id=$1`
			case "event":
				if _, e = db.Pool.Exec(t.Context(), `INSERT INTO mail_outbox_event VALUES(repeat('b',64),'fixture',decode(repeat('00',32),'hex'))`); e != nil {
					t.Fatal(e)
				}
				sql = `UPDATE mail_outbox SET event_key=repeat('b',64) WHERE id=$1`
			default:
				var key string
				var sealed []byte
				if e = db.Pool.QueryRow(t.Context(), `SELECT key_id,sealed FROM mail_outbox WHERE id=$1`, id).Scan(&key, &sealed); e != nil {
					t.Fatal(e)
				}
				aad, _ := payloadAAD(id, ValidationApprovedKind, Version)
				pt, e := keys.Open(key, sealed, aad)
				if e != nil {
					t.Fatal(e)
				}
				var v map[string]any
				_ = json.Unmarshal(pt, &v)
				if tamper == "canonical-unknown" {
					v["unknown"] = "unrecognized"
				} else {
					input := v["input"].(map[string]any)
					input["Subject"] = "drifted"
				}
				pt, _ = json.Marshal(v)
				k, seal, e := keys.Seal(pt, aad)
				if e != nil {
					t.Fatal(e)
				}
				fp, _ := keys.Fingerprint(k, pt)
				if _, e = db.Pool.Exec(t.Context(), `UPDATE mail_outbox SET key_id=$2,sealed=$3,fingerprint=$4 WHERE id=$1`, id, k, seal, fp); e != nil {
					t.Fatal(e)
				}
			}
			if sql != "" {
				if _, e = db.Pool.Exec(t.Context(), sql, id); e != nil {
					t.Fatal(e)
				}
			}
			env, e := s.BeginDispatch(t.Context(), cl)
			if !errors.Is(e, ErrPayload) || len(env.Wire()) != 0 {
				t.Fatal("tamper reached plaintext", e)
			}
			var n int
			if e = db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM mail_outbox_attempt`).Scan(&n); e != nil || n != 0 {
				t.Fatal("tamper reached intent", e)
			}
		})
	}
	db := validationPG(t)
	s := New(db, validationPGKeys(t))
	if e := s.Ensure(t.Context()); e != nil {
		t.Fatal(e)
	}
	v := validationPGInput()
	id := validationEnqueue(t, db, s, v)
	if again := validationEnqueue(t, db, s, v); id != again {
		t.Fatal("duplicate identity")
	}
	for _, change := range []func(*ValidationApproved){func(v *ValidationApproved) { v.Subject += "x" }, func(v *ValidationApproved) { v.Recipients = []string{"new@example.invalid"} }, func(v *ValidationApproved) { v.FullName += "x" }, func(v *ValidationApproved) { v.LoginURL += "/login" }, func(v *ValidationApproved) {
		v.Date = v.Date.Add(time.Nanosecond)
		v.ExpiresAt = v.ExpiresAt.Add(time.Nanosecond)
	}} {
		next := v
		change(&next)
		if e := db.WithTx(t.Context(), func(tc context.Context) error { _, e := s.EnqueueValidationTx(tc, next); return e }); !errors.Is(e, ErrConflict) {
			t.Fatal("immutable drift accepted", e)
		}
	}
	var metadata string
	if e := db.Pool.QueryRow(t.Context(), `SELECT (to_jsonb(o)-'sealed'-'fingerprint')::text FROM mail_outbox o WHERE id=$1`, id).Scan(&metadata); e != nil {
		t.Fatal(e)
	}
	for _, private := range []string{v.FullName, v.From, v.Subject, v.LoginURL, "one@example.invalid"} {
		if strings.Contains(metadata, private) {
			t.Fatal("metadata leaks PII")
		}
	}
}

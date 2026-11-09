//go:build integration

package mailoutbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

func expiredCorePG(t *testing.T) *pgdoc.DB {
	t.Helper()
	if v := os.Getenv("SAVINGS_EXPIRED_TEST_PG"); v != "" {
		t.Setenv("VALIDATION_OUTBOX_TEST_PG", v)
	}
	return validationPG(t)
}
func expiredCoreInput() SavingsExpired {
	v := expiredFixture()
	v.Date = time.Now().UTC().Truncate(time.Microsecond)
	v.ExpiresAt = v.Date.Add(SavingsExpiredMaxAge)
	v.Recipients = []string{"one@example.invalid"}
	return v
}
func expiredCoreEnqueue(t *testing.T, db *pgdoc.DB, s *Store, v SavingsExpired) string {
	t.Helper()
	var q QueueResult
	if e := db.WithTx(t.Context(), func(tc context.Context) error { var e error; q, e = s.EnqueueSavingsExpiredTx(tc, v); return e }); e != nil || len(q.IDs) != 1 {
		t.Fatal("enqueue", e)
	}
	return q.IDs[0]
}
func archivedThreeFamily(t *testing.T) string {
	t.Helper()
	raw, e := os.ReadFile("testdata/savings-expired/three-family-v2.go.txt")
	if e != nil {
		t.Fatal(e)
	}
	parts := strings.Split(string(raw), "`")
	if len(parts) != 3 {
		t.Fatal("archive format")
	}
	return parts[1]
}

func TestSavingsExpiredV3PopulatedThreeFamilyCipherPlainAADWireUnchangedConcurrentRR(t *testing.T) {
	db := expiredCorePG(t)
	ctx := t.Context()
	if _, e := db.Pool.Exec(ctx, archivedThreeFamily(t)); e != nil {
		t.Fatal(e)
	}
	keys := validationPGKeys(t)
	s := New(db, keys)
	now := time.Now().UTC().Truncate(time.Second)
	reminder := Reminder{ContractID: "old-savings", ExpiryGeneration: "v1", WindowDays: 2, Date: now, EndDate: now.Add(48 * time.Hour), Recipients: []Recipient{{Key: "recipient-00", Address: "one@example.invalid"}}, From: "sender@example.invalid", Subject: "saved reminder", TextBody: "saved"}
	invite := Invite{InviteID: uuid.NewString(), Date: now, ExpiresAt: now.Add(24 * time.Hour), Recipient: Recipient{Key: "recipient-00", Address: "two@example.invalid"}, From: "sender@example.invalid", Subject: "saved invite", TextBody: "saved invite"}
	validation := validationPGInput()
	validation.Recipients = []string{"three@example.invalid"}
	type snap struct {
		id, kind, key        string
		pt, sealed, fp, wire []byte
	}
	var snaps []snap
	re, _ := PrepareReminder(reminder)
	ie, _ := PrepareInvite(invite)
	ve, _ := PrepareValidationApproved(validation)
	if e := db.WithTx(ctx, func(tc context.Context) error {
		q, e := s.EnqueueTx(tc, reminder)
		if e != nil {
			return e
		}
		snaps = append(snaps, snap{id: q.IDs[0], kind: Kind, wire: re[0].Wire()})
		q, e = s.EnqueueInviteTx(tc, invite)
		if e != nil {
			return e
		}
		snaps = append(snaps, snap{id: q.IDs[0], kind: InviteKind, wire: ie[0].Wire()})
		q, e = s.EnqueueValidationTx(tc, validation)
		if e != nil {
			return e
		}
		snaps = append(snaps, snap{id: q.IDs[0], kind: ValidationApprovedKind, wire: ve[0].Wire()})
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	for i := range snaps {
		v := &snaps[i]
		if e := db.Pool.QueryRow(ctx, `SELECT key_id,sealed,fingerprint FROM mail_outbox WHERE id=$1`, v.id).Scan(&v.key, &v.sealed, &v.fp); e != nil {
			t.Fatal(e)
		}
		var e error
		v.pt, e = keys.Open(v.key, v.sealed, []byte(v.id+"\x00"+v.kind+"\x001"))
		if e != nil {
			t.Fatal("legacy AAD", e)
		}
	}
	peer := validationPGPeer(t, db)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for _, store := range []*Store{s, New(peer, keys), s, New(peer, keys)} {
		wg.Go(func() { errs <- store.Ensure(ctx) })
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if e := s.Ensure(ctx); e != nil {
		t.Fatal(e)
	}
	var family string
	if e := db.Pool.QueryRow(ctx, `SELECT conname FROM pg_constraint WHERE conrelid='mail_outbox'::regclass AND conname LIKE 'mail_outbox_family_%'`).Scan(&family); e != nil || family != "mail_outbox_family_v3" {
		t.Fatal("latest exact family", e)
	}
	for _, v := range snaps {
		var sealed, fp []byte
		if e := db.Pool.QueryRow(ctx, `SELECT sealed,fingerprint FROM mail_outbox WHERE id=$1`, v.id).Scan(&sealed, &fp); e != nil || !bytes.Equal(v.sealed, sealed) || !bytes.Equal(v.fp, fp) {
			t.Fatal("old payload resealed", e)
		}
		pt, e := keys.Open(v.key, sealed, []byte(v.id+"\x00"+v.kind+"\x001"))
		if e != nil || !bytes.Equal(v.pt, pt) {
			t.Fatal("old plaintext drift", e)
		}
	}
	for range 3 {
		cl, e := s.Claim(ctx, time.Minute)
		if e != nil || cl == nil {
			t.Fatal(e)
		}
		env, e := s.BeginDispatch(ctx, cl)
		if e != nil {
			t.Fatal(e)
		}
		for _, v := range snaps {
			if v.id == cl.ID && !bytes.Equal(v.wire, env.Wire()) {
				t.Fatal("old wire drift")
			}
		}
	}
	expiredCoreEnqueue(t, db, s, expiredCoreInput())
	if _, e := db.Pool.Exec(ctx, `UPDATE mail_outbox SET kind='savings-contract-expired/v1' WHERE id=$1`, snaps[0].id); e == nil {
		t.Fatal("immutable trigger weakened")
	}
}
func TestSavingsExpiredV3ExactConstraintDriftFailsRollback(t *testing.T) {
	for _, version := range []string{"v2", "v3"} {
		for _, drift := range []string{"missing", "weak", "not-validated", "wrong-type"} {
			t.Run(version+"/"+drift, func(t *testing.T) {
				db := expiredCorePG(t)
				if _, e := db.Pool.Exec(t.Context(), archivedThreeFamily(t)); e != nil {
					t.Fatal(e)
				}
				s := New(db, validationPGKeys(t))
				if version == "v3" {
					if e := s.Ensure(t.Context()); e != nil {
						t.Fatal(e)
					}
				}
				name := "mail_outbox_family_" + version
				setup := `ALTER TABLE mail_outbox DROP CONSTRAINT ` + name + `; `
				switch drift {
				case "weak":
					setup += `ALTER TABLE mail_outbox ADD CONSTRAINT ` + name + ` CHECK(version=1)`
				case "not-validated":
					kinds := `'savings-reminder/v1','project-invite/v1','validation-approved/v1'`
					if version == "v3" {
						kinds += `,'savings-contract-expired/v1'`
					}
					setup += `ALTER TABLE mail_outbox ADD CONSTRAINT ` + name + ` CHECK(kind IN (` + kinds + `) AND version=1) NOT VALID`
				case "wrong-type":
					setup += `ALTER TABLE mail_outbox ADD CONSTRAINT ` + name + ` UNIQUE(key_id,id)`
				}
				if _, e := db.Pool.Exec(t.Context(), setup); e != nil {
					t.Fatal(e)
				}
				query := `SELECT jsonb_agg(jsonb_build_array(conname,contype,convalidated,pg_get_constraintdef(oid)) ORDER BY conname)::text FROM pg_constraint WHERE conrelid='mail_outbox'::regclass`
				var before, after string
				if e := db.Pool.QueryRow(t.Context(), query).Scan(&before); e != nil {
					t.Fatal(e)
				}
				if e := s.Ensure(t.Context()); e == nil {
					t.Fatal("drift admitted")
				}
				if e := db.Pool.QueryRow(t.Context(), query).Scan(&after); e != nil || before != after {
					t.Fatal("migration did not rollback", e)
				}
			})
		}
	}
}
func TestSavingsExpiredV3TamperBeforeIntentAndEventDrift(t *testing.T) {
	for _, mode := range []string{"unknown-kind", "version", "reminder", "invite", "validation", "ciphertext", "event", "recipient", "expiry", "unknown-field", "subject"} {
		t.Run(mode, func(t *testing.T) {
			db := expiredCorePG(t)
			keys := validationPGKeys(t)
			s := New(db, keys)
			if e := s.Ensure(t.Context()); e != nil {
				t.Fatal(e)
			}
			v := expiredCoreInput()
			id := expiredCoreEnqueue(t, db, s, v)
			cl, e := s.Claim(t.Context(), time.Minute)
			if e != nil || cl == nil {
				t.Fatal(e)
			}
			if _, e = db.Pool.Exec(t.Context(), `DROP TRIGGER mail_outbox_immutable_payload ON mail_outbox; ALTER TABLE mail_outbox DROP CONSTRAINT mail_outbox_family_v3; ALTER TABLE mail_outbox DROP CONSTRAINT mail_outbox_version_check`); e != nil {
				t.Fatal(e)
			}
			sql := ""
			switch mode {
			case "unknown-kind":
				sql = `UPDATE mail_outbox SET kind='unknown/v1' WHERE id=$1`
			case "version":
				sql = `UPDATE mail_outbox SET version=2 WHERE id=$1`
			case "reminder":
				sql = `UPDATE mail_outbox SET kind='savings-reminder/v1' WHERE id=$1`
			case "invite":
				sql = `UPDATE mail_outbox SET kind='project-invite/v1' WHERE id=$1`
			case "validation":
				sql = `UPDATE mail_outbox SET kind='validation-approved/v1' WHERE id=$1`
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
				aad, _ := payloadAAD(id, SavingsExpiredKind, Version)
				pt, e := keys.Open(key, sealed, aad)
				if e != nil {
					t.Fatal(e)
				}
				var object map[string]any
				_ = json.Unmarshal(pt, &object)
				if mode == "unknown-field" {
					object["unknown"] = "ignored?"
				} else {
					object["input"].(map[string]any)["Subject"] = "drift"
				}
				pt, _ = json.Marshal(object)
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
				t.Fatal("tamper reached dispatch", e)
			}
			var intents int
			if e = db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM mail_outbox_attempt`).Scan(&intents); e != nil || intents != 0 {
				t.Fatal("tamper recorded intent", e)
			}
		})
	}
	db := expiredCorePG(t)
	s := New(db, validationPGKeys(t))
	if e := s.Ensure(t.Context()); e != nil {
		t.Fatal(e)
	}
	v := expiredCoreInput()
	id := expiredCoreEnqueue(t, db, s, v)
	if again := expiredCoreEnqueue(t, db, s, v); again != id {
		t.Fatal("exact reenqueue changed ID")
	}
	for _, change := range []func(*SavingsExpired){func(v *SavingsExpired) { v.Subject += "x" }, func(v *SavingsExpired) { v.Recipients = append(v.Recipients, "new@example.invalid") }, func(v *SavingsExpired) {
		v.Date = v.Date.Add(time.Microsecond)
		v.ExpiresAt = v.ExpiresAt.Add(time.Microsecond)
	}, func(v *SavingsExpired) { v.FullName += "x" }} {
		w := v
		change(&w)
		if e := db.WithTx(t.Context(), func(tc context.Context) error { _, e := s.EnqueueSavingsExpiredTx(tc, w); return e }); !errors.Is(e, ErrConflict) {
			t.Fatal("same-event changed immutable snapshot", e)
		}
	}
}

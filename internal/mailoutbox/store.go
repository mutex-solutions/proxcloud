package mailoutbox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

type Store struct {
	db   *pgdoc.DB
	keys *textcrypt.BlobKeys
}

func New(db *pgdoc.DB, keys *textcrypt.BlobKeys) *Store { return &Store{db: db, keys: keys} }

// Ensure requires the SAME writable primary as the owning business repositories.
// No replica handle is accepted for transitions or payload reads.
func (s *Store) Ensure(ctx context.Context) error {
	if err := pgdoc.RequireNoTransaction(ctx); err != nil {
		return err
	}
	return s.db.WithTx(ctx, func(tc context.Context) error {
		tx, err := s.db.Tx(tc)
		if err != nil {
			return err
		}
		// Take a fresh statement snapshot AFTER a competing schema writer commits.
		if _, err = tx.Exec(tc, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
			return err
		}
		if err = pgdoc.RequirePrimary(tc, tx); err != nil {
			return err
		}
		if err = s.db.LockXact(tc, "mail-outbox-schema-v1"); err != nil {
			return err
		}
		_, err = tx.Exec(tc, schema)
		return err
	})
}

// EnqueueTx never creates a hidden transaction. The event, all single-recipient
// rows, and caller's business decision commit or roll back together. Event-level
// keyed plaintext fingerprints detect ALL immutable changes, including additions
// or removals from the recipient set, even across key rotation/random nonces.
func (s *Store) EnqueueTx(ctx context.Context, r Reminder) (QueueResult, error) {
	if _, err := s.db.Tx(ctx); err != nil {
		return QueueResult{}, err
	}
	ps, err := prepare(r)
	if err != nil {
		return QueueResult{}, err
	}
	return s.persistPrepared(ctx, ps)
}

// persistPrepared is private: only the two typed fixed constructors can reach it.
func (s *Store) persistPrepared(ctx context.Context, ps []prepared) (QueueResult, error) {
	tx, err := s.db.Tx(ctx)
	if err != nil {
		return QueueResult{}, err
	}
	if len(ps) == 0 {
		return QueueResult{}, ErrInvalid
	}
	if s.keys == nil {
		return QueueResult{}, textcrypt.ErrBlobKey
	}
	all := make([]json.RawMessage, len(ps))
	for i := range ps {
		all[i] = ps[i].plaintext
	}
	canonical, _ := json.Marshal(all)
	keyID := s.keys.ActiveID()
	fp, err := s.keys.Fingerprint(keyID, canonical)
	if err != nil {
		return QueueResult{}, err
	}
	// Conflict DO NOTHING waits for an uncommitted concurrent event before the
	// subsequent fresh SELECT. It never overwrites previously selected content.
	_, err = tx.Exec(ctx, `INSERT INTO mail_outbox_event(event_key,key_id,fingerprint) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, ps[0].event, keyID, fp)
	if err != nil {
		return QueueResult{}, err
	}
	var savedFP []byte
	if err = tx.QueryRow(ctx, `SELECT key_id,fingerprint FROM mail_outbox_event WHERE event_key=$1 FOR UPDATE`, ps[0].event).Scan(&keyID, &savedFP); err != nil {
		return QueueResult{}, err
	}
	fp, err = s.keys.Fingerprint(keyID, canonical)
	if err != nil {
		return QueueResult{}, err
	}
	if !hmac.Equal(fp, savedFP) {
		return QueueResult{}, ErrConflict
	}
	result := QueueResult{}
	addResult := func(id, state string) {
		result.IDs = append(result.IDs, id)
		if result.State == "" {
			result.State = state
		} else if result.State != state {
			result.State = "mixed"
		}
	}
	for _, p := range ps {
		var id, state string
		err = tx.QueryRow(ctx, `SELECT id::text,state FROM mail_outbox WHERE event_key=$1 AND recipient_key=$2`, p.event, p.recipient).Scan(&id, &state)
		if err == nil {
			addResult(id, state)
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return QueueResult{}, err
		}
		id = uuid.NewString()
		aad, err := payloadAAD(id, p.kind, p.version)
		if err != nil {
			return QueueResult{}, err
		}
		sealKey, sealed, err := s.keys.Seal(p.plaintext, aad)
		if err != nil {
			return QueueResult{}, err
		}
		fingerprint, err := s.keys.Fingerprint(sealKey, p.plaintext)
		if err != nil {
			return QueueResult{}, err
		}
		// Expiry is decided by the database clock, not worker wall clock.
		err = tx.QueryRow(ctx, `INSERT INTO mail_outbox(id,event_key,recipient_key,kind,version,key_id,sealed,fingerprint,state,expires_at,reason)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,CASE WHEN $10<=clock_timestamp() THEN 'expired' ELSE $9 END,$10,CASE WHEN $10<=clock_timestamp() THEN 'expired' WHEN $9='pending' THEN '' ELSE $9 END) RETURNING state`, id, p.event, p.recipient, p.kind, p.version, sealKey, sealed, fingerprint, p.state, p.expiry).Scan(&state)
		if err != nil {
			return QueueResult{}, err
		}
		addResult(id, state)
	}
	return result, nil
}

func validTTL(ttl time.Duration) bool { return ttl >= 10*time.Millisecond && ttl <= 5*time.Minute }

// workerTx is intentionally private and only runs short database operations. It
// rejects a caller's owned transaction; BeginDispatch must COMMIT before network.
func (s *Store) workerTx(ctx context.Context, fn func(pgx.Tx) error) error {
	if err := pgdoc.RequireNoTransaction(ctx); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.db.Pool, fn)
}

// sweep makes ambiguity durable. A DISPATCHING worker may still own a LIVE SMTP
// socket, so expired intent is UNKNOWN forever, NEVER a claim candidate.
func sweep(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `WITH lost AS (
	 SELECT id FROM mail_outbox WHERE state='DISPATCHING' AND lease_until<=clock_timestamp() ORDER BY lease_until LIMIT 100 FOR UPDATE SKIP LOCKED
	), held AS (
	 UPDATE mail_outbox o SET state='UNKNOWN',reason='dispatch_lease_expired' FROM lost WHERE o.id=lost.id RETURNING o.*
	) INSERT INTO mail_outbox_attempt(outbox_id,attempt_id,epoch,phase,delivery,stage,class)
	 SELECT id,attempt_id,epoch,'unknown','unknown','reply','dispatch_lease_expired' FROM held ON CONFLICT DO NOTHING`)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `WITH old AS (
	 SELECT id FROM mail_outbox WHERE state IN ('pending','claimed','blocked_config','suppressed','no_recipient') AND expires_at<=clock_timestamp() LIMIT 100 FOR UPDATE SKIP LOCKED
	) UPDATE mail_outbox o SET state='expired',reason='expired',owner=NULL,lease_until=NULL FROM old WHERE o.id=old.id`)
	return err
}

// Claim uses DB-clock leases, fresh UUID ownership, monotonic epochs and SKIP
// LOCKED. Only pre-intent claimed leases can be rescued after a crash.
func (s *Store) Claim(ctx context.Context, ttl time.Duration) (*Claim, error) {
	if !validTTL(ttl) {
		return nil, ErrInvalid
	}
	var c *Claim
	err := s.workerTx(ctx, func(tx pgx.Tx) error {
		if err := sweep(ctx, tx); err != nil {
			return err
		}
		v := &Claim{Owner: uuid.NewString()}
		err := tx.QueryRow(ctx, `WITH candidate AS (
		 SELECT id FROM mail_outbox WHERE (state='pending' AND next_at<=clock_timestamp() OR state='claimed' AND lease_until<=clock_timestamp())
		 AND expires_at>clock_timestamp() AND attempts<8 ORDER BY next_at,id LIMIT 1 FOR UPDATE SKIP LOCKED
		) UPDATE mail_outbox o SET state='claimed',owner=$1,epoch=epoch+1,lease_until=clock_timestamp()+$2*interval '1 microsecond'
		 FROM candidate WHERE o.id=candidate.id AND o.expires_at>clock_timestamp()
		 RETURNING o.id::text,o.epoch,o.attempts`, v.Owner, ttl.Microseconds()).Scan(&v.ID, &v.Epoch, &v.Attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err == nil {
			c = v
		}
		return err
	})
	return c, err
}

// locked acquires the row FIRST, then obtains a fresh database time. Never put a
// TTL predicate only in a pre-wait snapshot (clock_timestamp is checked again).
func locked(ctx context.Context, tx pgx.Tx, c *Claim, states ...string) error {
	if c == nil {
		return ErrLeaseLost
	}
	var state, owner, attempt string
	var epoch int64
	var lease, expiry, now time.Time
	err := tx.QueryRow(ctx, `SELECT state,coalesce(owner::text,''),epoch,coalesce(attempt_id::text,''),coalesce(lease_until,'epoch'),expires_at FROM mail_outbox WHERE id=$1 FOR UPDATE`, c.ID).Scan(&state, &owner, &epoch, &attempt, &lease, &expiry)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	allowed := false
	for _, v := range states {
		if state == v {
			allowed = true
		}
	}
	if !allowed || owner != c.Owner || epoch != c.Epoch || !lease.After(now) || !expiry.After(now) || (state == Dispatching && attempt != c.AttemptID) {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) Renew(ctx context.Context, c *Claim, ttl time.Duration) error {
	if !validTTL(ttl) {
		return ErrInvalid
	}
	return s.workerTx(ctx, func(tx pgx.Tx) error {
		if err := locked(ctx, tx, c, Claimed, Dispatching); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE mail_outbox SET lease_until=clock_timestamp()+$4*interval '1 microsecond' WHERE id=$1 AND owner=$2 AND epoch=$3 AND lease_until>clock_timestamp() AND expires_at>clock_timestamp()`, c.ID, c.Owner, c.Epoch, ttl.Microseconds())
		if err == nil && tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		return err
	})
}

// BeginDispatch decrypts/authenticates the selected immutable envelope, appends
// intent and COMMITs T2. Only then may the caller open an SMTP connection.
func (s *Store) BeginDispatch(ctx context.Context, c *Claim) (Envelope, error) {
	var e Envelope
	err := s.workerTx(ctx, func(tx pgx.Tx) error {
		if err := locked(ctx, tx, c, Claimed); err != nil {
			return err
		}
		var key, kind, event, recipient string
		var version int
		var expiry time.Time
		var sealed, fp []byte
		if err := tx.QueryRow(ctx, `SELECT kind,version,event_key,recipient_key,expires_at,key_id,sealed,fingerprint FROM mail_outbox WHERE id=$1`, c.ID).Scan(&kind, &version, &event, &recipient, &expiry, &key, &sealed, &fp); err != nil {
			return err
		}
		// Validate the persisted pair BEFORE Open; never guess/fallback to savings.
		aad, err := payloadAAD(c.ID, kind, version)
		if err != nil || s.keys == nil {
			return ErrPayload
		}
		pt, err := s.keys.Open(key, sealed, aad)
		if err != nil {
			return ErrPayload
		}
		actual, err := s.keys.Fingerprint(key, pt)
		if err != nil || !hmac.Equal(actual, fp) {
			return ErrPayload
		}
		if kind == InviteKind && !validInvitePayload(pt, event, recipient, expiry) {
			return ErrPayload
		}
		var f struct {
			Payload frozen `json:"payload"`
		}
		if json.Unmarshal(pt, &f) != nil || len(f.Payload.Wire) == 0 || len(f.Payload.Wire) > MaxWireBytes || f.Payload.Disposition != Pending {
			return ErrPayload
		}
		// Revalidate sealed SMTP envelopes; corruption/missing key never sends.
		from, err := address(f.Payload.From)
		if err != nil {
			return ErrPayload
		}
		to, err := address(f.Payload.To)
		if err != nil {
			return ErrPayload
		}
		e = Envelope{from: from.Address, to: to.Address, wire: bytes.Clone(f.Payload.Wire)}
		attempt := uuid.NewString()
		tag, err := tx.Exec(ctx, `UPDATE mail_outbox SET state='DISPATCHING',attempt_id=$4,attempts=attempts+1,reason='' WHERE id=$1 AND state='claimed' AND owner=$2 AND epoch=$3 AND lease_until>clock_timestamp() AND expires_at>clock_timestamp() AND attempts<8`, c.ID, c.Owner, c.Epoch, attempt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		_, err = tx.Exec(ctx, `INSERT INTO mail_outbox_attempt(outbox_id,attempt_id,epoch,phase) VALUES($1,$2,$3,'intent')`, c.ID, attempt, c.Epoch)
		if err != nil {
			return err
		}
		// Journal insertion can wait on a relation lock AFTER the first CAS.
		// Recheck fresh DB time at the FINAL T2 write before committing intent.
		tag, err = tx.Exec(ctx, `UPDATE mail_outbox SET reason='' WHERE id=$1 AND state='DISPATCHING' AND owner=$2 AND epoch=$3 AND attempt_id=$4 AND lease_until>clock_timestamp() AND expires_at>clock_timestamp()`, c.ID, c.Owner, c.Epoch, attempt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		c.AttemptID = attempt
		c.Attempts++
		return nil
	})
	if err != nil {
		return Envelope{}, err
	}
	return e, nil
}

// BlockClaim records an explicit proven non-delivery BEFORE intent. It is not an
// acceptance ACK and there is no automatic release/resend endpoint in this phase.
func (s *Store) BlockClaim(ctx context.Context, c *Claim, reason SafeClass) error {
	if reason != ClassPayload && reason != ClassConfig {
		return ErrInvalid
	}
	return s.workerTx(ctx, func(tx pgx.Tx) error {
		if err := locked(ctx, tx, c, Claimed); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE mail_outbox SET state='blocked_config',reason=$4,owner=NULL,lease_until=NULL WHERE id=$1 AND owner=$2 AND epoch=$3 AND state='claimed' AND lease_until>clock_timestamp() AND expires_at>clock_timestamp()`, c.ID, c.Owner, c.Epoch, string(reason))
		if err == nil && tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		return err
	})
}

// RecordOutcome binds an append-only receipt to ONE immutable attempt. The SAME
// UNKNOWN attempt may acquire a late definitive receipt (even after TTL); there
// is no replay API. An older attempt can never update a newer owner/attempt.
func (s *Store) RecordOutcome(ctx context.Context, c *Claim, r Receipt) error {
	if c == nil || c.AttemptID == "" || !r.valid() {
		return ErrReceipt
	}
	return s.workerTx(ctx, func(tx pgx.Tx) error {
		var state, owner, attempt string
		var epoch int64
		var attempts int
		var expiry, now time.Time
		err := tx.QueryRow(ctx, `SELECT state,coalesce(owner::text,''),epoch,coalesce(attempt_id::text,''),attempts,expires_at FROM mail_outbox WHERE id=$1 FOR UPDATE`, c.ID).Scan(&state, &owner, &epoch, &attempt, &attempts, &expiry)
		if err != nil {
			return err
		}
		if owner != c.Owner || epoch != c.Epoch || attempt != c.AttemptID {
			return ErrReceipt
		}
		// Idempotent repeated definitive ACKs are safe; contradictory ACKs fail.
		var prev Receipt
		err = tx.QueryRow(ctx, `SELECT delivery,stage,class,permanent FROM mail_outbox_attempt WHERE attempt_id=$1 AND phase='receipt'`, c.AttemptID).Scan(&prev.Delivery, &prev.Stage, &prev.Class, &prev.Permanent)
		if err == nil {
			if prev == r {
				return nil
			}
			return ErrReceipt
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if state != Dispatching && state != Unknown {
			return ErrReceipt
		}
		if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		phase := "receipt"
		nextState := Pending
		reason := string(r.Class)
		var nextAt time.Time
		if r.Delivery == Indeterminate {
			phase = "unknown"
			nextState = Unknown
		} else if r.Delivery == RelayAccepted {
			nextState = Accepted
		} else if r.Class == ClassConfig || r.Class == ClassTLS {
			nextState = Blocked
		} else if !expiry.After(now) {
			nextState = Expired
		} else if r.Permanent || attempts >= MaxAttempts {
			nextState = Dead
		} else {
			nextAt = now.Add(retryDelay(attempts))
			if !nextAt.Before(expiry) {
				nextState = Expired
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO mail_outbox_attempt(outbox_id,attempt_id,epoch,phase,delivery,stage,class,permanent) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, c.ID, c.AttemptID, c.Epoch, phase, string(r.Delivery), string(r.Stage), string(r.Class), r.Permanent)
		if err != nil {
			return err
		}
		// Retain owner/attempt to authenticate repeated and late receipts. A future
		// safe pending acquisition changes UUID/epoch and rejects the old owner.
		_, err = tx.Exec(ctx, `UPDATE mail_outbox SET state=$4,reason=$5,next_at=coalesce($6,next_at),lease_until=CASE WHEN $4='UNKNOWN' THEN lease_until ELSE NULL END WHERE id=$1 AND owner=$2 AND epoch=$3 AND attempt_id=$7`, c.ID, c.Owner, c.Epoch, nextState, reason, nullableTime(nextAt), c.AttemptID)
		return err
	})
}
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func (s *Store) GetStatus(ctx context.Context, id string) (*Status, error) {
	if err := pgdoc.RequireNoTransaction(ctx); err != nil {
		return nil, err
	}
	var v Status
	err := s.db.Pool.QueryRow(ctx, `SELECT id::text,state,attempts,reason,created_at,expires_at FROM mail_outbox WHERE id=$1`, id).Scan(&v.ID, &v.State, &v.Attempts, &v.Reason, &v.CreatedAt, &v.ExpiresAt)
	return &v, err
}

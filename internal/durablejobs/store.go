// Package durablejobs is the primary-only job/outbox store for finite, in-tree
// handlers. It is not a broker, event bus, or arbitrary workflow executor.
package durablejobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

const (
	Billing  = "billing.charge/v1"
	Selftest = "selftest/v1"
	// TemplateUpdateVM imports a published VM artifact as a new Proxmox template
	// (internal/templateupdates). It is an EXTERNAL-effect kind: never registered on the DB-only
	// Worker; its runner fences every step with Checkpoint and resumes from the last receipt.
	TemplateUpdateVM = "templateupdate.vm/v1"
	// ProviderAttach attaches one cloud provider to a scoped set of existing projects
	// (internal/providerattach). EXTERNAL-effect kind: each project's provisioning is fenced by Checkpoint (intent before,
	// result after) and a new acquisition resumes from the per-project results.
	ProviderAttach = "providerattach.run/v1"
	// TemplateUpdateSmoke boots a throwaway clone of an imported (staged) template and checks it
	// (internal/templateupdates/smoke.go). External-effect kind, same runner and fencing.
	TemplateUpdateSmoke = "templateupdate.smoke/v1"
	// TemplateUpdateSandbox mirrors a built-in sandbox template image into the sandbox registry and points the
	// prepull DaemonSets at it, or rolls that back (internal/templateupdates, sandbox*.go). External-effect kind.
	TemplateUpdateSandbox = "templateupdate.sandbox/v1"
)

var (
	ErrLeaseLost = errors.New("durable job lease lost")
	ErrConflict  = errors.New("durable job identity conflicts with frozen payload")
	ErrReplay    = errors.New("job cannot be replayed (missing, not dead, or invalid payload)")
)

type Store struct{ db *pgdoc.DB }

func New(db *pgdoc.DB) *Store { return &Store{db: db} }

type Job struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Key         string          `json:"key"`
	Payload     json.RawMessage `json:"-"`
	State       string          `json:"state"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"maxAttempts"`
	Epoch       int64           `json:"epoch"`
	Owner       string          `json:"-"`
	LeaseUntil  *time.Time      `json:"leaseUntil,omitempty"`
	NextAt      time.Time       `json:"nextAt"`
	CreatedAt   time.Time       `json:"createdAt"`
	FinishedAt  *time.Time      `json:"finishedAt,omitempty"`
	LastError   string          `json:"lastError,omitempty"`
}

const columns = `id::text,kind,key,payload,state,attempts,max_attempts,epoch,coalesce(owner::text,''),lease_until,next_at,created_at,finished_at,last_error`

func scan(row pgx.Row) (*Job, error) {
	j := &Job{}
	err := row.Scan(&j.ID, &j.Kind, &j.Key, &j.Payload, &j.State, &j.Attempts, &j.MaxAttempts, &j.Epoch, &j.Owner, &j.LeaseUntil, &j.NextAt, &j.CreatedAt, &j.FinishedAt, &j.LastError)
	return j, err
}
func known(kind string) bool { return kind == Billing || kind == Selftest || External(kind) }

// External reports a kind whose handler performs external I/O. Such kinds are claimed only by a
// dedicated runner that renews its lease and commits each step through Checkpoint; the DB-only
// Worker refuses to register them.
func External(kind string) bool {
	return kind == TemplateUpdateVM || kind == ProviderAttach || kind == TemplateUpdateSmoke || kind == TemplateUpdateSandbox
}

// ClaimDeadKinds are the kinds ClaimDead re-acquires for a cleanup: the template update runner's
// (its dead-job pass cleans their Proxmox leftovers) and the sandbox mirror runner's (its dead-job
// pass deletes the job's worker pods and restores a pin the DaemonSets never ran). A dead bulk
// attach (ProviderAttach) is deliberately not one: it has no cleanup pass.
var ClaimDeadKinds = []string{TemplateUpdateVM, TemplateUpdateSmoke, TemplateUpdateSandbox}

// Checkpoint commits fn (DB-only writes: a step receipt) only while j's acquisition still owns an
// unexpired lease, exactly like Complete but without acknowledging the job. A runner calls it
// BEFORE starting an external effect (intent) and after it (result); ErrLeaseLost means another
// acquisition may own the job, so the caller must perform no further external step.
func (s *Store) Checkpoint(ctx context.Context, j *Job, fn func(context.Context) error) error {
	return s.db.WithTx(ctx, func(tc context.Context) error {
		tx, err := s.locked(tc, j)
		if err != nil {
			return err
		}
		if fn != nil {
			if err = fn(tc); err != nil {
				return err
			}
		}
		// Re-check under the row lock at commit time: a lease that expired while fn ran rolls back.
		var valid bool
		if err = tx.QueryRow(tc, `SELECT EXISTS(SELECT 1 FROM durable_job WHERE id=$1 AND state='running' AND owner=$2 AND epoch=$3 AND lease_until>clock_timestamp())`, j.ID, j.Owner, j.Epoch).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return ErrLeaseLost
		}
		return nil
	})
}

// ClaimDead re-acquires ONE dead external-kind job for its cleanup only: a new owner, a higher
// epoch and a lease, so cleanup is fenced exactly like a run. attempts is pinned to max_attempts,
// so the acquisition can never become a normal run again: a crash mid-cleanup expires it straight
// back to dead ("lease_expired"), and only an operator Replay (attempts reset) re-runs the job.
// Returns nil when the job is not dead (any more) or not an external kind.
func (s *Store) ClaimDead(ctx context.Context, id string, ttl time.Duration) (*Job, error) {
	if ttl <= 0 {
		return nil, errors.New("positive lease required")
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrReplay
	}
	j, err := scan(s.db.Pool.QueryRow(ctx, `UPDATE durable_job SET state='running',attempts=max_attempts,epoch=epoch+1,owner=$2,
	 lease_until=clock_timestamp()+$3*interval '1 microsecond',finished_at=NULL
	 WHERE id=$1 AND state='dead' AND kind=ANY($4) AND last_error IS DISTINCT FROM 'cleaned_up' RETURNING `+columns, id, uuid.NewString(), ttl.Microseconds(), ClaimDeadKinds))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// Fail ends an external-kind acquisition with a FIXED classification code (never a provider
// error string): permanent → dead now; otherwise pending again after delay (bounded by attempts).
func (s *Store) Fail(ctx context.Context, j *Job, delay time.Duration, permanent bool, code string) error {
	if delay < 0 || delay > time.Hour || !failCodeRE(code) {
		return errors.New("invalid failure")
	}
	return s.db.WithTx(ctx, func(tc context.Context) error {
		tx, err := s.locked(tc, j)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(tc, `UPDATE durable_job SET state=CASE WHEN attempts>=max_attempts OR $5 THEN 'dead' ELSE 'pending' END,
		 finished_at=CASE WHEN attempts>=max_attempts OR $5 THEN clock_timestamp() ELSE NULL END,
		 next_at=clock_timestamp()+$4*interval '1 microsecond',owner=NULL,lease_until=NULL,last_error=$6
		 WHERE id=$1 AND state='running' AND owner=$2 AND epoch=$3 AND lease_until>clock_timestamp()`, j.ID, j.Owner, j.Epoch, delay.Microseconds(), permanent, code)
		if err == nil && tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		return err
	})
}

// Defer returns an external-kind acquisition to pending after delay WITHOUT spending an attempt:
// the job could not start because a resource lock it needs is busy (e.g. one smoke test per
// provider), which is no failure of the job. Fenced like Fail; code is a fixed classification.
func (s *Store) Defer(ctx context.Context, j *Job, delay time.Duration, code string) error {
	if delay < 0 || delay > time.Hour || !failCodeRE(code) || !External(j.Kind) {
		return errors.New("invalid deferral")
	}
	return s.db.WithTx(ctx, func(tc context.Context) error {
		tx, err := s.locked(tc, j)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(tc, `UPDATE durable_job SET state='pending',attempts=greatest(attempts-1,0),finished_at=NULL,
		 next_at=clock_timestamp()+$4*interval '1 microsecond',owner=NULL,lease_until=NULL,last_error=$5
		 WHERE id=$1 AND state='running' AND owner=$2 AND epoch=$3 AND lease_until>clock_timestamp()`, j.ID, j.Owner, j.Epoch, delay.Microseconds(), code)
		if err == nil && tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		return err
	})
}

func failCodeRE(code string) bool {
	if code == "" || len(code) > 64 {
		return false
	}
	for _, c := range code {
		if !(c >= 'a' && c <= 'z' || c == '_') {
			return false
		}
	}
	return true
}

// Ensure runs before workers start, serialized across API replicas. There is no
// read pool on Store; every transition and transaction uses the supplied primary.
func (s *Store) Ensure(ctx context.Context) error {
	if err := pgdoc.RequirePrimary(ctx, s.db.Pool); err != nil {
		return err
	}
	return s.db.WithTx(ctx, func(tc context.Context) error {
		if err := s.db.LockXact(tc, "durablejobs-schema-v1"); err != nil {
			return err
		}
		tx, _ := s.db.Tx(tc)
		_, err := tx.Exec(tc, `CREATE TABLE IF NOT EXISTS durable_job (
		 id uuid PRIMARY KEY, kind text NOT NULL, key text NOT NULL,
		 payload jsonb NOT NULL CHECK(octet_length(payload::text)<=65536),
		 state text NOT NULL CHECK(state IN ('pending','running','succeeded','dead')),
		 attempts int NOT NULL DEFAULT 0, max_attempts int NOT NULL CHECK(max_attempts BETWEEN 1 AND 20),
		 epoch bigint NOT NULL DEFAULT 0, owner uuid, lease_until timestamptz,
		 next_at timestamptz NOT NULL DEFAULT clock_timestamp(),
		 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
		 finished_at timestamptz, last_error text NOT NULL DEFAULT '',
		 UNIQUE(kind,key), CHECK((state='running')=(owner IS NOT NULL AND lease_until IS NOT NULL)));
		 CREATE INDEX IF NOT EXISTS durable_job_pending ON durable_job(next_at,created_at,id) WHERE state='pending';
		 CREATE INDEX IF NOT EXISTS durable_job_expired ON durable_job(lease_until,id) WHERE state='running';
		 CREATE INDEX IF NOT EXISTS durable_job_billing_backlog ON durable_job((payload->>'profileId')) WHERE kind='billing.charge/v1' AND state IN ('pending','running','dead');
		 CREATE INDEX IF NOT EXISTS durable_job_finished ON durable_job(finished_at,id) WHERE state IN ('succeeded','dead');
		 CREATE TABLE IF NOT EXISTS durable_job_replay (
		 id uuid PRIMARY KEY, job_id uuid NOT NULL, actor text NOT NULL, reason text NOT NULL,
		 previous_attempts int NOT NULL, at timestamptz NOT NULL DEFAULT clock_timestamp());`)
		return err
	})
}

// EnqueueTx MUST join the caller's pgdoc.WithTx business write. Rollback removes
// both; it never opens a hidden second transaction. Same identity/different payload
// is rejected rather than silently replacing a previously accepted cutoff.
func (s *Store) EnqueueTx(ctx context.Context, kind, key string, payload []byte, maxAttempts int) (string, error) {
	if !known(kind) || key == "" || len(key) > 256 || !json.Valid(payload) || len(payload) > 65536 || maxAttempts < 1 || maxAttempts > 20 {
		return "", errors.New("invalid durable job")
	}
	tx, err := s.db.Tx(ctx)
	if err != nil {
		return "", err
	}
	id := uuid.NewString()
	var got string
	err = tx.QueryRow(ctx, `INSERT INTO durable_job(id,kind,key,payload,state,max_attempts) VALUES($1,$2,$3,$4,'pending',$5)
	 ON CONFLICT(kind,key) DO UPDATE SET key=durable_job.key
	 WHERE durable_job.payload=EXCLUDED.payload RETURNING id::text`, id, kind, key, payload, maxAttempts).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrConflict
	}
	return got, err
}
func (s *Store) Enqueue(ctx context.Context, kind, key string, payload []byte, maxAttempts int) (id string, err error) {
	err = s.db.WithTx(ctx, func(tc context.Context) error {
		var e error
		id, e = s.EnqueueTx(tc, kind, key, payload, maxAttempts)
		return e
	})
	return
}

// Claim leases one job with DB time and SKIP LOCKED. Every acquisition has a new
// UUID owner and retained increasing epoch, including a process-crash recovery.
func (s *Store) Claim(ctx context.Context, ttl time.Duration) (*Job, error) {
	return s.ClaimKinds(ctx, ttl, []string{Billing, Selftest})
}

// ClaimKinds leaves valid work for an unregistered provider/kind untouched. An
// external-billing deployment must not consume native money jobs as poison.
func (s *Store) ClaimKinds(ctx context.Context, ttl time.Duration, kinds []string) (*Job, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	for _, kind := range kinds {
		if !known(kind) {
			return nil, errors.New("unsupported job kind")
		}
	}
	if ttl <= 0 {
		return nil, errors.New("positive lease required")
	}
	// Bound crash poison recovery too: the final crashed acquisition becomes dead.
	_, err := s.db.Pool.Exec(ctx, `WITH expired AS (SELECT id FROM durable_job WHERE kind=ANY($1) AND state='running' AND lease_until<=clock_timestamp() AND attempts>=max_attempts ORDER BY lease_until LIMIT 100 FOR UPDATE SKIP LOCKED)
	 UPDATE durable_job j SET state='dead',owner=NULL,lease_until=NULL,finished_at=clock_timestamp(),last_error='lease_expired' FROM expired WHERE j.id=expired.id`, kinds)
	if err != nil {
		return nil, err
	}
	j, err := scan(s.db.Pool.QueryRow(ctx, `WITH picked AS (
	 SELECT id FROM durable_job WHERE kind=ANY($3) AND attempts<max_attempts AND ((state='pending' AND next_at<=clock_timestamp()) OR (state='running' AND lease_until<=clock_timestamp()))
	 ORDER BY next_at,created_at,id LIMIT 1 FOR UPDATE SKIP LOCKED)
	 UPDATE durable_job j SET state='running',attempts=attempts+1,epoch=epoch+1,owner=$1,
	 lease_until=clock_timestamp()+$2*interval '1 microsecond' FROM picked WHERE j.id=picked.id RETURNING `+prefixedColumns("j"), uuid.NewString(), ttl.Microseconds(), kinds))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return j, err
}
func prefixedColumns(p string) string {
	return fmt.Sprintf(`%s.id::text,%s.kind,%s.key,%s.payload,%s.state,%s.attempts,%s.max_attempts,%s.epoch,coalesce(%s.owner::text,''),%s.lease_until,%s.next_at,%s.created_at,%s.finished_at,%s.last_error`, p, p, p, p, p, p, p, p, p, p, p, p, p, p)
}

// locked checks time AFTER the row lock was acquired, not before a lock wait.
func (s *Store) locked(ctx context.Context, j *Job) (pgx.Tx, error) {
	tx, err := s.db.Tx(ctx)
	if err != nil {
		return nil, err
	}
	var until time.Time
	err = tx.QueryRow(ctx, `SELECT lease_until FROM durable_job WHERE id=$1 AND state='running' AND owner=$2 AND epoch=$3 FOR UPDATE`, j.ID, j.Owner, j.Epoch).Scan(&until)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLeaseLost
	}
	if err != nil {
		return nil, err
	}
	var valid bool
	if err = tx.QueryRow(ctx, `SELECT $1::timestamptz>clock_timestamp()`, until).Scan(&valid); err != nil {
		return nil, err
	}
	if !valid {
		return nil, ErrLeaseLost
	}
	return tx, nil
}
func (s *Store) Renew(ctx context.Context, j *Job, ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("positive lease required")
	}
	return s.db.WithTx(ctx, func(tc context.Context) error {
		tx, err := s.locked(tc, j)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(tc, `UPDATE durable_job SET lease_until=clock_timestamp()+$4*interval '1 microsecond' WHERE id=$1 AND owner=$2 AND epoch=$3 AND lease_until>clock_timestamp()`, j.ID, j.Owner, j.Epoch, ttl.Microseconds())
		if err == nil && tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		return err
	})
}

// Complete atomically commits a DB-ONLY handler, its receipt/outbox writes, and ACK.
// An expiry during work fails the final CAS and rolls the WHOLE transaction back.
// fn must not perform external I/O. External side effects need their own receipts
// and idempotency contract; adding mail is explicitly a separate integration.
func (s *Store) Complete(ctx context.Context, j *Job, fn func(context.Context) error) error {
	return s.db.WithTx(ctx, func(tc context.Context) error {
		tx, err := s.locked(tc, j)
		if err != nil {
			return err
		}
		if fn != nil {
			if err = fn(tc); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(tc, `UPDATE durable_job SET state='succeeded',finished_at=clock_timestamp(),owner=NULL,lease_until=NULL,last_error=''
		 WHERE id=$1 AND state='running' AND owner=$2 AND epoch=$3 AND lease_until>clock_timestamp()`, j.ID, j.Owner, j.Epoch)
		if err == nil && tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		return err
	})
}
func (s *Store) Retry(ctx context.Context, j *Job, delay time.Duration, permanent bool) error {
	if delay < 0 || delay > time.Hour {
		return errors.New("invalid retry delay")
	}
	return s.db.WithTx(ctx, func(tc context.Context) error {
		tx, err := s.locked(tc, j)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(tc, `UPDATE durable_job SET state=CASE WHEN attempts>=max_attempts OR $5 THEN 'dead' ELSE 'pending' END,
		 finished_at=CASE WHEN attempts>=max_attempts OR $5 THEN clock_timestamp() ELSE NULL END,
		 next_at=clock_timestamp()+$4*interval '1 microsecond',owner=NULL,lease_until=NULL,last_error=CASE WHEN $5 THEN 'invalid_payload' ELSE 'handler_failed' END
		 WHERE id=$1 AND state='running' AND owner=$2 AND epoch=$3 AND lease_until>clock_timestamp()`, j.ID, j.Owner, j.Epoch, delay.Microseconds(), permanent)
		if err == nil && tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		return err
	})
}
func (s *Store) Get(ctx context.Context, id string) (*Job, error) {
	return scan(s.db.Pool.QueryRow(ctx, `SELECT `+columns+` FROM durable_job WHERE id=$1`, id))
}

// Replay never edits a payload or accepts a function/command name. Authorization
// happens in admin on PRIMARY; validation and the immutable replay audit commit
// in the same transaction as the dead->pending transition.
func (s *Store) Replay(ctx context.Context, id, actor, reason string, validate func(*Job) error) error {
	if _, err := uuid.Parse(id); err != nil || actor == "" || len(actor) > 256 || len(reason) < 8 || len(reason) > 500 || validate == nil {
		return ErrReplay
	}
	return s.db.WithTx(ctx, func(tc context.Context) error {
		tx, _ := s.db.Tx(tc)
		j, err := scan(tx.QueryRow(tc, `SELECT `+columns+` FROM durable_job WHERE id=$1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrReplay
		}
		if err != nil {
			return err
		}
		if j.State != "dead" || !known(j.Kind) || validate(j) != nil {
			return ErrReplay
		}
		if _, err = tx.Exec(tc, `INSERT INTO durable_job_replay(id,job_id,actor,reason,previous_attempts) VALUES($1,$2,$3,$4,$5)`, uuid.NewString(), id, actor, reason, j.Attempts); err != nil {
			return err
		}
		_, err = tx.Exec(tc, `UPDATE durable_job SET state='pending',attempts=0,next_at=clock_timestamp(),finished_at=NULL,last_error='' WHERE id=$1`, id)
		return err
	})
}

// Prune deletes bounded completed payloads only. Dead letters and all billing
// receipts/replay audits are deliberately retained until operator reconciliation.
func (s *Store) Prune(ctx context.Context, keep time.Duration) (int64, error) {
	if keep < 24*time.Hour {
		return 0, errors.New("retention minimum is 24h")
	}
	tag, err := s.db.Pool.Exec(ctx, `WITH old AS(SELECT id FROM durable_job WHERE state='succeeded' AND finished_at<clock_timestamp()-$1*interval '1 microsecond' ORDER BY finished_at LIMIT 500 FOR UPDATE SKIP LOCKED) DELETE FROM durable_job j USING old WHERE j.id=old.id`, keep.Microseconds())
	return tag.RowsAffected(), err
}

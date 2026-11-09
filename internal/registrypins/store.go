package registrypins

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Store has one trusted Config host and one writable PRIMARY. There is no read
// pool, lease worker, network callback, coverage flag or delete-enabling switch.
type Store struct {
	db   *pgdoc.DB
	host string
}

func New(db *pgdoc.DB, trustedConfigHost string) (*Store, error) {
	host, err := CanonicalHost(trustedConfigHost)
	if err != nil || db == nil || db.Pool == nil {
		return nil, ErrInvalid
	}
	return &Store{db: db, host: host}, nil
}

// Operation is a stable application operation and its exact owner generation.
// Generation is the expected immutable spec/owner nonce that future writers must
// stamp on, then independently observe from, the durable binding (not a lease).
// ID must not be regenerated to retry an ambiguous external operation. Reusing an
// accepted ID is a conflict, not permission to redispatch its external effect.
type Operation struct{ ID, ProjectID, Owner, Generation string }

func (o Operation) valid() bool {
	id, err := uuid.Parse(o.ID)
	return err == nil && id != uuid.Nil && id.String() == o.ID && projectRE.MatchString(o.ProjectID) && identityRE.MatchString(o.Owner) && identityRE.MatchString(o.Generation)
}

// Ensure is explicit and dormant: API startup does not call it in this PR.
func (s *Store) Ensure(ctx context.Context) error {
	if err := pgdoc.RejectInheritedTransaction(ctx); err != nil {
		return err
	}
	return s.db.WithTx(ctx, func(tc context.Context) error {
		tx, err := s.db.Tx(tc)
		if err != nil {
			return err
		}
		// A pre-lock snapshot under REPEATABLE READ could miss a competing
		// pin committed while this transaction waited for the namespace lock.
		// Set isolation BEFORE the primary probe (the first snapshot query).
		if _, err = tx.Exec(tc, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
			return err
		}
		if err = pgdoc.RequirePrimary(tc, tx); err != nil {
			return err
		}
		if err = s.db.LockXact(tc, "registrypins-schema-v1"); err != nil {
			return err
		}
		_, err = tx.Exec(tc, `CREATE TABLE IF NOT EXISTS registry_pin (
 id uuid PRIMARY KEY, operation uuid NOT NULL, project_id text NOT NULL, owner text NOT NULL, generation text NOT NULL,
 host text NOT NULL, repo text NOT NULL, requested_ref text NOT NULL, ref text NOT NULL, digest text NOT NULL DEFAULT '',
 state text NOT NULL CHECK(state IN ('pending','bound','unknown','released')),
 epoch bigint NOT NULL CHECK(epoch>0), binding_uid text NOT NULL DEFAULT '', binding_generation text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(operation,requested_ref), CHECK(state<>'bound' OR (digest<>'' AND binding_uid<>'' AND binding_generation=generation)));
 CREATE INDEX IF NOT EXISTS registry_pin_live ON registry_pin(host,repo) WHERE state<>'released';
 CREATE TABLE IF NOT EXISTS registry_prune_intent (
 id uuid PRIMARY KEY, project_id text NOT NULL, owner text NOT NULL, generation text NOT NULL,
 host text NOT NULL, repo text NOT NULL, ref text NOT NULL,
 state text NOT NULL CHECK(state IN ('prepared','sending','unknown','settled')),
 epoch bigint NOT NULL CHECK(epoch>0), outcome text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((state='settled')=(outcome IN ('deleted','already_absent','not_deleted'))));
 CREATE UNIQUE INDEX IF NOT EXISTS registry_prune_exclusive ON registry_prune_intent(host,repo) WHERE state<>'settled';
 CREATE TABLE IF NOT EXISTS registry_build_dispatch (
 operation uuid PRIMARY KEY,project_id text NOT NULL,owner text NOT NULL,generation text NOT NULL,host text NOT NULL,
 base_ref text NOT NULL,destination_ref text NOT NULL,pins_hash text NOT NULL,
 state text NOT NULL CHECK(state IN ('authorized','observed','terminal','unknown','known_no_effect')),
 pod_uid text NOT NULL DEFAULT '',phase text NOT NULL DEFAULT '',epoch bigint NOT NULL DEFAULT 1,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),updated_at timestamptz NOT NULL DEFAULT clock_timestamp());
 CREATE TABLE IF NOT EXISTS registry_import_dispatch (
 operation uuid PRIMARY KEY,project_id text NOT NULL,owner text NOT NULL,generation text NOT NULL,host text NOT NULL,
 source_ref text NOT NULL,destination_ref text NOT NULL,kind text NOT NULL CHECK(kind IN ('verify','copy')),pins_hash text NOT NULL,
 state text NOT NULL CHECK(state IN ('authorized','observed','terminal','unknown','known_no_effect')),
 pod_uid text NOT NULL DEFAULT '',phase text NOT NULL DEFAULT '',epoch bigint NOT NULL DEFAULT 1 CHECK(epoch>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((state='terminal')=(phase IN ('Succeeded','Failed'))),
 CHECK(state NOT IN ('observed','terminal') OR pod_uid<>''), CHECK(state<>'known_no_effect' OR pod_uid=''));
 CREATE TABLE IF NOT EXISTS registry_runtime_dispatch (
 operation uuid PRIMARY KEY,project_id text NOT NULL,owner text NOT NULL,generation text NOT NULL,host text NOT NULL,
 service_id text NOT NULL,sandbox_id text NOT NULL,mode text NOT NULL CHECK(mode IN ('create','resume','fork')),
 image_ref text NOT NULL,spec_fingerprint text NOT NULL CHECK(spec_fingerprint ~ '^[0-9a-f]{64}$'),
 pin_id uuid NOT NULL,pin_epoch bigint NOT NULL CHECK(pin_epoch>0),
 state text NOT NULL CHECK(state IN ('authorized','observed','terminal','unknown','known_no_effect')),
 pod_uid text NOT NULL DEFAULT '',cr_uid text NOT NULL DEFAULT '',phase text NOT NULL DEFAULT '',epoch bigint NOT NULL DEFAULT 1 CHECK(epoch>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((state='terminal')=(phase IN ('Succeeded','Failed'))),
 CHECK(state<>'observed' OR phase='Running'), CHECK((pod_uid='')=(cr_uid='')),
 CHECK(state NOT IN ('observed','terminal') OR pod_uid<>''), CHECK(state NOT IN ('authorized','known_no_effect') OR (pod_uid='' AND phase='')));`)
		return err
	})
}

// transaction never inherits a business transaction: a returned fence is already
// committed. All lock orders are namespace first, then pin/intent rows. No fn is
// exposed outside this package, so no SQL transaction can wrap network effects.
func (s *Store) transaction(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	if err := pgdoc.RejectInheritedTransaction(ctx); err != nil {
		return err
	}
	return s.db.WithTx(ctx, func(tc context.Context) error {
		tx, err := s.db.Tx(tc)
		if err != nil {
			return err
		}
		// A pre-lock snapshot under REPEATABLE READ could miss a competing
		// pin committed while this transaction waited for the namespace lock.
		// Set isolation BEFORE the primary probe (the first snapshot query).
		if _, err = tx.Exec(tc, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
			return err
		}
		if err = pgdoc.RequirePrimary(tc, tx); err != nil {
			return err
		}
		if err = s.db.LockXact(tc, "registry-ref:"+s.host); err != nil {
			return err
		}
		return fn(tc, tx)
	})
}

// lockProject proves the repository's project is current on this PRIMARY, not a
// foreign/unknown/deleting project. Authorization of the server-supplied project
// remains the caller's job; this core exposes no user-facing API. Lock ordering is
// registry namespace -> project row -> pin/intent rows. Never lock a project first.
func (s *Store) lockProject(ctx context.Context, projectID string) error {
	var p struct {
		Status string `json:"status"`
	}
	found, err := s.db.C("project").GetForUpdate(ctx, projectID, &p)
	if err != nil {
		return err
	}
	if !found || (p.Status != "ENABLED" && p.Status != "DISABLED") {
		return ErrScope
	}
	return nil
}

func (s *Store) reference(raw string) (Ref, error) { return ParseRef(s.host, raw) }
func conflict(err error) error {
	if errors.Is(err, pgx.ErrNoRows) || pgdoc.IsDup(err) {
		return ErrConflict
	}
	return err
}

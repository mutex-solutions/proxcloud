package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
)

// HolderOperation is evidence, never a replayable command. Pending rows have NO
// expiry. Losing an HTTP answer, DB connection or worker must not admit a writer.
type HolderOperation struct {
	ID        string      `json:"id"`
	Scope     string      `json:"scope"`
	Owner     string      `json:"owner"`
	Epoch     int64       `json:"epoch"`
	State     string      `json:"state"`
	UPID      UPID        `json:"upid,omitempty"`
	Proof     holderProof `json:"proof"`
	CreatedAt time.Time   `json:"createdAt"`
}

type HolderOperationStore interface {
	Acquire(context.Context, string, time.Duration) (*lock.Lease, error)
	Renew(context.Context, lock.Lease, time.Duration) error
	Check(context.Context, lock.Lease) error
	Release(context.Context, lock.Lease) error
	Pending(context.Context, string) (*HolderOperation, error)
	Begin(context.Context, lock.Lease, holderProof) (*HolderOperation, error)
	RecordUPID(context.Context, lock.Lease, string, UPID) error
	Settle(context.Context, lock.Lease, *HolderOperation, string) error
}

type HolderOperationRepo struct {
	db     *pgdoc.DB
	leases *lock.Leases
}

func NewHolderOperationRepo(db *pgdoc.DB) *HolderOperationRepo {
	return &HolderOperationRepo{db: db, leases: lock.NewLeases(db.Pool)}
}
func (r *HolderOperationRepo) Ensure(ctx context.Context) error {
	if err := r.leases.Ensure(ctx); err != nil {
		return err
	}
	_, err := r.db.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS "pveHolderOperation" (
		id uuid PRIMARY KEY, scope text NOT NULL, owner uuid NOT NULL, epoch bigint NOT NULL,
		state text NOT NULL, upid text NOT NULL DEFAULT '', proof jsonb NOT NULL,
		created_at timestamptz NOT NULL DEFAULT clock_timestamp(), settled_at timestamptz);
		CREATE UNIQUE INDEX IF NOT EXISTS pve_holder_pending ON "pveHolderOperation" (scope)
		WHERE settled_at IS NULL`)
	return err
}
func (r *HolderOperationRepo) Acquire(ctx context.Context, n string, ttl time.Duration) (*lock.Lease, error) {
	return r.leases.Acquire(ctx, n, ttl)
}
func (r *HolderOperationRepo) Renew(ctx context.Context, x lock.Lease, ttl time.Duration) error {
	return r.leases.Renew(ctx, x, ttl)
}
func (r *HolderOperationRepo) Release(ctx context.Context, x lock.Lease) error {
	return r.leases.Release(ctx, x)
}
func scanHolder(row pgx.Row) (*HolderOperation, error) {
	x := new(HolderOperation)
	var proof []byte
	err := row.Scan(&x.ID, &x.Scope, &x.Owner, &x.Epoch, &x.State, &x.UPID, &proof, &x.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(proof, &x.Proof)
	return x, err
}

const holderColumns = `id::text,scope,owner::text,epoch,state,upid,proof,created_at`

func (r *HolderOperationRepo) Pending(ctx context.Context, scope string) (*HolderOperation, error) {
	return scanHolder(r.db.Pool.QueryRow(ctx, `SELECT `+holderColumns+` FROM "pveHolderOperation" WHERE scope=$1 AND settled_at IS NULL`, scope))
}
func holderPending(op *HolderOperation) error {
	return conflictf("Volume operation %s is unresolved (%s); an administrator must inspect holder-operation status. No retry was dispatched", op.ID, op.State)
}
func (r *HolderOperationRepo) Check(ctx context.Context, x lock.Lease) error {
	return pgx.BeginFunc(ctx, r.db.Pool, func(tx pgx.Tx) error {
		if err := r.leases.CheckTx(ctx, tx, x); err != nil {
			return err
		}
		p, err := scanHolder(tx.QueryRow(ctx, `SELECT `+holderColumns+` FROM "pveHolderOperation" WHERE scope=$1 AND settled_at IS NULL`, x.Name))
		if err != nil {
			return err
		}
		if p != nil {
			return holderPending(p)
		}
		return nil
	})
}

// Begin closes check/POST admission races: commit DISPATCHING before any I/O.
// A successor may acquire the expired lease, but cannot get past this row.
func (r *HolderOperationRepo) Begin(ctx context.Context, x lock.Lease, p holderProof) (op *HolderOperation, err error) {
	body, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	op = &HolderOperation{ID: uuid.NewString(), Scope: x.Name, Owner: x.Owner, Epoch: x.Epoch, State: "DISPATCHING", Proof: p}
	err = pgx.BeginFunc(ctx, r.db.Pool, func(tx pgx.Tx) error {
		if err := r.leases.CheckTx(ctx, tx, x); err != nil {
			return err
		}
		pending, e := scanHolder(tx.QueryRow(ctx, `SELECT `+holderColumns+` FROM "pveHolderOperation" WHERE scope=$1 AND settled_at IS NULL`, x.Name))
		if e != nil {
			return e
		}
		if pending != nil {
			return holderPending(pending)
		}
		err := tx.QueryRow(ctx, `INSERT INTO "pveHolderOperation" (id,scope,owner,epoch,state,proof)
			SELECT $1,$2,$3,$4,'DISPATCHING',$5 FROM "operationLease"
			WHERE name=$2 AND owner=$3 AND epoch=$4 AND until_at>clock_timestamp() RETURNING created_at`, op.ID, op.Scope, op.Owner, op.Epoch, body).Scan(&op.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return lock.ErrLeaseLost
		}
		return err
	})
	return op, err
}
func (r *HolderOperationRepo) RecordUPID(ctx context.Context, x lock.Lease, id string, up UPID) error {
	return pgx.BeginFunc(ctx, r.db.Pool, func(tx pgx.Tx) error {
		if err := r.leases.CheckTx(ctx, tx, x); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE "pveHolderOperation" SET upid=$5,state='OBSERVING'
			WHERE id=$1 AND scope=$2 AND owner=$3 AND epoch=$4 AND settled_at IS NULL AND state='DISPATCHING'
			AND EXISTS (SELECT 1 FROM "operationLease" WHERE name=$2 AND owner=$3 AND epoch=$4 AND until_at>clock_timestamp())`, id, x.Name, x.Owner, x.Epoch, up)
		if err == nil && tag.RowsAffected() != 1 {
			return lock.ErrLeaseLost
		}
		return err
	})
}

// Settle is CAS exactly once. Recovery may own a NEW lease, never a new POST.
// The service, not an administrator-supplied boolean, supplies readback proof.
func (r *HolderOperationRepo) Settle(ctx context.Context, x lock.Lease, op *HolderOperation, state string) error {
	if state != "SUCCEEDED" && state != "FAILED" && state != "REFUSED" {
		return fmt.Errorf("invalid holder settlement %q", state)
	}
	return pgx.BeginFunc(ctx, r.db.Pool, func(tx pgx.Tx) error {
		if err := r.leases.CheckTx(ctx, tx, x); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE "pveHolderOperation" SET state=$4,settled_at=clock_timestamp()
			WHERE id=$1 AND scope=$2 AND upid=$3 AND settled_at IS NULL
			AND EXISTS (SELECT 1 FROM "operationLease" WHERE name=$2 AND owner=$5 AND epoch=$6 AND until_at>clock_timestamp())`, op.ID, x.Name, op.UPID, state, x.Owner, x.Epoch)
		if err == nil && tag.RowsAffected() != 1 {
			return lock.ErrLeaseLost
		}
		return err
	})
}

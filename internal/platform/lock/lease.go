package lock

// Renewable leases are deliberately separate from ShedLock: callers need an
// acquisition identity, database time and fail-closed checks, not a scheduler TTL.
import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrLeaseLost = errors.New("operation lease lost")

// Lease identifies ONE acquisition, even when two workers share a host.
// Epochs are retained after release and increase on every acquisition.
type Lease struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
	Epoch int64  `json:"epoch"`
}

type Leases struct{ Pool *pgxpool.Pool }

func NewLeases(pool *pgxpool.Pool) *Leases { return &Leases{Pool: pool} }

func (l *Leases) Ensure(ctx context.Context) error {
	_, err := l.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS "operationLease" (
		name text PRIMARY KEY, owner uuid NOT NULL, epoch bigint NOT NULL,
		until_at timestamptz NOT NULL)`)
	return err
}

// Acquire is a single DB-clock CAS. A lease admits a worker, but cannot fence
// an external API: the caller MUST also persist a nonexpiring dispatch intent.
func (l *Leases) Acquire(ctx context.Context, name string, ttl time.Duration) (*Lease, error) {
	if ttl <= 0 {
		return nil, errors.New("lease TTL must be positive")
	}
	x := &Lease{Name: name, Owner: uuid.NewString()}
	err := l.Pool.QueryRow(ctx, `INSERT INTO "operationLease" (name,owner,epoch,until_at)
		VALUES ($1,$2,1,clock_timestamp()+$3*interval '1 microsecond')
		ON CONFLICT (name) DO UPDATE SET owner=EXCLUDED.owner,
		epoch="operationLease".epoch+1, until_at=clock_timestamp()+$3*interval '1 microsecond'
		WHERE "operationLease".until_at <= clock_timestamp() RETURNING epoch`, name, x.Owner, ttl.Microseconds()).Scan(&x.Epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return x, err
}

// CheckTx locks only the lease row, in the caller's SHORT database transaction.
// External calls must happen AFTER commit. Expired owners cannot resurrect leases.
func (l *Leases) CheckTx(ctx context.Context, tx pgx.Tx, x Lease) error {
	var until time.Time
	err := tx.QueryRow(ctx, `SELECT until_at FROM "operationLease"
		WHERE name=$1 AND owner=$2 AND epoch=$3 FOR UPDATE`, x.Name, x.Owner, x.Epoch).Scan(&until)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	// Evaluate time AFTER acquiring the row lock, not before a potentially long wait.
	var valid bool
	if err = tx.QueryRow(ctx, `SELECT $1::timestamptz > clock_timestamp()`, until).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrLeaseLost
	}
	return nil
}

func (l *Leases) Check(ctx context.Context, x Lease) error {
	var valid bool
	err := l.Pool.QueryRow(ctx, `SELECT until_at>clock_timestamp() FROM "operationLease"
		WHERE name=$1 AND owner=$2 AND epoch=$3`, x.Name, x.Owner, x.Epoch).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !valid {
		return ErrLeaseLost
	}
	return err
}

func (l *Leases) Renew(ctx context.Context, x Lease, ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("lease TTL must be positive")
	}
	return pgx.BeginFunc(ctx, l.Pool, func(tx pgx.Tx) error {
		if err := l.CheckTx(ctx, tx, x); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE "operationLease" SET until_at=clock_timestamp()+$4*interval '1 microsecond'
			WHERE name=$1 AND owner=$2 AND epoch=$3 AND until_at>clock_timestamp()`, x.Name, x.Owner, x.Epoch, ttl.Microseconds())
		if err == nil && tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		return err
	})
}

// Release cannot free a successor, nor revive an expired acquisition.
func (l *Leases) Release(ctx context.Context, x Lease) error {
	return pgx.BeginFunc(ctx, l.Pool, func(tx pgx.Tx) error {
		if err := l.CheckTx(ctx, tx, x); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE "operationLease" SET until_at=clock_timestamp()
			WHERE name=$1 AND owner=$2 AND epoch=$3 AND until_at>clock_timestamp()`, x.Name, x.Owner, x.Epoch)
		if err == nil && tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		return err
	})
}

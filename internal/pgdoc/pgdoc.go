// Package pgdoc is a thin document store over PostgreSQL JSONB — the ProxCloud
// primary datastore. One table per document kind: (id text primary key,
// doc jsonb not null). It exposes only the primitives the app uses (get,
// find with a small filter-operator subset, insert, replace, set/unset,
// delete, count) — deliberately not a general query translator.
package pgdoc

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB wraps the PostgreSQL connection pool.
type DB struct {
	Pool     *pgxpool.Pool
	lockMu   sync.Mutex
	lockPool *pgxpool.Pool
}

// Connect dials PostgreSQL using the configured DSN.
func Connect(ctx context.Context, dsn string) (*DB, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("pgdoc: primary database configuration invalid")
	}
	// Validate EVERY newly opened connection: a multihost DSN or failover must
	// not route later repo/auth/worker reads through a physical/read-only replica.
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error { return RequirePrimary(ctx, conn) }
	pool, err := pgxpool.NewWithConfig(cctx, cfg)
	if err != nil {
		return nil, errors.New("pgdoc: primary database connection unavailable")
	}
	if err := pool.Ping(cctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgdoc ping: %w", err)
	}
	if err := RequirePrimary(cctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	if err := ensureTimeFn(cctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{Pool: pool}, nil
}

// ensureTimeFn installs pgdoc_ts(text) → timestamptz, an IMMUTABLE cast used by
// time-field expression indexes and sorts. Marking it immutable is safe because
// stored times are RFC3339 strings carrying their own offset, so the result is
// independent of the TimeZone GUC (unlike a bare ::timestamptz cast, which
// Postgres refuses to index).
func ensureTimeFn(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}) error {
	const ddl = `CREATE OR REPLACE FUNCTION pgdoc_ts(text) RETURNS timestamptz
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS 'SELECT $1::timestamptz'`
	if _, err := q.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("pgdoc pgdoc_ts: %w", err)
	}
	return nil
}

// Ping verifies the server is reachable (used by the readiness probe).
func (d *DB) Ping(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return d.Pool.Ping(pctx)
}

func (d *DB) Close(ctx context.Context) error {
	d.lockMu.Lock()
	lp := d.lockPool
	d.lockMu.Unlock()
	if lp != nil {
		lp.Close()
	}
	d.Pool.Close()
	return nil
}

// C returns the Store for a document table (mirrors db.Collection ergonomics).
func (d *DB) C(table string) *Store {
	return &Store{db: d, table: table}
}

// querier is satisfied by both the pool and a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type txKey struct{}
type txOwnerKey struct{}

// ErrInheritedTransaction identifies operations that must commit their durable
// fence before a caller starts its business transaction or external effects.
var ErrInheritedTransaction = errors.New("pgdoc: inherited transaction is not allowed")

// RejectInheritedTransaction rejects BOTH this DB's and another DB's transaction.
// It never strips context values or silently opens an independent nested commit.
func RejectInheritedTransaction(ctx context.Context) error {
	if ctx.Value(txKey{}) != nil || ctx.Value(txOwnerKey{}) != nil {
		return ErrInheritedTransaction
	}
	return nil
}

// Tx exposes only this DB's current WithTx transaction. It never starts an
// independent transaction: transactional outbox writes must join the business
// commit. Callers must not commit or roll back the returned handle themselves.
func (d *DB) Tx(ctx context.Context) (pgx.Tx, error) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	if !ok || ctx.Value(txOwnerKey{}) != d {
		return nil, errors.New("pgdoc: transaction required on this database")
	}
	return tx, nil
}

// WithTx runs fn inside a transaction; every Store call made with the ctx fn
// receives executes on that transaction (commit on nil, rollback on error).
// This is the replacement for the old single-document atomic
// read-modify-write patterns (get-and-lock, upsert-if, OCC).
//
// Called inside another WithTx, fn joins that transaction (a nested BeginFunc on the pool would
// be a second transaction, deadlocking on rows the outer one locked).
func (d *DB) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		if _, err := d.Tx(ctx); err != nil {
			return err
		}
		return fn(ctx)
	}
	return pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		tctx := context.WithValue(ctx, txKey{}, tx)
		return fn(context.WithValue(tctx, txOwnerKey{}, d))
	})
}

// LockXact takes an advisory lock on key until the surrounding WithTx ends. Unlike a row lock it
// also works for a key with no row behind it.
func (d *DB) LockXact(ctx context.Context, key string) error {
	_, err := d.q(ctx).Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", key)
	return err
}

// WithAdvisoryLock holds a connection-level lock without wrapping fn in a SQL
// transaction. Long capture operations must commit their progress independently.
// A nonblocking loser returns acquired=false and never calls fn.
func (d *DB) WithAdvisoryLock(ctx context.Context, key string, try bool, fn func() error) (acquired bool, err error) {
	// Long exec/capture holders and blocked lock waiters must not consume the
	// primary pool: callbacks need that pool for independent progress transactions.
	d.lockMu.Lock()
	if d.lockPool == nil {
		cfg := d.Pool.Config()
		cfg.MinConns = 0
		cfg.MaxConns = 8
		lp, e := pgxpool.NewWithConfig(ctx, cfg)
		if e != nil {
			d.lockMu.Unlock()
			return false, e
		}
		d.lockPool = lp
	}
	lp := d.lockPool
	d.lockMu.Unlock()
	c, err := lp.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer c.Release()
	if try {
		if err := c.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtext($1))", key).Scan(&acquired); err != nil {
			return false, err
		}
		if !acquired {
			return false, nil
		}
	} else {
		if _, err := c.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", key); err != nil {
			return false, err
		}
		acquired = true
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, e := c.Exec(cleanup, "SELECT pg_advisory_unlock(hashtext($1))", key); e != nil {
			_ = c.Conn().Close(cleanup)
		}
	}()
	return true, fn()
}

// TryAdvisoryLock takes a session-level advisory lock on key on a dedicated connection of the lock
// pool and returns its release (idempotent). acquired=false (no error) when another session holds
// it. The lock also ends if the process dies (its connection closes). For holders that span long
// external work, where WithAdvisoryLock's callback shape does not fit.
func (d *DB) TryAdvisoryLock(ctx context.Context, key string) (release func(), acquired bool, err error) {
	d.lockMu.Lock()
	if d.lockPool == nil {
		cfg := d.Pool.Config()
		cfg.MinConns = 0
		cfg.MaxConns = 8
		lp, e := pgxpool.NewWithConfig(ctx, cfg)
		if e != nil {
			d.lockMu.Unlock()
			return nil, false, e
		}
		d.lockPool = lp
	}
	lp := d.lockPool
	d.lockMu.Unlock()
	c, err := lp.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := c.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtext($1))", key).Scan(&acquired); err != nil || !acquired {
		c.Release()
		return nil, false, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, e := c.Exec(cleanup, "SELECT pg_advisory_unlock(hashtext($1))", key); e != nil {
				_ = c.Conn().Close(cleanup)
			}
			c.Release()
		})
	}, true, nil
}

func (d *DB) q(ctx context.Context) querier {
	if _, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		tx, err := d.Tx(ctx)
		if err != nil {
			return rejectedQuerier{err: err}
		}
		return tx
	}
	return d.Pool
}

// Reject a wrong-DB context at EVERY Store SQL boundary, including reads and
// LockXact. Falling back to this pool would also break atomic business/outbox.
type rejectedQuerier struct{ err error }

func (q rejectedQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, q.err
}
func (q rejectedQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, q.err }
func (q rejectedQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	return rejectedRow{err: q.err}
}

type rejectedRow struct{ err error }

func (r rejectedRow) Scan(...any) error { return r.err }

// Per-process id state: a fixed random middle + a monotonic counter tail, so
// ids minted by one process sort in insertion order even within one second.
var (
	idProcRand = func() [5]byte {
		var r [5]byte
		if _, err := rand.Read(r[:]); err != nil {
			panic(fmt.Sprintf("pgdoc: rand: %v", err)) // crypto/rand never fails on supported platforms
		}
		return r
	}()
	idCounter = func() *atomic.Uint64 {
		var c atomic.Uint64
		var seed [8]byte
		if _, err := rand.Read(seed[:]); err != nil {
			panic(fmt.Sprintf("pgdoc: rand: %v", err))
		}
		c.Store(binary.BigEndian.Uint64(seed[:]) & 0xFFFF) // small start, room before wrap
		return &c
	}()
)

// NewID returns a 24-char hex id: 4 time bytes (unix seconds, big-endian) +
// 5 per-process random bytes + 3 counter bytes. Time-prefixed and
// counter-tailed so lexicographic order ≈ insertion order, which keyset
// (marker) paging and recency sorts rely on.
func NewID() string {
	var b [12]byte
	binary.BigEndian.PutUint32(b[:4], uint32(time.Now().Unix()))
	copy(b[4:9], idProcRand[:])
	n := idCounter.Add(1)
	b[9], b[10], b[11] = byte(n>>16), byte(n>>8), byte(n)
	return hex.EncodeToString(b[:])
}

// IsDup reports whether err is a unique-constraint violation (the duplicate-key
// signal repos branch on, e.g. get-or-create races).
func IsDup(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

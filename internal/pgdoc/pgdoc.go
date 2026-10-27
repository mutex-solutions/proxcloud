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
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB wraps the PostgreSQL connection pool.
type DB struct {
	Pool *pgxpool.Pool
}

// Connect dials PostgreSQL using the configured DSN.
func Connect(ctx context.Context, dsn string) (*DB, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(cctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgdoc connect: %w", err)
	}
	if err := pool.Ping(cctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgdoc ping: %w", err)
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

// WithTx runs fn inside a transaction; every Store call made with the ctx fn
// receives executes on that transaction (commit on nil, rollback on error).
// This is the replacement for the old single-document atomic
// read-modify-write patterns (get-and-lock, upsert-if, OCC).
func (d *DB) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		return fn(context.WithValue(ctx, txKey{}, tx))
	})
}

func (d *DB) q(ctx context.Context) querier {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return tx
	}
	return d.Pool
}

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

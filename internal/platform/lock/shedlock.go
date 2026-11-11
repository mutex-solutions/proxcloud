// Package lock is a PostgreSQL-backed distributed lock over the `shedLock` table, so the
// scheduled jobs (billing charge cron, JWK rotation, …) never double-run across pods.
// Schema per lock: {_id: name, lockUntil, lockedAt, lockedBy}.
package lock

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

const collectionName = "shedLock"

type ShedLock struct {
	db   *pgdoc.DB
	col  *pgdoc.Store
	host string
}

func New(db *pgdoc.DB) *ShedLock {
	h, _ := os.Hostname()
	if h == "" {
		h = "proxcloud"
	}
	return &ShedLock{db: db, col: db.C(collectionName), host: h}
}

// Lock tries to acquire the named lock for up to atMostFor (the safety ceiling if the holder
// dies). Returns true iff acquired, in one statement: insert the lock row, or take over an
// existing one only when its lockUntil ≤ now. A live lock hits the conflict arm's WHERE guard
// → zero rows → not acquired (not an error).
func (l *ShedLock) Lock(ctx context.Context, name string, atMostFor time.Duration, now time.Time) (bool, error) {
	body, _, err := pgdoc.Marshal(pgdoc.M{
		"lockUntil": now.Add(atMostFor),
		"lockedAt":  acquisition(now),
		"lockedBy":  l.host,
	})
	if err != nil {
		return false, err
	}
	const sql = `INSERT INTO "shedLock" (id, doc) VALUES ($1, $2)` +
		` ON CONFLICT (id) DO UPDATE SET doc = EXCLUDED.doc` +
		` WHERE ("shedLock".doc->>'lockUntil')::timestamptz <= $3`
	tag, err := l.db.Pool.Exec(ctx, sql, name, body, now)
	if err != nil {
		// Likely a missing table on first use — create it and retry once
		// (mirrors the store's implicit-collection behaviour).
		if e := l.col.Ensure(ctx); e != nil {
			return false, err
		}
		if tag, err = l.db.Pool.Exec(ctx, sql, name, body, now); err != nil {
			return false, err
		}
	}
	return tag.RowsAffected() > 0, nil
}

// Wait is Lock retried for up to wait. It returns the release, or acquired false when the lock stayed busy. The release
// still works once ctx is cancelled.
func (l *ShedLock) Wait(ctx context.Context, name string, atMostFor, wait time.Duration) (release func(), acquired bool, err error) {
	const step = 500 * time.Millisecond
	for left := wait; ; left -= step {
		now := time.Now().UTC()
		ok, err := l.Lock(ctx, name, atMostFor, now)
		if err != nil {
			return nil, false, err
		}
		if ok {
			return func() { _ = l.Unlock(context.WithoutCancel(ctx), name, 0, now, time.Now().UTC()) }, true, nil
		}
		if left <= 0 {
			return nil, false, nil
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(step):
		}
	}
}

// Unlock releases the named lock, honouring atLeastFor: lockUntil is set to the later of
// (acquiredAt + atLeastFor) and now — so a job that finished fast can't be re-run before its
// minimum interval.
//
// It releases only the acquisition it was given: a holder that outlived atMostFor no longer owns the row, and writing to it
// would free the next owner's lock. Who owns a row is who wrote lockedBy and lockedAt, the same now the caller passed to Lock.
func (l *ShedLock) Unlock(ctx context.Context, name string, atLeastFor time.Duration, acquiredAt, now time.Time) error {
	until := now
	if floor := acquiredAt.Add(atLeastFor); floor.After(until) {
		until = floor
	}
	owned, err := l.col.SetFieldsOne(ctx, pgdoc.M{"_id": name, "lockedBy": l.host, "lockedAt": acquisition(acquiredAt)}, pgdoc.M{"lockUntil": until}, nil)
	if err == nil && !owned {
		slog.Warn("lock: a holder outlived its lock and held nothing to release", "lock", name, "acquiredAt", acquiredAt)
	}
	return err
}

// acquisition is an acquisition time as stored and compared: the database keeps microseconds, and a compare rounding either
// side differently would read an owner's own lock as someone else's.
func acquisition(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

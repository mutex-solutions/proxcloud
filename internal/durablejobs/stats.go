package durablejobs

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// History is the ONLY consumer of the optional read pool. There is no Store,
// WithTx, auth, job-state decision or worker attached to this handle.
type History struct {
	pool   *pgxpool.Pool
	source string
	owned  bool
}

func NewHistory(primary *pgxpool.Pool) *History { return &History{pool: primary, source: "primary"} }
func OpenHistory(ctx context.Context, url string) (*History, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("job history database configuration invalid")
	}
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("job history database unavailable")
	}
	return &History{pool: pool, source: "configured-read", owned: true}, nil
}
func (h *History) Close() {
	if h.owned {
		h.pool.Close()
	}
}

type Stats struct {
	Source      string           `json:"source"`
	LagTolerant bool             `json:"lagTolerant"`
	ObservedAt  time.Time        `json:"observedAt"`
	ReplayAt    *time.Time       `json:"walReplayAt,omitempty"`
	LagSeconds  *float64         `json:"lagSeconds,omitempty"`
	Counts      map[string]int64 `json:"counts"`
	Recent      []*Job           `json:"recent"`
}

func (h *History) Stats(ctx context.Context) (*Stats, error) {
	out := &Stats{Source: h.source, LagTolerant: true, Counts: map[string]int64{}, Recent: []*Job{}}
	// These values describe WAL replay, not a bound on application-data staleness:
	// an idle primary can produce no new replay timestamp. Never gate a write on it.
	err := h.pool.QueryRow(ctx, `SELECT clock_timestamp(),pg_last_xact_replay_timestamp(),CASE WHEN pg_is_in_recovery() THEN extract(epoch FROM clock_timestamp()-pg_last_xact_replay_timestamp())::float8 END`).Scan(&out.ObservedAt, &out.ReplayAt, &out.LagSeconds)
	if err != nil {
		return nil, err
	}
	rows, err := h.pool.Query(ctx, `SELECT state,count(*) FROM durable_job GROUP BY state`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var state string
		var n int64
		if err = rows.Scan(&state, &n); err != nil {
			rows.Close()
			return nil, err
		}
		out.Counts[state] = n
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = h.pool.Query(ctx, `SELECT `+columns+` FROM durable_job ORDER BY created_at DESC,id DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		j, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out.Recent = append(out.Recent, j)
	}
	return out, rows.Err()
}

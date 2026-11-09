package mailoutbox

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Overview is primary metadata only, no payload/decryption or extra read pool.
// The application never uses status to make ownership or delivery decisions.
type Overview struct {
	Source          string           `json:"source"`
	DispatchEnabled bool             `json:"dispatchEnabled"`
	Counts          map[string]int64 `json:"counts"`
}

func (s *Store) Overview(ctx context.Context) (Overview, error) {
	v := Overview{Source: "primary", Counts: map[string]int64{}}
	if err := pgdoc.RequireNoTransaction(ctx); err != nil {
		return v, err
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT state,count(*) FROM mail_outbox GROUP BY state`)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var count int64
		if err = rows.Scan(&state, &count); err != nil {
			return v, err
		}
		v.Counts[state] = count
	}
	return v, rows.Err()
}

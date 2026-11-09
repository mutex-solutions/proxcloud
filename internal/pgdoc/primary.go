package pgdoc

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// PrimaryProbe is intentionally tiny so a wrong-pool startup path is testable.
type PrimaryProbe interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// RequirePrimary rejects physical standbys AND read-only sessions before any DDL,
// job claim or business write. Read-only observability pools must not use Connect.
func RequirePrimary(ctx context.Context, q PrimaryProbe) error {
	var recovery, readOnly bool
	if err := q.QueryRow(ctx, `SELECT pg_is_in_recovery(), current_setting('transaction_read_only')::boolean`).Scan(&recovery, &readOnly); err != nil {
		return err
	}
	if recovery || readOnly {
		return errors.New("pgdoc: primary writable PostgreSQL required (recovery or transaction_read_only is enabled)")
	}
	return nil
}

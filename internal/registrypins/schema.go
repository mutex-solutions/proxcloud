package registrypins

import (
	"context"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// EnsureSchema installs only shared fence infrastructure. A host is selected
// later per trusted provider Config; no registry I/O or delete authorization.
func EnsureSchema(ctx context.Context, db *pgdoc.DB) error {
	if db == nil || db.Pool == nil {
		return ErrInvalid
	}
	return (&Store{db: db}).Ensure(ctx)
}

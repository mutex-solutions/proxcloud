//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// mustInsert inserts a doc, failing the test on error.
func mustInsert(t *testing.T, db *pgdoc.DB, col string, doc any) {
	t.Helper()
	if _, err := db.C(col).InsertOne(context.Background(), doc); err != nil {
		t.Fatalf("insert %s: %v", col, err)
	}
}

// mustInsertID inserts a doc and returns its id (generated when the doc carries
// no _id) — the form the string-typed domain ID fields decode to.
func mustInsertID(t *testing.T, db *pgdoc.DB, col string, doc any) string {
	t.Helper()
	id, err := db.C(col).InsertOne(context.Background(), doc)
	if err != nil {
		t.Fatalf("insert %s: %v", col, err)
	}
	return id
}

// decimalOf parses a decimal money value for seeding (stored as
// a decimal string in jsonb by the pgdoc codec).
func decimalOf(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("decimal %s: %v", s, err)
	}
	return d
}

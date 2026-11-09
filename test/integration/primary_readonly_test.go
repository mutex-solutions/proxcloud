//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

func TestPrimaryStartupRejectsReadOnlySession(t *testing.T) {
	db := freshPG(t)
	cfg := db.Pool.Config()
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = pgdoc.RequirePrimary(context.Background(), pool); err == nil || !strings.Contains(err.Error(), "primary writable") {
		t.Fatalf("readonly claimed as primary: %v", err)
	}
	if _, err = pgdoc.Connect(context.Background(), cfg.ConnString()+"&default_transaction_read_only=on"); err == nil {
		t.Fatal("Connect accepted read-only primary URL")
	}
}

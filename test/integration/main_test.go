//go:build integration

// Package integration holds hermetic integration tests for the platform slices,
// run against a throwaway PostgreSQL started via testcontainers-go. Build-tagged
// so the default `go test ./...` (unit tests) needs no Docker; run explicitly with
//
//	go test -tags=integration ./test/integration/...
package integration

import (
	"context"
	"crypto/md5"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// testPG is the shared throwaway database for all integration tests;
// pgBase is its DSN (used to mint per-test databases).
var (
	testPG *pgdoc.DB
	pgBase string
)

func TestMain(m *testing.M) {
	ctx := context.Background()

	pgc, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("proxcloud_test"),
		tcpostgres.WithUsername("proxcloud"),
		tcpostgres.WithPassword("proxcloud"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres container: %v\n", err)
		os.Exit(1)
	}
	pgBase, err = pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pg connection string: %v\n", err)
		os.Exit(1)
	}
	testPG, err = pgdoc.Connect(ctx, pgBase)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pg connect: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	testPG.Close(context.Background())
	_ = testcontainers.TerminateContainer(pgc)
	os.Exit(code)
}

// freshPG returns a pgdoc.DB on a uniquely-named database so each test is
// isolated (no cross-test bleed-through on shared tables).
func freshPG(t *testing.T) *pgdoc.DB {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("it_%x", md5.Sum([]byte(t.Name())))
	if _, err := testPG.Pool.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatalf("create db: %v", err)
	}
	// pgBase is URL-form: …/proxcloud_test?sslmode=disable
	dsn := strings.Replace(pgBase, "/proxcloud_test?", "/"+name+"?", 1)
	db, err := pgdoc.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect fresh pg: %v", err)
	}
	t.Cleanup(func() {
		db.Close(context.Background())
		_, _ = testPG.Pool.Exec(context.Background(), `DROP DATABASE "`+name+`" WITH (FORCE)`)
	})
	return db
}

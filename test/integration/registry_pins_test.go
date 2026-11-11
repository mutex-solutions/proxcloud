//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/registrypins"
)

const pinHost = "registry.internal"
const pinRepo = pinHost + "/t/project1/0123456789abcdef0123"

var pinDigest = pinRepo + "@sha256:" + strings.Repeat("a", 64)

func pinOp() registrypins.Operation {
	return registrypins.Operation{ID: uuid.NewString(), ProjectID: "project1", Owner: "build:test", Generation: "spec-v2"}
}
func pinsFixture(t *testing.T) (*pgdoc.DB, *registrypins.Store, *registrypins.Store) {
	t.Helper()
	ctx := context.Background()
	db := freshPG(t)
	if err := db.C("project").Upsert(ctx, "project1", pgdoc.M{"status": "ENABLED"}); err != nil {
		t.Fatal(err)
	}
	s, err := registrypins.New(db, pinHost)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	// A second real pool models another API instance, not a shared in-memory lock.
	cfg := db.Pool.Config()
	// The core must override even a PRIMARY configured with stale-snapshot defaults.
	cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s2, err := registrypins.New(&pgdoc.DB{Pool: pool}, pinHost)
	if err != nil {
		t.Fatal(err)
	}
	return db, s, s2
}
func onePin(t *testing.T, s *registrypins.Store, ref string) registrypins.Pin {
	t.Helper()
	pins, err := s.BeginPins(context.Background(), pinOp(), []string{ref})
	if err != nil || len(pins) != 1 {
		t.Fatalf("pin=%+v %v", pins, err)
	}
	return pins[0]
}
func binding(p registrypins.Pin) registrypins.BindingEvidence {
	return registrypins.BindingEvidence{UID: "pod-uid", Generation: p.Generation, DigestRef: pinDigest}
}

func TestRegistryPinsAcquireVersusPruneAcrossInstances(t *testing.T) {
	ctx := context.Background()
	db, a, b := pinsFixture(t)
	for _, pinFirst := range []bool{true, false} {
		// Hold exactly the production namespace lock, queue the chosen winner,
		// and use pg_stat_activity as a deterministic lock-wait barrier.
		tx, err := db.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "registry-ref:"+pinHost); err != nil {
			t.Fatal(err)
		}
		pinCh := make(chan struct {
			pins []registrypins.Pin
			err  error
		}, 1)
		pruneCh := make(chan struct {
			intent registrypins.PruneIntent
			err    error
		}, 1)
		startPin := func() {
			go func() {
				ps, e := a.BeginPins(ctx, pinOp(), []string{pinRepo + ":unresolved-dest"})
				pinCh <- struct {
					pins []registrypins.Pin
					err  error
				}{ps, e}
			}()
		}
		startPrune := func() {
			go func() {
				p, e := b.BeginPrune(ctx, pinOp(), pinDigest)
				pruneCh <- struct {
					intent registrypins.PruneIntent
					err    error
				}{p, e}
			}()
		}
		if pinFirst {
			startPin()
		} else {
			startPrune()
		}
		waitRegistryLock(t, db, 1)
		if pinFirst {
			startPrune()
		} else {
			startPin()
		}
		// BOTH transactions reached the advisory lock before either can commit.
		// Removing the core's SET READ COMMITTED reproduces a stale RR snapshot.
		waitRegistryLock(t, db, 2)
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		pr, dr := <-pinCh, <-pruneCh
		if pinFirst {
			if pr.err != nil || !errors.Is(dr.err, registrypins.ErrOperatorHold) {
				t.Fatalf("pin-first %+v %+v", pr, dr)
			}
			if _, err = a.ReleaseKnownNoEffect(ctx, pr.pins[0]); err != nil {
				t.Fatal(err)
			}
		} else {
			if dr.err != nil || !errors.Is(pr.err, registrypins.ErrOperatorHold) {
				t.Fatalf("prune-first %+v %+v", pr, dr)
			}
			sent, e := b.MarkSending(ctx, dr.intent)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = b.SettleKnown(ctx, sent, "already_absent"); e != nil {
				t.Fatal(e)
			}
		}
	}
}
func waitRegistryLock(t *testing.T, db *pgdoc.DB, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var n int
		if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE '%pg_advisory_xact_lock%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= want {
			return
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
	}
}

func TestRegistryPinsBindingGenerationAliasesAndCleanup(t *testing.T) {
	ctx := context.Background()
	db, a, b := pinsFixture(t)
	ps, err := a.BeginPins(ctx, pinOp(), []string{pinRepo + ":a", pinRepo + ":b"})
	if err != nil {
		t.Fatal(err)
	}
	bad := binding(ps[0])
	bad.Generation = "spec-v1"
	if _, err = a.ObserveBinding(ctx, ps[0], bad); err == nil {
		t.Fatal("old live generation bound current operation")
	}
	for i := range ps {
		ps[i], err = a.ObserveBinding(ctx, ps[i], binding(ps[i]))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = b.BeginPrune(ctx, pinOp(), pinDigest); !errors.Is(err, registrypins.ErrOperatorHold) {
		t.Fatal(err)
	}
	bad = binding(ps[0])
	bad.Generation = "spec-v1"
	if _, err = a.ReleaseBinding(ctx, ps[0], bad); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatalf("same UID/stale gen released: %v", err)
	}
	forged := ps[0]
	forged.Generation = "spec-v3"
	if _, err = a.ReleaseBinding(ctx, forged, binding(forged)); err == nil {
		t.Fatal("forged generation released binding")
	}
	stale := ps[0]
	if _, err = a.ReleaseBinding(ctx, ps[0], binding(ps[0])); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ReleaseBinding(ctx, stale, binding(stale)); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("stale receipt released")
	}
	if _, err = b.BeginPrune(ctx, pinOp(), pinDigest); !errors.Is(err, registrypins.ErrOperatorHold) {
		t.Fatal("second alias lost protection", err)
	}
	// Positive exact generation cleanup remains possible after project deletion starts.
	if _, err = db.C("project").SetByID(ctx, "project1", pgdoc.M{"status": "DELETE_IN_PROGRESS"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ReleaseBinding(ctx, ps[1], binding(ps[1])); err != nil {
		t.Fatal(err)
	}
	if _, err = db.C("project").SetByID(ctx, "project1", pgdoc.M{"status": "ENABLED"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = b.BeginPrune(ctx, pinOp(), pinDigest); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryPinsUnknownAndSendingNeverReplayOrExpire(t *testing.T) {
	ctx := context.Background()
	db, a, b := pinsFixture(t)
	p := onePin(t, a, pinRepo+":dest")
	u, err := a.MarkUnknown(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.ReleaseKnownNoEffect(ctx, u); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal(err)
	}
	if _, err = a.ReleaseKnownNoEffect(ctx, p); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("stale pending receipt released UNKNOWN")
	}
	if _, err = db.Pool.Exec(ctx, `UPDATE registry_pin SET created_at='2000-01-01',updated_at='2000-01-01'`); err != nil {
		t.Fatal(err)
	}
	if _, err = b.BeginPrune(ctx, pinOp(), pinDigest); !errors.Is(err, registrypins.ErrOperatorHold) {
		t.Fatal(err)
	}
	// Separate repo: a sent operation cannot be repeated, reclaimed or cleared by age.
	other := strings.Replace(pinDigest, "0123456789abcdef0123", "0123456789abcdef0124", 1)
	d, err := b.BeginPrune(ctx, pinOp(), other)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := b.MarkSending(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.MarkSending(ctx, d); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("old epoch resent")
	}
	if _, err = a.MarkSending(ctx, sent); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("sent state resent")
	}
	if _, err = db.Pool.Exec(ctx, `UPDATE registry_prune_intent SET created_at='2000-01-01',updated_at='2000-01-01'`); err != nil {
		t.Fatal(err)
	}
	unknown, err := b.PruneUnknown(ctx, sent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.SettleKnown(ctx, unknown, "deleted"); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("UNKNOWN cleared")
	}
	if _, err = b.SettleKnown(ctx, sent, "deleted"); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("stale sent cleared UNKNOWN")
	}
	if _, err = a.BeginPins(ctx, pinOp(), []string{other}); !errors.Is(err, registrypins.ErrOperatorHold) {
		t.Fatal("UNKNOWN allowed new reference", err)
	}
}

func TestRegistryPinsScopeInheritedTransactionsAndAtomicity(t *testing.T) {
	ctx := context.Background()
	db, a, b := pinsFixture(t)
	if err := db.C("project").Upsert(ctx, "project2", pgdoc.M{"status": "ENABLED"}); err != nil {
		t.Fatal(err)
	}
	foreign := strings.Replace(pinDigest, "/project1/", "/project2/", 1)
	if _, err := a.BeginPins(ctx, pinOp(), []string{foreign}); !errors.Is(err, registrypins.ErrScope) {
		t.Fatal(err)
	}
	if _, err := a.BeginPrune(ctx, pinOp(), foreign); !errors.Is(err, registrypins.ErrScope) {
		t.Fatal(err)
	}
	missing := pinOp()
	missing.ProjectID = "missing"
	if _, err := a.BeginPins(ctx, missing, []string{strings.Replace(pinDigest, "/project1/", "/missing/", 1)}); !errors.Is(err, registrypins.ErrScope) {
		t.Fatal(err)
	}
	for _, status := range []string{"SCHEDULED_FOR_DELETION", "DELETE_IN_PROGRESS", "DELETED", "UNKNOWN", ""} {
		if _, err := db.C("project").SetByID(ctx, "project1", pgdoc.M{"status": status}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := a.BeginPins(ctx, pinOp(), []string{pinDigest}); !errors.Is(err, registrypins.ErrScope) {
			t.Fatalf("status %s: %v", status, err)
		}
		if _, err := a.BeginPrune(ctx, pinOp(), pinDigest); !errors.Is(err, registrypins.ErrScope) {
			t.Fatal(err)
		}
	}
	if _, err := db.C("project").SetByID(ctx, "project1", pgdoc.M{"status": "ENABLED"}, nil); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("business rollback")
	if err := db.WithTx(ctx, func(tc context.Context) error {
		for _, s := range []*registrypins.Store{a, b} {
			if _, err := s.BeginPins(tc, pinOp(), []string{pinDigest}); !errors.Is(err, pgdoc.ErrInheritedTransaction) {
				t.Fatalf("inherited begin accepted %v", err)
			}
			if _, err := s.BeginPrune(tc, pinOp(), pinDigest); !errors.Is(err, pgdoc.ErrInheritedTransaction) {
				t.Fatal(err)
			}
			if err := s.Ensure(tc); !errors.Is(err, pgdoc.ErrInheritedTransaction) {
				t.Fatal(err)
			}
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	// A failed multi-repo acquisition must not leave its first pin committed.
	other := strings.Replace(pinDigest, "0123456789abcdef0123", "0123456789abcdef0124", 1)
	if _, err := b.BeginPrune(ctx, pinOp(), other); err != nil {
		t.Fatal(err)
	}
	if _, err := a.BeginPins(ctx, pinOp(), []string{pinDigest, other}); !errors.Is(err, registrypins.ErrOperatorHold) {
		t.Fatal(err)
	}
	var n int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM registry_pin`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial pin commit %d %v", n, err)
	}
	p := onePin(t, a, pinDigest)
	if _, err := a.BeginPins(ctx, p.Operation, []string{pinDigest}); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("stable operation replay accepted", err)
	}
}

func TestRegistryPinsConfiguredOnPublicPruneAlwaysHolds(t *testing.T) {
	ctx := context.Background()
	f := newSbxFixture(t, gatesHold)
	f.h.SetSandboxTemplates(f.db.C("sandbox_templates"))
	var requests atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusAccepted) }))
	defer srv.Close()
	for _, cfg := range []pgdoc.M{
		{"registryPrune": "on"},
		{"registryPrune": "on", "templateRegistry": strings.TrimPrefix(srv.URL, "https://") + "/t"},
		{"registryPrune": "on", "templateRegistry": strings.TrimPrefix(srv.URL, "https://") + "/t", "templates": pgdoc.M{}},
	} {
		if _, err := f.db.C("externalService").SetByID(ctx, "sbx", pgdoc.M{"config.sandbox": cfg, "status": "DISABLED"}, nil); err != nil {
			t.Fatal(err)
		}
		err := f.h.PruneSandboxRegistryOlderThan(ctx, project.RegistryPruneGrace)
		var held *registrypins.OperatorHoldError
		if !errors.Is(err, registrypins.ErrOperatorHold) || !errors.As(err, &held) {
			t.Fatalf("configured ON hid HOLD: %v", err)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("configured ON made %d HTTP calls", requests.Load())
	}
}

func TestRegistryPinsRejectReadOnlyPrimary(t *testing.T) {
	ctx := context.Background()
	db, _, _ := pinsFixture(t)
	cfg := db.Pool.Config()
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s, err := registrypins.New(&pgdoc.DB{Pool: pool}, pinHost)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Ensure(ctx); err == nil {
		t.Fatal("read-only Ensure accepted")
	}
	if _, err = s.BeginPins(ctx, pinOp(), []string{pinDigest}); err == nil {
		t.Fatal("read-only pins accepted")
	}
	if _, err = s.BeginPrune(ctx, pinOp(), pinDigest); err == nil {
		t.Fatal("read-only prune accepted")
	}
}

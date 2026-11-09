package registrypins

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Opt-in REAL PostgreSQL tests use only an explicitly supplied disposable loopback
// instance. They never start/pull a container, contact registries or reach a lab.
func importPGFixture(t *testing.T) (*pgdoc.DB, *Store, *Store) {
	t.Helper()
	dsn := os.Getenv("REGISTRYPINS_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set REGISTRYPINS_TEST_POSTGRES to a disposable loopback PostgreSQL DSN")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() != "127.0.0.1" || u.Port() == "" {
		t.Fatal("REGISTRYPINS_TEST_POSTGRES must be an explicit loopback PostgreSQL URL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := "import_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := pgdoc.Connect(ctx, u.String())
	if err != nil {
		_, _ = admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		admin.Close()
		t.Fatal(err)
	}
	cfg := db.Pool.Config()
	cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_ = db.Close(ctx)
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	if err = db.C("project").Upsert(ctx, "project1", pgdoc.M{"status": "ENABLED"}); err != nil {
		t.Fatal(err)
	}
	a, err := New(db, importTestHost)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(&pgdoc.DB{Pool: pool}, importTestHost)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if err = b.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	return db, a, b
}

func importPGOperation() Operation {
	return Operation{ID: uuid.NewString(), ProjectID: "project1", Owner: "import:template:build1", Generation: uuid.NewString()}
}
func importPGPins(t *testing.T, s *Store, op Operation, refs ...string) []Pin {
	t.Helper()
	ps, err := s.BeginPins(context.Background(), op, refs)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}
func importPGAuthorize(t *testing.T, s *Store, op Operation, source, destination string) *ImportProof {
	t.Helper()
	p, err := s.AuthorizeImportDispatch(context.Background(), op, source, destination)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func importPGEvidence(op Operation, source, destination, phase string) ImportPodEvidence {
	return ImportPodEvidence{UID: "pod-uid", Operation: op.ID, Nonce: op.Generation, Source: source, Destination: destination, Phase: phase}
}
func importPGConflict(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestImportPostgresManagedVerifyOnePinTerminalAndOwnerBinding(t *testing.T) {
	ctx := context.Background()
	db, a, b := importPGFixture(t)
	op := importPGOperation()
	ps := importPGPins(t, a, op, importTestSource)
	if _, err := a.AuthorizeBuildDispatch(ctx, op, importTestSource, importTestRepo); !errors.Is(err, ErrScope) {
		t.Fatal("untagged verify destination weakened BuildProof", err)
	}
	proof := importPGAuthorize(t, a, op, importTestSource, importTestRepo)
	if _, err := b.AuthorizeImportDispatch(ctx, op, importTestSource, importTestRepo); !errors.Is(err, ErrConflict) {
		t.Fatal("cross-store remint", err)
	}
	var kind, hash string
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT kind,pins_hash FROM registry_import_dispatch WHERE operation=$1`, op.ID).Scan(&kind, &hash); err != nil || kind != "verify" || len(hash) != 64 {
		t.Fatal(kind, hash, err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM registry_pin WHERE operation=$1`, op.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("verify must have one source pin", count, err)
	}
	p := ps[0]
	_, err := a.ReleaseKnownNoEffect(ctx, p)
	importPGConflict(t, err)
	binding := BindingEvidence{UID: "template-build-owner", Generation: op.Generation, DigestRef: importTestSource}
	_, err = a.ObserveBinding(ctx, p, binding)
	importPGConflict(t, err)
	if operation, nonce, err := proof.Consume(op.ProjectID, importTestSource, importTestRepo); err != nil || operation != op.ID || nonce != op.Generation {
		t.Fatal(operation, nonce, err)
	}
	e := importPGEvidence(op, importTestSource, importTestRepo, "Running")
	_, err = a.ObserveImportPod(ctx, p, e)
	importPGConflict(t, err) // marker UID must first be independently recorded
	if err = a.RecordImportPod(ctx, op, e); err != nil {
		t.Fatal(err)
	}
	p, err = a.ObserveImportPod(ctx, p, e)
	if err != nil || p.State != Pending || p.Ref != importTestSource || p.BindingUID != e.UID || p.BindingGeneration != e.Nonce {
		t.Fatal(p, err)
	}
	fakeTerminal := e
	fakeTerminal.Phase = "Succeeded"
	_, err = a.ReleaseImportTerminal(ctx, p, fakeTerminal)
	importPGConflict(t, err)
	_, err = a.ObserveBinding(ctx, p, binding)
	importPGConflict(t, err)
	bad := e
	bad.UID = "wrong-pod"
	importPGConflict(t, a.RecordImportPod(ctx, op, bad))
	bad = e
	bad.Nonce = "wrong-generation"
	importPGConflict(t, a.RecordImportPod(ctx, op, bad))
	bad = e
	bad.Source = strings.Replace(importTestSource, "a", "b", 1)
	importPGConflict(t, a.RecordImportPod(ctx, op, bad))
	e.Phase = "Succeeded"
	if err = a.RecordImportPod(ctx, op, e); err != nil {
		t.Fatal(err)
	}
	var epoch int64
	if err = db.Pool.QueryRow(ctx, `SELECT epoch FROM registry_import_dispatch WHERE operation=$1`, op.ID).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	stale := e
	stale.Phase = "Running"
	if err = b.RecordImportPod(ctx, op, stale); err != nil {
		t.Fatal("same-envelope stale Running", err)
	}
	var after int64
	if err = db.Pool.QueryRow(ctx, `SELECT epoch FROM registry_import_dispatch WHERE operation=$1`, op.ID).Scan(&after); err != nil || after != epoch {
		t.Fatal("terminal changed by stale observation", epoch, after, err)
	}
	bad = e
	bad.Phase = "Failed"
	importPGConflict(t, a.RecordImportPod(ctx, op, bad))
	if known, err := b.ImportTerminalKnown(ctx, op); err != nil || !known {
		t.Fatal(known, err)
	}
	_, err = a.MarkUnknown(ctx, p)
	importPGConflict(t, err)
	_, err = a.ReleaseImportTerminal(ctx, p, e)
	importPGConflict(t, err) // verify's successful source is also its runtime destination
	wrongPin := p
	wrongPin.BindingUID = "wrong-pod"
	_, err = a.ObserveBinding(ctx, wrongPin, binding)
	importPGConflict(t, err)
	badBinding := binding
	badBinding.Generation = "wrong-generation"
	if _, err = a.ObserveBinding(ctx, p, badBinding); err == nil {
		t.Fatal("wrong generation bound")
	}
	badBinding = binding
	badBinding.DigestRef = importTestRepo + "@sha256:" + strings.Repeat("b", 64)
	if _, err = a.ObserveBinding(ctx, p, badBinding); err == nil {
		t.Fatal("managed verify accepted wrong digest")
	}
	badBinding.DigestRef = importTestOtherRepo + "@" + importTestDigest
	if _, err = a.ObserveBinding(ctx, p, badBinding); err == nil {
		t.Fatal("foreign repository bound")
	}
	bound, err := b.ObserveBinding(ctx, p, binding)
	if err != nil || bound.State != Bound || bound.BindingUID != binding.UID || bound.BindingUID == e.UID || bound.Ref != importTestSource {
		t.Fatal("exact pod->durable-owner transfer", bound, err)
	}
	if _, err = a.BeginPrune(ctx, importPGOperation(), importTestSource); !errors.Is(err, ErrOperatorHold) {
		t.Fatal("bound verify lost HOLD", err)
	}
	badBinding = binding
	badBinding.UID = e.UID
	_, err = a.ReleaseBinding(ctx, bound, badBinding)
	importPGConflict(t, err)
	if _, err = b.ReleaseBinding(ctx, bound, binding); err != nil {
		t.Fatal(err)
	}
	if err = RealDeleteHold(); !errors.Is(err, ErrOperatorHold) {
		t.Fatal("public delete HOLD changed", err)
	}
}

func TestImportPostgresCopyPinSetAndExactTerminalEvidence(t *testing.T) {
	ctx := context.Background()
	_, a, b := importPGFixture(t)
	dest := importTestOtherRepo + ":build1"
	for _, source := range []string{importTestExternal, importTestSource} {
		op := importPGOperation()
		refs := []string{dest}
		if source == importTestSource {
			refs = append(refs, source)
		}
		ps := importPGPins(t, a, op, refs...)
		proof := importPGAuthorize(t, a, op, source, dest)
		if _, _, err := proof.Consume(op.ProjectID, source, dest); err != nil {
			t.Fatal(err)
		}
		e := importPGEvidence(op, source, dest, "Running")
		if err := a.RecordImportPod(ctx, op, e); err != nil {
			t.Fatal(err)
		}
		for i, p := range ps {
			var err error
			ps[i], err = a.ObserveImportPod(ctx, p, e)
			if err != nil {
				t.Fatal(err)
			}
			_, err = a.ReleaseKnownNoEffect(ctx, ps[i])
			importPGConflict(t, err)
			_, err = a.ObserveBinding(ctx, ps[i], BindingEvidence{UID: "owner", Generation: op.Generation, DigestRef: importTestOtherRepo + "@" + importTestDigest})
			if err == nil {
				t.Fatal("running alias collapsed")
			}
		}
		e.Phase = "Succeeded"
		if err := a.RecordImportPod(ctx, op, e); err != nil {
			t.Fatal(err)
		}
		for _, p := range ps {
			if p.RequestedRef == dest {
				_, err := b.ReleaseImportTerminal(ctx, p, e)
				importPGConflict(t, err)
				be := BindingEvidence{UID: "durable-template-owner", Generation: op.Generation, DigestRef: importTestOtherRepo + "@" + importTestDigest}
				wrongDigest := be
				wrongDigest.DigestRef = importTestOtherRepo + "@sha256:" + strings.Repeat("b", 64)
				_, err = b.ObserveBinding(ctx, p, wrongDigest)
				importPGConflict(t, err)
				if _, err = b.BeginPrune(ctx, importPGOperation(), wrongDigest.DigestRef); !errors.Is(err, ErrOperatorHold) {
					t.Fatal("wrong digest bind dropped pending destination tag HOLD", err)
				}
				if _, err = b.ObserveBinding(ctx, p, be); err != nil {
					t.Fatal(err)
				}
				continue
			}
			wrong := e
			wrong.Destination = importTestRepo
			_, err := a.ReleaseImportTerminal(ctx, p, wrong)
			importPGConflict(t, err)
			wrong = e
			wrong.Phase = "Failed"
			_, err = a.ReleaseImportTerminal(ctx, p, wrong)
			importPGConflict(t, err)
			buildEvidence := BuildPodEvidence{UID: e.UID, Operation: e.Operation, Nonce: e.Nonce, BaseRef: source, DestinationRef: dest, Phase: "Succeeded"}
			_, err = a.ReleaseBuildTerminal(ctx, p, buildEvidence)
			importPGConflict(t, err) // wrong action cannot substitute weaker evidence
			_, err = b.ReleaseImportTerminal(ctx, p, e)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	// A failed import releases every exact observed pending pin, never from a
	// fabricated terminal phase or wrong UID/ref/generation envelope.
	op := importPGOperation()
	ps := importPGPins(t, a, op, importTestSource, dest)
	importPGAuthorize(t, a, op, importTestSource, dest)
	e := importPGEvidence(op, importTestSource, dest, "Failed")
	if err := a.RecordImportPod(ctx, op, e); err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		p, err := a.ObserveImportPod(ctx, p, e)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = a.ReleaseImportTerminal(ctx, p, e); err != nil {
			t.Fatal(err)
		}
	}
}

func TestImportPostgresCancelCopiesRacesAndUnknownHold(t *testing.T) {
	ctx := context.Background()
	db, a, b := importPGFixture(t)
	op := importPGOperation()
	ps := importPGPins(t, a, op, importTestSource)
	p := importPGAuthorize(t, a, op, importTestSource, importTestRepo)
	wrong := op
	wrong.Owner = "same-project-wrong-owner"
	importPGConflict(t, b.CancelUnusedImportDispatch(ctx, p, wrong))
	clone := *p
	if err := b.CancelUnusedImportDispatch(ctx, &clone, op); err != nil {
		t.Fatal(err)
	}
	_, _, err := p.Consume(op.ProjectID, importTestSource, importTestRepo)
	importPGConflict(t, err)
	if _, err = a.ReleaseKnownNoEffect(ctx, ps[0]); err != nil {
		t.Fatal(err)
	}
	_, err = b.AuthorizeImportDispatch(ctx, op, importTestSource, importTestRepo)
	importPGConflict(t, err)
	for range 8 {
		op := importPGOperation()
		importPGPins(t, a, op, importTestOtherRepo+":build1")
		proof := importPGAuthorize(t, a, op, importTestExternal, importTestOtherRepo+":build1")
		copy := *proof
		start := make(chan struct{})
		consume, cancel := make(chan error, 1), make(chan error, 1)
		go func() {
			<-start
			_, _, err := proof.Consume(op.ProjectID, importTestExternal, importTestOtherRepo+":build1")
			consume <- err
		}()
		go func() { <-start; cancel <- b.CancelUnusedImportDispatch(ctx, &copy, op) }()
		close(start)
		ce, xe := <-consume, <-cancel
		if (ce == nil) == (xe == nil) || (ce != nil && !errors.Is(ce, ErrConflict)) || (xe != nil && !errors.Is(xe, ErrConflict)) {
			t.Fatal("consume/cancel must have one winner", ce, xe)
		}
	}
	op = importPGOperation()
	ps = importPGPins(t, a, op, importTestSource, importTestOtherRepo+":build1")
	p = importPGAuthorize(t, a, op, importTestSource, importTestOtherRepo+":build1")
	if _, err = a.MarkUnknown(ctx, ps[0]); err != nil {
		t.Fatal(err)
	}
	_, err = b.ReleaseKnownNoEffect(ctx, ps[1])
	importPGConflict(t, err)
	importPGConflict(t, b.CancelUnusedImportDispatch(ctx, p, op))
	_, _, err = p.Consume(op.ProjectID, importTestSource, importTestOtherRepo+":build1")
	importPGConflict(t, err) // ambiguous cancellation invalidates opaque proof first
	_, err = a.AuthorizeImportDispatch(ctx, op, importTestSource, importTestOtherRepo+":build1")
	importPGConflict(t, err)
	importPGConflict(t, a.RecordImportPod(ctx, op, importPGEvidence(op, importTestSource, importTestOtherRepo+":build1", "Succeeded")))
	if _, err = db.Pool.Exec(ctx, `UPDATE registry_pin SET created_at=clock_timestamp()-interval '100 years',updated_at=clock_timestamp()-interval '100 years' WHERE operation=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{importTestSource, importTestOtherRepo + "@" + importTestDigest} {
		if _, err = b.BeginPrune(ctx, importPGOperation(), ref); !errors.Is(err, ErrOperatorHold) {
			t.Fatal("UNKNOWN/outstanding alias expired", err)
		}
	}
}

func TestImportPostgresCrossKindExactPinsProjectAndInheritedTransactions(t *testing.T) {
	ctx := context.Background()
	db, a, b := importPGFixture(t)
	dest := importTestOtherRepo + ":build1"
	for _, buildFirst := range []bool{true, false} {
		op := importPGOperation()
		importPGPins(t, a, op, dest)
		if buildFirst {
			if _, err := a.AuthorizeBuildDispatch(ctx, op, importTestExternal, dest); err != nil {
				t.Fatal(err)
			}
			_, err := b.AuthorizeImportDispatch(ctx, op, importTestExternal, dest)
			importPGConflict(t, err)
		} else {
			importPGAuthorize(t, a, op, importTestExternal, dest)
			_, err := b.AuthorizeBuildDispatch(ctx, op, importTestExternal, dest)
			importPGConflict(t, err)
		}
	}
	for _, refs := range [][]string{{dest, importTestSource}, {importTestSource}, {dest, importTestRepo + ":extra"}} {
		op := importPGOperation()
		importPGPins(t, a, op, refs...)
		_, err := a.AuthorizeImportDispatch(ctx, op, importTestExternal, dest)
		importPGConflict(t, err)
	}
	op := importPGOperation()
	importPGPins(t, a, op, dest)
	wrong := op
	wrong.Owner = "wrong-owner"
	_, err := a.AuthorizeImportDispatch(ctx, wrong, importTestExternal, dest)
	importPGConflict(t, err)
	wrong = op
	wrong.Generation = "wrong-generation"
	_, err = a.AuthorizeImportDispatch(ctx, wrong, importTestExternal, dest)
	importPGConflict(t, err)
	foreign := strings.Replace(importTestSource, "project1", "project2", 1)
	if _, err = a.AuthorizeImportDispatch(ctx, op, foreign, dest); !errors.Is(err, ErrScope) {
		t.Fatal(err)
	}
	for _, status := range []string{"SCHEDULED_FOR_DELETION", "DELETE_IN_PROGRESS", "DELETED", "UNKNOWN", ""} {
		if _, err = db.C("project").SetByID(ctx, op.ProjectID, pgdoc.M{"status": status}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err = a.AuthorizeImportDispatch(ctx, op, importTestExternal, dest); !errors.Is(err, ErrScope) {
			t.Fatal("noncurrent project authorized", status, err)
		}
	}
	if _, err = db.C("project").SetByID(ctx, op.ProjectID, pgdoc.M{"status": "ENABLED"}, nil); err != nil {
		t.Fatal(err)
	}
	if err = db.WithTx(ctx, func(tc context.Context) error {
		_, err := b.AuthorizeImportDispatch(tc, op, importTestExternal, dest)
		if !errors.Is(err, pgdoc.ErrInheritedTransaction) {
			t.Fatal("cross-pool inherited transaction accepted", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	proof := importPGAuthorize(t, a, op, importTestExternal, dest)
	if _, _, err = proof.Consume(op.ProjectID, importTestExternal, dest); err != nil {
		t.Fatal(err)
	}
	e := importPGEvidence(op, importTestExternal, dest, "Running")
	if err = b.RecordBuildPod(ctx, op, BuildPodEvidence{UID: e.UID, Operation: op.ID, Nonce: op.Generation, BaseRef: e.Source, DestinationRef: e.Destination, Phase: e.Phase}); !errors.Is(err, ErrConflict) {
		t.Fatal("wrong action marker accepted", err)
	}
	if err = a.RecordImportPod(ctx, op, e); err != nil {
		t.Fatal(err)
	}
	ps, err := a.PinsForOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.ObserveBuildPod(ctx, ps[0], BuildPodEvidence{UID: e.UID, Operation: op.ID, Nonce: op.Generation, BaseRef: e.Source, DestinationRef: e.Destination, Phase: e.Phase}); !errors.Is(err, ErrConflict) {
		t.Fatal("build observation accepted import pins", err)
	}
}

func TestImportPostgresCrossStoreConcurrentMintAndCancelFailure(t *testing.T) {
	ctx := context.Background()
	db, a, b := importPGFixture(t)
	op := importPGOperation()
	ps := importPGPins(t, a, op, importTestSource)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, store := range []*Store{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.AuthorizeImportDispatch(ctx, op, importTestSource, importTestRepo)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else {
			importPGConflict(t, err)
		}
	}
	if wins != 1 {
		t.Fatal("cross-store mint winners", wins)
	}
	// A crash/lost proof cannot be recovered with the same operation receipts.
	_, err := a.AuthorizeImportDispatch(ctx, op, importTestSource, importTestRepo)
	importPGConflict(t, err)
	_, err = b.ReleaseKnownNoEffect(ctx, ps[0])
	importPGConflict(t, err)
	op = importPGOperation()
	ps = importPGPins(t, a, op, importTestOtherRepo+":build1")
	proof := importPGAuthorize(t, a, op, importTestExternal, importTestOtherRepo+":build1")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err = a.CancelUnusedImportDispatch(canceled, proof, op); err == nil {
		t.Fatal("canceled DB request succeeded")
	}
	_, _, err = proof.Consume(op.ProjectID, importTestExternal, importTestOtherRepo+":build1")
	importPGConflict(t, err)
	importPGConflict(t, b.CancelUnusedImportDispatch(ctx, proof, op))
	_, err = b.ReleaseKnownNoEffect(ctx, ps[0])
	importPGConflict(t, err)
	var marker string
	if err = db.Pool.QueryRow(ctx, `SELECT state FROM registry_import_dispatch WHERE operation=$1`, op.ID).Scan(&marker); err != nil || marker != "authorized" {
		t.Fatal("ambiguous cancel cleared DB hold", marker, err)
	}
}

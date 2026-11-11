package registrypins

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// These tests share the existing opt-in fixture: a new database per test, two
// independent pools (one defaults to REPEATABLE READ), loopback DSN only. No test
// starts or pulls a container or accesses a cluster/registry/provider.
func runtimePGOperation() Operation {
	return Operation{ID: uuid.NewString(), ProjectID: "project1", Owner: "run:sbx-0123456789abcdef", Generation: uuid.NewString()}
}

func runtimePGAuthorize(t *testing.T, s *Store, op Operation, spec RuntimeSpec) *RuntimeProof {
	t.Helper()
	proof, err := s.AuthorizeRuntimeDispatch(context.Background(), op, spec)
	if err != nil {
		t.Fatal(err)
	}
	return proof
}

func runtimePGEvidence(op Operation, spec RuntimeSpec, phase string) RuntimePodEvidence {
	return RuntimePodEvidence{UID: "runtime-pod-uid", CRUID: "sandbox-cr-uid", Operation: op.ID, Nonce: op.Generation, Spec: spec, Phase: phase}
}

func runtimePGConsume(t *testing.T, proof *RuntimeProof, op Operation, spec RuntimeSpec) {
	t.Helper()
	id, nonce, err := proof.Consume(op.ProjectID, spec)
	if err != nil || id != op.ID || nonce != op.Generation {
		t.Fatal(id, nonce, err)
	}
}

func runtimePGHold(t *testing.T, s *Store, ref string) {
	t.Helper()
	if _, err := s.BeginPrune(context.Background(), runtimePGOperation(), ref); !errors.Is(err, ErrOperatorHold) {
		t.Fatal("runtime image lost hold", err)
	}
}

func TestRuntimePostgresRunningBindingTypedGuardsAndTerminal(t *testing.T) {
	ctx := context.Background()
	db, a, b := importPGFixture(t)
	spec := runtimeTestSpec()
	op := runtimePGOperation()
	pin := importPGPins(t, a, op, spec.Image)[0]
	proof := runtimePGAuthorize(t, a, op, spec)
	_, err := b.AuthorizeRuntimeDispatch(ctx, op, spec)
	importPGConflict(t, err)
	_, err = a.ReleaseKnownNoEffect(ctx, pin)
	importPGConflict(t, err)
	binding := BindingEvidence{UID: "runtime-pod-uid", Generation: op.Generation, DigestRef: spec.Image}
	_, err = a.ObserveBinding(ctx, pin, binding)
	importPGConflict(t, err) // matching copied UID/gen/image is not runtime evidence
	runtimePGConsume(t, proof, op, spec)
	e := runtimePGEvidence(op, spec, "Running")
	_, err = a.ObserveRuntimePod(ctx, pin, e)
	importPGConflict(t, err) // separate independent observation must commit first
	if err = a.RecordRuntimePod(ctx, op, e); err != nil {
		t.Fatal(err)
	}
	build := BuildPodEvidence{UID: e.UID, Operation: op.ID, Nonce: op.Generation, BaseRef: spec.Image, DestinationRef: importTestOtherRepo + ":build1", Phase: "Running"}
	imp := ImportPodEvidence{UID: e.UID, Operation: op.ID, Nonce: op.Generation, Source: spec.Image, Destination: importTestRepo, Phase: "Running"}
	importPGConflict(t, a.RecordBuildPod(ctx, op, build))
	importPGConflict(t, a.RecordImportPod(ctx, op, imp))
	_, err = a.ObserveBuildPod(ctx, pin, build)
	importPGConflict(t, err)
	_, err = a.ObserveImportPod(ctx, pin, imp)
	importPGConflict(t, err)
	for _, mutate := range []func(*RuntimePodEvidence){
		func(e *RuntimePodEvidence) { e.UID = "wrong-pod" },
		func(e *RuntimePodEvidence) { e.CRUID = "wrong-cr" },
		func(e *RuntimePodEvidence) { e.Operation = uuid.NewString() },
		func(e *RuntimePodEvidence) { e.Nonce = uuid.NewString() },
		func(e *RuntimePodEvidence) { e.Spec.ServiceID = "different-service" },
		func(e *RuntimePodEvidence) { e.Spec.SandboxID = "sbx-abcdef01" },
		func(e *RuntimePodEvidence) { e.Spec.Mode = RuntimeResume },
		func(e *RuntimePodEvidence) { e.Spec.Fingerprint = strings.Repeat("c", 64) },
		func(e *RuntimePodEvidence) { e.Spec.Image = importTestOtherRepo + "@" + importTestDigest },
		func(e *RuntimePodEvidence) { e.Phase = "ReadyCandidate" },
	} {
		wrong := e
		mutate(&wrong)
		importPGConflict(t, b.RecordRuntimePod(ctx, op, wrong))
		_, err = b.ObserveRuntimePod(ctx, pin, wrong)
		importPGConflict(t, err)
	}
	bound, err := b.ObserveRuntimePod(ctx, pin, e)
	if err != nil || bound.State != Bound || bound.Ref != spec.Image || bound.BindingUID != e.UID || bound.BindingGeneration != e.Nonce || bound.Epoch != pin.Epoch+1 || bound.BindingUID == e.CRUID {
		t.Fatal("running consumer must bind actual pod", bound, err)
	}
	var markerState string
	var markerPinEpoch int64
	if err = db.Pool.QueryRow(ctx, `SELECT state,pin_epoch FROM registry_runtime_dispatch WHERE operation=$1`, op.ID).Scan(&markerState, &markerPinEpoch); err != nil || markerState != "observed" || markerPinEpoch != bound.Epoch {
		t.Fatal("marker and pin epoch diverged", markerState, markerPinEpoch, err)
	}
	_, err = a.ReleaseBinding(ctx, bound, binding)
	importPGConflict(t, err)
	_, err = a.ReleaseRuntimeTerminal(ctx, bound, e)
	importPGConflict(t, err)
	terminal := e
	terminal.Phase = "Succeeded"
	_, err = a.ReleaseRuntimeTerminal(ctx, bound, terminal)
	importPGConflict(t, err) // caller phase string alone cannot invent terminal marker
	runtimePGHold(t, b, spec.Image)
	if err = a.RecordRuntimePod(ctx, op, terminal); err != nil {
		t.Fatal(err)
	}
	var epoch, after int64
	if err = db.Pool.QueryRow(ctx, `SELECT epoch FROM registry_runtime_dispatch WHERE operation=$1`, op.ID).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	if err = b.RecordRuntimePod(ctx, op, e); err != nil {
		t.Fatal("same-envelope stale Running must be marker no-op", err)
	}
	if err = db.Pool.QueryRow(ctx, `SELECT epoch FROM registry_runtime_dispatch WHERE operation=$1`, op.ID).Scan(&after); err != nil || after != epoch {
		t.Fatal("stale Running changed terminal marker", epoch, after, err)
	}
	if known, err := b.RuntimeTerminalKnown(ctx, op); err != nil || !known {
		t.Fatal(known, err)
	}
	_, err = b.MarkUnknown(ctx, bound)
	importPGConflict(t, err)
	_, err = a.ReleaseBinding(ctx, bound, binding)
	importPGConflict(t, err) // even terminal marker does not enable generic release
	wrong := terminal
	wrong.CRUID = "different-cr"
	_, err = a.ReleaseRuntimeTerminal(ctx, bound, wrong)
	importPGConflict(t, err)
	wrong = terminal
	wrong.Phase = "Failed"
	importPGConflict(t, a.RecordRuntimePod(ctx, op, wrong))
	_, err = a.ReleaseRuntimeTerminal(ctx, bound, wrong)
	importPGConflict(t, err)
	released, err := b.ReleaseRuntimeTerminal(ctx, bound, terminal)
	if err != nil || released.State != Released || released.Epoch != bound.Epoch+1 {
		t.Fatal(released, err)
	}
	_, err = b.ObserveRuntimePod(ctx, pin, e)
	importPGConflict(t, err) // stale epoch/Running never revives the pin
	if err = b.RecordRuntimePod(ctx, op, e); err != nil {
		t.Fatal("terminal observation recovery remains no-op after release", err)
	}
	if _, err = a.BeginPrune(ctx, runtimePGOperation(), spec.Image); err != nil {
		t.Fatal("positive terminal release did not remove cooperative hold", err)
	}
	if !errors.Is(RealDeleteHold(), ErrOperatorHold) {
		t.Fatal("physical GC gate changed")
	}
}

func TestRuntimePostgresDirectTerminalRequiresTypedBindingAndFreshNonce(t *testing.T) {
	ctx := context.Background()
	_, a, b := importPGFixture(t)
	for _, phase := range []string{"Succeeded", "Failed"} {
		spec := runtimeTestSpec()
		op := runtimePGOperation()
		pin := importPGPins(t, a, op, spec.Image)[0]
		runtimePGConsume(t, runtimePGAuthorize(t, a, op, spec), op, spec)
		e := runtimePGEvidence(op, spec, phase)
		if err := a.RecordRuntimePod(ctx, op, e); err != nil {
			t.Fatal(err)
		}
		_, err := a.ReleaseRuntimeTerminal(ctx, pin, e)
		importPGConflict(t, err) // terminal marker cannot release unobserved receipt
		bound, err := a.ObserveRuntimePod(ctx, pin, e)
		if err != nil || bound.BindingUID != e.UID {
			t.Fatal(bound, err)
		}
		if _, err = b.ReleaseRuntimeTerminal(ctx, bound, e); err != nil {
			t.Fatal(err)
		}
		// Same sandbox/CR AND pod UID, but a new runtime spec nonce/incarnation.
		next := runtimePGOperation()
		spec.Mode = RuntimeResume
		newPin := importPGPins(t, a, next, spec.Image)[0]
		proof := runtimePGAuthorize(t, a, next, spec)
		runtimePGConsume(t, proof, next, spec)
		importPGConflict(t, b.RecordRuntimePod(ctx, next, e))
		_, err = b.ObserveRuntimePod(ctx, newPin, e)
		importPGConflict(t, err)
		fresh := runtimePGEvidence(next, spec, "Running")
		if err = a.RecordRuntimePod(ctx, next, fresh); err != nil {
			t.Fatal(err)
		}
		newBound, err := a.ObserveRuntimePod(ctx, newPin, fresh)
		if err != nil || newBound.Generation == bound.Generation {
			t.Fatal(newBound, err)
		}
		_, err = b.ReleaseRuntimeTerminal(ctx, newBound, e)
		importPGConflict(t, err)
		fresh.Phase = "Failed"
		if err = a.RecordRuntimePod(ctx, next, fresh); err != nil {
			t.Fatal(err)
		}
		if _, err = a.ReleaseRuntimeTerminal(ctx, newBound, fresh); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRuntimePostgresUnusedCancelCopiesRaceAndAmbiguity(t *testing.T) {
	ctx := context.Background()
	db, a, b := importPGFixture(t)
	spec := runtimeTestSpec()
	op := runtimePGOperation()
	pin := importPGPins(t, a, op, spec.Image)[0]
	proof := runtimePGAuthorize(t, a, op, spec)
	clone := *proof
	if err := b.CancelUnusedRuntimeDispatch(ctx, &clone); err != nil {
		t.Fatal(err)
	}
	_, _, err := proof.Consume(op.ProjectID, spec)
	importPGConflict(t, err)
	importPGConflict(t, a.CancelUnusedRuntimeDispatch(ctx, proof))
	if _, err = a.ReleaseKnownNoEffect(ctx, pin); err != nil {
		t.Fatal(err)
	}
	_, err = b.AuthorizeRuntimeDispatch(ctx, op, spec)
	importPGConflict(t, err)
	if known, err := a.RuntimeTerminalKnown(ctx, op); err != nil || !known {
		t.Fatal(known, err)
	}
	for range 8 {
		op := runtimePGOperation()
		pin := importPGPins(t, a, op, spec.Image)[0]
		proof := runtimePGAuthorize(t, a, op, spec)
		copy := *proof
		start := make(chan struct{})
		consume, cancel := make(chan error, 1), make(chan error, 1)
		go func() { <-start; _, _, err := proof.Consume(op.ProjectID, spec); consume <- err }()
		go func() { <-start; cancel <- b.CancelUnusedRuntimeDispatch(ctx, &copy) }()
		close(start)
		ce, xe := <-consume, <-cancel
		if (ce == nil) == (xe == nil) || (ce != nil && !errors.Is(ce, ErrConflict)) || (xe != nil && !errors.Is(xe, ErrConflict)) {
			t.Fatal("consume/cancel need one shared winner", ce, xe)
		}
		if ce == nil {
			_, err = b.ReleaseKnownNoEffect(ctx, pin)
			importPGConflict(t, err)
		} else if _, err = b.ReleaseKnownNoEffect(ctx, pin); err != nil {
			t.Fatal(err)
		}
	}
	op = runtimePGOperation()
	pin = importPGPins(t, a, op, spec.Image)[0]
	proof = runtimePGAuthorize(t, a, op, spec)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err = a.CancelUnusedRuntimeDispatch(canceled, proof); err == nil {
		t.Fatal("canceled DB operation unexpectedly succeeded")
	}
	_, _, err = proof.Consume(op.ProjectID, spec)
	importPGConflict(t, err)
	importPGConflict(t, b.CancelUnusedRuntimeDispatch(ctx, proof))
	_, err = b.ReleaseKnownNoEffect(ctx, pin)
	importPGConflict(t, err)
	var state string
	if err = db.Pool.QueryRow(ctx, `SELECT state FROM registry_runtime_dispatch WHERE operation=$1`, op.ID).Scan(&state); err != nil || state != "authorized" {
		t.Fatal("DB ambiguity cleared hold", state, err)
	}
}

func TestRuntimePostgresUnknownNonexpiryRestartAndEpochCAS(t *testing.T) {
	ctx := context.Background()
	db, a, b := importPGFixture(t)
	spec := runtimeTestSpec()
	for _, live := range []bool{false, true} {
		op := runtimePGOperation()
		pin := importPGPins(t, a, op, spec.Image)[0]
		proof := runtimePGAuthorize(t, a, op, spec)
		e := runtimePGEvidence(op, spec, "Running")
		if live {
			runtimePGConsume(t, proof, op, spec)
			if err := a.RecordRuntimePod(ctx, op, e); err != nil {
				t.Fatal(err)
			}
			var err error
			pin, err = a.ObserveRuntimePod(ctx, pin, e)
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, mutate := range []func(*Pin){
			func(p *Pin) { p.Epoch++ },
			func(p *Pin) { p.Owner = "different-owner" },
			func(p *Pin) { p.Generation = uuid.NewString() },
			func(p *Pin) { p.BindingUID = "copied-wrong-pod" },
		} {
			wrong := pin
			mutate(&wrong)
			_, err := b.MarkUnknown(ctx, wrong)
			importPGConflict(t, err)
		}
		unknown, err := b.MarkUnknown(ctx, pin)
		if err != nil || unknown.State != Unknown || unknown.Epoch != pin.Epoch+1 {
			t.Fatal(unknown, err)
		}
		var marker string
		var markerEpoch int64
		if err = db.Pool.QueryRow(ctx, `SELECT state,pin_epoch FROM registry_runtime_dispatch WHERE operation=$1`, op.ID).Scan(&marker, &markerEpoch); err != nil || marker != "unknown" || markerEpoch != unknown.Epoch {
			t.Fatal(marker, markerEpoch, err)
		}
		// No TTL or createdAt authority: ancient marker and pin are still live.
		if _, err = db.Pool.Exec(ctx, `UPDATE registry_pin SET created_at='1900-01-01',updated_at='1900-01-01' WHERE operation=$1;`, op.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Pool.Exec(ctx, `UPDATE registry_runtime_dispatch SET created_at='1900-01-01',updated_at='1900-01-01' WHERE operation=$1`, op.ID); err != nil {
			t.Fatal(err)
		}
		restarted, err := New(b.db, importTestHost)
		if err != nil {
			t.Fatal(err)
		}
		_, err = restarted.AuthorizeRuntimeDispatch(ctx, op, spec)
		importPGConflict(t, err)
		importPGConflict(t, restarted.CancelUnusedRuntimeDispatch(ctx, proof))
		e.Phase = "Succeeded"
		importPGConflict(t, restarted.RecordRuntimePod(ctx, op, e))
		_, err = restarted.ReleaseRuntimeTerminal(ctx, unknown, e)
		importPGConflict(t, err)
		_, err = restarted.ReleaseKnownNoEffect(ctx, unknown)
		importPGConflict(t, err)
		if known, err := restarted.RuntimeTerminalKnown(ctx, op); err != nil || known {
			t.Fatal("UNKNOWN became terminal authority", known, err)
		}
		runtimePGHold(t, restarted, spec.Image)
		for _, phase := range []string{"NotFound", "Unknown", "Deleted", "204", "DeadlineExceeded"} {
			e.Phase = phase
			importPGConflict(t, restarted.RecordRuntimePod(ctx, op, e))
		}
	}
}

func TestRuntimePostgresFactoryFullSetCurrentProjectAndTransactions(t *testing.T) {
	ctx := context.Background()
	db, a, b := importPGFixture(t)
	spec := runtimeTestSpec()
	for _, refs := range [][]string{{spec.Image, importTestOtherRepo + ":extra"}, {importTestRepo + ":ready"}, {importTestOtherRepo + "@" + importTestDigest}} {
		op := runtimePGOperation()
		importPGPins(t, a, op, refs...)
		_, err := a.AuthorizeRuntimeDispatch(ctx, op, spec)
		importPGConflict(t, err)
	}
	op := runtimePGOperation()
	_, err := a.AuthorizeRuntimeDispatch(ctx, op, spec)
	importPGConflict(t, err)
	pin := importPGPins(t, a, op, spec.Image)[0]
	for _, mutate := range []func(*Operation){
		func(o *Operation) { o.Owner = "wrong-owner" },
		func(o *Operation) { o.Generation = uuid.NewString() },
	} {
		wrong := op
		mutate(&wrong)
		_, err = a.AuthorizeRuntimeDispatch(ctx, wrong, spec)
		importPGConflict(t, err)
	}
	// Alias-resolved receipt is not an originally requested immutable image.
	aliasOp := runtimePGOperation()
	alias := importPGPins(t, a, aliasOp, importTestRepo+":ready")[0]
	alias, err = a.ObserveBinding(ctx, alias, BindingEvidence{UID: "template-owner", Generation: aliasOp.Generation, DigestRef: spec.Image})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.AuthorizeRuntimeDispatch(ctx, aliasOp, spec)
	importPGConflict(t, err)
	for _, status := range []string{"DELETE_IN_PROGRESS", "SCHEDULED_FOR_DELETION", "DELETED", "UNKNOWN", ""} {
		if _, err = db.C("project").SetByID(ctx, op.ProjectID, pgdoc.M{"status": status}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err = b.AuthorizeRuntimeDispatch(ctx, op, spec); !errors.Is(err, ErrScope) {
			t.Fatal("noncurrent project authorized", status, err)
		}
	}
	if err = db.C("project").Upsert(ctx, op.ProjectID, pgdoc.M{"status": "DISABLED"}); err != nil {
		t.Fatal(err)
	}
	// An operation's all-row envelope may not be filtered down by owner/project.
	if _, err = db.Pool.Exec(ctx, `INSERT INTO registry_pin(id,operation,project_id,owner,generation,host,repo,requested_ref,ref,state,epoch) VALUES($1,$2,'project2','other-owner','other-generation','registry.other','t/project2/0123456789abcdef0123','registry.other/t/project2/0123456789abcdef0123:other','registry.other/t/project2/0123456789abcdef0123:other','pending',1)`, uuid.NewString(), op.ID); err != nil {
		t.Fatal(err)
	}
	_, err = b.AuthorizeRuntimeDispatch(ctx, op, spec)
	importPGConflict(t, err)
	// Use a separate exact operation for inherited owned/cross-DB transaction cases.
	op = runtimePGOperation()
	pin = importPGPins(t, a, op, spec.Image)[0]
	otherDB, otherStore, _ := importPGFixture(t)
	for _, ownerDB := range []*pgdoc.DB{db, otherDB} {
		if err = ownerDB.WithTx(ctx, func(tc context.Context) error {
			_, err := a.AuthorizeRuntimeDispatch(tc, op, spec)
			if !errors.Is(err, pgdoc.ErrInheritedTransaction) {
				t.Fatal("inherited transaction accepted", err)
			}
			if err = a.Ensure(tc); !errors.Is(err, pgdoc.ErrInheritedTransaction) {
				t.Fatal("inherited schema transaction accepted", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Missing project with syntactically valid exact own image never authorizes.
	missing := runtimePGOperation()
	missing.ProjectID = "missingproject"
	missingSpec := spec
	missingSpec.Image = strings.Replace(spec.Image, "project1", missing.ProjectID, 1)
	if _, err = otherStore.AuthorizeRuntimeDispatch(ctx, missing, missingSpec); !errors.Is(err, ErrScope) {
		t.Fatal("missing current project accepted", err)
	}
	proof := runtimePGAuthorize(t, a, op, spec)
	if err = db.WithTx(ctx, func(tc context.Context) error {
		if err := a.RecordRuntimePod(tc, op, runtimePGEvidence(op, spec, "Running")); !errors.Is(err, pgdoc.ErrInheritedTransaction) {
			t.Fatal(err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = b.CancelUnusedRuntimeDispatch(ctx, proof); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ReleaseKnownNoEffect(ctx, pin); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimePostgresReadOnlyPrimaryRejected(t *testing.T) {
	ctx := context.Background()
	db, a, _ := importPGFixture(t)
	spec := runtimeTestSpec()
	op := runtimePGOperation()
	importPGPins(t, a, op, spec.Image)
	cfg := db.Pool.Config()
	cfg.AfterConnect = nil // exercise Store's own probe, not pgdoc.Connect's probe
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ro, err := New(&pgdoc.DB{Pool: pool}, importTestHost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ro.AuthorizeRuntimeDispatch(ctx, op, spec); err == nil || !strings.Contains(err.Error(), "primary writable PostgreSQL required") {
		t.Fatal("wrong PRIMARY admission", err)
	}
	if err = ro.Ensure(ctx); err == nil || !strings.Contains(err.Error(), "primary writable PostgreSQL required") {
		t.Fatal("wrong PRIMARY DDL", err)
	}
}

func TestRuntimePostgresOtherKindsCannotUseTypedRuntimeEvidence(t *testing.T) {
	ctx := context.Background()
	_, a, b := importPGFixture(t)
	spec := runtimeTestSpec()
	for _, kind := range []string{"plain", "import", "build"} {
		op := runtimePGOperation()
		refs := []string{spec.Image}
		if kind == "build" {
			refs = append(refs, importTestOtherRepo+":build1")
		}
		pins := importPGPins(t, a, op, refs...)
		switch kind {
		case "import":
			importPGAuthorize(t, a, op, spec.Image, importTestRepo)
		case "build":
			if _, err := a.AuthorizeBuildDispatch(ctx, op, spec.Image, importTestOtherRepo+":build1"); err != nil {
				t.Fatal(err)
			}
		}
		e := runtimePGEvidence(op, spec, "Running")
		importPGConflict(t, b.RecordRuntimePod(ctx, op, e))
		for _, pin := range pins {
			_, err := b.ObserveRuntimePod(ctx, pin, e)
			importPGConflict(t, err)
		}
	}
}

func TestRuntimePostgresAcquireVersusPruneBothNamespaceOrders(t *testing.T) {
	ctx := context.Background()
	db, a, b := importPGFixture(t)
	spec := runtimeTestSpec()
	for _, pinFirst := range []bool{true, false} {
		// Use a different repository each round; active intents never get cleared.
		if !pinFirst {
			spec.Image = importTestOtherRepo + "@" + importTestDigest
		}
		barrier, err := db.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = barrier.Rollback(ctx) })
		if _, err = barrier.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "registry-ref:"+importTestHost); err != nil {
			t.Fatal(err)
		}
		pinResult, pruneResult := make(chan error, 1), make(chan error, 1)
		startPin := func() {
			go func() { _, err := b.BeginPins(ctx, runtimePGOperation(), []string{spec.Image}); pinResult <- err }()
		}
		startPrune := func() {
			go func() { _, err := a.BeginPrune(ctx, runtimePGOperation(), spec.Image); pruneResult <- err }()
		}
		if pinFirst {
			startPin()
		} else {
			startPrune()
		}
		waitImportAdvisory(t, db, 1)
		if pinFirst {
			startPrune()
		} else {
			startPin()
		}
		waitImportAdvisory(t, db, 2)
		if err = barrier.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		pe, de := <-pinResult, <-pruneResult
		if pinFirst && (pe != nil || !errors.Is(de, ErrOperatorHold)) || !pinFirst && (de != nil || !errors.Is(pe, ErrOperatorHold)) {
			t.Fatal("namespace ordering lost exclusion/RR snapshot", pinFirst, pe, de)
		}
	}
}

func TestRuntimePostgresGlobalOperationLockAcrossActionsHostsAndProjects(t *testing.T) {
	ctx := context.Background()
	db, a, _ := importPGFixture(t)
	if err := db.C("project").Upsert(ctx, "project2", pgdoc.M{"status": "ENABLED"}); err != nil {
		t.Fatal(err)
	}
	b, err := New(db, "registry.other")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"build", "import", "runtime"} {
		for _, runtimeFirst := range []bool{true, false} {
			op := runtimePGOperation()
			spec := runtimeTestSpec()
			pin := importPGPins(t, a, op, spec.Image)[0]
			other := op
			other.ProjectID, other.Owner, other.Generation = "project2", "other:owner", uuid.NewString()
			otherSpec := spec
			otherSpec.Image = strings.Replace(strings.Replace(spec.Image, importTestHost, "registry.other", 1), "project1", "project2", 1)
			otherDest := strings.Split(otherSpec.Image, "@")[0] + ":build1"
			barrier, err := db.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = barrier.Rollback(ctx) })
			if _, err = barrier.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "registry-dispatch-op:"+op.ID); err != nil {
				t.Fatal(err)
			}
			runtimeResult, otherResult := make(chan error, 1), make(chan error, 1)
			startRuntime := func() {
				go func() { _, err := a.AuthorizeRuntimeDispatch(ctx, op, spec); runtimeResult <- err }()
			}
			startOther := func() {
				go func() {
					var err error
					switch kind {
					case "build":
						_, err = b.AuthorizeBuildDispatch(ctx, other, importTestExternal, otherDest)
					case "import":
						_, err = b.AuthorizeImportDispatch(ctx, other, otherSpec.Image, strings.Split(otherSpec.Image, "@")[0])
					case "runtime":
						_, err = b.AuthorizeRuntimeDispatch(ctx, other, otherSpec)
					}
					otherResult <- err
				}()
			}
			if runtimeFirst {
				startRuntime()
			} else {
				startOther()
			}
			waitImportAdvisory(t, db, 1)
			if runtimeFirst {
				startOther()
			} else {
				startRuntime()
			}
			waitImportAdvisory(t, db, 2)
			if err = barrier.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			re, oe := <-runtimeResult, <-otherResult
			if re != nil || !errors.Is(oe, ErrConflict) {
				t.Fatal("wrong-host/project operation bypassed exact global fence", kind, runtimeFirst, re, oe)
			}
			var count int
			if err = db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM registry_build_dispatch WHERE operation=$1)+(SELECT count(*) FROM registry_import_dispatch WHERE operation=$1)+(SELECT count(*) FROM registry_runtime_dispatch WHERE operation=$1)`, op.ID).Scan(&count); err != nil || count != 1 {
				t.Fatal("multiple action markers", count, err)
			}
			_, err = a.ReleaseKnownNoEffect(ctx, pin)
			importPGConflict(t, err)
		}
	}
}

// Producer tagged destinations and consumer immutable one-ref sets are naturally
// disjoint. Test-only receipt replacement below deliberately removes that first
// line of defense so the cross-kind marker checks themselves must reject reuse.
func TestRuntimePostgresBuildRuntimeMarkerChecksBothDirections(t *testing.T) {
	ctx := context.Background()
	db, a, b := importPGFixture(t)
	spec := runtimeTestSpec()
	dest := importTestRepo + ":build1"
	for _, runtimeFirst := range []bool{true, false} {
		op := runtimePGOperation()
		if runtimeFirst {
			importPGPins(t, a, op, spec.Image)
			runtimePGAuthorize(t, a, op, spec)
			if _, err := db.Pool.Exec(ctx, `UPDATE registry_pin SET requested_ref=$1,ref=$1,digest='' WHERE operation=$2`, dest, op.ID); err != nil {
				t.Fatal(err)
			}
			_, err := b.AuthorizeBuildDispatch(ctx, op, importTestExternal, dest)
			importPGConflict(t, err)
		} else {
			importPGPins(t, a, op, dest)
			if _, err := a.AuthorizeBuildDispatch(ctx, op, importTestExternal, dest); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Pool.Exec(ctx, `UPDATE registry_pin SET requested_ref=$1,ref=$1,digest=$2 WHERE operation=$3`, spec.Image, importTestDigest, op.ID); err != nil {
				t.Fatal(err)
			}
			_, err := b.AuthorizeRuntimeDispatch(ctx, op, spec)
			importPGConflict(t, err)
		}
	}
}

func TestRuntimePostgresImportRuntimeBothDirectionsAndConcurrentMint(t *testing.T) {
	ctx := context.Background()
	_, a, b := importPGFixture(t)
	spec := runtimeTestSpec()
	for _, runtimeFirst := range []bool{true, false} {
		op := runtimePGOperation()
		importPGPins(t, a, op, spec.Image)
		if runtimeFirst {
			runtimePGAuthorize(t, a, op, spec)
			_, err := b.AuthorizeImportDispatch(ctx, op, spec.Image, importTestRepo)
			importPGConflict(t, err)
			_, err = b.AuthorizeBuildDispatch(ctx, op, spec.Image, importTestOtherRepo+":build1")
			importPGConflict(t, err)
		} else {
			importPGAuthorize(t, a, op, spec.Image, importTestRepo)
			_, err := b.AuthorizeRuntimeDispatch(ctx, op, spec)
			importPGConflict(t, err)
		}
	}
	op := runtimePGOperation()
	importPGPins(t, a, op, spec.Image)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, s := range []*Store{a, b} {
		go func() { <-start; _, err := s.AuthorizeRuntimeDispatch(ctx, op, spec); results <- err }()
	}
	close(start)
	first, second := <-results, <-results
	if (first == nil) == (second == nil) || first != nil && !errors.Is(first, ErrConflict) || second != nil && !errors.Is(second, ErrConflict) {
		t.Fatal("two stores minted more than one proof", first, second)
	}
	_, err := b.AuthorizeRuntimeDispatch(ctx, op, spec)
	importPGConflict(t, err) // lost winner proof cannot be recovered after a restart
}

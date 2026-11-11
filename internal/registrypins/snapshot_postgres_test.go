package registrypins

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

func snapshotPGOperation() Operation {
	return Operation{ID: uuid.NewString(), ProjectID: "project1", Owner: "snapshot:template:build1", Generation: uuid.NewString()}
}
func snapshotPGEvidence(op Operation, spec SnapshotSpec, phase string) SnapshotPodEvidence {
	return SnapshotPodEvidence{UID: "assembly-pod-uid", Operation: op.ID, Nonce: op.Generation, Spec: spec, Phase: phase}
}
func snapshotPGSetup(t *testing.T) (*pgdoc.DB, *Store, *Store, Operation, SnapshotSpec, []Pin, *SnapshotProof) {
	t.Helper()
	db, a, b := importPGFixture(t)
	op := snapshotPGOperation()
	spec := snapshotTestSpec()
	ps := importPGPins(t, a, op, spec.Base, spec.Destination)
	proof, e := a.AuthorizeSnapshotDispatch(t.Context(), op, spec)
	if e != nil {
		t.Fatal(e)
	}
	return db, a, b, op, spec, ps, proof
}
func snapshotPGObserve(t *testing.T, s *Store, op Operation, spec SnapshotSpec, ps []Pin, phase string) []Pin {
	t.Helper()
	e := snapshotPGEvidence(op, spec, phase)
	if err := s.RecordSnapshotPod(t.Context(), op, e); err != nil {
		t.Fatal(err)
	}
	out := make([]Pin, len(ps))
	for i, p := range ps {
		var err error
		out[i], err = s.ObserveSnapshotPod(t.Context(), p, e)
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}
func snapshotPGMarker(t *testing.T, db *pgdoc.DB, op Operation) string {
	t.Helper()
	var s string
	if e := db.Pool.QueryRow(t.Context(), `SELECT to_jsonb(d)::text FROM registry_snapshot_dispatch d WHERE operation=$1`, op.ID).Scan(&s); e != nil {
		t.Fatal(e)
	}
	return s
}
func TestSnapshotPostgresTypedPublicationBaseReleaseAndNoGenericBypass(t *testing.T) {
	db, a, b, op, spec, ps, proof := snapshotPGSetup(t)
	ctx := t.Context()
	_, _, e := proof.Consume(op.ProjectID, spec)
	if e != nil {
		t.Fatal(e)
	}
	for _, p := range ps {
		_, e = a.ReleaseKnownNoEffect(ctx, p)
		importPGConflict(t, e)
		r, _ := a.reference(p.RequestedRef)
		_, e = a.ObserveBinding(ctx, p, BindingEvidence{UID: "assembly-pod-uid", Generation: op.Generation, DigestRef: r.Host + "/" + r.Repo + "@" + importTestDigest})
		importPGConflict(t, e)
	}
	wrongSpec := spec
	wrongSpec.ProviderRevisionHash = strings.Repeat("d", 64)
	importPGConflict(t, a.RecordSnapshotPod(ctx, op, snapshotPGEvidence(op, wrongSpec, "Running")))
	ePod := snapshotPGEvidence(op, spec, "Running")
	_, e = b.ObserveSnapshotPod(ctx, ps[0], ePod)
	importPGConflict(t, e)
	ps = snapshotPGObserve(t, a, op, spec, ps, "Running")
	outDigest := importTestOtherRepo + "@sha256:" + strings.Repeat("d", 64)
	publication := BindingEvidence{UID: "durable-template-owner", Generation: op.Generation, DigestRef: outDigest}
	var base, dest Pin
	for _, p := range ps {
		if p.RequestedRef == spec.Base {
			base = p
		} else {
			dest = p
		}
	}
	for _, p := range ps {
		r, _ := b.reference(p.RequestedRef)
		generic := publication
		generic.DigestRef = r.Host + "/" + r.Repo + "@" + importTestDigest
		_, e = b.ObserveBinding(ctx, p, generic)
		importPGConflict(t, e)
		_, e = b.ReleaseBinding(ctx, p, BindingEvidence{UID: p.BindingUID, Generation: p.BindingGeneration, DigestRef: p.Ref})
		importPGConflict(t, e)
		_, e = b.ReleaseKnownNoEffect(ctx, p)
		importPGConflict(t, e)
		build := BuildPodEvidence{UID: ePod.UID, Operation: op.ID, Nonce: op.Generation, BaseRef: spec.Base, DestinationRef: spec.Destination, Phase: "Running"}
		imp := ImportPodEvidence{UID: ePod.UID, Operation: op.ID, Nonce: op.Generation, Source: importTestExternal, Destination: spec.Destination, Phase: "Running"}
		if p.RequestedRef == spec.Base {
			imp.Source, imp.Destination = spec.Base, importTestRepo
		}
		run := RuntimePodEvidence{UID: ePod.UID, CRUID: spec.SourceCRUID, Operation: op.ID, Nonce: op.Generation, Spec: RuntimeSpec{ServiceID: spec.ServiceID, SandboxID: spec.SandboxID, Mode: RuntimeCreate, Image: spec.Base, Fingerprint: spec.ActionSpecFingerprint}, Phase: "Running"}
		importPGConflict(t, b.RecordBuildPod(ctx, op, build))
		importPGConflict(t, b.RecordImportPod(ctx, op, imp))
		importPGConflict(t, b.RecordRuntimePod(ctx, op, run))
		_, e = b.ObserveBuildPod(ctx, p, build)
		importPGConflict(t, e)
		_, e = b.ObserveImportPod(ctx, p, imp)
		importPGConflict(t, e)
		_, e = b.ObserveRuntimePod(ctx, p, run)
		importPGConflict(t, e)
		_, e = b.ReleaseBuildTerminal(ctx, p, build)
		importPGConflict(t, e)
		_, e = b.ReleaseImportTerminal(ctx, p, imp)
		importPGConflict(t, e)
		_, e = b.ReleaseRuntimeTerminal(ctx, p, run)
		importPGConflict(t, e)
	}
	_, e = b.BindSnapshotDestination(ctx, dest, ePod, publication)
	importPGConflict(t, e)
	terminal := ePod
	terminal.Phase = "Succeeded"
	_, e = b.BindSnapshotDestination(ctx, dest, terminal, publication)
	importPGConflict(t, e)
	_, e = b.ReleaseSnapshotTerminal(ctx, base, terminal)
	importPGConflict(t, e)
	if e = a.RecordSnapshotPod(ctx, op, terminal); e != nil {
		t.Fatal(e)
	}
	before := snapshotPGMarker(t, db, op)
	if e = b.RecordSnapshotPod(ctx, op, ePod); e != nil {
		t.Fatal("stale running", e)
	}
	if snapshotPGMarker(t, db, op) != before {
		t.Fatal("terminal changed by stale Running")
	}
	known, e := b.SnapshotTerminalKnown(ctx, op)
	if e != nil || !known {
		t.Fatal(known, e)
	}
	_, e = b.MarkUnknown(ctx, base)
	importPGConflict(t, e)
	conflictTerm := terminal
	conflictTerm.Phase = "Failed"
	importPGConflict(t, b.RecordSnapshotPod(ctx, op, conflictTerm))
	for _, change := range []func(*SnapshotPodEvidence){func(v *SnapshotPodEvidence) { v.UID = "replacement" }, func(v *SnapshotPodEvidence) { v.Spec.SourceCRUID = "replacement-source" }, func(v *SnapshotPodEvidence) { v.Spec.SourcePodUID = "replacement-source" }, func(v *SnapshotPodEvidence) { v.Spec.SourceFingerprint = strings.Repeat("d", 64) }, func(v *SnapshotPodEvidence) { v.Spec.ActionSpecFingerprint = strings.Repeat("d", 64) }, func(v *SnapshotPodEvidence) { v.Nonce = uuid.NewString() }} {
		v := terminal
		change(&v)
		importPGConflict(t, b.RecordSnapshotPod(ctx, op, v))
		_, e = b.BindSnapshotDestination(ctx, dest, v, publication)
		importPGConflict(t, e)
	}
	for _, bad := range []BindingEvidence{{UID: publication.UID, Generation: op.Generation, DigestRef: spec.Base}, {UID: "", Generation: op.Generation, DigestRef: outDigest}, {UID: publication.UID, Generation: "other", DigestRef: outDigest}, {UID: publication.UID, Generation: op.Generation, DigestRef: importTestOtherRepo + ":tag"}} {
		_, e = b.BindSnapshotDestination(ctx, dest, terminal, bad)
		importPGConflict(t, e)
	}
	bound, e := b.BindSnapshotDestination(ctx, dest, terminal, publication)
	if e != nil || bound.State != Bound || bound.Ref != outDigest || bound.BindingUID != publication.UID {
		t.Fatal(bound, e)
	}
	released, e := b.ReleaseSnapshotTerminal(ctx, base, terminal)
	if e != nil || released.State != Released {
		t.Fatal(released, e)
	}
	_, e = b.ReleaseSnapshotTerminal(ctx, dest, terminal)
	importPGConflict(t, e)
	_, e = b.ReleaseBinding(ctx, bound, publication)
	importPGConflict(t, e)
	_, e = b.ReleaseKnownNoEffect(ctx, bound)
	importPGConflict(t, e)
	_, e = b.BindSnapshotDestination(ctx, bound, terminal, publication)
	importPGConflict(t, e)
	_, e = b.BeginPrune(ctx, snapshotPGOperation(), outDigest)
	if !errors.Is(e, ErrOperatorHold) {
		t.Fatal("DEST lost hold", e)
	}
}
func TestSnapshotPostgresFailedDestinationAndUnknownArePermanent(t *testing.T) {
	for _, phase := range []string{"Failed", "unknown-before-observation", "unknown-observed"} {
		t.Run(phase, func(t *testing.T) {
			_, a, b, op, spec, ps, _ := snapshotPGSetup(t)
			ctx := t.Context()
			ePod := snapshotPGEvidence(op, spec, "Running")
			if phase != "unknown-before-observation" {
				ps = snapshotPGObserve(t, a, op, spec, ps, "Running")
			}
			if phase == "Failed" {
				ePod.Phase = "Failed"
				if e := a.RecordSnapshotPod(ctx, op, ePod); e != nil {
					t.Fatal(e)
				}
				for _, p := range ps {
					_, e := b.ReleaseSnapshotTerminal(ctx, p, ePod)
					if p.RequestedRef == spec.Base {
						if e != nil {
							t.Fatal(e)
						}
					} else {
						importPGConflict(t, e)
						_, e = b.BindSnapshotDestination(ctx, p, ePod, BindingEvidence{UID: "owner", Generation: op.Generation, DigestRef: importTestOtherRepo + "@" + importTestDigest})
						importPGConflict(t, e)
					}
				}
			} else {
				_, e := a.MarkUnknown(ctx, ps[0])
				if e != nil {
					t.Fatal(e)
				}
				importPGConflict(t, b.RecordSnapshotPod(ctx, op, ePod))
				ePod.Phase = "Succeeded"
				importPGConflict(t, b.RecordSnapshotPod(ctx, op, ePod))
				_, e = b.ReleaseKnownNoEffect(ctx, ps[1])
				importPGConflict(t, e)
				_, e = b.ObserveSnapshotPod(ctx, ps[1], ePod)
				importPGConflict(t, e)
				known, e := b.SnapshotTerminalKnown(ctx, op)
				if e != nil || known {
					t.Fatal(known, e)
				}
			}
			_, e := b.AuthorizeSnapshotDispatch(ctx, op, spec)
			importPGConflict(t, e)
			for _, phase := range []string{"", "Unknown", "NotFound", "timeout", "Ready"} {
				v := ePod
				v.Phase = phase
				importPGConflict(t, a.RecordSnapshotPod(ctx, op, v))
			}
			_, e = b.BeginPrune(ctx, snapshotPGOperation(), importTestOtherRepo+"@"+importTestDigest)
			if !errors.Is(e, ErrOperatorHold) {
				t.Fatal("failed/unknown DEST lost hold", e)
			}
		})
	}
}
func TestSnapshotPostgresUnusedCancelBurnsCopiesAndFullCASRollsBackMarker(t *testing.T) {
	db, a, b, op, spec, ps, proof := snapshotPGSetup(t)
	ctx := t.Context()
	clone := *proof
	if e := b.CancelUnusedSnapshotDispatch(ctx, proof); e != nil {
		t.Fatal(e)
	}
	_, _, e := clone.Consume(op.ProjectID, spec)
	importPGConflict(t, e)
	for _, p := range ps {
		if _, e = a.ReleaseKnownNoEffect(ctx, p); e != nil {
			t.Fatal(e)
		}
	}
	_, e = b.AuthorizeSnapshotDispatch(ctx, op, spec)
	importPGConflict(t, e)
	// A failed cancel must still burn all local copies and retain the authorized hold.
	op2 := snapshotPGOperation()
	ps = importPGPins(t, a, op2, spec.Base, spec.Destination)
	proof, e = a.AuthorizeSnapshotDispatch(ctx, op2, spec)
	if e != nil {
		t.Fatal(e)
	}
	clone = *proof
	if _, e = db.Pool.Exec(ctx, `CREATE FUNCTION fixture_cancel_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.state='known_no_effect' THEN RAISE EXCEPTION 'fixture'; END IF; RETURN NEW; END $$; CREATE TRIGGER fixture_cancel_fail BEFORE UPDATE ON registry_snapshot_dispatch FOR EACH ROW EXECUTE FUNCTION fixture_cancel_fail()`); e != nil {
		t.Fatal(e)
	}
	if e = a.CancelUnusedSnapshotDispatch(ctx, proof); e == nil {
		t.Fatal("cancel should fail")
	}
	_, _, e = clone.Consume(op2.ProjectID, spec)
	importPGConflict(t, e)
	if _, e = db.Pool.Exec(ctx, `DROP TRIGGER fixture_cancel_fail ON registry_snapshot_dispatch`); e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*Pin){func(p *Pin) { p.BindingGeneration = "forged" }, func(p *Pin) { p.BindingUID = "forged" }, func(p *Pin) { p.State = Bound }, func(p *Pin) { p.Epoch++ }, func(p *Pin) { p.RequestedRef = importTestRepo + ":wrong" }} {
		forged := ps[0]
		mutate(&forged)
		before := snapshotPGMarker(t, db, op2)
		_, e = a.MarkUnknown(ctx, forged)
		if e == nil {
			t.Fatal("forged receipt admitted")
		}
		if snapshotPGMarker(t, db, op2) != before {
			t.Fatal("forged CAS changed marker")
		}
	}
	_, e = b.ReleaseKnownNoEffect(ctx, ps[0])
	importPGConflict(t, e)
	observed := snapshotPGObserve(t, a, op2, spec, ps, "Running")
	before := snapshotPGMarker(t, db, op2)
	_, e = a.MarkUnknown(ctx, ps[0])
	importPGConflict(t, e)
	if snapshotPGMarker(t, db, op2) != before {
		t.Fatal("stale marker epoch escaped")
	}
	// Private multi-action evidence cannot be assembled into a generic grant.
	ev := snapshotPGEvidence(op2, spec, "Running")
	runtime := runtimePGEvidence(op2, runtimeTestSpec(), "Running")
	_, e = a.advancePin(ctx, observed[0], Pending, observed[0].Ref, importTestDigest, ev.UID, ev.Nonce, pinEvidence{snapshotPod: &ev, runtimePod: &runtime})
	importPGConflict(t, e)
}
func TestSnapshotPostgresWholeSetScopeAndPruneExclusion(t *testing.T) {
	for _, mode := range []string{"extra", "missing-base", "owner", "generation", "host", "bound", "binding-generation"} {
		t.Run(mode, func(t *testing.T) {
			db, a, b := importPGFixture(t)
			op := snapshotPGOperation()
			spec := snapshotTestSpec()
			refs := []string{spec.Base, spec.Destination}
			if mode == "extra" {
				refs = append(refs, importTestRepo+":extra")
			}
			if mode == "missing-base" {
				refs = refs[1:]
			}
			ps := importPGPins(t, a, op, refs...)
			field, value := "", ""
			switch mode {
			case "owner":
				field, value = "owner", "other"
			case "generation":
				field, value = "generation", "other"
			case "host":
				field, value = "host", "other.internal"
			case "bound":
				field, value = "state", Released
			case "binding-generation":
				field, value = "binding_generation", "other"
			}
			if field != "" {
				if _, e := db.Pool.Exec(t.Context(), `UPDATE registry_pin SET `+field+`=$1 WHERE id=$2`, value, ps[0].ID); e != nil {
					t.Fatal(e)
				}
			}
			_, e := b.AuthorizeSnapshotDispatch(t.Context(), op, spec)
			importPGConflict(t, e)
			var count int
			if e = db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM registry_snapshot_dispatch`).Scan(&count); e != nil || count != 0 {
				t.Fatal("partial marker", e)
			}
		})
	}
	for _, base := range []string{importTestExternal, importTestHost + "/library/base@" + importTestDigest} {
		_, a, b := importPGFixture(t)
		op := snapshotPGOperation()
		spec := snapshotTestSpec()
		spec.Base = base
		ps := importPGPins(t, a, op, spec.Destination)
		proof, e := b.AuthorizeSnapshotDispatch(t.Context(), op, spec)
		if e != nil {
			t.Fatal(e)
		}
		if _, _, e = proof.Consume(op.ProjectID, spec); e != nil {
			t.Fatal(e)
		}
		ps = snapshotPGObserve(t, b, op, spec, ps, "Succeeded")
		_, e = b.ReleaseSnapshotTerminal(t.Context(), ps[0], snapshotPGEvidence(op, spec, "Succeeded"))
		importPGConflict(t, e)
	}
	_, a, _ := importPGFixture(t)
	op := snapshotPGOperation()
	spec := snapshotTestSpec()
	if _, e := a.BeginPrune(t.Context(), op, importTestOtherRepo+"@"+importTestDigest); e != nil {
		t.Fatal(e)
	}
	if _, e := a.BeginPins(t.Context(), snapshotPGOperation(), []string{spec.Base, spec.Destination}); !errors.Is(e, ErrOperatorHold) {
		t.Fatal("prune exclusion", e)
	}
}

func TestSnapshotPostgresProofConsumeCancelRaceAndNoRemint(t *testing.T) {
	_, a, b, op, spec, _, proof := snapshotPGSetup(t)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 32 {
		copy := *proof
		wg.Go(func() {
			var e error
			if i%2 == 0 {
				_, _, e = copy.Consume(op.ProjectID, spec)
			} else {
				e = a.CancelUnusedSnapshotDispatch(context.Background(), &copy)
			}
			if e == nil {
				wins.Add(1)
			} else if !errors.Is(e, ErrConflict) {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("one-use race", wins.Load())
	}
	_, e := b.AuthorizeSnapshotDispatch(t.Context(), op, spec)
	importPGConflict(t, e)
}

func TestSnapshotPostgresCrossActionUUIDBothDirectionsAndHosts(t *testing.T) {
	for _, legacy := range []string{"build", "import", "runtime"} {
		for _, snapshotFirst := range []bool{false, true} {
			for _, crossHost := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/snapshotFirst%t/crossHost%t", legacy, snapshotFirst, crossHost), func(t *testing.T) {
					db, a, b := importPGFixture(t)
					op := snapshotPGOperation()
					spec := snapshotTestSpec()
					importPGPins(t, a, op, spec.Base, spec.Destination)
					if crossHost {
						var e error
						b, e = New(db, "other.internal")
						if e != nil {
							t.Fatal(e)
						}
					}
					// Marker history is the authority: missing/mismatching pins must not allow
					// another action or host to reuse even terminal/unused operation identities.
					insertLegacy := func(state string) {
						var e error
						switch legacy {
						case "build":
							_, e = db.Pool.Exec(t.Context(), `INSERT INTO registry_build_dispatch(operation,project_id,owner,generation,host,base_ref,destination_ref,pins_hash,state) VALUES($1,$2,$3,$4,$5,$6,$7,'hash',$8)`, op.ID, op.ProjectID, op.Owner, op.Generation, a.host, spec.Base, spec.Destination, state)
						case "import":
							_, e = db.Pool.Exec(t.Context(), `INSERT INTO registry_import_dispatch(operation,project_id,owner,generation,host,source_ref,destination_ref,kind,pins_hash,state) VALUES($1,$2,$3,$4,$5,$6,$7,'copy','hash',$8)`, op.ID, op.ProjectID, op.Owner, op.Generation, a.host, importTestExternal, spec.Destination, state)
						case "runtime":
							_, e = db.Pool.Exec(t.Context(), `INSERT INTO registry_runtime_dispatch(operation,project_id,owner,generation,host,service_id,sandbox_id,mode,image_ref,spec_fingerprint,pin_id,pin_epoch,state) VALUES($1,$2,$3,$4,$5,$6,$7,'create',$8,$9,$10,1,$11)`, op.ID, op.ProjectID, op.Owner, op.Generation, a.host, spec.ServiceID, spec.SandboxID, spec.Base, spec.ActionSpecFingerprint, uuid.NewString(), state)
						}
						if e != nil {
							t.Fatal(e)
						}
					}
					if !snapshotFirst {
						insertLegacy("known_no_effect")
						candidate := spec
						if crossHost {
							candidate.Destination = strings.Replace(spec.Destination, a.host, b.host, 1)
						}
						_, e := b.AuthorizeSnapshotDispatch(t.Context(), op, candidate)
						importPGConflict(t, e)
						return
					}
					if _, e := a.AuthorizeSnapshotDispatch(t.Context(), op, spec); e != nil {
						t.Fatal(e)
					}
					var e error
					switch legacy {
					case "build":
						_, e = b.AuthorizeBuildDispatch(t.Context(), op, spec.Base, spec.Destination)
					case "import":
						_, e = b.AuthorizeImportDispatch(t.Context(), op, importTestExternal, spec.Destination)
					case "runtime":
						_, e = b.AuthorizeRuntimeDispatch(t.Context(), op, RuntimeSpec{ServiceID: spec.ServiceID, SandboxID: spec.SandboxID, Mode: RuntimeCreate, Image: strings.Replace(spec.Base, a.host, b.host, 1), Fingerprint: spec.ActionSpecFingerprint})
					}
					// Destination must be valid for the tested host to reach UUID guards.
					if crossHost && legacy != "runtime" {
						dest := strings.Replace(spec.Destination, a.host, b.host, 1)
						if legacy == "build" {
							_, e = b.AuthorizeBuildDispatch(t.Context(), op, importTestExternal, dest)
						} else {
							_, e = b.AuthorizeImportDispatch(t.Context(), op, importTestExternal, dest)
						}
					}
					importPGConflict(t, e)
				})
			}
		}
	}
	// BUILD/IMPORT-copy/SNAPSHOT have compatible exact role sets. Prove each
	// contender succeeds alone before racing. RUNTIME's immutable consumer set is
	// incompatible with producer DEST-tag roles; its exclusion is sequential above,
	// and its GLOBAL UUID lock participation is tested separately below.
	for _, otherAction := range []string{"build", "import", "snapshot"} {
		t.Run("race-"+otherAction, func(t *testing.T) {
			db, a, b := importPGFixture(t)
			op := snapshotPGOperation()
			spec := snapshotTestSpec()
			if otherAction == "import" {
				spec.Base = importTestExternal
			}
			refs := []string{spec.Destination}
			if otherAction != "import" {
				refs = append(refs, spec.Base)
			}
			authorizeOther := func(s *Store, operation Operation) error {
				switch otherAction {
				case "build":
					_, e := s.AuthorizeBuildDispatch(t.Context(), operation, spec.Base, spec.Destination)
					return e
				case "import":
					_, e := s.AuthorizeImportDispatch(t.Context(), operation, spec.Base, spec.Destination)
					return e
				default:
					_, e := s.AuthorizeSnapshotDispatch(t.Context(), operation, spec)
					return e
				}
			}
			for _, snapshotOnly := range []bool{true, false} {
				control := snapshotPGOperation()
				importPGPins(t, a, control, refs...)
				var e error
				if snapshotOnly {
					_, e = a.AuthorizeSnapshotDispatch(t.Context(), control, spec)
				} else {
					e = authorizeOther(b, control)
				}
				if e != nil {
					t.Fatal("uncontended positive control", e)
				}
			}
			importPGPins(t, a, op, refs...)
			var wins atomic.Int32
			var wg sync.WaitGroup
			start := make(chan struct{})
			wg.Go(func() {
				<-start
				if _, e := a.AuthorizeSnapshotDispatch(context.Background(), op, spec); e == nil {
					wins.Add(1)
				} else if !errors.Is(e, ErrConflict) {
					t.Error(e)
				}
			})
			wg.Go(func() {
				<-start
				e := authorizeOther(b, op)
				if e == nil {
					wins.Add(1)
				} else if !errors.Is(e, ErrConflict) {
					t.Error(e)
				}
			})
			close(start)
			wg.Wait()
			if wins.Load() != 1 {
				t.Fatal("global action winners", wins.Load())
			}
			var n int
			if e := db.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM registry_snapshot_dispatch WHERE operation=$1)+(SELECT count(*) FROM registry_build_dispatch WHERE operation=$1)+(SELECT count(*) FROM registry_import_dispatch WHERE operation=$1)+(SELECT count(*) FROM registry_runtime_dispatch WHERE operation=$1)`, op.ID).Scan(&n); e != nil || n != 1 {
				t.Fatal("cross-action duplicate", n, e)
			}
		})
	}
}

func TestSnapshotPostgresInheritedAndQueuedDefaultRR(t *testing.T) {
	db, a, b := importPGFixture(t)
	other, _, _ := importPGFixture(t)
	op := snapshotPGOperation()
	spec := snapshotTestSpec()
	ps := importPGPins(t, a, op, spec.Base, spec.Destination)
	for _, outer := range []*pgdoc.DB{db, other} {
		if e := outer.WithTx(t.Context(), func(tc context.Context) error {
			if _, e := a.AuthorizeSnapshotDispatch(tc, op, spec); !errors.Is(e, pgdoc.ErrInheritedTransaction) {
				return fmt.Errorf("authorize inherited: %v", e)
			}
			if e := a.RecordSnapshotPod(tc, op, snapshotPGEvidence(op, spec, "Running")); !errors.Is(e, pgdoc.ErrInheritedTransaction) {
				return fmt.Errorf("observe inherited: %v", e)
			}
			if _, e := a.SnapshotTerminalKnown(tc, op); !errors.Is(e, pgdoc.ErrInheritedTransaction) {
				return fmt.Errorf("known inherited: %v", e)
			}
			return nil
		}); e != nil {
			t.Fatal(e)
		}
	}
	// Hold the namespace, queue authorization on the RR-default pool, then mutate a
	// current receipt. RC FIRST must see the committed invalidation after the wait.
	conn, e := db.Pool.Acquire(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Release()
	tx, e := conn.Begin(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	if _, e = tx.Exec(t.Context(), `SELECT pg_advisory_xact_lock(hashtext($1))`, "registry-ref:"+a.host); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, e := b.AuthorizeSnapshotDispatch(context.Background(), op, spec); done <- e }()
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	waiting := false
	for !waiting {
		select {
		case <-deadline:
			t.Fatal("authorizer never queued")
		case <-tick.C:
			var n int
			if e = db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted`).Scan(&n); e != nil {
				t.Fatal(e)
			}
			waiting = n > 0
		}
	}
	if _, e = tx.Exec(t.Context(), `UPDATE registry_pin SET state='released',epoch=epoch+1 WHERE id=$1`, ps[0].ID); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(t.Context()); e != nil {
		t.Fatal(e)
	}
	importPGConflict(t, <-done)
}

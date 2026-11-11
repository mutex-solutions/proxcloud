//go:build integration

package integration

import (
	"context"
	"errors"
	"github.com/mutex-solutions/proxcloud/internal/registrypins"
	"testing"
)

func TestRegistryBuildProofDurableOnceCancelAndAliasTerminal(t *testing.T) {
	ctx := context.Background()
	_, a, b := pinsFixture(t)
	op := pinOp()
	base := pinRepo + ":base"
	dest := pinRepo + ":dest"
	ps, err := a.BeginPins(ctx, op, []string{base, dest})
	if err != nil {
		t.Fatal(err)
	}
	proof, err := a.AuthorizeBuildDispatch(ctx, op, base, dest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.AuthorizeBuildDispatch(ctx, op, base, dest); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("second replica reminted", err)
	}
	if _, err = a.ReleaseKnownNoEffect(ctx, ps[0]); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("authorized pin released", err)
	}
	if _, err = a.ObserveBinding(ctx, ps[0], registrypins.BindingEvidence{UID: "template1", Generation: op.Generation, DigestRef: pinDigest}); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("BASE alias collapsed while active", err)
	}
	clone := *proof
	if _, _, err = proof.Consume(op.ProjectID, base, dest); err != nil {
		t.Fatal(err)
	}
	if _, _, err = clone.Consume(op.ProjectID, base, dest); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("shallow-copy replay", err)
	}
	if err = a.CancelUnusedDispatch(ctx, &clone, op); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("used proof canceled", err)
	}
	ev := registrypins.BuildPodEvidence{UID: "pod1", Operation: op.ID, Nonce: op.Generation, BaseRef: base, DestinationRef: dest, Phase: "Running"}
	if err = a.RecordBuildPod(ctx, op, ev); err != nil {
		t.Fatal(err)
	}
	for i, p := range ps {
		ps[i], err = a.ObserveBuildPod(ctx, p, ev)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = a.ReleaseBuildTerminal(ctx, ps[0], ev); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("running alias released", err)
	}
	ev.Phase = "Succeeded"
	wrong := ev
	wrong.UID = "replacement"
	if err = a.RecordBuildPod(ctx, op, wrong); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("wrong pod terminal accepted", err)
	}
	if err = a.RecordBuildPod(ctx, op, ev); err != nil {
		t.Fatal(err)
	}
	staleRunning := ev
	staleRunning.Phase = "Running"
	if err = b.RecordBuildPod(ctx, op, staleRunning); err != nil {
		t.Fatal("same pod stale Running poisoned terminal", err)
	}
	if _, err = b.MarkUnknown(ctx, ps[0]); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("terminal marker poisoned UNKNOWN", err)
	}
	for _, p := range ps {
		if p.RequestedRef == base {
			if _, err = a.ReleaseBuildTerminal(ctx, p, ev); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err = a.ObserveBinding(ctx, p, registrypins.BindingEvidence{UID: "template1", Generation: op.Generation, DigestRef: pinDigest}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = b.BeginPrune(ctx, pinOp(), pinDigest); !errors.Is(err, registrypins.ErrOperatorHold) {
		t.Fatal("DEST owner lost", err)
	}
}

func TestRegistryBuildProofUnusedCancelAndUnknownNeverReissue(t *testing.T) {
	ctx := context.Background()
	_, a, b := pinsFixture(t)
	op := pinOp()
	dest := pinRepo + ":dest"
	base := "registry.internal/library/base:1"
	ps, err := a.BeginPins(ctx, op, []string{dest})
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.AuthorizeBuildDispatch(ctx, op, base, dest)
	if err != nil {
		t.Fatal(err)
	}
	clone := *p
	if err = b.CancelUnusedDispatch(ctx, &clone, op); err != nil {
		t.Fatal(err)
	}
	if _, _, err = p.Consume(op.ProjectID, base, dest); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("canceled proof consumed", err)
	}
	if err = a.CancelUnusedDispatch(ctx, p, op); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("second cancel", err)
	}
	if _, err = a.ReleaseKnownNoEffect(ctx, ps[0]); err != nil {
		t.Fatal(err)
	}
	if _, err = b.AuthorizeBuildDispatch(ctx, op, base, dest); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("canceled auth reminted", err)
	}
	op2 := pinOp()
	ps, err = a.BeginPins(ctx, op2, []string{dest})
	if err != nil {
		t.Fatal(err)
	}
	p, err = a.AuthorizeBuildDispatch(ctx, op2, base, dest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.MarkUnknown(ctx, ps[0]); err != nil {
		t.Fatal(err)
	}
	if _, err = b.BeginPrune(ctx, pinOp(), pinDigest); !errors.Is(err, registrypins.ErrOperatorHold) {
		t.Fatal(err)
	}
	if _, err = b.AuthorizeBuildDispatch(ctx, op2, base, dest); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("UNKNOWN reminted", err)
	}
	// An already-issued external attempt might still run, but its UNKNOWN pin can
	// neither expire nor become prunable. This is not an HTTP cancellation claim.
	if _, _, err = p.Consume(op2.ProjectID, base, dest); err != nil {
		t.Fatal(err)
	}
}

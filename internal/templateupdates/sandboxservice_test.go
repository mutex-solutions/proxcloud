package templateupdates

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// N2: an image is free for a new enqueue or a pin delete only when every earlier receipt is settled: its step is
// succeeded or its cleanup ran (cleaned), or its durable job is dead or succeeded. A failed step alone is not enough:
// terminal() writes it before running the job's Cleanup, with the job row still running.
func TestSandboxImageBusyNeedsASettledJob(t *testing.T) {
	ctx := context.Background()
	rec := func(step Step, cleaned bool) SandboxReceipt {
		return SandboxReceipt{ID: sbxJob, Kind: "sandbox", Payload: sbxPayload(), Step: step, Cleaned: cleaned}
	}
	for _, c := range []struct {
		name  string
		rec   SandboxReceipt
		state string
		busy  bool
	}{
		{"failed, its cleanup still running", rec(SbxFailed, false), "running", true},
		{"failed, a retry pending", rec(SbxFailed, false), "pending", true},
		// Dead but not cleaned: the dead-job pass's Cleanup is still to come and could undo a newer job's switch.
		{"failed, job dead, its cleanup still to come", rec(SbxFailed, false), "dead", true},
		{"failed and cleaned, job dead", rec(SbxFailed, true), "dead", false},
		{"failed and cleaned, job row not dead yet", rec(SbxFailed, true), "running", false},
		{"succeeded, the dead-job pass finishing", rec(SbxSucceeded, true), "running", false},
		{"pinned, job dead, the dead-job pass not done", rec(SbxPinned, false), "dead", true},
		{"pinned and cleaned, job dead", rec(SbxPinned, true), "dead", false},
		{"prepull, job running", rec(SbxPrepull, false), "running", true},
		{"queued, job pending", rec(SbxQueued, false), "pending", true},
		{"queued, job succeeded", rec(SbxQueued, false), "succeeded", false},
		{"queued, job row unreadable", rec(SbxQueued, false), "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			asked := 0
			err := sandboxImageBusy(ctx, []SandboxReceipt{c.rec}, func(_ context.Context, id string) (string, error) {
				asked++
				if id != sbxJob {
					t.Fatalf("asked the state of %s", id)
				}
				return c.state, nil
			})
			if c.busy && !errors.Is(err, ErrSandboxBusy) || !c.busy && err != nil {
				t.Fatalf("want busy=%v, got %v", c.busy, err)
			}
			if (c.rec.Step == SbxSucceeded || c.rec.Cleaned) && asked != 0 {
				t.Fatal("a settled receipt's job state was read")
			}
		})
	}
	// One unsettled receipt among settled ones keeps the image busy; a lookup that fails fails closed.
	states := map[string]string{"a": "dead", "b": "running", "c": "succeeded"}
	lookup := func(_ context.Context, id string) (string, error) { return states[id], nil }
	prior := []SandboxReceipt{{ID: "a", Step: SbxFailed, Cleaned: true}, {ID: "c", Step: SbxQueued}, {ID: "b", Step: SbxFailed}, {ID: "d", Step: SbxSucceeded}}
	if err := sandboxImageBusy(ctx, prior, lookup); !errors.Is(err, ErrSandboxBusy) {
		t.Fatalf("mixed: %v", err)
	}
	states["b"] = "dead"
	if err := sandboxImageBusy(ctx, prior, lookup); !errors.Is(err, ErrSandboxBusy) || !strings.Contains(err.Error(), "job b, is still waiting for its cleanup") {
		t.Fatalf("a dead, uncleaned job let the image go (or did not say why): %v", err)
	}
	prior[2].Cleaned = true
	if err := sandboxImageBusy(ctx, prior, lookup); err != nil {
		t.Fatalf("all settled: %v", err)
	}
	boom := errors.New("db down")
	if err := sandboxImageBusy(ctx, prior, func(context.Context, string) (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Fatalf("lookup failure: %v", err)
	}
}

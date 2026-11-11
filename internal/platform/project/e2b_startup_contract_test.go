package project

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

func TestStartupResponseRejectsCancellationExpiredAndUnready(t *testing.T) {
	live := map[string]any{"status": sandbox.StatusRunning, "ready": true, "shutdownTime": time.Now().Add(time.Minute).UTC().Format(time.RFC3339)}
	if err := startupResponseAlive(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := startupResponseAlive(ctx, live); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled create could return success: %v", err)
	}
	live["shutdownTime"] = time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
	if err := startupResponseAlive(context.Background(), live); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expired runtime accepted", err)
	}
	live["shutdownTime"] = time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	live["ready"] = false
	if err := startupResponseAlive(context.Background(), live); !errors.Is(err, sandbox.ErrNotRunning) {
		t.Fatal("billing RUNNING without current readiness accepted", err)
	}
}

func TestStartupOuterBudgetIncludesFinalAccessAndDisconnect(t *testing.T) {
	now := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	_, err := e2bColdAccess(ctx, time.Minute, time.Time{}, func() time.Time { return now }, func(context.Context) error { return nil }, func(context.Context) (*sandbox.Access, error) { cancel(); return &sandbox.Access{}, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal("disconnect during access produced usable success", err)
	}
	_, err = e2bColdAccess(context.Background(), time.Minute, time.Time{}, func() time.Time { return now }, func(context.Context) error { return nil }, func(context.Context) (*sandbox.Access, error) {
		now = now.Add(2 * time.Minute)
		return &sandbox.Access{}, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("late Access produced usable success", err)
	}
}

func TestCommittedReadyTTLBoundsFinalAccess(t *testing.T) {
	now := time.Now()
	live := map[string]any{"status": sandbox.StatusRunning, "ready": true, "shutdownTime": now.Add(time.Second).UTC().Format(time.RFC3339)}
	_, err := committedStartupAccess(context.Background(), live, func() time.Time { return now }, func(ctx context.Context) (*sandbox.Access, error) {
		if deadline, ok := ctx.Deadline(); !ok || deadline.After(now.Add(time.Second)) {
			t.Fatal("access not bounded by committed end")
		}
		now = now.Add(2 * time.Second)
		return &sandbox.Access{}, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expired committed TTL returned usable access", err)
	}
	live["shutdownTime"] = now.Add(time.Minute).UTC().Format(time.RFC3339)
	if _, err := committedStartupAccess(context.Background(), live, func() time.Time { return now }, func(context.Context) (*sandbox.Access, error) { return &sandbox.Access{}, nil }); err != nil {
		t.Fatal("live access refused", err)
	}
}

// A one-second committed runtime is a deterministic response boundary, not a
// promise that an entire HTTP/SQL create finishes within one wall-clock second.
func TestOneSecondCommittedRuntimeResponseAndAccessBoundary(t *testing.T) {
	// Keep the real context deadline in the future while all semantic lifetime
	// decisions use this fixed, explicitly advanced fake clock.
	first := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	end := first.Add(time.Second)
	for _, offset := range []time.Duration{0, 500 * time.Millisecond, time.Second - time.Nanosecond, time.Second, time.Second + time.Nanosecond} {
		t.Run(offset.String(), func(t *testing.T) {
			live := map[string]any{"status": sandbox.StatusRunning, "ready": true, "runningSince": first.Format(time.RFC3339), "shutdownTime": end.Format(time.RFC3339)}
			now := first.Add(offset)
			alive := offset < time.Second
			err := startupResponseAliveAt(context.Background(), live, now)
			if (alive && err != nil) || (!alive && !errors.Is(err, context.DeadlineExceeded)) {
				t.Fatal("one-second response boundary changed", err)
			}
			calls := 0
			access, err := committedStartupAccess(context.Background(), live, func() time.Time { return now }, func(ctx context.Context) (*sandbox.Access, error) {
				calls++
				if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(end) || ctx.Err() != nil {
					t.Fatal("access must keep the immutable one-second end as its real deadline")
				}
				return &sandbox.Access{}, nil
			})
			if alive {
				if err != nil || access == nil || calls != 1 {
					t.Fatal("live one-second access refused", err, calls)
				}
			} else if access != nil || !errors.Is(err, context.DeadlineExceeded) || calls != 0 {
				t.Fatal("at/past end invoked access or returned a usable response", err, calls)
			}
			if live["shutdownTime"] != end.Format(time.RFC3339) || live["runningSince"] != first.Format(time.RFC3339) {
				t.Fatal("response/access check renewed the original one-second runtime")
			}
		})
	}
	for _, late := range []time.Duration{0, time.Nanosecond} {
		t.Run("access-finishes-at-end/"+late.String(), func(t *testing.T) {
			live := map[string]any{"status": sandbox.StatusRunning, "ready": true, "runningSince": first.Format(time.RFC3339), "shutdownTime": end.Format(time.RFC3339)}
			now := first.Add(500 * time.Millisecond)
			calls := 0
			access, err := committedStartupAccess(context.Background(), live, func() time.Time { return now }, func(context.Context) (*sandbox.Access, error) {
				calls++
				now = end.Add(late)
				return &sandbox.Access{}, nil
			})
			if access != nil || !errors.Is(err, context.DeadlineExceeded) || calls != 1 || live["shutdownTime"] != end.Format(time.RFC3339) || live["runningSince"] != first.Format(time.RFC3339) {
				t.Fatal("late access renewed runtime or produced usable success", err, calls)
			}
		})
	}
}

func TestFinalLifetimeCheckAfterCleanupAndSiblingWait(t *testing.T) {
	live := map[string]any{"status": sandbox.StatusRunning, "ready": true, "shutdownTime": time.Now().Add(-time.Second).UTC().Format(time.RFC3339)}
	// Both resume's final response boundary and fork's post-join array boundary
	// use the captured committed row, never a rebased TTL or a fresh clock origin.
	if err := startupResponseAlive(context.Background(), live); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("cleanup/sibling wait exposed expired sandbox", err)
	}
}

func TestForkFinalSweepRejectsEarlierSuccessExpiredBySiblingCleanup(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Second)
	now := start
	row := func(id string, end time.Time) *cloud.CloudResource {
		return &cloud.CloudResource{ExternalID: id, Data: map[string]any{"status": sandbox.StatusRunning, "ready": true, "shutdownTime": end.Format(time.RFC3339)}}
	}
	rows := []*cloud.CloudResource{row("first", start.Add(time.Second)), row("second", start.Add(-time.Second)), row("still-live", start.Add(time.Minute))}
	results := []any{map[string]any{"sandbox": "first"}, map[string]any{"sandbox": "second"}, map[string]any{"sandbox": "still-live"}}
	var cleaned []string
	deferred := finalizeForkResults(context.Background(), rows, results, func() time.Time { return now }, func(ctx context.Context, row *cloud.CloudResource) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("fork cleanup must have finite deadline")
		}
		cleaned = append(cleaned, row.ExternalID)
		now = now.Add(2 * time.Second)
	})
	if len(cleaned) != 1 || cleaned[0] != "second" {
		t.Fatalf("blocking cleanup must precede final sweep: %v", cleaned)
	}
	if len(deferred) != 1 || deferred[0].ExternalID != "first" {
		t.Fatalf("newly expired earlier result must defer cleanup: %+v", deferred)
	}
	for _, i := range []int{0, 1} {
		if _, ok := results[i].(map[string]any)["error"]; !ok {
			t.Fatalf("expired fork %d remained successful: %v", i, results)
		}
	}
	if _, ok := results[2].(map[string]any)["sandbox"]; !ok {
		t.Fatal("still-live fork was lost")
	}
}

func TestRetainedSnapshotStorageCountsUniqueImmutableImages(t *testing.T) {
	tdoc := &sandboxTemplateDoc{Builds: []templateBuild{{Status: sandbox.BuildReady, Image: "registry.example.org/t/p/a@sha256:a", SizeBytes: 160}, {Status: sandbox.BuildReady, Image: "registry.example.org/t/p/a@sha256:a", SizeBytes: 160}, {Status: sandbox.BuildReady, Image: "registry.example.org/t/p/a@sha256:b", SizeBytes: 240}, {Status: sandbox.BuildError, Image: "registry.example.org/t/p/a@sha256:c", SizeBytes: 400}}}
	if got := templateStoredSize(tdoc); got != 400 {
		t.Fatalf("distinct digests additive, repeated digest once: got %d", got)
	}
}

func TestResumeLeaseCoversHardStartupAndSnapshotWithoutTakeover(t *testing.T) {
	minimum := snapshotWait + time.Duration(sandbox.MaxStartupSeconds)*time.Second + time.Minute
	if resumeLease <= minimum {
		t.Fatalf("resume lease %s cannot cover bounded critical path %s", resumeLease, minimum)
	}
	if e2bWakeLimit >= resumeLease || e2bWakeLimit < snapshotWait+time.Duration(sandbox.MaxStartupSeconds)*time.Second {
		t.Fatal("wake budget must cover critical path and end before lease")
	}
}

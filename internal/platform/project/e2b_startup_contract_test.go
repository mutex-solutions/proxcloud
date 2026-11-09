package project

import (
	"context"
	"errors"
	"testing"
	"time"

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

func TestFinalLifetimeCheckAfterCleanupAndSiblingWait(t *testing.T) {
	live := map[string]any{"status": sandbox.StatusRunning, "ready": true, "shutdownTime": time.Now().Add(-time.Second).UTC().Format(time.RFC3339)}
	// Both resume's final response boundary and fork's post-join array boundary
	// use the captured committed row, never a rebased TTL or a fresh clock origin.
	if err := startupResponseAlive(context.Background(), live); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("cleanup/sibling wait exposed expired sandbox", err)
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

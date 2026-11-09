package project

import (
	"context"
	"fmt"
	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"log/slog"
	"time"
)

// savedStartupAccess makes token retrieval part of resume/fork startup success. A failed
// response must not leave an unknown new fork or destroy a retryable paused snapshot.
func (h *Handler) savedStartupAccess(ctx context.Context, c *e2bCaller, id string) (*sandbox.Access, error) {
	return h.savedStartupAccessRow(ctx, c, id, h.e2bRow(ctx, c, id))
}
func (h *Handler) savedStartupAccessRow(ctx context.Context, c *e2bCaller, id string, row *cloud.CloudResource) (*sandbox.Access, error) {
	var a *sandbox.Access
	var err error
	if row != nil {
		a, err = committedStartupAccess(ctx, row.Data, time.Now, func(ctx context.Context) (*sandbox.Access, error) { return c.ss.AccessFor(ctx, c.proj.ID, id) })
	} else {
		a, err = c.ss.AccessFor(ctx, c.proj.ID, id)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		return a, nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if row := h.e2bRow(cleanup, c, id); row != nil {
		h.cleanupFailedStartup(cleanup, c, row)
	} else if derr := c.ss.DeleteSandbox(cleanup, c.proj.ID, id); derr != nil {
		slog.Error("e2b: cleanup after access failure", "sandbox", id, "err", derr)
	}
	return nil, err
}

// startupResponseAlive is checked after readiness, cache persistence and access reads.
// The runtime TTL was already committed by the first Ready observation; it cannot be
// extended by a late API response, a probe flap or the caller's retry.
func startupResponseAlive(ctx context.Context, data map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	end, err := time.Parse(time.RFC3339, fmt.Sprint(data["shutdownTime"]))
	if err != nil || !time.Now().Before(end) {
		return context.DeadlineExceeded
	}
	if data["ready"] != true || data["status"] != sandbox.StatusRunning {
		return sandbox.ErrNotRunning
	}
	return nil
}

// A disconnected create/resume is rolled back, not detached as an unobserved background create.
// Cleanup itself ignores cancellation but has a finite deadline; the CR's provisional startup
// end remains a second cleanup backstop if the cluster is temporarily unreachable.
func (h *Handler) cleanupFailedStartup(ctx context.Context, c *e2bCaller, row *cloud.CloudResource) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := h.sandboxDelete(cleanup, c.es, c.proj, row); err != nil {
		slog.Error("e2b: cleanup after failed startup", "sandbox", row.ExternalID, "err", err)
	}
}

// The first Ready TTL is committed before this helper. Its actual end (never
// the provisional create end) also bounds cache/access and final usable success.
func committedStartupAccess(ctx context.Context, live map[string]any, now func() time.Time, access func(context.Context) (*sandbox.Access, error)) (*sandbox.Access, error) {
	end, err := time.Parse(time.RFC3339, fmt.Sprint(live["shutdownTime"]))
	if err != nil {
		return nil, context.DeadlineExceeded
	}
	if err := e2bReadyAlive(ctx, end, now()); err != nil {
		return nil, err
	}
	if live["ready"] != true || live["status"] != sandbox.StatusRunning {
		return nil, sandbox.ErrNotRunning
	}
	ctx, cancel := context.WithDeadline(ctx, end)
	defer cancel()
	a, err := access(ctx)
	if err != nil {
		return nil, err
	}
	if err := e2bReadyAlive(ctx, end, now()); err != nil {
		return nil, err
	}
	return a, nil
}

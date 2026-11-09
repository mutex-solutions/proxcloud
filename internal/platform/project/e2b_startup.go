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
	a, err := c.ss.AccessFor(ctx, c.proj.ID, id)
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

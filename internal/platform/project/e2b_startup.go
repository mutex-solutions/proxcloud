package project

import (
	"context"
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

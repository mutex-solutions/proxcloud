package project

import (
	"context"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// reserveBuildLaunch serializes capacity using durable reservations. The caller
// observes Kubernetes before entering this short transaction; no cluster I/O or
// pin acquisition occurs while the project/name lock is held.
func (h *Handler) reserveBuildLaunch(ctx context.Context, c *e2bCaller, t *sandboxTemplateDoc, b *templateBuild, active map[string]bool) error {
	ctx, cancel := context.WithTimeout(ctx, templateMutationTimeout)
	defer cancel()
	return h.sbTemplates.DB().WithTx(ctx, func(tc context.Context) error {
		if err := h.sbTemplates.DB().LockXact(tc, projectTemplateLock(t.ProjectID)); err != nil {
			return err
		}
		if err := h.workerAdmission(tc, c, b.BuildID, active); err != nil {
			return err
		}
		var cur sandboxTemplateDoc
		found, err := h.sbTemplates.GetForUpdate(tc, t.ID, &cur)
		if err != nil {
			return err
		}
		if !found || cur.Deleting || cur.ProjectID != t.ProjectID {
			return errBuildPinState
		}
		cb := cur.build(b.BuildID)
		if cb == nil || cb.Import != nil || cb.Status != sandbox.BuildWaiting || cb.CancelRequested {
			return errBuildPinState
		}
		if cb.Registry != nil && (b.Registry == nil || cb.Registry.Operation != b.Registry.Operation || cb.Registry.State != buildPinned) {
			return errBuildPinState
		}
		cb.From, cb.StartCmd, cb.ReadyCmd = b.From, b.StartCmd, b.ReadyCmd
		cb.User, cb.Workdir, cb.StartAsRoot = b.User, b.Workdir, b.StartAsRoot
		cb.Status = sandbox.BuildBuilding
		now := time.Now().UTC()
		cb.LaunchStartedAt = &now
		_, err = h.sbTemplates.Replace(tc, cur.ID, &cur)
		if err == nil {
			*b = *cb
		}
		return err
	})
}

// recordBuildCancellation is an intent, NOT proof that Kubernetes had no effect.
// UNKNOWN registry receipts remain held, and sync may only finish them using the
// exact owned pod's positive terminal observation. There is no public cancel API.
func (h *Handler) recordBuildCancellation(ctx context.Context, t *sandboxTemplateDoc, b *templateBuild) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), buildCompensationTimeout)
	defer cancel()
	_ = h.sbTemplates.DB().WithTx(cleanup, func(tc context.Context) error {
		var cur sandboxTemplateDoc
		found, err := h.sbTemplates.GetForUpdate(tc, t.ID, &cur)
		if err != nil || !found || cur.ProjectID != t.ProjectID {
			return err
		}
		cb := cur.build(b.BuildID)
		if cb == nil {
			return nil
		}
		cb.CancelRequested = true
		_, err = h.sbTemplates.Replace(tc, cur.ID, &cur)
		return err
	})
}

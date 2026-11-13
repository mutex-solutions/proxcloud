package templateupdates

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
)

// SandboxExecutorFactory builds the executor for a payload at run time (the provider's cluster client, its mirror
// account, the GitHub integration's token source). Credentials never enter payloads or receipts.
type SandboxExecutorFactory func(ctx context.Context, p SandboxPayload) (*SandboxExecutor, error)

// SandboxRunner claims only TemplateUpdateSandbox jobs, one at a time, renewing the lease while the executor runs.
// Losing the lease cancels the executor's context and every further step is refused by the fence.
type SandboxRunner struct {
	Store    JobStore
	Receipts SandboxReceipts
	Build    SandboxExecutorFactory
	Lease    time.Duration
	Poll     time.Duration
	Log      *slog.Logger
	// Timeout bounds one acquisition (default 1h: the worker pod's deadline plus the prepull patch).
	Timeout time.Duration
	// Dead / ClaimDead: the dead-job cleanup pass (optional), like the VM runner.
	Dead      func(ctx context.Context) ([]string, error)
	ClaimDead func(ctx context.Context, id string, ttl time.Duration) (*durablejobs.Job, error)
	// OnFinish receives every job's final receipt (succeeded or failed): the system audit row (cmd/api).
	OnFinish func(rec SandboxReceipt)
	// CleanupOnly: the feature flag is off, so no job ever runs and no mirror starts. A job the runner can claim (queued,
	// or running with an EXPIRED lease: its runner is gone) is ended as feature_disabled through the terminal path, so
	// its Cleanup still runs and it stops blocking its image (retire). A running job with a live lease (another replica,
	// flag on) is never claimed. The dead-job pass and the orphan sweep run as usual, so worker pods and their
	// credential Secrets never outlive a job because the flag was turned off.
	CleanupOnly bool
	// Orphans, when set, deletes worker pods (and with them their Secrets) that belong to no live job; called at most
	// every OrphanEvery (default 10 minutes). It returns how many pods it deleted.
	Orphans     func(ctx context.Context) (int, error)
	OrphanEvery time.Duration
	lastOrphans time.Time
}

// Run polls until ctx ends.
func (r *SandboxRunner) Run(ctx context.Context) {
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.Poll <= 0 {
		r.Poll = 5 * time.Second
	}
	for ctx.Err() == nil {
		did, err := r.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			r.Log.Warn("sandbox template update runner", "err", err)
		}
		if did {
			continue
		}
		t := time.NewTimer(r.Poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// RunOnce claims and runs at most one job.
func (r *SandboxRunner) RunOnce(ctx context.Context) (bool, error) {
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.Lease <= 0 {
		r.Lease = time.Minute
	}
	r.sweepOrphans(ctx)
	// Also with the flag off: ClaimKinds is what expires a running job's lease (on the last attempt straight to dead),
	// and it only ever hands out a queued job or one whose lease expired, never one another replica holds.
	j, err := r.Store.ClaimKinds(ctx, r.Lease, []string{durablejobs.TemplateUpdateSandbox})
	if err != nil {
		return false, err
	}
	if j == nil {
		return r.cleanupDead(ctx)
	}
	if r.CleanupOnly {
		r.retire(ctx, j)
		return true, nil
	}
	r.perform(ctx, j)
	return true, nil
}

// retire ends a job claimed while the flag is off (CleanupOnly) WITHOUT running it: no mirror starts. It is
// dead-lettered as feature_disabled through terminal, so its Cleanup runs under this fenced, heartbeat-renewed
// acquisition (worker pods and their Secrets deleted, a pin whose prepull switch never landed restored). A job whose
// switch already landed (pinned) is acknowledged as succeeded instead, never rolled back; a failure an earlier
// acquisition already recorded keeps its code.
func (r *SandboxRunner) retire(parent context.Context, j *durablejobs.Job) {
	work, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	isLost, stopHB := heartbeat(r.Store, r.Lease, work, cancel, j)
	defer stopHB()
	fence := &jobFence{store: r.Store, job: j, lost: isLost}
	p, err := DecodeSandboxPayload(j.Payload)
	if err != nil {
		r.terminal(parent, j, fence, nil, SandboxPayload{}, &Permanent{Code: "invalid_payload", Message: "the job payload failed validation"})
		return
	}
	rec, err := r.Receipts.Load(work, j.ID)
	if err != nil {
		r.retry(parent, j, fence, "retryable")
		return
	}
	perm := &Permanent{Code: "feature_disabled", Message: "sandbox template updates are turned off (PROXCLOUD_TEMPLATE_UPDATES_SANDBOX_ENABLED); the job was ended without running"}
	var x *SandboxExecutor
	if rec != nil {
		switch {
		case rec.Step == SbxPinned || rec.Step == SbxSucceeded:
			r.succeed(parent, j, p, isLost)
			return
		case rec.Step == SbxFailed && rec.Code != "":
			perm = &Permanent{Code: rec.Code, Message: rec.Message}
		}
		// Unreachable provider: no Cleanup now; the receipt stays uncleaned, so the dead-job pass retries it (or records
		// why it cannot).
		if b, err := r.Build(work, p); err == nil {
			x = b
		}
	}
	r.terminal(parent, j, fence, x, p, perm)
}

func (r *SandboxRunner) sweepOrphans(ctx context.Context) {
	if r.Orphans == nil {
		return
	}
	every := r.OrphanEvery
	if every <= 0 {
		every = 10 * time.Minute
	}
	if !r.lastOrphans.IsZero() && time.Since(r.lastOrphans) < every {
		return
	}
	r.lastOrphans = time.Now()
	if n, err := r.Orphans(ctx); err != nil {
		r.Log.Warn("sandbox template update: orphan worker sweep failed", "err", err)
	} else if n > 0 {
		r.Log.Warn("sandbox template update: deleted orphan mirror worker pods", "count", n)
	}
}

func (r *SandboxRunner) perform(parent context.Context, j *durablejobs.Job) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = time.Hour
	}
	work, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	isLost, stopHB := heartbeat(r.Store, r.Lease, work, cancel, j)
	defer stopHB()
	fence := &jobFence{store: r.Store, job: j, lost: isLost}
	p, err := DecodeSandboxPayload(j.Payload)
	if err != nil {
		r.terminal(parent, j, fence, nil, SandboxPayload{}, &Permanent{Code: "invalid_payload", Message: "the job payload failed validation"})
		return
	}
	x, err := r.Build(work, p)
	if err != nil {
		var perm *Permanent
		if errors.As(err, &perm) {
			r.terminal(parent, j, fence, nil, p, perm)
			return
		}
		if j.Attempts >= j.MaxAttempts {
			r.terminal(parent, j, fence, nil, p, &Permanent{Code: "executor_unavailable", Message: "the sandbox provider was unavailable on every attempt"})
			return
		}
		r.retry(parent, j, fence, "executor_unavailable")
		return
	}
	err = x.Run(work, j.ID, p, fence)
	switch {
	case err == nil:
		r.succeed(parent, j, p, isLost)
	case errors.Is(err, ErrStopped) || isLost():
		r.Log.Warn("sandbox template update: lease lost; stopped", "job", j.ID, "epoch", j.Epoch)
	default:
		var perm *Permanent
		if errors.As(err, &perm) {
			r.terminal(parent, j, fence, x, p, perm)
			return
		}
		if j.Attempts >= j.MaxAttempts {
			r.terminal(parent, j, fence, x, p, &Permanent{Code: "attempts_exhausted", Message: "the update failed on every attempt"})
			return
		}
		r.retry(parent, j, fence, "retryable")
	}
}

// succeed acknowledges an acquisition whose prepull switch landed (receipt pinned): the receipt becomes succeeded in
// the job's fenced Complete.
func (r *SandboxRunner) succeed(parent context.Context, j *durablejobs.Job, p SandboxPayload, isLost func() bool) {
	cctx, ccancel := context.WithTimeout(context.WithoutCancel(parent), 15*time.Second)
	defer ccancel()
	if isLost() {
		return
	}
	e := r.Store.Complete(cctx, j, func(tc context.Context) error {
		rec, err := r.Receipts.Load(tc, j.ID)
		if err != nil || rec == nil {
			return errors.New("receipt missing")
		}
		rec.Step, rec.Epoch, rec.UpdatedAt, rec.Code = SbxSucceeded, j.Epoch, time.Now().UTC(), ""
		rec.Message = "The prepull DaemonSets run the new digest; templates switch once every Ready node has pulled it"
		if p.Action == SandboxRollback {
			rec.Message = "The prepull DaemonSets run the previous digest again; templates switch back once every Ready node has it"
		}
		return r.Receipts.Save(tc, rec)
	})
	if e != nil && !errors.Is(e, durablejobs.ErrLeaseLost) {
		r.Log.Warn("sandbox template update: complete failed", "job", j.ID)
	}
	r.finished(cctx, j.ID)
}

func (r *SandboxRunner) retry(parent context.Context, j *durablejobs.Job, fence *jobFence, code string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	if fence.lost() {
		return
	}
	if err := r.Store.Fail(ctx, j, durablejobs.Backoff(j.Attempts)+30*time.Second, false, code); err != nil && !errors.Is(err, durablejobs.ErrLeaseLost) {
		r.Log.Warn("sandbox template update: retry scheduling failed", "job", j.ID)
	}
	r.Log.Warn("sandbox template update attempt failed; will retry", "job", j.ID, "attempt", j.Attempts, "code", code)
}

// terminal records the failure (fenced), cleans up only this job's own resources, then dead-letters the job.
func (r *SandboxRunner) terminal(parent context.Context, j *durablejobs.Job, fence *jobFence, x *SandboxExecutor, p SandboxPayload, perm *Permanent) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Minute)
	defer cancel()
	var rec *SandboxReceipt
	err := fence.Checkpoint(ctx, func(tc context.Context) error {
		got, err := r.Receipts.Load(tc, j.ID)
		if err != nil || got == nil {
			return err
		}
		got.Step, got.Epoch, got.UpdatedAt, got.Code, got.Message = SbxFailed, j.Epoch, time.Now().UTC(), perm.Code, perm.Message
		rec = got
		return r.Receipts.Save(tc, got)
	})
	if err != nil {
		return
	}
	if x != nil && rec != nil {
		warns := x.Cleanup(ctx, fence, j.ID, p, rec)
		if !stopped(warns) {
			_ = fence.Checkpoint(ctx, func(tc context.Context) error {
				rec.Warnings, rec.Cleaned = append(rec.Warnings, warns...), true
				return r.Receipts.Save(tc, rec)
			})
		}
	}
	if err := r.Store.Fail(ctx, j, 0, true, perm.Code); err != nil && !errors.Is(err, durablejobs.ErrLeaseLost) {
		r.Log.Warn("sandbox template update: dead-letter failed", "job", j.ID)
	}
	r.Log.Warn("sandbox template update failed", "job", j.ID, "code", perm.Code)
	r.finished(ctx, j.ID)
}

// finished hands the final receipt to OnFinish (the system audit row), best-effort.
func (r *SandboxRunner) finished(ctx context.Context, id string) {
	if r.OnFinish == nil {
		return
	}
	if rec, err := r.Receipts.Load(ctx, id); err == nil && rec != nil && (rec.Step == SbxSucceeded || rec.Step == SbxFailed) {
		r.OnFinish(*rec)
	}
}

// releaseForeign hands a dead job of another kind (a miswired Dead list) straight back to dead WITHOUT cleaned_up and
// with its own failure code, so its own runner's dead-job pass still cleans it up.
func (r *SandboxRunner) releaseForeign(ctx context.Context, j *durablejobs.Job) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	code := j.LastError
	if code == "" || code == "cleaned_up" {
		code = "cleanup_pending"
	}
	err := r.Store.Fail(fctx, j, 0, true, code)
	if err != nil && code != "cleanup_pending" && !errors.Is(err, durablejobs.ErrLeaseLost) {
		err = r.Store.Fail(fctx, j, 0, true, "cleanup_pending") // its code was not a fixed one
	}
	r.Log.Error("sandbox template update: the dead-job pass was handed a job of another kind; released it untouched",
		"job", j.ID, "kind", j.Kind, "released", err == nil)
}

// cleanupDead re-acquires one dead, uncleaned sandbox job and runs Cleanup under that fenced acquisition. A job of
// another kind is released at once (releaseForeign): never cleaned up here, never marked cleaned_up.
func (r *SandboxRunner) cleanupDead(ctx context.Context) (bool, error) {
	if r.Dead == nil || r.ClaimDead == nil {
		return false, nil
	}
	ids, err := r.Dead(ctx)
	if err != nil || len(ids) == 0 {
		return false, err
	}
	j, err := r.ClaimDead(ctx, ids[0], r.Lease)
	if err != nil || j == nil {
		return false, err
	}
	if j.Kind != durablejobs.TemplateUpdateSandbox {
		r.releaseForeign(ctx, j)
		return false, nil
	}
	work, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	isLost, stopHB := heartbeat(r.Store, r.Lease, work, cancel, j)
	defer stopHB()
	fence := &jobFence{store: r.Store, job: j, lost: isLost}
	finish := func(warns []string) bool {
		err := fence.Checkpoint(work, func(tc context.Context) error {
			rec, err := r.Receipts.Load(tc, j.ID)
			if err != nil || rec == nil {
				return err
			}
			switch rec.Step {
			case SbxSucceeded:
			case SbxPinned:
				rec.Step, rec.Message = SbxSucceeded, "The prepull DaemonSets run the new digest (the job ended before acknowledging)"
			default:
				rec.Step = SbxFailed
				if rec.Code == "" {
					rec.Code, rec.Message = "job_dead", "the job ended without completing; its worker pods were cleaned up"
				}
			}
			rec.Cleaned, rec.Warnings, rec.UpdatedAt = true, append(rec.Warnings, warns...), time.Now().UTC()
			return r.Receipts.Save(tc, rec)
		})
		if err != nil {
			return false
		}
		_ = r.Store.Fail(context.WithoutCancel(work), j, 0, true, "cleaned_up")
		return true
	}
	p, err := DecodeSandboxPayload(j.Payload)
	if err != nil {
		return finish([]string{"the job payload is invalid; nothing could be cleaned up"}), nil
	}
	rec, err := r.Receipts.Load(work, j.ID)
	if err != nil {
		_ = r.Store.Fail(context.WithoutCancel(work), j, 0, true, "cleanup_pending")
		return false, nil
	}
	x, err := r.Build(work, p)
	if err != nil {
		var perm *Permanent
		if errors.As(err, &perm) {
			return finish([]string{"cleanup impossible (" + perm.Code + "): remove any tu-mirror-* pod in sbx-build by hand"}), nil
		}
		_ = r.Store.Fail(context.WithoutCancel(work), j, 0, true, "cleanup_pending")
		return false, nil
	}
	warns := x.Cleanup(work, fence, j.ID, p, rec)
	if stopped(warns) || isLost() {
		return false, nil
	}
	if !finish(warns) {
		return false, nil
	}
	r.Log.Warn("sandbox template update: cleaned up after a dead job", "job", j.ID)
	return true, nil
}

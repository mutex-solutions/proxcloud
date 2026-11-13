package templateupdates

// smoke_runner.go — the runner's handling of templateupdate.smoke/v1 jobs: the same lease renewal,
// fencing, terminal cleanup and dead-job pass as the import job.

import (
	"context"
	"errors"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
)

// smokeLockRetry is how long a smoke job waits when another smoke test holds its provider's lock.
const smokeLockRetry = time.Minute

func (r *Runner) performSmoke(parent context.Context, j *durablejobs.Job) {
	timeout := r.SmokeTimeout
	if timeout <= 0 {
		timeout = time.Hour
	}
	work, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	isLost, stopHB := r.heartbeat(work, cancel, j)
	defer stopHB()
	fence := &jobFence{store: r.Store, job: j, lost: isLost}
	p, err := DecodeSmokePayload(j.Payload)
	if err != nil {
		r.terminalSmoke(parent, j, fence, nil, SmokePayload{}, &Permanent{Code: SmokeInvalidPayload, Message: "the smoke job payload failed validation"})
		return
	}
	if r.SmokeLock != nil {
		release, ok, err := r.SmokeLock(work, p.LockKey())
		if err != nil || !ok {
			// Another smoke test runs on this provider (or the lock is unreachable): not this job's
			// failure, so no attempt is spent.
			dctx, dcancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
			defer dcancel()
			if !isLost() {
				_ = r.Store.Defer(dctx, j, smokeLockRetry, "smoke_lock_busy")
			}
			return
		}
		defer release()
	}
	x, err := r.BuildSmoke(work, p)
	if err != nil {
		var perm *Permanent
		if errors.As(err, &perm) {
			r.terminalSmoke(parent, j, fence, nil, p, perm)
			return
		}
		if j.Attempts >= j.MaxAttempts {
			r.terminalSmoke(parent, j, fence, nil, p, &Permanent{Code: "executor_unavailable", Message: "the provider was unavailable on every attempt"})
			return
		}
		r.retrySmoke(parent, j, fence, "executor_unavailable")
		return
	}
	err = x.Run(work, j.ID, p, fence)
	switch {
	case err == nil:
		if isLost() {
			return
		}
		cctx, ccancel := context.WithTimeout(context.WithoutCancel(parent), 15*time.Second)
		defer ccancel()
		if e := r.Store.Complete(cctx, j, nil); e != nil && !errors.Is(e, durablejobs.ErrLeaseLost) {
			r.Log.Warn("template smoke: complete failed", "job", j.ID)
		}
	case errors.Is(err, ErrStopped) || isLost():
		r.Log.Warn("template smoke: lease lost; stopped", "job", j.ID, "epoch", j.Epoch)
	default:
		var perm *Permanent
		if errors.As(err, &perm) {
			r.terminalSmoke(parent, j, fence, x, p, perm)
			return
		}
		if j.Attempts >= j.MaxAttempts {
			r.terminalSmoke(parent, j, fence, x, p, &Permanent{Code: SmokeAttemptsExhausted, Message: "the smoke test could not run to the end on any attempt (provider errors)"})
			return
		}
		r.retrySmoke(parent, j, fence, "retryable")
	}
}

func (r *Runner) retrySmoke(parent context.Context, j *durablejobs.Job, fence *jobFence, code string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	if fence.lost() {
		return
	}
	if err := r.Store.Fail(ctx, j, durablejobs.Backoff(j.Attempts)+30*time.Second, false, code); err != nil && !errors.Is(err, durablejobs.ErrLeaseLost) {
		r.Log.Warn("template smoke: retry scheduling failed", "job", j.ID)
	}
	r.Log.Warn("template smoke attempt failed; will retry", "job", j.ID, "attempt", j.Attempts, "code", code)
}

// terminalSmoke records FAIL (unless a result was already recorded), removes this job's test VM,
// and marks the job dead. A lost fence skips cleanup: another acquisition owns the job.
func (r *Runner) terminalSmoke(parent context.Context, j *durablejobs.Job, fence *jobFence, x *SmokeExecutor, p SmokePayload, perm *Permanent) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Minute)
	defer cancel()
	var rec *SmokeReceipt
	err := fence.Checkpoint(ctx, func(tc context.Context) error {
		got, err := r.Smokes.LoadSmoke(tc, j.ID)
		if err != nil || got == nil {
			return err
		}
		now := time.Now().UTC()
		if got.Result == "" {
			got.Result, got.Code, got.Message, got.FinishedAt = SmokeFail, perm.Code, perm.Message, &now
		}
		got.Epoch, got.UpdatedAt = j.Epoch, now
		rec = got
		return r.Smokes.SaveSmoke(tc, got)
	})
	if err != nil {
		return
	}
	if x != nil && rec != nil {
		warns := x.Cleanup(ctx, fence, j.ID, p, rec)
		if !stopped(warns) {
			_ = fence.Checkpoint(ctx, func(tc context.Context) error {
				now := time.Now().UTC()
				rec.Warnings, rec.Cleaned, rec.Step, rec.CleanedAt = append(rec.Warnings, warns...), true, SmokeFinished, &now
				return r.Smokes.SaveSmoke(tc, rec)
			})
		}
	}
	if err := r.Store.Fail(ctx, j, 0, true, perm.Code); err != nil && !errors.Is(err, durablejobs.ErrLeaseLost) {
		r.Log.Warn("template smoke: dead-letter failed", "job", j.ID)
	}
	r.Log.Warn("template smoke ended without a passing run", "job", j.ID, "code", perm.Code)
}

// cleanupDeadSmoke is cleanupDead for smoke jobs: a dead smoke job's test VM is removed under a
// fenced, heartbeat-renewed ClaimDead acquisition and its receipt gets a FAIL result (job_dead) if it
// had none. Same throttling: "try later" returns did=false.
func (r *Runner) cleanupDeadSmoke(ctx context.Context) (bool, error) {
	if r.DeadSmoke == nil || r.ClaimDead == nil {
		return false, nil
	}
	ids, err := r.DeadSmoke(ctx)
	if err != nil || len(ids) == 0 {
		return false, err
	}
	j, err := r.ClaimDead(ctx, ids[0], r.Lease)
	if err != nil || j == nil {
		return false, err
	}
	work, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	isLost, stopHB := r.heartbeat(work, cancel, j)
	defer stopHB()
	fence := &jobFence{store: r.Store, job: j, lost: isLost}
	later := func(code string) (bool, error) {
		_ = r.Store.Fail(context.WithoutCancel(work), j, 0, true, code)
		return false, nil
	}
	finish := func(warns []string) bool {
		err := fence.Checkpoint(work, func(tc context.Context) error {
			rec, err := r.Smokes.LoadSmoke(tc, j.ID)
			if err != nil || rec == nil {
				return err
			}
			now := time.Now().UTC()
			if rec.Result == "" {
				rec.Result, rec.Code, rec.Message, rec.FinishedAt = SmokeFail, SmokeJobDead, "the smoke job ended without a result; its test VM was cleaned up", &now
			}
			rec.Step, rec.Cleaned, rec.CleanedAt, rec.Warnings, rec.UpdatedAt = SmokeFinished, true, &now, append(rec.Warnings, warns...), now
			return r.Smokes.SaveSmoke(tc, rec)
		})
		if err != nil {
			return false
		}
		_ = r.Store.Fail(context.WithoutCancel(work), j, 0, true, "cleaned_up")
		return true
	}
	p, err := DecodeSmokePayload(j.Payload)
	if err != nil {
		return finish([]string{"the smoke job payload is invalid; nothing could be cleaned up"}), nil
	}
	rec, err := r.Smokes.LoadSmoke(work, j.ID)
	if err != nil {
		return later("cleanup_pending")
	}
	x, err := r.BuildSmoke(work, p)
	if err != nil {
		var perm *Permanent
		if errors.As(err, &perm) {
			r.Log.Warn("template smoke: dead job's test VM is unreachable; not retrying cleanup", "job", j.ID, "code", perm.Code)
			return finish([]string{"cleanup impossible (" + perm.Code + "): " + perm.Message + "; remove any leftover pc-tu-smoke- VM by hand"}), nil
		}
		return later("cleanup_pending")
	}
	warns := x.Cleanup(work, fence, j.ID, p, rec)
	if stopped(warns) || isLost() {
		return false, nil
	}
	if !finish(warns) {
		return false, nil
	}
	r.Log.Warn("template smoke: cleaned up after a dead job", "job", j.ID)
	return true, nil
}

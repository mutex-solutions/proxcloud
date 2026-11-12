package templateupdates

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// JobStore is the subset of *durablejobs.Store the runner uses (a fake in unit tests).
type JobStore interface {
	ClaimKinds(ctx context.Context, ttl time.Duration, kinds []string) (*durablejobs.Job, error)
	Renew(ctx context.Context, j *durablejobs.Job, ttl time.Duration) error
	Checkpoint(ctx context.Context, j *durablejobs.Job, fn func(context.Context) error) error
	Complete(ctx context.Context, j *durablejobs.Job, fn func(context.Context) error) error
	Fail(ctx context.Context, j *durablejobs.Job, delay time.Duration, permanent bool, code string) error
}

// jobFence binds a Checkpoint to one acquisition.
type jobFence struct {
	store JobStore
	job   *durablejobs.Job
	lost  func() bool
}

func (f *jobFence) Epoch() int64 { return f.job.Epoch }
func (f *jobFence) Checkpoint(ctx context.Context, fn func(context.Context) error) error {
	if f.lost() {
		return durablejobs.ErrLeaseLost
	}
	return f.store.Checkpoint(ctx, f.job, fn)
}

// ExecutorFactory builds the executor for a payload at run time: the provider's PVE client and a
// GitHub client from the CURRENT integration token (tokens never enter payloads or receipts).
type ExecutorFactory func(ctx context.Context, p VMPayload) (*Executor, error)

// Runner claims only TemplateUpdateVM jobs, one at a time, renewing the lease while the
// executor runs. Losing the lease cancels the executor's context and every further step is
// refused by the fence.
type Runner struct {
	Store    JobStore
	Receipts Receipts
	Build    ExecutorFactory
	Lease    time.Duration
	Poll     time.Duration
	Log      *slog.Logger
	// Timeout bounds one acquisition's work (default 2h); must exceed one upload+import.
	Timeout time.Duration
}

// Run polls until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.Poll <= 0 {
		r.Poll = 5 * time.Second
	}
	for ctx.Err() == nil {
		did, err := r.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			r.Log.Warn("template update runner", "err", err)
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

// RunOnce claims and runs at most one job; did reports whether a job was claimed.
func (r *Runner) RunOnce(ctx context.Context) (did bool, err error) {
	if r.Lease < 10*time.Second {
		r.Lease = time.Minute
	}
	j, err := r.Store.ClaimKinds(ctx, r.Lease, []string{durablejobs.TemplateUpdateVM})
	if err != nil || j == nil {
		return false, err
	}
	r.perform(ctx, j)
	return true, nil
}

func (r *Runner) perform(parent context.Context, j *durablejobs.Job) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Hour
	}
	work, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	var mu sync.Mutex
	lost := false
	isLost := func() bool { mu.Lock(); defer mu.Unlock(); return lost }
	// Heartbeat: renew at a third of the lease; a lost lease cancels the work context.
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		t := time.NewTicker(r.Lease / 3)
		defer t.Stop()
		for {
			select {
			case <-work.Done():
				return
			case <-t.C:
				rc, rcancel := context.WithTimeout(work, r.Lease/3)
				err := r.Store.Renew(rc, j, r.Lease)
				rcancel()
				if errors.Is(err, durablejobs.ErrLeaseLost) {
					mu.Lock()
					lost = true
					mu.Unlock()
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-hbDone }()

	fence := &jobFence{store: r.Store, job: j, lost: isLost}
	p, err := DecodeVMPayload(j.Payload)
	if err != nil {
		r.terminal(parent, j, fence, nil, VMPayload{}, &Permanent{Code: "invalid_payload", Message: "the job payload failed validation"})
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
			r.terminal(parent, j, fence, nil, p, &Permanent{Code: "executor_unavailable", Message: "the provider or the GitHub integration was unavailable on every attempt"})
			return
		}
		r.retry(parent, j, fence, p, "executor_unavailable")
		return
	}
	err = x.Run(work, j.ID, p, fence)
	switch {
	case err == nil:
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
			rec.Step, rec.Epoch, rec.UpdatedAt, rec.Code, rec.Message = StepSucceeded, j.Epoch, time.Now().UTC(), "", "Imported as a new template"
			return r.Receipts.Save(tc, rec)
		})
		if e != nil && !errors.Is(e, durablejobs.ErrLeaseLost) {
			r.Log.Warn("template update: complete failed", "job", j.ID)
		}
	case errors.Is(err, ErrStopped) || isLost():
		r.Log.Warn("template update: lease lost; stopped", "job", j.ID, "epoch", j.Epoch)
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
		r.retry(parent, j, fence, p, "retryable")
	}
}

func (r *Runner) retry(parent context.Context, j *durablejobs.Job, fence *jobFence, p VMPayload, code string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	if fence.lost() {
		return
	}
	if err := r.Store.Fail(ctx, j, durablejobs.Backoff(j.Attempts)+30*time.Second, false, code); err != nil && !errors.Is(err, durablejobs.ErrLeaseLost) {
		r.Log.Warn("template update: retry scheduling failed", "job", j.ID)
	}
	r.Log.Warn("template update attempt failed; will retry", "job", j.ID, "attempt", j.Attempts, "code", code)
}

// terminal records the failure in the receipt (fenced), then cleans up ONLY this job's own
// resources, then marks the job dead. A lost fence skips cleanup: another acquisition owns it.
func (r *Runner) terminal(parent context.Context, j *durablejobs.Job, fence *jobFence, x *Executor, p VMPayload, perm *Permanent) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Minute)
	defer cancel()
	var rec *Receipt
	err := fence.Checkpoint(ctx, func(tc context.Context) error {
		got, err := r.Receipts.Load(tc, j.ID)
		if err != nil {
			return err
		}
		if got == nil {
			return nil
		}
		got.Step, got.Epoch, got.UpdatedAt, got.Code, got.Message = StepFailed, j.Epoch, time.Now().UTC(), perm.Code, perm.Message
		rec = got
		return r.Receipts.Save(tc, got)
	})
	if err != nil {
		return
	}
	if x != nil && rec != nil {
		if warns := x.Cleanup(ctx, j.ID, p, rec); len(warns) > 0 {
			_ = fence.Checkpoint(ctx, func(tc context.Context) error {
				rec.Warnings = append(rec.Warnings, warns...)
				return r.Receipts.Save(tc, rec)
			})
		}
	}
	if err := r.Store.Fail(ctx, j, 0, true, perm.Code); err != nil && !errors.Is(err, durablejobs.ErrLeaseLost) {
		r.Log.Warn("template update: dead-letter failed", "job", j.ID)
	}
	r.Log.Warn("template update failed", "job", j.ID, "code", perm.Code)
}

// ── receipts on pgdoc ─────────────────────────────────────────────────────────────────────────

// ReceiptCollection holds one receipt per job (id = durable job id).
const ReceiptCollection = "templateUpdate"

// PGReceipts stores receipts in pgdoc. Inside Checkpoint the context carries the job
// transaction, so receipt writes commit atomically with the fence check.
type PGReceipts struct{ DB *pgdoc.DB }

func (s PGReceipts) Load(ctx context.Context, id string) (*Receipt, error) {
	var r Receipt
	found, err := s.DB.C(ReceiptCollection).Get(ctx, id, &r)
	if err != nil || !found {
		return nil, err
	}
	return &r, nil
}

func (s PGReceipts) Save(ctx context.Context, r *Receipt) error {
	return s.DB.C(ReceiptCollection).Upsert(ctx, r.ID, r)
}

// Recent lists the newest receipts (history for the admin page).
func (s PGReceipts) Recent(ctx context.Context, limit int64) ([]Receipt, error) {
	out := []Receipt{}
	err := s.DB.C(ReceiptCollection).Find(ctx, nil, &out, pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime)), pgdoc.Limit(limit))
	return out, err
}

// ByKey lists receipts of the same service/node/digest (dedup on enqueue).
func (s PGReceipts) ByKey(ctx context.Context, serviceID, node, digest string) ([]Receipt, error) {
	out := []Receipt{}
	err := s.DB.C(ReceiptCollection).Find(ctx, pgdoc.M{"payload.serviceId": serviceID, "payload.node": node, "payload.digest": digest}, &out)
	return out, err
}

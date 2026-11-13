package providerattach

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// JobStore is the subset of *durablejobs.Store the runner uses.
type JobStore interface {
	ClaimKinds(ctx context.Context, ttl time.Duration, kinds []string) (*durablejobs.Job, error)
	Renew(ctx context.Context, j *durablejobs.Job, ttl time.Duration) error
	Checkpoint(ctx context.Context, j *durablejobs.Job, fn func(context.Context) error) error
	Complete(ctx context.Context, j *durablejobs.Job, fn func(context.Context) error) error
	Fail(ctx context.Context, j *durablejobs.Job, delay time.Duration, permanent bool, code string) error
}

// Outcomes the Attach function answers (project.Attach* constants, mirrored so this package needs no project import).
const (
	OutcomeAttached        = "attached"
	OutcomeAlreadyAttached = "already_attached"
	OutcomeNotFound        = "not_found"
	OutcomeDeleting        = "deleting"
)

// AttachFunc attaches one project to the provider through the per-project attach path (project.Handler.AttachByOperator
// with bulk=true): the outcome, or the provisioning error (never stored, only classified).
type AttachFunc func(ctx context.Context, projectID string, es *externalservice.ExternalService) (string, error)

// ProviderFunc loads the provider (decrypted); nil when it no longer exists.
type ProviderFunc func(ctx context.Context, id string) (*externalservice.ExternalService, error)

// Runner claims ProviderAttach jobs, one at a time, renews the lease while it works and fences every write.
type Runner struct {
	Store    JobStore
	Repo     Repo
	Provider ProviderFunc
	Attach   AttachFunc
	Lease    time.Duration
	Poll     time.Duration
	Log      *slog.Logger
	// AttemptTimeout bounds one project's provisioning (default 2 minutes).
	AttemptTimeout time.Duration
}

// Run polls until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	r.defaults()
	for ctx.Err() == nil {
		did, err := r.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			r.Log.Warn("provider attach runner", "err", err)
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

func (r *Runner) defaults() {
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.Poll <= 0 {
		r.Poll = 5 * time.Second
	}
	if r.Lease <= 0 {
		r.Lease = time.Minute
	}
	if r.AttemptTimeout <= 0 {
		r.AttemptTimeout = 2 * time.Minute
	}
}

// RunOnce claims and runs at most one job; did reports whether one was claimed.
func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	r.defaults()
	j, err := r.Store.ClaimKinds(ctx, r.Lease, []string{durablejobs.ProviderAttach})
	if err != nil || j == nil {
		return false, err
	}
	r.perform(ctx, j)
	return true, nil
}

// errStop ends an acquisition without acknowledging it: the lease was lost (another acquisition owns the job) or the
// process is stopping (the next acquisition resumes).
var errStop = errors.New("provider attach: stopped")

type acquisition struct {
	r     *Runner
	j     *durablejobs.Job
	lost  func() bool
	runID string
}

// checkpoint commits fn only while this acquisition still owns the job.
func (a *acquisition) checkpoint(ctx context.Context, fn func(context.Context) error) error {
	if a.lost() {
		return errStop
	}
	err := a.r.Store.Checkpoint(context.WithoutCancel(ctx), a.j, fn)
	if errors.Is(err, durablejobs.ErrLeaseLost) {
		return errStop
	}
	return err
}

func (r *Runner) perform(parent context.Context, j *durablejobs.Job) {
	work, cancel := context.WithCancel(parent)
	defer cancel()
	isLost, stopHB := r.heartbeat(work, cancel, j)
	defer stopHB()
	p, err := DecodePayload(j.Payload)
	if err != nil {
		r.fail(parent, j, isLost, true, "invalid_payload")
		return
	}
	a := &acquisition{r: r, j: j, lost: isLost, runID: p.RunID}
	err = r.execute(work, a, p)
	switch {
	case err == nil:
	case errors.Is(err, errStop) || isLost() || parent.Err() != nil:
		r.Log.Warn("provider attach: stopped; the next acquisition resumes", "job", j.ID, "epoch", j.Epoch)
	default:
		// A database hiccup (never a provider error: those are per-project results). Retry the job; its attempts are bounded.
		r.Log.Warn("provider attach: attempt failed", "job", j.ID, "attempt", j.Attempts)
		r.fail(parent, j, isLost, false, "retryable")
	}
}

func (r *Runner) fail(parent context.Context, j *durablejobs.Job, isLost func() bool, permanent bool, code string) {
	if isLost() {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	if err := r.Store.Fail(ctx, j, durablejobs.Backoff(j.Attempts), permanent, code); err != nil && !errors.Is(err, durablejobs.ErrLeaseLost) {
		r.Log.Warn("provider attach: could not record the failure", "job", j.ID)
	}
}

// execute plans the run once, then attaches its pending items with bounded concurrency, then finishes it.
func (r *Runner) execute(ctx context.Context, a *acquisition, p Payload) error {
	run, err := r.Repo.LoadRun(ctx, p.RunID)
	if err != nil {
		return err
	}
	if run == nil || run.ServiceID != p.ServiceID {
		return r.complete(ctx, a, nil, RunFailed, "run_missing")
	}
	if Terminal(run.Status) {
		return r.complete(ctx, a, run, run.Status, run.Code)
	}
	es, err := r.Provider(ctx, p.ServiceID)
	if err != nil {
		return err
	}
	switch {
	case es == nil:
		return r.abort(ctx, a, run, CodeProviderMissing)
	case es.IsDisabled():
		return r.abort(ctx, a, run, CodeProviderDisabled)
	}
	if err := r.start(ctx, a, run); err != nil {
		return err
	}
	if !run.Planned {
		if err := r.plan(ctx, a, run, p.ServiceID); err != nil {
			return err
		}
	}
	// Items a dead acquisition left running: attach them again (the attach is a no-op where it had landed), unless they
	// already took their attempts.
	if err := r.requeueRunning(ctx, a, run.ID); err != nil {
		return err
	}
	if err := r.workers(ctx, a, run, es); err != nil {
		return err
	}
	fresh, err := r.Repo.LoadRun(ctx, run.ID)
	if err != nil {
		return err
	}
	if fresh != nil && fresh.CancelRequested {
		return r.cancelRest(ctx, a, fresh)
	}
	counts, err := r.Repo.Counts(ctx, run.ID)
	if err != nil {
		return err
	}
	status := RunSucceeded
	if counts.Failed > 0 {
		status = RunCompletedWithFailures
	}
	return r.complete(ctx, a, fresh, status, "")
}

// start marks the run running (fenced).
func (r *Runner) start(ctx context.Context, a *acquisition, run *Run) error {
	if run.Status == RunRunning && run.StartedAt != nil {
		return nil
	}
	return a.checkpoint(ctx, func(tc context.Context) error {
		cur, err := r.Repo.LoadRunForUpdate(tc, run.ID)
		if err != nil || cur == nil {
			return err
		}
		now := time.Now().UTC()
		cur.Status = RunRunning
		if cur.StartedAt == nil {
			cur.StartedAt = &now
		}
		*run = *cur
		return r.Repo.SaveRun(tc, cur)
	})
}

// plan lists the scoped projects into items, once, in the same fenced transaction that marks the run planned. Projects
// already attached and projects on their way out are recorded as skipped right away; the rest are pending.
func (r *Runner) plan(ctx context.Context, a *acquisition, run *Run, serviceID string) error {
	var items []Item
	now := time.Now().UTC()
	add := func(p ProjectRef, missing bool) {
		it := Item{ID: ItemID(run.ID, p.ID), RunID: run.ID, ProjectID: p.ID, ProjectName: p.Name, OrganizationID: p.OrganizationID,
			State: ItemPending, UpdatedAt: now}
		switch {
		case missing:
			it.State, it.Code = ItemSkipped, CodeNotFound
		case p.HasService(serviceID):
			it.State, it.Code = ItemSkipped, CodeAlreadyAttached
		case p.Deleting():
			it.State, it.Code = ItemSkipped, CodeDeleting
		}
		items = append(items, it)
	}
	switch run.Scope.Mode {
	case ScopeRetry:
		src, err := r.Repo.LoadRun(ctx, run.Scope.SourceRunID)
		if err != nil {
			return err
		}
		states := []string{ItemFailed}
		if src != nil && src.Status != RunSucceeded && src.Status != RunCompletedWithFailures && src.Status != RunCancelled {
			states = append(states, ItemPending, ItemRunning) // the source run died before finishing them
		}
		var ids []string
		if src != nil {
			prev, err := r.Repo.ItemsInState(ctx, src.ID, states...)
			if err != nil {
				return err
			}
			for _, it := range prev {
				ids = append(ids, it.ProjectID)
			}
		}
		projects, err := r.Repo.ProjectsByID(ctx, ids)
		if err != nil {
			return err
		}
		for _, id := range ids {
			p, ok := projects[id]
			if !ok {
				p = ProjectRef{ID: id}
			}
			add(p, !ok)
		}
	case ScopeProjects:
		projects, err := r.Repo.ProjectsByID(ctx, run.Scope.ProjectIDs)
		if err != nil {
			return err
		}
		for _, id := range run.Scope.ProjectIDs {
			p, ok := projects[id]
			if !ok {
				p = ProjectRef{ID: id}
			}
			add(p, !ok)
		}
	default:
		projects, err := r.Repo.ScopedProjects(ctx, run.Scope)
		if err != nil {
			return err
		}
		for _, p := range projects {
			add(p, false)
		}
	}
	return a.checkpoint(ctx, func(tc context.Context) error {
		cur, err := r.Repo.LoadRunForUpdate(tc, run.ID)
		if err != nil || cur == nil {
			return err
		}
		if cur.Planned {
			*run = *cur
			return nil
		}
		if err := r.Repo.InsertItems(tc, items); err != nil {
			return err
		}
		cur.Planned = true
		*run = *cur
		return r.Repo.SaveRun(tc, cur)
	})
}

// requeueRunning puts back the items an earlier acquisition left running.
func (r *Runner) requeueRunning(ctx context.Context, a *acquisition, runID string) error {
	stale, err := r.Repo.ItemsInState(ctx, runID, ItemRunning)
	if err != nil || len(stale) == 0 {
		return err
	}
	return a.checkpoint(ctx, func(tc context.Context) error {
		for i := range stale {
			it := stale[i]
			if it.Attempts >= MaxItemAttempts {
				it.State, it.Code = ItemFailed, CodeAttemptsExhausted
			} else {
				it.State, it.Code = ItemPending, ""
			}
			if err := r.Repo.SaveItem(tc, &it); err != nil {
				return err
			}
		}
		return nil
	})
}

// workers attaches the pending items with run.Concurrency goroutines. A worker stops on a cancel request, a lost lease
// or a database error (the first error wins).
func (r *Runner) workers(ctx context.Context, a *acquisition, run *Run, es *externalservice.ExternalService) error {
	n := min(max(run.Concurrency, MinConcurrency), MaxConcurrency)
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	stop, cancel := context.WithCancel(ctx)
	defer cancel()
	setErr := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		cancel()
	}
	for range n {
		wg.Go(func() {
			for stop.Err() == nil {
				more, err := r.one(stop, a, run.ID, es)
				if err != nil {
					setErr(err)
					return
				}
				if !more {
					return
				}
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if a.lost() || ctx.Err() != nil {
		return errStop
	}
	return nil
}

// one takes the next pending item (fenced: running, attempts+1, this epoch), attaches it, and records the result
// (fenced). more=false when nothing is left or a cancel was requested.
func (r *Runner) one(ctx context.Context, a *acquisition, runID string, es *externalservice.ExternalService) (more bool, err error) {
	var it *Item
	cancelled := false
	err = a.checkpoint(ctx, func(tc context.Context) error {
		run, err := r.Repo.LoadRun(tc, runID)
		if err != nil {
			return err
		}
		if run == nil || run.CancelRequested {
			cancelled = true
			return nil
		}
		next, err := r.Repo.NextPending(tc, runID)
		if err != nil || next == nil {
			return err
		}
		next.State, next.Attempts, next.Epoch, next.Code = ItemRunning, next.Attempts+1, a.j.Epoch, ""
		if err := r.Repo.SaveItem(tc, next); err != nil {
			return err
		}
		it = next
		return nil
	})
	if err != nil || cancelled || it == nil {
		return false, err
	}
	attemptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.AttemptTimeout)
	outcome, aerr := r.Attach(attemptCtx, it.ProjectID, es)
	timedOut := attemptCtx.Err() != nil
	cancel()
	state, code := classify(outcome, aerr, timedOut)
	if aerr != nil {
		// Logged with ids only; the provider's text never reaches the job or the item.
		r.Log.Warn("provider attach: project failed", "run", runID, "project", it.ProjectID, "service", es.ID, "code", code)
	}
	err = a.checkpoint(ctx, func(tc context.Context) error {
		it.State, it.Code = state, code
		return r.Repo.SaveItem(tc, it)
	})
	return err == nil, err
}

// classify maps an attach outcome or error to the item's state and fixed code.
func classify(outcome string, err error, timedOut bool) (string, string) {
	if err != nil {
		if timedOut || errors.Is(err, context.DeadlineExceeded) {
			return ItemFailed, CodeTimeout
		}
		return ItemFailed, CodeProviderError
	}
	switch outcome {
	case OutcomeAttached:
		return ItemAttached, CodeAttached
	case OutcomeAlreadyAttached:
		return ItemSkipped, CodeAlreadyAttached
	case OutcomeDeleting:
		return ItemSkipped, CodeDeleting
	case OutcomeNotFound:
		return ItemSkipped, CodeNotFound
	}
	return ItemFailed, CodeProviderError
}

// cancelRest marks the pending items cancelled and ends the run cancelled.
func (r *Runner) cancelRest(ctx context.Context, a *acquisition, run *Run) error {
	if err := a.checkpoint(ctx, func(tc context.Context) error {
		_, err := r.Repo.SetItemsState(tc, run.ID, ItemPending, ItemCancelled, CodeCancelled)
		return err
	}); err != nil {
		return err
	}
	return r.complete(ctx, a, run, RunCancelled, "")
}

// abort ends a run whose provider is gone or disabled: every item not done yet fails with the code, so a retry of the
// failed projects picks them up once the provider is back.
func (r *Runner) abort(ctx context.Context, a *acquisition, run *Run, code string) error {
	if err := a.checkpoint(ctx, func(tc context.Context) error {
		if _, err := r.Repo.SetItemsState(tc, run.ID, ItemPending, ItemFailed, code); err != nil {
			return err
		}
		_, err := r.Repo.SetItemsState(tc, run.ID, ItemRunning, ItemFailed, code)
		return err
	}); err != nil {
		return err
	}
	return r.complete(ctx, a, run, RunFailed, code)
}

// complete freezes the counts on the run, sets its final status and acknowledges the job, in one fenced transaction.
func (r *Runner) complete(ctx context.Context, a *acquisition, run *Run, status, code string) error {
	if a.lost() {
		return errStop
	}
	err := r.Store.Complete(context.WithoutCancel(ctx), a.j, func(tc context.Context) error {
		if run == nil {
			return nil
		}
		cur, err := r.Repo.LoadRunForUpdate(tc, run.ID)
		if err != nil || cur == nil {
			return err
		}
		if !Terminal(cur.Status) {
			counts, err := r.Repo.Counts(tc, cur.ID)
			if err != nil {
				return err
			}
			now := time.Now().UTC()
			cur.Status, cur.Code, cur.Counts, cur.FinishedAt = status, code, counts, &now
		}
		return r.Repo.SaveRun(tc, cur)
	})
	if errors.Is(err, durablejobs.ErrLeaseLost) {
		return errStop
	}
	if err == nil {
		r.Log.Info("provider attach finished", "job", a.j.ID, "run", a.runID, "status", status)
	}
	return err
}

// heartbeat renews the lease every third of it until work ends; a refused renewal cancels work.
func (r *Runner) heartbeat(work context.Context, cancel context.CancelFunc, j *durablejobs.Job) (isLost func() bool, stop func()) {
	var mu sync.Mutex
	lost := false
	done := make(chan struct{})
	go func() {
		defer close(done)
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
	return func() bool { mu.Lock(); defer mu.Unlock(); return lost }, func() { cancel(); <-done }
}

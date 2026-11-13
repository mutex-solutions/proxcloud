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

// AttachFunc attaches one project to the provider through the per-project attach path (project.Handler.AttachByOperator):
// the outcome, or the provisioning error (never stored, only classified).
type AttachFunc func(ctx context.Context, projectID string, es *externalservice.ExternalService) (string, error)

// ProviderFunc loads the provider (decrypted); nil when it no longer exists.
type ProviderFunc func(ctx context.Context, id string) (*externalservice.ExternalService, error)

// Runner claims ProviderAttach jobs, one at a time, renews the lease while it works and fences every write.
type Runner struct {
	Store    JobStore
	Repo     Repo
	Provider ProviderFunc
	Attach   AttachFunc
	// CanAttach refuses a provider this API cannot attach projects to (project.Handler.CanAttach; nil = no check).
	CanAttach func(*externalservice.ExternalService) error
	Lease     time.Duration
	Poll      time.Duration
	Log       *slog.Logger
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

// RunOnce claims and runs at most one job; did reports whether one was claimed. With nothing to claim it prunes the
// items of runs finished more than ItemRetention ago (a few runs per pass).
func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	r.defaults()
	j, err := r.Store.ClaimKinds(ctx, r.Lease, []string{durablejobs.ProviderAttach})
	if err != nil {
		return false, err
	}
	if j == nil {
		return false, r.prune(ctx)
	}
	r.perform(ctx, j)
	return true, nil
}

// prune removes the items of runs finished more than ItemRetention ago; the runs keep their frozen counts.
func (r *Runner) prune(ctx context.Context) error {
	runs, err := r.Repo.FinishedRunsToPrune(ctx, time.Now().UTC().Add(-ItemRetention), 5)
	if err != nil {
		return err
	}
	for i := range runs {
		if !Terminal(runs[i].Status) {
			continue
		}
		if err := r.Repo.PruneItems(ctx, runs[i].ID); err != nil {
			return err
		}
	}
	return nil
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

// execute plans the run (in batches, resumable), then attaches its pending items with bounded concurrency, then
// finishes it.
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
	if es, stop, err := r.provider(ctx, a, run); err != nil || stop || es == nil {
		return err
	}
	if err := r.start(ctx, a, run); err != nil {
		return err
	}
	for !run.Planned {
		if err := r.planBatch(ctx, a, run, p.ServiceID); err != nil {
			return err
		}
	}
	// Items a dead acquisition left running: attach them again (the attach is a no-op where it had landed), unless they
	// already took their attempts.
	if err := r.requeueRunning(ctx, a, run.ID); err != nil {
		return err
	}
	for {
		ended, err := r.workers(ctx, a, run)
		if err != nil {
			return err
		}
		if !ended {
			break
		}
		// The provider was deleted, disabled or became unattachable mid-run: end the run on it (abort), unless it is
		// usable again by now, then carry on.
		if _, stop, err := r.provider(ctx, a, run); err != nil || stop {
			return err
		}
	}
	fresh, err := r.Repo.LoadRun(ctx, run.ID)
	if err != nil {
		return err
	}
	counts, err := r.Repo.Counts(ctx, run.ID)
	if err != nil {
		return err
	}
	// A cancel that arrived once every project was done changes nothing: the run completed.
	if fresh != nil && fresh.CancelRequested && counts.Pending > 0 {
		return r.cancelRest(ctx, a, fresh)
	}
	status := RunSucceeded
	if counts.Failed > 0 {
		status = RunCompletedWithFailures
	}
	return r.complete(ctx, a, fresh, status, "")
}

// provider loads the run's provider; a provider gone, disabled or no longer attachable ends the run (stop=true).
func (r *Runner) provider(ctx context.Context, a *acquisition, run *Run) (es *externalservice.ExternalService, stop bool, err error) {
	es, err = r.Provider(ctx, run.ServiceID)
	if err != nil {
		return nil, false, err
	}
	if code := r.unusable(es); code != "" {
		return nil, true, r.abort(ctx, a, run, code)
	}
	return es, false, nil
}

// unusable is the code that stops a run on this provider ("" = usable).
func (r *Runner) unusable(es *externalservice.ExternalService) string {
	switch {
	case es == nil:
		return CodeProviderMissing
	case es.IsDisabled():
		return CodeProviderDisabled
	case r.CanAttach != nil && r.CanAttach(es) != nil:
		return CodeProviderUnsupport
	}
	return ""
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

// planBatch lists the next PlanBatch scoped projects (in id order, after Run.PlanCursor) and records them in one fenced
// transaction with the advanced cursor: a run of any size is planned in short steps and a new acquisition resumes at
// the cursor. A project already attached gets no item, only a count (Run.PlanSkipped); one on its way out or gone is an
// item skipped with its code; the rest are pending.
func (r *Runner) planBatch(ctx context.Context, a *acquisition, run *Run, serviceID string) error {
	ids, err := r.scopeIDs(ctx, run, PlanBatch)
	if err != nil {
		return err
	}
	projects, err := r.Repo.ProjectsByID(ctx, ids)
	if err != nil {
		return err
	}
	var items []Item
	skipped := map[string]int64{}
	now := time.Now().UTC()
	for _, id := range ids {
		p, found := projects[id]
		if found && p.HasService(serviceID) {
			skipped[CodeAlreadyAttached]++
			continue
		}
		it := Item{ID: ItemID(run.ID, id), RunID: run.ID, ProjectID: id, ProjectName: p.Name, OrganizationID: p.OrganizationID,
			State: ItemPending, UpdatedAt: now}
		switch {
		case !found:
			it.State, it.Code = ItemSkipped, CodeNotFound
		case p.Deleting():
			it.State, it.Code = ItemSkipped, CodeDeleting
		}
		items = append(items, it)
	}
	done := len(ids) < PlanBatch
	cursor := run.PlanCursor
	if len(ids) > 0 {
		cursor = ids[len(ids)-1]
	}
	return a.checkpoint(ctx, func(tc context.Context) error {
		cur, err := r.Repo.LoadRunForUpdate(tc, run.ID)
		if err != nil || cur == nil {
			return err
		}
		if cur.Planned || cur.PlanCursor != run.PlanCursor {
			*run = *cur // another acquisition planned this batch
			return nil
		}
		if err := r.Repo.InsertItems(tc, items); err != nil {
			return err
		}
		for code, n := range skipped {
			if cur.PlanSkipped == nil {
				cur.PlanSkipped = map[string]int64{}
			}
			cur.PlanSkipped[code] += n
		}
		cur.PlanCursor, cur.Planned = cursor, done
		*run = *cur
		return r.Repo.SaveRun(tc, cur)
	})
}

// scopeIDs is the next batch of project ids of the run's scope after its cursor (sorted).
func (r *Runner) scopeIDs(ctx context.Context, run *Run, limit int) ([]string, error) {
	after := run.PlanCursor
	pick := func(all []string) []string {
		var out []string
		for _, id := range all { // all is sorted
			if id > after {
				out = append(out, id)
				if len(out) == limit {
					break
				}
			}
		}
		return out
	}
	switch run.Scope.Mode {
	case ScopeProjects:
		return pick(run.Scope.ProjectIDs), nil
	case ScopeRetry:
		src, err := r.Repo.LoadRun(ctx, run.Scope.SourceRunID)
		if err != nil || src == nil {
			return nil, err
		}
		states := []string{ItemFailed}
		if !Terminal(src.Status) {
			states = append(states, ItemPending, ItemRunning) // the source run died before finishing them
		}
		return r.Repo.ItemProjectIDs(ctx, src.ID, states, after, limit)
	default:
		return r.Repo.ScopedProjectIDs(ctx, run.Scope, after, limit)
	}
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

// workers attaches the pending items with run.Concurrency goroutines. A worker stops on a cancel request, a lost lease,
// a provider that became unusable (ended=true: the caller decides) or a database error (the first error wins).
func (r *Runner) workers(ctx context.Context, a *acquisition, run *Run) (ended bool, err error) {
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
	end := func() {
		mu.Lock()
		ended = true
		mu.Unlock()
		cancel()
	}
	for range n {
		wg.Go(func() {
			for stop.Err() == nil {
				more, code, err := r.one(stop, a, run.ID)
				if err != nil {
					setErr(err)
					return
				}
				if code != "" {
					end()
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
		return false, firstErr
	}
	if a.lost() || ctx.Err() != nil {
		return false, errStop
	}
	return ended, nil
}

// one takes the next pending item (fenced: running, attempts+1, this epoch), re-reads the provider (a disable or a
// credential rotation mid-run applies to the next project), attaches it, and records the result (fenced).
// more=false when nothing is left or a cancel was requested; code is set when the provider became unusable (the item
// goes back to pending).
func (r *Runner) one(ctx context.Context, a *acquisition, runID string) (more bool, code string, err error) {
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
		return false, "", err
	}
	// The item is taken: finish it even when the other workers are stopping (their stop must not read as an error here).
	read, done := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer done()
	run, err := r.Repo.LoadRun(read, runID)
	if err != nil || run == nil {
		return false, "", err
	}
	es, err := r.Provider(read, run.ServiceID)
	if err != nil {
		return false, "", err
	}
	if code := r.unusable(es); code != "" {
		// Not this project's fault: put it back; abort marks every unfinished item with the code.
		err = a.checkpoint(ctx, func(tc context.Context) error {
			it.State, it.Attempts = ItemPending, it.Attempts-1
			return r.Repo.SaveItem(tc, it)
		})
		return false, code, err
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
	return err == nil, "", err
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
			cur.Status, cur.Code, cur.Counts, cur.FinishedAt = status, code, cur.WithPlanSkipped(counts), &now
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

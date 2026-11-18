package platformlife

// runner.go — the runner of platform runs (design §4.1, §4.5). It claims PlatformRun jobs one at a time, renews a
// one-minute lease every 20 seconds and drives a run's stages in order. Every write goes through Checkpoint (intent
// before an external effect, result after it) and re-reads the run first, so a lost lease stops the run before its next
// external call and a cancel request is seen at every claim and at every write. A stage that has to wait, or meets a
// transient error, gives the job back with Defer: no attempt is spent, transient errors count in the stage's retries.
// Only a permanent error, a spent stage budget or a passed deadline fails the job. Messages are fixed text plus
// clipped Proxmox or Kubernetes error text, with every secret the run holds replaced; logs carry ids and codes only.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
)

// JobStore is the part of *durablejobs.Store the runner uses.
type JobStore interface {
	ClaimKinds(ctx context.Context, ttl time.Duration, kinds []string) (*durablejobs.Job, error)
	Renew(ctx context.Context, j *durablejobs.Job, ttl time.Duration) error
	Checkpoint(ctx context.Context, j *durablejobs.Job, fn func(context.Context) error) error
	Complete(ctx context.Context, j *durablejobs.Job, fn func(context.Context) error) error
	Fail(ctx context.Context, j *durablejobs.Job, delay time.Duration, permanent bool, code string) error
	Defer(ctx context.Context, j *durablejobs.Job, delay time.Duration, code string) error
}

// RunState is the part of *Store the runner uses.
type RunState interface {
	Load(ctx context.Context, runID string) (*Run, *Installation, *Secrets, error)
	Mutate(ctx context.Context, runID string, fn func(*Run, *Installation, *Secrets) error) error
}

// Imports is what stage 2 uses of Template updates (TemplateUpdates in production).
type Imports interface {
	ByKey(ctx context.Context, serviceID, node, digest string) ([]templateupdates.Receipt, error)
	JobState(ctx context.Context, id string) (string, error)
	EnqueueVM(ctx context.Context, req templateupdates.VMRequest, actor string) (*templateupdates.Receipt, error)
	MarkTemplateGone(ctx context.Context, pvFor templateupdates.PVEResolver, id string) error
}

// TemplateUpdates gives a run the imports of *templateupdates.Service.
type TemplateUpdates struct{ *templateupdates.Service }

// ByKey lists the import receipts of one artifact on one node.
func (t TemplateUpdates) ByKey(ctx context.Context, serviceID, node, digest string) ([]templateupdates.Receipt, error) {
	return t.Receipts.ByKey(ctx, serviceID, node, digest)
}

const (
	stageRetries = 40
	waitDelay    = 15 * time.Second
	maxDelay     = 5 * time.Minute
	taskBudget   = 2 * time.Minute // how long one acquisition follows a Proxmox task before it waits through Defer
)

// Runner claims PlatformRun jobs. It runs only with PROXCLOUD_PLATFORM_ENABLED and PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED.
type Runner struct {
	Jobs  JobStore
	State RunState
	// Provider loads the installation's Proxmox provider, decrypted (nil when it no longer exists).
	Provider func(ctx context.Context, id string) (*externalservice.ExternalService, error)
	// Preflight runs the read-only checks of POST …/preflight on an installation (stage 1).
	Preflight func(ctx context.Context, in *Installation) ([]Check, error)
	Imports   Imports
	// OnImport records the audit entry of an image import the run enqueued (the admin handler's entry).
	OnImport func(rec *templateupdates.Receipt, actor string)
	// Register creates or adopts one provider row of the installation from a stage 10 body and answers its id
	// (ErrProviderConflict when a row that is not the installation's is in the way). Disable sets the installation's
	// provider rows DISABLED (Remove). The admin handler implements both.
	Register func(ctx context.Context, body map[string]any) (string, error)
	Disable  func(ctx context.Context, installationID string) error
	// SDNLock takes the site's SDN apply lock (the one a tenant network change takes) for the routed platform network;
	// acquired=false when another change holds it. Nil in tests: no lock.
	SDNLock func(ctx context.Context) (release func(), acquired bool, err error)
	Log     *slog.Logger
	// SkipRolloutGate turns off the check an upgrade run makes before it lets the cluster chart of the target BOM sync
	// (rolloutGate; PROXCLOUD_PLATFORM_SKIP_ROLLOUT_GATE). The check reads ArgoCD's comparison and has not run on a
	// lab: the switch is for a site where ArgoCD reports it otherwise than the check expects.
	SkipRolloutGate bool
	Poll            time.Duration
	Lease           time.Duration // one minute when zero, renewed every third
	SeedPort        string        // the seed API's port, 6443 when empty
}

// Run polls until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	r.defaults()
	for ctx.Err() == nil {
		did, err := r.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			r.Log.Warn("platform run: claim", "err", err)
		}
		if did {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(r.Poll):
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
	if r.SeedPort == "" {
		r.SeedPort = "6443"
	}
}

// RunOnce claims and runs at most one job; did reports whether one was claimed.
func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	r.defaults()
	j, err := r.Jobs.ClaimKinds(ctx, r.Lease, []string{durablejobs.PlatformRun})
	if err != nil || j == nil {
		return false, err
	}
	r.perform(ctx, j)
	return true, nil
}

// end is how an acquisition gives its job back.
type end struct {
	kind  int
	delay time.Duration
	code  string
}

const (
	endComplete = iota
	endDefer
	endFail  // permanent
	endRetry // a database error: the job is retried and spends an attempt
	endStop  // the lease is lost or the process stops: the next acquisition resumes (at once after a clean stop)
)

func (r *Runner) perform(parent context.Context, j *durablejobs.Job) {
	work, cancel := context.WithCancel(parent)
	defer cancel()
	lost, stopHB := r.heartbeat(work, cancel, j)
	defer stopHB()
	x := &acq{r: r, j: j, lost: lost}
	e := x.execute(work)
	if e.kind == endStop || lost() || parent.Err() != nil {
		// A process that stops while it still holds the lease gives the job back at once, without an attempt: no
		// call of this acquisition is in flight any more (execute has returned), and Defer is fenced by owner, epoch
		// and lease like every other end, so it is refused when another runner holds the job. Without it the next
		// acquisition waits out the lease. A lost lease is never given back.
		released := false
		if parent.Err() != nil && !lost() {
			ctx, done := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
			released = r.Jobs.Defer(ctx, j, 0, "shutdown") == nil
			done()
		}
		r.Log.Warn("platform run: stopped; the next acquisition resumes", "job", j.ID, "run", x.runID, "released", released)
		return
	}
	ctx, done := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer done()
	var err error
	switch e.kind {
	case endComplete:
		err = r.Jobs.Complete(ctx, j, nil)
	case endDefer:
		err = r.Jobs.Defer(ctx, j, e.delay, e.code)
	case endFail:
		err = r.Jobs.Fail(ctx, j, 0, true, e.code)
	default:
		err = r.Jobs.Fail(ctx, j, durablejobs.Backoff(j.Attempts), false, "retryable")
	}
	if err != nil && !errors.Is(err, durablejobs.ErrLeaseLost) {
		r.Log.Warn("platform run: could not give the job back", "job", j.ID, "run", x.runID)
	}
}

// heartbeat renews the lease every third of it until work ends. A refused renewal cancels work, and so does a lease
// that no renewal reached in time (the database away, the process paused): another runner may hold the job by then.
func (r *Runner) heartbeat(work context.Context, cancel context.CancelFunc, j *durablejobs.Job) (isLost func() bool, stop func()) {
	var mu sync.Mutex
	lost := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(r.Lease / 3)
		defer t.Stop()
		renewed := time.Now()
		for {
			select {
			case <-work.Done():
				return
			case <-t.C:
				rc, rcancel := context.WithTimeout(work, r.Lease/3)
				err := r.Jobs.Renew(rc, j, r.Lease)
				rcancel()
				if err == nil {
					renewed = time.Now()
					continue
				}
				if errors.Is(err, durablejobs.ErrLeaseLost) || time.Since(renewed) >= r.Lease {
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

// acq is one acquisition of one run.
type acq struct {
	r            *Runner
	j            *durablejobs.Job
	lost         func() bool
	runID        string
	run          *Run
	in           *Installation
	sec          *Secrets
	es           *externalservice.ExternalService
	hidden       []string // secrets this acquisition read from a cluster: no message may carry them
	bom          *BOM
	from         *BOM // on an upgrade run, the BOM the platform is moved from
	portal, capi *pve.Client
	si           int // the stage being run
}

var (
	errStop      = errors.New("platform run: stopped")
	errCancelled = errors.New("platform run: cancelled")
	errSkipped   = errors.New("platform run: stage skipped")
)

// stageErr ends a stage's acquisition early: wait (re-observe later), retry (transient, counted) or fail (permanent).
// code is what the stage fails with if this is how its deadline or budget ends.
type stageErr struct {
	kind      int
	code, msg string
}

const (
	kindWait = iota
	kindRetry
	kindRefused // a Kubernetes 401 or 403: counted like a retry, final at the fifth look on end (startingUp)
	kindFail
)

func (e *stageErr) Error() string { return e.code + ": " + e.msg }

func waiting(code, format string, a ...any) error {
	return &stageErr{kindWait, code, fmt.Sprintf(format, a...)}
}
func permanent(code, format string, a ...any) error {
	return &stageErr{kindFail, code, fmt.Sprintf(format, a...)}
}
func transient(code string, err error) error { return &stageErr{kindRetry, code, clip(err.Error())} }

// classify makes a Proxmox or Kubernetes error a stage error: a refusal is permanent (a Proxmox 4xx but 429; a
// Kubernetes 4xx but 409 and 429, see refused), anything else (unreachable, 5xx, a clone lock, which Proxmox answers
// as a 500) transient. A Kubernetes 401 or 403 is what an API server also answers while it starts: tried again, and
// a refusal only when it stays (startingUp). Stage errors, a stop and a cancel pass through.
func classify(code string, err error) error {
	var se *stageErr
	if err == nil || errors.As(err, &se) || errors.Is(err, errStop) || errors.Is(err, errCancelled) {
		return err
	}
	if startingUp(err) {
		return &stageErr{kindRefused, code, clip(err.Error())}
	}
	var pa *pve.APIError
	if errors.As(err, &pa) && pa.Status >= 400 && pa.Status < 500 && pa.Status != 429 || refused(err) {
		return &stageErr{kindFail, code, clip(err.Error())}
	}
	return transient(code, err)
}

func clip(s string) string {
	if r := []rune(s); len(r) > 400 {
		return string(r[:400]) + "…"
	}
	return s
}

func (x *acq) execute(ctx context.Context) end {
	p, err := DecodePayload(x.j.Payload)
	if err != nil {
		return end{kind: endFail, code: "invalid_payload"}
	}
	x.runID = p.RunID
	run, in, sec, err := x.r.State.Load(ctx, p.RunID)
	if err != nil {
		x.r.Log.Warn("platform run: load failed", "run", p.RunID)
		return end{kind: endRetry}
	}
	if run == nil || in == nil || run.InstallationID != p.InstallationID || Terminal(run.State) {
		return end{kind: endComplete}
	}
	x.run, x.in, x.sec = run, in, sec
	// The stage the run stands at, for an end before the stages run (an upgrade, a provider that is gone): finish
	// marks that stage failed and the log names it, not the first.
	if x.si = slices.IndexFunc(run.Stages, func(s Stage) bool { return s.State != StageDone && s.State != StageSkipped }); x.si < 0 {
		x.si = len(run.Stages)
	}
	if run.CancelRequested {
		return x.finish(ctx, RunCancelled, "", "")
	}
	if run.Kind == RunApply {
		b, ok := Lookup(run.BOM.SHA256)
		if !ok {
			return x.finish(ctx, RunFailed, CodePortalChanged, "ProxCloud was upgraded during the run and no longer embeds its BOM "+
				run.BOM.Release+": Retry starts a new run.")
		}
		x.bom = b.BOM
		if run.FromBOM != nil {
			f, ok := Lookup(run.FromBOM.SHA256)
			if !ok {
				return x.finish(ctx, RunFailed, CodePortalChanged, "ProxCloud was upgraded during the run and no longer embeds the BOM "+
					run.FromBOM.Release+" the platform is moved from: Retry starts a new run.")
			}
			x.from = f.BOM
		}
	}
	es, err := x.r.Provider(ctx, in.ServiceID)
	if err != nil {
		return end{kind: endRetry}
	}
	if es == nil || !es.IsProxmox() {
		return x.finish(ctx, RunFailed, CodePreflightFailed, "The installation's Proxmox provider no longer exists.")
	}
	x.es = es
	if x.portal, err = pve.New(es.PVEConfig()); err == nil {
		x.capi, err = TokenClient(es, sec.CAPI)
	}
	if err != nil {
		return x.finish(ctx, RunFailed, CodePreflightFailed, "The Proxmox connection is not valid: "+clip(err.Error()))
	}
	if err := x.save(ctx, func(run *Run, _ *Installation, _ *Secrets) {
		now := time.Now().UTC()
		if run.StartedAt == nil {
			run.StartedAt = &now
		}
		run.State = RunRunning
	}); err != nil {
		return x.ended(ctx, err)
	}
	for x.si = range x.run.Stages {
		if e, done := x.stage(ctx); done {
			return e
		}
	}
	return x.finish(ctx, RunSucceeded, "", "")
}

type stageDef struct {
	deadline time.Duration
	run      func(*acq, context.Context) error
}

// stageDefs are the stages with their deadlines (design §4.2: stage 2's is a stall timer the stage renews whenever an
// import moves, stage 9's starts again at every wave; a remove waits up to 45 minutes for the machines to go).
var stageDefs = map[string]stageDef{
	StageValidate:         {5 * time.Minute, (*acq).validate},
	StageImages:           {60 * time.Minute, (*acq).images},
	StageSeedVM:           {30 * time.Minute, (*acq).seedVM},
	StageSeedAPI:          {20 * time.Minute, (*acq).seedAPI},
	StageSeedIdentity:     {10 * time.Minute, (*acq).seedIdentity},
	stageSeedPlatform:     {30 * time.Minute, (*acq).seedPlatform},
	stageCluster:          {60 * time.Minute, (*acq).cluster},
	stageWorkloadAccess:   {10 * time.Minute, (*acq).workloadAccess},
	stageWorkloadPlatform: {waveStall, (*acq).workloadPlatform},
	stageRegister:         {5 * time.Minute, (*acq).register},
	stageDone:             {5 * time.Minute, (*acq).done},
	StageRemoveCluster:    {45 * time.Minute, (*acq).removeCluster},
	StageRemoveSeed:       {30 * time.Minute, (*acq).removeSeed},
	StageRemoveSecrets:    {5 * time.Minute, (*acq).removeSecrets},
}

// stage runs the current stage once; done=true when the acquisition ends with it.
func (x *acq) stage(ctx context.Context) (end, bool) {
	s := x.run.Stages[x.si]
	if s.State == StageDone || s.State == StageSkipped {
		return end{}, false
	}
	now := time.Now().UTC()
	def, ok := stageDefs[s.ID]
	if !ok {
		if err := x.saveStage(ctx, func(s *Stage) { s.State, s.StartedAt = StageRunning, &now }); err != nil {
			return x.ended(ctx, err), true
		}
		return x.finish(ctx, RunFailed, CodeStageUnavailable, "This build has no stage "+s.ID+": the run was made by another release."), true
	}
	if s.StartedAt == nil {
		if err := x.saveStage(ctx, func(s *Stage) {
			d := now.Add(def.deadline)
			s.State, s.StartedAt, s.LastProgressAt, s.Deadline = StageRunning, &now, &now, &d
		}); err != nil {
			return x.ended(ctx, err), true
		}
	} else if s.Deadline != nil && now.After(*s.Deadline) {
		code := s.Code
		if code == "" {
			code = TimeoutCode(s.ID)
		}
		return x.finish(ctx, RunFailed, code, "The stage did not finish before its deadline. "+s.Message), true
	}
	err := def.run(x, ctx)
	state := StageDone
	if errors.Is(err, errSkipped) {
		state, err = StageSkipped, nil
	}
	if err == nil {
		err = x.saveStage(ctx, func(s *Stage) {
			fin := time.Now().UTC()
			s.State, s.FinishedAt, s.Code, s.Message, s.Refused = state, &fin, "", "", 0
		})
	}
	if err != nil {
		return x.ended(ctx, err), true
	}
	return end{}, false
}

// ended maps how a stage stopped to how the acquisition ends.
func (x *acq) ended(ctx context.Context, err error) end {
	var se *stageErr
	switch {
	case errors.Is(err, errStop) || x.lost() || ctx.Err() != nil:
		return end{kind: endStop}
	case errors.Is(err, errCancelled):
		return x.finish(ctx, RunCancelled, "", "")
	case !errors.As(err, &se):
		x.r.Log.Warn("platform run: attempt failed", "run", x.runID, "err", x.redact(err.Error()))
		return end{kind: endRetry}
	case se.kind == kindFail:
		return x.finish(ctx, RunFailed, se.code, se.msg)
	}
	s := x.run.Stages[x.si]
	msg, refused := se.msg, 0
	if se.kind == kindRefused {
		if s.Refused >= refusedLooks-1 {
			return x.finish(ctx, RunFailed, se.code, fmt.Sprintf("The cluster's API refused the run's credential at %d looks on end: %s", refusedLooks, se.msg))
		}
		msg, refused = startingUpNote+se.msg, s.Refused+1
	}
	retry := se.kind != kindWait
	if retry && s.Retries >= stageRetries {
		return x.finish(ctx, RunFailed, se.code, fmt.Sprintf("Gave up after %d retries: %s", s.Retries, msg))
	}
	delay, prefix := waitDelay, "waiting_"
	if retry {
		delay, prefix = min(waitDelay<<min(s.Retries, 5), maxDelay), "retry_"
	}
	if err := x.save(ctx, func(run *Run, _ *Installation, _ *Secrets) {
		st := &run.Stages[x.si]
		if retry {
			st.Retries++
		}
		st.State, st.Code, st.Message, st.Refused = StageWaiting, se.code, msg, refused
		run.State = RunWaiting
	}); err != nil {
		return x.ended(ctx, err)
	}
	x.r.Log.Info("platform run: waiting", "run", x.runID, "stage", s.ID, "code", se.code)
	return end{kind: endDefer, delay: delay, code: prefix + strings.ReplaceAll(s.ID, "-", "_")}
}

// finish ends the run and sets the installation's phase: ready or removed when it succeeded, failed otherwise (a
// cancelled run included).
func (x *acq) finish(ctx context.Context, state, code, msg string) end {
	now := time.Now().UTC()
	err := x.record(ctx, func(run *Run, in *Installation, _ *Secrets) {
		run.State, run.Code, run.Message, run.FinishedAt = state, code, msg, &now
		if state == RunFailed && x.si < len(run.Stages) {
			if st := &run.Stages[x.si]; st.StartedAt != nil && st.State != StageDone && st.State != StageSkipped {
				st.State, st.Code, st.Message, st.FinishedAt = StageFailed, code, msg, &now
			}
		}
		switch {
		case state == RunSucceeded && run.Kind == RunRemove:
			in.Status.Phase = PhaseRemoved
		case state == RunSucceeded:
			in.Status.Phase = PhaseReady
		default:
			in.Status.Phase = PhaseFailed
		}
	})
	if err != nil {
		if errors.Is(err, errStop) {
			return end{kind: endStop}
		}
		return end{kind: endRetry}
	}
	if state == RunFailed {
		stage := "" // a run can fail before its first stage starts
		if x.si < len(x.run.Stages) && x.run.Stages[x.si].StartedAt != nil {
			stage = x.run.Stages[x.si].ID
		}
		x.r.Log.Error("platform run failed", "run", x.runID, "stage", stage, "code", code, "message", x.redact(msg))
	} else {
		x.r.Log.Info("platform run finished", "run", x.runID, "state", state, "code", code)
	}
	if state == RunFailed {
		return end{kind: endFail, code: code}
	}
	return end{kind: endComplete}
}

// save commits fn's changes to the run, its installation and secrets, fenced by the lease: errStop when the lease is
// lost, errCancelled (nothing written) when a cancel was requested.
func (x *acq) save(ctx context.Context, fn func(*Run, *Installation, *Secrets)) error {
	return x.write(ctx, true, fn)
}

// record is save for the result of an external effect, written even when a cancel was requested meanwhile.
func (x *acq) record(ctx context.Context, fn func(*Run, *Installation, *Secrets)) error {
	return x.write(ctx, false, fn)
}

func (x *acq) write(ctx context.Context, honourCancel bool, fn func(*Run, *Installation, *Secrets)) error {
	if x.lost() {
		return errStop
	}
	err := x.r.Jobs.Checkpoint(context.WithoutCancel(ctx), x.j, func(tc context.Context) error {
		return x.r.State.Mutate(tc, x.runID, func(run *Run, in *Installation, sec *Secrets) error {
			if run.CancelRequested && honourCancel {
				return errCancelled
			}
			fn(run, in, sec)
			x.run, x.in, x.sec = run, in, sec
			x.scrub(run)
			return nil
		})
	})
	if errors.Is(err, durablejobs.ErrLeaseLost) {
		return errStop
	}
	return err
}

// check is a write of nothing: it stops on a lost lease or a cancel before the next external call.
func (x *acq) check(ctx context.Context) error {
	return x.save(ctx, func(*Run, *Installation, *Secrets) {})
}

func (x *acq) saveStage(ctx context.Context, fn func(*Stage)) error {
	return x.save(ctx, func(run *Run, _ *Installation, _ *Secrets) { fn(&run.Stages[x.si]) })
}

func (x *acq) saveReceipt(ctx context.Context, fn func(*StageReceipt, *Installation, *Secrets)) error {
	return x.write(ctx, true, x.inReceipt(fn))
}

func (x *acq) recordReceipt(ctx context.Context, fn func(*StageReceipt, *Installation, *Secrets)) error {
	return x.write(ctx, false, x.inReceipt(fn))
}

func (x *acq) inReceipt(fn func(*StageReceipt, *Installation, *Secrets)) func(*Run, *Installation, *Secrets) {
	return func(run *Run, in *Installation, sec *Secrets) {
		s := &run.Stages[x.si]
		if s.Receipt == nil {
			s.Receipt = &StageReceipt{}
		}
		fn(s.Receipt, in, sec)
	}
}

// receipt is a copy of the current stage's receipt.
func (x *acq) receipt() StageReceipt {
	if r := x.run.Stages[x.si].Receipt; r != nil {
		return *r
	}
	return StageReceipt{}
}

// secrets are the values no message may carry.
func (x *acq) secrets() []string {
	var out []string
	add := func(v string) {
		if len(v) >= 6 {
			out = append(out, v)
		}
	}
	sec := x.sec
	add(sec.K3sToken)
	add(sec.BootstrapToken)
	if _, s, ok := strings.Cut(sec.BootstrapToken, "."); ok {
		add(s)
	}
	if sec.SeedKubeconfig != nil {
		add(sec.SeedKubeconfig.Token)
	}
	for _, t := range []*TokenCredential{sec.CAPI, sec.CSI} {
		if t != nil {
			add(t.Secret)
		}
	}
	return append(out, x.hidden...)
}

func (x *acq) redact(s string) string {
	for _, v := range x.secrets() {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	return s
}

// scrub redacts every message of run.
func (x *acq) scrub(run *Run) {
	run.Message = x.redact(run.Message)
	for i := range run.Stages {
		s := &run.Stages[i]
		s.Message, s.Progress = x.redact(s.Message), x.redact(s.Progress)
		for j := range s.Items {
			s.Items[j].Message = x.redact(s.Items[j].Message)
		}
	}
}

func (r *Runner) seedServer(address string) string {
	return "https://" + net.JoinHostPort(address, r.SeedPort)
}

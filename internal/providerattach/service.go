package providerattach

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Service is the admin-facing side: start, cancel, retry and read runs.
type Service struct {
	DB   *pgdoc.DB
	Jobs *durablejobs.Store
	Repo Repo
}

// New builds the service on the primary.
func New(db *pgdoc.DB, jobs *durablejobs.Store) *Service {
	return &Service{DB: db, Jobs: jobs, Repo: Repo{DB: db}}
}

// Request errors (the admin handler maps them to 400 / 404 / 409).
var (
	ErrActive    = errors.New("a bulk attach of this provider is already queued or running; cancel it or wait for it to finish")
	ErrNotFound  = errors.New("bulk attach run not found")
	ErrFinished  = errors.New("this bulk attach has already finished")
	ErrNoFailed  = errors.New("this bulk attach has no failed projects to retry")
	ErrNotDone   = errors.New("this bulk attach is still running; retry its failed projects once it has finished")
	ErrBadScope  = errors.New("invalid scope")
	ErrDisabled  = errors.New("the provider is DISABLED: enable it before attaching projects to it")
	ErrBadParams = errors.New("invalid request")
)

// ScopeError is a 400 with the reason.
type ScopeError struct{ Msg string }

func (e *ScopeError) Error() string { return e.Msg }

// NormalizeScope validates a requested scope: a known mode (not retry), ids of the right shape, deduplicated, at most
// MaxScopeIDs, none for ScopeAll.
func NormalizeScope(s Scope) (Scope, error) {
	clean := func(ids []string, what string) ([]string, error) {
		if len(ids) == 0 {
			return nil, &ScopeError{"Pick at least one " + what}
		}
		if len(ids) > MaxScopeIDs {
			return nil, &ScopeError{fmt.Sprintf("At most %d %ss per run", MaxScopeIDs, what)}
		}
		out := make([]string, 0, len(ids))
		seen := map[string]bool{}
		for _, id := range ids {
			if !ValidID(id) {
				return nil, &ScopeError{"Invalid " + what + " id"}
			}
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		slices.Sort(out)
		return out, nil
	}
	var err error
	switch s.Mode {
	case ScopeAll:
		if len(s.OrganizationIDs) > 0 || len(s.ProjectIDs) > 0 {
			return s, &ScopeError{"Scope all takes no ids"}
		}
		return Scope{Mode: ScopeAll}, nil
	case ScopeOrganizations:
		if len(s.ProjectIDs) > 0 {
			return s, &ScopeError{"Scope organizations takes organizationIds only"}
		}
		s.OrganizationIDs, err = clean(s.OrganizationIDs, "organization")
		return Scope{Mode: ScopeOrganizations, OrganizationIDs: s.OrganizationIDs}, err
	case ScopeProjects:
		if len(s.OrganizationIDs) > 0 {
			return s, &ScopeError{"Scope projects takes projectIds only"}
		}
		s.ProjectIDs, err = clean(s.ProjectIDs, "project")
		return Scope{Mode: ScopeProjects, ProjectIDs: s.ProjectIDs}, err
	}
	return s, &ScopeError{"Scope mode must be all, organizations or projects"}
}

// NormalizeConcurrency: 0 = the default, else 1..MaxConcurrency.
func NormalizeConcurrency(n int) (int, error) {
	if n == 0 {
		return DefaultWorkers, nil
	}
	if n < MinConcurrency || n > MaxConcurrency {
		return 0, &ScopeError{fmt.Sprintf("Concurrency must be %d..%d", MinConcurrency, MaxConcurrency)}
	}
	return n, nil
}

// StartRequest is a new run.
type StartRequest struct {
	ServiceID   string
	ServiceName string
	Scope       Scope // normalized (or ScopeRetry, built by Retry)
	Concurrency int
	Actor       string
}

// lockKey serializes the starts of one provider's runs across API replicas.
func lockKey(serviceID string) string { return "providerattach-start:" + serviceID }

// Start records a run and enqueues its job in one transaction. At most one run per provider is active: the start holds a
// transaction advisory lock on the provider, refuses while an earlier run's job is pending or running (ErrActive), and
// keys the job <provider>#<sequence>, so even two starts that slipped past the lock would collide on the job's unique key
// instead of both running.
func (s *Service) Start(ctx context.Context, req StartRequest) (*Run, error) {
	if !ValidID(req.ServiceID) || req.Actor == "" || len(req.Actor) > 256 {
		return nil, ErrBadParams
	}
	conc, err := NormalizeConcurrency(req.Concurrency)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	run := &Run{ID: uuid.NewString(), ServiceID: req.ServiceID, ServiceName: req.ServiceName, Scope: req.Scope, Concurrency: conc,
		RequestedBy: req.Actor, Status: RunQueued, CreatedAt: now, UpdatedAt: now}
	raw, _ := json.Marshal(Payload{Version: Version, RunID: run.ID, ServiceID: run.ServiceID})
	err = s.DB.WithTx(ctx, func(tc context.Context) error {
		if err := s.DB.LockXact(tc, lockKey(req.ServiceID)); err != nil {
			return err
		}
		active, err := s.activeRun(tc, req.ServiceID)
		if err != nil {
			return err
		}
		if active != nil {
			return ErrActive
		}
		seq, err := s.Repo.CountRuns(tc, req.ServiceID)
		if err != nil {
			return err
		}
		jobID, err := s.Jobs.EnqueueTx(tc, durablejobs.ProviderAttach, fmt.Sprintf("%s#%d", req.ServiceID, seq+1), raw, JobAttempts)
		if errors.Is(err, durablejobs.ErrConflict) {
			return ErrActive
		}
		if err != nil {
			return err
		}
		run.JobID = jobID
		return s.Repo.SaveRun(tc, run)
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

// activeRun is the provider's run whose job is still pending or running (nil when none).
func (s *Service) activeRun(ctx context.Context, serviceID string) (*Run, error) {
	runs, err := s.Repo.RunsForService(ctx, serviceID, 20)
	if err != nil {
		return nil, err
	}
	for i := range runs {
		if Terminal(runs[i].Status) {
			continue
		}
		state, err := s.jobState(ctx, runs[i].JobID)
		if err != nil {
			return nil, err
		}
		if state == "pending" || state == "running" {
			return &runs[i], nil
		}
	}
	return nil, nil
}

// jobState is the durable job's state ("" when the row is gone).
func (s *Service) jobState(ctx context.Context, jobID string) (string, error) {
	if jobID == "" || s.Jobs == nil {
		return "", nil
	}
	j, err := s.Jobs.Get(ctx, jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return j.State, nil
}

// View is a run as the admin page shows it: live counts from the items and the status derived with the job's state
// (a run whose job died is "failed" with the job's fixed code; a cancel not honoured yet is "cancelling").
type View struct {
	Run
	JobState    string `json:"jobState,omitempty"`
	JobAttempts int    `json:"jobAttempts,omitempty"`
}

// View loads a run of the provider with live counts.
func (s *Service) View(ctx context.Context, serviceID, runID string) (*View, error) {
	run, err := s.Repo.LoadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run == nil || run.ServiceID != serviceID {
		return nil, ErrNotFound
	}
	return s.view(ctx, run)
}

func (s *Service) view(ctx context.Context, run *Run) (*View, error) {
	v := &View{Run: *run}
	if !Terminal(run.Status) {
		c, err := s.Repo.Counts(ctx, run.ID)
		if err != nil {
			return nil, err
		}
		v.Counts = c
	}
	if run.JobID != "" && s.Jobs != nil {
		if j, err := s.Jobs.Get(ctx, run.JobID); err == nil {
			v.JobState, v.JobAttempts = j.State, j.Attempts
			if j.State == "dead" && !Terminal(run.Status) {
				v.Status = RunFailed
				if v.Code == "" {
					v.Code = j.LastError // a fixed classification (durablejobs never stores provider text)
				}
			}
		}
	}
	if v.CancelRequested && !Terminal(v.Status) {
		v.Status = RunCancelling
	}
	return v, nil
}

// List returns the provider's newest runs (views) and the active one, if any.
func (s *Service) List(ctx context.Context, serviceID string, limit int64) ([]View, *View, error) {
	runs, err := s.Repo.RunsForService(ctx, serviceID, limit)
	if err != nil {
		return nil, nil, err
	}
	out := make([]View, 0, len(runs))
	var active *View
	for i := range runs {
		v, err := s.view(ctx, &runs[i])
		if err != nil {
			return nil, nil, err
		}
		out = append(out, *v)
		if active == nil && !Terminal(v.Status) && v.Status != RunFailed {
			a := *v
			active = &a
		}
	}
	return out, active, nil
}

// Cancel asks a run to stop: the runner finishes the projects in flight, marks the rest cancelled and ends the run.
// Already attached projects stay attached (nothing is detached).
func (s *Service) Cancel(ctx context.Context, serviceID, runID, actor string) (*View, error) {
	err := s.DB.WithTx(ctx, func(tc context.Context) error {
		run, err := s.Repo.LoadRunForUpdate(tc, runID)
		if err != nil {
			return err
		}
		if run == nil || run.ServiceID != serviceID {
			return ErrNotFound
		}
		if Terminal(run.Status) {
			return ErrFinished
		}
		if state, err := s.jobState(tc, run.JobID); err != nil {
			return err
		} else if state == "dead" || state == "succeeded" {
			return ErrFinished
		}
		if run.CancelRequested {
			return nil
		}
		run.CancelRequested, run.CancelledBy = true, actor
		return s.Repo.SaveRun(tc, run)
	})
	if err != nil {
		return nil, err
	}
	return s.View(ctx, serviceID, runID)
}

// Retry starts a new run over the projects an earlier, finished run failed on (and, when that run's job died, the ones
// it never finished).
func (s *Service) Retry(ctx context.Context, serviceID, serviceName, runID, actor string, concurrency int) (*Run, error) {
	v, err := s.View(ctx, serviceID, runID)
	if err != nil {
		return nil, err
	}
	if !Terminal(v.Status) {
		return nil, ErrNotDone
	}
	states := []string{ItemFailed}
	if v.Status == RunFailed {
		states = append(states, ItemPending, ItemRunning)
	}
	items, err := s.Repo.ItemsInState(ctx, runID, states...)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 && !(v.Status == RunFailed && !v.Planned) {
		return nil, ErrNoFailed
	}
	scope := Scope{Mode: ScopeRetry, SourceRunID: runID}
	if v.Status == RunFailed && !v.Planned {
		scope = v.Scope // it died before listing its projects: run its scope again
	}
	if concurrency == 0 {
		concurrency = v.Concurrency
	}
	return s.Start(ctx, StartRequest{ServiceID: serviceID, ServiceName: serviceName, Scope: scope, Concurrency: concurrency, Actor: actor})
}

// Items lists a run's items (optionally of one state).
func (s *Service) Items(ctx context.Context, serviceID, runID, state string, limit, offset int64) ([]Item, int64, error) {
	run, err := s.Repo.LoadRun(ctx, runID)
	if err != nil {
		return nil, 0, err
	}
	if run == nil || run.ServiceID != serviceID {
		return nil, 0, ErrNotFound
	}
	return s.Repo.ItemPage(ctx, runID, state, limit, offset)
}

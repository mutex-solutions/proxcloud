package platformlife

// job.go — the durable job of a run (design §3.4): its payload, the start that records a run and enqueues its job in
// one transaction (one unfinished run per installation), the cancel request, the reads with the job's state, and the
// load and fenced write the runner uses.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Payload is a PlatformRun job's payload.
type Payload struct {
	Version        int    `json:"version"`
	RunID          string `json:"runId"`
	InstallationID string `json:"installationId"`
}

// JobAttempts bounds a run's job: only a crash re-claim spends an attempt, because waits and transient errors Defer.
const JobAttempts = 20

var (
	ErrBadKind      = errors.New("kind must be apply or remove")
	ErrBadConfirm   = errors.New(`confirm must be "remove <installation name>", and only a remove takes it`)
	ErrRunNotFound  = errors.New("no run with that id")
	ErrRunFinished  = errors.New("this run has finished")
	ErrBOMNotHere   = errors.New(CodeBOMUnsupported + ": the installed BOM is not the one this release embeds, and no release upgrades between BOMs yet; run the ProxCloud release that installed the platform")
	errInvalidInput = errors.New("platform run: invalid job payload")
)

// DecodePayload decodes a job payload; an unknown field or a missing id is refused.
func DecodePayload(raw []byte) (Payload, error) {
	var p Payload
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil || p.Version != 1 || p.RunID == "" || p.InstallationID == "" {
		return Payload{}, errInvalidInput
	}
	return p, nil
}

// stageIDs are the stages a run of kind goes through, in order.
func stageIDs(kind string) []string {
	if kind == RunRemove {
		return []string{StageRemoveCluster, StageRemoveSeed, StageRemoveSecrets}
	}
	return []string{StageValidate, StageImages, StageSeedVM, StageSeedAPI, StageSeedIdentity, stageSeedPlatform, stageCluster,
		stageWorkloadAccess, stageWorkloadPlatform, stageRegister, stageDone}
}

// StartRun records a run of kind and enqueues its job (key platform-run-<runId>) in one transaction under the
// installation's run lock: ErrRunActive while another run is unfinished. A run whose job died is finished first, as
// failed/job_dead. An apply targets the installed BOM, or this release's for a new installation. confirm is the typed
// confirmation of a Remove ("remove <name>", or empty): see Run.Confirmed.
func (s *Store) StartRun(ctx context.Context, jobs *durablejobs.Store, installationID, kind, actor, confirm string) (*Run, error) {
	if kind != RunApply && kind != RunRemove {
		return nil, ErrBadKind
	}
	cur, err := Current()
	if err != nil {
		return nil, err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	run := &Run{ID: "pr-" + hex.EncodeToString(b[:]), InstallationID: installationID, Kind: kind, RequestedBy: actor, State: RunQueued,
		BOM: BOMRef{Release: cur.Release, SHA256: cur.SHA256}, CreatedAt: now}
	for _, id := range stageIDs(kind) {
		run.Stages = append(run.Stages, Stage{ID: id, State: StagePending})
	}
	err = s.DB.WithTx(ctx, func(tc context.Context) error {
		if err := s.DB.LockXact(tc, "platform-run:"+installationID); err != nil {
			return err
		}
		var in Installation
		if found, err := s.installations().GetForUpdate(tc, installationID, &in); err != nil {
			return err
		} else if !found {
			return ErrNotFound
		}
		if confirm != "" && (kind != RunRemove || confirm != "remove "+in.Name) {
			return ErrBadConfirm
		}
		run.Confirmed = confirm != ""
		if active, err := s.ActiveRun(tc, installationID); err != nil {
			return err
		} else if active != nil {
			if dead, err := s.settleDead(tc, jobs, active); err != nil {
				return err
			} else if !dead {
				return ErrRunActive
			}
		}
		if kind == RunApply && in.Status.BOM != nil {
			if _, ok := embeddedBOM(in.Status.BOM.SHA256); !ok {
				return ErrBOMNotHere
			}
			run.BOM = *in.Status.BOM
		}
		raw, _ := json.Marshal(Payload{Version: 1, RunID: run.ID, InstallationID: installationID})
		if run.JobID, err = jobs.EnqueueTx(tc, durablejobs.PlatformRun, "platform-run-"+run.ID, raw, JobAttempts); err != nil {
			return err
		}
		if err := s.SaveRun(tc, run); err != nil {
			return err
		}
		in.LastRunID, in.Status.Phase, in.UpdatedAt = run.ID, PhaseApplying, now
		if kind == RunRemove {
			in.Status.Phase = PhaseRemoving
		}
		_, err := s.installations().Replace(tc, installationID, &in)
		return err
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

// settleDead ends a run whose job is dead (its attempts ran out on crashes) as failed/job_dead, with the installation
// failed like after any other run that did not succeed; dead=false when the job still lives.
func (s *Store) settleDead(ctx context.Context, jobs *durablejobs.Store, run *Run) (dead bool, err error) {
	if state, err := jobState(ctx, jobs, run.JobID); err != nil || state != "dead" {
		return false, err
	}
	now := time.Now().UTC()
	run.State, run.Code, run.Message, run.FinishedAt = RunFailed, CodeJobDead, "The run's job ran out of attempts: Retry starts a new run.", &now
	if err := s.SaveRun(ctx, run); err != nil {
		return true, err
	}
	var in Installation
	if found, err := s.installations().GetForUpdate(ctx, run.InstallationID, &in); err != nil || !found {
		return true, err
	}
	in.Status.Phase, in.UpdatedAt = PhaseFailed, now
	_, err = s.installations().Replace(ctx, in.ID, &in)
	return true, err
}

func jobState(ctx context.Context, jobs *durablejobs.Store, id string) (string, error) {
	if id == "" || jobs == nil {
		return "", nil
	}
	j, err := jobs.Get(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return j.State, nil
}

// CancelRun asks an unfinished run to stop; the runner honours it at its next claim or write. A run whose job died is
// finished as failed/job_dead instead. It answers the run as it is stored after that.
func (s *Store) CancelRun(ctx context.Context, jobs *durablejobs.Store, installationID, runID string) (run *Run, err error) {
	err = s.DB.WithTx(ctx, func(tc context.Context) error {
		if err := s.DB.LockXact(tc, "platform-run:"+installationID); err != nil {
			return err
		}
		run, err = s.Run(tc, runID)
		switch {
		case err != nil:
			return err
		case run == nil || run.InstallationID != installationID:
			return ErrRunNotFound
		case Terminal(run.State):
			return ErrRunFinished
		}
		if dead, err := s.settleDead(tc, jobs, run); err != nil || dead {
			return err
		}
		run.CancelRequested = true
		return s.SaveRun(tc, run)
	})
	return run, err
}

// Run reads one run (nil when absent).
func (s *Store) Run(ctx context.Context, id string) (*Run, error) {
	var r Run
	found, err := s.runs().Get(ctx, id, &r)
	if err != nil || !found {
		return nil, err
	}
	return &r, nil
}

// RunView is a run as the routes answer it, its state derived with the job's: failed/job_dead when the job died first,
// cancelling while a cancel is not honoured yet.
func RunView(ctx context.Context, jobs *durablejobs.Store, r Run) (Run, error) {
	if Terminal(r.State) {
		return r, nil
	}
	if state, err := jobState(ctx, jobs, r.JobID); err != nil {
		return r, err
	} else if state == "dead" {
		r.State, r.Code = RunFailed, CodeJobDead
		return r, nil
	}
	if r.CancelRequested {
		r.State = "cancelling"
	}
	return r, nil
}

// Runs lists an installation's newest runs.
func (s *Store) Runs(ctx context.Context, installationID string, limit int64) ([]Run, error) {
	out := []Run{}
	err := s.runs().Find(ctx, pgdoc.M{"installationId": installationID}, &out,
		pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime)), pgdoc.Limit(limit))
	return out, err
}

// Load reads a run with its installation and secrets (nil run when absent; secrets never nil with a run).
func (s *Store) Load(ctx context.Context, runID string) (*Run, *Installation, *Secrets, error) {
	run, err := s.Run(ctx, runID)
	if err != nil || run == nil {
		return nil, nil, nil, err
	}
	in, err := s.Get(ctx, run.InstallationID)
	if errors.Is(err, ErrNotFound) {
		return run, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	sec, err := s.Secrets(ctx, in.ID)
	if sec == nil {
		sec = &Secrets{ID: in.ID}
	}
	return run, in, sec, err
}

// Mutate re-reads a run, its installation and secrets under the installation's run lock, applies fn and writes all
// three back (the secrets sealed; a document left with no secret is deleted). The runner calls it inside Checkpoint,
// so the write commits only while its job's lease holds.
func (s *Store) Mutate(ctx context.Context, runID string, fn func(*Run, *Installation, *Secrets) error) error {
	return s.DB.WithTx(ctx, func(tc context.Context) error {
		run, err := s.Run(tc, runID)
		if err != nil {
			return err
		} else if run == nil {
			return ErrRunNotFound
		}
		if err := s.DB.LockXact(tc, "platform-run:"+run.InstallationID); err != nil {
			return err
		}
		run, in, sec, err := s.Load(tc, runID)
		if err != nil {
			return err
		} else if in == nil {
			return ErrNotFound
		}
		if err := fn(run, in, sec); err != nil {
			return err
		}
		if err := s.SaveRun(tc, run); err != nil {
			return err
		}
		in.UpdatedAt = time.Now().UTC()
		if _, err := s.installations().Replace(tc, in.ID, in); err != nil {
			return err
		}
		if sec.CAPI == nil && sec.CSI == nil && sec.K3sToken == "" && sec.SeedKubeconfig == nil && sec.BootstrapToken == "" {
			_, err := s.secrets().DeleteByID(tc, in.ID)
			return err
		}
		if !s.enc.HasKey() {
			return ErrNoEncryptionKey
		}
		sec.UpdatedAt = time.Now().UTC()
		return s.secrets().Upsert(tc, in.ID, s.seal(*sec, s.enc.Encrypt))
	})
}

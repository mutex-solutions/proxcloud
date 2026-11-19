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
	ErrBadConfirm   = errors.New(`confirm must be "remove <installation name>" on a remove; "upgrade <installation name> to <release>" on an apply that starts an upgrade (targetBom); or "upgrade <installation name> to <release> and replace its machines" on the apply that begins the replacement of the platform cluster's machines, whether it starts the upgrade or continues it (GET …/plan names the phrase of each part); no other run takes one`)
	ErrBadPart      = errors.New(CodeBadPart + `: part must be "seed" or empty, and only an apply of an upgrade that replaces the platform cluster's machines takes "seed" (GET …/plan lists the parts)`)
	ErrBadTarget    = errors.New("targetBom must be the sha256 of this build's own BOM (GET /admin/platform/bom), or of the target of the upgrade in progress: a platform moves to no other BOM, and only an apply takes one")
	ErrSameBOM      = errors.New("targetBom is the BOM the platform is on: there is nothing to upgrade. An apply of the installed BOM takes neither targetBom nor confirm")
	ErrRunNotFound  = errors.New("no run with that id")
	ErrRunFinished  = errors.New("this run has finished")
	ErrBOMNotHere   = errors.New(CodeBOMUnsupported + ": the installed BOM, or the target of its unfinished upgrade, is not one this release embeds, so it can neither apply nor upgrade the platform; run the ProxCloud release that installed the platform, or delete every tenant cluster and database, then remove managed services and install again")
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

// stageIDs are the stages a run of kind goes through, in order. Part seed of an upgrade ends after the seed's own
// charts: the node image is on every node, the seed's k3s and charts have moved, and nothing of the platform cluster
// was touched. Stage done does not run, so status.targetBom stays for the platform part.
func stageIDs(kind, part string) []string {
	if kind == RunRemove {
		return []string{StageRemoveCluster, StageRemoveSeed, StageRemoveSecrets}
	}
	if part == PartSeed {
		return []string{StageValidate, StageImages, StageSeedVM, StageSeedAPI, StageSeedIdentity, stageSeedK3s, stageSeedPlatform}
	}
	return []string{StageValidate, StageImages, StageSeedVM, StageSeedAPI, StageSeedIdentity, stageSeedK3s, stageSeedPlatform, stageCluster,
		stageWorkloadAccess, stageWorkloadPlatform, stageRegister, stageDone}
}

// StartRun records a run of kind and enqueues its job (key platform-run-<runId>) in one transaction under the
// installation's run lock: ErrRunActive while another run is unfinished. A run whose job died is finished first, as
// failed/job_dead. An apply targets the installed BOM (the current one or a previous one this build embeds), or this
// release's for a new installation. targetBOM (the sha256 of this build's BOM) makes an apply an upgrade: it is
// accepted only when move allows it (UpgradeRefused otherwise) and confirm is "upgrade <name> to <release>"; the
// installation then records the target, and every later apply continues to it without a target or a confirmation
// until one succeeds. An upgrade that replaces the seed's k3s starts only with backup (SeedBackupMissing otherwise):
// the route reads Proxmox, the store decides. confirm is otherwise the typed confirmation of a Remove ("remove <name>", or empty): see
// Run.Confirmed.
//
// part "seed" makes an apply the seed part of an upgrade that replaces the platform cluster's machines; without it
// such an apply is the platform part, which begins the roll only with roll (what the route read of the site: RollRead)
// free of blocking reasons, and with the longer phrase.
func (s *Store) StartRun(ctx context.Context, jobs *durablejobs.Store, installationID, kind, actor, confirm, targetBOM, part string, backup SeedBackup, roll RollRead) (*Run, error) {
	if kind != RunApply && kind != RunRemove {
		return nil, ErrBadKind
	}
	if part != "" && (part != PartSeed || kind != RunApply) {
		return nil, ErrBadPart
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
	for _, id := range stageIDs(kind, part) {
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
		if kind == RunRemove && targetBOM != "" {
			return ErrBadTarget
		}
		if kind == RunRemove && confirm != "" && confirm != "remove "+in.Name {
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
		if kind == RunApply {
			if err := startApply(run, &in, targetBOM, confirm, part, backup, roll); err != nil {
				return err
			}
		}
		// An install starts without a tenant path test: the stored one is about a platform that was removed (Mutate
		// deletes it with the status; one left by an earlier release, or saved by a test that outlived the Remove, goes here).
		if kind == RunApply && !in.Installed() {
			if _, err := s.probes().DeleteByID(tc, installationID); err != nil {
				return err
			}
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

// SeedBackup is what an apply knows of a backup of the seed VM: Seen, the portal read one younger than SeedBackupAge
// from Proxmox (the seed-backup row is ok); Taken, the operator says one exists that the portal cannot see.
type SeedBackup struct{ Seen, Taken bool }

// startApply gives an apply its BOM (move). An upgrade, started or continued, also records where the platform comes
// from on the run and where it goes on the installation: from then on status.targetBom decides every apply. An
// upgrade that replaces the seed's k3s starts only with a backup of the seed; a Retry of a started one asks for none.
//
// A move that replaces machines has two parts (UpgradePart). Part seed is refused on every other apply. The platform
// part that begins the roll needs the site's read without a blocking reason, and takes the plan's surge or
// delete-first decision with it (Run.DeleteFirst), so the run does what the operator confirmed.
func startApply(run *Run, in *Installation, targetBOM, confirm, part string, backup SeedBackup, roll RollRead) error {
	to, up, err := move(in, targetBOM)
	allowed, blocking, phrase := up.part(part)
	begins := part == "" && up.NeedsRollRead(in)
	switch {
	case err != nil:
		return err
	case part == PartSeed && (up == nil || len(up.Parts) == 0):
		return ErrBadPart
	case up != nil && !allowed:
		return &UpgradeRefused{Reasons: blocking}
	case up == nil && targetBOM != "" && in.Status.BOM != nil: // before the confirmation: the phrase may be right, the target is not
		return ErrSameBOM
	case begins && !roll.Read:
		return rollRefused([]string{CodePlatformUnhealthy + ": the site could not be read before the replacement of the machines: the installation's Proxmox provider did not open; read the plan again"})
	case begins && len(roll.Blocking) > 0:
		return rollRefused(roll.Blocking)
	case up == nil && confirm != "", up != nil && confirm != phrase: // "" once nothing is left to confirm
		return ErrBadConfirm
	case up.MovesSeed() && !up.InProgress && !backup.Seen && !backup.Taken && in.Status.Seed != nil:
		return &SeedBackupMissing{VMID: in.Status.Seed.VMID}
	}
	run.SeedBackupTaken = up.MovesSeed() && !up.InProgress && backup.Taken
	if run.Part = part; begins {
		run.DeleteFirst = roll.DeleteFirst
	}
	if run.BOM = refOf(to); up != nil {
		from, target := *in.Status.BOM, run.BOM
		run.FromBOM, in.Status.TargetBOM = &from, &target
	}
	return nil
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
// three back (the secrets sealed; a document left with no secret is deleted, and the tenant path test once a Remove
// has wiped the status). The runner calls it inside Checkpoint,
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
		// A Remove that has wiped the status (removeSecrets): the last tenant path test was about the platform that is gone.
		if run.Kind == RunRemove && !in.Installed() {
			if _, err := s.probes().DeleteByID(tc, in.ID); err != nil {
				return err
			}
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

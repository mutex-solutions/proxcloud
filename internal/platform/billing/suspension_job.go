package billing

import (
	"context"
	"log/slog"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// SuspensionJob is the auto-suspension/dunning orchestration
// (autoSuspensionJob → autoSuspension). It is the
// billing-level state machine: an ACTIVE profile that has crossed its suspension limit gets
// a SuspensionProcess and is flipped to SUSPENDED; a profile that has recovered is RESOLVED
// and flipped back to ACTIVE.
//
// Before-suspension dunning emails + their SentAt/balance bookkeeping ARE now wired
// (executeBillingProfile). The live cloud suspend/resume runs through the
// CloudSuspender seam (nil → DB-only). The persisted profile-status flip + the process record ARE the billing effect.
type SuspensionJob struct {
	repo     *Repo
	balance  *BalanceService
	now      func() time.Time
	log      *slog.Logger
	notifier Notifier
	audit    *audit.Service
	clouds   CloudSuspender
}

func NewSuspensionJob(repo *Repo, log *slog.Logger) *SuspensionJob {
	if log == nil {
		log = slog.Default()
	}
	return &SuspensionJob{repo: repo, balance: NewBalanceService(repo), now: func() time.Time { return time.Now().UTC() }, log: log}
}

// SetNotifier wires the email hook (suspend/resume notifications). Nil → notifications skipped.
func (j *SuspensionJob) SetNotifier(n Notifier) { j.notifier = n }

// SetAudit wires the SYSTEM audit trail (auditSystem CREATE/NOTIFY/SUSPEND/UNSUSPEND on the
// SUSPENSION resource, ORGANIZATION context). Nil → no audit events.
func (j *SuspensionJob) SetAudit(a *audit.Service) { j.audit = a }

// CloudSuspender is the live cloud suspend/resume integration point
// (PVE suspend/resume each project server +
// project status DISABLED/ENABLED). Best-effort: cloud errors never block the billing state flip
// (cloud errors are swallowed). Nil → DB-only flips (the old behavior).
//
// EXTRACTION SEAM: the implementation is cloud-specific and stays in proxcloud when billing moves
// out; this becomes a billing→proxcloud HTTP callback
// (POST /internal/v1/billing-profiles/{id}/{suspend,resume}-clouds). The signature is already
// wire-ready — context-first, and at most a profile id as argument. Keep it that way.
//
// FinishResumes is the retry of the resume leg, across profiles: only the cloud side knows which VMs a suspension paused.
type CloudSuspender interface {
	SuspendBillingProfileClouds(ctx context.Context, billingProfileID string) error
	ResumeBillingProfileClouds(ctx context.Context, billingProfileID string) error
	FinishResumes(ctx context.Context) error
}

func (j *SuspensionJob) SetCloudSuspender(cs CloudSuspender) { j.clouds = cs }

// auditEvent emits one system suspension event (best-effort). Metadata carries the profile id +
// the caller's snapshot scalars (a lean snapshot — the full
// profile-object dump is omitted; the id resolves it).
func (j *SuspensionJob) auditEvent(action, procID string, profile *BillingProfile, meta map[string]any) {
	if j.audit == nil {
		return
	}
	if meta == nil {
		meta = map[string]any{}
	}
	meta["billingProfileId"] = profile.ID
	ev := audit.SystemEvent()
	ev.EventContext = audit.ContextOrganization
	ev.Action = action
	ev.ResourceType = audit.ResourceSuspension
	ev.ResourceID = procID
	ev.ResourceMetadata = meta
	ev.Outcome = audit.OutcomeSuccess
	now := j.now()
	ev.Timestamp = &now
	j.audit.LogAsync(ev)
}

// suspensionConfigFor resolves the suspension config for a profile: the
// profile's own config when it overwrites, else the global one.
func suspensionConfigFor(global *pricing.BillingAutomaticSuspensionConfig, profile *BillingProfile) *pricing.BillingAutomaticSuspensionConfig {
	if profile.SuspensionConfiguration != nil && profile.OverwriteSuspension {
		return profile.SuspensionConfiguration
	}
	return global
}

// ExecuteDunning is the autoSuspensionJob fan-out: gated on the global config being
// enabled (isSuspensionEnabled), it evaluates every ACTIVE profile for suspension. Per-profile
// errors are logged-and-skipped (per-profile failures are isolated). Returns the
// number of profiles newly suspended. Before that gate it finishes what earlier suspensions and resumes left, an operator's
// included.
func (j *SuspensionJob) ExecuteDunning(ctx context.Context) (int, error) {
	j.finishSuspensions(ctx)
	if j.clouds != nil {
		if err := j.clouds.FinishResumes(ctx); err != nil {
			j.log.Error("finishing resumes", "err", err)
		}
	}
	now := j.now()
	global, err := j.repo.SuspensionConfiguration(ctx)
	if err != nil {
		return 0, err
	}
	if global == nil || !global.Enabled {
		return 0, nil
	}
	baseCurrency, err := j.repo.BaseCurrency(ctx)
	if err != nil {
		return 0, err
	}
	profiles, err := j.repo.FindByStatus(ctx, StatusActive)
	if err != nil {
		return 0, err
	}
	suspended := 0
	for i := range profiles {
		// isNotReseller: a reseller profile is never auto-suspended.
		if profiles[i].Reseller != nil && profiles[i].Reseller.Enabled {
			continue
		}
		did, err := j.executeBillingProfile(ctx, global, &profiles[i], baseCurrency, now)
		if err != nil {
			j.log.Error("dunning failed for billing profile", "id", profiles[i].ID, "err", err)
			continue
		}
		if did {
			suspended++
		}
	}
	return suspended, nil
}

// finishSuspensions walks every SUSPENDED profile again. This is what catches a walk that failed or stopped halfway: the profile
// stays SUSPENDED, so a project left ENABLED or a VM left running gets another try every pass, whatever the failure was and
// whether or not anyone saw it. The walk only touches what is not yet down, so a clean re-walk costs reads.
//
// ponytail: it cannot tell a missed project from one an operator enabled under a suspended profile, so that enable lasts until
// the next pass. A per-project override is the upgrade if operators need one.
func (j *SuspensionJob) finishSuspensions(ctx context.Context) {
	if j.clouds == nil {
		return
	}
	profiles, err := j.repo.FindByStatus(ctx, StatusSuspended)
	if err != nil {
		j.log.Error("finishing suspensions: list profiles", "err", err)
		return
	}
	for i := range profiles {
		// A walk can take minutes, and a profile resumed since the list must not go down again, even for a moment.
		fresh, err := j.repo.FindByID(ctx, profiles[i].ID)
		if err != nil {
			j.log.Error("finishing suspensions: read profile", "billingProfile", profiles[i].ID, "err", err)
			continue
		}
		if fresh != nil && fresh.Status == StatusSuspended {
			suspendClouds(ctx, j.repo, j.clouds, fresh.ID, j.log)
		}
	}
}

// suspendClouds is the cloud leg of a suspension, run once the profile is saved SUSPENDED. A failure is logged and the
// suspension stands: the customer is suspended, and finishSuspensions walks again next pass. A resume landing during the walk
// could end with what the walk passed down again, so when the profile is ACTIVE afterwards the resume leg runs once more.
func suspendClouds(ctx context.Context, repo *Repo, clouds CloudSuspender, id string, log *slog.Logger) {
	if clouds == nil {
		return
	}
	if err := clouds.SuspendBillingProfileClouds(ctx, id); err != nil {
		log.Error("cloud suspend incomplete, the next suspension pass walks again", "billingProfile", id, "err", err)
	}
	bp, err := repo.FindByID(ctx, id)
	if err != nil || bp == nil || bp.Status != StatusActive {
		return
	}
	if err := clouds.ResumeBillingProfileClouds(ctx, id); err != nil {
		log.Error("cloud resume after a suspend walk", "billingProfile", id, "err", err)
	}
}

// resumeClouds is the cloud leg of a resume, run once the profile is saved ACTIVE. Like the suspend, the walk outlives the request:
// stopping halfway would leave VMs paused on a profile that has paid. A failure is logged, and the next pass picks up what it left
// (CloudSuspender.FinishResumes).
func resumeClouds(ctx context.Context, clouds CloudSuspender, id string, log *slog.Logger) {
	if clouds == nil {
		return
	}
	if err := clouds.ResumeBillingProfileClouds(context.WithoutCancel(ctx), id); err != nil {
		log.Error("cloud resume incomplete, the next suspension pass picks it up", "billingProfile", id, "err", err)
	}
}

// executeBillingProfile runs the suspension evaluation for one profile.
func (j *SuspensionJob) executeBillingProfile(ctx context.Context, global *pricing.BillingAutomaticSuspensionConfig, profile *BillingProfile, baseCurrency string, now time.Time) (bool, error) {
	cfg := suspensionConfigFor(global, profile)
	if cfg == nil || !cfg.Enabled {
		return false, nil
	}
	limit := cfg.StartAtSuspensionLimit()
	if limit == nil {
		return false, nil
	}
	balance, err := j.balance.CurrentBalance(ctx, profile.ID, now)
	if err != nil {
		return false, err
	}
	dueAts, err := j.balance.DueBills(ctx, profile.ID, now)
	if err != nil {
		return false, err
	}
	if profile.Status != StatusActive || !pricing.IsEligibleForSuspension(cfg, *limit, balance, dueAts, now) {
		return false, nil
	}
	// A process already SUSPENDED on this ACTIVE profile is a suspend whose profile save failed: it carries on from here.
	proc, err := j.repo.FindFirstSuspensionByStatusIn(ctx, profile.ID, SuspensionInProgress, SuspensionSuspended)
	if err != nil {
		return false, err
	}
	if proc == nil {
		proc = j.createProcess(cfg, profile, baseCurrency, now)
		if err := j.repo.SaveSuspensionProcess(ctx, proc); err != nil {
			return false, err
		}
		// create → auditSystem(CREATE, ORGANIZATION).
		j.auditEvent(audit.ActionCreate, proc.ID, profile, map[string]any{"suspensionType": cfg.Type})
	}
	// Before-suspension dunning: mark + send each not-yet-sent notification whose limit has been
	// crossed (setSentAt/setBalance → save → notifyCustomerBeforeSuspension).
	if n := pricing.MarkNotificationsToSend(cfg, proc.Notifications, dueAts, balance, now); n > 0 {
		if err := j.repo.SaveSuspensionProcess(ctx, proc); err != nil {
			return false, err
		}
		template := "notify_customer_before_suspension" // DUE_DATE
		if cfg.Type == pricing.SuspensionTypeBalance {
			template = "notify_customer_before_suspension_balance"
		}
		notify(ctx, j.notifier, template, profile, map[string]any{
			"fullName": profileFullName(profile), "balance": balance.StringFixed(2),
			"currency": baseCurrency, "unpaidInvoices": len(dueAts),
		})
		// auditSystem(NOTIFY, ORGANIZATION, {balance, unpaidInvoices, notificationsSent, suspensionType}).
		j.auditEvent(audit.ActionNotify, proc.ID, profile, map[string]any{
			"balance": balance.StringFixed(2), "unpaidInvoices": len(dueAts),
			"notificationsSent": n, "suspensionType": cfg.Type,
		})
	}
	if cfg.SuspendedAt != nil && pricing.IsEligibleForSuspension(cfg, *cfg.SuspendedAt, balance, dueAts, now) {
		if err := j.suspend(ctx, proc, profile, balance, baseCurrency); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// ReviewBillingProfile reviews a billing profile: when a profile with an
// open/suspended process is no longer eligible, resume it (if suspended) and mark the process
// RESOLVED. Exposed for wiring on balance-clearing events (e.g. a paid bill).
func (j *SuspensionJob) ReviewBillingProfile(ctx context.Context, profile *BillingProfile) error {
	now := j.now()
	global, err := j.repo.SuspensionConfiguration(ctx)
	if err != nil {
		return err
	}
	if global == nil || !global.Enabled { // BillingConfiguration.isSuspensionEnabled()
		return nil
	}
	cfg := suspensionConfigFor(global, profile)
	limit := cfg.StartAtSuspensionLimit()
	if limit == nil {
		return nil
	}
	exists, err := j.repo.ExistsSuspensionByStatusIn(ctx, profile.ID, SuspensionInProgress, SuspensionSuspended)
	if err != nil || !exists {
		return err
	}
	balance, err := j.balance.CurrentBalance(ctx, profile.ID, now)
	if err != nil {
		return err
	}
	dueAts, err := j.balance.DueBills(ctx, profile.ID, now)
	if err != nil {
		return err
	}
	if pricing.IsEligibleForSuspension(cfg, *limit, balance, dueAts, now) {
		return nil // still eligible — leave suspended
	}
	proc, err := j.repo.FindFirstSuspensionByStatusIn(ctx, profile.ID, SuspensionInProgress, SuspensionSuspended)
	if err != nil || proc == nil {
		return err
	}
	if proc.Status == SuspensionSuspended {
		if err := j.resume(ctx, profile); err != nil {
			return err
		}
	}
	proc.Status = SuspensionResolved
	if err := j.repo.SaveSuspensionProcess(ctx, proc); err != nil {
		return err
	}
	// reviewBillingProfile → auditSystem(UNSUSPEND, ORGANIZATION, {billingProfile}).
	j.auditEvent(audit.ActionUnsuspend, proc.ID, profile, nil)
	return nil
}

// createProcess creates a new IN_PROGRESS process whose notification
// slots come from the config's notification limits, stamped with the base currency.
func (j *SuspensionJob) createProcess(cfg *pricing.BillingAutomaticSuspensionConfig, profile *BillingProfile, baseCurrency string, now time.Time) *SuspensionProcess {
	notifs := make([]pricing.SuspensionNotification, 0, len(cfg.Notifications))
	for i := range cfg.Notifications {
		n := now
		notifs = append(notifs, pricing.SuspensionNotification{
			SuspensionLimit: cfg.Notifications[i],
			Currency:        baseCurrency,
			CreatedAt:       &n,
		})
	}
	return &SuspensionProcess{
		Status:           SuspensionInProgress,
		BillingProfileID: profile.ID,
		Notifications:    notifs,
	}
}

// suspend flips the process → SUSPENDED and the profile → SUSPENDED status
// (the billing-level part of suspendBillingProfile), then runs the live cloud suspend through CloudSuspender (nil-safe). The
// saves come first: a member opening a project the walk has just disabled must find the profile SUSPENDED, and a save failing
// with projects already down would leave them down with nothing to retry. Failing here leaves nothing touched, and the next pass
// carries on from whichever process status was saved.
func (j *SuspensionJob) suspend(ctx context.Context, proc *SuspensionProcess, profile *BillingProfile, balance decimal.Decimal, currency string) error {
	proc.Status = SuspensionSuspended
	if err := j.repo.SaveSuspensionProcess(ctx, proc); err != nil {
		return err
	}
	profile.Status = StatusSuspended
	if _, err := j.repo.Update(ctx, profile); err != nil {
		return err
	}
	suspendClouds(ctx, j.repo, j.clouds, profile.ID, j.log)
	j.log.Info("suspended billing profile (auto)", "id", profile.ID)
	notify(ctx, j.notifier, "notify_customer_is_suspended", profile, map[string]any{
		"fullName": profileFullName(profile), "balance": balance.StringFixed(2), "currency": currency,
	})
	// suspend → auditSystem(SUSPEND, ORGANIZATION, {billingProfile, balance}).
	j.auditEvent(audit.ActionSuspend, proc.ID, profile, map[string]any{"balance": balance.StringFixed(2)})
	return nil
}

// resume flips the profile → ACTIVE (the billing-level part of unsuspendBillingProfile).
// The live cloud resume runs through CloudSuspender (nil-safe).
func (j *SuspensionJob) resume(ctx context.Context, profile *BillingProfile) error {
	profile.Status = StatusActive
	if _, err := j.repo.Update(ctx, profile); err != nil {
		return err
	}
	// unsuspendBillingProfile: status flip first, then the live cloud resume (UNPAUSE +
	// project ENABLED), best-effort.
	resumeClouds(ctx, j.clouds, profile.ID, j.log)
	j.log.Info("resumed billing profile (auto)", "id", profile.ID)
	notify(ctx, j.notifier, "notify_customer_is_resumed", profile, map[string]any{
		"fullName": profileFullName(profile),
	})
	return nil
}

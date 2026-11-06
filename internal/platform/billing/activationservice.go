package billing

// activationservice.go implements the activation service (billing/activation): the DIRECT
// activate/suspend/resume orchestration driven by /api/v1/admin billing-profile status
// transitions and the public /admin-api/v1 endpoints (the dunning/auto flows stay in
// SuspensionJob). Cross-aggregate legs (org memberships + project enable/bootstrap) are
// injected from cmd/api since org/project sit above billing in the import graph.

import (
	"context"
	"log/slog"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

type ActivationService struct {
	repo     *Repo
	log      *slog.Logger
	audit    *audit.Service
	clouds   CloudSuspender // nil-safe: live PVE suspend/resume + project status flips
	notifier Notifier       // nil-safe: suspension/resume customer emails
	// activateProjects enables + bootstraps every non-ENABLED project of the profile with the
	// owning org's memberships. Best-effort (per-project errors are swallowed).
	//
	// EXTRACTION SEAM: org/project ownership stays in proxcloud, so this becomes a billing→proxcloud
	// callback (POST /internal/v1/billing-profiles/{id}/activate-projects). Already wire-ready:
	// context-first, profile id only.
	activateProjects func(ctx context.Context, bpID string) error
	loginURL         string // ApplicationUrlHolder.getUiBaseUrl — the {{loginUrl}} mail var
}

func NewActivationService(repo *Repo, a *audit.Service, log *slog.Logger) *ActivationService {
	if log == nil {
		log = slog.Default()
	}
	return &ActivationService{repo: repo, audit: a, log: log}
}

func (s *ActivationService) SetClouds(c CloudSuspender) { s.clouds = c }
func (s *ActivationService) SetNotifier(n Notifier)     { s.notifier = n }
func (s *ActivationService) SetLoginURL(u string)       { s.loginURL = u }

// NotifyValidation is the customer mail sent when a
// billing-profile validation flips to APPROVED (message key "billing_profile_validated";
// businessName is injected by the mail service when absent from vars). Best-effort.
func (s *ActivationService) NotifyValidation(ctx context.Context, bp *BillingProfile) {
	notify(ctx, s.notifier, "billing_profile_validated", bp, map[string]any{
		"fullName": profileFullName(bp), "email": bp.Email, "loginUrl": s.loginURL,
	})
}
func (s *ActivationService) SetActivateProjects(f func(ctx context.Context, bpID string) error) {
	s.activateProjects = f
}

// systemEvent emits a SYSTEM BILLING_PROFILE audit event.
func (s *ActivationService) systemEvent(action string, bp *BillingProfile, source string) {
	if s.audit == nil {
		return
	}
	ev := audit.SystemEvent()
	ev.Action = action
	ev.ResourceType = "BILLING_PROFILE"
	ev.ResourceID = bp.ID
	ev.ResourceDisplayName = bp.Email
	ev.ResourceMetadata = map[string]any{"source": source}
	ev.Outcome = audit.OutcomeSuccess
	s.audit.LogAsync(ev)
}

// Activate completes the activation constraint: only a NEW profile activates, and only when
// the auto-activation flow permits (ADMIN/ADMIN_API sources always may). Stamps the passed
// constraint + ACTIVE + activatedAt, then the activation side effects: affiliate sign-up
// bonus, configured provisioning promotional credits, and project enable+bootstrap.
func (s *ActivationService) Activate(ctx context.Context, bp *BillingProfile, source string) error {
	if bp.Status != StatusNew {
		return nil
	}
	flow, err := s.repo.autoActivationFlow(ctx)
	if err != nil {
		return err
	}
	if !canActivate(bp, source, flow) {
		return nil
	}
	now := time.Now().UTC()
	if bp.ActivationConstraints == nil {
		bp.ActivationConstraints = []ActivationConstraintPassed{}
	}
	bp.ActivationConstraints = append(bp.ActivationConstraints, ActivationConstraintPassed{Source: source, PassedAt: &now})
	bp.Status = StatusActive
	bp.ActivatedAt = &now
	if _, err := s.repo.Update(ctx, bp); err != nil {
		return err
	}
	s.log.Info("billing profile activated", "id", bp.ID, "source", source)
	s.systemEvent("ACTIVATE", bp, source)
	s.signUpBonus(ctx, bp)
	s.provisionPromotionalCredits(ctx, bp)
	if s.activateProjects != nil {
		if err := s.activateProjects(ctx, bp.ID); err != nil {
			// bootstrap runs per-project in isolation — never fail the activation.
			s.log.Error("activate: project bootstrap", "billingProfile", bp.ID, "err", err)
		}
	}
	return nil
}

// Suspend suspends the profile: live cloud suspend FIRST (best-effort — an error never
// blocks the billing flip), then the customer email, then SUSPENDED + save + SYSTEM audit.
func (s *ActivationService) Suspend(ctx context.Context, bp *BillingProfile, source string) error {
	if s.clouds != nil {
		if err := s.clouds.SuspendBillingProfileClouds(ctx, bp.ID); err != nil {
			s.log.Error("suspend: cloud leg failed (billing flip proceeds)", "billingProfile", bp.ID, "err", err)
		}
	}
	balance := s.currentBalance(ctx, bp.ID)
	currency, _ := s.repo.BaseCurrency(ctx)
	notify(ctx, s.notifier, "notify_customer_is_suspended", bp, map[string]any{
		"fullName": profileFullName(bp), "balance": balance.StringFixed(2), "currency": currency,
	})
	bp.Status = StatusSuspended
	if _, err := s.repo.Update(ctx, bp); err != nil {
		return err
	}
	s.log.Info("suspended billing profile", "id", bp.ID, "source", source)
	s.systemEvent(audit.ActionSuspend, bp, source)
	return nil
}

// Unsuspend resumes the profile: ACTIVE + save + audit first, then the live cloud
// resume (best-effort) and the customer email.
func (s *ActivationService) Unsuspend(ctx context.Context, bp *BillingProfile, source string) error {
	bp.Status = StatusActive
	if _, err := s.repo.Update(ctx, bp); err != nil {
		return err
	}
	s.log.Info("resumed billing profile", "id", bp.ID, "source", source)
	s.systemEvent(audit.ActionUnsuspend, bp, source)
	if s.clouds != nil {
		if err := s.clouds.ResumeBillingProfileClouds(ctx, bp.ID); err != nil {
			s.log.Error("resume: cloud leg failed (billing flip already done)", "billingProfile", bp.ID, "err", err)
		}
	}
	balance := s.currentBalance(ctx, bp.ID)
	currency, _ := s.repo.BaseCurrency(ctx)
	_ = balance // the resume mail renders balance too
	notify(ctx, s.notifier, "notify_customer_is_resumed", bp, map[string]any{
		"fullName": profileFullName(bp), "balance": balance.StringFixed(2), "currency": currency,
	})
	return nil
}

// MarkKycVerificationsAsVerified is the ADMIN "mark KYC
// verified" leg of the status transition: every verification flips verified+verifiedAt.
func (s *ActivationService) MarkKycVerificationsAsVerified(ctx context.Context, bp *BillingProfile) error {
	if len(bp.Verifications) == 0 {
		return nil
	}
	now := time.Now().UTC()
	for i := range bp.Verifications {
		if v, ok := bp.Verifications[i].(map[string]any); ok {
			v["verified"] = true
			v["verifiedAt"] = now
			bp.Verifications[i] = v
		}
	}
	_, err := s.repo.Update(ctx, bp)
	return err
}

// SuspendProcessIfExists marks a suspension process if one exists: an IN_PROGRESS dunning
// process (if any) is marked SUSPENDED so the auto flow doesn't re-drive a profile an admin
// already suspended.
func (s *ActivationService) SuspendProcessIfExists(ctx context.Context, bp *BillingProfile, source string) error {
	proc, err := s.repo.FindFirstSuspensionByStatus(ctx, bp.ID, SuspensionInProgress)
	if err != nil || proc == nil {
		return err
	}
	proc.Status = SuspensionSuspended
	if err := s.repo.SaveSuspensionProcess(ctx, proc); err != nil {
		return err
	}
	s.systemEvent(audit.ActionSuspend, bp, source)
	return nil
}

// ResolveSuspensionIfExists resolves a suspension if one exists: a SUSPENDED process resolves.
func (s *ActivationService) ResolveSuspensionIfExists(ctx context.Context, bp *BillingProfile, source string) error {
	proc, err := s.repo.FindFirstSuspensionByStatus(ctx, bp.ID, SuspensionSuspended)
	if err != nil || proc == nil {
		return err
	}
	proc.Status = SuspensionResolved
	if err := s.repo.SaveSuspensionProcess(ctx, proc); err != nil {
		return err
	}
	s.systemEvent(audit.ActionUnsuspend, bp, source)
	return nil
}

func (s *ActivationService) currentBalance(ctx context.Context, bpID string) decimal.Decimal {
	b, err := NewBalanceService(s.repo).CurrentBalance(ctx, bpID, time.Now().UTC())
	if err != nil {
		return decimal.Zero
	}
	return b
}

// signUpBonus grants the affiliate sign-up bonus: an affiliate-referred profile gets a one-time
// 10-credit (60-day) promotional credit, stamped in customInfo so it never re-applies.
func (s *ActivationService) signUpBonus(ctx context.Context, bp *BillingProfile) {
	const bonusKey = "CFYAFF_BONUS_VALUE"
	if bp.AffiliateID == "" {
		return
	}
	if bp.CustomInfo != nil {
		if _, done := bp.CustomInfo[bonusKey]; done {
			return
		}
	}
	if err := s.createPromoCredit(ctx, bp.ID, decimal.NewFromInt(10), 60); err != nil {
		s.log.Error("signUpBonus promo credit", "billingProfile", bp.ID, "err", err)
		return
	}
	if bp.CustomInfo == nil {
		bp.CustomInfo = map[string]any{}
	}
	bp.CustomInfo[bonusKey] = 10
	_, _ = s.repo.Update(ctx, bp)
}

// provisionPromotionalCredits mints the provisioning promotional credits:
// billingConfiguration.provisioningSettings.promotionals[{amount,daysValidity}] each mint a
// promotional credit for the newly-activated profile. Best-effort.
func (s *ActivationService) provisionPromotionalCredits(ctx context.Context, bp *BillingProfile) {
	promos, err := s.repo.ProvisioningPromotionals(ctx)
	if err != nil {
		s.log.Error("provisioning promotionals read", "err", err)
		return
	}
	for _, p := range promos {
		if err := s.createPromoCredit(ctx, bp.ID, p.Amount, p.DaysValidity); err != nil {
			s.log.Error("provisioning promotional credit", "billingProfile", bp.ID, "err", err)
		}
	}
}

// createPromoCredit creates a promotional credit (bpId, amount, daysValid, nil):
// daysValid must be >0 and amount >0 (a non-positive amount is silently skipped in saveCredit).
func (s *ActivationService) createPromoCredit(ctx context.Context, bpID string, amount decimal.Decimal, daysValid int) error {
	if daysValid <= 0 || !amount.IsPositive() {
		return nil
	}
	now := time.Now().UTC()
	exp := now.AddDate(0, 0, daysValid)
	_, err := s.repo.InsertPromotionalCredit(ctx, &pricing.PromotionalCredit{
		BillingProfileID: bpID, InitialAmount: &amount, RemainingAmount: &amount,
		ExpirationDate: &exp, CreatedAt: &now, UpdatedAt: &now,
	})
	return err
}

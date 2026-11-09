package main

import (
	"context"
	"errors"

	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/mail"
	"github.com/mutex-solutions/proxcloud/internal/platform/message"
)

// Prepare optional DB reads BEFORE serving, never after a missing-table error has
// already aborted the approval TX. There is no network/Mailer/activation here.
func prepareValidationDependencies(ctx context.Context, db *pgdoc.DB) error {
	if err := pgdoc.RequireNoTransaction(ctx); err != nil {
		return err
	}
	for _, name := range []string{"billingProfileValidation", "billingProfile", "thirdPartyIntegration", "messageTemplate", "billingConfiguration"} {
		if err := db.C(name).Ensure(ctx); err != nil {
			return errors.New("validation preparation store unavailable")
		}
	}
	return nil
}

type validationOutboxAdapter struct {
	store interface {
		EnqueueValidationTx(context.Context, mailoutbox.ValidationApproved) (mailoutbox.QueueResult, error)
	}
	service *mail.Service
	message interface {
		ByKey(context.Context, string) (*message.MessageTemplate, error)
	}
	config   mail.DurableConfigResolver
	loginURL string // fixed application config, never a request/document/token URL
}

func (a validationOutboxAdapter) EnqueueValidationTx(ctx context.Context, in admin.ValidationApprovalRequest) (mailoutbox.QueueResult, error) {
	// EXACTLY one lookup of the new approval-only key. Never legacy validated.
	template, err := a.message.ByKey(ctx, "billing_profile_validation_approved")
	if err != nil || template == nil {
		return mailoutbox.QueueResult{}, errors.New("validation approval template unavailable")
	}
	r := mailoutbox.ValidationApproved{ValidationID: in.ValidationID, ApprovalGeneration: in.ApprovalGeneration, Date: in.Date, ExpiresAt: in.ExpiresAt, Recipients: in.Recipients, FullName: in.FullName, LoginURL: a.loginURL}
	draft, ok, err := a.service.PrepareTemplateSnapshot(ctx, template.MessageTitle, template.MessageBody, template.Disabled, map[string]any{"fullName": in.FullName, "loginUrl": a.loginURL})
	if err != nil {
		return mailoutbox.QueueResult{}, errors.New("validation approval template preparation failed")
	}
	if !ok {
		r.Disposition = mailoutbox.Suppressed
	} else {
		r.Subject, r.TextBody, r.HTMLBody = draft.Subject, draft.TextBody, draft.HTMLBody
	}
	if a.config != nil {
		cfg, err := a.config(ctx) // DB/env only; no Mailer, SMTP or cloud capability
		if err == nil {
			r.From = cfg.From
		} else if r.Disposition == "" {
			r.Disposition = mailoutbox.Blocked
		}
	} else if r.Disposition == "" {
		r.Disposition = mailoutbox.Blocked
	}
	if len(in.Recipients) == 0 {
		r.Disposition = mailoutbox.NoRecipient
	}
	return a.store.EnqueueValidationTx(ctx, r)
}

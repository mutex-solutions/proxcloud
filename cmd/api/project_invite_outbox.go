package main

import (
	"context"
	"errors"

	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/mail"
	"github.com/mutex-solutions/proxcloud/internal/platform/message"
	"github.com/mutex-solutions/proxcloud/internal/platform/projectinvite"
)

// Optional read dependencies must exist BEFORE the business transaction. This
// adapter has no SMTP probe/sender capability and can only read DB/env config.
func prepareProjectInviteDependencies(ctx context.Context, db *pgdoc.DB) error {
	if err := pgdoc.RequireNoTransaction(ctx); err != nil {
		return err
	}
	for _, name := range []string{"projectInvite", "thirdPartyIntegration", "messageTemplate", "billingConfiguration"} {
		if err := db.C(name).Ensure(ctx); err != nil {
			return errors.New("project invite preparation store unavailable")
		}
	}
	return nil
}

type projectInviteOutboxAdapter struct {
	store interface {
		EnqueueInviteTx(context.Context, mailoutbox.Invite) (mailoutbox.QueueResult, error)
	}
	service *mail.Service
	message interface {
		ByKey(context.Context, string) (*message.MessageTemplate, error)
	}
	config mail.DurableConfigResolver
}

func (a projectInviteOutboxAdapter) EnqueueInviteTx(ctx context.Context, in projectinvite.InviteRequest) (mailoutbox.QueueResult, error) {
	// Exactly ONE snapshot lookup. No caller-selectable template key/raw vars.
	template, err := a.message.ByKey(ctx, "send_invite_to_project")
	if err != nil || template == nil {
		return mailoutbox.QueueResult{}, errors.New("project invite template unavailable")
	}
	r := mailoutbox.Invite{InviteID: in.InviteID, Date: in.Date, ExpiresAt: in.ExpiresAt,
		Recipient: mailoutbox.Recipient{Key: "recipient-00", Address: in.Email}}
	draft, ok, err := a.service.PrepareTemplateSnapshot(ctx, template.MessageTitle, template.MessageBody, template.Disabled,
		map[string]any{"projectName": in.ProjectName, "projectInviteUrl": in.JoinURL, "email": in.Email, "expiryHours": 24})
	if err != nil {
		return mailoutbox.QueueResult{}, errors.New("project invite template preparation failed")
	}
	if !ok {
		r.Disposition = mailoutbox.Suppressed
	} else {
		r.Subject, r.TextBody, r.HTMLBody = draft.Subject, draft.TextBody, draft.HTMLBody
	}
	if a.config != nil {
		cfg, err := a.config(ctx) // never constructs a Mailer or probes SMTP
		if err == nil {
			r.From = cfg.From
		} else if r.Disposition == "" {
			r.Disposition = mailoutbox.Blocked
		}
	} else if r.Disposition == "" {
		r.Disposition = mailoutbox.Blocked
	}
	if in.Email == "" {
		r.Disposition = mailoutbox.NoRecipient
	}
	return a.store.EnqueueInviteTx(ctx, r)
}

package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/mail"
	"github.com/mutex-solutions/proxcloud/internal/platform/message"
)

// Ensure optional read dependencies BEFORE owned transactions start. A missing
// optional SMTP/config table must yield blocked_config, not an aborted SQL TX.
func prepareReminderDependencies(ctx context.Context, db *pgdoc.DB) error {
	if err := pgdoc.RequireNoTransaction(ctx); err != nil {
		return err
	}
	for _, name := range []string{"thirdPartyIntegration", "messageTemplate", "billingConfiguration"} {
		if err := db.C(name).Ensure(ctx); err != nil {
			return errors.New("reminder preparation store unavailable")
		}
	}
	return nil
}

func reminderResult(durable bool, count int) map[string]any {
	if durable {
		return map[string]any{"ran": "reminders", "queuedDecisions": count, "dispatchEnabled": false}
	}
	return map[string]any{"ran": "reminders", "sent": count}
}

// This adapter is enqueue-only. Neither SMTP construction nor dispatch belongs in
// the business transaction; existing global notifiers retain legacy behavior.
type reminderOutboxAdapter struct {
	store interface {
		EnqueueTx(context.Context, mailoutbox.Reminder) (mailoutbox.QueueResult, error)
		CancelPendingTx(context.Context, []string) error
	}
	service *mail.Service
	message interface {
		ByKey(context.Context, string) (*message.MessageTemplate, error)
	}
	config mail.DurableConfigResolver
}

func (a reminderOutboxAdapter) EnqueueReminderTx(ctx context.Context, in billing.ReminderRequest) (billing.ReminderQueueResult, error) {
	const key = "savings_contract_expiry_reminder"
	template, err := a.message.ByKey(ctx, key)
	if err != nil || template == nil {
		return billing.ReminderQueueResult{}, errors.New("reminder template unavailable")
	}
	r := mailoutbox.Reminder{ContractID: in.ContractID, ExpiryGeneration: in.ExpiryGeneration,
		WindowDays: in.WindowDays, EndDate: in.EndDate, Date: in.Date}
	if template.Disabled {
		r.Disposition = mailoutbox.Suppressed
	} else {
		draft, ok, err := a.service.PrepareTemplateSnapshot(ctx, template.MessageTitle, template.MessageBody, template.Disabled, in.Vars)
		if err != nil {
			return billing.ReminderQueueResult{}, err
		}
		if !ok { // a concurrently disabled template is still explicit non-delivery
			r.Disposition = mailoutbox.Suppressed
		} else {
			r.Subject, r.TextBody, r.HTMLBody = draft.Subject, draft.TextBody, draft.HTMLBody
		}
	}
	if a.config != nil {
		cfg, err := a.config(ctx) // DB/env reads ONLY, never an SMTP probe
		if err == nil {
			r.From = cfg.From
		} else if r.Disposition == "" {
			r.Disposition = mailoutbox.Blocked
		}
	} else if r.Disposition == "" {
		r.Disposition = mailoutbox.Blocked
	}
	recipients := append([]string(nil), in.Recipients...)
	sort.Slice(recipients, func(i, j int) bool {
		return strings.ToLower(strings.TrimSpace(recipients[i])) < strings.ToLower(strings.TrimSpace(recipients[j]))
	})
	for i, address := range recipients {
		// Per-event ordinal keys reveal no globally correlatable address hash.
		// A fingerprints the encrypted full manifest, rejecting recipient drift.
		r.Recipients = append(r.Recipients, mailoutbox.Recipient{Key: fmt.Sprintf("recipient-%02d", i), Address: address})
	}
	q, err := a.store.EnqueueTx(ctx, r)
	return billing.ReminderQueueResult{IDs: q.IDs, State: q.State}, err
}
func (a reminderOutboxAdapter) CancelPendingTx(ctx context.Context, ids []string) error {
	return a.store.CancelPendingTx(ctx, ids)
}

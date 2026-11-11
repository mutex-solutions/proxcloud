package main

import (
	"context"
	"errors"

	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/mail"
	"github.com/mutex-solutions/proxcloud/internal/platform/message"
)

func prepareSavingsExpiredDependencies(ctx context.Context, db *pgdoc.DB) error {
	if err := pgdoc.RejectInheritedTransaction(ctx); err != nil {
		return err
	}
	for _, name := range []string{"billingProfile", "thirdPartyIntegration", "messageTemplate", "billingConfiguration"} {
		if err := db.C(name).Ensure(ctx); err != nil {
			return errors.New("savings expired preparation store unavailable")
		}
	}
	return nil
}

// Fixed snapshot + DB/env resolver ONLY; no Notifier, SMTP, payment or cloud
// callback is callable through this adapter. Template key is not caller input.
type savingsExpiredOutboxAdapter struct {
	store interface {
		EnqueueSavingsExpiredTx(context.Context, mailoutbox.SavingsExpired) (mailoutbox.QueueResult, error)
	}
	snapshot interface {
		PrepareTemplateSnapshot(context.Context, string, string, bool, map[string]any) (mail.Message, bool, error)
	}
	message interface {
		ByKey(context.Context, string) (*message.MessageTemplate, error)
	}
	config mail.DurableConfigResolver
}

func (a savingsExpiredOutboxAdapter) EnqueueSavingsExpiredTx(ctx context.Context, in billing.SavingsExpiredRequest) (billing.ReminderQueueResult, error) {
	template, err := a.message.ByKey(ctx, "savings_contract_expired") // exactly ONCE
	if err != nil || template == nil {
		return billing.ReminderQueueResult{}, errors.New("savings expired template unavailable")
	}
	r := mailoutbox.SavingsExpired{ContractID: in.ContractID, TermGeneration: in.TermGeneration, CanonicalEndDate: in.CanonicalEndDate, Date: in.Date, ExpiresAt: in.ExpiresAt,
		Recipients: in.Recipients, FullName: in.FullName, SavingsPlanName: in.SavingsPlanName, Currency: in.Currency, StartDate: in.StartDate, MonthlyCommittedAmount: in.MonthlyCommittedAmount}
	draft, ok, err := a.snapshot.PrepareTemplateSnapshot(ctx, template.MessageTitle, template.MessageBody, template.Disabled, map[string]any{
		"fullName": in.FullName, "savingsPlanName": in.SavingsPlanName, "currency": in.Currency, "startDate": in.StartDate, "endDate": in.CanonicalEndDate.Format("2006-01-02"), "monthlyCommittedAmount": in.MonthlyCommittedAmount})
	if err != nil {
		return billing.ReminderQueueResult{}, errors.New("savings expired template preparation failed")
	}
	if !ok {
		r.Disposition = mailoutbox.Suppressed
	} else {
		r.Subject, r.TextBody, r.HTMLBody = draft.Subject, draft.TextBody, draft.HTMLBody
	}
	if a.config != nil {
		cfg, err := a.config(ctx) // DB/env ONLY, never SMTP probe
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
	q, err := a.store.EnqueueSavingsExpiredTx(ctx, r)
	return billing.ReminderQueueResult{IDs: q.IDs, State: q.State}, err
}

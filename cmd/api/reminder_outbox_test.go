package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/mail"
	"github.com/mutex-solutions/proxcloud/internal/platform/message"
)

type reminderTemplateFixture struct {
	template *message.MessageTemplate
	reads    int
}

func (f *reminderTemplateFixture) ByKey(context.Context, string) (*message.MessageTemplate, error) {
	f.reads++
	if f.reads > 1 {
		return nil, errors.New("concurrent deletion fixture")
	}
	return f.template, nil
}

type reminderQueueFixture struct{ draft mailoutbox.Reminder }

func (f *reminderQueueFixture) EnqueueTx(_ context.Context, r mailoutbox.Reminder) (mailoutbox.QueueResult, error) {
	f.draft = r
	state := r.Disposition
	if len(r.Recipients) == 0 {
		state = mailoutbox.NoRecipient
	} else if state == "" {
		state = mailoutbox.Pending
	}
	return mailoutbox.QueueResult{IDs: []string{"opaque-id"}, State: state}, nil
}
func (*reminderQueueFixture) CancelPendingTx(context.Context, []string) error { return nil }

func TestReminderResultNeverClaimsDeliveryInDurableMode(t *testing.T) {
	result := reminderResult(true, 3)
	if _, ok := result["sent"]; ok || result["queuedDecisions"] != 3 || result["dispatchEnabled"] != false {
		t.Fatal("durable bookkeeping claimed delivery")
	}
	if reminderResult(false, 3)["sent"] != 3 {
		t.Fatal("legacy response changed")
	}
}

func TestReminderAdapterSingleSnapshotAndOpaqueOrdinalKeys(t *testing.T) {
	for _, disposition := range []string{"pending", "blocked_config", "suppressed", "no_recipient", "missing"} {
		t.Run(disposition, func(t *testing.T) {
			queue := new(reminderQueueFixture)
			templates := &reminderTemplateFixture{template: &message.MessageTemplate{MessageTitle: "Expiry {{fullName}}", MessageBody: "<p>{{fullName}}</p>", Disabled: disposition == "suppressed"}}
			if disposition == "missing" {
				templates.template = nil
			}
			service := mail.NewService(func(context.Context, string, map[string]any) (string, string, bool, error) {
				t.Fatal("second template read/render happened")
				return "", "", false, nil
			}, func(context.Context) (mail.Mailer, string) { t.Fatal("SMTP resolver invoked"); return nil, "" }, "Fixture", nil)
			config := func(context.Context) (mail.Config, error) {
				return mail.Config{Host: "unused.invalid", From: "sender@example.invalid"}, nil
			}
			if disposition == "blocked_config" {
				config = func(context.Context) (mail.Config, error) { return mail.Config{}, errors.New("unconfigured") }
			}
			a := reminderOutboxAdapter{store: queue, service: service, message: templates, config: config}
			r := billing.ReminderRequest{ContractID: "fixture", ExpiryGeneration: "expiry", EndDate: time.Now().Add(time.Hour), Date: time.Now(), WindowDays: 7, Recipients: []string{"z@example.invalid", "a@example.invalid"}, Vars: map[string]any{"fullName": "<fixture>"}}
			if disposition == "no_recipient" {
				r.Recipients = nil
			}
			got, err := a.EnqueueReminderTx(t.Context(), r)
			if disposition == "missing" {
				if err == nil {
					t.Fatal("missing template suppressed")
				}
				return
			}
			if err != nil || got.State != disposition || templates.reads != 1 {
				t.Fatalf("state=%s reads=%d err=%v", got.State, templates.reads, err)
			}
			if len(queue.draft.Recipients) > 0 && (queue.draft.Recipients[0].Key != "recipient-00" || queue.draft.Recipients[0].Address != "a@example.invalid") {
				t.Fatal("recipient key is address-dependent or unstable")
			}
			if disposition != "suppressed" && !strings.Contains(queue.draft.HTMLBody, "&lt;fixture&gt;") {
				t.Fatal("rendered snapshot not escaped/frozen")
			}
		})
	}
}

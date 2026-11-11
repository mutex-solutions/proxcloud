package main

import (
	"context"
	"errors"
	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/mail"
	"github.com/mutex-solutions/proxcloud/internal/platform/message"
	"strings"
	"testing"
	"time"
)

type expiredTemplateSpy struct {
	calls    int
	template *message.MessageTemplate
	err      error
}

func (s *expiredTemplateSpy) ByKey(_ context.Context, key string) (*message.MessageTemplate, error) {
	s.calls++
	if key != "savings_contract_expired" {
		panic("caller selected template")
	}
	return s.template, s.err
}

type expiredStoreSpy struct {
	calls int
	got   mailoutbox.SavingsExpired
	err   error
}

func (s *expiredStoreSpy) EnqueueSavingsExpiredTx(_ context.Context, in mailoutbox.SavingsExpired) (mailoutbox.QueueResult, error) {
	s.calls++
	s.got = in
	return mailoutbox.QueueResult{IDs: []string{"opaque"}, State: in.Disposition}, s.err
}

type expiredSnapshotError struct{}

func (expiredSnapshotError) PrepareTemplateSnapshot(context.Context, string, string, bool, map[string]any) (mail.Message, bool, error) {
	return mail.Message{}, false, errors.New("PRIVATE_RENDER")
}
func TestSavingsExpiredAdapterExactlyOneFixedSnapshotNoSMTP(t *testing.T) {
	for _, mode := range []string{"pending", "suppressed", "blocked", "no-recipient", "missing", "read-error", "render-error", "enqueue-error"} {
		t.Run(mode, func(t *testing.T) {
			tmpl := &expiredTemplateSpy{template: &message.MessageTemplate{MessageTitle: "Expired {{savingsPlanName}}", MessageBody: "<p>{{fullName}} {{currency}} {{startDate}} {{endDate}} {{monthlyCommittedAmount}}</p>"}}
			store := new(expiredStoreSpy)
			svc := mail.NewService(func(context.Context, string, map[string]any) (string, string, bool, error) {
				panic("second template lookup")
			}, func(context.Context) (mail.Mailer, string) { panic("SMTP forbidden") }, "fixture", nil)
			a := savingsExpiredOutboxAdapter{store: store, snapshot: svc, message: tmpl, config: func(context.Context) (mail.Config, error) { return mail.Config{From: "sender@example.invalid"}, nil }}
			now := time.Now().UTC().Truncate(time.Microsecond)
			in := billing.SavingsExpiredRequest{ContractID: "0123456789abcdef01234567", TermGeneration: strings.Repeat("a", 64), CanonicalEndDate: now.Add(-time.Hour), Date: now, ExpiresAt: now.Add(24 * time.Hour), Recipients: []string{"one@example.invalid"}, FullName: "PRIVATE_NAME", SavingsPlanName: "fixture-plan", Currency: "USD", StartDate: "2026-10-01", MonthlyCommittedAmount: "10.00"}
			want := ""
			switch mode {
			case "suppressed":
				tmpl.template.Disabled = true
				want = mailoutbox.Suppressed
			case "blocked":
				a.config = func(context.Context) (mail.Config, error) { return mail.Config{}, errors.New("PRIVATE_CONFIG") }
				want = mailoutbox.Blocked
			case "no-recipient":
				in.Recipients = nil
				want = mailoutbox.NoRecipient
			case "missing":
				tmpl.template = nil
			case "read-error":
				tmpl.err = errors.New("PRIVATE_SQL")
			case "render-error":
				a.snapshot = expiredSnapshotError{}
			case "enqueue-error":
				store.err = mailoutbox.ErrConflict
			}
			_, e := a.EnqueueSavingsExpiredTx(t.Context(), in)
			if tmpl.calls != 1 {
				t.Fatal("not exactly one snapshot")
			}
			if mode == "missing" || mode == "read-error" || mode == "render-error" || mode == "enqueue-error" {
				if e == nil || strings.Contains(e.Error(), "PRIVATE_") {
					t.Fatal("unsafe error", e)
				}
				return
			}
			if e != nil || store.calls != 1 || store.got.Disposition != want || store.got.TermGeneration != in.TermGeneration || !store.got.Date.Equal(in.Date) || !store.got.ExpiresAt.Equal(in.ExpiresAt) {
				t.Fatal("fixed request mismatch", e)
			}
			if mode == "pending" && (!strings.Contains(store.got.HTMLBody, "PRIVATE_NAME") || !strings.Contains(store.got.HTMLBody, "10.00")) {
				t.Fatal("fixed safe variables not rendered")
			}
		})
	}
}

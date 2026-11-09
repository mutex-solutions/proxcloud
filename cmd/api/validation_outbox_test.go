package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/mail"
	"github.com/mutex-solutions/proxcloud/internal/platform/message"
)

type validationTemplateSpy struct {
	calls    int
	template *message.MessageTemplate
	err      error
}

func (s *validationTemplateSpy) ByKey(_ context.Context, key string) (*message.MessageTemplate, error) {
	s.calls++
	if key != "billing_profile_validation_approved" {
		panic("arbitrary/legacy template")
	}
	return s.template, s.err
}

type validationStoreSpy struct {
	calls int
	got   mailoutbox.ValidationApproved
	err   error
}

func (s *validationStoreSpy) EnqueueValidationTx(_ context.Context, in mailoutbox.ValidationApproved) (mailoutbox.QueueResult, error) {
	s.calls++
	s.got = in
	return mailoutbox.QueueResult{IDs: []string{"opaque"}, State: in.Disposition}, s.err
}
func TestValidationAdapterOnlyNewFixedSnapshotAndNoSender(t *testing.T) {
	for _, mode := range []string{"pending", "suppressed", "blocked", "no-recipient", "missing", "read-error", "prepare-error", "enqueue-error"} {
		t.Run(mode, func(t *testing.T) {
			tmpl := &validationTemplateSpy{template: &message.MessageTemplate{MessageTitle: "Validation approved", MessageBody: "<p>{{fullName}}: validation approved only. {{loginUrl}}</p>"}}
			store := &validationStoreSpy{}
			service := mail.NewService(func(context.Context, string, map[string]any) (string, string, bool, error) {
				panic("second template read")
			}, func(context.Context) (mail.Mailer, string) { panic("SMTP forbidden") }, "Fixture", nil)
			a := validationOutboxAdapter{store: store, service: service, message: tmpl, config: func(context.Context) (mail.Config, error) { return mail.Config{From: "sender@example.invalid"}, nil }, loginURL: "https://console.example.invalid/login"}
			now := time.Now().UTC()
			in := admin.ValidationApprovalRequest{ValidationID: "0123456789abcdef01234567", ApprovalGeneration: "12345678-1234-4234-8234-123456789abc", Date: now, ExpiresAt: now.Add(24 * time.Hour), Recipients: []string{"one@example.invalid"}, FullName: "PRIVATE_NAME"}
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
			case "prepare-error":
				a.service = nil
			case "enqueue-error":
				store.err = mailoutbox.ErrConflict
			}
			_, err := a.EnqueueValidationTx(t.Context(), in)
			if tmpl.calls != 1 {
				t.Fatal("not exactly one snapshot")
			}
			if mode == "missing" || mode == "read-error" || mode == "prepare-error" || mode == "enqueue-error" {
				if err == nil || strings.Contains(err.Error(), "PRIVATE_") {
					t.Fatal("safe failure missing", err)
				}
				return
			}
			if err != nil || store.calls != 1 || store.got.Disposition != want || store.got.ApprovalGeneration != in.ApprovalGeneration || store.got.LoginURL != a.loginURL || !store.got.Date.Equal(now) {
				t.Fatal("typed snapshot mismatch", err)
			}
			if mode == "pending" && (!strings.Contains(store.got.HTMLBody, "PRIVATE_NAME") || strings.Contains(store.got.HTMLBody, "fully activated")) {
				t.Fatal("approval-only snapshot incorrect")
			}
		})
	}
}
func TestValidationSeedApprovalTruthAndLegacyUnchanged(t *testing.T) {
	var found, legacy bool
	for _, v := range message.SystemTemplates() {
		switch v.Key {
		case "billing_profile_validation_approved":
			found = true
			if !strings.Contains(v.MessageBody, "validation has been approved") || !strings.Contains(v.MessageBody, "does not confirm account activation") {
				t.Fatal("false approval template")
			}
		case "billing_profile_validated":
			legacy = true
			if !strings.Contains(v.MessageBody, "Your account is now fully activated") {
				t.Fatal("legacy operator template rewritten")
			}
		}
	}
	if !found || !legacy {
		t.Fatal("fixed template missing")
	}
}

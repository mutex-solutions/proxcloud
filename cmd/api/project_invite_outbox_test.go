package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/platform/mail"
	"github.com/mutex-solutions/proxcloud/internal/platform/message"
	"github.com/mutex-solutions/proxcloud/internal/platform/projectinvite"
)

type inviteTemplateSpy struct {
	calls    int
	template *message.MessageTemplate
	err      error
}

func (s *inviteTemplateSpy) ByKey(_ context.Context, key string) (*message.MessageTemplate, error) {
	s.calls++
	if key != "send_invite_to_project" {
		panic("caller selected arbitrary template")
	}
	return s.template, s.err
}

type inviteStoreSpy struct {
	calls int
	got   mailoutbox.Invite
	err   error
}

func (s *inviteStoreSpy) EnqueueInviteTx(_ context.Context, v mailoutbox.Invite) (mailoutbox.QueueResult, error) {
	s.calls++
	s.got = v
	return mailoutbox.QueueResult{IDs: []string{"opaque"}, State: v.Disposition}, s.err
}

func TestProjectInviteAdapterOneSnapshotNoSenderAndSafeErrors(t *testing.T) {
	for _, mode := range []string{"pending", "suppressed", "blocked", "no-recipient", "missing", "read-error", "prepare-error", "enqueue-error"} {
		t.Run(mode, func(t *testing.T) {
			templates := &inviteTemplateSpy{template: &message.MessageTemplate{MessageTitle: "Join {{projectName}}", MessageBody: "<p>{{projectInviteUrl}}</p>"}}
			store := &inviteStoreSpy{}
			service := mail.NewService(func(context.Context, string, map[string]any) (string, string, bool, error) {
				panic("second template lookup")
			}, func(context.Context) (mail.Mailer, string) { panic("SMTP inside adapter") }, "Fixture", nil)
			a := projectInviteOutboxAdapter{store: store, service: service, message: templates, config: func(context.Context) (mail.Config, error) { return mail.Config{From: "sender@example.invalid"}, nil }}
			now := time.Now().UTC()
			in := projectinvite.InviteRequest{InviteID: "0123456789abcdef01234567", Email: "one@example.invalid", ProjectName: "Project", JoinURL: "https://console.example.invalid/?invite-token=PRIVATE_TOKEN", Date: now, ExpiresAt: now.Add(24 * time.Hour)}
			want := ""
			switch mode {
			case "suppressed":
				templates.template.Disabled = true
				want = mailoutbox.Suppressed
			case "blocked":
				a.config = func(context.Context) (mail.Config, error) { return mail.Config{}, errors.New("PRIVATE_CONFIG") }
				want = mailoutbox.Blocked
			case "no-recipient":
				in.Email = ""
				want = mailoutbox.NoRecipient
			case "missing":
				templates.template = nil
			case "read-error":
				templates.err = errors.New("PRIVATE_SQL")
			case "prepare-error":
				a.service = nil
			case "enqueue-error":
				store.err = mailoutbox.ErrConflict
			}
			_, err := a.EnqueueInviteTx(t.Context(), in)
			if templates.calls != 1 {
				t.Fatal("snapshot count", templates.calls)
			}
			if mode == "missing" || mode == "read-error" || mode == "prepare-error" || mode == "enqueue-error" {
				if err == nil || strings.Contains(err.Error(), "PRIVATE_") {
					t.Fatal("error ignored or secret leaked", err)
				}
				return
			}
			if err != nil || store.calls != 1 || store.got.Disposition != want || store.got.Recipient.Key != "recipient-00" || store.got.InviteID != in.InviteID || !store.got.Date.Equal(now) || !store.got.ExpiresAt.Equal(in.ExpiresAt) {
				t.Fatal("fixed typed DTO changed", err)
			}
			if mode == "pending" && !strings.Contains(store.got.HTMLBody, "PRIVATE_TOKEN") {
				t.Fatal("rendered credential missing from sealed draft")
			}
		})
	}
}

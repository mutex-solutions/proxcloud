// Package mail is the mail-sending foundation. Sending is template-driven over a provider
// factory (MailFactory→MailOperations) + MailHistory; this package is the SEND foundation: a
// Mailer interface + an SMTP gateway impl. Template rendering (message templates) + per-email
// methods + MailHistory land in follow-up slices; the gated job side-effects (dunning/
// collect-invoice/reminders/sendBills) call Mailer so they degrade to a no-op when no gateway
// is configured.
package mail

import (
	"context"
	"os"
)

// Message is one outbound email. HTMLBody and TextBody are both optional; when both are set a
// multipart/alternative body is sent, else whichever is present.
type Message struct {
	From     string
	To       []string
	Subject  string
	TextBody string
	HTMLBody string
}

// Mailer sends email. The jobs depend on this interface (not the concrete SMTP impl) so they are
// testable and degrade gracefully when mail is unconfigured.
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// NoopMailer is the default when no mail gateway is configured: it drops the message (the gated
// email side-effects become no-ops, faithful to "notif subsystem not wired").
type NoopMailer struct{}

func (NoopMailer) Send(context.Context, Message) error { return nil }

// FromEnv builds the configured Mailer from PROXCLOUD_MAIL_* env — the bootstrap/fallback gateway.
// The admin-configured DB gateway (SMTPFromStore) takes precedence when present; this env gateway
// covers the pre-config bootstrap (and deploys that prefer config-as-code). Returns a NoopMailer
// when unconfigured.
func FromEnv() Mailer {
	cfg := Config{
		Host:     os.Getenv("PROXCLOUD_MAIL_SMTP_HOST"),
		Port:     envOr("PROXCLOUD_MAIL_SMTP_PORT", "587"),
		Username: os.Getenv("PROXCLOUD_MAIL_SMTP_USERNAME"),
		Password: os.Getenv("PROXCLOUD_MAIL_SMTP_PASSWORD"),
		From:     os.Getenv("PROXCLOUD_MAIL_FROM"),
	}
	if !cfg.Enabled() {
		return NoopMailer{}
	}
	return NewSMTPMailer(cfg)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/mail"
)

// TestSMTPFromStore verifies the admin-configured SMTP thirdPartyIntegration is decoded into a
// usable Mailer (the DB mail gateway that wins over PROXCLOUD_MAIL_* env), and that an absent SMTP
// integration yields ok=false so the caller falls back to env.
func TestSMTPFromStore(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	store := db.C("thirdPartyIntegration")

	// No SMTP integration yet → fall back (ok=false).
	if _, _, ok := mail.SMTPFromStore(ctx, store); ok {
		t.Fatal("empty store: want ok=false")
	}

	// Save an SMTP integration exactly as the admin integrations page does.
	if _, err := store.InsertOne(ctx, pgdoc.M{
		"name": "SMTP", "thirdParty": "SMTP",
		"config": pgdoc.M{
			"domain": "smtp.sendgrid.net", "port": 587, "username": "apikey",
			"fromName": "Support", "fromEmail": "no-reply@acme.test", "noAuth": false,
		},
		"secret": pgdoc.M{"password": "SG.secret"},
	}); err != nil {
		t.Fatalf("seed integration: %v", err)
	}

	mailer, from, ok := mail.SMTPFromStore(ctx, store)
	if !ok || mailer == nil {
		t.Fatalf("configured store: ok=%v mailer=%v", ok, mailer)
	}
	if from != "Support <no-reply@acme.test>" {
		t.Fatalf("from = %q, want %q", from, "Support <no-reply@acme.test>")
	}
}

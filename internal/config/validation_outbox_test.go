package config

import (
	"strings"
	"testing"
)

func TestValidationOutboxDefaultOFFStrictBooleanKeyless(t *testing.T) {
	t.Setenv("PROXCLOUD_CONFIG_FILE", t.TempDir()+"/absent")
	t.Setenv("PROXCLOUD_DB_URL", "postgres://primary/db")
	t.Setenv("PROXCLOUD_JOBS_RABBIT_FANOUT", "")
	t.Setenv("PROXCLOUD_MAIL_REMINDER_OUTBOX_ENABLED", "")
	t.Setenv("PROXCLOUD_MAIL_PROJECT_INVITE_OUTBOX_ENABLED", "")
	t.Setenv("PROXCLOUD_MAIL_VALIDATION_APPROVED_OUTBOX_ENABLED", "")
	for _, key := range []string{"", "legacy-short"} {
		t.Setenv("PROXCLOUD_ENCRYPTION_DEFAULT_KEY", key)
		c, err := Load()
		if err != nil || c.Mail.ValidationApprovedOutboxEnabled || c.Validate() != nil {
			t.Fatal("OFF startup changed", err)
		}
	}
	t.Setenv("PROXCLOUD_MAIL_VALIDATION_APPROVED_OUTBOX_ENABLED", "true")
	c, err := Load()
	if err != nil || c.Validate() == nil {
		t.Fatal("ON short key accepted", err)
	}
	t.Setenv("PROXCLOUD_ENCRYPTION_DEFAULT_KEY", strings.Repeat("x", 32))
	c, err = Load()
	if err != nil || c.Validate() != nil || !c.Mail.ValidationApprovedOutboxEnabled || c.Mail.ProjectInviteOutboxEnabled || c.Mail.ReminderOutboxEnabled {
		t.Fatal("ON isolated flag changed", err)
	}
	t.Setenv("PROXCLOUD_MAIL_VALIDATION_APPROVED_OUTBOX_ENABLED", "PRIVATE_BOOLEAN")
	if _, err = Load(); err == nil || strings.Contains(err.Error(), "PRIVATE_BOOLEAN") {
		t.Fatal("malformed flag accepted/echoed")
	}
}

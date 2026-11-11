package config

import (
	"strings"
	"testing"
)

func TestProjectInviteOutboxDefaultOFFStrictBooleanAndKey(t *testing.T) {
	t.Setenv("PROXCLOUD_CONFIG_FILE", t.TempDir()+"/absent")
	t.Setenv("PROXCLOUD_DB_URL", "postgres://primary/db")
	t.Setenv("PROXCLOUD_JOBS_RABBIT_FANOUT", "")
	t.Setenv("PROXCLOUD_MAIL_REMINDER_OUTBOX_ENABLED", "")
	t.Setenv("PROXCLOUD_MAIL_PROJECT_INVITE_OUTBOX_ENABLED", "")
	t.Setenv("PROXCLOUD_ENCRYPTION_DEFAULT_KEY", "legacy-short")
	c, err := Load()
	if err != nil || c.Mail.ProjectInviteOutboxEnabled || c.Validate() != nil {
		t.Fatal("default OFF legacy startup changed", err)
	}
	t.Setenv("PROXCLOUD_MAIL_PROJECT_INVITE_OUTBOX_ENABLED", "true")
	c, err = Load()
	if err != nil || c.Validate() == nil {
		t.Fatal("ON accepted short key", err)
	}
	t.Setenv("PROXCLOUD_ENCRYPTION_DEFAULT_KEY", strings.Repeat("x", 32))
	c, err = Load()
	if err != nil || !c.Mail.ProjectInviteOutboxEnabled || c.Validate() != nil || c.Mail.ReminderOutboxEnabled {
		t.Fatal("typed opt-in changed other producer", err)
	}
	t.Setenv("PROXCLOUD_MAIL_PROJECT_INVITE_OUTBOX_ENABLED", "PRIVATE_BOOLEAN_CANARY")
	if _, err = Load(); err == nil || strings.Contains(err.Error(), "PRIVATE_BOOLEAN_CANARY") {
		t.Fatal("malformed flag accepted/echoed")
	}
}

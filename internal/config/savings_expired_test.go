package config

import (
	"strings"
	"testing"
)

func TestSavingsExpiredDefaultOFFStrictBooleanKeyless(t *testing.T) {
	t.Setenv("PROXCLOUD_CONFIG_FILE", t.TempDir()+"/absent")
	t.Setenv("PROXCLOUD_DB_URL", "postgres://primary/db")
	t.Setenv("PROXCLOUD_JOBS_RABBIT_FANOUT", "")
	for _, flag := range []string{"PROXCLOUD_MAIL_REMINDER_OUTBOX_ENABLED", "PROXCLOUD_MAIL_PROJECT_INVITE_OUTBOX_ENABLED", "PROXCLOUD_MAIL_VALIDATION_APPROVED_OUTBOX_ENABLED", "PROXCLOUD_MAIL_SAVINGS_EXPIRED_OUTBOX_ENABLED"} {
		t.Setenv(flag, "")
	}
	for _, key := range []string{"", "legacy-short"} {
		t.Setenv("PROXCLOUD_ENCRYPTION_DEFAULT_KEY", key)
		c, e := Load()
		if e != nil || c.Mail.SavingsExpiredOutboxEnabled || c.Validate() != nil {
			t.Fatal("OFF keyless startup", e)
		}
	}
	t.Setenv("PROXCLOUD_MAIL_SAVINGS_EXPIRED_OUTBOX_ENABLED", "true")
	c, e := Load()
	if e != nil || c.Validate() == nil {
		t.Fatal("ON short key admitted")
	}
	t.Setenv("PROXCLOUD_ENCRYPTION_DEFAULT_KEY", strings.Repeat("x", 32))
	c, e = Load()
	if e != nil || c.Validate() != nil || !c.Mail.SavingsExpiredOutboxEnabled || c.Mail.ReminderOutboxEnabled || c.Mail.ProjectInviteOutboxEnabled || c.Mail.ValidationApprovedOutboxEnabled {
		t.Fatal("dedicated flag", e)
	}
	t.Setenv("PROXCLOUD_MAIL_SAVINGS_EXPIRED_OUTBOX_ENABLED", "PRIVATE_FLAG")
	if _, e = Load(); e == nil || strings.Contains(e.Error(), "PRIVATE_FLAG") {
		t.Fatal("bad flag admitted/echoed")
	}
}

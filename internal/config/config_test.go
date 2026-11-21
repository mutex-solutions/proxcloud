package config

import (
	"strings"
	"testing"
)

func TestDurableJobsNoRabbitDefaultAndMigrationGuard(t *testing.T) {
	t.Setenv("PROXCLOUD_CONFIG_FILE", t.TempDir()+"/absent")
	t.Setenv("PROXCLOUD_DB_URL", "postgres://primary/db")
	t.Setenv("PROXCLOUD_DB_READ_URL", "postgres://history/db")
	t.Setenv("PROXCLOUD_RABBITMQ_HOST", "")
	t.Setenv("PROXCLOUD_JOBS_RABBIT_FANOUT", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.DB.URL == c.DB.ReadURL || c.Jobs.WorkerConcurrency != 2 {
		t.Fatalf("wrong primary/stats defaults: %+v", c)
	}
	c.Jobs.RabbitFanout = true
	if err = c.Validate(); err == nil || !strings.Contains(err.Error(), "drain/reconcile") {
		t.Fatalf("legacy lost-jobs migration not guarded: %v", err)
	}
}

func TestReminderOutboxOptInRequiresStrictKeyButNeverDispatch(t *testing.T) {
	t.Setenv("PROXCLOUD_CONFIG_FILE", t.TempDir()+"/absent")
	t.Setenv("PROXCLOUD_DB_URL", "postgres://primary/db")
	t.Setenv("PROXCLOUD_JOBS_RABBIT_FANOUT", "")
	t.Setenv("PROXCLOUD_ENCRYPTION_DEFAULT_KEY", "legacy-short")
	t.Setenv("PROXCLOUD_MAIL_REMINDER_OUTBOX_ENABLED", "")
	c, err := Load()
	if err != nil || c.Mail.ReminderOutboxEnabled || c.Validate() != nil {
		t.Fatal("legacy default changed")
	}
	t.Setenv("PROXCLOUD_MAIL_REMINDER_OUTBOX_ENABLED", "true")
	c, err = Load()
	if err != nil || c.Validate() == nil {
		t.Fatal("short key accepted for durable reminders")
	}
	t.Setenv("PROXCLOUD_ENCRYPTION_DEFAULT_KEY", strings.Repeat("fixture-only-", 3))
	c, err = Load()
	if err != nil || !c.Mail.ReminderOutboxEnabled || c.Validate() != nil {
		t.Fatalf("durable enqueue configuration rejected: %v", err)
	}
	t.Setenv("PROXCLOUD_MAIL_REMINDER_OUTBOX_ENABLED", "private-invalid-canary")
	if _, err := Load(); err == nil || strings.Contains(err.Error(), "canary") {
		t.Fatal("malformed opt-in accepted or echoed")
	}
}

func TestDurableJobBounds(t *testing.T) {
	t.Setenv("PROXCLOUD_CONFIG_FILE", t.TempDir()+"/absent")
	t.Setenv("PROXCLOUD_DB_URL", "postgres://primary/db")
	t.Setenv("PROXCLOUD_JOBS_RABBIT_FANOUT", "")
	for _, value := range []string{"0", "-1", "17"} {
		t.Setenv("PROXCLOUD_JOBS_WORKER_CONCURRENCY", value)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.Validate() == nil {
			t.Fatalf("unbounded worker concurrency %s", value)
		}
	}
	t.Setenv("PROXCLOUD_JOBS_WORKER_CONCURRENCY", "garbage-canary-do-not-echo")
	if _, err := Load(); err == nil || strings.Contains(err.Error(), "garbage-canary") {
		t.Fatalf("malformed concurrency not safely rejected: %v", err)
	}
	t.Setenv("PROXCLOUD_JOBS_WORKER_CONCURRENCY", "2")
	t.Setenv("PROXCLOUD_JOBS_POLL_INTERVAL", "nonsense")
	if _, err := Load(); err == nil {
		t.Fatal("invalid duration ignored")
	}
}

// A self-signed gateway certificate: its CA is the certificate unless one is named, and it needs the URL (its host is
// the certificate's) and the two file paths.
func TestCSIGatewaySelfSigned(t *testing.T) {
	t.Setenv("PROXCLOUD_CONFIG_FILE", t.TempDir()+"/absent")
	t.Setenv("PROXCLOUD_DB_URL", "postgres://primary/db")
	t.Setenv("PROXCLOUD_JOBS_RABBIT_FANOUT", "")
	t.Setenv("PROXCLOUD_CSI_GATEWAY_ADDR", ":8444")
	t.Setenv("PROXCLOUD_CSI_GATEWAY_TLS_CERT", "/var/lib/proxcloud/gateway/tls.crt")
	t.Setenv("PROXCLOUD_CSI_GATEWAY_TLS_KEY", "/var/lib/proxcloud/gateway/tls.key")
	t.Setenv("PROXCLOUD_CSI_GATEWAY_URL", "https://192.0.2.7:8444")
	t.Setenv("PROXCLOUD_CSI_GATEWAY_CA_FILE", "")
	t.Setenv("PROXCLOUD_CSI_GATEWAY_SELF_SIGNED", "true")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil || !c.CSIGateway.SelfSigned || c.CSIGateway.CAFile != "/var/lib/proxcloud/gateway/tls.crt" {
		t.Fatalf("%v %+v", err, c.CSIGateway)
	}
	c.CSIGateway.URL = ""
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "PROXCLOUD_CSI_GATEWAY_SELF_SIGNED") {
		t.Fatalf("self-signed without a URL: %v", err)
	}
	t.Setenv("PROXCLOUD_CSI_GATEWAY_SELF_SIGNED", "")
	if c, _ := Load(); c.CSIGateway.SelfSigned || c.CSIGateway.CAFile != "" {
		t.Fatalf("off by default: %+v", c.CSIGateway)
	}
}

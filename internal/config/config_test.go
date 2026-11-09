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

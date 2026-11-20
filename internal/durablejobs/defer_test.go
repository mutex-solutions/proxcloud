package durablejobs

import (
	"context"
	"testing"
)

// Defer is for external-kind acquisitions only (a busy resource lock of the template update runner);
// a DB-only job is refused before any database access.
func TestDeferRefusesNonExternalKinds(t *testing.T) {
	s := &Store{}
	for _, kind := range []string{Billing, Selftest, "unknown"} {
		if err := s.Defer(context.Background(), &Job{Kind: kind}, 0, "smoke_lock_busy"); err == nil {
			t.Errorf("%s deferred", kind)
		}
	}
	if err := s.Defer(context.Background(), &Job{Kind: TemplateUpdateSmoke}, 0, "Bad Code"); err == nil {
		t.Error("free-form code accepted")
	}
}

// A code is a-z, 0-9 and "_" and starts with a letter: the names codes are built from carry digits (stage seed-k3s).
func TestValidCode(t *testing.T) {
	for _, ok := range []string{"smoke_lock_busy", "waiting_seed_k3s", "seed_k3s_failed", "retry_seed_k3s", "a"} {
		if !ValidCode(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	long := make([]byte, 65)
	for i := range long {
		long[i] = 'a'
	}
	for _, bad := range []string{"", "Bad Code", "seed-k3s", "3k", "_x", "dial tcp 10.0.0.1:6443", string(long)} {
		if ValidCode(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	s := &Store{}
	if err := s.Fail(context.Background(), &Job{Kind: TemplateUpdateSmoke}, 0, true, "seed-k3s"); err == nil || err.Error() != "invalid failure" {
		t.Errorf("Fail with a dash: %v", err)
	}
}

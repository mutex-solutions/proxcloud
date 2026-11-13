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

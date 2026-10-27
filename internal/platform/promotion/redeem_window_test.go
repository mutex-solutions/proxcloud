package promotion

import (
	"net/http"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// TestValidateRedeemable_stringWindowEnforced is the regression for finding [20]: validFrom/validUntil
// stored as an ISO-8601 STRING (or an epoch number) must enforce the redemption window. Previously
// promoTime only understood an RFC3339 date / time.Time, so a string bound was silently ignored and the
// code stayed redeemable outside its window.
func TestValidateRedeemable_stringWindowEnforced(t *testing.T) {
	const org = "org-1"
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)

	// String validFrom in the future → rejected (was silently allowed before the fix).
	if e := validateRedeemable("X", pgdoc.M{"status": "ACTIVE", "validFrom": future}, org); e == nil || e.Status != http.StatusBadRequest || e.Msg != "This promotion code is not yet active" {
		t.Errorf("string validFrom in future: got %v, want 400 not-yet-active", e)
	}
	// String validUntil in the past → rejected.
	if e := validateRedeemable("X", pgdoc.M{"status": "ACTIVE", "validUntil": past}, org); e == nil || e.Msg != "This promotion code has expired" {
		t.Errorf("string validUntil in past: got %v, want expired", e)
	}
	// Epoch-millis bound is enforced too.
	futMillis := time.Now().Add(48 * time.Hour).UnixMilli()
	if e := validateRedeemable("X", pgdoc.M{"status": "ACTIVE", "validFrom": futMillis}, org); e == nil || e.Msg != "This promotion code is not yet active" {
		t.Errorf("epoch validFrom in future: got %v, want not-yet-active", e)
	}
	// A still-valid string window passes.
	if e := validateRedeemable("X", pgdoc.M{"status": "ACTIVE", "validFrom": past, "validUntil": future}, org); e != nil {
		t.Errorf("in-window string bounds: got %v, want nil", e)
	}
}

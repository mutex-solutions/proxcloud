package promotion

import (
	"net/http"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

func TestParseISO8601Duration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"PT720H", 720 * time.Hour, true},   // 30 days expressed as hours
		{"P30D", 30 * 24 * time.Hour, true}, // 30 days
		{"PT24H", 24 * time.Hour, true},
		{"P1DT2H30M", 24*time.Hour + 2*time.Hour + 30*time.Minute, true},
		{"PT0.5H", 30 * time.Minute, true}, // fractional
		{"pt24h", 24 * time.Hour, true},    // case-insensitive
		{"PT30M", 30 * time.Minute, true},
		{"PT45S", 45 * time.Second, true},
		{"", 0, false},
		{"P", 0, false},      // no components
		{"30", 0, false},     // missing P
		{"P30", 0, false},    // bare date number, no unit
		{"PT30X", 0, false},  // bad unit
		{"PTABCH", 0, false}, // non-numeric
	}
	for _, c := range cases {
		got, ok := parseISO8601Duration(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseISO8601Duration(%q) = (%v,%v), want (%v,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestPromoExpiry(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)

	// ISO-8601 string shape → now + duration.
	if got := promoExpiry(pgdoc.M{"creditValidityDuration": "PT24H"}, now); !got.Equal(now.Add(24 * time.Hour)) {
		t.Errorf("string PT24H expiry = %v, want %v", got, now.Add(24*time.Hour))
	}
	if got := promoExpiry(pgdoc.M{"creditValidityDuration": "P30D"}, now); !got.Equal(now.AddDate(0, 0, 30)) {
		t.Errorf("string P30D expiry = %v, want %v", got, now.AddDate(0, 0, 30))
	}

	// Absent → NO_EXPIRATION sentinel (never nil).
	if got := promoExpiry(pgdoc.M{}, now); !got.Equal(noExpiration) {
		t.Errorf("absent duration expiry = %v, want NO_EXPIRATION", got)
	}
	// Unparseable string → NO_EXPIRATION (never an instantly-expired credit).
	if got := promoExpiry(pgdoc.M{"creditValidityDuration": "garbage"}, now); !got.Equal(noExpiration) {
		t.Errorf("garbage duration expiry = %v, want NO_EXPIRATION", got)
	}
	// Legacy {value,unit} fallback (defensive).
	if got := promoExpiry(pgdoc.M{"creditValidityDuration": pgdoc.M{"value": int32(2), "unit": "MONTHS"}}, now); !got.Equal(now.AddDate(0, 2, 0)) {
		t.Errorf("legacy 2 MONTHS expiry = %v, want %v", got, now.AddDate(0, 2, 0))
	}
	// Bare number = days (defensive).
	if got := promoExpiry(pgdoc.M{"creditValidityDuration": int32(7)}, now); !got.Equal(now.AddDate(0, 0, 7)) {
		t.Errorf("bare-number 7 expiry = %v, want %v", got, now.AddDate(0, 0, 7))
	}
}

func TestValidateRedeemable(t *testing.T) {
	const org = "org-1"
	active := pgdoc.M{"status": "ACTIVE"}

	// Blank code → 400 "Invalid code. "
	if e := validateRedeemable("", active, org); e == nil || e.Status != http.StatusBadRequest || e.Msg != "Invalid code. " {
		t.Errorf("blank code: got %v, want 400 'Invalid code. '", e)
	}
	// Not found (nil pc) → 400 "Invalid code. " (NOT 404)
	if e := validateRedeemable("X", nil, org); e == nil || e.Status != http.StatusBadRequest || e.Msg != "Invalid code. " {
		t.Errorf("nil pc: got %v, want 400 'Invalid code. '", e)
	}
	// DISABLED → 400 "This promotion code is disabled"
	if e := validateRedeemable("X", pgdoc.M{"status": "DISABLED"}, org); e == nil || e.Msg != "This promotion code is disabled" {
		t.Errorf("disabled: got %v", e)
	}
	// ACTIVE → ok
	if e := validateRedeemable("X", active, org); e != nil {
		t.Errorf("active: got %v, want nil", e)
	}
	// Absent status → ok (only DISABLED is rejected, not non-ACTIVE)
	if e := validateRedeemable("X", pgdoc.M{}, org); e != nil {
		t.Errorf("absent status: got %v, want nil", e)
	}
	// validFrom in the future → "not yet active" (promoTime accepts a raw time.Time)
	if e := validateRedeemable("X", pgdoc.M{"status": "ACTIVE", "validFrom": time.Now().Add(48 * time.Hour)}, org); e == nil || e.Msg != "This promotion code is not yet active" {
		t.Errorf("future validFrom: got %v", e)
	}
	// validUntil in the past → "has expired"
	if e := validateRedeemable("X", pgdoc.M{"status": "ACTIVE", "validUntil": time.Now().Add(-48 * time.Hour)}, org); e == nil || e.Msg != "This promotion code has expired" {
		t.Errorf("past validUntil: got %v", e)
	}
	// targeted to a different org → "cannot be redeemed"
	if e := validateRedeemable("X", pgdoc.M{"status": "ACTIVE", "targetOrganizationIds": pgdoc.A{"other-org"}}, org); e == nil || e.Msg != "This promotion code cannot be redeemed by your organization" {
		t.Errorf("wrong target: got %v", e)
	}
	// targeted to this org → ok
	if e := validateRedeemable("X", pgdoc.M{"status": "ACTIVE", "targetOrganizationIds": pgdoc.A{org}}, org); e != nil {
		t.Errorf("right target: got %v, want nil", e)
	}
}

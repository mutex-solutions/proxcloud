package billingprovider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/billingclient"
)

func TestNormalize(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", Native}, // unset means native: a deployment that never heard of this gets the in-tree engine
		{"   ", Native},
		{"native", Native},
		{"NATIVE", Native},
		{" External ", External},
		{"typo", "typo"}, // preserved so Validate can name it back to the operator
	} {
		if got := Normalize(tc.in); got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestValidateRejectsUnknown: a typo in the provider name must stop the deployment. Defaulting it
// would charge bills with an engine the operator did not choose — silently, and possibly into the
// wrong database.
func TestValidateRejectsUnknown(t *testing.T) {
	for _, ok := range []string{"", "native", "NATIVE", "external", " external "} {
		if err := Validate(ok); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"externl", "remote", "mono", "none"} {
		err := Validate(bad)
		if err == nil {
			t.Errorf("Validate(%q) = nil, want an error", bad)
			continue
		}
		// The message must name the offending value, or an operator cannot find the typo.
		if !contains(err.Error(), bad) {
			t.Errorf("Validate(%q) error should quote the value, got: %v", bad, err)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// --- external provider -------------------------------------------------------------------------

type stubCharger struct {
	ids     []string
	resp    *billingclient.ChargeResponse
	chargeN int
	err     error
}

func (s *stubCharger) ActiveProfileIDs(context.Context) ([]string, error) { return s.ids, s.err }
func (s *stubCharger) Charge(context.Context, billingclient.ChargeRequest) (*billingclient.ChargeResponse, error) {
	s.chargeN++
	return s.resp, nil
}

type stubResolver struct{ n int }

func (r *stubResolver) ResolveByProfileID(_ context.Context, id, tu string, _ time.Time) (billingclient.ChargeRequest, error) {
	r.n++
	return billingclient.ChargeRequest{ProfileID: id, TimeUnit: tu}, nil
}

func TestExternalProviderCharges(t *testing.T) {
	c := &stubCharger{ids: []string{"bp-1", "bp-2"}, resp: &billingclient.ChargeResponse{Charged: true}}
	r := &stubResolver{}
	p := NewExternal(r, c, nil)

	if p.Name() != External {
		t.Errorf("Name() = %q, want %q", p.Name(), External)
	}
	if err := p.Charge(context.Background(), "minute", time.Now().UTC()); err != nil {
		t.Fatalf("charge: %v", err)
	}
	if r.n != 2 || c.chargeN != 2 {
		t.Errorf("resolved %d and charged %d, want 2 and 2", r.n, c.chargeN)
	}
}

// TestExternalProviderDoesNotFallBack is the important one. Both engines write bills, and during a
// migration they may write to DIFFERENT databases — so quietly charging in-process when the billing
// service is unreachable could bill the same cycle twice, in two places. The run must fail instead.
func TestExternalProviderDoesNotFallBack(t *testing.T) {
	c := &stubCharger{err: errors.New("billing service unreachable")}
	p := NewExternal(&stubResolver{}, c, nil)

	err := p.Charge(context.Background(), "minute", time.Now().UTC())
	if err == nil {
		t.Fatal("an unreachable billing service must fail the run, not fall back to the local engine")
	}
	if c.chargeN != 0 {
		t.Errorf("nothing should have been charged, got %d", c.chargeN)
	}
}

// --- native provider ---------------------------------------------------------------------------

func TestNativeDurableDispatchNoFallback(t *testing.T) {
	boom := errors.New("primary queue unavailable")
	original := time.Date(2026, 10, 31, 23, 59, 0, 0, time.UTC)
	calls := 0
	p := NewNative(func(_ context.Context, unit string, at time.Time) error {
		calls++
		if unit != "minute" || !at.Equal(original) {
			t.Fatal("original dispatch identity changed")
		}
		return boom
	})
	if p.Name() != Native || !errors.Is(p.Charge(context.Background(), "minute", original), boom) || calls != 1 {
		t.Fatal("native queue error silently bypassed")
	}
	if NewNative(nil).Charge(context.Background(), "minute", original) == nil {
		t.Fatal("missing queue accepted")
	}
}

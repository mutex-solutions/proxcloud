package billingjob

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/billingclient"
)

// stubCharger stands in for the billing service.
type stubCharger struct {
	ids  []string
	resp *billingclient.ChargeResponse
	err  error
}

func (s *stubCharger) ActiveProfileIDs(context.Context) ([]string, error) { return s.ids, nil }

// stubResolver stands in for the cloud-resolution half so these tests exercise the driver's
// response handling without a database.
type stubResolver struct{}

func (stubResolver) ResolveByProfileID(_ context.Context, profileID, timeUnit string, _ time.Time) (billingclient.ChargeRequest, error) {
	return billingclient.ChargeRequest{ProfileID: profileID, TimeUnit: timeUnit}, nil
}
func (s *stubCharger) Charge(context.Context, billingclient.ChargeRequest) (*billingclient.ChargeResponse, error) {
	return s.resp, s.err
}

// TestRemoteChargeFailsWhenNothingIsCharged is the guard against the worst failure mode of the
// split: the billing service answers 200 with charged=false — because its engine is disabled, or it
// has no billingConfiguration — and the cron treats that as a good run. Bills silently stop being
// produced while every dashboard stays green.
func TestRemoteChargeFailsWhenNothingIsCharged(t *testing.T) {
	c := &stubCharger{
		ids:  []string{"bp-1", "bp-2"},
		resp: &billingclient.ChargeResponse{Charged: false, Reason: "billing not configured"},
	}

	err := RemoteCharge(context.Background(), stubResolver{}, c, "minute", time.Now().UTC(), nil)
	if err == nil {
		t.Fatal("a run where no active profile was charged must fail the cron, not pass silently")
	}
	if !strings.Contains(err.Error(), "none of the") {
		t.Errorf("error should explain that nothing was charged, got: %v", err)
	}
}

// TestRemoteChargeSucceedsWithNoActiveProfiles: zero ACTIVE profiles is a legitimately empty run,
// not a failure — otherwise a fresh deployment would alarm forever.
func TestRemoteChargeSucceedsWithNoActiveProfiles(t *testing.T) {
	c := &stubCharger{ids: nil}
	if err := RemoteCharge(context.Background(), stubResolver{}, c, "minute", time.Now().UTC(), nil); err != nil {
		t.Errorf("an empty profile list is not an error, got: %v", err)
	}
}

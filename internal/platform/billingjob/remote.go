package billingjob

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/billingresource"
	"github.com/mutex-solutions/proxcloud/internal/platform/billingclient"
)

// remote.go is the charge path for the EXTERNAL billing provider
// (PROXCLOUD_BILLING_PROVIDER=external). It is one of two implementations behind
// internal/platform/billingprovider; the other, and the default, is the billing engine in this
// repository.
//
// The work splits where the in-process driver already splits it: RESOLUTION (read the cloud cache,
// resolve the profile's projects, build billable units) always happens here because it is
// cloud-specific and nothing else can do it, and RATING goes to the billing service. The only
// difference from Service.Charge is that the units are POSTed instead of handed to pricing
// in-process.
//
// Note what this path does NOT touch: the local billing repo. The profile work-list comes from the
// billing service, and resolution only needs a profile id to walk orgs → projects → cloud
// resources. The native engine remains fully present and is what a plain checkout of proxcloud runs.

// Charger is the subset of billingclient.Client the remote driver needs (an interface so the driver
// can be tested without a server).
type Charger interface {
	ActiveProfileIDs(ctx context.Context) ([]string, error)
	Charge(ctx context.Context, req billingclient.ChargeRequest) (*billingclient.ChargeResponse, error)
}

// Resolver builds a profile's charge request from local cloud state. *Service implements it; the
// interface keeps the driver's response handling testable without a database behind it.
type Resolver interface {
	ResolveByProfileID(ctx context.Context, profileID, timeUnit string, now time.Time) (billingclient.ChargeRequest, error)
}

// ResolveByProfileID builds one profile's charge request from local cloud state.
//
// It mirrors chargeBillingResource's resolution steps: the profile's ENABLED projects that have
// services, then each external service's project-scoped cloud resources dispatched through the
// type→Provider registry. Rating inputs the billing service cannot know (the resource-type catalog)
// travel with the request.
func (s *Service) ResolveByProfileID(ctx context.Context, profileID, timeUnit string, now time.Time) (billingclient.ChargeRequest, error) {
	cycleStart, cycleEnd, cycleTimestamp := chargeCycle(now, timeUnit)
	req := billingclient.ChargeRequest{
		ProfileID:      profileID,
		TimeUnit:       timeUnit,
		CycleStart:     cycleStart,
		CycleEnd:       cycleEnd,
		CycleTimestamp: cycleTimestamp,
		Catalog:        billingresource.Catalog(),
	}

	projects, err := s.activeProjectsWithServices(ctx, profileID)
	if err != nil {
		return req, err
	}
	externalServices, err := s.d.ExternalServices.List(ctx)
	if err != nil {
		return req, err
	}

	// The rating side must see the SAME per-time-unit limits this side resolved with: they feed both
	// the charge cap and the elapsed-units calculation, so a mismatch silently rates differently.
	// billingContext() returns no limits today (the engine falls back to its defaults on both sides),
	// but propagating it now means the day billingConfiguration.settings.timeUnitLimits starts being
	// honoured here, the remote path follows automatically instead of quietly diverging.
	bc := s.billingContext(ctx)
	req.TimeUnitLimits = bc.TimeUnitLimits

	for i := range externalServices {
		es := &externalServices[i]
		resources, err := s.billingResources(ctx, bc, projects, es, timeUnit, cycleStart)
		if err != nil {
			return req, err
		}
		// Services with no resources are included deliberately, not skipped. The in-process driver
		// calls ChargeBillingResources once per external service regardless, and that call
		// CREATES the current bill when absent. Dropping empty groups
		// here would mean an ACTIVE profile with nothing billable stops getting a bill created,
		// which is visible to the customer. The wire cost of an empty array is trivial next to a
		// behaviour change in billing.
		req.Services = append(req.Services, billingclient.ServiceResources{
			ServiceID: es.ID,
			Resources: resources,
		})
	}
	return req, nil
}

// RemoteCharge is the cron entry point for the split charge flow: ask the billing service which
// profiles are ACTIVE, resolve each one's resources locally, and POST them back to be rated.
//
// Per-profile errors are logged and skipped rather than aborting the cycle — the same isolation the
// in-process loop and the RabbitMQ fan-out both provide, so one bad profile cannot stall the rest.
// The first error is returned so the run is still visibly unhealthy.
func RemoteCharge(ctx context.Context, resolver Resolver, client Charger, timeUnit string, now time.Time, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}

	ids, err := client.ActiveProfileIDs(ctx)
	if err != nil {
		return err
	}

	var firstErr error
	var charged, skipped int
	for _, id := range ids {
		req, err := resolver.ResolveByProfileID(ctx, id, timeUnit, now)
		if err != nil {
			log.Error("remote charge: resolve", "profileId", id, "timeUnit", timeUnit, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		resp, err := client.Charge(ctx, req)
		if err != nil {
			log.Error("remote charge: charge", "profileId", id, "timeUnit", timeUnit, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// A 200 does not mean a charge happened. The service answers "not charged" with a reason for
		// legitimate no-ops (the profile stopped being ACTIVE between resolution and rating) but ALSO
		// for misconfiguration (its engine disabled, or no billingConfiguration). Treating both as
		// success is how a deploy mistake turns into silently billing nothing, indefinitely, with a
		// green cron.
		if resp == nil || !resp.Charged {
			reason := "unknown"
			if resp != nil && resp.Reason != "" {
				reason = resp.Reason
			}
			skipped++
			log.Warn("remote charge: profile not charged", "profileId", id, "timeUnit", timeUnit, "reason", reason)
			continue
		}
		charged++
	}

	// Every profile the service itself listed as ACTIVE came back uncharged: that is not a run of
	// unlucky no-ops, it is the billing service refusing to bill. Surface it as a failed run so the
	// cron is visibly unhealthy rather than quietly producing no bills.
	if firstErr == nil && charged == 0 && skipped > 0 {
		return fmt.Errorf("remote charge: none of the %d active profiles were charged (last reason logged); "+
			"check the billing service's CLOUD_DB_URL and billingConfiguration", skipped)
	}
	log.Info("remote charge complete", "timeUnit", timeUnit, "charged", charged, "skipped", skipped)
	return firstErr
}

// compile-time proof that the concrete client satisfies the driver's interface.
var (
	_ Charger  = (*billingclient.Client)(nil)
	_ Resolver = (*Service)(nil)
)

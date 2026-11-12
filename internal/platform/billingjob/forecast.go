package billingjob

import (
	"context"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/billingresource"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// forecastTimeUnits are the cadences the charge job runs (minutelyCharge / hourlyCharge /
// monthlyCharge). A resource is rated against each one's rules, as each pass would.
var forecastTimeUnits = []string{pricing.TimeUnitMinute, pricing.TimeUnitHour, pricing.TimeUnitMonth}

// ForecastInput resolves and rates a profile's billable resources for the month-end forecast with
// the charge job's own steps, read-only:
//   - billing enabled, the profile ACTIVE (otherwise nothing accrues: an empty input);
//   - its ENABLED projects with services, each external service's cloud resources through the
//     provider registry (billingResources: eligibility, the stale-provider write-off, the rows
//     archived this month whose last interval is still to bill);
//   - each resource rated by the Engine against the price-plan rules ChargeRules selects for its
//     service and time unit, at `now`.
//
// A resource the charge does not bill now (NotEligibleForBilling) is left out; one the rules fail to
// rate is reported unrated (the charge would fail on it too). Nothing is written. More than
// billing.ForecastMaxResources billable resources → billing.ErrForecastTooLarge. The request path
// wraps this in billing.CachedForecaster (cache, singleflight, timeout).
func (s *Service) ForecastInput(ctx context.Context, profileID string, now time.Time) (billing.ForecastInput, error) {
	in := billing.ForecastInput{}
	now = now.UTC()
	enabled, err := s.billingEnabled(ctx)
	if err != nil || !enabled {
		return in, err
	}
	// Read-only load: FindByID may backfill a blank currency, and a GET must not write.
	profile, err := s.d.Billing.FindByIDReadOnly(ctx, profileID)
	if err != nil || profile == nil || profile.Status != billing.StatusActive {
		return in, err
	}
	projects, err := s.activeProjectsWithServices(ctx, profileID)
	if err != nil || len(projects) == 0 {
		return in, err
	}
	externalServices, err := s.d.ExternalServices.List(ctx)
	if err != nil {
		return in, err
	}
	bc := s.billingContext(ctx)
	in.Billing = bc
	cycleStart, _ := monthBounds(now)
	planIDs, includePublic := pricePlanConfig(profile)
	eng := s.d.Engine.At(now)
	seen := 0
	for i := range externalServices {
		if err := ctx.Err(); err != nil {
			return in, err
		}
		es := &externalServices[i]
		// The hour pass's resolution: the live rows plus those archived this month (a minute or hour
		// rule still bills their last interval; a month rule skips them, as its pass does, because
		// their end is behind `now`).
		resources, err := s.billingResources(ctx, bc, projects, es, pricing.TimeUnitHour, cycleStart, now)
		if err != nil {
			return in, err
		}
		if len(resources) == 0 {
			continue
		}
		if seen += len(resources); seen > billing.ForecastMaxResources {
			return billing.ForecastInput{}, billing.ErrForecastTooLarge
		}
		rules := map[string][]pricing.PricePlanRule{}
		for _, tu := range forecastTimeUnits {
			_, rs, err := s.d.Pricing.ChargeRules(ctx, planIDs, includePublic, es.ID, tu)
			if err != nil {
				return in, err
			}
			rules[tu] = rs
		}
		for _, res := range pricing.FromAPIResources(resources) {
			if res.NotEligibleForBilling {
				if res.SkipInterval {
					in.Stale = true
				}
				continue
			}
			fr := billing.ForecastResource{
				ResourceID: res.ResourceID, ResourceType: res.ResourceType, ProjectID: res.ProjectID,
				CreatedAt: res.CreatedAt, EndsAt: res.DeletedAt,
			}
			if fr.EndsAt == nil && res.ResourceType == "sandbox" {
				fr.EndsAt = s.sandboxPlannedEnd(ctx, res.ResourceID)
			}
			rated := true
			for _, tu := range forecastTimeUnits {
				if len(rules[tu]) == 0 {
					continue
				}
				results, err := eng.ApplyPricePlanRules(rules[tu], res, tu)
				if err != nil {
					rated = false
					break
				}
				if len(results) > 0 {
					fr.Rates = append(fr.Rates, billing.ForecastRate{TimeUnit: tu, Results: results})
				}
			}
			if !rated {
				in.Unrated = append(in.Unrated, billing.ForecastUnratedResource{ResourceType: res.ResourceType, ProjectID: res.ProjectID})
				continue
			}
			if len(fr.Rates) > 0 {
				in.Resources = append(in.Resources, fr)
			}
		}
	}
	return in, nil
}

// sandboxPlannedEnd reads a sandbox's planned end off its cloud row (nil when it has none or the
// row cannot be read: it is then projected to the end of the month, like any running resource).
func (s *Service) sandboxPlannedEnd(ctx context.Context, resourceID string) *time.Time {
	cr, err := s.d.Cloud.FindByID(ctx, resourceID)
	if err != nil || cr == nil || cr.Type != cloud.TypeSandbox {
		return nil
	}
	return billingresource.SandboxPlannedEnd(cr)
}

var _ billing.Forecaster = (*Service)(nil)

// Package billingjob is the charge cron driver: {minutely,hourly,
// monthly}Charge (load ACTIVE profiles + all externalServices + billingConfig, fan out per
// profile) folded together with the consumer + BillingJobService
// .charge{Minutely,Hourly,Monthly}BillingResource → chargeBillingResource. It runs
// in-process under a ShedLock (scheduler.RunLocked) instead of fanning a RabbitMQ message
// per profile — the per-profile unit of work is identical.
//
// The charge step reads the PostgreSQL CACHE (cloudResource + usageMetrics + pricePlan), NOT
// live cloud — so the whole driver is testcontainer-verifiable. Populating that cache is
// the SYNC providers + metrics job (live cloud, separate).
package billingjob

import (
	"context"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/billingresource"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// Deps are the collaborators the charge driver needs. Registry maps a CloudResourceType to
// its cloud→billing Provider (SERVER → billingresource.ServerProvider, etc.).
type Deps struct {
	Billing          *billing.Repo
	ExternalServices *externalservice.Service
	Projects         *project.Repo
	Orgs             *org.Repo
	Pricing          *pricing.Repo
	Engine           *pricing.Engine
	Cloud            *cloud.Repo
	Registry         map[string]billingresource.Provider
	Now              func() time.Time
}

type Service struct{ d Deps }

func New(d Deps) *Service {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{d: d}
}

// Charge is the cron entry point (one of minutely/hourly/monthly). It gates on
// isBillingEnabled, loads the ACTIVE profiles + every external service + the billing
// context, then charges each profile. Errors per profile are logged-and-skipped (the
// per-profile consumer isolates failures) so one bad profile can't stall the rest.
func (s *Service) Charge(ctx context.Context, timeUnit string, now time.Time) error {
	enabled, err := s.billingEnabled(ctx)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	profiles, err := s.d.Billing.FindByStatus(ctx, billing.StatusActive)
	if err != nil {
		return err
	}
	externalServices, err := s.d.ExternalServices.List(ctx)
	if err != nil {
		return err
	}
	bc := s.billingContext(ctx)
	cycleStart, cycleEnd := monthBounds(now)
	cycleTimestamp := truncateForTimeUnit(now, timeUnit)
	var firstErr error
	for i := range profiles {
		if err := s.chargeBillingResource(ctx, &profiles[i], bc, externalServices, timeUnit, cycleTimestamp, cycleStart, cycleEnd); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ActiveProfileIDs returns the ids of every ACTIVE profile — the work-list the RabbitMQ
// fan-out producer publishes (one message per id), gated on isBillingEnabled like Charge.
func (s *Service) ActiveProfileIDs(ctx context.Context) ([]string, error) {
	enabled, err := s.billingEnabled(ctx)
	if err != nil || !enabled {
		return nil, err
	}
	profiles, err := s.d.Billing.FindByStatus(ctx, billing.StatusActive)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(profiles))
	for i := range profiles {
		ids = append(ids, profiles[i].ID)
	}
	return ids, nil
}

// ChargeProfileByID charges ONE profile — the fan-out consumer's unit of work, identical to
// the in-process Charge loop body (the per-profile consumer). A non-ACTIVE
// or vanished profile is a no-op (it may have changed since the message was published).
func (s *Service) ChargeProfileByID(ctx context.Context, profileID, timeUnit string, now time.Time) error {
	enabled, err := s.billingEnabled(ctx)
	if err != nil || !enabled {
		return err
	}
	profile, err := s.d.Billing.FindByID(ctx, profileID)
	if err != nil {
		return err
	}
	if profile == nil || profile.Status != billing.StatusActive {
		return nil
	}
	externalServices, err := s.d.ExternalServices.List(ctx)
	if err != nil {
		return err
	}
	bc := s.billingContext(ctx)
	cycleStart, cycleEnd := monthBounds(now)
	cycleTimestamp := truncateForTimeUnit(now, timeUnit)
	return s.chargeBillingResource(ctx, profile, bc, externalServices, timeUnit, cycleTimestamp, cycleStart, cycleEnd)
}

// chargeBillingResource charges the billing resources for one profile: for
// each external service, gather the project-scoped billing resources, select the applicable
// price-plan rules, and apply them onto the locked current bill.
func (s *Service) chargeBillingResource(
	ctx context.Context,
	profile *billing.BillingProfile,
	bc pricing.BillingContext,
	externalServices []externalservice.ExternalService,
	timeUnit string,
	cycleTimestamp, cycleStart, cycleEnd time.Time,
) error {
	projects, err := s.activeProjectsWithServices(ctx, profile.ID)
	if err != nil {
		return err
	}
	planIDs, includePublic := pricePlanConfig(profile)
	for i := range externalServices {
		es := &externalServices[i]
		rc := pricing.RatingContext{TimeUnit: timeUnit, CycleTimestamp: cycleTimestamp}
		resources, err := s.billingResources(ctx, bc, projects, es.ID)
		if err != nil {
			return err
		}
		plans := pricing.SelectPricePlansForService(s.d.Pricing.PlanSource(ctx), planIDs, includePublic, es.ID)
		rules := pricing.ApplicableRules(plans, s.d.Pricing.RuleSource(ctx), timeUnit)
		adjust := s.billAdjuster(ctx, profile, plans, cycleStart, cycleEnd)
		// THE CHARGE SEAM: everything above is resolution (cloud cache → billable units, in the
		// billingapi wire contract); everything below is rating (price them onto the bill). When
		// billing moves out, resolution stays here and the wire resources are POSTed to the billing
		// service, which performs exactly this FromAPIResources + ChargeBillingResources step.
		if _, err := pricing.ChargeBillingResources(ctx, s.d.Pricing, s.d.Engine, rc, bc, profile.ID, rules, pricing.FromAPIResources(resources), cycleStart, cycleEnd, cycleTimestamp, profile.Currency, adjust); err != nil {
			return err
		}
	}
	return nil
}

// billAdjuster builds the after-rating bill adjustment closure (the applyPricePlans tail):
// savings-contract discounts for contracts covering the billing cycle + the price-adjustment rules
// of the service's price plans. Returns nil if neither applies (cheap no-op for the common case).
func (s *Service) billAdjuster(ctx context.Context, profile *billing.BillingProfile, plans []pricing.PricePlan, cycleStart, cycleEnd time.Time) func(*pricing.Bill) {
	savings := s.availableSavingsAdj(ctx, profile.ID, cycleStart, cycleEnd)
	planIDs := make([]string, 0, len(plans))
	for i := range plans {
		planIDs = append(planIDs, plans[i].ID)
	}
	rules, _ := s.d.Billing.PriceAdjustmentRulesByPricePlanIDs(ctx, planIDs)
	if len(savings) == 0 && len(rules) == 0 {
		return nil
	}
	// The catalog is produced by the resolution side in the billingapi wire contract; the engine
	// (rating side) takes its own types.
	catalog := pricing.FromAPIResourceTypes(billingresource.Catalog())
	return func(bill *pricing.Bill) {
		s.d.Engine.ApplySavingsContractDiscounts(bill, savings, catalog)
		s.d.Engine.ApplyPriceAdjustmentRules(bill, rules, catalog)
	}
}

// availableSavingsAdj adapts the profile's savings contracts that cover the whole billing cycle
// (listAvailableContractsByBillingProfileId: startDate ≤ cycleStart AND endDate ≥ cycleEnd) into
// the pricing-native discount inputs.
func (s *Service) availableSavingsAdj(ctx context.Context, bpID string, cycleStart, cycleEnd time.Time) []pricing.SavingsContractAdj {
	contracts, err := s.d.Billing.SavingsContractsByBillingProfile(ctx, bpID)
	if err != nil {
		return nil
	}
	out := make([]pricing.SavingsContractAdj, 0, len(contracts))
	for i := range contracts {
		c := &contracts[i]
		if c.StartDate == nil || c.EndDate == nil || c.StartDate.After(cycleStart) || c.EndDate.Before(cycleEnd) {
			continue
		}
		committed, rate := decimal.Zero, decimal.Zero
		if c.MonthlyCommittedAmount != nil {
			committed = *c.MonthlyCommittedAmount
		}
		if c.DiscountRate != nil {
			rate = *c.DiscountRate
		}
		targets := make([]pricing.AdjustmentTarget, 0, len(c.Targets))
		for _, t := range c.Targets {
			targets = append(targets, pricing.AdjustmentTarget{ResourceType: t.ResourceType, Filters: t.Filters})
		}
		out = append(out, pricing.SavingsContractAdj{
			ID: c.ID, SavingsPlanName: c.SavingsPlanName, MonthlyCommittedAmount: committed, DiscountRate: rate,
			PaidUpfront: c.PaidUpfront, StartDate: c.StartDate, EndDate: c.EndDate, Targets: targets,
		})
	}
	return out
}

// activeProjectsWithServices returns the profile's active projects with services: the profile's projects
// (direct + via its orgs) filtered to ENABLED ones that have at least one service.
func (s *Service) activeProjectsWithServices(ctx context.Context, billingProfileID string) ([]project.Project, error) {
	orgs, err := s.d.Orgs.FindAllByBillingProfileID(ctx, billingProfileID)
	if err != nil {
		return nil, err
	}
	orgIDs := make([]string, 0, len(orgs))
	for i := range orgs {
		orgIDs = append(orgIDs, orgs[i].ID)
	}
	all, err := s.d.Projects.AllByBillingProfile(ctx, billingProfileID, orgIDs)
	if err != nil {
		return nil, err
	}
	out := make([]project.Project, 0, len(all))
	for i := range all {
		if all[i].IsEnabled() && all[i].HasServices() {
			out = append(out, all[i])
		}
	}
	return out, nil
}

// billingResources builds the billing resources (project-scoped): for each
// project attached to the external service, dispatch its cloud resources through the
// type→Provider registry into priced BillingResources.
func (s *Service) billingResources(ctx context.Context, bc pricing.BillingContext, projects []project.Project, serviceID string) ([]*billingapi.BillingResource, error) {
	out := []*billingapi.BillingResource{}
	pbc := pricing.ToAPIBillingContext(bc)
	for i := range projects {
		p := &projects[i]
		if !p.HasService(serviceID) {
			continue
		}
		brs, err := billingresource.GetBillingResources(ctx, s.d.Cloud, s.d.Registry, p.ID, serviceID, pbc)
		if err != nil {
			return nil, err
		}
		out = append(out, brs...)
	}
	return out, nil
}

// billingEnabled reports whether billing is enabled. Open-source build (no
// gate: reduces to whether a billingConfiguration document exists.
func (s *Service) billingEnabled(ctx context.Context) (bool, error) {
	_, _, found, err := s.d.Billing.Configuration(ctx)
	if err != nil {
		return false, err
	}
	return found, nil
}

// billingContext builds the per-charge BillingContext. The configurable timeUnitLimits live
// on billingConfiguration.settings (not yet modeled); nil → pricing's defaults (30-day month).
func (s *Service) billingContext(_ context.Context) pricing.BillingContext {
	return pricing.BillingContext{}
}

func pricePlanConfig(p *billing.BillingProfile) (ids []string, includePublic bool) {
	if p.PricePlanConfig == nil {
		return nil, false
	}
	return p.PricePlanConfig.PricePlanIDs, p.PricePlanConfig.IncludePublicPricePlans
}

// monthBounds returns [first-of-month 00:00, first-of-next-month 00:00) in UTC — the billing
// cycle window (cycleStart matches ServerProvider.firstDayOfCurrentMonth so the usage-metrics
// lookup lines up).
func monthBounds(now time.Time) (time.Time, time.Time) {
	y, m, _ := now.UTC().Date()
	start := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// truncateForTimeUnit mirrors Instant.truncatedTo: MINUTE→minute, HOUR→hour, MONTH→day.
func truncateForTimeUnit(now time.Time, timeUnit string) time.Time {
	now = now.UTC()
	switch timeUnit {
	case pricing.TimeUnitMinute:
		return now.Truncate(time.Minute)
	case pricing.TimeUnitHour:
		return now.Truncate(time.Hour)
	case pricing.TimeUnitMonth:
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	default:
		return now
	}
}

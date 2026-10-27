package pricing

// Price-plan selection. Implemented as pure functions over small source interfaces so
// they are golden-testable now; the datastore-backed implementations of the interfaces
// (plan-by-id, the enabled public plans, and the rules for a plan and time unit)
// land with the persistence slice. The BillingProfile's pricePlanConfig is passed as
// (pricePlanIDs, includePublic) to keep this decoupled from the billing-profile type.

// PricePlanSource abstracts the two plan reads the selection needs.
type PricePlanSource interface {
	// FindByID returns a plan by id.
	FindByID(id string) (PricePlan, bool)
	// PublicPricePlans returns the enabled PUBLIC plans.
	PublicPricePlans() []PricePlan
}

// PricePlanRuleSource abstracts the rule read.
type PricePlanRuleSource interface {
	RulesByPricePlanIDAndTimeUnit(pricePlanID, timeUnit string) []PricePlanRule
}

// SelectPricePlans selects the profile's plans: when the
// profile scopes plans (non-empty pricePlanIDs), take those that resolve + are
// enabled + SCOPED, plus the public plans when includePublic; otherwise just the
// public plans.
func SelectPricePlans(src PricePlanSource, pricePlanIDs []string, includePublic bool) []PricePlan {
	plans := []PricePlan{}
	if len(pricePlanIDs) > 0 {
		for _, id := range pricePlanIDs {
			if pp, ok := src.FindByID(id); ok && pp.Enabled && pp.AccessMode == AccessScoped {
				plans = append(plans, pp)
			}
		}
		if includePublic {
			plans = append(plans, src.PublicPricePlans()...)
		}
	} else {
		plans = append(plans, src.PublicPricePlans()...)
	}
	return plans
}

// SelectPricePlansForService selects the profile's plans for a service:
// the profile's plans narrowed to those scoped to the external service (no service
// providers ⇒ matches all; else by serviceId).
func SelectPricePlansForService(src PricePlanSource, pricePlanIDs []string, includePublic bool, externalServiceID string) []PricePlan {
	all := SelectPricePlans(src, pricePlanIDs, includePublic)
	out := make([]PricePlan, 0, len(all))
	for _, pp := range all {
		if pp.IsServiceProviderScoped(externalServiceID) {
			out = append(out, pp)
		}
	}
	return out
}

// ApplicableRules flat-maps the selected
// plans to their rules for the given time unit (the engine re-filters by timeUnit,
// so this is idempotent).
func ApplicableRules(plans []PricePlan, ruleSrc PricePlanRuleSource, timeUnit string) []PricePlanRule {
	out := []PricePlanRule{}
	for _, pp := range plans {
		out = append(out, ruleSrc.RulesByPricePlanIDAndTimeUnit(pp.ID, timeUnit)...)
	}
	return out
}

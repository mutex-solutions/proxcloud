package pricing

import "context"

// ChargeRules uses error-preserving reads for durable billing. The legacy pure
// selector adapters deliberately collapse DB errors to empty results; recording
// a successful receipt after such a read would permanently undercharge a cycle.
func (r *Repo) ChargeRules(ctx context.Context, ids []string, includePublic bool, service, unit string) ([]PricePlan, []PricePlanRule, error) {
	plans := []PricePlan{}
	for _, id := range ids {
		p, err := r.FindPricePlanByID(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		if p != nil && p.Enabled && p.AccessMode == AccessScoped {
			plans = append(plans, *p)
		}
	}
	if len(ids) == 0 || includePublic {
		public, err := r.PublicPricePlans(ctx)
		if err != nil {
			return nil, nil, err
		}
		plans = append(plans, public...)
	}
	selected, rules := []PricePlan{}, []PricePlanRule{}
	for _, p := range plans {
		if !p.IsServiceProviderScoped(service) {
			continue
		}
		selected = append(selected, p)
		rs, err := r.RulesByPricePlanIDAndTimeUnit(ctx, p.ID, unit)
		if err != nil {
			return nil, nil, err
		}
		rules = append(rules, rs...)
	}
	return selected, rules, nil
}

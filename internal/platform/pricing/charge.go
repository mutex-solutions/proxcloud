package pricing

import (
	"context"
	"time"
)

// ChargeBillingResources performs the per-(profile,
// service) charge step: lock/create the current bill, rate each billing resource through
// the Engine + accumulate it onto the bill via SaveChargingToBill, then persist.
//
// The pure pieces (Engine.ApplyPricePlanRules, SaveChargingToBill) are golden-tested; this
// is the I/O orchestration tying them to the locked bill. `adjust` (nil-safe) runs after the
// rating loop, before save — it applies the
// savings-contract discount + price-adjustment rules to the bill. cycleStart/End bound the
// billing month; `now` is the charge instant (rc.CycleTimestamp).
func ChargeBillingResources(
	ctx context.Context,
	repo *Repo,
	eng *Engine,
	rc RatingContext,
	bc BillingContext,
	billingProfileID string,
	rules []PricePlanRule,
	resources []*BillingResource,
	cycleStart, cycleEnd, now time.Time,
	currency string,
	adjust func(*Bill),
) (*Bill, error) {
	bill, err := repo.GetCurrentBill(ctx, billingProfileID, cycleStart, cycleEnd, now, currency)
	if err != nil {
		return nil, err
	}
	for _, res := range resources {
		results, err := eng.ApplyPricePlanRules(rules, res, rc.TimeUnit)
		if err != nil {
			return nil, err
		}
		SaveChargingToBill(rc, bc, bill, res, results, rc.CycleTimestamp)
	}
	if adjust != nil {
		adjust(bill)
	}
	return repo.SaveBill(ctx, bill)
}

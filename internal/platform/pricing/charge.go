package pricing

import (
	"context"
	"errors"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"time"
)

// ErrChargeReconciliation prevents an async backlog from reopening an issued
// month or acknowledging a missing closing interval. Operators must reconcile;
// silently changing an issued/paid bill is never a queue retry side effect.
var ErrChargeReconciliation = errors.New("billing cycle requires reconciliation (issued bill or missing closing bill)")

// ChargeBillingResources performs the per-(profile, service) charge step: under the profile lock,
// get or create the cycle's OPEN bill, rate each billing resource through the Engine, accumulate it
// onto the bill via SaveChargingToBill, then persist.
//
// The lock is what makes "one OPEN bill per cycle" and "no lost charge" true: the crons (hourly and
// monthly share a spec and have different ShedLock names), the debug triggers, the fan-out consumers
// and other replicas all arrive here at once. Nothing in the section touches a cloud or a gateway:
// rating and `adjust` are pure, and the caller resolved its inputs beforehand.
//
// The pure pieces (Engine.ApplyPricePlanRules, SaveChargingToBill) are golden-tested; this
// is the I/O orchestration tying them to the bill. `adjust` (nil-safe) runs after the
// rating loop, before save — it applies the
// savings-contract discount + price-adjustment rules to the bill. cycleStart/End bound the
// billing month; `now` is the charge instant (rc.CycleTimestamp). A `now` at cycleEnd is the pass
// closing that cycle's last interval: it tops up the OPEN bill and never opens one.
// An issued cycle or a missing nonempty closing bill requires reconciliation,
// not a success receipt or a fresh bill with reset month-long watermarks.
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
	if err := repo.ensureBills(ctx); err != nil {
		return nil, err
	}
	var saved *Bill
	err := repo.db.WithTx(ctx, func(ctx context.Context) error {
		if err := repo.db.LockXact(ctx, ProfileLockKey(billingProfileID)); err != nil {
			return err
		}
		var issued Bill
		found, err := repo.bills.FindOne(ctx, pgdoc.M{"billingProfileId": billingProfileID, "status": pgdoc.M{"$in": []string{string(BillStatusSent), string(BillStatusPaid)}}, "billingCycle.startDate": pgdoc.M{"$gte": cycleStart, "$lt": cycleEnd}}, &issued)
		if err != nil {
			return err
		}
		if found {
			return ErrChargeReconciliation
		}
		bill, err := repo.openBill(ctx, billingProfileID, cycleStart, cycleEnd)
		if err != nil {
			return err
		}
		if bill == nil {
			if !now.Before(cycleEnd) {
				if len(resources) > 0 {
					return ErrChargeReconciliation
				}
				return nil
			}
			if bill, err = repo.createBill(ctx, billingProfileID, cycleStart, cycleEnd, currency); err != nil {
				return err
			}
		}
		for _, res := range resources {
			if res.CreatedAt != nil && res.CreatedAt.After(rc.CycleTimestamp) {
				continue
			}
			results, err := eng.At(rc.CycleTimestamp).ApplyPricePlanRules(rules, res, rc.TimeUnit)
			if err != nil {
				return err
			}
			SaveChargingToBill(rc, bc, bill, res, results, rc.CycleTimestamp)
		}
		if adjust != nil {
			adjust(bill)
		}
		saved, err = repo.saveBill(ctx, bill)
		return err
	})
	if err != nil {
		return nil, err
	}
	return saved, nil
}

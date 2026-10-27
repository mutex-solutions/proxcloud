package admin

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
)

// savingsplan_repo.go holds the SavingsPlan-typed store helpers the admin mutations need. The
// generic crud.go helpers operate on pgdoc.M; SavingsPlan carries decimal money (maxAmount /
// startAmount / discount) that must round-trip through the decimal codec, so these
// read/write the typed billing.SavingsPlan value. `_id` is a plain String id.

// InsertSavingsPlan saves a NEW plan with a freshly-generated hex String `_id`.
// The id is set on the returned value for the response.
func (r *Repo) InsertSavingsPlan(ctx context.Context, collection string, plan billing.SavingsPlan) (*billing.SavingsPlan, error) {
	plan.ID = pgdoc.NewID()
	if _, err := r.c(collection).InsertOne(ctx, plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

// SavingsPlanByID loads a plan by id: the typed plan, or (nil,nil) when absent.
func (r *Repo) SavingsPlanByID(ctx context.Context, collection, id string) (*billing.SavingsPlan, error) {
	var plan billing.SavingsPlan
	found, err := r.c(collection).Get(ctx, id, &plan)
	if err != nil || !found {
		return nil, err
	}
	return &plan, nil
}

// ReplaceSavingsPlan saves an EXISTING plan: id-preserving replace.
func (r *Repo) ReplaceSavingsPlan(ctx context.Context, collection, id string, plan billing.SavingsPlan) error {
	plan.ID = id
	_, err := r.c(collection).Replace(ctx, id, plan)
	return err
}

// DeleteSavingsPlan deletes a plan by id → deleted count.
func (r *Repo) DeleteSavingsPlan(ctx context.Context, collection, id string) (int64, error) {
	ok, err := r.c(collection).DeleteByID(ctx, id)
	if err != nil {
		return 0, err
	}
	if ok {
		return 1, nil
	}
	return 0, nil
}

// AvailableSavingsPlans loads the plans marked available: the typed plans
// (never nil). The caller applies isEligibleForBillingProfile.
func (r *Repo) AvailableSavingsPlans(ctx context.Context, collection string) ([]billing.SavingsPlan, error) {
	out := []billing.SavingsPlan{}
	if err := r.c(collection).Find(ctx, pgdoc.M{"available": true}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

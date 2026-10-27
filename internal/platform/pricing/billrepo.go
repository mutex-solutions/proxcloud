package pricing

import (
	"context"
	"errors"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// lockDuration is how far ahead getAndLockBill pushes lockedAt (now+1m).
const lockDuration = time.Minute

// ExistsCurrentMonthBill reports whether an OPEN bill exists for the
// profile whose billing cycle started this month.
func (r *Repo) ExistsCurrentMonthBill(ctx context.Context, billingProfileID string, cycleStart time.Time) (bool, error) {
	return r.bills.Exists(ctx, pgdoc.M{
		"billingProfileId":       billingProfileID,
		"status":                 string(BillStatusOpen),
		"billingCycle.startDate": pgdoc.M{"$gte": cycleStart},
	})
}

// CreateBill inserts a fresh OPEN bill for the current cycle. lockedAt
// starts at `now` so getAndLockBill (lockedAt ≤ now) can immediately claim it. invoiceCurrency =
// the billing profile's currency — without it
// the client Bill History renders the net amount as "<n> undefined".
func (r *Repo) CreateBill(ctx context.Context, billingProfileID string, cycleStart, cycleEnd, now time.Time, currency string) (*Bill, error) {
	cs, ce, ln := cycleStart, cycleEnd, now
	bill := &Bill{
		Status:           BillStatusOpen,
		BillingProfileID: billingProfileID,
		InvoiceCurrency:  currency,
		BillingCycle:     &BillBillingCycle{StartDate: &cs, EndDate: &ce},
		Items:            []BillItem{},
		LockedAt:         &ln,
		CreatedAt:        &ln,
		UpdatedAt:        &ln,
	}
	id, err := r.bills.InsertOne(ctx, bill)
	if err != nil {
		return nil, err
	}
	bill.ID = id
	return bill, nil
}

// GetAndLockBill atomically claims the current OPEN bill
// (status OPEN, cycle started this month, lockedAt ≤ now) by pushing lockedAt to now+1m, and
// return the PRE-image (the bill as it was). Returns (nil,nil) when no claimable bill exists
// (already locked by another worker) so the caller can retry.
func (r *Repo) GetAndLockBill(ctx context.Context, billingProfileID string, cycleStart, now time.Time) (*Bill, error) {
	if err := r.ensureBills(ctx); err != nil {
		return nil, err
	}
	filter := pgdoc.M{
		"billingProfileId":       billingProfileID,
		"status":                 string(BillStatusOpen),
		"billingCycle.startDate": pgdoc.M{"$gte": cycleStart},
		"lockedAt":               pgdoc.M{"$lte": now},
	}
	var bill *Bill
	err := r.db.WithTx(ctx, func(tc context.Context) error {
		var b Bill
		id, found, err := r.bills.FindOneForUpdate(tc, filter, &b)
		if err != nil || !found {
			return err
		}
		if _, err := r.bills.SetByID(tc, id, pgdoc.M{"lockedAt": now.Add(lockDuration)}, nil); err != nil {
			return err
		}
		bill = &b // pre-image: decoded before the lockedAt push
		return nil
	})
	if err != nil {
		return nil, err
	}
	return bill, nil
}

// GetCurrentBill ensures an OPEN bill exists for the
// current cycle, then claims it. Bounded retry (rather than an unbounded sleep loop
// on a held lock under contention) — returns an error rather than spinning forever.
func (r *Repo) GetCurrentBill(ctx context.Context, billingProfileID string, cycleStart, cycleEnd, now time.Time, currency string) (*Bill, error) {
	// The bill LOCK is a wall-clock lease (lockedAt ≤ lockNow → claim, push to lockNow+1m). It must
	// use real time, NOT the charge's `now` (which the caller truncates to the cadence boundary —
	// minute/hour/month). A finer cadence (minutely) locks the bill at realNow+1m; a coarser one
	// (hourly) truncates `now` to the top of the hour, which is in the PAST, so `lockedAt ≤ now` could
	// never match a still-leased bill → spurious "could not acquire bill lock". cycleStart/cycleEnd
	// (the month bounds) still scope the bill; only the lock comparison uses wall-clock.
	lockNow := time.Now().UTC()
	exists, err := r.ExistsCurrentMonthBill(ctx, billingProfileID, cycleStart)
	if err != nil {
		return nil, err
	}
	if !exists {
		if _, err := r.CreateBill(ctx, billingProfileID, cycleStart, cycleEnd, lockNow, currency); err != nil {
			return nil, err
		}
	}
	for attempt := 0; attempt < 5; attempt++ {
		bill, err := r.GetAndLockBill(ctx, billingProfileID, cycleStart, lockNow)
		if err != nil {
			return nil, err
		}
		if bill != nil {
			return bill, nil
		}
		lockNow = lockNow.Add(lockDuration) // a stale lock expires; advance past it on retry
	}
	return nil, errors.New("pricing: could not acquire bill lock")
}

// SaveBill saves a bill by _id (insert when new, full replace otherwise).
// Mirrors billRepository.save.
func (r *Repo) SaveBill(ctx context.Context, bill *Bill) (*Bill, error) {
	now := time.Now().UTC()
	bill.UpdatedAt = &now
	if bill.ID == "" {
		id, err := r.bills.InsertOne(ctx, bill)
		if err != nil {
			return nil, err
		}
		bill.ID = id
		return bill, nil
	}
	if _, err := r.bills.Replace(ctx, bill.ID, bill); err != nil {
		return nil, err
	}
	return bill, nil
}

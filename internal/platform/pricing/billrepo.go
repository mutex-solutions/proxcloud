package pricing

import (
	"context"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// ProfileLockKey is the advisory lock on one profile's money. Anything that reads a bill or a credit
// balance and writes it back holds it (pgdoc.DB.LockXact inside WithTx): a bill is replaced whole, so a
// second writer silently undoes the first. The name is older than the charge path taking it, and a
// replica on the previous release still locks this exact key.
func ProfileLockKey(profileID string) string { return "billing-profile-credit:" + profileID }

// openBill returns the profile's OPEN bill of this cycle, nil when there is none. The caller holds
// ProfileLockKey. Oldest first, so a site with duplicates from before the lock (docs/billing.md) keeps
// charging one of them: an item first seen on the other one would start its watermark over.
func (r *Repo) openBill(ctx context.Context, billingProfileID string, cycleStart, cycleEnd time.Time) (*Bill, error) {
	var b Bill
	found, err := r.bills.FindOne(ctx, pgdoc.M{
		"billingProfileId":       billingProfileID,
		"status":                 string(BillStatusOpen),
		"billingCycle.startDate": pgdoc.M{"$gte": cycleStart, "$lt": cycleEnd},
	}, &b, pgdoc.Sort(pgdoc.Asc("_id")))
	if err != nil || !found {
		return nil, err
	}
	return &b, nil
}

// createBill inserts a fresh OPEN bill for the cycle. invoiceCurrency = the billing profile's
// currency; without it the client Bill History renders the net amount as "<n> undefined".
func (r *Repo) createBill(ctx context.Context, billingProfileID string, cycleStart, cycleEnd time.Time, currency string) (*Bill, error) {
	cs, ce, now := cycleStart, cycleEnd, time.Now().UTC()
	bill := &Bill{
		Status:           BillStatusOpen,
		BillingProfileID: billingProfileID,
		InvoiceCurrency:  currency,
		BillingCycle:     &BillBillingCycle{StartDate: &cs, EndDate: &ce},
		Items:            []BillItem{},
		CreatedAt:        &now,
		UpdatedAt:        &now,
	}
	id, err := r.bills.InsertOne(ctx, bill)
	if err != nil {
		return nil, err
	}
	bill.ID = id
	return bill, nil
}

// saveBill replaces the bill by _id.
func (r *Repo) saveBill(ctx context.Context, bill *Bill) (*Bill, error) {
	now := time.Now().UTC()
	bill.UpdatedAt = &now
	if _, err := r.bills.Replace(ctx, bill.ID, bill); err != nil {
		return nil, err
	}
	return bill, nil
}

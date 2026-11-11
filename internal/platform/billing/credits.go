package billing

import (
	"context"
	"errors"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// Repo access for the credit-settlement pay path: the bill, the
// spendable account-credit balance docs (accountCredit) + promotional credits, and the
// write-backs the settlement mutates. Account credits are ordered createdAt DESC
// (the settle query sorts by createdAt, newest first).

// BillByID loads a bill by _id (nil when absent / malformed id).
func (r *Repo) BillByID(ctx context.Context, id string) (*pricing.Bill, error) {
	var b pricing.Bill
	found, err := r.bills.Get(ctx, id, &b)
	if err != nil || !found {
		return nil, err
	}
	return &b, nil
}

// SaveBillDoc replaces a bill by _id (the id column is preserved; the codec strips
// _id from the stored body). Mirrors pricing.Repo.SaveBill.
func (r *Repo) SaveBillDoc(ctx context.Context, bill *pricing.Bill) error {
	return replaceByID(ctx, r.bills, bill.ID, bill)
}

// AccountCreditsByProfile returns the profile's spendable account credits in insertion
// order — the order the settle path consumes them
// (filtered by billing-profile id, NO sort — natural order). ids are time-prefixed +
// counter-tailed, so _id ASC reproduces insertion order deterministically. NOTE: the
// DESC-sorted by-profile lookup is a DIFFERENT query used elsewhere; the settle
// path must NOT sort by date DESC, else multi-credit bills exhaust a different credit
// first → different applied sub-docs/residuals.
func (r *Repo) AccountCreditsByProfile(ctx context.Context, billingProfileID string) ([]*pricing.AccountCredit, error) {
	var out []pricing.AccountCredit
	err := r.acctCredits.Find(ctx, pgdoc.M{"billingProfileId": billingProfileID}, &out,
		pgdoc.Sort(pgdoc.Asc("_id")))
	if err != nil {
		return nil, err
	}
	return ptrs(out), nil
}

// PromotionalCreditCandidates returns the profile's NON-EXPIRED promotional credits with a
// positive remaining balance (the take candidates). The filter is
// remaining amount > 0 AND expiration date after now — an expired promo must NOT be
// applied to a bill. (Minted credits carry the NO_EXPIRATION sentinel 9999-01-01, which passes
// `> now`; same filter as AvailablePromotionalTotal.) _id ASC keeps consumption order
// deterministic (insertion order, like the unsorted original).
func (r *Repo) PromotionalCreditCandidates(ctx context.Context, billingProfileID string, now time.Time) ([]*pricing.PromotionalCredit, error) {
	var all []pricing.PromotionalCredit
	err := r.promoCredit.Find(ctx, pgdoc.M{
		"billingProfileId": billingProfileID,
		"expirationDate":   pgdoc.M{"$gt": now},
	}, &all, pgdoc.Sort(pgdoc.Asc("_id")))
	if err != nil {
		return nil, err
	}
	out := make([]*pricing.PromotionalCredit, 0, len(all))
	for i := range all {
		if all[i].RemainingAmount != nil && all[i].RemainingAmount.IsPositive() {
			out = append(out, &all[i])
		}
	}
	return out, nil
}

// WithProfileLock runs fn in one transaction holding the profile's credit lock. Everything that reads
// a credit balance or a bill's unpaid amount and then writes it goes through here: two payers otherwise
// both read the same state, and one spends the same balance twice or overwrites the other's payment.
// Keep gateway calls out where you can, it blocks that profile's other credit operations.
//
// Reads inside fn must hit tables that exist: an undefined table aborts the transaction, so the ones
// a fresh install may not have yet are created first.
func (r *Repo) WithProfileLock(ctx context.Context, profileID string, fn func(ctx context.Context) error) error {
	for _, t := range []*pgdoc.Store{r.acctCredits, r.promoCredit, r.counters, r.bills, r.collects} {
		if err := t.Ensure(ctx); err != nil {
			return err
		}
	}
	db := r.acctCredits.DB()
	return db.WithTx(ctx, func(ctx context.Context) error {
		if err := db.LockXact(ctx, pricing.ProfileLockKey(profileID)); err != nil {
			return err
		}
		return fn(ctx)
	})
}

// SaveAccountCredit / SavePromotionalCredit persist a credit whose balance the settlement
// consumed (replace by _id).
func (r *Repo) SaveAccountCredit(ctx context.Context, ac *pricing.AccountCredit) error {
	return replaceByID(ctx, r.acctCredits, ac.ID, ac)
}

// PendingAccountCreditTransactions / PendingCollectTransactions back the transaction scanner
// (status PENDING with createdAt between now-24h and now-20min — exclusive bounds): stuck
// PENDING transactions old enough to not race the live flow but young enough to still matter.
func (r *Repo) PendingAccountCreditTransactions(ctx context.Context, from, to time.Time) ([]AccountCreditTransaction, error) {
	return findTyped[AccountCreditTransaction](ctx, r.credits, pgdoc.M{
		"status": "PENDING", "createdAt": pgdoc.M{"$gt": from, "$lt": to},
	})
}

// StuckAccountCreditTransactions lists deposits left PROCESSING since before `before`: a settle
// claims the row for seconds, so these are a pod that died mid-settle. Nothing else picks them up.
func (r *Repo) StuckAccountCreditTransactions(ctx context.Context, before time.Time) ([]AccountCreditTransaction, error) {
	return findTyped[AccountCreditTransaction](ctx, r.credits, pgdoc.M{"status": "PROCESSING", "updatedAt": pgdoc.M{"$lt": before}})
}

func (r *Repo) PendingCollectTransactions(ctx context.Context, from, to time.Time) ([]pricing.CollectTransaction, error) {
	return findTyped[pricing.CollectTransaction](ctx, r.collects, pgdoc.M{
		"status": string(pricing.CollectTransactionStatusPending), "createdAt": pgdoc.M{"$gt": from, "$lt": to},
	})
}

// PendingCollectForBill returns the bill's PENDING collect transactions, however old: a charge whose outcome is
// unknown. A new charge for the bill must not start while one exists, or the customer may be charged twice.
func (r *Repo) PendingCollectForBill(ctx context.Context, billID string) ([]pricing.CollectTransaction, error) {
	return findTyped[pricing.CollectTransaction](ctx, r.collects, pgdoc.M{
		"billId": billID, "status": string(pricing.CollectTransactionStatusPending),
	})
}

// CreditCardByToken finds a profile's stored card by its gateway token (nil if absent) — the
// card-confirm callback's duplicate guard.
func (r *Repo) CreditCardByToken(ctx context.Context, bpID, tokenID string) (*CreditCard, error) {
	var c CreditCard
	found, err := r.cards.FindOne(ctx, pgdoc.M{"billingProfileId": bpID, "tokenId": tokenID}, &c)
	if err != nil || !found {
		return nil, err
	}
	return &c, nil
}

// UpdatePendingCollectTransaction sets the status and message of a charge still PENDING. A settle that
// won meanwhile is left alone, which a replace of the row read earlier would undo.
func (r *Repo) UpdatePendingCollectTransaction(ctx context.Context, id string, status pricing.CollectTransactionStatus, message string) error {
	_, err := r.collects.SetFieldsOne(ctx, pgdoc.M{"_id": id, "status": string(pricing.CollectTransactionStatusPending)},
		pgdoc.M{"status": string(status), "errorMessage": message, "updatedAt": time.Now().UTC()}, nil)
	return err
}

// SaveCollectTransaction upserts a collect transaction (insert on a blank id, else replace
// by id — same pattern as SaveAccountCreditTransaction).
func (r *Repo) SaveCollectTransaction(ctx context.Context, t *pricing.CollectTransaction) (*pricing.CollectTransaction, error) {
	now := time.Now().UTC()
	if t.CreatedAt == nil {
		t.CreatedAt = &now
	}
	t.UpdatedAt = &now
	if t.ID == "" {
		id, err := r.collects.InsertOne(ctx, t)
		if err != nil {
			return nil, err
		}
		t.ID = id
		return t, nil
	}
	if _, err := r.collects.Replace(ctx, t.ID, t); err != nil {
		return nil, err
	}
	return t, nil
}

// AccountCreditByID loads one spendable account-credit by id (the refund void path). nil if absent.
func (r *Repo) AccountCreditByID(ctx context.Context, id string) (*pricing.AccountCredit, error) {
	var ac pricing.AccountCredit
	found, err := r.acctCredits.Get(ctx, id, &ac)
	if err != nil || !found {
		return nil, err
	}
	return &ac, nil
}

// MarkAccountCreditRefunded voids a refunded deposit's credit WITHOUT deleting it: the remaining
// balance is zeroed and refundedAt stamped, initialAmount stays for the audit trail. Every balance
// reader sums Amount (AccountCreditTotal, creditBalance) and the settle path skips zero credits
// (pricing.SettleAccountCredits), so a voided credit is excluded exactly as a deleted one was.
func MarkAccountCreditRefunded(ac *pricing.AccountCredit, now time.Time) {
	zero := decimal.Zero
	ac.Amount = &zero
	ac.RefundedAt = &now
	ac.UpdatedAt = &now
}

// RestoreAccountCredit undoes MarkAccountCreditRefunded when the gateway could not complete the
// refund: the balance the customer had before the refund comes back and refundedAt is cleared.
func RestoreAccountCredit(ac *pricing.AccountCredit, amount decimal.Decimal, now time.Time) {
	ac.Amount = &amount
	ac.RefundedAt = nil
	ac.UpdatedAt = &now
}

// RefundedAccountCreditTransactions lists REFUNDED deposits updated after `since` — the scanner
// filters those whose gateway refund is still pending.
func (r *Repo) RefundedAccountCreditTransactions(ctx context.Context, since time.Time) ([]AccountCreditTransaction, error) {
	return findTyped[AccountCreditTransaction](ctx, r.credits, pgdoc.M{"status": "REFUNDED", "updatedAt": pgdoc.M{"$gt": since}})
}

// CreateAccountCredit inserts a NEW spendable AccountCredit (the deposit credit)
// and back-fills its generated id.
func (r *Repo) CreateAccountCredit(ctx context.Context, ac *pricing.AccountCredit) error {
	id, err := r.acctCredits.InsertOne(ctx, ac)
	if err != nil {
		return err
	}
	ac.ID = id
	return nil
}

// CreateAccountCreditForTxn mints the credit of one money movement. Its id is the transaction's, so a
// settle retried after a failure further on hits the primary key instead of minting a second credit.
func (r *Repo) CreateAccountCreditForTxn(ctx context.Context, txnID string, ac *pricing.AccountCredit) error {
	ac.ID = txnID
	if _, err := r.acctCredits.InsertOne(ctx, ac); err != nil && !pgdoc.IsDup(err) {
		return err
	}
	return nil
}

func (r *Repo) SavePromotionalCredit(ctx context.Context, pc *pricing.PromotionalCredit) error {
	return replaceByID(ctx, r.promoCredit, pc.ID, pc)
}

// replaceByID full-replaces a document by id (the codec strips _id from the stored
// body, so the id column is preserved without clear/restore dances).
func replaceByID(ctx context.Context, col *pgdoc.Store, id string, doc any) error {
	if id == "" {
		return errors.New("billing: save requires an id")
	}
	_, err := col.Replace(ctx, id, doc)
	return err
}

func ptrs[T any](s []T) []*T {
	out := make([]*T, len(s))
	for i := range s {
		out[i] = &s[i]
	}
	return out
}

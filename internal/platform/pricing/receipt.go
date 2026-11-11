package pricing

import (
	"context"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// PrepareCharges warms financial tables before workers start a transaction.
// Failures are NOT cached forever; a canceled preflight can be retried safely.
func (r *Repo) PrepareCharges(ctx context.Context) error {
	if err := r.ensureBills(ctx); err != nil {
		return err
	}
	r.ensureMu.Lock()
	defer r.ensureMu.Unlock()
	if r.receiptsReady {
		return nil
	}
	if err := r.db.C("billingChargeReceipt").Ensure(ctx); err != nil {
		return err
	}
	r.receiptsReady = true
	return nil
}

// WithChargeReceipt joins WithTx, serializes the profile's money using the SAME
// legacy profile lock, and commits ALL service mutations with one durable receipt.
// fn is strictly database/pure computation: never a cloud, HTTP, SMTP or gateway call.
// A receipt survives queue-row retention, so a crash after business commit before
// job acknowledgment, or an operator replay, cannot charge the original cycle twice.
func (r *Repo) WithChargeReceipt(ctx context.Context, profile, unit string, cutoff time.Time, fn func(context.Context) error) error {
	if err := r.PrepareCharges(ctx); err != nil {
		return err
	}
	receipts := r.db.C("billingChargeReceipt")
	key := profile + "|" + unit + "|" + cutoff.UTC().Format(time.RFC3339Nano)
	return r.db.WithTx(ctx, func(ctx context.Context) error {
		if err := r.db.LockXact(ctx, ProfileLockKey(profile)); err != nil {
			return err
		}
		var receipt pgdoc.M
		found, err := receipts.Get(ctx, key, &receipt)
		if err != nil || found {
			return err
		}
		if err := fn(ctx); err != nil {
			return err
		}
		_, err = receipts.InsertOne(ctx, pgdoc.M{"_id": key, "billingProfileId": profile, "timeUnit": unit, "cycleAt": cutoff.UTC()})
		return err
	})
}

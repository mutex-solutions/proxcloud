package durablejobs

import (
	"context"
	"time"
)

// BillingBacklogTx is used ONLY under the bill sender's primary profile lock.
// No job-row lock (worker order is job row -> profile); unresolved dead letters
// block issuance too. It reads primary and the caller's transaction, never stats.
// A late publish after issuance is rejected by the pricing issued-cycle guard.
func (s *Store) BillingBacklogTx(ctx context.Context, profile string, start, end time.Time) (bool, error) {
	tx, err := s.db.Tx(ctx)
	if err != nil {
		return false, err
	}
	var pending bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM durable_job WHERE kind=$1 AND state IN ('pending','running','dead') AND payload->>'profileId'=$2 AND (
	 ((payload->>'timeUnit')='month' AND (payload->>'cycleAt')::timestamptz >=$3 AND (payload->>'cycleAt')::timestamptz <$4) OR
	 ((payload->>'timeUnit') IN ('minute','hour') AND (payload->>'cycleAt')::timestamptz >$3 AND (payload->>'cycleAt')::timestamptz <=$4)))`, Billing, profile, start, end).Scan(&pending)
	return pending, err
}

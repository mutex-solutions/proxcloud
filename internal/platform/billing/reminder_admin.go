package billing

import (
	"context"
	"time"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// WithSavingsContractMutation is the admin raw-document bridge. Its DB-only
// callback preserves existing DTO/validation/unknown fields while joining the
// shared contract->reminder->outbox transaction. Cancel old queued selections
// on expiry/status/profile change or deletion; rendering-only/no-op edits keep
// frozen selections and future windows. DISPATCHING/UNKNOWN remain immutable.
func (r *Repo) WithSavingsContractMutation(ctx context.Context, id string, fn func(context.Context) error) error {
	return r.withReminderContract(ctx, id, func(tc context.Context, c *SavingsContract) error {
		if c == nil {
			return httpx.NotFound("Savings contract not found")
		}
		if err := fn(tc); err != nil {
			return err
		}
		after, err := r.SavingsContractByID(tc, id)
		if err != nil {
			return err
		}
		endChanged := after != nil && ((c.EndDate == nil) != (after.EndDate == nil))
		if after != nil && c.EndDate != nil && after.EndDate != nil {
			endChanged = reminderGeneration(*c.EndDate) != reminderGeneration(*after.EndDate)
		}
		if after == nil || endChanged || after.Status != c.Status || after.BillingProfileID != c.BillingProfileID {
			if err := r.cancelReminderRowsTx(tc, id, c.EndDate, time.Now().UTC()); err != nil {
				return err
			}
			return r.cancelSavingsExpiredTx(tc, id)
		}
		// Rendering-only/no-op edits keep existing frozen messages and leave
		// unselected windows eligible; only their future snapshots see new fields.
		return nil
	})
}

package mailoutbox

import (
	"context"

	"github.com/google/uuid"
)

// CancelPendingTx is the business cancellation boundary, not a resend/admin API.
// IDs must come from persisted reminder decisions under contract->reminder locks.
// It joins that SAME primary transaction and fences claims before intent. A row
// already DISPATCHING/UNKNOWN/accepted/dead cannot be unsent or rewritten here.
func (s *Store) CancelPendingTx(ctx context.Context, ids []string) error {
	tx, err := s.db.Tx(ctx)
	if err != nil {
		return err
	}
	if len(ids) > MaxRecipients {
		return ErrInvalid
	}
	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil {
			return ErrInvalid
		}
	}
	_, err = tx.Exec(ctx, `WITH selected AS (
	 SELECT id FROM mail_outbox WHERE id=ANY($1::uuid[]) ORDER BY id FOR UPDATE
	) UPDATE mail_outbox o SET state='expired',reason='cancelled',epoch=epoch+1,owner=NULL,lease_until=NULL
	 FROM selected WHERE o.id=selected.id AND o.state IN ('pending','claimed','blocked_config','suppressed','no_recipient')`, ids)
	return err
}

package pgdoc

import (
	"context"
	"errors"
)

// RequireNoTransaction protects infrastructure boundaries that must independently
// COMMIT before external I/O. Reject ANY pgdoc-owned transaction, including one
// belonging to a different DB; a failed DB.Tx ownership check is not proof that
// there is no transaction. This does not start or join a transaction.
func RequireNoTransaction(ctx context.Context) error {
	if ctx.Value(txKey{}) != nil || ctx.Value(txOwnerKey{}) != nil {
		return errors.New("pgdoc: independent operation requires a transaction-free context")
	}
	return nil
}

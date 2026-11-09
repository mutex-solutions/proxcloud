package cloud

import "fmt"

// InventoryUntrustedError means absence cannot be distinguished from lost visibility.
// Reconcile must leave the scope untouched; operators repair the configured credential
// and retry, rather than deleting rows (and the access/billing state they carry).
type InventoryUntrustedError struct {
	Type, ExternalID, Reason string
	Err                      error
}

func (e *InventoryUntrustedError) Error() string {
	return fmt.Sprintf("untrusted %s inventory (%s): %s; keeping cached inventory, restore provider visibility and retry", e.Type, e.ExternalID, e.Reason)
}

func (e *InventoryUntrustedError) Unwrap() error { return e.Err }

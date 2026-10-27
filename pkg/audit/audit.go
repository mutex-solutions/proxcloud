// Package audit captures an audit trail for mutating service paths.
// Every mutating service path wraps its work in Audited(...) so a before/after
// snapshot diff is captured and written to the auditEvent collection. This is
// currently a no-op stub establishing the extension point; the differ + async
// write are not yet wired (a CI lint asserts every mutator is wrapped).
package audit

import "context"

// Snapshotable is implemented by auditable entities.
type Snapshotable interface {
	AuditSnapshot() map[string]any
}

// Audited runs fn, and (once wired) records the before/after diff as an audit event.
func Audited[T any](ctx context.Context, action string, fn func() (T, error)) (T, error) {
	// TODO: capture before snapshot, diff against after, enqueue auditEvent.
	return fn()
}

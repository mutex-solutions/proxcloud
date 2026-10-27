package admin

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// CountAdminPermissionsByRole counts by role name — the number of
// adminPermission documents currently assigned the given role name. Used by role deletion
// to block deleting a role that still has users (→ 400 "Cannot delete role '%s' because %d user(s)
// are assigned to it"). The adminPermission collection keys role by the `role` field.
func (r *Repo) CountAdminPermissionsByRole(ctx context.Context, roleName string) (int64, error) {
	return r.col.Count(ctx, pgdoc.M{"role": roleName})
}

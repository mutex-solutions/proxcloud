package rbac

// Built-in role names. BILLING exists as a role name but is
// NOT a static permission role (RolePermissions only knows OWNER/ADMIN/MEMBER);
// a BILLING member resolves to no static permissions.
const (
	RoleOwner   = "OWNER"
	RoleAdmin   = "ADMIN"
	RoleMember  = "MEMBER"
	RoleBilling = "BILLING"
)

// static role → granted permission patterns. Verbatim from RolePermissions.
var staticRolePermissions = map[string][]string{
	RoleOwner: {"organization:*", "project:*", "billing_profile:*"},
	RoleAdmin: {
		"organization:read", "organization:update", "organization:manage_members",
		"organization:manage_roles", "project:*", "billing_profile:*",
	},
	RoleMember: {
		"organization:read", "project:create", "project:cloud_resource:*",
		"billing_profile:read", "billing_profile:read_invoices",
		"billing_profile:download_invoices", "billing_profile:read_transactions",
	},
}

// IsStaticRole reports whether the role is one of OWNER/ADMIN/MEMBER.
func IsStaticRole(role string) bool {
	_, ok := staticRolePermissions[role]
	return ok
}

// RolePermissions returns a static role's raw granted patterns (the shape the
// org DTO's currentUserPermissions exposes), or empty for unknown roles.
func RolePermissions(role string) []string {
	if p, ok := staticRolePermissions[role]; ok {
		// copy so callers can't mutate the table
		out := make([]string, len(p))
		copy(out, p)
		return out
	}
	return []string{}
}

// RoleHasPermission reports whether a static role grants the permission key.
func RoleHasPermission(role, key string) bool {
	return Matches(RolePermissions(role), key)
}

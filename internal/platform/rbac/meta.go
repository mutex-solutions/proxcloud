package rbac

import "strings"

// ResourceType returns the first ":"-segment of a permission key
// (key.split(":")[0]). E.g. "project:cloud_resource:read" → "project".
func ResourceType(key string) string {
	return strings.SplitN(key, ":", 2)[0]
}

// validResourceTypes is the set of first-segment resource types across all
// permissions ({organization, project, billing_profile}) — used to validate
// "resource:*" custom-role patterns.
var validResourceTypes = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range AllPermissions {
		m[ResourceType(k)] = true
	}
	return m
}()

// validPermissionKeys is the exact-key set.
var validPermissionKeys = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range AllPermissions {
		m[k] = true
	}
	return m
}()

// IsValidPermission reports whether a pattern is valid:
// "*" is valid; an exact key is valid; "resource:*" is valid iff resource is a
// known first-segment resource type. Note "project:cloud_resource:*" is INVALID
// (its prefix "project:cloud_resource" is not a first-segment resource type).
func IsValidPermission(p string) bool {
	switch {
	case p == "*":
		return true
	case validPermissionKeys[p]:
		return true
	case strings.HasSuffix(p, ":*"):
		return validResourceTypes[p[:len(p)-2]]
	default:
		return false
	}
}

// PermissionMeta is the {key,description,resourceType} shape the roles
// /permissions endpoint exposes.
type PermissionMeta struct {
	Key          string `json:"key"`
	Description  string `json:"description"`
	ResourceType string `json:"resourceType"`
}

// AllPermissionMeta returns metadata for every permission, in declaration order.
func AllPermissionMeta() []PermissionMeta {
	out := make([]PermissionMeta, 0, len(AllPermissions))
	for _, k := range AllPermissions {
		out = append(out, PermissionMeta{Key: k, Description: descriptions[k], ResourceType: ResourceType(k)})
	}
	return out
}

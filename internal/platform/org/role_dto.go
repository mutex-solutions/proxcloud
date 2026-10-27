package org

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
)

// RoleDto describes an organization role. Static (built-in) roles set builtIn
// + omit description/createdAt/updatedAt when null;
// permissions + expandedPermissions are always present arrays.
type RoleDto struct {
	ID                  string
	Name                string
	Description         string
	Permissions         []string
	ExpandedPermissions []string
	CreatedAt           *time.Time
	UpdatedAt           *time.Time
	BuiltIn             bool
}

// MarshalJSON omits description/createdAt/updatedAt when
// empty; permissions/expandedPermissions always present (sorted for stability).
func (d RoleDto) MarshalJSON() ([]byte, error) {
	perms := d.Permissions
	if perms == nil {
		perms = []string{}
	}
	exp := d.ExpandedPermissions
	if exp == nil {
		exp = []string{}
	}
	var desc *string
	if d.Description != "" {
		desc = &d.Description
	}
	return json.Marshal(struct {
		ID                  string     `json:"id"`
		Name                string     `json:"name"`
		Description         *string    `json:"description,omitempty"`
		Permissions         []string   `json:"permissions"`
		ExpandedPermissions []string   `json:"expandedPermissions"`
		CreatedAt           *time.Time `json:"createdAt,omitempty"`
		UpdatedAt           *time.Time `json:"updatedAt,omitempty"`
		BuiltIn             bool       `json:"builtIn"`
	}{
		ID: d.ID, Name: d.Name, Description: desc, Permissions: perms,
		ExpandedPermissions: exp, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, BuiltIn: d.BuiltIn,
	})
}

// roleDtoFromRole builds the DTO for a custom role (builtIn=false).
func roleDtoFromRole(r *Role) RoleDto {
	perms := append([]string(nil), r.Permissions...)
	sort.Strings(perms)
	return RoleDto{
		ID:                  r.ID,
		Name:                r.Name,
		Description:         r.Description,
		Permissions:         perms,
		ExpandedPermissions: rbac.ExpandPatterns(r.Permissions),
		CreatedAt:           r.CreatedAt,
		UpdatedAt:           r.UpdatedAt,
		BuiltIn:             false,
	}
}

// roleDtoFromStatic builds the DTO for a built-in role (id == name, builtIn=true,
// no description/timestamps).
func roleDtoFromStatic(name string) RoleDto {
	patterns := rbac.RolePermissions(name)
	sort.Strings(patterns)
	return RoleDto{
		ID:                  name,
		Name:                name,
		Permissions:         patterns,
		ExpandedPermissions: rbac.ExpandPatterns(patterns),
		BuiltIn:             true,
	}
}

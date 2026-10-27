package org

import "time"

// Role is a document in the "roleDefinition" collection:
// a custom, per-organization permission set. Permissions is the raw pattern set
// (e.g. ["organization:read","project:*"]). Built-in roles (OWNER/ADMIN/MEMBER)
// are NOT stored here — they live in the rbac kernel.
type Role struct {
	ID             string     `json:"id,omitempty"`
	OrganizationID string     `json:"organizationId"`
	Name           string     `json:"name"`
	Description    string     `json:"description,omitempty"`
	Permissions    []string   `json:"permissions"`
	CreatedAt      *time.Time `json:"createdAt,omitempty"`
	UpdatedAt      *time.Time `json:"updatedAt,omitempty"`
}

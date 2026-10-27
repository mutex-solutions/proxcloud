// Package org implements the Organization slice: org CRUD + members + RBAC.
package org

import "time"

// Organization is a document in the `organization` collection.
type Organization struct {
	ID               string         `json:"id,omitempty"`
	Name             string         `json:"name"`
	Description      string         `json:"description,omitempty"`
	BillingProfileID string         `json:"billingProfileId,omitempty"`
	CustomInfo       map[string]any `json:"customInfo"`
	CreatedAt        *time.Time     `json:"createdAt,omitempty"`
	UpdatedAt        *time.Time     `json:"updatedAt,omitempty"`
}

// Member is a document in the `organization_members` collection. Role is roles[0].
type Member struct {
	ID             string     `json:"id,omitempty"`
	OrganizationID string     `json:"organizationId"`
	Sub            string     `json:"sub"`
	Roles          []string   `json:"roles"`
	CreatedAt      *time.Time `json:"createdAt,omitempty"`
	UpdatedAt      *time.Time `json:"updatedAt,omitempty"`
}

// Role returns roles[0] (the member's effective role), or "" if none.
func (m *Member) Role() string {
	if len(m.Roles) > 0 {
		return m.Roles[0]
	}
	return ""
}

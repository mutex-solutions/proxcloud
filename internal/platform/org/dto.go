package org

import (
	"context"
	"sort"
	"time"
)

// OrganizationDto carries Organization plus a few extras.
// billingProfile (full object) is never set by the controller → omitted.
type OrganizationDto struct {
	ID                     string         `json:"id,omitempty"`
	Name                   string         `json:"name"`
	Description            string         `json:"description,omitempty"`
	BillingProfileID       string         `json:"billingProfileId,omitempty"`
	CustomInfo             map[string]any `json:"customInfo"`
	CreatedAt              *time.Time     `json:"createdAt,omitempty"`
	UpdatedAt              *time.Time     `json:"updatedAt,omitempty"`
	CurrentUserRole        string         `json:"currentUserRole,omitempty"`
	ProjectCount           int64          `json:"projectCount"`
	MemberCount            int64          `json:"memberCount"`
	CurrentUserPermissions []string       `json:"currentUserPermissions"`
}

// MemberDto describes an organization member.
type MemberDto struct {
	Sub       string `json:"sub"`
	FirstName string `json:"firstName,omitempty"`
	LastName  string `json:"lastName,omitempty"`
	Email     string `json:"email,omitempty"`
	Role      string `json:"role,omitempty"`
}

// toDto builds the org DTO with the per-user computed fields.
func (h *Handler) toDto(ctx context.Context, o *Organization, userSub string) OrganizationDto {
	ci := o.CustomInfo
	if ci == nil {
		ci = map[string]any{}
	}
	perms := h.policy.UserPermissionKeys(ctx, userSub, o.ID)
	if perms == nil {
		perms = []string{}
	}
	sort.Strings(perms)
	members, _ := h.repo.Members(ctx, o.ID)
	projectCount, _ := h.repo.CountProjects(ctx, o.ID)
	return OrganizationDto{
		ID:                     o.ID,
		Name:                   o.Name,
		Description:            o.Description,
		BillingProfileID:       o.BillingProfileID,
		CustomInfo:             ci,
		CreatedAt:              o.CreatedAt,
		UpdatedAt:              o.UpdatedAt,
		CurrentUserRole:        h.policy.UserRole(ctx, o.ID, userSub),
		ProjectCount:           projectCount,
		MemberCount:            int64(len(members)),
		CurrentUserPermissions: perms,
	}
}

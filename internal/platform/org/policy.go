package org

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Policy is the org policy checker: resolves a user's role in an org and checks
// permissions via the RBAC kernel (static roles) or the roleDefinition
// collection (custom roles).
type Policy struct{ repo *Repo }

func NewPolicy(repo *Repo) *Policy { return &Policy{repo: repo} }

func (p *Policy) role(ctx context.Context, orgID, sub string) (string, bool) {
	m, err := p.repo.FindMember(ctx, orgID, sub)
	if err != nil || m == nil {
		return "", false
	}
	return m.Role(), true
}

// HasPermission reports whether the user's role in the org grants the perm key.
// Static roles resolve via the rbac kernel; custom roles via the roleDefinition
// patterns.
func (p *Policy) HasPermission(ctx context.Context, sub, orgID, permKey string) bool {
	role, ok := p.role(ctx, orgID, sub)
	if !ok {
		return false
	}
	if rbac.IsStaticRole(role) {
		return rbac.RoleHasPermission(role, permKey)
	}
	perms, _ := p.repo.RolePermissions(ctx, orgID, role)
	return rbac.Matches(perms, permKey)
}

// UserPermissionKeys returns the RAW permission patterns for the user's role
// (what the org DTO's currentUserPermissions exposes): static role patterns, or
// the custom role's permission set. Empty if no role.
func (p *Policy) UserPermissionKeys(ctx context.Context, sub, orgID string) []string {
	role, ok := p.role(ctx, orgID, sub)
	if !ok {
		return []string{}
	}
	if rbac.IsStaticRole(role) {
		return rbac.RolePermissions(role)
	}
	perms, _ := p.repo.RolePermissions(ctx, orgID, role)
	if perms == nil {
		perms = []string{}
	}
	return perms
}

func (p *Policy) IsMember(ctx context.Context, orgID, sub string) bool {
	_, ok := p.role(ctx, orgID, sub)
	return ok
}

func (p *Policy) IsOwner(ctx context.Context, orgID, sub string) bool {
	role, ok := p.role(ctx, orgID, sub)
	return ok && role == rbac.RoleOwner
}

func (p *Policy) IsAdmin(ctx context.Context, orgID, sub string) bool {
	role, ok := p.role(ctx, orgID, sub)
	return ok && (role == rbac.RoleAdmin || role == rbac.RoleOwner)
}

func (p *Policy) UserRole(ctx context.Context, orgID, sub string) string {
	role, _ := p.role(ctx, orgID, sub)
	return role
}

// RequirePermission returns a 403 *HTTPError if the user lacks the permission.
func (p *Policy) RequirePermission(ctx context.Context, sub, orgID, permKey string) error {
	if !p.HasPermission(ctx, sub, orgID, permKey) {
		return httpx.Forbidden("You do not have permission to perform this action: " + rbac.Description(permKey))
	}
	return nil
}

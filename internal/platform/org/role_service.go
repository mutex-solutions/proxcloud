package org

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// roleNamePattern is the allowed role-name pattern.
var roleNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// RoleService is the custom-role business layer.
// Audit logging is deferred to the audit pipeline.
type RoleService struct{ repo *Repo }

func NewRoleService(repo *Repo) *RoleService { return &RoleService{repo: repo} }

// Create validates + persists a new custom role (name uppercased; reserved/pattern
// checks; permissions validated + non-empty; unique per org).
func (s *RoleService) Create(ctx context.Context, orgID, name, description string, permissions []string) (*Role, error) {
	normalized := strings.ToUpper(name)
	if err := validateRoleName(normalized); err != nil {
		return nil, err
	}
	if err := validateRolePermissions(permissions); err != nil {
		return nil, err
	}
	exists, err := s.repo.ExistsRoleByOrgAndName(ctx, orgID, normalized)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, httpx.BadRequest(fmt.Sprintf("A role with name '%s' already exists in this organization", normalized))
	}
	return s.repo.InsertRole(ctx, &Role{
		OrganizationID: orgID, Name: normalized, Description: description, Permissions: permissions,
	})
}

// Update sets description and/or permissions (each only when provided). Org-scoped: a role id from
// another org is invisible (404), mirroring Delete.
func (s *RoleService) Update(ctx context.Context, roleID, orgID string, description *string, permissions []string) (*Role, error) {
	role, err := s.repo.FindRoleByID(ctx, roleID)
	if err != nil {
		return nil, err
	}
	if err := roleInOrg(role, orgID); err != nil {
		return nil, err
	}
	if permissions != nil {
		if err := validateRolePermissions(permissions); err != nil {
			return nil, err
		}
		role.Permissions = permissions
	}
	if description != nil {
		role.Description = *description
	}
	if err := s.repo.SaveRole(ctx, role); err != nil {
		return nil, err
	}
	return role, nil
}

// Delete removes a role; 404 if missing or not owned by the org.
func (s *RoleService) Delete(ctx context.Context, roleID, orgID string) error {
	role, err := s.repo.FindRoleByID(ctx, roleID)
	if err != nil {
		return err
	}
	if err := roleInOrg(role, orgID); err != nil {
		return err
	}
	return s.repo.DeleteRole(ctx, roleID)
}

// roleInOrg returns NotFound when the role is missing or owned by a different org — the org-scope
// guard shared by Update/Delete so a role id from another org is never mutated.
func roleInOrg(role *Role, orgID string) error {
	if role == nil || role.OrganizationID != orgID {
		return httpx.NotFound("Organization role not found")
	}
	return nil
}

func (s *RoleService) ListByOrg(ctx context.Context, orgID string) ([]Role, error) {
	return s.repo.RolesByOrg(ctx, orgID)
}

// GetByID returns a custom role scoped to the org, or 404 "Role not found".
func (s *RoleService) GetByID(ctx context.Context, orgID, roleID string) (*Role, error) {
	roles, err := s.repo.RolesByOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	for i := range roles {
		if roles[i].ID == roleID {
			return &roles[i], nil
		}
	}
	return nil, httpx.NotFound("Role not found")
}

func validateRoleName(name string) error {
	if rbac.IsStaticRole(name) {
		return httpx.BadRequest(fmt.Sprintf("Cannot use reserved role name '%s'", name))
	}
	if !roleNamePattern.MatchString(name) {
		return httpx.BadRequest("Role name must start with a letter and contain only uppercase letters, digits, and underscores")
	}
	return nil
}

func validateRolePermissions(permissions []string) error {
	if len(permissions) == 0 {
		return httpx.BadRequest("Permissions must not be empty")
	}
	for _, p := range permissions {
		if !rbac.IsValidPermission(p) {
			return httpx.BadRequest(fmt.Sprintf("Invalid permission: '%s'", p))
		}
	}
	return nil
}

package project

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Policy is the project-side permission checker. Org-level permission resolution is
// delegated to the org Policy; project-level checks add the rule that a
// project OWNER is treated as an ADMIN for permission purposes (even if their
// org role is MEMBER).
type Policy struct{ org *org.Policy }

func NewPolicy(orgPolicy *org.Policy) *Policy { return &Policy{org: orgPolicy} }

// RequireOrgPermission gates an org-level permission (e.g. project:create is
// granted at the org level).
func (p *Policy) RequireOrgPermission(ctx context.Context, sub, orgID, permKey string) error {
	return p.org.RequirePermission(ctx, sub, orgID, permKey)
}

// RequireProjectPermission: a project OWNER always has ADMIN-level permissions;
// otherwise fall back to the user's org-level permission. Denies with the
// project-scoped 403 message.
func (p *Policy) RequireProjectPermission(ctx context.Context, sub string, proj *Project, permKey string) error {
	if p.hasProjectPermission(ctx, sub, proj, permKey) {
		return nil
	}
	return httpx.Forbidden("You do not have permission to perform this action on this project: " + rbac.Description(permKey))
}

func (p *Policy) hasProjectPermission(ctx context.Context, sub string, proj *Project, permKey string) bool {
	if proj == nil || sub == "" || permKey == "" {
		return false
	}
	if proj.IsUserOwner(sub) {
		return rbac.RoleHasPermission(rbac.RoleAdmin, permKey)
	}
	return p.org.HasPermission(ctx, sub, proj.OrganizationID, permKey)
}

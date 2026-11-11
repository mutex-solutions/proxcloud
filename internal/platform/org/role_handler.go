package org

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// RoleHandler serves the custom-role endpoints under
// /api/v1/organizations/{id}/roles. The {id}
// param name matches the org handler's so chi shares the param node.
type RoleHandler struct {
	roleSvc *RoleService
	orgSvc  *Service
	policy  *Policy
	users   *user.Repo
	audit   *audit.Service
}

func NewRoleHandler(roleSvc *RoleService, orgSvc *Service, policy *Policy, users *user.Repo, a *audit.Service) *RoleHandler {
	return &RoleHandler{roleSvc: roleSvc, orgSvc: orgSvc, policy: policy, users: users, audit: a}
}

// roleAudit emits an ORGANIZATION_ROLE event.
func (h *RoleHandler) roleAudit(sub, displayName, action, orgID, roleID, roleName string) {
	ev := audit.ClientUserEvent(sub, displayName)
	ev.EventContext = audit.ContextOrganization
	ev.Action = action
	ev.ResourceType = audit.ResourceOrganizationRole
	ev.ResourceID = roleID
	ev.ResourceDisplayName = roleName
	ev.OrganizationID = orgID
	ev.Outcome = audit.OutcomeSuccess
	h.audit.LogAsync(ev)
}

func (h *RoleHandler) Routes(r chi.Router) {
	r.Get("/organizations/{id}/roles", h.listRoles)
	r.Get("/organizations/{id}/roles/permissions", h.listPermissions)
	r.Get("/organizations/{id}/roles/{roleId}", h.getRole)
	r.Post("/organizations/{id}/roles", h.createRole)
	r.Put("/organizations/{id}/roles/{roleId}", h.updateRole)
	r.Delete("/organizations/{id}/roles/{roleId}", h.deleteRole)
}

func (h *RoleHandler) fail(w http.ResponseWriter, err error) {
	httpx.WriteError(w, err)
}

// org loads the initialized user + the org (member-gated) + enforces a permission,
// returning (user, orgID, ok). On failure it has already written the response.
func (h *RoleHandler) org(w http.ResponseWriter, r *http.Request, permKey string) (*user.User, string, bool) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		h.fail(w, err)
		return nil, "", false
	}
	id := chi.URLParam(r, "id")
	if _, err := h.orgSvc.GetOrganizationForUser(r.Context(), id, u.Sub); err != nil {
		h.fail(w, err)
		return nil, "", false
	}
	if err := h.policy.RequirePermission(r.Context(), u.Sub, id, permKey); err != nil {
		h.fail(w, err)
		return nil, "", false
	}
	return u, id, true
}

func (h *RoleHandler) listRoles(w http.ResponseWriter, r *http.Request) {
	_, orgID, ok := h.org(w, r, rbac.OrganizationRead)
	if !ok {
		return
	}
	dtos := []RoleDto{
		roleDtoFromStatic(rbac.RoleOwner),
		roleDtoFromStatic(rbac.RoleAdmin),
		roleDtoFromStatic(rbac.RoleMember),
	}
	custom, err := h.roleSvc.ListByOrg(r.Context(), orgID)
	if err != nil {
		h.fail(w, err)
		return
	}
	for i := range custom {
		dtos = append(dtos, roleDtoFromRole(&custom[i]))
	}
	httpx.List(w, dtos)
}

func (h *RoleHandler) listPermissions(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.org(w, r, rbac.OrganizationRead); !ok {
		return
	}
	httpx.List(w, rbac.AllPermissionMeta())
}

func (h *RoleHandler) getRole(w http.ResponseWriter, r *http.Request) {
	_, orgID, ok := h.org(w, r, rbac.OrganizationRead)
	if !ok {
		return
	}
	role, err := h.roleSvc.GetByID(r.Context(), orgID, chi.URLParam(r, "roleId"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.OK(w, roleDtoFromRole(role))
}

func (h *RoleHandler) createRole(w http.ResponseWriter, r *http.Request) {
	u, orgID, ok := h.org(w, r, rbac.OrganizationManageRoles)
	if !ok {
		return
	}
	var req struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Permissions []string `json:"permissions"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	role, err := h.roleSvc.Create(r.Context(), orgID, req.Name, req.Description, req.Permissions)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.roleAudit(u.Sub, u.FullName(), audit.ActionCreate, orgID, role.ID, role.Name)
	httpx.OK(w, roleDtoFromRole(role))
}

func (h *RoleHandler) updateRole(w http.ResponseWriter, r *http.Request) {
	u, orgID, ok := h.org(w, r, rbac.OrganizationManageRoles)
	if !ok {
		return
	}
	var req struct {
		Description *string  `json:"description"`
		Permissions []string `json:"permissions"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	role, err := h.roleSvc.Update(r.Context(), chi.URLParam(r, "roleId"), orgID, req.Description, req.Permissions)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.roleAudit(u.Sub, u.FullName(), audit.ActionUpdate, role.OrganizationID, role.ID, role.Name)
	httpx.OK(w, roleDtoFromRole(role))
}

func (h *RoleHandler) deleteRole(w http.ResponseWriter, r *http.Request) {
	u, orgID, ok := h.org(w, r, rbac.OrganizationManageRoles)
	if !ok {
		return
	}
	roleID := chi.URLParam(r, "roleId")
	name := ""
	if role, _ := h.roleSvc.GetByID(r.Context(), orgID, roleID); role != nil {
		name = role.Name
	}
	if err := h.roleSvc.Delete(r.Context(), roleID, orgID); err != nil {
		h.fail(w, err)
		return
	}
	h.roleAudit(u.Sub, u.FullName(), audit.ActionDelete, orgID, roleID, name)
	httpx.OK(w, "Successful operation")
}

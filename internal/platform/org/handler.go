package org

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

type Handler struct {
	svc            *Service
	policy         *Policy
	repo           *Repo
	users          *user.Repo
	audit          *audit.Service
	projectMembers ProjectMemberAdder // nil-safe; set post-construction (avoids the org→project cycle)
}

// ProjectMemberAdder propagates a new organization member onto the org's projects.
// Implemented by project.Service, injected via a
// setter (org must not import project — import cycle).
type ProjectMemberAdder interface {
	AddMemberToOrgProjects(ctx context.Context, orgID, userSub string, projectIDs []string) error
}

func NewHandler(svc *Service, policy *Policy, repo *Repo, users *user.Repo, a *audit.Service) *Handler {
	return &Handler{svc: svc, policy: policy, repo: repo, users: users, audit: a}
}

// SetProjectMemberAdder wires the project-membership propagation (called from main.go after the
// project service is constructed).
func (h *Handler) SetProjectMemberAdder(p ProjectMemberAdder) { h.projectMembers = p }

// Routes registers the organization endpoints under the /api/v1 group.
// The static /self-service segment precedes the /{id} param route (chi matches static first).
func (h *Handler) Routes(r chi.Router) {
	r.Get("/organizations", h.list)
	r.Get("/organizations/self-service", h.selfService)
	r.Post("/organizations", h.create)
	r.Get("/organizations/{id}", h.get)
	r.Put("/organizations/{id}", h.update)
	r.Delete("/organizations/{id}", h.delete)
	r.Post("/organizations/{id}/member", h.addMember)
	r.Delete("/organizations/{id}/member/{sub}", h.removeMember)
	r.Put("/organizations/{id}/member/{sub}/role", h.updateMemberRole)
	r.Get("/organizations/{id}/members", h.getMembers)
}

// fail writes a typed *HTTPError envelope, else a 500.
func (h *Handler) fail(w http.ResponseWriter, err error) {
	if !httpx.WriteError(w, err) {
		httpx.Err(w, http.StatusInternalServerError, 500, "internal.error")
	}
}

// principal loads the initialized User (400 if not).
func (h *Handler) principal(w http.ResponseWriter, r *http.Request) (*user.User, bool) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		h.fail(w, err)
		return nil, false
	}
	return u, true
}

// selfService reports whether the caller may self-create an organization under the
// platform org-provisioning quota — the client onboarding gates its create form on this
// (limit 0 + enabled = operator-only mode, the form hides and shows a contact note).
func (h *Handler) selfService(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	can, err := h.svc.CanCreateOrganization(r.Context(), u.Sub)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"canCreateOrganization": can})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	orgs, err := h.svc.GetOrganizationsForUser(r.Context(), u.Sub)
	if err != nil {
		h.fail(w, err)
		return
	}
	dtos := make([]OrganizationDto, 0, len(orgs))
	for i := range orgs {
		dtos = append(dtos, h.toDto(r.Context(), &orgs[i], u.Sub))
	}
	httpx.List(w, dtos)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	o, err := h.svc.CreateOrganization(r.Context(), u, req.Name, req.Description)
	if err != nil {
		h.fail(w, err)
		return
	}
	ev := audit.ClientUserEvent(u.Sub, u.FullName())
	ev.EventContext = audit.ContextOrganization
	ev.Action = audit.ActionCreate
	ev.ResourceType = audit.ResourceOrganization
	ev.ResourceID = o.ID
	ev.ResourceDisplayName = o.Name
	ev.OrganizationID = o.ID
	ev.Outcome = audit.OutcomeSuccess
	h.audit.LogAsync(ev)
	httpx.OK(w, h.toDto(r.Context(), o, u.Sub))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	o, err := h.svc.GetOrganizationForUser(r.Context(), chi.URLParam(r, "id"), u.Sub)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.OK(w, h.toDto(r.Context(), o, u.Sub))
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := h.svc.GetOrganizationForUser(r.Context(), id, u.Sub); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.policy.RequirePermission(r.Context(), u.Sub, id, rbac.OrganizationUpdate); err != nil {
		h.fail(w, err)
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	o, err := h.svc.UpdateOrganization(r.Context(), id, u.Sub, req.Name, req.Description)
	if err != nil {
		h.fail(w, err)
		return
	}
	ev := audit.ClientUserEvent(u.Sub, u.FullName())
	ev.EventContext = audit.ContextOrganization
	ev.Action = audit.ActionUpdate
	ev.ResourceType = audit.ResourceOrganization
	ev.ResourceID = o.ID
	ev.ResourceDisplayName = o.Name
	ev.OrganizationID = o.ID
	ev.Outcome = audit.OutcomeSuccess
	h.audit.LogAsync(ev)
	httpx.OK(w, h.toDto(r.Context(), o, u.Sub))
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	o, err := h.svc.GetOrganizationForUser(r.Context(), id, u.Sub)
	if err != nil {
		h.fail(w, err)
		return
	}
	if err := h.policy.RequirePermission(r.Context(), u.Sub, id, rbac.OrganizationDelete); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.svc.DeleteOrganization(r.Context(), id, u.Sub); err != nil {
		h.fail(w, err)
		return
	}
	ev := audit.ClientUserEvent(u.Sub, u.FullName())
	ev.EventContext = audit.ContextOrganization
	ev.Action = audit.ActionDelete
	ev.ResourceType = audit.ResourceOrganization
	ev.ResourceID = id
	ev.ResourceDisplayName = o.Name
	ev.OrganizationID = id
	ev.Outcome = audit.OutcomeSuccess
	h.audit.LogAsync(ev)
	httpx.OK(w, "Successful operation")
}

func (h *Handler) addMember(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := h.svc.GetOrganizationForUser(r.Context(), id, u.Sub); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.policy.RequirePermission(r.Context(), u.Sub, id, rbac.OrganizationManageMembers); err != nil {
		h.fail(w, err)
		return
	}
	var req struct {
		Email      string   `json:"email"`
		Role       string   `json:"role"`
		ProjectIDs []string `json:"projectIds"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	// Assigning a privileged static role (OWNER/ADMIN) is a role-management action: require
	// organization:manage_roles too, else a manage_members-only actor could escalate a user to
	// OWNER/ADMIN.
	if isPrivilegedRole(req.Role) {
		if err := h.policy.RequirePermission(r.Context(), u.Sub, id, rbac.OrganizationManageRoles); err != nil {
			h.fail(w, err)
			return
		}
	}
	member, err := h.users.FindByEmail(r.Context(), req.Email)
	if err != nil {
		h.fail(w, err)
		return
	}
	if member == nil {
		h.fail(w, httpx.BadRequest("User with email "+req.Email+" not found"))
		return
	}
	if _, err := h.repo.AddMember(r.Context(), id, member.Sub, req.Role); err != nil {
		h.fail(w, err)
		return
	}
	// Propagate the new member onto the org's projects (projectIds==null → all
	// org projects, else the validated subset).
	if h.projectMembers != nil {
		if err := h.projectMembers.AddMemberToOrgProjects(r.Context(), id, member.Sub, req.ProjectIDs); err != nil {
			h.fail(w, err)
			return
		}
	}
	o, _ := h.svc.GetOrganization(r.Context(), id)
	h.memberAudit(u, o, id, audit.ActionAddMember, memberSnapshot(member, "role", req.Role))
	httpx.OK(w, h.toDto(r.Context(), o, u.Sub))
}

// memberAudit emits an ORGANIZATION member-management event.
func (h *Handler) memberAudit(u *user.User, o *Organization, orgID, action string, metadata map[string]any) {
	ev := audit.ClientUserEvent(u.Sub, u.FullName())
	ev.EventContext = audit.ContextOrganization
	ev.Action = action
	ev.ResourceType = audit.ResourceOrganization
	ev.ResourceID = orgID
	if o != nil {
		ev.ResourceDisplayName = o.Name
	}
	ev.OrganizationID = orgID
	ev.ResourceMetadata = metadata
	ev.Outcome = audit.OutcomeSuccess
	h.audit.LogAsync(ev)
}

// isPrivilegedRole reports whether role is a privileged static role (OWNER/ADMIN) whose assignment
// must additionally be gated on organization:manage_roles.
func isPrivilegedRole(role string) bool {
	return role == rbac.RoleOwner || role == rbac.RoleAdmin
}

// memberSnapshot builds an audit metadata snapshot for a member ("member", user, extra...).
func memberSnapshot(m *user.User, extra ...string) map[string]any {
	md := map[string]any{}
	if m != nil {
		md["member.id"] = m.ID
		md["member.firstName"] = m.FirstName
		md["member.lastName"] = m.LastName
		md["member.email"] = m.Email
	}
	for i := 0; i+1 < len(extra); i += 2 {
		md[extra[i]] = extra[i+1]
	}
	return md
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := h.svc.GetOrganizationForUser(r.Context(), id, u.Sub); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.policy.RequirePermission(r.Context(), u.Sub, id, rbac.OrganizationManageMembers); err != nil {
		h.fail(w, err)
		return
	}
	sub := chi.URLParam(r, "sub")
	if err := h.repo.RemoveMember(r.Context(), id, sub); err != nil {
		h.fail(w, err)
		return
	}
	o, _ := h.svc.GetOrganization(r.Context(), id)
	member, _ := h.users.FindBySub(r.Context(), sub)
	md := memberSnapshot(member)
	if member == nil {
		md = map[string]any{"memberSub": sub}
	}
	h.memberAudit(u, o, id, audit.ActionRemoveMember, md)
	httpx.OK(w, h.toDto(r.Context(), o, u.Sub))
}

func (h *Handler) updateMemberRole(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := h.svc.GetOrganizationForUser(r.Context(), id, u.Sub); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.policy.RequirePermission(r.Context(), u.Sub, id, rbac.OrganizationManageRoles); err != nil {
		h.fail(w, err)
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	sub := chi.URLParam(r, "sub")
	if err := h.repo.UpdateMemberRole(r.Context(), id, sub, req.Role); err != nil {
		h.fail(w, err)
		return
	}
	o, _ := h.svc.GetOrganization(r.Context(), id)
	member, _ := h.users.FindBySub(r.Context(), sub)
	md := memberSnapshot(member, "newRole", req.Role)
	if member == nil {
		md = map[string]any{"memberSub": sub, "newRole": req.Role}
	}
	h.memberAudit(u, o, id, audit.ActionChangeRole, md)
	httpx.OK(w, h.toDto(r.Context(), o, u.Sub))
}

func (h *Handler) getMembers(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := h.svc.GetOrganizationForUser(r.Context(), id, u.Sub); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.policy.RequirePermission(r.Context(), u.Sub, id, rbac.OrganizationRead); err != nil {
		h.fail(w, err)
		return
	}
	members, err := h.repo.Members(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	dtos := make([]MemberDto, 0, len(members))
	for _, m := range members {
		d := MemberDto{Sub: m.Sub, Role: m.Role()}
		if mu, _ := h.users.FindBySub(r.Context(), m.Sub); mu != nil {
			d.FirstName, d.LastName, d.Email = mu.FirstName, mu.LastName, mu.Email
		}
		dtos = append(dtos, d)
	}
	httpx.List(w, dtos)
}

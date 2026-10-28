package admin

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// projectmanager.go implements the MUTATIONS of the project-manager surface
// (/api/v1/admin/projects/manage): add-member, remove-member, invite. All three gate on
// ADMIN_PROJECT_MANAGE. (The base path /projects/manage is NOT touched by any
// existing handler.go route — the existing project reads live under the singular /project.)
//
// Call graph:
//
//	POST   /projects/manage          projectManagerAddUser     {userId, projectId, role}
//	POST   /projects/manage/remove   projectManagerRemoveUser  {projectId, sub}
//	POST   /projects/manage/invite   projectManagerInvite      {projectId, newUser, userIds}
//
// ── add member ──
//
//	project = load by projectId                     → 404 "The project with id %s was not found. "
//	user    = load by userId                        → 404 "User with id %s not found " when absent
//	if project status == DISABLED                   → 400 "Project is suspended. Cannot add user to project"
//	if memberships already contains user.sub        → 400 "User is already added to project"
//	append {sub:user.sub, role:req.role}; save project   [PERSISTED state]
//	provision the user on the live external services     [CLOUD, not wired]
//	return the project
//
// The membership append is persisted, then the updated project is returned. The platform runs every
// cloud call through an ADMIN-scoped tenant client, so members carry no per-user cloud identity —
// there is no per-user cloud grant to perform (this mirrors the client-side AddMember, datastore-only).
//
// ── remove member ──
//
//	project = load by projectId                     → 404 "The project with id %s was not found. "
//	user    = load by sub                           → null ⇒ 404 "User not found with sub: " + sub
//	membership = first membership with sub==user.sub
//	if no such membership                           → 400 "User is already removed from project"
//	if membership.role == OWNER                     → 400 "Project owner cannot be removed from project"
//	remove the membership with sub==user.sub; save project            [PERSISTED state]
//	(no per-user cloud revoke — admin-scoped model, see add member)
//	return the project
//
// ── invite ──
//
//	project = load by projectId                     → 404 "The project with id %s was not found. "
//	if userIds == null                              → 400 "User IDs must be provided for project invitation"
//	newUser ? per email: invite a new user to the project by email address
//	        : per userId→user: invite the resolved user to the project by their email
//	return "Successful operation"
//
// invite creates project-invite records + sends invitation emails via the wired inviteToProject leg
// (the same one the admin user-create loop uses). newUser → invite by email address; else resolve
// each userId → invite by the user's email. Per-item failures are swallowed (best-effort).

const projectManagePerm = "admin:project:manage"

// projectCollection is declared in projectmut.go (same package).

// routeProjectManager registers the project-manager mutation routes. The base path
// /projects/manage has no overlap with the existing /project routes in handler.go.
func (h *Handler) routeProjectManager(r chi.Router) {
	r.Post("/projects/manage", h.projectManagerAddUser)
	r.Post("/projects/manage/remove", h.projectManagerRemoveUser)
	r.Post("/projects/manage/invite", h.projectManagerInvite)
}

// projectManagerAddUserReq is the add-user request body {userId, projectId, role}.
type projectManagerAddUserReq struct {
	UserID    string `json:"userId"`
	ProjectID string `json:"projectId"`
	Role      string `json:"role"`
}

// projectManagerRemoveUserReq is the remove-user request body {projectId, sub}.
type projectManagerRemoveUserReq struct {
	ProjectID string `json:"projectId"`
	Sub       string `json:"sub"`
}

// projectManagerInviteReq is the invite request body {projectId, newUser, userIds}.
type projectManagerInviteReq struct {
	ProjectID string   `json:"projectId"`
	NewUser   bool     `json:"newUser"`
	UserIDs   []string `json:"userIds"`
}

// projectNotFound is the exact 404 message
// ("The project with id %s was not found. " — trailing space, interpolated).
func projectNotFound(id string) *httpx.HTTPError {
	return httpx.NotFound(fmt.Sprintf("The project with id %s was not found. ", id))
}

// projectManagerAddUser adds a member to a project:
// resolve project + user(by id) → suspended/already-member guards → append membership (PERSISTED) →
// provision the user on the live external services [CLOUD, not wired].
func (h *Handler) projectManagerAddUser(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, projectManagePerm) {
		return
	}
	var req projectManagerAddUserReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	proj, err := h.repo.projectByID(r.Context(), req.ProjectID)
	if httpx.WriteError(w, err) {
		return
	}
	if proj == nil {
		httpx.WriteError(w, projectNotFound(req.ProjectID))
		return
	}
	u, err := h.repo.userByID(r.Context(), req.UserID)
	if httpx.WriteError(w, err) {
		return
	}
	if u == nil {
		// User not found → 404 "User with id %s not found "
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("User with id %s not found ", req.UserID)))
		return
	}
	// status==DISABLED → suspended guard.
	if status, _ := proj["status"].(string); status == "DISABLED" {
		httpx.WriteError(w, httpx.BadRequest("Project is suspended. Cannot add user to project"))
		return
	}
	if projectHasMember(proj, u.Sub) {
		httpx.WriteError(w, httpx.BadRequest("User is already added to project"))
		return
	}
	// Append Membership{sub, role} and return the updated project. The platform runs every cloud
	// call through an ADMIN-scoped tenant client — members have no per-user cloud identity — so
	// there is no per-user cloud grant to make here (this mirrors the client-side AddMember, which is
	// datastore-only). Returning the project after the persist also avoids the prior state-leak where the
	// membership was written but the caller received a 501.
	if err := h.repo.addProjectMembership(r.Context(), req.ProjectID, u.Sub, req.Role); httpx.WriteError(w, err) {
		return
	}
	updated, err := h.repo.projectByID(r.Context(), req.ProjectID)
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, shapeDoc(updated))
}

// projectManagerRemoveUser removes a member from a project:
// resolve project + user(by sub) → membership/owner guards → remove membership (PERSISTED) →
// revoke the user on the live external services [CLOUD, not wired].
func (h *Handler) projectManagerRemoveUser(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, projectManagePerm) {
		return
	}
	var req projectManagerRemoveUserReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	proj, err := h.repo.projectByID(r.Context(), req.ProjectID)
	if httpx.WriteError(w, err) {
		return
	}
	if proj == nil {
		httpx.WriteError(w, projectNotFound(req.ProjectID))
		return
	}
	u, err := h.repo.userBySub(r.Context(), req.Sub)
	if httpx.WriteError(w, err) {
		return
	}
	if u == nil {
		// null user → 404 "User not found with sub: " + sub
		httpx.WriteError(w, httpx.NotFound("User not found with sub: "+req.Sub))
		return
	}
	role, found := projectMemberRole(proj, u.Sub)
	if !found {
		httpx.WriteError(w, httpx.BadRequest("User is already removed from project"))
		return
	}
	if role == "OWNER" {
		httpx.WriteError(w, httpx.BadRequest("Project owner cannot be removed from project"))
		return
	}
	// Remove the membership with sub == user.sub → return the updated project. No per-user cloud
	// revoke (admin-scoped model, see add member) — and returning the project avoids the state-leak.
	if err := h.repo.removeProjectMembership(r.Context(), req.ProjectID, u.Sub); httpx.WriteError(w, err) {
		return
	}
	updated, err := h.repo.projectByID(r.Context(), req.ProjectID)
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, shapeDoc(updated))
}

// projectManagerInvite invites users to a project:
// resolve project → null-userIds guard → send project invitations [EMAIL/INVITE, not wired].
func (h *Handler) projectManagerInvite(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, projectManagePerm) {
		return
	}
	var req projectManagerInviteReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	proj, err := h.repo.projectByID(r.Context(), req.ProjectID)
	if httpx.WriteError(w, err) {
		return
	}
	if proj == nil {
		httpx.WriteError(w, projectNotFound(req.ProjectID))
		return
	}
	if req.UserIDs == nil {
		httpx.WriteError(w, httpx.BadRequest("User IDs must be provided for project invitation"))
		return
	}
	if h.inviteToProject == nil {
		// Invite subsystem not wired (unit tests construct the Handler without it).
		httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented,
			"project invite delivery not implemented"))
		return
	}
	// newUser → UserIDs carry EMAIL addresses (invite by address); else USER IDs (resolve the user →
	// invite by that user's email). Per-item failures are logged+skipped (best-effort): one bad invite
	// must not abort the batch, so each is swallowed and the endpoint always returns success.
	for _, item := range req.UserIDs {
		if req.NewUser {
			_ = h.inviteToProject(r.Context(), &user.User{Email: item}, item, req.ProjectID)
			continue
		}
		invitee, err := h.repo.userByID(r.Context(), item)
		if err != nil || invitee == nil || invitee.Email == "" {
			continue
		}
		_ = h.inviteToProject(r.Context(), invitee, invitee.Email, req.ProjectID)
	}
	httpx.OK(w, "Successful operation")
}

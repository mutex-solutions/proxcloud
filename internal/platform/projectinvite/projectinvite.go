// Package projectinvite handles project invites: create an
// invite (+ the "send_invite_to_project" mail + INVITE audit), look one up by the caller's
// email + token, and accept/decline it. A missing invite on GET → null project → empty
// envelope {}.
package projectinvite

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

type Repo struct {
	invites  *pgdoc.Store
	canceler inviteCanceler
}

func NewRepo(db *pgdoc.DB) *Repo { return &Repo{invites: db.C("projectInvite")} }

// purgeExpired deletes lapsed invites (PG has no TTL index; this reproduces the
// an opportunistic TTL self-cleanup opportunistically before reads and on insert).
func (r *Repo) purgeExpired(ctx context.Context) error {
	if err := r.invites.Ensure(ctx); err != nil {
		return err
	}
	return r.deleteInvites(ctx, "", "", true)
}

// ByEmailAndToken finds an invite by email + token. nil when none, and for a blank
// email (what callerEmail yields for an unverified address).
func (r *Repo) ByEmailAndToken(ctx context.Context, email, token string) (pgdoc.M, error) {
	if email == "" {
		return nil, nil
	}
	if err := r.purgeExpired(ctx); err != nil {
		return nil, err
	}
	var doc pgdoc.M
	found, err := r.invites.FindOne(ctx, pgdoc.M{"email": email, "token": token}, &doc)
	if err != nil || !found {
		return nil, err
	}
	return doc, nil
}

// ByEmail lists the pending (non-expired) invites for an email.
func (r *Repo) ByEmail(ctx context.Context, email string) ([]pgdoc.M, error) {
	if email == "" {
		return nil, nil
	}
	if err := r.purgeExpired(ctx); err != nil {
		return nil, err
	}
	var docs []pgdoc.M
	if err := r.invites.Find(ctx, pgdoc.M{"email": email}, &docs); err != nil {
		return nil, err
	}
	return docs, nil
}

// DeleteByEmailAndProjectID removes all invites for an email+project (accept-invite cleanup).
func (r *Repo) DeleteByEmailAndProjectID(ctx context.Context, email, projectID string) error {
	if err := r.invites.Ensure(ctx); err != nil {
		return err
	}
	return r.deleteInvites(ctx, email, projectID, false)
}

// Create inserts a new invite doc, returning its generated id.
// Also purges lapsed invites (the TTL-index replacement).
func (r *Repo) Create(ctx context.Context, doc pgdoc.M) (string, error) {
	if err := pgdoc.RejectInheritedTransaction(ctx); err != nil {
		return "", err
	}
	if err := r.purgeExpired(ctx); err != nil {
		return "", err
	}
	var id string
	err := r.invites.DB().WithTx(ctx, func(tc context.Context) error {
		if err := r.invites.DB().LockXact(tc, inviteDecisionLock); err != nil {
			return err
		}
		var err error
		id, err = r.invites.InsertOne(tc, doc)
		return err
	})
	return id, err
}

// EnsureIndexes creates the table + the expiresAt index (a document-store TTL index would have
// no PG equivalent; expiry is enforced by purgeExpired, this index serves it).
func (r *Repo) EnsureIndexes(ctx context.Context) error {
	if err := r.invites.Ensure(ctx); err != nil {
		return err
	}
	return r.invites.EnsureIndex(ctx, "expiresAt", false, pgdoc.IndexField{Field: "expiresAt", Kind: pgdoc.KTime})
}

// Notifier sends a templated email (mail.Service satisfies it; nil-safe at the call site).
type Notifier interface {
	SendTemplate(ctx context.Context, key string, to []string, vars map[string]any) error
}

type Handler struct {
	repo       *Repo
	users      *user.Repo
	projectSvc *project.Service
	orgRepo    *org.Repo
	audit      *audit.Service
	notifier   Notifier
	outbox     InviteOutbox
	uiBaseURL  string
	// authorizer gates the HTTP invite() on project:manage_members. Nil (production default) → the
	// real project policy is built from orgRepo in authz(); tests inject a fake.
	authorizer projectAuthorizer
}

// projectAuthorizer gates a project-scoped permission for a caller. *project.Policy satisfies it.
type projectAuthorizer interface {
	RequireProjectPermission(ctx context.Context, sub string, proj *project.Project, permKey string) error
}

// SetAuthorizer overrides the project authorizer (tests). Production leaves it nil and authz()
// builds the real policy from orgRepo.
func (h *Handler) SetAuthorizer(a projectAuthorizer) { h.authorizer = a }

// authz returns the project authorizer — the injected one, else the real policy built from the
// org repo (secure by default; no extra wiring required).
func (h *Handler) authz() projectAuthorizer {
	if h.authorizer != nil {
		return h.authorizer
	}
	return project.NewPolicy(org.NewPolicy(h.orgRepo))
}

// requireInvitePermission enforces project:manage_members for the caller on proj (the HTTP-invite
// authorization boundary — separated for unit testing without the datastore).
func (h *Handler) requireInvitePermission(ctx context.Context, sub string, proj *project.Project) error {
	return h.authz().RequireProjectPermission(ctx, sub, proj, rbac.ProjectManageMembers)
}

func NewHandler(repo *Repo, users *user.Repo, projectSvc *project.Service, orgRepo *org.Repo, a *audit.Service, notifier Notifier, uiBaseURL string) *Handler {
	return &Handler{repo: repo, users: users, projectSvc: projectSvc, orgRepo: orgRepo, audit: a, notifier: notifier, uiBaseURL: uiBaseURL}
}

// inviteDocID renders the raw _id as the id string for the audit event.
func inviteDocID(inv map[string]any) string {
	v, _ := inv["_id"].(string)
	return v
}

// auditInvite emits the invite audit event (CLIENT_AREA user actor, PROJECT context,
// resource PROJECT_INVITE). Best-effort; nil audit → no-op.
func (h *Handler) auditInvite(u *user.User, action, inviteID, projectID, projectName string, meta map[string]any) {
	if h.audit == nil {
		return
	}
	ev := audit.ClientUserEvent(u.Sub, u.FullName())
	ev.EventContext = audit.ContextProject
	ev.Action = action
	ev.ResourceType = audit.ResourceProjectInvite
	ev.ResourceID = inviteID
	ev.ResourceDisplayName = projectName
	ev.ProjectID = projectID
	ev.ResourceMetadata = meta
	ev.Outcome = audit.OutcomeSuccess
	h.audit.LogAsync(ev)
}

func (h *Handler) Routes(r chi.Router) {
	r.Post("/project-invites/invite", h.invite)
	r.Get("/project-invites/mine", h.mine) // static — precedes /{token}
	r.Get("/project-invites/{token}", h.get)
	r.Post("/project-invites/accept/{token}", h.accept)
	r.Post("/project-invites/decline/{token}", h.decline)
}

// invite handles the invite-to-project request:
// project-by-id 404, already-a-member 400 ("User is already added to project"), then save
// {email, projectId, token: UUID, expiresAt: +24h}, send the "send_invite_to_project" mail with
// the join-project deep link, and audit INVITE (resourceMetadata.invitedEmail). The caller must
// hold project:manage_members on the target project (requireInvitePermission). 202 Accepted.
func (h *Handler) invite(w http.ResponseWriter, r *http.Request) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	var req struct {
		Email     string `json:"email"`
		ProjectID string `json:"projectId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	// Authorize the caller on the TARGET project before minting an invite — without this a member
	// of any project could mint invites into an arbitrary project by id.
	proj, err := h.projectSvc.GetProjectByID(r.Context(), req.ProjectID)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	if err := h.requireInvitePermission(r.Context(), u.Sub, proj); err != nil {
		httpx.WriteError(w, err)
		return
	}
	if err := h.InviteToProject(r.Context(), u, req.Email, req.ProjectID); err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.Accepted(w)
}

// InviteToProject is the service leg of invite — shared
// by the HTTP handler above and the admin user-create projectIds loop.
// Returns *httpx.HTTPError on project-404 / already-a-member-400.
func (h *Handler) InviteToProject(ctx context.Context, u *user.User, email, projectID string) error {
	if err := pgdoc.RejectInheritedTransaction(ctx); err != nil {
		return err
	}
	proj, err := h.projectSvc.GetProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	// An existing user who is already a project member → 400.
	if invited, _ := h.users.FindVerifiedByEmail(ctx, email); invited != nil && proj.IsMember(invited.Sub) {
		return httpx.BadRequest("User is already added to project")
	}
	if h.outbox != nil {
		id, err := h.createDurableInvite(ctx, email, proj)
		if err != nil {
			return err // never fall back to the best-effort notifier
		}
		h.auditInvite(u, audit.ActionInvite, id, proj.ID, proj.Name, map[string]any{"invitedEmail": email})
		return nil
	}
	token := uuid.NewString()
	inviteID, err := h.repo.Create(ctx, pgdoc.M{
		"email":        email,
		"projectId":    proj.ID,
		"token":        token,
		"expiresAt":    time.Now().UTC().Add(24 * time.Hour),
		"deliveryMode": "legacy",
	})
	if err != nil {
		return err
	}
	// Send the project-invitation mail → messageKey send_invite_to_project (best-effort).
	if h.notifier != nil && email != "" {
		_ = h.notifier.SendTemplate(ctx, "send_invite_to_project", []string{email}, map[string]any{
			"projectName":      proj.Name,
			"projectInviteUrl": h.uiBaseURL + "/join-project?invite-token=" + token,
			"email":            email,
			"expiryHours":      24,
		})
	}
	h.auditInvite(u, audit.ActionInvite, inviteID, proj.ID, proj.Name, map[string]any{"invitedEmail": email})
	return nil
}

// accept resolves the caller's invite by email+token, adds
// them to the project's organization (if needed) + the project (MEMBER), then deletes the invite(s).
func (h *Handler) accept(w http.ResponseWriter, r *http.Request) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	if !u.EmailVerified() {
		httpx.WriteError(w, httpx.Forbidden("Verify your email address before accepting a project invite"))
		return
	}
	inv, err := h.repo.ByEmailAndToken(r.Context(), callerEmail(u), chi.URLParam(r, "token"))
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, http.StatusInternalServerError, err.Error())
		return
	}
	if inv == nil {
		httpx.Err(w, http.StatusNotFound, http.StatusNotFound, "Project invite not found")
		return
	}
	projectID, _ := inv["projectId"].(string)
	proj, err := h.projectSvc.GetProjectByID(r.Context(), projectID)
	if err != nil || proj == nil {
		// An untyped lookup error stays a 404 here, as before: only a typed error keeps its own answer.
		var he *httpx.HTTPError
		if errors.As(err, &he) {
			httpx.WriteError(w, err)
		} else {
			httpx.Err(w, http.StatusNotFound, http.StatusNotFound, "Project invite not found")
		}
		return
	}
	// Add the user to the project's organization if not already a member.
	if proj.OrganizationID != "" {
		if m, _ := h.orgRepo.FindMember(r.Context(), proj.OrganizationID, u.Sub); m == nil {
			_, _ = h.orgRepo.AddMember(r.Context(), proj.OrganizationID, u.Sub, rbac.RoleMember)
		}
	}
	// Add to the project as MEMBER (best-effort — already-a-member is ignored).
	_, _ = h.projectSvc.AddMember(r.Context(), projectID, u.Sub, project.RoleMember)
	if err := h.repo.DeleteByEmailAndProjectID(r.Context(), u.Email, projectID); err != nil {
		httpx.Err(w, http.StatusInternalServerError, http.StatusInternalServerError, err.Error())
		return
	}
	// Audit ACCEPT_INVITE on PROJECT_INVITE (project context, invite id, project name).
	h.auditInvite(u, audit.ActionAcceptInvite, inviteDocID(inv), projectID, proj.Name, nil)
	httpx.Accepted(w)
}

// callerEmail is the email the caller's invites are matched on: blank (matches
// nothing) until the IdP has verified it, so an account signed up with someone
// else's address can neither see nor claim their invites.
func callerEmail(u *user.User) string {
	if !u.EmailVerified() {
		return ""
	}
	return u.Email
}

// decline deletes the invite if present (no 404). 202.
func (h *Handler) decline(w http.ResponseWriter, r *http.Request) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	inv, err := h.repo.ByEmailAndToken(r.Context(), callerEmail(u), chi.URLParam(r, "token"))
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, http.StatusInternalServerError, err.Error())
		return
	}
	if inv != nil {
		projectID, _ := inv["projectId"].(string)
		if err := h.repo.DeleteByEmailAndProjectID(r.Context(), u.Email, projectID); err != nil {
			httpx.Err(w, http.StatusInternalServerError, http.StatusInternalServerError, err.Error())
			return
		}
		// Audit DELETE on PROJECT_INVITE for a declined invite.
		projName := ""
		if p, _ := h.projectSvc.GetProjectByID(r.Context(), projectID); p != nil {
			projName = p.Name
		}
		h.auditInvite(u, audit.ActionDelete, inviteDocID(inv), projectID, projName, nil)
	}
	httpx.Accepted(w)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	inv, err := h.repo.ByEmailAndToken(r.Context(), callerEmail(u), chi.URLParam(r, "token"))
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, http.StatusInternalServerError, err.Error())
		return
	}
	if inv == nil {
		httpx.Empty(w) // single(null) → {}
		return
	}
	httpx.OK(w, inv) // deferred: resolve + return the invited Project
}

// mine lists the caller's pending project invites (by their verified email), each with the
// target project name + token. It lets a user who logged in directly — without clicking the
// emailed join link — still discover and accept their invitations (so they land in the org +
// project instead of an empty onboarding). No token needed: the email comes from the session.
func (h *Handler) mine(w http.ResponseWriter, r *http.Request) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	invs, err := h.repo.ByEmail(r.Context(), callerEmail(u))
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(invs))
	for _, inv := range invs {
		projectID, _ := inv["projectId"].(string)
		item := map[string]any{"token": inv["token"], "projectId": projectID}
		if exp, ok := inv["expiresAt"]; ok {
			item["expiresAt"] = exp
		}
		if p, _ := h.projectSvc.GetProjectByID(r.Context(), projectID); p != nil {
			item["projectName"] = p.Name
		}
		out = append(out, item)
	}
	httpx.List(w, out)
}

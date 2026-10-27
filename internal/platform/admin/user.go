package admin

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// user.go implements the admin user-management surface (/api/v1/admin/user).
//
// ⚠ The identity stack is an EMBEDDED authorization server over this same datastore
// users/userCredential store — there is NO Keycloak in it. create/delete are
// therefore plain datastore (+ the per-user keystone cleanup on delete); only impersonate (a local
// OAuth2 token MINT) stays unimplemented, because this service is a pure OIDC resource server
// (Keycloak owns token issuance).
//
// Routes:
//
//	POST   /user                  userCreate            ADMIN_USER_CREATE       (datastore upsert-by-email + best-effort project invites)
//	GET    /user/{id}             userGet               ADMIN_USER_READ         (datastore load-by-id-or-404)
//	GET    /user                  (list)                ADMIN_USER_READ         (ALREADY in handler.go — h.listRaw "users"; NOT re-registered)
//	GET    /user/sub/{sub}        userBySub             ADMIN_USER_READ         (datastore load-by-sub; null → {})
//	GET    /user/project/{pid}    userByProject         ADMIN_USER_READ         (project load-by-id-or-404 → owner sub → user; null → {})
//	GET    /user/{id}/services    userExternalServices  ADMIN_USER_READ         (load-by-id-or-404 → list the user's external services → DTO; empty greenfield → [])
//	PUT    /user/{id}             userUpdate            ADMIN_USER_UPDATE       (datastore: sub/firstName/lastName/email overwrite)
//	DELETE /user/{id}             userDelete            ADMIN_USER_DELETE       (per-user keystone cleanup → delete by id)
//	POST   /user/{id}/impersonate userImpersonate       ADMIN_USER_IMPERSONATE  [not implemented: local OAuth2 token mint — divergent by design]
//
// Error strings are exact: "User with id %s not found ",
// "The project with id %s was not found. ",
// "User is in use for projects", "Cannot delete user " — all verbatim incl.
// trailing spaces. Datastore writes via the crud.go helpers + user_repo.go. Identity/Keycloak/token
// ops return 501. The surface also writes audit events (deferred, // TODO(audit)).

const userCollection = "users"

const (
	userReadPerm        = "admin:user:read"
	userCreatePerm      = "admin:user:create"
	userUpdatePerm      = "admin:user:update"
	userDeletePerm      = "admin:user:delete"
	userImpersonatePerm = "admin:user:impersonate"
)

// routeUser registers the admin user-management endpoints not already in handler.go. The {id} param
// name reuses the one chi already uses elsewhere at this path position. GET /user (the list) is
// already registered as h.listRaw("admin:user:read","users") and is intentionally NOT re-registered.
func (h *Handler) routeUser(r chi.Router) {
	r.Post("/user", h.userCreate)
	r.Get("/user/sub/{sub}", h.userBySub)
	r.Get("/user/project/{projectId}", h.userByProject)
	r.Get("/user/{id}/services", h.userExternalServices)
	r.Get("/user/{id}", h.userGet)
	r.Put("/user/{id}", h.userUpdate)
	r.Delete("/user/{id}", h.userDelete)
	r.Post("/user/{id}/impersonate", h.userImpersonate)
}

// userNotFound is the exact 404 ("User with id %s not found " —
// trailing space, interpolated). Used by the load-by-id endpoints.
func userNotFound(id string) *httpx.HTTPError {
	return httpx.NotFound(fmt.Sprintf("User with id %s not found ", id))
}

// userUpdateReq holds the mutable fields userUpdate copies off the request-body user:
// sub, firstName, lastName, email (only these four are overwritten).
type userUpdateReq struct {
	Sub       string `json:"sub"`
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	Email     string `json:"email"`
}

// userGet loads a user by id: load by _id or 404, then return the single user.
func (h *Handler) userGet(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, userReadPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	u, err := h.repo.userByID(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if u == nil {
		httpx.WriteError(w, userNotFound(id))
		return
	}
	httpx.OK(w, u)
}

// userBySub loads the newest user with the given sub. A missing user is returned as an
// empty {} envelope (the null data is dropped), NOT a 404.
func (h *Handler) userBySub(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, userReadPerm) {
		return
	}
	u, err := h.repo.userBySub(r.Context(), chi.URLParam(r, "sub"))
	if httpx.WriteError(w, err) {
		return
	}
	if u == nil {
		httpx.Empty(w)
		return
	}
	httpx.OK(w, u)
}

// userByProject loads the project by id (or 404 "The project with id %s was not found. "),
// then loads the user by the project owner's sub — taken from the OWNER membership
// (else the project's `owner` field). A missing user → empty {}.
func (h *Handler) userByProject(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, userReadPerm) {
		return
	}
	pid := chi.URLParam(r, "projectId")
	proj, err := h.repo.projectByID(r.Context(), pid)
	if httpx.WriteError(w, err) {
		return
	}
	if proj == nil {
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("The project with id %s was not found. ", pid)))
		return
	}
	ownerSub := projectOwnerSub(proj)
	u, err := h.repo.userBySub(r.Context(), ownerSub)
	if httpx.WriteError(w, err) {
		return
	}
	if u == nil {
		httpx.Empty(w)
		return
	}
	httpx.OK(w, u)
}

// userExternalServices loads the user by id (or 404), then lists that user's external services
// as DTOs. The user resolve (404 path) is the real behavior; the live OpenStack external-service
// listing is a cloud integration point (cloud-admin), so under greenfield the list is empty →
// {data:[], paging}. (Same stub posture as the existing /admin/service/{id}/user/services endpoint.)
func (h *Handler) userExternalServices(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, userReadPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	u, err := h.repo.userByID(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if u == nil {
		httpx.WriteError(w, userNotFound(id))
		return
	}
	// External integration point: list the user's live OpenStack external services (cloud-admin).
	httpx.List(w, []any{})
}

// userUpdate loads the user by id (or 404) → overwrites
// sub/firstName/lastName/email off the request body → saves → returns the saved user. Only those four
// fields are copied; all other persisted fields are preserved.
func (h *Handler) userUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, userUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req userUpdateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, err := h.repo.FindDoc(r.Context(), userCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, userNotFound(id))
		return
	}
	before := maps.Clone(existing)
	// Overwrite the four mutable fields unconditionally: a null/blank request value clears the field —
	// set the value, and drop the key when blank so the stored doc / null-omitting JSON matches a
	// cleared field.
	for k, v := range map[string]string{"sub": req.Sub, "firstName": req.FirstName, "lastName": req.LastName, "email": req.Email} {
		if v == "" {
			delete(existing, k)
		} else {
			existing[k] = v
		}
	}
	if err := h.repo.ReplaceDoc(r.Context(), userCollection, id, existing); httpx.WriteError(w, err) {
		return
	}
	// UPDATE USER audit: field-level before/after diff (the middleware computes the snapshot diff).
	after, _ := h.repo.FindDoc(r.Context(), userCollection, id)
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, shapeDoc(existing))
}

// userDelete loads the user by id (or 404); if the user still owns projects → 400 "User is in use
// for projects"; else cleans up the user (delete the user's per-service keystone users — one per
// NON-SHARED openstack service in user.services[], via config.openstackUserId) → delete the user +
// audit; a failed keystone delete → 400 "Cannot delete user ". Greenfield users carry no services[]
// (the bootstrap creates no per-customer keystone users) → the cleanup set is empty → plain
// datastore delete.
func (h *Handler) userDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, userDeletePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	u, err := h.repo.userByID(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if u == nil {
		httpx.WriteError(w, userNotFound(id))
		return
	}
	inUse, err := h.repo.userInUse(r.Context(), u.Sub)
	if httpx.WriteError(w, err) {
		return
	}
	if inUse {
		httpx.WriteError(w, httpx.BadRequest("User is in use for projects"))
		return
	}
	if !h.cleanUpUser(w, r, id) {
		return
	}
	if err := h.repo.deleteUserByID(r.Context(), id); httpx.WriteError(w, err) {
		return
	}
	// DELETE USER audit (PLATFORM scope — the middleware emits the admin event).
	httpx.OK(w, "Successful operation")
}

// cleanUpUser deletes the user's per-service keystone identities: for each of the user's attached
// NON-SHARED openstack services, keystone-delete config.openstackUserId; all must succeed to allow the
// datastore delete. Writes the response (400 "Cannot delete user " / 501 when a cleanup is needed but
// the cloud factory is unwired) and returns false when the caller must stop.
func (h *Handler) cleanUpUser(w http.ResponseWriter, r *http.Request, userID string) bool {
	raw, err := h.repo.FindDoc(r.Context(), "users", userID)
	if httpx.WriteError(w, err) {
		return false
	}
	svcs, _ := raw["services"].(pgdoc.A)
	if len(svcs) == 0 {
		return true // no per-user cloud identities → nothing to clean (the greenfield case)
	}
	// Per-user keystone identities died with the OpenStack backend; any stored
	// binding is a pre-fork import with nothing live behind it.
	return true
}

// userCreate rejects a duplicate email → 400 "User with this email already exists"; else does a
// plain datastore upsert-by-email (sub = random UUID, NO password/credential, NO identity-provider
// call: the identity stack is the embedded auth server over this same `users` collection) — then
// best-effort invites the user to each projectId (each caught + logged). Returns the created user
// (newest by sub).
func (h *Handler) userCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, userCreatePerm) {
		return
	}
	var req struct {
		FirstName  string   `json:"firstName"`
		LastName   string   `json:"lastName"`
		Email      string   `json:"email"`
		ProjectIDs []string `json:"projectIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, err := h.repo.userByEmail(r.Context(), req.Email)
	if httpx.WriteError(w, err) {
		return
	}
	if existing != nil {
		httpx.WriteError(w, httpx.BadRequest("User with this email already exists"))
		return
	}
	now := time.Now().UTC()
	doc := pgdoc.M{
		"sub":          uuid.NewString(),
		"email":        req.Email,
		"createdAt":    now,
		"modelVersion": 1,
		"customInfo":   pgdoc.M{},
		"consent":      pgdoc.A{},
		"identities":   pgdoc.A{},
		"services":     pgdoc.A{},
		"metadata":     pgdoc.M{},
	}
	if req.FirstName != "" {
		doc["firstName"] = req.FirstName
	}
	if req.LastName != "" {
		doc["lastName"] = req.LastName
	}
	if _, err := h.repo.insertUserDoc(r.Context(), doc); httpx.WriteError(w, err) {
		return
	}
	created, err := h.repo.userBySub(r.Context(), doc["sub"].(string))
	if httpx.WriteError(w, err) {
		return
	}
	// Optional invites — best-effort (each caught + logged per projectId).
	if h.inviteToProject != nil {
		for _, pid := range req.ProjectIDs {
			if err := h.inviteToProject(r.Context(), created, req.Email, pid); err != nil {
				slog.Error("admin user-create: invite", "user", created.ID, "project", pid, "err", err)
			}
		}
	}
	// CREATE USER audit (the middleware emits the admin event).
	httpx.OK(w, created)
}

// userImpersonate loads the user by id (or 404 "User not found"); the embedded authorization server
// would then MINT local OAuth2 tokens (client "cloud-dashboard", grant JWT_BEARER) and return
// {"url": "<ui>/login#access_token=…&id_token=…"}. DIVERGENT BY DESIGN: token
// issuance belongs to Keycloak and this service is a pure OIDC resource server — it cannot mint
// tokens the UI would accept. Stays 501 after the user-exists check. (A Keycloak
// token-exchange/impersonation leg would be a NEW feature — do only on request.)
func (h *Handler) userImpersonate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, userImpersonatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	u, err := h.repo.userByID(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if u == nil {
		// Return 404 "User not found" when the target user is absent.
		httpx.WriteError(w, httpx.NotFound("User not found"))
		return
	}
	// Not implemented: token generation / OAuth2 authorization (cloud-dashboard client) producing
	// {redirectUrl, accessToken, idToken}, returned as {"url": redirectUrl}.
	// The Go service issues no tokens (pure OIDC resource server). TODO(audit): IMPERSONATE event.
	httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented,
		"impersonation token generation not implemented"))
}

// projectOwnerSub returns the project owner sub: the OWNER membership's sub, else the
// project's `owner` field. memberships is an array of {sub, role} sub-docs.
func projectOwnerSub(proj pgdoc.M) string {
	if ms, ok := proj["memberships"].(pgdoc.A); ok {
		for _, m := range ms {
			mm, ok := m.(pgdoc.M)
			if !ok {
				continue
			}
			if role, _ := mm["role"].(string); role == "OWNER" {
				if sub, _ := mm["sub"].(string); sub != "" {
					return sub
				}
			}
		}
	}
	if owner, _ := proj["owner"].(string); owner != "" {
		return owner
	}
	return ""
}

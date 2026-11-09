package adminapi

// users.go serves /admin-api/v1/users.

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
)

type apiIdentity struct {
	Sub    string `json:"sub,omitempty"`
	Issuer string `json:"issuer,omitempty"`
}

type apiUser struct {
	ID         string        `json:"id,omitempty"`
	Sub        string        `json:"sub,omitempty"`
	FirstName  string        `json:"first_name,omitempty"`
	LastName   string        `json:"last_name,omitempty"`
	Email      string        `json:"email,omitempty"`
	Identities []apiIdentity `json:"identities"` // orElse(new ArrayList) — always an array
}

func mapUser(u *user.User) apiUser {
	ids := make([]apiIdentity, 0, len(u.Identities))
	for _, i := range u.Identities {
		ids = append(ids, apiIdentity{Sub: i.Sub, Issuer: i.Issuer})
	}
	return apiUser{ID: u.ID, Sub: u.Sub, FirstName: u.FirstName, LastName: u.LastName, Email: u.Email, Identities: ids}
}

func (h *Handler) routeUsers(r chi.Router) {
	r.Get("/users", h.usersList)
	r.Post("/users", h.userCreate)
	r.Get("/users/{id}", h.userGet)
	r.Delete("/users/{id}", h.userDelete)
}

func (h *Handler) usersList(w http.ResponseWriter, r *http.Request) {
	req, ok := listParams(w, r)
	if !ok {
		return
	}
	f := pgdoc.M{}
	if email := r.URL.Query().Get("email"); email != "" {
		f["email"] = email
	}
	if sub := r.URL.Query().Get("sub"); sub != "" {
		f["$or"] = []pgdoc.M{{"sub": sub}, {"identities": pgdoc.M{"$contains": pgdoc.M{"sub": sub}}}}
	}
	users, err := findPage[user.User](r.Context(), h.db.C("users"), f, req)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	page, next := pageOut(req, users, func(u user.User) string { return u.ID })
	out := make([]apiUser, 0, len(page))
	for i := range page {
		out = append(out, mapUser(&page[i]))
	}
	writeList(w, out, next)
}

func (h *Handler) userGet(w http.ResponseWriter, r *http.Request) {
	var u user.User
	found, err := h.db.C("users").Get(r.Context(), chi.URLParam(r, "id"), &u)
	if err != nil || !found {
		apiNotFound(w)
		return
	}
	writeEntity(w, mapUser(&u))
}

// userCreate makes a user. With a sub it is a pre-create for an integrator who manages the IdP: the
// first login under that sub lands on this row. Without one the account is made in Keycloak with a
// temporary password returned once, as in the operator console, since a row whose sub no login can
// ever match would be a user who cannot sign in. No service account and no sub → 400. An existing
// email → 409.
func (h *Handler) userCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email     string `json:"email"`
		Sub       string `json:"sub"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.Email == "" {
		badRequest(w, "email is required")
		return
	}
	if existing, _ := h.users.FindByEmail(r.Context(), req.Email); existing != nil {
		conflict(w, "A user with this email already exists.")
		return
	}
	sub, tempPassword := req.Sub, ""
	if sub == "" {
		if h.keycloak == nil {
			badRequest(w, "sub is required: this platform has no identity service account to create the login with")
			return
		}
		var err error
		if sub, tempPassword, err = h.keycloak.CreateUser(r.Context(), req.Email, req.FirstName, req.LastName); err != nil {
			writeErr(w, http.StatusBadGateway, "BAD_GATEWAY", err.Error())
			return
		}
	}
	u := user.User{ID: newID(), Sub: sub, Email: req.Email, FirstName: req.FirstName, LastName: req.LastName}
	if req.Sub != "" { // Keycloak's own login fills in its identity, as for a user the operator console makes
		u.Identities = []user.Identity{{Issuer: "api", Sub: sub}}
	}
	if _, err := h.db.C("users").InsertOne(r.Context(), &u); err != nil {
		if tempPassword != "" { // a login without a row is the reverse of what this path fixes
			_ = h.keycloak.DeleteUser(r.Context(), sub)
		}
		badRequest(w, err.Error())
		return
	}
	h.logAdmin(r, audit.ActionCreate, "USER", u.ID, u.Email)
	if tempPassword != "" {
		writeEntity(w, struct {
			apiUser
			TemporaryPassword string `json:"temporary_password"`
		}{mapUser(&u), tempPassword})
		return
	}
	writeEntity(w, mapUser(&u))
}

func (h *Handler) userDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var u user.User
	if found, err := h.db.C("users").Get(r.Context(), id, &u); err != nil || !found {
		apiNotFound(w)
		return
	}
	// The login goes too, or the user signs back in and comes back as a new row. A sub Keycloak does
	// not know (an integrator's IdP) is a no-op there.
	if h.keycloak != nil {
		if err := h.keycloak.DeleteUser(r.Context(), u.Sub); err != nil {
			writeErr(w, http.StatusBadGateway, "BAD_GATEWAY", err.Error())
			return
		}
	}
	if _, err := h.db.C("users").DeleteByID(r.Context(), id); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.logAdmin(r, audit.ActionDelete, "USER", id, "")
	w.WriteHeader(http.StatusOK) // 200 empty
}

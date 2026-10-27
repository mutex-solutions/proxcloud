package adminapi

// users.go serves /admin-api/v1/users.

import (
	"crypto/md5"
	"encoding/hex"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

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

// userCreate pre-creates a user before first login. sub defaults to "user-<md5(uuid)>"; the
// identity is issuer "api". An existing email → 409.
func (h *Handler) userCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		Sub   string `json:"sub"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if existing, _ := h.users.FindByEmail(r.Context(), req.Email); existing != nil {
		conflict(w, "A user with this email already exists.")
		return
	}
	sub := req.Sub
	if sub == "" {
		s := md5.Sum([]byte(uuid.NewString()))
		sub = "user-" + hex.EncodeToString(s[:])
	}
	u := user.User{
		ID: newID(), Sub: sub, Email: req.Email,
		Identities: []user.Identity{{Issuer: "api", Sub: sub}},
	}
	if _, err := h.db.C("users").InsertOne(r.Context(), &u); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.logAdmin(r, audit.ActionCreate, "USER", u.ID, u.Email)
	writeEntity(w, mapUser(&u))
}

func (h *Handler) userDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var u user.User
	if found, err := h.db.C("users").Get(r.Context(), id, &u); err != nil || !found {
		apiNotFound(w)
		return
	}
	if _, err := h.db.C("users").DeleteByID(r.Context(), id); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.logAdmin(r, audit.ActionDelete, "USER", id, "")
	w.WriteHeader(http.StatusOK) // 200 empty
}

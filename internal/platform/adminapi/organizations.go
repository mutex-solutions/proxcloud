package adminapi

// organizations.go serves /admin-api/v1/organizations.

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
)

type apiOrgMember struct {
	Sub       string `json:"sub,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Email     string `json:"email,omitempty"`
	Role      string `json:"role,omitempty"`
}

type apiOrganization struct {
	ID               string         `json:"id,omitempty"`
	Name             string         `json:"name,omitempty"`
	Description      string         `json:"description,omitempty"`
	BillingProfileID string         `json:"billing_profile_id,omitempty"`
	Members          []apiOrgMember `json:"members"`
	CreatedAt        *time.Time     `json:"created_at,omitempty"`
	UpdatedAt        *time.Time     `json:"updated_at,omitempty"`
}

func (h *Handler) routeOrganizations(r chi.Router) {
	r.Get("/organizations", h.orgsList)
	r.Post("/organizations", h.orgCreate)
	r.Get("/organizations/{id}", h.orgGet)
	r.Put("/organizations/{id}", h.orgUpdate)
	r.Get("/organizations/{id}/members", h.orgMembersList)
	r.Post("/organizations/{id}/members", h.orgMemberAdd)
	r.Delete("/organizations/{id}/members/{sub}", h.orgMemberRemove)
	r.Put("/organizations/{id}/members/{sub}/role", h.orgMemberRole)
}

func (h *Handler) mapOrg(r *http.Request, o *org.Organization) apiOrganization {
	members, _ := h.orgs.Members(r.Context(), o.ID)
	return apiOrganization{
		ID: o.ID, Name: o.Name, Description: o.Description, BillingProfileID: o.BillingProfileID,
		Members: h.mapMembers(r, members), CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
	}
}

func (h *Handler) mapMembers(r *http.Request, members []org.Member) []apiOrgMember {
	out := make([]apiOrgMember, 0, len(members))
	for i := range members {
		out = append(out, h.mapMember(r, &members[i]))
	}
	return out
}

func (h *Handler) mapMember(r *http.Request, m *org.Member) apiOrgMember {
	am := apiOrgMember{Sub: m.Sub, Role: m.Role()}
	if u, _ := h.users.FindBySub(r.Context(), m.Sub); u != nil {
		am.FirstName, am.LastName, am.Email = u.FirstName, u.LastName, u.Email
	}
	return am
}

func (h *Handler) orgsList(w http.ResponseWriter, r *http.Request) {
	req, ok := listParams(w, r)
	if !ok {
		return
	}
	f := pgdoc.M{}
	if name := r.URL.Query().Get("name"); name != "" {
		f["name"] = name
	}
	if sub := r.URL.Query().Get("member_sub"); sub != "" {
		ids, err := h.orgs.OrgIDsForSub(r.Context(), sub)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		if len(ids) == 0 {
			writeList(w, []apiOrganization{}, "")
			return
		}
		f["_id"] = pgdoc.M{"$in": ids}
	}
	if bpID := r.URL.Query().Get("billing_profile_id"); bpID != "" {
		f["billingProfileId"] = bpID
	}
	orgsPage, err := findPage[org.Organization](r.Context(), h.db.C("organization"), f, req)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	page, next := pageOut(req, orgsPage, func(o org.Organization) string { return o.ID })
	out := make([]apiOrganization, 0, len(page))
	for i := range page {
		out = append(out, h.mapOrg(r, &page[i]))
	}
	writeList(w, out, next)
}

func (h *Handler) orgGet(w http.ResponseWriter, r *http.Request) {
	o, err := h.orgs.FindByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil || o == nil {
		apiNotFound(w)
		return
	}
	writeEntity(w, h.mapOrg(r, o))
}

func (h *Handler) orgCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name             string `json:"name"`
		Description      string `json:"description"`
		OwnerSub         string `json:"owner_sub"`
		BillingProfileID string `json:"billing_profile_id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	// owner_sub must resolve to an existing user BEFORE anything persists.
	if req.OwnerSub != "" {
		if u, _ := h.users.FindBySub(r.Context(), req.OwnerSub); u == nil {
			apiNotFoundMsg(w, "User not found")
			return
		}
	}
	o := &org.Organization{Name: req.Name, Description: req.Description, CustomInfo: map[string]any{}}
	saved, err := h.orgs.Insert(r.Context(), o)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if req.OwnerSub != "" {
		_, _ = h.orgs.AddMember(r.Context(), saved.ID, req.OwnerSub, "OWNER")
	}
	if req.BillingProfileID != "" {
		saved.BillingProfileID = req.BillingProfileID
		if err := h.orgs.Save(r.Context(), saved); err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	h.logAdmin(r, audit.ActionCreate, "ORGANIZATION", saved.ID, saved.Name)
	writeEntity(w, h.mapOrg(r, saved))
}

func (h *Handler) orgUpdate(w http.ResponseWriter, r *http.Request) {
	o, err := h.orgs.FindByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil || o == nil {
		apiNotFound(w)
		return
	}
	// Only NON-NULL request fields overwrite — pointers to tell null from "".
	var req struct {
		Name             *string `json:"name"`
		Description      *string `json:"description"`
		BillingProfileID *string `json:"billing_profile_id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Name != nil {
		o.Name = *req.Name
	}
	if req.Description != nil {
		o.Description = *req.Description
	}
	if req.BillingProfileID != nil {
		o.BillingProfileID = *req.BillingProfileID
	}
	if err := h.orgs.Save(r.Context(), o); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.logAdmin(r, audit.ActionUpdate, "ORGANIZATION", o.ID, o.Name)
	writeEntity(w, h.mapOrg(r, o))
}

func (h *Handler) orgMembersList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if o, err := h.orgs.FindByID(r.Context(), id); err != nil || o == nil {
		apiNotFound(w)
		return
	}
	members, err := h.orgs.Members(r.Context(), id)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	// This endpoint returns the BARE array (no envelope).
	writeJSON(w, http.StatusOK, h.mapMembers(r, members))
}

func (h *Handler) orgMemberAdd(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	o, err := h.orgs.FindByID(r.Context(), id)
	if err != nil || o == nil {
		apiNotFound(w)
		return
	}
	var req struct {
		Sub  string `json:"sub"`
		Role string `json:"role"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if u, _ := h.users.FindBySub(r.Context(), req.Sub); u == nil {
		apiNotFoundMsg(w, "User not found")
		return
	}
	if m, _ := h.orgs.FindMember(r.Context(), id, req.Sub); m != nil {
		conflict(w, "User is already a member of this organization")
		return
	}
	m, err := h.orgs.AddMember(r.Context(), id, req.Sub, req.Role)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	h.logAdmin(r, audit.ActionAddMember, "ORGANIZATION", o.ID, o.Name)
	writeCreated(w, h.mapMember(r, m)) // @ResponseStatus(CREATED)
}

func (h *Handler) orgMemberRemove(w http.ResponseWriter, r *http.Request) {
	id, sub := chi.URLParam(r, "id"), chi.URLParam(r, "sub")
	o, err := h.orgs.FindByID(r.Context(), id)
	if err != nil || o == nil {
		apiNotFound(w)
		return
	}
	m, _ := h.orgs.FindMember(r.Context(), id, sub)
	if m == nil {
		apiNotFoundMsg(w, "Member not found")
		return
	}
	if m.Role() == "OWNER" && h.ownerCount(r, id) <= 1 {
		conflict(w, "Cannot remove the last owner")
		return
	}
	if err := h.orgs.RemoveMember(r.Context(), id, sub); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.logAdmin(r, audit.ActionRemoveMember, "ORGANIZATION", o.ID, o.Name)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) orgMemberRole(w http.ResponseWriter, r *http.Request) {
	id, sub := chi.URLParam(r, "id"), chi.URLParam(r, "sub")
	o, err := h.orgs.FindByID(r.Context(), id)
	if err != nil || o == nil {
		apiNotFound(w)
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	m, _ := h.orgs.FindMember(r.Context(), id, sub)
	if m == nil {
		apiNotFoundMsg(w, "Member not found")
		return
	}
	if m.Role() == "OWNER" && h.ownerCount(r, id) <= 1 && req.Role != "OWNER" {
		conflict(w, "Cannot change role of the last owner")
		return
	}
	if err := h.orgs.UpdateMemberRole(r.Context(), id, sub, req.Role); err != nil {
		badRequest(w, err.Error())
		return
	}
	m.Roles = []string{req.Role}
	h.logAdmin(r, audit.ActionChangeRole, "ORGANIZATION", o.ID, o.Name)
	writeEntity(w, h.mapMember(r, m))
}

func (h *Handler) ownerCount(r *http.Request, orgID string) int {
	members, _ := h.orgs.Members(r.Context(), orgID)
	n := 0
	for i := range members {
		if members[i].Role() == "OWNER" {
			n++
		}
	}
	return n
}

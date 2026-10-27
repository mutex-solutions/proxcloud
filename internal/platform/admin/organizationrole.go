package admin

// organizationrole.go implements the full CRUD for an organization's custom
// roles (/api/v1/admin/organizations/{organizationId}/roles, the `roleDefinition` collection). NONE
// of these endpoints is registered in handler.go, so this file registers all five
// (list / get / create / update / delete). Follows the custommenu.go
// reference: id-aware CRUD via the crud.go helpers, exact perms / error strings /
// response envelopes.
//
// Every endpoint gates on ADMIN_ORGANIZATION_MANAGE_ROLES (admin:organization:manage_roles). The
// create/update/delete should write async admin audit events — deferred this pass
// (// TODO(audit)); the persisted state + responses are faithful, which is what the admin UI drives.
//
// RESPONSE SHAPE: the endpoints return the role DTO (NOT the raw stored doc). The DTO
// adds `expandedPermissions` (the role's permission patterns expanded, key set)
// and a constant `builtIn:false`. Built via organizationRoleDto() below — a null
// description / createdAt / updatedAt is omitted, the permission sets are always emitted, builtIn is a
// primitive always emitted.

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const organizationRolePerm = "admin:organization:manage_roles"

const organizationRoleCollection = "roleDefinition"

// roleNamePattern is the role-name pattern "^[A-Z][A-Z0-9_]*$" (applied to the
// upper-cased name).
var roleNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// routeOrganizationRole registers the OrganizationRole admin CRUD routes (all new — none of these is
// registered in handler.go). `{organizationId}` and `{roleId}` are fresh param names confined to this
// route subtree, so there is no chi param-name conflict with the existing `{id}` routes.
func (h *Handler) routeOrganizationRole(r chi.Router) {
	r.Get("/organizations/{organizationId}/roles", h.organizationRoleList)
	r.Post("/organizations/{organizationId}/roles", h.organizationRoleCreate)
	r.Get("/organizations/{organizationId}/roles/{roleId}", h.organizationRoleGet)
	r.Put("/organizations/{organizationId}/roles/{roleId}", h.organizationRoleUpdate)
	r.Delete("/organizations/{organizationId}/roles/{roleId}", h.organizationRoleDelete)
}

// createOrganizationRoleReq is the create-role request body (name + optional description +
// permissions). `permissions` is a pointer so an omitted set (null) is distinguishable from an empty
// one — both fail permission validation ("Permissions must not be empty").
type createOrganizationRoleReq struct {
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	Permissions *[]string `json:"permissions"`
}

// updateOrganizationRoleReq is the update-role request body (optional description + permissions;
// no name). Both pointers so a null is distinguishable from a value (a field is applied only when
// the request value is non-null).
type updateOrganizationRoleReq struct {
	Description *string   `json:"description"`
	Permissions *[]string `json:"permissions"`
}

// validateRoleName validates the role name (run against the UPPER-cased name):
// a reserved static-role name → "Cannot use reserved role name '%s'"; a name failing the pattern →
// the fixed format message.
func validateRoleName(name string) *httpx.HTTPError {
	if rbac.IsStaticRole(name) {
		return httpx.BadRequest(fmt.Sprintf("Cannot use reserved role name '%s'", name))
	}
	if !roleNamePattern.MatchString(name) {
		return httpx.BadRequest("Role name must start with a letter and contain only uppercase letters, digits, and underscores")
	}
	return nil
}

// validateRolePermissions validates the permission set: a null/empty set →
// "Permissions must not be empty"; any invalid permission → "Invalid permission: '%s'". `set` is the
// decoded pointer (nil ⇒ absent). Returns the validated slice on success.
func validateRolePermissions(set *[]string) ([]string, *httpx.HTTPError) {
	if set == nil || len(*set) == 0 {
		return nil, httpx.BadRequest("Permissions must not be empty")
	}
	for _, p := range *set {
		if !rbac.IsValidPermission(p) {
			return nil, httpx.BadRequest(fmt.Sprintf("Invalid permission: '%s'", p))
		}
	}
	return *set, nil
}

// organizationRoleDto builds the role DTO over a stored roleDefinition doc:
// `_id`→`id`, drop `_class`, add expandedPermissions (key set) + builtIn:false. A null
// description / createdAt / updatedAt is omitted; the permission sets are always present.
func organizationRoleDto(doc pgdoc.M) pgdoc.M {
	out := pgdoc.M{}
	if v, ok := doc["_id"]; ok {
		out["id"] = v
	} else if v, ok := doc["id"]; ok {
		out["id"] = v
	}
	if v, ok := doc["name"]; ok {
		out["name"] = v
	}
	if v, ok := doc["description"]; ok && v != nil {
		out["description"] = v
	}
	// permissions: always emitted (never null). Read as a []string for expansion; fall back to the
	// raw value for the response when it is not a plain string slice.
	perms := stringSlice(doc["permissions"])
	if doc["permissions"] != nil {
		out["permissions"] = doc["permissions"]
	} else {
		out["permissions"] = []string{}
	}
	out["expandedPermissions"] = rbac.ExpandPatterns(perms)
	if v, ok := doc["createdAt"]; ok && v != nil {
		out["createdAt"] = v
	}
	if v, ok := doc["updatedAt"]; ok && v != nil {
		out["updatedAt"] = v
	}
	out["builtIn"] = false
	return out
}

// (permissions value → []string coercion uses the package-shared stringSlice from adminrole.go.)

// organizationRoleList lists an org's roles as DTOs. ADMIN_ORGANIZATION_MANAGE_ROLES.
func (h *Handler) organizationRoleList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, organizationRolePerm) {
		return
	}
	orgID := chi.URLParam(r, "organizationId")
	docs, err := h.repo.OrganizationRolesByOrganization(r.Context(), orgID)
	if httpx.WriteError(w, err) {
		return
	}
	dtos := make([]pgdoc.M, 0, len(docs))
	for i := range docs {
		dtos = append(dtos, organizationRoleDto(docs[i]))
	}
	httpx.List(w, dtos)
}

// organizationRoleGet returns one of an org's roles by id → 404 "Role not found".
// (Intentionally scans the org's roles by id, so a role of another org → 404.)
func (h *Handler) organizationRoleGet(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, organizationRolePerm) {
		return
	}
	orgID := chi.URLParam(r, "organizationId")
	roleID := chi.URLParam(r, "roleId")
	docs, err := h.repo.OrganizationRolesByOrganization(r.Context(), orgID)
	if httpx.WriteError(w, err) {
		return
	}
	for i := range docs {
		if roleDocID(docs[i]) == roleID {
			httpx.OK(w, organizationRoleDto(docs[i]))
			return
		}
	}
	httpx.WriteError(w, httpx.NotFound("Role not found"))
}

// organizationRoleCreate creates a role:
//
//	upper-case the name → validate the name → validate the permissions → reject with 400 when a role
//	with that name already exists in the org → build and persist the role → return the DTO.
func (h *Handler) organizationRoleCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, organizationRolePerm) {
		return
	}
	orgID := chi.URLParam(r, "organizationId")
	var req createOrganizationRoleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	name := strings.ToUpper(req.Name)
	if verr := validateRoleName(name); verr != nil {
		httpx.WriteError(w, verr)
		return
	}
	perms, verr := validateRolePermissions(req.Permissions)
	if verr != nil {
		httpx.WriteError(w, verr)
		return
	}
	exists, err := h.repo.OrganizationRoleExistsByName(r.Context(), orgID, name)
	if httpx.WriteError(w, err) {
		return
	}
	if exists {
		httpx.WriteError(w, httpx.BadRequest(fmt.Sprintf("A role with name '%s' already exists in this organization", name)))
		return
	}
	doc := pgdoc.M{"organizationId": orgID, "name": name, "permissions": perms}
	// description: set from the request value (null when absent → omitted).
	if req.Description != nil {
		doc["description"] = *req.Description
	}
	saved, err := h.repo.InsertDoc(r.Context(), organizationRoleCollection, doc)
	if httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an async admin audit event for the role creation.
	httpx.OK(w, organizationRoleDto(saved))
}

// organizationRoleUpdate updates a role:
//
//	load by id → 404 "Organization role not found" → if permissions!=null: validate + set →
//	if description!=null: set → save → DTO.
//
// NOTE: the route carries organizationId but the update IGNORES it (looks up by
// roleId only), so a roleId belonging to another org is still updatable.
func (h *Handler) organizationRoleUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, organizationRolePerm) {
		return
	}
	roleID := chi.URLParam(r, "roleId")
	var req updateOrganizationRoleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, err := h.repo.FindDoc(r.Context(), organizationRoleCollection, roleID)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Organization role not found"))
		return
	}
	before := maps.Clone(existing)
	// permissions: only when non-null, and validated (an empty set → "Permissions must not be empty").
	if req.Permissions != nil {
		perms, verr := validateRolePermissions(req.Permissions)
		if verr != nil {
			httpx.WriteError(w, verr)
			return
		}
		existing["permissions"] = perms
	}
	// description: only when non-null (a present value, even "", is applied).
	if req.Description != nil {
		existing["description"] = *req.Description
	}
	if err := h.repo.ReplaceDoc(r.Context(), organizationRoleCollection, roleID, existing); httpx.WriteError(w, err) {
		return
	}
	// UPDATE ORGANIZATION_ROLE: record a field-level diff of the before/after snapshots.
	after, _ := h.repo.FindDoc(r.Context(), organizationRoleCollection, roleID)
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, organizationRoleDto(existing))
}

// organizationRoleDelete deletes a role:
//
//	load by id → 404 "Organization role not found" → if role.organizationId != organizationId →
//	404 "Organization role not found" → delete → respond "Successful operation".
func (h *Handler) organizationRoleDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, organizationRolePerm) {
		return
	}
	orgID := chi.URLParam(r, "organizationId")
	roleID := chi.URLParam(r, "roleId")
	existing, err := h.repo.FindDoc(r.Context(), organizationRoleCollection, roleID)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Organization role not found"))
		return
	}
	if orgIDOf(existing) != orgID {
		httpx.WriteError(w, httpx.NotFound("Organization role not found"))
		return
	}
	if _, err := h.repo.DeleteDoc(r.Context(), organizationRoleCollection, roleID); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an async admin audit event for the role deletion.
	httpx.OK(w, "Successful operation")
}

// roleDocID returns the stored doc's id as a string (the `_id` hex id, or a raw `id`/`_id`
// string) — used to match the by-id scan.
func roleDocID(doc pgdoc.M) string {
	if v, ok := doc["_id"]; ok {
		return idString(v)
	}
	if v, ok := doc["id"]; ok {
		return idString(v)
	}
	return ""
}

// orgIDOf returns the stored doc's organizationId as a string.
func orgIDOf(doc pgdoc.M) string {
	if v, ok := doc["organizationId"].(string); ok {
		return v
	}
	return ""
}

// idString renders an id value as its bare string form: a string → itself,
// any other value → its default string form.
func idString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		return fmt.Sprintf("%v", v)
	}
}

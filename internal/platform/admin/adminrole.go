package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// adminrole.go serves the admin-role MUTATIONS (/api/v1/admin/admin-roles):
// create (POST), update (PUT /{roleId}), delete (DELETE /{roleId}). The list read
// (GET /api/v1/admin/admin-roles → the 5 built-in roles) is ALREADY registered in handler.go
// (h.adminRoles) and is deliberately NOT re-registered here.
//
// create/update/delete run over the `adminRole` collection, plus its name +
// permission validation. Each mutation also writes an audit event — deferred
// this pass (// TODO(audit)); the persisted state + response envelope are faithful.
//
// Permissions: the list read gates on "admin:role:read" and
// create/update/delete on "admin:role:manage".

const adminRoleManagePerm = "admin:role:manage"

const adminRoleCollection = "adminRole"

// adminRoleNamePattern is the allowed role-name pattern: "^[A-Z][A-Z0-9_]*$".
var adminRoleNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// routeAdminRole registers the AdminRole mutation routes. The GET list is already registered in
// handler.go (h.adminRoles) so it is NOT registered here.
func (h *Handler) routeAdminRole(r chi.Router) {
	r.Post("/admin-roles", h.createAdminRole)
	r.Put("/admin-roles/{roleId}", h.updateAdminRole)
	r.Delete("/admin-roles/{roleId}", h.deleteAdminRole)
}

// createAdminRoleReq is the create request (name required, permissions non-empty).
type createAdminRoleReq struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Permissions []string `json:"permissions"`
}

// updateAdminRoleReq is the update request (both fields optional/nullable). Permissions is
// a pointer so a missing/null field (don't touch) is distinguished from an explicit [] (which is
// then rejected by validatePermissions "Permissions must not be empty").
type updateAdminRoleReq struct {
	Description *string   `json:"description"`
	Permissions *[]string `json:"permissions"`
}

// adminRoleStoredDoc builds the AdminRole persisted fields (the adminRole collection).
// name/description/permissions; createdAt/updatedAt are audit timestamps set
// here. description is omitted when blank (dropped when empty).
func adminRoleStoredDoc(name, description string, permissions []string, now time.Time) pgdoc.M {
	d := pgdoc.M{
		"name":        name,
		"permissions": permissions,
		"createdAt":   now,
		"updatedAt":   now,
	}
	if description != "" {
		d["description"] = description
	}
	return d
}

// createAdminRole creates a role: normalize name (upper), validate name +
// permissions, reject a duplicate name, then return the saved role as a single DTO.
func (h *Handler) createAdminRole(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, adminRoleManagePerm) {
		return
	}
	var req createAdminRoleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	normalizedName := strings.ToUpper(req.Name)
	if err := validateAdminRoleName(normalizedName); err != nil {
		httpx.WriteError(w, err)
		return
	}
	if err := validateAdminRolePermissions(req.Permissions); err != nil {
		httpx.WriteError(w, err)
		return
	}
	// Reject when a role with this name already exists.
	existing, err := h.repo.FindOneBy(r.Context(), adminRoleCollection, pgdoc.M{"name": normalizedName})
	if httpx.WriteError(w, err) {
		return
	}
	if existing != nil {
		httpx.WriteError(w, httpx.BadRequest(fmt.Sprintf("A role with name '%s' already exists", normalizedName)))
		return
	}
	now := time.Now()
	saved, err := h.repo.InsertDoc(r.Context(), adminRoleCollection, adminRoleStoredDoc(normalizedName, req.Description, req.Permissions, now))
	if httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event when a role is created.
	httpx.OK(w, adminRoleDtoFromDoc(saved))
}

// updateAdminRole updates a role: look up by id (404 when absent), then (if non-null) validate +
// replace permissions and (if non-null) replace description, then return the saved role as a single DTO.
func (h *Handler) updateAdminRole(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, adminRoleManagePerm) {
		return
	}
	roleID := chi.URLParam(r, "roleId")
	var req updateAdminRoleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, err := h.repo.FindDoc(r.Context(), adminRoleCollection, roleID)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Admin role not found"))
		return
	}
	if req.Permissions != nil {
		if err := validateAdminRolePermissions(*req.Permissions); err != nil {
			httpx.WriteError(w, err)
			return
		}
		existing["permissions"] = *req.Permissions
	}
	if req.Description != nil {
		existing["description"] = *req.Description
	}
	existing["updatedAt"] = time.Now()
	if err := h.repo.ReplaceDoc(r.Context(), adminRoleCollection, roleID, existing); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event when a role is updated.
	httpx.OK(w, adminRoleDtoFromDoc(existing))
}

// deleteAdminRole deletes a role: look up by id (404 when absent), reject if any adminPermission
// doc still references the role name, else delete and return "Successful operation".
func (h *Handler) deleteAdminRole(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, adminRoleManagePerm) {
		return
	}
	roleID := chi.URLParam(r, "roleId")
	existing, err := h.repo.FindDoc(r.Context(), adminRoleCollection, roleID)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Admin role not found"))
		return
	}
	name, _ := existing["name"].(string)
	usersWithRole, err := h.repo.CountAdminPermissionsByRole(r.Context(), name)
	if httpx.WriteError(w, err) {
		return
	}
	if usersWithRole > 0 {
		httpx.WriteError(w, httpx.BadRequest(fmt.Sprintf("Cannot delete role '%s' because %d user(s) are assigned to it", name, usersWithRole)))
		return
	}
	if _, err := h.repo.DeleteDoc(r.Context(), adminRoleCollection, roleID); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event when a role is deleted.
	httpx.OK(w, "Successful operation")
}

// validateAdminRoleName checks the name: a built-in name is reserved; otherwise
// it must match the allowed role-name pattern.
func validateAdminRoleName(name string) *httpx.HTTPError {
	if isBuiltInAdminRole(name) {
		return httpx.BadRequest(fmt.Sprintf("Cannot use reserved role name '%s'", name))
	}
	if !adminRoleNamePattern.MatchString(name) {
		return httpx.BadRequest("Role name must start with a letter and contain only uppercase letters, digits, and underscores")
	}
	return nil
}

// validateAdminRolePermissions checks the permission list: empty → 400; otherwise
// every entry must be a valid permission token.
func validateAdminRolePermissions(permissions []string) *httpx.HTTPError {
	if len(permissions) == 0 {
		return httpx.BadRequest("Permissions must not be empty")
	}
	for _, p := range permissions {
		if !isValidAdminPermissionToken(p) {
			return httpx.BadRequest(fmt.Sprintf("Invalid permission: '%s'", p))
		}
	}
	return nil
}

// isBuiltInAdminRole reports whether the role is one of the built-in roles.
func isBuiltInAdminRole(role string) bool {
	switch role {
	case "SUPER_ADMIN", "ADMIN", "SUPPORT", "BILLING_ADMIN", "VIEWER":
		return true
	default:
		return false
	}
}

// isValidAdminPermissionToken reports whether a token is valid: "*" (bare) is valid; an
// exact admin-permission key is valid; a "<resourceType>:*" wildcard is valid iff the resource
// type matches a known key prefix (the key text up to its last ':').
func isValidAdminPermissionToken(permission string) bool {
	if permission == "*" {
		return true
	}
	if adminPermissionKeySet[permission] {
		return true
	}
	if strings.HasSuffix(permission, ":*") {
		resourceType := permission[:len(permission)-2]
		return adminPermissionResourceTypeSet[resourceType]
	}
	return false
}

// adminPermissionKeySet is the set form of AllPermissionKeys (the valid permission keys).
var adminPermissionKeySet = func() map[string]bool {
	m := make(map[string]bool, len(AllPermissionKeys))
	for _, k := range AllPermissionKeys {
		m[k] = true
	}
	return m
}()

// adminPermissionResourceTypeSet is the valid resource types: each admin-permission
// key's text up to its last ':' (e.g. "admin:user:read" → "admin:user").
var adminPermissionResourceTypeSet = func() map[string]bool {
	m := make(map[string]bool, len(AllPermissionKeys))
	for _, k := range AllPermissionKeys {
		if i := strings.LastIndex(k, ":"); i >= 0 {
			m[k[:i]] = true
		}
	}
	return m
}()

// adminRoleDtoFromDoc builds the DTO for a CUSTOM (persisted) role: id/name/
// description/permissions + expandedPermissions (ExpandPatterns) + createdAt/updatedAt + builtIn:false.
// A null description is omitted; permissions/expandedPermissions are always emitted (the
// create/update paths guarantee a non-empty set). The id is shaped from the stored `_id`.
func adminRoleDtoFromDoc(doc pgdoc.M) customAdminRoleDto {
	dto := customAdminRoleDto{BuiltIn: false}
	if v, ok := doc["_id"]; ok {
		dto.ID = v
	}
	if v, ok := doc["name"].(string); ok {
		dto.Name = v
	}
	if v, ok := doc["description"].(string); ok && v != "" {
		dto.Description = v
	}
	perms := stringSlice(doc["permissions"])
	dto.Permissions = perms
	dto.ExpandedPermissions = ExpandPatterns(perms)
	dto.CreatedAt = doc["createdAt"]
	dto.UpdatedAt = doc["updatedAt"]
	return dto
}

// customAdminRoleDto is the DTO for a custom role (builtIn:false):
// description omitted when blank; createdAt/updatedAt omitted when nil (set on the mutation paths).
// The harness masks id/*At so their exact values are not compared.
type customAdminRoleDto struct {
	ID                  any      `json:"id,omitempty"`
	Name                string   `json:"name,omitempty"`
	Description         string   `json:"description,omitempty"`
	Permissions         []string `json:"permissions"`
	ExpandedPermissions []string `json:"expandedPermissions"`
	CreatedAt           any      `json:"createdAt,omitempty"`
	UpdatedAt           any      `json:"updatedAt,omitempty"`
	BuiltIn             bool     `json:"builtIn"`
}

// stringSlice coerces a stored permissions value (pgdoc.A of strings, or []string) to []string,
// never nil so the DTO always emits a JSON array (non-null empties are kept).
func stringSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, e := range s {
			if str, ok := e.(string); ok {
				out = append(out, str)
			}
		}
		return out
	default:
		return []string{}
	}
}

package admin

import (
	"encoding/json"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

func TestValidateRoleName(t *testing.T) {
	// reserved static role → "Cannot use reserved role name '%s'"
	if e := validateRoleName("OWNER"); e == nil || e.Msg != "Cannot use reserved role name 'OWNER'" {
		t.Errorf("reserved OWNER want reserved message, got %v", e)
	}
	if e := validateRoleName("ADMIN"); e == nil || e.Msg != "Cannot use reserved role name 'ADMIN'" {
		t.Errorf("reserved ADMIN want reserved message, got %v", e)
	}
	if e := validateRoleName("MEMBER"); e == nil {
		t.Error("reserved MEMBER must be rejected")
	}
	// bad pattern → fixed message
	const patMsg = "Role name must start with a letter and contain only uppercase letters, digits, and underscores"
	for _, bad := range []string{"1ABC", "_ABC", "AB-C", "AB C", "ABc", ""} {
		if e := validateRoleName(bad); e == nil || e.Msg != patMsg {
			t.Errorf("bad name %q want pattern message, got %v", bad, e)
		}
	}
	// valid (non-reserved, pattern-matching, upper)
	for _, ok := range []string{"BILLING_MANAGER", "DEVOPS", "A", "X1_Y2", "BILLING"} {
		if e := validateRoleName(ok); e != nil {
			t.Errorf("valid name %q want nil, got %v", ok, e)
		}
	}
}

func TestValidateRolePermissions(t *testing.T) {
	// nil set → "Permissions must not be empty"
	if _, e := validateRolePermissions(nil); e == nil || e.Msg != "Permissions must not be empty" {
		t.Errorf("nil perms want empty message, got %v", e)
	}
	// empty set → "Permissions must not be empty"
	empty := []string{}
	if _, e := validateRolePermissions(&empty); e == nil || e.Msg != "Permissions must not be empty" {
		t.Errorf("empty perms want empty message, got %v", e)
	}
	// invalid permission → "Invalid permission: '%s'"
	bad := []string{"organization:read", "not_a_perm"}
	if _, e := validateRolePermissions(&bad); e == nil || e.Msg != "Invalid permission: 'not_a_perm'" {
		t.Errorf("invalid perm want invalid message, got %v", e)
	}
	// "project:cloud_resource:*" is INVALID (prefix not a first-segment resource type).
	badWild := []string{"project:cloud_resource:*"}
	if _, e := validateRolePermissions(&badWild); e == nil || e.Msg != "Invalid permission: 'project:cloud_resource:*'" {
		t.Errorf("project:cloud_resource:* must be invalid, got %v", e)
	}
	// valid: exact key, resource:* wildcard, and global "*"
	good := []string{"organization:read", "project:*", "*"}
	got, e := validateRolePermissions(&good)
	if e != nil {
		t.Fatalf("valid perms want nil, got %v", e)
	}
	if len(got) != 3 {
		t.Errorf("validated slice should be returned unchanged, got %v", got)
	}
}

func TestCreateOrganizationRoleReqDecode(t *testing.T) {
	var req createOrganizationRoleReq
	body := `{"name":"devops","description":"d","permissions":["organization:read","project:*"]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != "devops" {
		t.Errorf("name decode mismatch: %q", req.Name)
	}
	if req.Description == nil || *req.Description != "d" {
		t.Errorf("description should be present pointer 'd', got %v", req.Description)
	}
	if req.Permissions == nil || len(*req.Permissions) != 2 {
		t.Errorf("permissions should decode to 2, got %v", req.Permissions)
	}

	// omitted optionals stay nil (null) → distinguishable from empty.
	var bare createOrganizationRoleReq
	if err := json.Unmarshal([]byte(`{"name":"X"}`), &bare); err != nil {
		t.Fatal(err)
	}
	if bare.Description != nil {
		t.Error("omitted description must stay nil")
	}
	if bare.Permissions != nil {
		t.Error("omitted permissions must stay nil (→ 'Permissions must not be empty')")
	}
}

func TestUpdateOrganizationRoleReqDecode(t *testing.T) {
	// present empty description ("") must be a non-nil pointer (it is applied).
	var req updateOrganizationRoleReq
	if err := json.Unmarshal([]byte(`{"description":"","permissions":["organization:read"]}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Description == nil || *req.Description != "" {
		t.Errorf("empty description should be a non-nil pointer to \"\", got %v", req.Description)
	}
	if req.Permissions == nil || len(*req.Permissions) != 1 {
		t.Errorf("permissions should decode to 1, got %v", req.Permissions)
	}

	// omitted → both nil (the setter is skipped).
	var bare updateOrganizationRoleReq
	if err := json.Unmarshal([]byte(`{}`), &bare); err != nil {
		t.Fatal(err)
	}
	if bare.Description != nil || bare.Permissions != nil {
		t.Errorf("omitted update fields must stay nil, got %+v", bare)
	}
}

func TestOrganizationRoleDto(t *testing.T) {
	// full doc: permissions expand, builtIn false, description present.
	doc := pgdoc.M{
		"_id":         "abc123",
		"name":        "DEVOPS",
		"description": "team role",
		"permissions": pgdoc.A{"project:*"},
		"_class":      "OrganizationRole",
	}
	dto := organizationRoleDto(doc)
	if dto["id"] != "abc123" {
		t.Errorf("_id must map to id, got %v", dto["id"])
	}
	if _, ok := dto["_id"]; ok {
		t.Error("_id must be dropped")
	}
	if _, ok := dto["_class"]; ok {
		t.Error("_class must be dropped")
	}
	if dto["name"] != "DEVOPS" {
		t.Errorf("name mismatch: %v", dto["name"])
	}
	if dto["description"] != "team role" {
		t.Errorf("description mismatch: %v", dto["description"])
	}
	if dto["builtIn"] != false {
		t.Errorf("builtIn must be constant false, got %v", dto["builtIn"])
	}
	exp, ok := dto["expandedPermissions"].([]string)
	if !ok || len(exp) == 0 {
		t.Fatalf("expandedPermissions must be a non-empty []string, got %#v", dto["expandedPermissions"])
	}
	// project:* expands to every project:* key.
	for _, k := range exp {
		if len(k) < 8 || k[:8] != "project:" {
			t.Errorf("project:* should expand only to project keys, got %q", k)
		}
	}
}

func TestOrganizationRoleDtoNonNull(t *testing.T) {
	// no description / no timestamps → those keys are omitted; permission sets always present.
	doc := pgdoc.M{"_id": "x", "name": "R", "permissions": pgdoc.A{}}
	dto := organizationRoleDto(doc)
	if _, ok := dto["description"]; ok {
		t.Error("absent description must be omitted")
	}
	if _, ok := dto["createdAt"]; ok {
		t.Error("absent createdAt must be omitted")
	}
	if _, ok := dto["updatedAt"]; ok {
		t.Error("absent updatedAt must be omitted")
	}
	if _, ok := dto["permissions"]; !ok {
		t.Error("permissions must always be emitted (non-null set)")
	}
	if exp, ok := dto["expandedPermissions"].([]string); !ok || exp == nil {
		t.Errorf("expandedPermissions must be a non-nil []string even when empty, got %#v", dto["expandedPermissions"])
	}
	if dto["builtIn"] != false {
		t.Error("builtIn must always be emitted as false")
	}
	// explicit null description in the stored doc must also be omitted.
	docNull := pgdoc.M{"_id": "y", "name": "R", "permissions": pgdoc.A{}, "description": nil}
	if _, ok := organizationRoleDto(docNull)["description"]; ok {
		t.Error("null description must be omitted")
	}
}

func TestRoleDocIDAndOrgID(t *testing.T) {
	if got := roleDocID(pgdoc.M{"_id": "rid"}); got != "rid" {
		t.Errorf("roleDocID _id mismatch: %q", got)
	}
	if got := roleDocID(pgdoc.M{"id": "rid2"}); got != "rid2" {
		t.Errorf("roleDocID id mismatch: %q", got)
	}
	if got := orgIDOf(pgdoc.M{"organizationId": "org1"}); got != "org1" {
		t.Errorf("orgIDOf mismatch: %q", got)
	}
	if got := orgIDOf(pgdoc.M{}); got != "" {
		t.Errorf("orgIDOf absent want empty, got %q", got)
	}
}

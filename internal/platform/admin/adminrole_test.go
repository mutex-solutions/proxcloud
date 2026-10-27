package admin

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

func fixedTime() time.Time { return time.Date(2026, 6, 27, 0, 0, 0, 0, time.UTC) }

func TestCreateAdminRoleReqDecode(t *testing.T) {
	var req createAdminRoleReq
	if err := json.Unmarshal([]byte(`{"name":"ops","description":"d","permissions":["admin:user:read","admin:bill:*"]}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != "ops" || req.Description != "d" {
		t.Errorf("decoded scalar mismatch: %+v", req)
	}
	if !reflect.DeepEqual(req.Permissions, []string{"admin:user:read", "admin:bill:*"}) {
		t.Errorf("decoded permissions mismatch: %#v", req.Permissions)
	}
}

func TestUpdateAdminRoleReqDecode_distinguishesMissingFromExplicit(t *testing.T) {
	// Missing fields → nil pointers (don't touch).
	var missing updateAdminRoleReq
	if err := json.Unmarshal([]byte(`{}`), &missing); err != nil {
		t.Fatal(err)
	}
	if missing.Description != nil || missing.Permissions != nil {
		t.Errorf("missing fields must decode to nil pointers: %+v", missing)
	}
	// Explicit values → non-nil pointers.
	var present updateAdminRoleReq
	if err := json.Unmarshal([]byte(`{"description":"x","permissions":["admin:user:read"]}`), &present); err != nil {
		t.Fatal(err)
	}
	if present.Description == nil || *present.Description != "x" {
		t.Errorf("description pointer mismatch: %+v", present.Description)
	}
	if present.Permissions == nil || !reflect.DeepEqual(*present.Permissions, []string{"admin:user:read"}) {
		t.Errorf("permissions pointer mismatch: %#v", present.Permissions)
	}
	// Explicit empty permissions [] → non-nil empty slice (validate then rejects it).
	var empty updateAdminRoleReq
	if err := json.Unmarshal([]byte(`{"permissions":[]}`), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Permissions == nil || len(*empty.Permissions) != 0 {
		t.Errorf("explicit [] must be non-nil empty: %#v", empty.Permissions)
	}
}

func TestValidateAdminRoleName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
		wantMsg string
	}{
		{"valid", "OPERATIONS", false, ""},
		{"valid-digits-underscore", "OPS_TEAM_2", false, ""},
		{"reserved-super", "SUPER_ADMIN", true, "Cannot use reserved role name 'SUPER_ADMIN'"},
		{"reserved-viewer", "VIEWER", true, "Cannot use reserved role name 'VIEWER'"},
		{"lowercase", "ops", true, "Role name must start with a letter and contain only uppercase letters, digits, and underscores"},
		{"leading-digit", "2OPS", true, "Role name must start with a letter and contain only uppercase letters, digits, and underscores"},
		{"empty", "", true, "Role name must start with a letter and contain only uppercase letters, digits, and underscores"},
		{"hyphen", "OPS-TEAM", true, "Role name must start with a letter and contain only uppercase letters, digits, and underscores"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateAdminRoleName(c.input)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", c.input)
				}
				if err.Status != http.StatusBadRequest {
					t.Errorf("status=%d want 400", err.Status)
				}
				if err.Msg != c.wantMsg {
					t.Errorf("msg=%q want %q", err.Msg, c.wantMsg)
				}
			} else if err != nil {
				t.Fatalf("unexpected error for %q: %v", c.input, err)
			}
		})
	}
}

func TestValidateAdminRolePermissions(t *testing.T) {
	cases := []struct {
		name    string
		input   []string
		wantErr bool
		wantMsg string
	}{
		{"empty", []string{}, true, "Permissions must not be empty"},
		{"nil", nil, true, "Permissions must not be empty"},
		{"star", []string{"*"}, false, ""},
		{"exact-key", []string{"admin:user:read"}, false, ""},
		{"resource-wildcard", []string{"admin:bill:*"}, false, ""},
		{"mixed-valid", []string{"admin:user:read", "admin:transaction:*", "*"}, false, ""},
		{"unknown-key", []string{"admin:bogus:read"}, true, "Invalid permission: 'admin:bogus:read'"},
		{"unknown-wildcard", []string{"admin:bogus:*"}, true, "Invalid permission: 'admin:bogus:*'"},
		{"bare-token", []string{"read"}, true, "Invalid permission: 'read'"},
		{"first-bad-wins", []string{"admin:user:read", "bad"}, true, "Invalid permission: 'bad'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateAdminRolePermissions(c.input)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %#v", c.input)
				}
				if err.Status != http.StatusBadRequest {
					t.Errorf("status=%d want 400", err.Status)
				}
				if err.Msg != c.wantMsg {
					t.Errorf("msg=%q want %q", err.Msg, c.wantMsg)
				}
			} else if err != nil {
				t.Fatalf("unexpected error for %#v: %v", c.input, err)
			}
		})
	}
}

func TestIsValidAdminPermissionToken_resourceTypeSet(t *testing.T) {
	// Every resource-type wildcard derived from a known key must validate.
	for _, k := range AllPermissionKeys {
		i := lastColon(k)
		rt := k[:i]
		if !isValidAdminPermissionToken(rt + ":*") {
			t.Errorf("resource wildcard %q should be valid", rt+":*")
		}
		if !isValidAdminPermissionToken(k) {
			t.Errorf("exact key %q should be valid", k)
		}
	}
	if isValidAdminPermissionToken("admin:*") {
		// "admin:*" → resourceType "admin"; "admin" is not a key prefix (every key has >=2 colons),
		// so this is NOT a valid token per isValidPermission. Guard against accidental accept.
		t.Errorf(`"admin:*" must NOT be a valid permission token (resourceType "admin" is unknown)`)
	}
}

func lastColon(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}

func TestAdminRoleStoredDocOmitsBlankDescription(t *testing.T) {
	d := adminRoleStoredDoc("OPS", "", []string{"admin:user:read"}, fixedTime())
	if _, ok := d["description"]; ok {
		t.Errorf("blank description must be omitted, got %#v", d["description"])
	}
	d = adminRoleStoredDoc("OPS", "desc", []string{"admin:user:read"}, fixedTime())
	if d["description"] != "desc" {
		t.Errorf("description=%#v want %q", d["description"], "desc")
	}
	if d["name"] != "OPS" {
		t.Errorf("name=%#v want OPS", d["name"])
	}
}

func TestAdminRoleDtoFromDoc(t *testing.T) {
	doc := pgdoc.M{
		"_id":         "abc123",
		"name":        "OPS",
		"description": "desc",
		"permissions": pgdoc.A{"admin:user:read"},
		"createdAt":   fixedTime(),
		"updatedAt":   fixedTime(),
	}
	dto := adminRoleDtoFromDoc(doc)
	if dto.BuiltIn {
		t.Errorf("custom role must have builtIn=false")
	}
	if dto.Name != "OPS" || dto.Description != "desc" {
		t.Errorf("scalar mismatch: %+v", dto)
	}
	if !reflect.DeepEqual(dto.Permissions, []string{"admin:user:read"}) {
		t.Errorf("permissions=%#v", dto.Permissions)
	}
	// expandedPermissions = ExpandPatterns(permissions): an exact key expands to itself.
	if !reflect.DeepEqual(dto.ExpandedPermissions, []string{"admin:user:read"}) {
		t.Errorf("expandedPermissions=%#v want [admin:user:read]", dto.ExpandedPermissions)
	}

	// A wildcard expands to all matching keys (must contain at least the read+create user keys).
	wild := adminRoleDtoFromDoc(pgdoc.M{"name": "OPS", "permissions": pgdoc.A{"admin:user:*"}})
	if len(wild.ExpandedPermissions) < 2 {
		t.Errorf("admin:user:* should expand to multiple keys, got %#v", wild.ExpandedPermissions)
	}
}

func TestAdminRoleDtoFromDoc_jsonAlwaysEmitsArrays(t *testing.T) {
	// permissions + expandedPermissions are always present (non-null), even empty.
	dto := adminRoleDtoFromDoc(pgdoc.M{"name": "OPS"})
	b, _ := json.Marshal(dto)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["permissions"]; !ok {
		t.Errorf("permissions must always be emitted: %s", b)
	}
	if _, ok := m["expandedPermissions"]; !ok {
		t.Errorf("expandedPermissions must always be emitted: %s", b)
	}
	if _, ok := m["description"]; ok {
		t.Errorf("blank description must be omitted: %s", b)
	}
	if _, ok := m["builtIn"]; !ok {
		t.Errorf("builtIn primitive must always be emitted: %s", b)
	}
}

func TestStringSlice(t *testing.T) {
	if got := stringSlice(pgdoc.A{"a", "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("pgdoc.A → %#v", got)
	}
	if got := stringSlice([]string{"x"}); !reflect.DeepEqual(got, []string{"x"}) {
		t.Errorf("[]string → %#v", got)
	}
	if got := stringSlice(nil); got == nil || len(got) != 0 {
		t.Errorf("nil → must be non-nil empty, got %#v", got)
	}
}

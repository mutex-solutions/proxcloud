package org

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
)

// A role id owned by another org must be invisible to Update/Delete (org-scope guard, finding [9]).
func TestRoleInOrg_crossOrgRejected(t *testing.T) {
	foreign := &Role{ID: "r1", OrganizationID: "orgB"}
	if err := roleInOrg(foreign, "orgA"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("cross-org role = %v, want not-found", err)
	}
	if err := roleInOrg(nil, "orgA"); err == nil {
		t.Error("nil role must be not-found")
	}
	if err := roleInOrg(&Role{OrganizationID: "orgA"}, "orgA"); err != nil {
		t.Errorf("same-org role = %v, want nil", err)
	}
}

// Only OWNER/ADMIN require the extra manage_roles gate on addMember (privilege-escalation guard,
// finding [31]).
func TestIsPrivilegedRole(t *testing.T) {
	for _, r := range []string{rbac.RoleOwner, rbac.RoleAdmin} {
		if !isPrivilegedRole(r) {
			t.Errorf("isPrivilegedRole(%q) = false, want true", r)
		}
	}
	for _, r := range []string{rbac.RoleMember, "FINANCE", "", "owner"} {
		if isPrivilegedRole(r) {
			t.Errorf("isPrivilegedRole(%q) = true, want false", r)
		}
	}
}

func TestValidateRoleName(t *testing.T) {
	// reserved (static) names rejected
	for _, n := range []string{"OWNER", "ADMIN", "MEMBER"} {
		if err := validateRoleName(n); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("validateRoleName(%q) = %v, want reserved error", n, err)
		}
	}
	// pattern failures
	for _, n := range []string{"1FOO", "FOO BAR", "FOO-BAR", "_FOO"} {
		if err := validateRoleName(n); err == nil || !strings.Contains(err.Error(), "must start with a letter") {
			t.Errorf("validateRoleName(%q) = %v, want pattern error", n, err)
		}
	}
	// valid
	for _, n := range []string{"FINANCE", "BILLING", "FOO_BAR2"} {
		if err := validateRoleName(n); err != nil {
			t.Errorf("validateRoleName(%q) = %v, want nil", n, err)
		}
	}
}

func TestValidateRolePermissions(t *testing.T) {
	if err := validateRolePermissions(nil); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Errorf("empty perms = %v, want must-not-be-empty", err)
	}
	if err := validateRolePermissions([]string{"organization:read", "project:*"}); err != nil {
		t.Errorf("valid perms = %v, want nil", err)
	}
	if err := validateRolePermissions([]string{"organization:read", "bogus"}); err == nil || !strings.Contains(err.Error(), "Invalid permission: 'bogus'") {
		t.Errorf("bad perm = %v, want invalid-permission", err)
	}
}

func TestRoleDtoStaticShape(t *testing.T) {
	b, _ := json.Marshal(roleDtoFromStatic("OWNER"))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["builtIn"] != true {
		t.Errorf("builtIn = %v, want true", m["builtIn"])
	}
	if m["id"] != "OWNER" || m["name"] != "OWNER" {
		t.Errorf("id/name = %v/%v", m["id"], m["name"])
	}
	// static role: no description / timestamps (omitted when null)
	for _, k := range []string{"description", "createdAt", "updatedAt"} {
		if _, ok := m[k]; ok {
			t.Errorf("static role should omit %q", k)
		}
	}
	perms, _ := m["permissions"].([]any)
	if len(perms) != 3 {
		t.Errorf("OWNER permissions = %v, want 3 patterns", m["permissions"])
	}
	if _, ok := m["expandedPermissions"].([]any); !ok {
		t.Errorf("expandedPermissions missing/!array: %v", m["expandedPermissions"])
	}
}

func TestRoleDtoCustomShape(t *testing.T) {
	b, _ := json.Marshal(roleDtoFromRole(&Role{ID: "abc", Name: "FINANCE", Description: "d", Permissions: []string{"project:create"}}))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["builtIn"] != false {
		t.Errorf("builtIn = %v, want false", m["builtIn"])
	}
	if m["description"] != "d" {
		t.Errorf("description = %v, want d", m["description"])
	}
	if exp, _ := m["expandedPermissions"].([]any); len(exp) != 1 || exp[0] != "project:create" {
		t.Errorf("expandedPermissions = %v, want [project:create]", m["expandedPermissions"])
	}
}

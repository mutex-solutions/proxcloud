package rbac

import (
	"reflect"
	"testing"
)

func TestMatchesPattern(t *testing.T) {
	cases := []struct {
		pattern, key string
		want         bool
	}{
		{"*", "organization:read", true},
		{"organization:read", "organization:read", true},
		{"organization:read", "organization:update", false},
		{"project:*", "project:create", true},
		{"project:*", "project:cloud_resource:read", true},
		{"project:*", "billing_profile:read", false},
		{"project:cloud_resource:*", "project:cloud_resource:read", true},
		{"project:cloud_resource:*", "project:create", false}, // narrower wildcard
		{"", "project:create", false},
		{"project:*", "", false},
	}
	for _, c := range cases {
		if got := MatchesPattern(c.pattern, c.key); got != c.want {
			t.Errorf("MatchesPattern(%q,%q)=%v want %v", c.pattern, c.key, got, c.want)
		}
	}
}

func TestRoleHasPermission(t *testing.T) {
	cases := []struct {
		role, key string
		want      bool
	}{
		{RoleOwner, OrganizationDelete, true},
		{RoleOwner, BillingProfileAddFunds, true},
		{RoleAdmin, OrganizationUpdate, true},
		{RoleAdmin, OrganizationDelete, false}, // admin can't delete org (owner-only)
		{RoleAdmin, ProjectDelete, true},       // project:* covers it
		{RoleMember, OrganizationRead, true},
		{RoleMember, OrganizationUpdate, false},
		{RoleMember, ProjectCreate, true},
		{RoleMember, ProjectUpdate, false}, // only project:create + cloud_resource:*
		{RoleMember, ProjectCloudResourceManage, true},
		{RoleBilling, OrganizationRead, false}, // BILLING is not a static perm role
		{"NOPE", OrganizationRead, false},
	}
	for _, c := range cases {
		if got := RoleHasPermission(c.role, c.key); got != c.want {
			t.Errorf("RoleHasPermission(%q,%q)=%v want %v", c.role, c.key, got, c.want)
		}
	}
}

func TestRolePermissionsRaw(t *testing.T) {
	owner := RolePermissions(RoleOwner)
	want := []string{"organization:*", "project:*", "billing_profile:*"}
	if !reflect.DeepEqual(owner, want) {
		t.Errorf("OWNER patterns = %v, want %v", owner, want)
	}
	if len(RolePermissions("CUSTOM")) != 0 {
		t.Errorf("unknown role should have no static perms")
	}
}

func TestExpandPatterns_OwnerCoversAll(t *testing.T) {
	got := ExpandPatterns(RolePermissions(RoleOwner))
	if len(got) != len(AllPermissions) {
		t.Errorf("OWNER expands to %d perms, want all %d", len(got), len(AllPermissions))
	}
}

func TestExpandPatterns_Member(t *testing.T) {
	got := ExpandPatterns(RolePermissions(RoleMember))
	// MEMBER: organization:read, project:create, project:cloud_resource:{read,manage,api_access},
	// billing_profile:{read,read_invoices,download_invoices,read_transactions} = 9 keys.
	if len(got) != 9 {
		t.Errorf("MEMBER expands to %d perms, want 9: %v", len(got), got)
	}
}

func TestIsStaticRole(t *testing.T) {
	for _, r := range []string{RoleOwner, RoleAdmin, RoleMember} {
		if !IsStaticRole(r) {
			t.Errorf("%s should be static", r)
		}
	}
	for _, r := range []string{RoleBilling, "CUSTOM", ""} {
		if IsStaticRole(r) {
			t.Errorf("%s should NOT be static", r)
		}
	}
}

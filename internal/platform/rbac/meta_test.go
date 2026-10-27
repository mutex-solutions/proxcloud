package rbac

import "testing"

func TestResourceType(t *testing.T) {
	cases := map[string]string{
		"organization:read":           "organization",
		"project:create":              "project",
		"project:cloud_resource:read": "project",
		"billing_profile:read":        "billing_profile",
	}
	for in, want := range cases {
		if got := ResourceType(in); got != want {
			t.Errorf("ResourceType(%q)=%q want %q", in, got, want)
		}
	}
}

func TestIsValidPermission(t *testing.T) {
	valid := []string{"*", "organization:read", "project:*", "billing_profile:*", "project:cloud_resource:read", "organization:*"}
	for _, p := range valid {
		if !IsValidPermission(p) {
			t.Errorf("IsValidPermission(%q)=false, want true", p)
		}
	}
	// "project:cloud_resource:*" is INVALID (prefix not a first-segment resource
	// type).
	invalid := []string{"project:cloud_resource:*", "bogus", "bogus:*", "", "organization", "project:bogus"}
	for _, p := range invalid {
		if IsValidPermission(p) {
			t.Errorf("IsValidPermission(%q)=true, want false", p)
		}
	}
}

func TestAllPermissionMeta(t *testing.T) {
	meta := AllPermissionMeta()
	if len(meta) != len(AllPermissions) {
		t.Fatalf("meta len=%d want %d", len(meta), len(AllPermissions))
	}
	if meta[0].Key != "organization:read" || meta[0].ResourceType != "organization" || meta[0].Description == "" {
		t.Errorf("meta[0]=%+v", meta[0])
	}
}

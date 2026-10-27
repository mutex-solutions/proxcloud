package admin

import (
	"encoding/json"
	"testing"
)

func TestGrantAdminPermissionRequestDecode(t *testing.T) {
	var req grantAdminPermissionRequest
	if err := json.Unmarshal([]byte(`{"sub":"ada@example.com","role":"SUPPORT"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Sub != "ada@example.com" || req.Role != "SUPPORT" {
		t.Errorf("decoded req mismatch: %+v", req)
	}
}

func TestUpdateAdminPermissionRequestDecode(t *testing.T) {
	var req updateAdminPermissionRequest
	if err := json.Unmarshal([]byte(`{"role":"BILLING_ADMIN"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Role != "BILLING_ADMIN" {
		t.Errorf("decoded req mismatch: %+v", req)
	}
}

// adminPermissionToView must emit `sub` (NOT `id`) for the id-as-sub field, always emit `pending`
// (primitive), and OMIT blank email/role (null fields dropped).
func TestAdminPermissionToViewBlankFieldsOmitted(t *testing.T) {
	v := adminPermissionToView(&AdminPermission{Sub: "u1", Pending: false})
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if got != `{"sub":"u1","pending":false}` {
		t.Errorf("blank email/role must be omitted, pending kept; got %s", got)
	}
}

func TestAdminPermissionToViewFull(t *testing.T) {
	v := adminPermissionToView(&AdminPermission{Sub: "u1", Email: "ada@example.com", Role: "ADMIN", Pending: true})
	var m map[string]any
	b, _ := json.Marshal(v)
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["sub"] != "u1" || m["email"] != "ada@example.com" || m["role"] != "ADMIN" || m["pending"] != true {
		t.Errorf("full view mismatch: %v", m)
	}
	if _, hasID := m["id"]; hasID {
		t.Errorf("must not emit `id` (the id field serializes as `sub`): %v", m)
	}
}

// A nil permission maps to the zero view (defensive; pending=false, all omitted).
func TestAdminPermissionToViewNil(t *testing.T) {
	b, _ := json.Marshal(adminPermissionToView(nil))
	if string(b) != `{"pending":false}` {
		t.Errorf("nil → zero view; got %s", b)
	}
}

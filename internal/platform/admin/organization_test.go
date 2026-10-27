package admin

// organization_test.go — PURE unit tests for the organization admin controller (no datastore/network):
// request decoding (pointer-vs-value semantics for the optional update/role fields), the
// orgMemberRole / userSub / orgHexID helpers, and the organizationMemberDto null-omitting JSON shape.

import (
	"encoding/json"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

func TestCreateOrganizationReqDecode(t *testing.T) {
	var req createOrganizationReq
	body := `{"name":"Acme","description":"d","ownerSub":"sub-1","billingProfileId":"bp-1","createBillingProfile":true}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != "Acme" || req.Description != "d" || req.OwnerSub != "sub-1" {
		t.Errorf("decode mismatch: %+v", req)
	}
	if req.BillingProfileID != "bp-1" || !req.CreateBillingProfile {
		t.Errorf("bp/createBillingProfile mismatch: %+v", req)
	}
}

func TestCreateOrganizationReqDefaults(t *testing.T) {
	var req createOrganizationReq
	if err := json.Unmarshal([]byte(`{"name":"X"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.CreateBillingProfile {
		t.Error("createBillingProfile must default false")
	}
	if req.OwnerSub != "" || req.BillingProfileID != "" {
		t.Errorf("optional fields must be empty: %+v", req)
	}
}

func TestUpdateOrganizationReqPointerSemantics(t *testing.T) {
	// Only `name` present → Name non-nil, Description/BillingProfileID nil (so they are NOT touched).
	var req updateOrganizationReq
	if err := json.Unmarshal([]byte(`{"name":"New"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Name == nil || *req.Name != "New" {
		t.Errorf("name should be non-nil 'New', got %v", req.Name)
	}
	if req.Description != nil {
		t.Error("omitted description must stay nil (no change applied)")
	}
	if req.BillingProfileID != nil {
		t.Error("omitted billingProfileId must stay nil (no change applied)")
	}

	// Explicit empty string is a real value (clear the field) → non-nil pointer to "".
	var req2 updateOrganizationReq
	if err := json.Unmarshal([]byte(`{"description":""}`), &req2); err != nil {
		t.Fatal(err)
	}
	if req2.Description == nil || *req2.Description != "" {
		t.Errorf("explicit empty description should be a non-nil pointer to \"\", got %v", req2.Description)
	}
}

func TestAddOrganizationMemberReqDecode(t *testing.T) {
	var req addOrganizationMemberReq
	if err := json.Unmarshal([]byte(`{"userId":"u1","role":"ADMIN"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.UserID != "u1" || req.Role != "ADMIN" {
		t.Errorf("decode mismatch: %+v", req)
	}
}

func TestUpdateOrganizationMemberRoleReqDecode(t *testing.T) {
	var req updateOrganizationMemberRoleReq
	if err := json.Unmarshal([]byte(`{"role":"MEMBER"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Role != "MEMBER" {
		t.Errorf("role should be MEMBER, got %q", req.Role)
	}
}

func TestOrgMemberRole(t *testing.T) {
	// roles stored as a pgdoc.A (array) → role is the first element.
	if got := orgMemberRole(pgdoc.M{"roles": pgdoc.A{"OWNER", "ADMIN"}}); got != "OWNER" {
		t.Errorf("orgMemberRole=%q want OWNER", got)
	}
	if got := orgMemberRole(pgdoc.M{"roles": pgdoc.A{}}); got != "" {
		t.Errorf("empty roles → \"\", got %q", got)
	}
	if got := orgMemberRole(pgdoc.M{}); got != "" {
		t.Errorf("missing roles → \"\", got %q", got)
	}
	if got := orgMemberRole(pgdoc.M{"roles": pgdoc.A{42}}); got != "" {
		t.Errorf("non-string role element → \"\", got %q", got)
	}
}

func TestUserSub(t *testing.T) {
	if got := userSub(pgdoc.M{"sub": "abc"}); got != "abc" {
		t.Errorf("userSub=%q want abc", got)
	}
	if got := userSub(pgdoc.M{}); got != "" {
		t.Errorf("missing sub → \"\", got %q", got)
	}
	if got := userSub(nil); got != "" {
		t.Errorf("nil doc → \"\", got %q", got)
	}
}

func TestOrgHexID(t *testing.T) {
	if got := orgHexID("64aabbccddeeff0011223344"); got != "64aabbccddeeff0011223344" {
		t.Errorf("string id passthrough mismatch: %q", got)
	}
	oid := "64aabbccddeeff0011223344"
	if got := orgHexID(oid); got != "64aabbccddeeff0011223344" {
		t.Errorf("hex id mismatch: %q", got)
	}
	if got := orgHexID(nil); got != "" {
		t.Errorf("nil → \"\", got %q", got)
	}
}

func TestOrganizationMemberDtoNonNull(t *testing.T) {
	// All optional → an empty object (every field omitted under omitempty).
	b, err := json.Marshal(organizationMemberDto{})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "{}" {
		t.Errorf("empty member dto should marshal to {}, got %s", b)
	}
	// Populated → all five keys present.
	b, err = json.Marshal(organizationMemberDto{Sub: "s", FirstName: "F", LastName: "L", Email: "e@x", Role: "OWNER"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"sub", "firstName", "lastName", "email", "role"} {
		if _, ok := m[k]; !ok {
			t.Errorf("populated member dto missing key %q: %s", k, b)
		}
	}
}

package project

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
)

// A billing profile from a DIFFERENT organization must never be attachable to a project
// (changeBillingProfile cross-org guard, finding [4]).
func TestSameOrgBillingProfile_crossOrgRejected(t *testing.T) {
	proj := &Project{OrganizationID: "orgA"}
	foreign := &billing.BillingProfile{OrganizationID: "orgB"}
	if sameOrgBillingProfile(foreign, proj) {
		t.Error("billing profile from orgB must be rejected for an orgA project")
	}
	same := &billing.BillingProfile{OrganizationID: "orgA"}
	if !sameOrgBillingProfile(same, proj) {
		t.Error("billing profile from the project's own org must be allowed")
	}
	if sameOrgBillingProfile(nil, proj) || sameOrgBillingProfile(same, nil) {
		t.Error("nil target/project must be rejected")
	}
}

// Project must serialize with every field present, data == null
// when absent, and customInfo/services/memberships are non-null ({}/[]/[]).
func TestProjectMarshalJSON_emptyShape(t *testing.T) {
	b, err := json.Marshal(Project{Name: "p", Status: StatusDisabled, OrganizationID: "org1", Owner: "subA"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Null fields are OMITTED.
	for _, k := range []string{"data", "billingProfileId", "scheduledForDeletionAt"} {
		if _, ok := m[k]; ok {
			t.Errorf("key %q should be omitted when null, got %v", k, m[k])
		}
	}
	// Non-null empty collections are KEPT (omit null, not empty).
	if ci, ok := m["customInfo"].(map[string]any); !ok || len(ci) != 0 {
		t.Errorf("customInfo: want {} , got %v (%T)", m["customInfo"], m["customInfo"])
	}
	if sv, ok := m["services"].([]any); !ok || len(sv) != 0 {
		t.Errorf("services: want [], got %v (%T)", m["services"], m["services"])
	}
	if ms, ok := m["memberships"].([]any); !ok || len(ms) != 0 {
		t.Errorf("memberships: want [], got %v (%T)", m["memberships"], m["memberships"])
	}
	for _, k := range []string{"name", "status", "owner", "organizationId"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
}

func TestProjectMarshalJSON_members(t *testing.T) {
	b, _ := json.Marshal(Project{
		Name: "p", Status: StatusEnabled, OrganizationID: "o",
		Memberships: []Membership{{Sub: "subA", Role: RoleOwner}},
	})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	ms, _ := m["memberships"].([]any)
	if len(ms) != 1 {
		t.Fatalf("want 1 membership, got %v", m["memberships"])
	}
	first, _ := ms[0].(map[string]any)
	if first["sub"] != "subA" || first["role"] != "OWNER" {
		t.Errorf("membership shape = %v", first)
	}
}

func TestProjectViewMarshalJSON_arraysPresent(t *testing.T) {
	b, _ := json.Marshal(toView(&Project{Name: "p", Status: StatusDisabled, OrganizationID: "o"}))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if rc, ok := m["resourcesCount"].([]any); !ok || len(rc) != 0 {
		t.Errorf("resourcesCount: want [], got %v (%T)", m["resourcesCount"], m["resourcesCount"])
	}
	if _, ok := m["memberships"].([]any); !ok {
		t.Errorf("memberships: want [] present, got %v", m["memberships"])
	}
}

func TestMapRole(t *testing.T) {
	cases := map[string]string{
		"OWNER":   RoleOwner,
		"ADMIN":   RoleOwner,
		"MEMBER":  RoleMember,
		"BILLING": RoleMember,
		"":        RoleMember,
	}
	for in, want := range cases {
		if got := mapRole(in); got != want {
			t.Errorf("mapRole(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToMemberships_allMembers(t *testing.T) {
	members := []org.Member{
		{Sub: "a", Roles: []string{"OWNER"}},
		{Sub: "b", Roles: []string{"MEMBER"}},
	}
	got := toMemberships(members, "a", nil)
	want := []Membership{{Sub: "a", Role: RoleOwner}, {Sub: "b", Role: RoleMember}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("toMemberships(all) = %v, want %v", got, want)
	}
}

func TestToMemberships_filteredKeepsRequester(t *testing.T) {
	members := []org.Member{
		{Sub: "a", Roles: []string{"OWNER"}},
		{Sub: "b", Roles: []string{"MEMBER"}},
		{Sub: "c", Roles: []string{"MEMBER"}},
	}
	// memberSubs selects b; requester a must be auto-included; c excluded.
	got := toMemberships(members, "a", []string{"b"})
	subs := map[string]bool{}
	for _, m := range got {
		subs[m.Sub] = true
	}
	if !subs["a"] || !subs["b"] || subs["c"] || len(got) != 2 {
		t.Errorf("filtered toMemberships = %v, want {a,b}", got)
	}
}

func TestIsUserOwnerAndMember(t *testing.T) {
	p := &Project{Memberships: []Membership{{Sub: "a", Role: RoleOwner}, {Sub: "b", Role: RoleMember}}}
	if !p.IsUserOwner("a") {
		t.Error("a should be owner")
	}
	if p.IsUserOwner("b") {
		t.Error("b should not be owner")
	}
	if !p.IsMember("b") || !p.IsMember("a") {
		t.Error("a,b should be members")
	}
	if p.IsMember("c") {
		t.Error("c should not be a member")
	}
}

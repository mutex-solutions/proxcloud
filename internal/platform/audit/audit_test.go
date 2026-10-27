package audit

import (
	"encoding/json"
	"testing"
)

func TestAuditEventJSONShape(t *testing.T) {
	ev := ClientUserEvent("sub1", "Ada Lovelace")
	ev.EventContext = ContextOrganization
	ev.Action = ActionCreate
	ev.ResourceType = ResourceOrganization
	ev.ResourceID = "org1"
	ev.ResourceDisplayName = "Acme"
	ev.OrganizationID = "org1"
	ev.Outcome = OutcomeSuccess

	b, _ := json.Marshal(ev)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// archived is a primitive bool → ALWAYS present (no omitempty), even when false.
	if v, ok := m["archived"]; !ok || v != false {
		t.Errorf("archived: want present false, got ok=%v val=%v", ok, v)
	}
	if m["requestInterface"] != "CLIENT_AREA" || m["action"] != "CREATE" || m["resourceType"] != "ORGANIZATION" {
		t.Errorf("core fields wrong: %v", m)
	}
	// nil fields omitted.
	for _, k := range []string{"timestamp", "errorMessage", "changes", "resourceMetadata", "projectId"} {
		if _, ok := m[k]; ok {
			t.Errorf("nil field %q should be omitted, got %v", k, m[k])
		}
	}
	actor, ok := m["actor"].(map[string]any)
	if !ok || actor["type"] != "USER" || actor["id"] != "sub1" || actor["displayName"] != "Ada Lovelace" {
		t.Errorf("actor shape wrong: %v", m["actor"])
	}
	// actor volatile fields (ip/userAgent) omitted when unset.
	if _, ok := actor["ipAddress"]; ok {
		t.Errorf("actor.ipAddress should be omitted when unset")
	}
}

func TestFilterCriteria(t *testing.T) {
	f := Filter{OrganizationID: "o1", RequestInterface: "CLIENT_AREA", ResourceType: "ORGANIZATION", Action: "CREATE", ActorID: "subX"}
	c := f.criteria()
	if c["organizationId"] != "o1" || c["requestInterface"] != "CLIENT_AREA" || c["resourceType"] != "ORGANIZATION" || c["action"] != "CREATE" {
		t.Errorf("criteria mapping wrong: %v", c)
	}
	if c["actor.id"] != "subX" {
		t.Errorf("actorId should map to actor.id, got %v", c["actor.id"])
	}
	// unset optional fields absent
	if _, ok := c["outcome"]; ok {
		t.Errorf("unset outcome should be absent")
	}
}

func TestParseLimit(t *testing.T) {
	cases := map[string]int{"": 50, "0": 50, "-3": 50, "abc": 50, "10": 10, "200": 200, "999": 200}
	for in, want := range cases {
		if got := ParseLimit(in); got != want {
			t.Errorf("ParseLimit(%q)=%d want %d", in, got, want)
		}
	}
}

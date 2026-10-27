package project

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
)

// TestCloudResourceName checks the resource-name derivation the audit event's display column
// depends on (bucketName → name → displayName → externalId → cache id).
func TestCloudResourceName(t *testing.T) {
	cases := []struct {
		name string
		cr   cloud.CloudResource
		want string
	}{
		{"bucket", cloud.CloudResource{Type: "BUCKET", Data: map[string]any{"bucketName": "platform-data"}, ExternalID: "ext"}, "platform-data"},
		{"server-name", cloud.CloudResource{Type: "SERVER", Data: map[string]any{"name": "vm-1"}}, "vm-1"},
		{"displayName", cloud.CloudResource{Data: map[string]any{"displayName": "disp"}}, "disp"},
		{"externalId-fallback", cloud.CloudResource{ExternalID: "uuid-x"}, "uuid-x"},
		{"cacheId-fallback", cloud.CloudResource{ID: "cache-1"}, "cache-1"},
		{"bucket-wins-over-name", cloud.CloudResource{Data: map[string]any{"bucketName": "b", "name": "n"}}, "b"},
	}
	for _, c := range cases {
		if got := cloudResourceName(&c.cr); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// TestNewCloudResourceEvent asserts the audit event carries the RESOURCE's own identity (the
// reported bug: bucket-create events were project-anonymous and unfindable by bucket name).
func TestNewCloudResourceEvent(t *testing.T) {
	u := &user.User{Sub: "sub-1", FirstName: "Hien", LastName: "To"}
	p := &Project{ID: "proj-1", Name: "example.com", OrganizationID: "org-1"}
	cr := &cloud.CloudResource{
		ID: "cache-9", Type: "BUCKET", ExternalID: "platform-data",
		Data: map[string]any{"bucketName": "platform-data"},
	}
	ev := newCloudResourceEvent(u, p, "CLOUD_RESOURCE_CREATE", "", cr)

	if ev.ResourceType != "BUCKET" {
		t.Errorf("resourceType = %q, want BUCKET (must be the resource kind, not PROJECT)", ev.ResourceType)
	}
	if ev.ResourceDisplayName != "platform-data" {
		t.Errorf("resourceDisplayName = %q, want the bucket name (searchable)", ev.ResourceDisplayName)
	}
	if ev.ResourceID != "platform-data" {
		t.Errorf("resourceID = %q, want the external id", ev.ResourceID)
	}
	// scoping preserved for the org-audit reader
	if ev.ProjectID != "proj-1" || ev.OrganizationID != "org-1" {
		t.Errorf("scoping lost: projectId=%q orgId=%q", ev.ProjectID, ev.OrganizationID)
	}
	if ev.ResourceMetadata["cacheId"] != "cache-9" || ev.ResourceMetadata["projectName"] != "example.com" {
		t.Errorf("metadata missing cacheId/projectName: %#v", ev.ResourceMetadata)
	}

	// A specific verb distinct from the coarse action lands in metadata.
	ev2 := newCloudResourceEvent(u, p, "CLOUD_RESOURCE_ACTION", "MAKE_BUCKET_PUBLIC", cr)
	if ev2.ResourceMetadata["verb"] != "MAKE_BUCKET_PUBLIC" {
		t.Errorf("verb not recorded: %#v", ev2.ResourceMetadata)
	}
	// A verb equal to the action is not duplicated into metadata.
	ev3 := newCloudResourceEvent(u, p, "CLOUD_RESOURCE_CREATE", "CLOUD_RESOURCE_CREATE", cr)
	if _, ok := ev3.ResourceMetadata["verb"]; ok {
		t.Errorf("verb should be omitted when equal to action: %#v", ev3.ResourceMetadata)
	}
}

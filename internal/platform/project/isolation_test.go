package project

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// §5/§7: a cloud read/mutate/delete may only target a cache row owned by the acting project. A row
// owned by another project, or a nil row (a raw/uncached id), must be rejected.
func TestResourceOwnedBy_crossProjectDenied(t *testing.T) {
	owned := &cloud.CloudResource{ID: "r1", ExternalID: "ext-1", ProjectID: "proj-A"}
	if !resourceOwnedBy(owned, "proj-A") {
		t.Fatal("an owned resource must pass")
	}
	if resourceOwnedBy(owned, "proj-B") {
		t.Fatal("§5/§7: another project must not act on this resource")
	}
	if resourceOwnedBy(nil, "proj-A") {
		t.Fatal("§5/§7: a raw/uncached id (nil cache row) must be rejected, not acted on")
	}
}

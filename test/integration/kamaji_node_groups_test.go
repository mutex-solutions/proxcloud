//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// A node group edit on the real flavor catalog, whose Get returns a retired row and nil for a deleted one, and whose
// pveFlavorOf answers a 400 for both. A fake that returned nil for "retired" is how this broke while tested.
func TestNodeGroupEditWithARetiredFlavor(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	flavors := pveflavor.New(db)
	for _, f := range []*pveflavor.Flavor{
		{Slug: "old", VCPUs: 2, RAMMb: 2048, DiskGb: 20, Active: false},
		{Slug: "new", VCPUs: 2, RAMMb: 2048, DiskGb: 20, Active: true},
	} {
		if err := flavors.Save(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	h := project.NewHandler(project.NewService(project.NewRepo(db), nil, nil, nil, nil), nil, nil, nil, nil, nil, cloud.NewRepo(db), externalservice.NewService(externalservice.NewRepo(db), nil), nil, nil, "")
	h.SetPVEFlavors(flavors)
	compute := &externalservice.ExternalService{ID: "svc", Config: map[string]any{"provider": "proxmox"}}
	d := kamaji.ClusterDefaults{RootVolumeGiB: 30}

	row := func(groups ...map[string]any) *cloud.CloudResource {
		raw := make([]any, len(groups))
		for i, g := range groups {
			raw[i] = g
		}
		return &cloud.CloudResource{ExternalID: "pc-c-1", Data: map[string]any{"cluster": map[string]any{"node_groups": raw}}}
	}
	running := func(name, flavor string, count float64) map[string]any {
		return map[string]any{"name": name, "flavor_id": flavor, "count": count, "root_volume_gib": float64(30)}
	}
	edit := func(quota map[string]any, cr *cloud.CloudResource, groups ...kamaji.NodeGroup) error {
		return h.KamajiCheckNodeGroups(ctx, &project.Project{ID: "p1", Quota: quota}, compute, d, cr, groups)
	}
	want4xx := func(what string, err error, status int) {
		t.Helper()
		var he *httpx.HTTPError
		if !errors.As(err, &he) || he.Status != status {
			t.Errorf("%s: %v, want %d", what, err, status)
		}
	}
	noRoom := map[string]any{"servers": 0}

	if err := edit(nil, row(running("w", "old", 2)), kamaji.NodeGroup{Name: "w", FlavorID: "new", Count: 2}); err != nil {
		t.Errorf("move off a retired flavor: %v", err)
	}
	if err := edit(noRoom, row(running("w", "old", 2)), kamaji.NodeGroup{Name: "w", FlavorID: "new", Count: 2}); err != nil {
		t.Errorf("a move at the limit asks for nothing, the retired flavor still has its size: %v", err)
	}
	if err := edit(nil, row(running("a", "old", 1), running("b", "old", 1)),
		kamaji.NodeGroup{Name: "a", FlavorID: "new", Count: 1}, kamaji.NodeGroup{Name: "b", FlavorID: "old", Count: 3}); err != nil {
		t.Errorf("one group moves, another stays on the retired flavor and scales: %v", err)
	}
	want4xx("a new group on a retired flavor", edit(nil, row(running("w", "old", 1)),
		kamaji.NodeGroup{Name: "w", FlavorID: "old", Count: 1}, kamaji.NodeGroup{Name: "other", FlavorID: "old", Count: 1}), http.StatusBadRequest)
	want4xx("a group moving onto a retired flavor", edit(nil, row(running("w", "new", 1)), kamaji.NodeGroup{Name: "w", FlavorID: "old", Count: 1}), http.StatusBadRequest)
	want4xx("over the quota", edit(noRoom, row(running("w", "new", 1)), kamaji.NodeGroup{Name: "w", FlavorID: "new", Count: 2}), http.StatusConflict)

	// Deleted, not retired: the move still works, and counts the old workers as nothing.
	if err := flavors.Delete(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	if err := edit(nil, row(running("w", "old", 2)), kamaji.NodeGroup{Name: "w", FlavorID: "new", Count: 2}); err != nil {
		t.Errorf("move off a deleted flavor: %v", err)
	}
	want4xx("a move off a deleted flavor needs room", edit(noRoom, row(running("w", "old", 2)), kamaji.NodeGroup{Name: "w", FlavorID: "new", Count: 2}), http.StatusConflict)
	err := edit(nil, row(running("w", "old", 2)), kamaji.NodeGroup{Name: "w", FlavorID: "old", Count: 3})
	want4xx("staying on a deleted flavor", err, http.StatusBadRequest)
	if err != nil && !strings.Contains(err.Error(), "flavor") {
		t.Errorf("the refusal should name the flavor: %v", err)
	}

	// The reported 409: 3 x 100 GiB down to 2 x 100, with no disk left.
	big := map[string]any{"name": "w", "flavor_id": "new", "count": float64(3), "root_volume_gib": float64(100)}
	if err := edit(map[string]any{"diskGb": 0}, row(big), kamaji.NodeGroup{Name: "w", FlavorID: "new", Count: 2, RootVolumeGiB: 100}); err != nil {
		t.Errorf("scale down a 100 GiB group: %v", err)
	}
}

package templateupdates

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// Placement: per node only the ACTIVE storages with import / images content, the provider's configured
// import storage first, nodes sorted; a placement not in that list is refused (fail closed).
func TestPlacementOptionsAndAllowed(t *testing.T) {
	up := pve.StorageNodeState{Active: true}
	caps := map[string]pve.StorageCapability{
		"a-import":   {Content: []string{"import"}, Nodes: map[string]pve.StorageNodeState{"pve1": up}},
		"nas-import": {Content: []string{"import", "iso"}, Nodes: map[string]pve.StorageNodeState{"pve1": up, "pve2": up}},
		"local-zfs":  {Content: []string{"images", "rootdir"}, Nodes: map[string]pve.StorageNodeState{"pve2": up, "pve1": up, "pve3": {}}},
		"ceph":       {Content: []string{"images"}, Nodes: map[string]pve.StorageNodeState{"pve1": up}},
		"iso-only":   {Content: []string{"iso"}, Nodes: map[string]pve.StorageNodeState{"pve4": up}},
	}
	got := PlacementOptions(caps, "nas-import")
	want := []PlacementOption{
		{Node: "pve1", ImportStorages: []string{"nas-import", "a-import"}, DiskStorages: []string{"ceph", "local-zfs"}},
		{Node: "pve2", ImportStorages: []string{"nas-import"}, DiskStorages: []string{"local-zfs"}},
		{Node: "pve4", ImportStorages: []string{}, DiskStorages: []string{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("options:\n got %+v\nwant %+v", got, want)
	}
	if out := PlacementOptions(nil, ""); out == nil || len(out) != 0 {
		t.Fatalf("no storage: %#v (must be an empty list, not null)", out)
	}
	for _, c := range []struct {
		node, imp, disk string
		ok              bool
	}{
		{"pve1", "nas-import", "local-zfs", true},
		{"pve1", "a-import", "ceph", true},
		{"pve2", "a-import", "local-zfs", false},   // import storage not on that node
		{"pve2", "nas-import", "ceph", false},      // disk storage not on that node
		{"pve3", "nas-import", "local-zfs", false}, // inactive there
		{"pve1", "local-zfs", "nas-import", false}, // contents swapped
		{"pve4", "", "", false},
		{"pve9", "nas-import", "local-zfs", false}, // unknown node
	} {
		if PlacementAllowed(got, c.node, c.imp, c.disk) != c.ok {
			t.Errorf("PlacementAllowed(%s, %s, %s) != %v", c.node, c.imp, c.disk, c.ok)
		}
	}
}

// What EnqueueVM refuses for the request itself is ErrRefused, so a caller that retries knows not to.
func TestEnqueueVMRefusalIsErrRefused(t *testing.T) {
	s := &Service{VMEnabled: true}
	if _, err := s.EnqueueVM(context.Background(), VMRequest{Digest: "pending", ConfirmDigest: "pending"}, "a"); !errors.Is(err, ErrRefused) ||
		err.Error() != "confirm the exact digest to import" {
		t.Fatalf("err %v", err)
	}
}

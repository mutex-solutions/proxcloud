package billingresource

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

func TestVolumeBackedServerDoesNotBillFlavorRootDisk(t *testing.T) {
	cr := &cloud.CloudResource{
		ID:        "server-1",
		ProjectID: "project-1",
		Data: map[string]any{
			"volumeBacked": true,
			"server": map[string]any{
				"flavor": map[string]any{"disk": 80, "ram": 4096, "vcpus": 2},
			},
		},
	}

	got := (&ServerProvider{}).instanceBR(cr, false)
	if got == nil || got.Values["root_disk_gb"] != 0 {
		t.Fatalf("root_disk_gb = %#v, want 0 for a Cinder-backed root", got)
	}
}

func TestNovaVolumeBackedShapeDoesNotBillFlavorRootDiskWithoutMarker(t *testing.T) {
	cr := &cloud.CloudResource{Data: map[string]any{
		"server": map[string]any{
			"image":  "",
			"flavor": map[string]any{"disk": 80},
		},
	}}
	got := (&ServerProvider{}).instanceBR(cr, false)
	if got == nil || got.Values["root_disk_gb"] != 0 {
		t.Fatalf("root_disk_gb = %#v, want 0 from Nova's empty image reference", got)
	}
}

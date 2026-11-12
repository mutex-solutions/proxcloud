package project

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// detachedVolumeName is the name the volume sync gives a disk on a holder (#218 fallback answer): its file name.
func TestDetachedVolumeName(t *testing.T) {
	for volID, want := range map[string]string{
		"local-zfs:vm-900-disk-1":     "vm-900-disk-1",
		"nas:900/vm-900-disk-1.raw":   "vm-900-disk-1",
		"nas:900/vm-900-disk-2.qcow2": "vm-900-disk-2",
	} {
		if got := detachedVolumeName(volID); got != want {
			t.Errorf("%s: %q, want %q", volID, got, want)
		}
	}
}

// A create whose row is not readable yet still answers a VOLUME in the row's shape: service, project, region and the
// data.volume the create knows.
func TestPVEVolumeFallback(t *testing.T) {
	h := &Handler{defaultRegion: "dflt"}
	es := &externalservice.ExternalService{ID: "pve", Config: map[string]any{"provider": "proxmox"}}
	p := &Project{ID: "p1", Services: []any{map[string]any{"serviceId": "pve", "region": "r1"}}}
	cr := h.pveVolumeFallback(es, p, "nas:900/vm-900-disk-1.raw", map[string]any{"name": "d", "size": 5, "status": "available"})
	vol, _ := cr.Data["volume"].(map[string]any)
	if cr.Type != cloud.TypeVolume || cr.ServiceID != "pve" || cr.ProjectID != "p1" || cr.Region != "r1" ||
		cr.ExternalID != "nas:900/vm-900-disk-1.raw" || vol["size"] != 5 || vol["status"] != "available" || cr.CreatedAt == nil {
		t.Fatalf("fallback = %+v", cr)
	}
}

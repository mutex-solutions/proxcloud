package pve

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// A tenant cluster's disk is in no VM of the pool, so the volume sync never lists it: its row must survive that sync,
// which the gateway's registry provider reconciles instead.
func TestVolumeSyncKeepsClusterDisks(t *testing.T) {
	p := &VolumeSyncProvider{unread: map[int]bool{}}
	disk := &cloud.CloudResource{Data: map[string]any{"volume": map[string]any{DataK8sCluster: "pc-c-1"}}}
	if keep, _ := p.KeepUnread(disk); !keep {
		t.Fatal("cluster disk row archived by the PVE volume sync")
	}
	vmDisk := &cloud.CloudResource{Data: map[string]any{"volume": map[string]any{"vmid": 101}}}
	if keep, _ := p.KeepUnread(vmDisk); keep {
		t.Fatal("a vanished server disk kept")
	}
}

package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

func TestFencedAttachCleanupDefersNeverDestroyUnknown(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprint(shared), func(t *testing.T) {
			m := newMemHolder()
			deletes := 0
			cfg := VMConfig{"smbios1": "uuid=carrier", "tags": "pc-holder;proxcloud", "unused0": "nas:900/vm-900-disk-0.raw"}
			node := "pve1"
			routes := fakeRoutes{
				"GET /api2/json/pools/tp-p1":                 say(`{"data":{"poolid":"tp-p1","members":[{"id":"qemu/900","type":"qemu","node":"pve1","vmid":900},{"id":"qemu/102","type":"qemu","node":"pve1","vmid":102},{"id":"qemu/103","type":"qemu","node":"pve2","vmid":103}]}}`),
				"GET " + carrierPath + "/config":             func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(map[string]any{"data": cfg}) },
				"GET " + carrierPath + "/pending":            say(`{"data":[]}`),
				"GET /api2/json/nodes/pve1/qemu/102/config":  say(`{"data":{"smbios1":"uuid=target","scsi0":"nas:vm-102-disk-0,size=8G"}}`),
				"GET /api2/json/nodes/pve1/qemu/102/pending": say(`{"data":[]}`),
				"POST " + carrierPath + "/move_disk":         refuse,
				"DELETE " + carrierPath:                      func(w http.ResponseWriter, r *http.Request) { deletes++ },
			}
			if shared { // an unknown CREATE is enough to require retaining the original holder
				node = "pve2"
				routes["GET /api2/json/nodes/pve2/qemu/103/config"] = say(`{"data":{"smbios1":"uuid=target","scsi0":"nas:vm-103-disk-0,size=8G"}}`)
				routes["GET /api2/json/nodes/pve2/qemu/103/pending"] = say(`{"data":[]}`)
				routes["POST /api2/json/nodes/pve1/qemu"] = refuse
			}
			s := volCluster(t, cfgSharedUnused, map[string][]url.Values{}, routes)
			s.HolderOperations = m
			s.HolderBudgets = smallHolderBudgets()
			target := "102"
			if node == "pve2" {
				target = "103"
			}
			if e := s.AttachVolume(context.Background(), "p1", 900, "nas:900/vm-900-disk-0.raw", target); e == nil {
				t.Fatal("unknown attach succeeded")
			}
			if deletes != 0 || m.pending == nil {
				t.Fatalf("cleanup deletes=%d pending=%v", deletes, m.pending)
			}
		})
	}
}
func TestActualPVEMoveTaskCompositeIdentity(t *testing.T) {
	up := UPID("UPID:pve2:001A2939:0120FB51:6AF17B54:qmmove:103-scsi0>102-scsi0:proxcloud@pve!platform:")
	if !validHolderUPID(up, holderVMIdentity{Node: "pve2", VMID: 103}) {
		t.Fatal("actual PVE9 qmmove reply rejected")
	}
	if validHolderUPID(up, holderVMIdentity{Node: "pve2", VMID: 102}) {
		t.Fatal("foreign source identity accepted")
	}
}

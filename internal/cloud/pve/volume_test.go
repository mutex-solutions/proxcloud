package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAddVolumeStorageRoom(t *testing.T) {
	const upid = "UPID:pve1:0:0:0:qmconfig:101:u:"
	tests := []struct {
		name     string
		storage  string // row for "local-zfs" on pve1; "" = no row
		headroom float64
		wantFull bool
	}{
		{"room for disk + headroom", `{"type":"storage","storage":"local-zfs","node":"pve1","status":"available","maxdisk":107374182400,"disk":53687091200}`, 10, false},
		{"disk eats into headroom", `{"type":"storage","storage":"local-zfs","node":"pve1","status":"available","maxdisk":107374182400,"disk":80530636800}`, 10, true},
		{"storage inactive on the VM's node", `{"type":"storage","storage":"local-zfs","node":"pve1","status":"unknown","maxdisk":107374182400,"disk":0}`, 0, true},
		{"storage not on the VM's node", `{"type":"storage","storage":"local-zfs","node":"pve2","status":"available","maxdisk":107374182400,"disk":0}`, 0, true},
		{"no storage row", "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var setCalls int
			_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
				"GET /api2/json/cluster/resources": func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Query().Get("type") == "storage" {
						fmt.Fprintf(w, `{"data":[%s]}`, tt.storage)
						return
					}
					fmt.Fprint(w, `{"data":[{"id":"qemu/101","type":"qemu","vmid":101,"node":"pve1","pool":"tp-p1"}]}`)
				},
				"GET /api2/json/nodes/pve1/qemu/101/config": func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, `{"data":{"scsi0":"local-zfs:vm-101-disk-0,size=32G"}}`)
				},
				"POST /api2/json/nodes/pve1/qemu/101/config": func(w http.ResponseWriter, r *http.Request) {
					setCalls++
					_ = r.ParseForm()
					if got := r.PostForm.Get("scsi1"); got != "local-zfs:20" {
						t.Errorf("scsi1 = %q", got)
					}
					fmt.Fprintf(w, `{"data":%q}`, upid)
				},
				"GET /api2/json/nodes/pve1/tasks/" + upid + "/status": func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, `{"data":{"status":"stopped","exitstatus":"OK"}}`)
				},
			})
			s := &Service{C: c, PoolPrefix: "tp-"}
			slot, err := s.AddVolume(context.Background(), "p1", "101", 20, "local-zfs", tt.headroom)
			if tt.wantFull {
				if !errors.Is(err, ErrStorageFull) || setCalls != 0 {
					t.Fatalf("err = %v, config writes = %d; want ErrStorageFull and no write", err, setCalls)
				}
				return
			}
			if err != nil || slot != "scsi1" || setCalls != 1 {
				t.Fatalf("slot %q err %v writes %d", slot, err, setCalls)
			}
		})
	}
}

// volCluster fakes a pool of VMs 101+102 on pve1 and 103 on pve2. VM 101's config is cfg101;
// 102/103 have a root disk only. Storages: local-zfs (local, both nodes), nas (shared NFS),
// isos (dir, iso only), tiny (local, 1 GiB free). Writes are recorded per path. VM 900 is the
// holder a cross-node attach creates; it only exists for those calls. nas1 is shared, but enabled on pve1 only.
// overrides replace or add routes.
func volCluster(t *testing.T, cfg101 string, writes map[string][]url.Values, overrides ...fakeRoutes) *Service {
	const upid = volUPID
	rec := func(key string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			writes[key] = append(writes[key], r.PostForm)
			fmt.Fprintf(w, `{"data":%q}`, upid)
		}
	}
	const g = 1 << 30
	routes := fakeRoutes{
		"GET /api2/json/cluster/resources": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("type") == "storage" {
				fmt.Fprintf(w, `{"data":[
					{"type":"storage","storage":"local-zfs","node":"pve1","status":"available","maxdisk":%d,"disk":0},
					{"type":"storage","storage":"local-zfs","node":"pve2","status":"available","maxdisk":%d,"disk":0},
					{"type":"storage","storage":"nas","node":"pve1","status":"available","maxdisk":%d,"disk":0},
					{"type":"storage","storage":"nas","node":"pve2","status":"available","maxdisk":%d,"disk":0},
					{"type":"storage","storage":"nas1","node":"pve1","status":"available","maxdisk":%d,"disk":0},
					{"type":"storage","storage":"isos","node":"pve1","status":"available","maxdisk":%d,"disk":0},
					{"type":"storage","storage":"tiny","node":"pve1","status":"available","maxdisk":%d,"disk":%d}]}`,
					100*g, 100*g, 100*g, 100*g, 100*g, 100*g, 10*g, 9*g)
				return
			}
			fmt.Fprint(w, `{"data":[
				{"id":"qemu/101","type":"qemu","vmid":101,"name":"a","node":"pve1","pool":"tp-p1"},
				{"id":"qemu/102","type":"qemu","vmid":102,"name":"b","node":"pve1","pool":"tp-p1"},
				{"id":"qemu/103","type":"qemu","vmid":103,"name":"c","node":"pve2","pool":"tp-p1"},
				{"id":"qemu/104","type":"qemu","vmid":104,"name":"other","node":"pve1","pool":"tp-p2"},
				{"id":"qemu/900","type":"qemu","vmid":900,"name":"pc-holder-pve1","node":"pve1","pool":"tp-p1","tags":"pc-holder;proxcloud"}]}`)
		},
		"GET /api2/json/storage": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[
				{"storage":"local-zfs","type":"zfspool","content":"images,rootdir"},
				{"storage":"nas","type":"nfs","content":"images"},
				{"storage":"nas1","type":"nfs","content":"images","nodes":"pve1"},
				{"storage":"isos","type":"dir","content":"iso"},
				{"storage":"tiny","type":"lvmthin","content":"images"}]}`)
		},
		"GET /api2/json/nodes/pve1/qemu/101/config": func(w http.ResponseWriter, r *http.Request) {
			// A config write that deletes keys takes effect, unless the test pins the volume
			// (PVE keeps unusedN while a snapshot references it).
			var cfg map[string]any
			_ = json.Unmarshal([]byte(cfg101), &cfg)
			if _, pinned := writes["pinned"]; !pinned {
				for _, w := range writes["config"] {
					delete(cfg, w.Get("delete"))
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": cfg})
		},
		"GET /api2/json/nodes/pve1/qemu/102/config": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":{"scsi0":"local-zfs:vm-102-disk-0,size=10G"}}`)
		},
		"GET /api2/json/nodes/pve2/qemu/103/config": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":{"scsi0":"local-zfs:vm-103-disk-0,size=10G"}}`)
		},
		"GET /api2/json/cluster/nextid": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":"900"}`)
		},
		"GET /api2/json/nodes/pve2/qemu/900/config": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":{"tags":"pc-holder;proxcloud"}}`)
		},
		"POST /api2/json/nodes/pve1/qemu":               rec("create"),
		"POST /api2/json/nodes/pve1/qemu/900/config":    rec("holderconfig"),
		"POST /api2/json/nodes/pve1/qemu/900/migrate":   rec("migrate"),
		"POST /api2/json/nodes/pve2/qemu/900/move_disk": rec("carriermove"),
		"DELETE /api2/json/nodes/pve2/qemu/900":         rec("carrierdelete"),
		"POST /api2/json/nodes/pve1/qemu/101/move_disk": rec("move"),
		"POST /api2/json/nodes/pve1/qemu/101/config":    rec("config"),
		"GET /api2/json/nodes/pve1/tasks/" + upid + "/status": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":{"status":"stopped","exitstatus":"OK"}}`)
		},
	}
	for _, o := range overrides {
		maps.Copy(routes, o)
	}
	_, c := fakePVE(t, routes)
	return &Service{C: c, PoolPrefix: "tp-"}
}

const (
	volUPID         = "UPID:pve1:0:0:0:qmmove:101:u:"
	cfgNas1Unused   = `{"scsi0":"local-zfs:vm-101-disk-0,size=10G","unused0":"nas1:101/vm-101-disk-1.raw"}`
	cfgLocalUnused  = `{"scsi0":"local-zfs:vm-101-disk-0,size=10G","unused0":"local-zfs:vm-101-disk-1"}`
	cfgSharedUnused = `{"scsi0":"local-zfs:vm-101-disk-0,size=10G","unused0":"nas:101/vm-101-disk-1.raw"}`
	cfgAttached     = `{"scsi0":"local-zfs:vm-101-disk-0,size=10G","scsi1":"local-zfs:vm-101-disk-1,size=20G"}`
)

func TestAttachVolumeCrossNode(t *testing.T) {
	tests := []struct {
		name, cfg, vol, target string
		wantConflict           bool
	}{
		{"local disk to a VM on another node", cfgLocalUnused, "local-zfs:vm-101-disk-1", "103", true},
		{"local disk to a VM on the same node", cfgLocalUnused, "local-zfs:vm-101-disk-1", "102", false},
		{"shared disk to a VM on the same node", cfgSharedUnused, "nas:101/vm-101-disk-1.raw", "102", false},
		{"shared disk to a VM on a node without its storage", cfgNas1Unused, "nas1:101/vm-101-disk-1.raw", "103", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writes := map[string][]url.Values{}
			s := volCluster(t, tt.cfg, writes)
			err := s.AttachVolume(context.Background(), "p1", 101, tt.vol, tt.target)
			var ce *ConflictError
			if tt.wantConflict {
				if !errors.As(err, &ce) || len(writes["move"]) != 0 {
					t.Fatalf("err = %v, moves = %d; want ConflictError and no move", err, len(writes["move"]))
				}
				return
			}
			if err != nil || len(writes["move"]) != 1 {
				t.Fatalf("err = %v, moves = %d", err, len(writes["move"]))
			}
			if f := writes["move"][0]; f.Get("disk") != "unused0" || f.Get("target-vmid") != tt.target || f.Get("target-disk") != "scsi1" {
				t.Fatalf("move_disk form = %v", f)
			}
		})
	}
}

// PVE's reassign refuses VMs on different nodes even on shared storage, so the disk rides a holder across.
func TestAttachVolumeSharedAcrossNodes(t *testing.T) {
	writes := map[string][]url.Values{}
	s := volCluster(t, cfgSharedUnused, writes)
	if err := s.AttachVolume(context.Background(), "p1", 101, "nas:101/vm-101-disk-1.raw", "103"); err != nil {
		t.Fatal(err)
	}
	if f := writes["move"]; len(f) != 1 || f[0].Get("target-vmid") != "900" || f[0].Get("target-disk") != "unused0" {
		t.Fatalf("move onto the holder = %v", f)
	}
	if f := writes["migrate"]; len(f) != 1 || f[0].Get("target") != "pve2" || f[0].Get("online") == "1" {
		t.Fatalf("holder migrate = %v", f)
	}
	if f := writes["carriermove"]; len(f) != 1 || f[0].Get("disk") != "unused0" || f[0].Get("target-vmid") != "103" || f[0].Get("target-disk") != "scsi1" {
		t.Fatalf("move onto the target = %v", f)
	}
	if len(writes["carrierdelete"]) != 1 {
		t.Fatalf("the empty holder must go: %v", writes)
	}
	if tags := writes["holderconfig"][0].Get("tags"); !hasTag(tags, tagHolder) || !hasTag(tags, tagCarrier) {
		t.Fatalf("carrier tags = %q", tags)
	}
}

func TestAttachTargets(t *testing.T) {
	tests := []struct {
		name, cfg, vol string
		want           []int
	}{
		{"local detached disk: VMs on its node only", cfgLocalUnused, "local-zfs:vm-101-disk-1", []int{101, 102}},
		{"shared detached disk: every VM of the pool", cfgSharedUnused, "nas:101/vm-101-disk-1.raw", []int{101, 102, 103}},
		{"shared detached disk: not a node without its storage", cfgNas1Unused, "nas1:101/vm-101-disk-1.raw", []int{101, 102}},
		{"attached disk: none until detached", cfgAttached, "local-zfs:vm-101-disk-1", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := volCluster(t, tt.cfg, map[string][]url.Values{})
			got, err := s.AttachTargets(context.Background(), "p1", 101, tt.vol)
			if err != nil {
				t.Fatal(err)
			}
			var ids []int
			for _, vm := range got {
				ids = append(ids, vm.VMID)
			}
			if fmt.Sprint(ids) != fmt.Sprint(tt.want) {
				t.Fatalf("targets = %v, want %v", ids, tt.want)
			}
		})
	}
}

func TestDeleteVolume(t *testing.T) {
	tests := []struct {
		name, cfg    string
		wantConflict bool
	}{
		{"detached disk is destroyed", cfgLocalUnused, false},
		{"attached disk must be detached first", cfgAttached, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writes := map[string][]url.Values{}
			s := volCluster(t, tt.cfg, writes)
			err := s.DeleteVolume(context.Background(), "p1", 101, "local-zfs:vm-101-disk-1")
			var ce *ConflictError
			if tt.wantConflict {
				if !errors.As(err, &ce) || len(writes["config"]) != 0 {
					t.Fatalf("err = %v, writes = %d; want ConflictError and no write", err, len(writes["config"]))
				}
				return
			}
			if err != nil || len(writes["config"]) != 1 || writes["config"][0].Get("delete") != "unused0" {
				t.Fatalf("err = %v, writes = %v", err, writes["config"])
			}
		})
	}
}

func TestWithUnusedSizes(t *testing.T) {
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/nodes/pve1/storage/nas/content": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"data":[{"volid":"nas:101/d.raw","size":%d}]}`, int64(3)<<30)
		},
	})
	s := &Service{C: c}
	disks := s.withUnusedSizes(context.Background(), "pve1", 101, []VMDisk{
		{Slot: "scsi0", VolID: "local-zfs:d0", Storage: "local-zfs", SizeGiB: 20},
		{Slot: "unused0", VolID: "nas:101/d.raw", Storage: "nas"},
	})
	if disks[0].SizeGiB != 20 || disks[1].SizeGiB != 3 {
		t.Fatalf("sizes = %d, %d; want 20, 3", disks[0].SizeGiB, disks[1].SizeGiB)
	}
}

func TestDeleteVolumeStillInSnapshot(t *testing.T) {
	writes := map[string][]url.Values{"pinned": nil}
	s := volCluster(t, cfgLocalUnused, writes)
	var ce *ConflictError
	if err := s.DeleteVolume(context.Background(), "p1", 101, "local-zfs:vm-101-disk-1"); !errors.As(err, &ce) {
		t.Fatalf("err = %v, want ConflictError", err)
	}
}

// A status read that fails mid-move (seen on the lab) still returns the error, but only once the move has ended: the
// caller then finds the disk where it is.
func TestRetypeVolumeWaitsOutALostStatus(t *testing.T) {
	polls := 0
	s := volCluster(t, cfgAttached, map[string][]url.Values{}, fakeRoutes{
		"GET /api2/json/nodes/pve1/tasks/" + volUPID + "/status": func(w http.ResponseWriter, r *http.Request) {
			polls++
			switch polls {
			case 1:
				http.Error(w, "refused", http.StatusBadGateway)
			case 2:
				fmt.Fprint(w, `{"data":{"status":"running"}}`)
			default:
				fmt.Fprint(w, `{"data":{"status":"stopped","exitstatus":"OK"}}`)
			}
		},
	})
	if err := s.RetypeVolume(context.Background(), "p1", 101, "local-zfs:vm-101-disk-1", "nas", 10); err == nil {
		t.Fatal("the lost status must reach the caller")
	}
	if polls < 3 {
		t.Fatalf("returned after %d status reads, before the move ended", polls)
	}
}

func TestRetypeVolume(t *testing.T) {
	tests := []struct {
		name, storage string
		wantConflict  string // "" = moved
	}{
		{"to shared storage on the node", "nas", ""},
		{"same storage", "local-zfs", "already on storage"},
		{"storage that holds no disks", "isos", "cannot hold disks"},
		{"storage unknown on the node", "gone", "cannot hold disks"},
		{"storage without room for the disk plus headroom", "tiny", "Not enough free space"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writes := map[string][]url.Values{}
			s := volCluster(t, cfgAttached, writes)
			err := s.RetypeVolume(context.Background(), "p1", 101, "local-zfs:vm-101-disk-1", tt.storage, 10)
			if tt.wantConflict != "" {
				var ce *ConflictError
				if !errors.As(err, &ce) || !strings.Contains(ce.Msg, tt.wantConflict) || len(writes["move"]) != 0 {
					t.Fatalf("err = %v, moves = %d; want ConflictError %q and no move", err, len(writes["move"]), tt.wantConflict)
				}
				return
			}
			if err != nil || len(writes["move"]) != 1 {
				t.Fatalf("err = %v, moves = %d", err, len(writes["move"]))
			}
			if f := writes["move"][0]; f.Get("disk") != "scsi1" || f.Get("storage") != "nas" || f.Get("delete") != "1" || f.Get("target-vmid") != "" {
				t.Fatalf("move_disk form = %v", f)
			}
		})
	}
}

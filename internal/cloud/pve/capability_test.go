package pve

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
)

func TestClassify(t *testing.T) {
	type flags struct {
		shared, snap, thin, migrate, repl bool
		tier, class                       string
	}
	tests := []struct {
		name string
		def  StorageDef
		want flags
	}{
		{"local zfs sparse", StorageDef{Type: "zfspool", Sparse: 1}, flags{false, true, true, false, true, TierStandard, ClassPerformance}},
		{"local zfs thick", StorageDef{Type: "zfspool"}, flags{false, true, false, false, true, TierStandard, ClassPerformance}},
		{"lvmthin", StorageDef{Type: "lvmthin"}, flags{false, true, true, false, false, TierStandard, ClassPerformance}},
		{"local lvm", StorageDef{Type: "lvm"}, flags{false, false, false, false, false, TierStandard, ClassPerformance}},
		{"shared lvm (pve 9 on iscsi)", StorageDef{Type: "lvm", Shared: 1}, flags{true, false, false, true, false, TierStandard, ClassStandard}},
		{"rbd", StorageDef{Type: "rbd"}, flags{true, true, true, true, false, TierFull, ClassPerformance}},
		{"cephfs", StorageDef{Type: "cephfs"}, flags{true, true, false, true, false, TierFull, ClassStandard}},
		{"nfs", StorageDef{Type: "nfs"}, flags{true, false, true, true, false, TierStandard, ClassCapacity}},
		{"cifs", StorageDef{Type: "cifs"}, flags{true, false, true, true, false, TierStandard, ClassCapacity}},
		{"iscsi", StorageDef{Type: "iscsi"}, flags{true, false, false, true, false, TierStandard, ClassStandard}},
		{"zfs over iscsi", StorageDef{Type: "zfs", Sparse: 1}, flags{true, true, true, true, false, TierStandard, ClassStandard}},
		{"local dir", StorageDef{Type: "dir"}, flags{false, false, true, false, false, TierStandard, ClassCapacity}},
		{"shared dir", StorageDef{Type: "dir", Shared: 1}, flags{true, false, true, true, false, TierStandard, ClassCapacity}},
		{"unknown type claims nothing", StorageDef{Type: "btrfs"}, flags{false, false, false, false, false, TierStandard, ClassCapacity}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := classify(tt.def)
			got := flags{c.Shared, c.Snapshots, c.Thin, c.LiveMigrate, c.Replication, c.Tier, c.Class}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDetectStorage(t *testing.T) {
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/storage": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[
				{"storage":"local-zfs","type":"zfspool","content":"images,rootdir","sparse":1},
				{"storage":"nas","type":"nfs","content":"images, iso","nodes":"pve1"},
				{"storage":"old","type":"dir","content":"iso","disable":1}]}`)
		},
		"GET /api2/json/cluster/resources": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("type") != "storage" {
				t.Errorf("type = %q", r.URL.Query().Get("type"))
			}
			fmt.Fprint(w, `{"data":[
				{"type":"storage","storage":"local-zfs","node":"pve1","status":"available","maxdisk":100,"disk":40},
				{"type":"storage","storage":"local-zfs","node":"pve2","status":"unknown","maxdisk":0,"disk":0},
				{"type":"storage","storage":"nas","node":"pve1","status":"available","maxdisk":1000,"disk":10},
				{"type":"storage","storage":"nas","node":"pve2","status":"available","maxdisk":1000,"disk":10},
				{"type":"storage","storage":"old","node":"pve1","status":"available","maxdisk":10,"disk":20},
				{"type":"storage","storage":"ghost","node":"pve1","status":"available"}]}`)
		},
	})
	got, err := c.DetectStorage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d storages, want 3 (rows for undefined storages ignored)", len(got))
	}
	want := map[string]map[string]StorageNodeState{
		"local-zfs": {"pve1": {true, 100, 60}, "pve2": {false, 0, 0}},
		"nas":       {"pve1": {true, 1000, 990}}, // pve2 is outside the nodes restriction
		"old":       {"pve1": {false, 10, 0}},    // disabled cluster-wide; avail clamped at 0
	}
	for id, nodes := range want {
		if !reflect.DeepEqual(got[id].Nodes, nodes) {
			t.Errorf("%s nodes = %+v, want %+v", id, got[id].Nodes, nodes)
		}
	}
	if !reflect.DeepEqual(got["nas"].Content, []string{"images", "iso"}) || !got["nas"].HasContent("images") {
		t.Errorf("nas content = %v", got["nas"].Content)
	}
	if !got["local-zfs"].Thin || got["local-zfs"].Class != ClassPerformance {
		t.Errorf("local-zfs = %+v", got["local-zfs"])
	}
}

func TestStorageHasRoom(t *testing.T) {
	const g = int64(1) << 30
	on := StorageNodeState{Active: true, Total: 100 * g, Avail: 30 * g}
	tests := []struct {
		name     string
		st       StorageNodeState
		disk     int64
		headroom float64
		want     bool
	}{
		{"fits with headroom", on, 15 * g, 10, true},
		{"exactly at headroom", on, 20 * g, 10, true}, // 30-20 = 10 = 10% of 100
		{"eats into headroom", on, 21 * g, 10, false},
		{"zero headroom fills to the brim", on, 30 * g, 0, true},
		{"bigger than avail", on, 31 * g, 0, false},
		{"inactive storage", StorageNodeState{Total: 100 * g, Avail: 100 * g}, g, 0, false},
		{"unknown node (zero state)", StorageNodeState{}, 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StorageHasRoom(tt.st, tt.disk, tt.headroom); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTemplateShared(t *testing.T) {
	caps := map[string]StorageCapability{
		"nas-templates": {Shared: true},
		"rbd":           {Shared: true},
		"local-zfs":     {},
		"local":         {},
	}
	tests := []struct {
		name string
		cfg  VMConfig
		want bool
	}{
		{"all on nfs", VMConfig{"scsi0": "nas-templates:9000/base-9000-disk-0.raw,size=10G", "ide2": "nas-templates:9000/vm-9000-cloudinit.qcow2,media=cdrom", "cores": float64(2)}, true},
		{"rbd root + empty cdrom", VMConfig{"virtio0": "rbd:base-9000-disk-0,size=10G", "ide2": "none,media=cdrom", "ide0": "cdrom,media=cdrom"}, true},
		{"root on local zfs", VMConfig{"scsi0": "local-zfs:base-9000-disk-0,size=10G"}, false},
		{"cloud-init drive on local zfs", VMConfig{"scsi0": "nas-templates:9000/base-9000-disk-0.raw", "ide2": "local-zfs:vm-9000-cloudinit,media=cdrom"}, false},
		{"iso on local dir", VMConfig{"scsi0": "rbd:base-9000-disk-0", "ide2": "local:iso/u24.iso,media=cdrom"}, false},
		{"efidisk on local", VMConfig{"scsi0": "rbd:base-9000-disk-0", "efidisk0": "local-zfs:base-9000-disk-1,size=1M"}, false},
		{"unused local volume is not cloned", VMConfig{"scsi0": "rbd:base-9000-disk-0", "unused0": "local-zfs:vm-9000-disk-2"}, true},
		{"raw device path", VMConfig{"scsi0": "/dev/disk/by-id/ata-x,size=10G"}, false},
		{"unknown storage", VMConfig{"scsi0": "gone:base-9000-disk-0"}, false},
		{"net0 and tags are not volumes", VMConfig{"scsi0": "rbd:base-9000-disk-0", "net0": "virtio=AA:BB,bridge=vmbr0", "tags": "a;b"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TemplateShared(tt.cfg, caps); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

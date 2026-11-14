package externalservice

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

func TestPVENetworkConfigIsolation(t *testing.T) {
	tests := []struct {
		name     string
		network  map[string]any
		wantMode string
		wantCtl  string
	}{
		{"absent defaults to shared", map[string]any{}, pve.IsolationShared, ""},
		{"unknown falls back to shared", map[string]any{"isolation": "none"}, pve.IsolationShared, ""},
		{"evpn per project", map[string]any{"isolation": "evpn-per-project", "evpnController": "pcx"}, pve.IsolationEVPN, "pcx"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := (&ExternalService{Config: map[string]any{"network": tt.network}}).PVENetworkConfig()
			if n.Isolation != tt.wantMode || n.EVPNController != tt.wantCtl {
				t.Fatalf("isolation=%q controller=%q, want %q %q", n.Isolation, n.EVPNController, tt.wantMode, tt.wantCtl)
			}
		})
	}
}

func TestPVENetworkConfigPublicPool(t *testing.T) {
	n := (&ExternalService{Config: map[string]any{"network": map[string]any{
		"publicNetworks": []any{
			map[string]any{"id": "vmbr1", "name": "Internet", "cidr": "203.0.113.0/24", "gateway": "203.0.113.1",
				"ranges": []any{"203.0.113.10-203.0.113.20"}, "exclude": []any{"203.0.113.15"}},
			map[string]any{"id": "vmbr2"},
		},
	}}}).PVENetworkConfig()
	if len(n.PublicNetworks) != 2 {
		t.Fatalf("got %d public networks", len(n.PublicNetworks))
	}
	pn := n.PublicNetworks[0]
	if pn.CIDR != "203.0.113.0/24" || pn.Gateway != "203.0.113.1" ||
		!reflect.DeepEqual(pn.Ranges, []string{"203.0.113.10-203.0.113.20"}) || !reflect.DeepEqual(pn.Exclude, []string{"203.0.113.15"}) {
		t.Fatalf("pool not parsed: %+v", pn)
	}
	if bare := n.PublicNetworks[1]; bare.Name != "vmbr2" || bare.CIDR != "" || len(bare.Ranges) != 0 {
		t.Fatalf("bare network: %+v", bare)
	}
}

func TestPVEDefaultQuota(t *testing.T) {
	gpu := map[string]any{"*": float64(1)}
	tests := []struct {
		name  string
		quota any
		want  map[string]any
	}{
		{"absent", nil, nil},
		{"empty", map[string]any{}, nil},
		{"proxmox keys", map[string]any{"servers": float64(5), "vcpus": float64(10), "ramMb": float64(8192),
			"diskGb": float64(100), "networks": float64(2), "floatingIps": float64(1), "gpu": gpu},
			map[string]any{"servers": 5, "vcpus": 10, "ramMb": 8192, "diskGb": 100, "networks": 2, "floatingIps": 1, "gpu": gpu}},
		{"legacy aliases", map[string]any{"instances": float64(3), "cores": float64(6), "ram": float64(4096),
			"gigabytes": float64(50), "floatingips": float64(1), "volumes": float64(9), "ports": float64(20)},
			map[string]any{"servers": 3, "vcpus": 6, "ramMb": 4096, "diskGb": 50, "floatingIps": 1}},
		{"proxmox key wins over alias", map[string]any{"cores": float64(6), "vcpus": float64(12)}, map[string]any{"vcpus": 12}},
		{"unlimited and junk dropped", map[string]any{"cores": float64(-1), "ramMb": "lots", "networks": float64(0)}, map[string]any{"networks": 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := map[string]any{}
			if tt.quota != nil {
				cfg["provisioning"] = map[string]any{"quota": tt.quota}
			}
			got := (&ExternalService{Config: cfg}).PVEDefaultQuota()
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestPVEStorageProfilePlacement(t *testing.T) {
	zfs1 := pve.StorageNodeState{Active: true, Total: 100, Avail: 60}
	zfs2 := pve.StorageNodeState{Active: true, Total: 100, Avail: 5}
	tank3 := pve.StorageNodeState{Active: true, Total: 500, Avail: 400}
	caps := map[string]pve.StorageCapability{
		"local-zfs": {Nodes: map[string]pve.StorageNodeState{"pve1": zfs1, "pve2": zfs2, "pve3": {Total: 100}}},
		"tank":      {Nodes: map[string]pve.StorageNodeState{"pve3": tank3}},
	}
	tests := []struct {
		name     string
		storages map[string]string
		want     map[string]pve.StorageNodeState
	}{
		{"star = nodes where the storage is active", map[string]string{"*": "local-zfs"}, map[string]pve.StorageNodeState{"pve1": zfs1, "pve2": zfs2}},
		{"explicit node wins over star", map[string]string{"*": "local-zfs", "pve3": "tank"}, map[string]pve.StorageNodeState{"pve1": zfs1, "pve2": zfs2, "pve3": tank3}},
		{"explicit node with inactive storage excluded", map[string]string{"pve1": "local-zfs", "pve2": "tank"}, map[string]pve.StorageNodeState{"pve1": zfs1}},
		{"storage active nowhere", map[string]string{"*": "gone"}, map[string]pve.StorageNodeState{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PVEStorageProfile{Storages: tt.storages}.Placement(caps)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPVEStorageHeadroomPct(t *testing.T) {
	for _, tt := range []struct {
		raw  any
		want float64
	}{{nil, 10}, {float64(20), 20}, {float64(0), 0}, {float64(-1), 10}, {float64(100), 10}, {"15", 10}} {
		es := &ExternalService{Config: map[string]any{}}
		if tt.raw != nil {
			es.Config["storageHeadroomPct"] = tt.raw
		}
		if got := es.PVEStorageHeadroomPct(); got != tt.want {
			t.Errorf("storageHeadroomPct=%v: got %v, want %v", tt.raw, got, tt.want)
		}
	}
}

func TestValidatePVEStorageProfiles(t *testing.T) {
	on := map[string]pve.StorageNodeState{"pve1": {Active: true}, "pve2": {Active: false}}
	caps := map[string]pve.StorageCapability{
		"local-zfs": {Type: "zfspool", Content: []string{"images", "rootdir"}, Snapshots: true, Thin: true, Replication: true, Nodes: on},
		"nas":       {Type: "nfs", Content: []string{"images"}, Shared: true, LiveMigrate: true, Thin: true, Nodes: on},
		"isos":      {Type: "nfs", Content: []string{"iso"}, Shared: true, Nodes: on},
		"down":      {Type: "zfspool", Content: []string{"images"}, Nodes: map[string]pve.StorageNodeState{"pve1": {}}},
	}
	prof := func(storages map[string]any, flags map[string]any) map[string]any {
		p := map[string]any{"storages": storages}
		for k, v := range flags {
			p[k] = v
		}
		return map[string]any{"p": p}
	}
	tests := []struct {
		name    string
		raw     any
		wantErr string
	}{
		{"absent", nil, ""},
		{"valid std downgrade", prof(map[string]any{"*": "local-zfs"}, map[string]any{"snapshots": true, "replication": false}), ""},
		{"valid nfs shared", prof(map[string]any{"*": "nas"}, map[string]any{"shared": true, "liveMigrate": true}), ""},
		{"not an object", "x", "must be an object"},
		{"no storages", map[string]any{"p": map[string]any{"shared": false}}, `"p" maps no storage`},
		{"unknown storage", prof(map[string]any{"*": "nope"}, nil), `"nope" does not exist`},
		{"no images content", prof(map[string]any{"*": "isos"}, nil), `"isos" does not allow VM disks`},
		{"star inactive everywhere", prof(map[string]any{"*": "down"}, nil), "not active on any node"},
		{"explicit inactive node", prof(map[string]any{"pve2": "local-zfs"}, nil), "not active on node pve2"},
		{"explicit unknown node", prof(map[string]any{"pve9": "local-zfs"}, nil), "not active on node pve9"},
		{"claims shared on local zfs", prof(map[string]any{"*": "local-zfs"}, map[string]any{"shared": true}), "claims shared"},
		{"claims replication on nfs", prof(map[string]any{"*": "nas"}, map[string]any{"replication": true}), "claims replication"},
		{"claims snapshots on nfs", prof(map[string]any{"*": "nas"}, map[string]any{"snapshots": true}), "claims snapshots"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePVEStorageProfiles(tt.raw, caps, nil)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("got %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestPVEEffectiveStorageClass(t *testing.T) {
	es := &ExternalService{Config: map[string]any{
		"capabilities": map[string]any{"storages": map[string]any{
			"local-zfs": map[string]any{"class": "performance"},
			"nas":       map[string]any{"class": "capacity"},
			"lun":       map[string]any{"class": "standard"},
		}},
		"storageClasses": map[string]any{
			"nas": map[string]any{"class": "standard", "suggestedClass": "capacity"},
			"lun": map[string]any{"class": "bogus"},
			"new": map[string]any{"suggestedClass": "performance"},
		},
	}}
	for storage, want := range map[string]string{
		"local-zfs": "performance", // detected, no override
		"nas":       "standard",    // override wins
		"lun":       "standard",    // invalid override ignored
		"new":       "",            // suggestion alone is never applied; never detected
	} {
		if got := es.PVEEffectiveStorageClass(storage); got != want {
			t.Errorf("%s: got %q, want %q", storage, got, want)
		}
	}
	if got := es.PVEStorageClasses()["nas"].SuggestedClass; got != "capacity" {
		t.Errorf("suggestedClass = %q", got)
	}
}

func TestPVEStorageProfilesTierClassCaps(t *testing.T) {
	es := &ExternalService{Config: map[string]any{
		"capabilities": map[string]any{"storages": map[string]any{
			"local-zfs":    map[string]any{"tier": "standard", "class": "performance", "snapshots": true},
			"nas-capacity": map[string]any{"tier": "standard", "class": "capacity", "shared": true},
			"lun":          map[string]any{"tier": "standard", "class": "standard"},
			"ceph-vm":      map[string]any{"tier": "full", "class": "performance"},
			"ceph-fs":      map[string]any{"tier": "full", "class": "standard"},
			"nas-ssd":      map[string]any{"tier": "standard", "class": "performance"},
		}},
		"storageClasses": map[string]any{"nas-ssd": map[string]any{"class": "capacity"}},
		"storageProfiles": map[string]any{
			"std":      map[string]any{"storages": map[string]any{"*": "local-zfs"}, "snapshots": true},
			"capacity": map[string]any{"label": "Capacity HDD (NFS)", "storages": map[string]any{"*": "nas-capacity"}, "snapshots": true, "bootDisk": true, "dbaas": true},
			"mixed":    map[string]any{"storages": map[string]any{"pve1": "local-zfs", "pve2": "lun"}},
			"ceph":     map[string]any{"storages": map[string]any{"pve1": "ceph-vm", "pve2": "ceph-fs"}},
			"overrid":  map[string]any{"storages": map[string]any{"*": "nas-ssd"}},
			"nodb":     map[string]any{"storages": map[string]any{"*": "local-zfs"}, "dbaas": false, "bootDisk": false},
			"unknown":  map[string]any{"storages": map[string]any{"*": "never-detected"}},
			// Claims past detection (written on provider create, no save-time validation) are dropped.
			"upgraded": map[string]any{"storages": map[string]any{"*": "lun"}, "snapshots": true, "shared": true},
		},
	}}
	type want struct {
		tier, class, label         string
		snapshots, bootDisk, dbaas bool
	}
	tests := map[string]want{
		"std":      {"standard", "performance", "std", true, true, true},
		"capacity": {"standard", "capacity", "Capacity HDD (NFS)", false, false, false}, // claims ignored on capacity
		"mixed":    {"standard", "standard", "mixed", false, true, true},                // lowest class of its storages
		"ceph":     {"full", "standard", "ceph", false, true, true},
		"overrid":  {"standard", "capacity", "overrid", false, false, false}, // admin override to capacity
		"nodb":     {"standard", "performance", "nodb", false, false, false}, // explicit downgrades
		"unknown":  {"standard", "", "unknown", false, true, true},
		"upgraded": {"standard", "standard", "upgraded", false, true, true},
	}
	profiles := es.PVEStorageProfiles()
	for slug, w := range tests {
		p := profiles[slug]
		got := want{p.Tier, p.Class, p.DisplayName(), p.Snapshots, p.BootDisk(), p.DBaaS()}
		if got != w {
			t.Errorf("%s: got %+v, want %+v", slug, got, w)
		}
	}
	if profiles["upgraded"].Shared {
		t.Errorf("upgraded: shared claim survived on a non-shared storage")
	}
	// Never detected: no tier claimed.
	bare := (&ExternalService{Config: map[string]any{"storageProfiles": map[string]any{
		"std": map[string]any{"storages": map[string]any{"*": "local-zfs"}},
	}}}).PVEStorageProfiles()["std"]
	if bare.Tier != "" || bare.Class != "" || !bare.BootDisk() {
		t.Errorf("undetected profile = %+v", bare)
	}
}

func TestValidatePVEStorageProfilesCapacity(t *testing.T) {
	on := map[string]pve.StorageNodeState{"pve1": {Active: true}}
	caps := map[string]pve.StorageCapability{
		"nas-capacity": {Type: "nfs", Content: []string{"images"}, Shared: true, LiveMigrate: true, Class: pve.ClassCapacity, Nodes: on},
		"local-zfs":    {Type: "zfspool", Content: []string{"images"}, Snapshots: true, Class: pve.ClassPerformance, Nodes: on},
	}
	prof := func(storage string, flags map[string]any) map[string]any {
		p := map[string]any{"storages": map[string]any{"*": storage}}
		for k, v := range flags {
			p[k] = v
		}
		return map[string]any{"p": p}
	}
	overrideZFS := map[string]any{"local-zfs": map[string]any{"class": "capacity"}}
	tests := []struct {
		name    string
		raw     any
		classes any
		wantErr string
	}{
		{"capacity, honest flags", prof("nas-capacity", map[string]any{"shared": true, "liveMigrate": true, "bootDisk": false}), nil, ""},
		{"capacity claims bootDisk", prof("nas-capacity", map[string]any{"bootDisk": true}), nil, "cannot claim bootDisk"},
		{"capacity claims dbaas", prof("nas-capacity", map[string]any{"dbaas": true}), nil, "cannot claim dbaas"},
		{"capacity claims snapshots", prof("nas-capacity", map[string]any{"snapshots": true}), nil, "cannot claim snapshots"},
		{"zfs claims bootDisk", prof("local-zfs", map[string]any{"bootDisk": true, "snapshots": true}), nil, ""},
		{"zfs overridden to capacity claims snapshots", prof("local-zfs", map[string]any{"snapshots": true}), overrideZFS, "cannot claim snapshots"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePVEStorageProfiles(tt.raw, caps, tt.classes)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("got %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestPVEVolumeProfileFunc(t *testing.T) {
	es := &ExternalService{Config: map[string]any{
		"capabilities": map[string]any{"storages": map[string]any{
			"local-zfs":    map[string]any{"class": "performance"},
			"nas-capacity": map[string]any{"class": "capacity"},
		}},
		"storageProfiles": map[string]any{
			"std":       map[string]any{"storages": map[string]any{"*": "local-zfs"}},
			"protected": map[string]any{"storages": map[string]any{"*": "local-zfs"}},
			"capacity":  map[string]any{"storages": map[string]any{"pve1": "nas-capacity"}},
		},
	}}
	f := es.PVEVolumeProfileFunc()
	tests := []struct {
		node, storage, vmProfile string
		wantProfile, wantClass   string
	}{
		{"pve1", "nas-capacity", "std", "capacity", "capacity"},        // data disk retyped off the VM's profile
		{"pve1", "local-zfs", "std", "std", "performance"},             // VM profile maps it
		{"pve1", "local-zfs", "protected", "protected", "performance"}, // shared storage: VM's profile preferred
		{"pve1", "local-zfs", "", "protected", "performance"},          // first by slug
		{"pve2", "nas-capacity", "", "", "capacity"},                   // no profile maps it on pve2
		{"pve1", "other", "std", "", ""},
	}
	for _, tt := range tests {
		p, c := f(tt.node, tt.storage, tt.vmProfile)
		if p != tt.wantProfile || c != tt.wantClass {
			t.Errorf("%s/%s/%s: got %q %q, want %q %q", tt.node, tt.storage, tt.vmProfile, p, c, tt.wantProfile, tt.wantClass)
		}
	}
}

// Per-node copies of one template can differ in size (a 5 GiB rebuild next to a 20 GiB copy that
// --if-missing kept). A server placed on the node with the larger copy gets that size, so the catalog
// minimum is the largest copy, not the newest one.
func TestPVEImageCatalogMinDiskIsTheLargestCopy(t *testing.T) {
	found := []pve.DiscoveredTemplate{{Slug: "ubuntu-2404", Node: "pve1", VMID: 9300, Copies: map[string]pve.TemplateCopy{
		"pve1": {VMID: 9300, Version: "20261114", DiskGb: 5},
		"pve2": {VMID: 9200, Version: "20261001", DiskGb: 20},
	}}, {Slug: "debian-13", Node: "pve1", VMID: 9301, Copies: map[string]pve.TemplateCopy{
		"pve1": {VMID: 9301, Version: "20261114", DiskGb: 5},
		"pve2": {VMID: 9302, Version: "20261114", DiskGb: 5},
	}}}
	es := &ExternalService{Config: map[string]any{"templates": map[string]any{"debian-13": map[string]any{"minDiskGb": float64(8)}}}}
	got := map[string]int{}
	for _, tpl := range es.PVEImageCatalog(found) {
		got[tpl.Slug] = tpl.MinDiskGb
	}
	if want := map[string]int{"ubuntu-2404": 20, "debian-13": 8}; !reflect.DeepEqual(got, want) {
		t.Fatalf("minimum disk per slug = %v, want %v", got, want)
	}
	if got := (&ExternalService{}).PVEImageCatalog(found[1:])[0].MinDiskGb; got != 5 {
		t.Fatalf("equal 5 GiB copies: minimum %d, want 5", got)
	}
}

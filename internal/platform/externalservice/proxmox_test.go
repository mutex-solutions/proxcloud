package externalservice

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

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

// The setup defaults fill only what the operator has not set: a limit under either spelling, an explicit unlimited
// and the per-model GPU limits all stay.
func TestMergePVEDefaultQuota(t *testing.T) {
	defaults := map[string]int{"servers": 20, "vcpus": 80, "networks": 10}
	gpu := map[string]any{"nvidia-4090": float64(2), "*": float64(0)}
	tests := []struct {
		name   string
		stored map[string]any
		want   map[string]any
		added  []string
	}{
		{"nothing stored", nil, map[string]any{"servers": 20, "vcpus": 80, "networks": 10}, []string{"networks", "servers", "vcpus"}},
		{"gpu limits survive", map[string]any{"gpu": gpu}, map[string]any{"gpu": gpu, "servers": 20, "vcpus": 80, "networks": 10}, []string{"networks", "servers", "vcpus"}},
		{"stored value wins", map[string]any{"servers": float64(50)}, map[string]any{"servers": float64(50), "vcpus": 80, "networks": 10}, []string{"networks", "vcpus"}},
		{"legacy alias counts as set", map[string]any{"instances": float64(3), "cores": float64(6)}, map[string]any{"instances": float64(3), "cores": float64(6), "networks": 10}, []string{"networks"}},
		{"unlimited stays unlimited", map[string]any{"vcpus": float64(-1)}, map[string]any{"vcpus": float64(-1), "servers": 20, "networks": 10}, []string{"networks", "servers"}},
		{"complete", map[string]any{"servers": float64(1), "vcpus": float64(2), "networks": float64(3), "sandboxes": float64(4)},
			map[string]any{"servers": float64(1), "vcpus": float64(2), "networks": float64(3), "sandboxes": float64(4)}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, added := MergePVEDefaultQuota(tt.stored, defaults)
			if !reflect.DeepEqual(got, tt.want) || !reflect.DeepEqual(added, tt.added) {
				t.Fatalf("got %#v added %v, want %#v added %v", got, added, tt.want, tt.added)
			}
		})
	}
	stored := map[string]any{"gpu": gpu}
	if MergePVEDefaultQuota(stored, defaults); len(stored) != 1 {
		t.Fatalf("the stored map was changed: %v", stored)
	}
}

// What storage detection proposes can be saved as it is: a proposal never claims more than the storage has.
func TestProposedProfilesPassValidation(t *testing.T) {
	on := map[string]pve.StorageNodeState{"pve1": {Active: true}}
	caps := map[string]pve.StorageCapability{
		"local-zfs":    {Storage: "local-zfs", Type: "zfspool", Content: []string{"images"}, Snapshots: true, Replication: true, Class: pve.ClassPerformance, Nodes: on},
		"local-lvm":    {Storage: "local-lvm", Type: "lvmthin", Content: []string{"images"}, Snapshots: true, Thin: true, Class: pve.ClassPerformance, Nodes: on},
		"nas-capacity": {Storage: "nas-capacity", Type: "nfs", Content: []string{"images"}, Shared: true, Thin: true, LiveMigrate: true, Class: pve.ClassCapacity, Nodes: on},
		"ceph":         {Storage: "ceph", Type: "rbd", Content: []string{"images"}, Shared: true, Snapshots: true, Thin: true, LiveMigrate: true, Tier: pve.TierFull, Class: pve.ClassPerformance, Nodes: on},
	}
	tests := []struct {
		name    string
		classes map[string]any // config.storageClasses
	}{
		{"detected classes", nil},
		// An operator who marks a snapshot-capable store capacity class gets a proposal without snapshots.
		{"capacity overrides", map[string]any{"local-zfs": map[string]any{"class": "capacity"}, "ceph": map[string]any{"class": "capacity"}, "local-lvm": map[string]any{"class": "capacity"}}},
		{"an invalid override is ignored", map[string]any{"local-zfs": map[string]any{"class": "fast"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proposed := (&ExternalService{Config: map[string]any{"storageClasses": tt.classes}}).PVEProposedProfiles(caps)
			if len(proposed) != 4 {
				t.Fatalf("proposed: %v", proposed)
			}
			if err := ValidatePVEStorageProfiles(proposed, caps, tt.classes); err != nil {
				t.Fatal(err)
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

func TestParseCIDRPool(t *testing.T) {
	tests := []struct {
		pool, want string // want "" = refused
	}{
		{"10.100.0.0/16", "10.100.0.0/16"},
		{"10.100.0.0/24", "10.100.0.0/24"},
		{"10.100.3.7/16", "10.100.0.0/16"},
		{"10.100.0.0/25", ""},
		{"fd00::/48", ""},
		{"10.100.0.0", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got, err := ParseCIDRPool(tt.pool)
		if (tt.want == "") != (err != nil) || (err == nil && got.String() != tt.want) {
			t.Errorf("ParseCIDRPool(%q) = %v, %v; want %q", tt.pool, got, err, tt.want)
		}
	}
}

// A network block is refused when its tenant networks would fail, and kept when it is only not set up yet.
func TestValidatePVENetwork(t *testing.T) {
	tests := []struct {
		name, network string // JSON, as a save body carries it
		wantErr       string // "" = accepted
	}{
		{"no network", `null`, ""},
		{"empty", `{}`, ""},
		{"the console's create form", `{"isolation":"shared","blockedCidrs":["192.0.2.0/24"]}`, ""},
		{"a zone per project, no zone yet", `{"isolation":"evpn-per-project","evpnController":"pcx"}`, ""},
		{"the documented example", `{"defaultZone":"overlay","zones":{"overlay":{"kind":"evpn","controller":"proxcloud","asn":65000,"peers":["10.0.0.1"],"exitNodes":["pve1"],"mtu":1450}},
			"publicNetworks":[{"id":"vmbr1","name":"Internet","cidr":"203.0.113.0/24"}],"cidrPool":"10.100.0.0/16","isolation":"shared"}`, ""},
		{"one zone needs no defaultZone", `{"zones":{"simple":{"kind":"simple"}},"cidrPool":"10.100.0.0/24"}`, ""},
		{"vlan zone on a bridge", `{"defaultZone":"vlan","zones":{"vlan":{"kind":"vlan","bridge":"vmbr0","vlanMin":100,"vlanMax":199}}}`, ""},
		{"kind left out is evpn", `{"defaultZone":"overlay","zones":{"overlay":{}},"isolation":"evpn-per-project"}`, ""},
		{"overlay kind is evpn", `{"defaultZone":"o","zones":{"o":{"kind":"overlay"}},"isolation":"evpn-per-project"}`, ""},
		{"8-character zone id", `{"defaultZone":"zone1234","zones":{"zone1234":{"kind":"simple"},"vlan":{"kind":"vlan","bridge":"vmbr1","vlanMin":1,"vlanMax":4094}}}`, ""},
		{"vlan range of one tag", `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":100,"vlanMax":100}}}`, ""},
		{"vlan range as Ansible sends it", `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":"100","vlanMax":"199"}}}`, ""},
		{"vlan range as a whole float", `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":100.0,"vlanMax":1.99e2}}}`, ""},
		{"a range on another kind is not read", `{"zones":{"s":{"kind":"simple","vlanMin":0,"vlanMax":"x"}}}`, ""},
		{"blank cidrPool", `{"cidrPool":""}`, ""},

		{"not an object", `["overlay"]`, "config.network must be an object"},
		{"cidrPool /25", `{"cidrPool":"10.100.0.0/25"}`, "config.network.cidrPool must be an IPv4 range of /24 or larger"},
		{"cidrPool IPv6", `{"cidrPool":"fd00::/48"}`, "config.network.cidrPool must be an IPv4 range"},
		{"cidrPool without a prefix", `{"cidrPool":"10.100.0.0"}`, "config.network.cidrPool must be an IPv4 range"},
		{"cidrPool not a string", `{"cidrPool":16}`, "config.network.cidrPool must be a string"},
		{"isolation not a string", `{"isolation":true}`, "config.network.isolation must be a string"},
		{"zones not an object", `{"zones":["overlay"]}`, "config.network.zones must be an object"},
		{"zone not an object", `{"zones":{"overlay":"evpn"}}`, "config.network.zones.overlay must be an object"},
		{"9-character zone id", `{"zones":{"overlay12":{"kind":"evpn"}}}`, `the zone id "overlay12" is the Proxmox SDN zone id, of 1 to 8 characters`},
		{"empty zone id", `{"zones":{"":{"kind":"evpn"}}}`, `the zone id ""`},
		{"unknown kind", `{"zones":{"overlay":{"kind":"vxlan"}}}`, "config.network.zones.overlay.kind must be evpn, vlan or simple"},
		{"kind not a string", `{"zones":{"overlay":{"kind":1}}}`, "config.network.zones.overlay.kind must be"},
		{"vlan without a bridge", `{"zones":{"vlan":{"kind":"vlan","vlanMin":100}}}`, "config.network.zones.vlan is a vlan zone and needs a bridge"},
		{"vlan on a blank bridge", `{"zones":{"vlan":{"kind":"vlan","bridge":""}}}`, "needs a bridge"},
		{"vlan without a range", `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0"}}}`, "config.network.zones.v is a vlan zone and needs vlanMin"},
		{"vlan without vlanMax", `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":100}}}`, "config.network.zones.v is a vlan zone and needs vlanMax"},
		{"vlan with a null vlanMin", `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":null,"vlanMax":199}}}`, "needs vlanMin"},
		{"vlanMin 0", `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":0,"vlanMax":199}}}`,
			"config.network.zones.v.vlanMin must be a VLAN tag, a whole number from 1 to 4094, not 0"},
		{"vlanMax 4095", `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":100,"vlanMax":4095}}}`, "config.network.zones.v.vlanMax must be a VLAN tag, a whole number from 1 to 4094, not 4095"},
		{"negative vlanMin", `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":-1,"vlanMax":199}}}`, "vlanMin must be a VLAN tag, a whole number from 1 to 4094, not -1"},
		{"fractional vlanMin", `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":100.5,"vlanMax":199}}}`, "vlanMin must be a VLAN tag, a whole number from 1 to 4094, not 100.5"},
		{"vlanMax a word", `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":100,"vlanMax":"many"}}}`, "vlanMax must be a VLAN tag, a whole number from 1 to 4094, not many"},
		{"vlanMax a fractional string", `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":100,"vlanMax":"199.5"}}}`, "vlanMax must be a VLAN tag"},
		{"vlanMin a bool", `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":true,"vlanMax":199}}}`, "vlanMin must be a VLAN tag, a whole number from 1 to 4094, not true"},
		{"vlan range inverted", `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":200,"vlanMax":100}}}`,
			"config.network.zones.v.vlanMin must be at most vlanMax, not 200 with vlanMax 100"},
		{"defaultZone not a zone", `{"defaultZone":"overlay","zones":{"simple":{"kind":"simple"}}}`, "config.network.defaultZone must name one of config.network.zones"},
		{"defaultZone without zones", `{"defaultZone":"overlay"}`, "config.network.defaultZone must name one"},
		{"two zones, no defaultZone", `{"zones":{"a":{"kind":"simple"},"b":{"kind":"simple"}}}`, "config.network.defaultZone must name one"},
		{"a zone per project from a simple zone", `{"defaultZone":"s","zones":{"s":{"kind":"simple"}},"isolation":"evpn-per-project"}`,
			"evpn-per-project copies every project's zone from the default zone, which must then be an evpn zone; s is simple"},
		{"a zone per project from a vlan zone", `{"defaultZone":"v","zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":100,"vlanMax":199}},"isolation":"evpn-per-project"}`, "v is vlan"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var raw any
			if err := json.Unmarshal([]byte(tt.network), &raw); err != nil {
				t.Fatal(err)
			}
			err := ValidatePVENetwork(raw, nil)
			if (tt.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("ValidatePVENetwork(%s) = %v, want %q", tt.network, err, tt.wantErr)
			}
		})
	}
}

// A save is refused only for a problem the block it replaces does not have: a wrong value that changes is a new one.
func TestValidatePVENetworkStored(t *testing.T) {
	tests := []struct {
		name, network, stored string // JSON
		wantErr               string // "" = accepted
	}{
		{"the stored problem kept", `{"cidrPool":"10.100.0.0/26"}`, `{"cidrPool":"10.100.0.0/26"}`, ""},
		{"the stored problem fixed", `{"cidrPool":"10.100.0.0/16"}`, `{"cidrPool":"10.100.0.0/26"}`, ""},
		{"another wrong pool", `{"cidrPool":"10.100.0.0/25"}`, `{"cidrPool":"10.100.0.0/26"}`, "not 10.100.0.0/25"},
		{"another missing default zone", `{"defaultZone":"b","zones":{"s":{"kind":"simple"}}}`, `{"defaultZone":"a","zones":{"s":{"kind":"simple"}}}`, `not "b"`},
		{"another unknown kind", `{"zones":{"o":{"kind":"geneve"}}}`, `{"zones":{"o":{"kind":"vxlan"}}}`, "not geneve"},
		{"a stored vlan zone without a range kept", `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0"}}}`, `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0"}}}`, ""},
		{"a stored inverted range kept", `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":200,"vlanMax":100}}}`,
			`{"zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":200,"vlanMax":100}}}`, ""},
		{"another inverted range", `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":300,"vlanMax":100}}}`,
			`{"zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":200,"vlanMax":100}}}`, "not 300 with vlanMax 100"},
		{"a stored missing range filled in wrong", `{"zones":{"v":{"kind":"vlan","bridge":"vmbr0","vlanMin":0,"vlanMax":199}}}`,
			`{"zones":{"v":{"kind":"vlan","bridge":"vmbr0"}}}`, "vlanMin must be a VLAN tag"},
		{"stored is not an object", `{"zones":{"o":{"kind":"vxlan"}}}`, `"overlay"`, "config.network.zones.o.kind must be"},
		{"no stored block", `{"cidrPool":"10.100.0.0/26"}`, `null`, "config.network.cidrPool must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var raw, stored any
			if err := json.Unmarshal([]byte(tt.network), &raw); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tt.stored), &stored); err != nil {
				t.Fatal(err)
			}
			err := ValidatePVENetwork(raw, stored)
			if (tt.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("ValidatePVENetwork(%s, %s) = %v, want %q", tt.network, tt.stored, err, tt.wantErr)
			}
		})
	}
}

// The network blocks the repository registers pass the save check: the dev seed, and what installer stage 60
// (installer/playbooks/60-register.yml, provider_base.config.network) builds from the inventory defaults, for each
// zone kind and isolation the defaults offer.
func TestValidatePVENetworkAcceptsShippedConfigs(t *testing.T) {
	raw, err := os.ReadFile("../../../deploy/seed/external-service-dev.json")
	if err != nil {
		t.Fatal(err)
	}
	var seed []map[string]any
	if err := json.Unmarshal(raw, &seed); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, doc := range seed {
		if cfg, _ := doc["config"].(map[string]any); cfg["provider"] == "proxmox" {
			seen++
			if err := ValidatePVENetwork(cfg["network"], nil); err != nil {
				t.Errorf("seed %v: %v", doc["_id"], err)
			}
		}
	}
	if seen == 0 {
		t.Fatal("the seed has no proxmox provider")
	}

	raw, err = os.ReadFile("../../../installer/playbooks/group_vars/all/defaults.yml")
	if err != nil {
		t.Fatal(err)
	}
	var defaults struct {
		PC struct {
			Network struct {
				DefaultZone string   `yaml:"default_zone"`
				Kind        string   `yaml:"kind"`
				Bridge      string   `yaml:"bridge"`
				VlanMin     int      `yaml:"vlan_min"`
				VlanMax     int      `yaml:"vlan_max"`
				Controller  string   `yaml:"controller"`
				CIDRPool    string   `yaml:"cidr_pool"`
				Isolation   string   `yaml:"isolation"`
				Blocked     []string `yaml:"blocked_cidrs"`
			} `yaml:"network"`
		} `yaml:"pc_defaults"`
	}
	if err := yaml.Unmarshal(raw, &defaults); err != nil {
		t.Fatal(err)
	}
	n := defaults.PC.Network
	if n.DefaultZone == "" || n.Kind == "" || n.CIDRPool == "" {
		t.Fatalf("installer network defaults not found: %+v", n)
	}
	// The refused ones are what stage 00 refuses first (00-preflight.yml, "Check the tenant network settings"), so an
	// install does not get as far as stage 60 with them.
	for _, v := range []struct{ kind, bridge, isolation, pool, wantErr string }{
		{n.Kind, n.Bridge, n.Isolation, n.CIDRPool, ""}, // as shipped
		{"simple", n.Bridge, n.Isolation, n.CIDRPool, ""},
		{"vlan", "vmbr1", n.Isolation, n.CIDRPool, ""},
		{n.Kind, n.Bridge, "evpn", n.CIDRPool, ""},
		{"vlan", n.Bridge, n.Isolation, n.CIDRPool, "is a vlan zone and needs a bridge"},
		{"simple", n.Bridge, "evpn", n.CIDRPool, "must then be an evpn zone"},
		{n.Kind, n.Bridge, n.Isolation, "10.100.0.0/25", "cidrPool must be an IPv4 range of /24 or larger"},
	} {
		isolation := pve.IsolationShared // stage 60 registers evpn and evpn-per-project as evpn-per-project
		if v.isolation == "evpn" || v.isolation == pve.IsolationEVPN {
			isolation = pve.IsolationEVPN
		}
		network := map[string]any{
			"defaultZone": n.DefaultZone,
			"zones": map[string]any{n.DefaultZone: map[string]any{
				"kind": v.kind, "controller": n.Controller, "asn": 65000, "peers": []any{"192.0.2.11"}, "exitNodes": []any{"pve1"},
				"vrfVxlan": 10000, "mtu": 1450, "bridge": v.bridge, "vlanMin": n.VlanMin, "vlanMax": n.VlanMax,
			}},
			"publicNetworks": []any{},
			"cidrPool":       v.pool,
			"isolation":      isolation,
			"blockedCidrs":   n.Blocked,
			"sandboxCidrs":   []any{},
			"evpnController": n.Controller,
		}
		err := ValidatePVENetwork(network, nil)
		if (v.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), v.wantErr)) {
			t.Errorf("installer defaults with kind %s, bridge %q, isolation %s, cidr_pool %s: %v, want %q", v.kind, v.bridge, v.isolation, v.pool, err, v.wantErr)
		}
	}
}

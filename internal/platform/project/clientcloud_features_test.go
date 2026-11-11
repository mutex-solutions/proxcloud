package project

import (
	"reflect"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

func TestCloudFeatures(t *testing.T) {
	profile := func(storage string) map[string]any {
		return map[string]any{"storages": map[string]any{"*": storage}}
	}
	cases := []struct {
		name string
		cfg  map[string]any
		want []string
	}{
		{"ceph-s3 serves none", map[string]any{"provider": "ceph-s3"}, []string{}},
		{"kamaji serves none", map[string]any{"provider": "kamaji"}, []string{}},
		{"proxmox without profiles", map[string]any{"provider": "proxmox"}, []string{"serverSnapshots", "userData", "serverPassword", "serverRebuild", "serverDiskSnapshots", "subnets", "spreadGroups"}},
		{"proxmox with one profile: nothing to retype to", map[string]any{
			"provider": "proxmox", "storageProfiles": map[string]any{"std": profile("local-zfs")},
		}, []string{"serverSnapshots", "userData", "serverPassword", "serverRebuild", "serverDiskSnapshots", "subnets", "spreadGroups"}},
		{"proxmox with a storage-less profile does not count it", map[string]any{
			"provider": "proxmox", "storageProfiles": map[string]any{"std": profile("local-zfs"), "empty": map[string]any{}},
		}, []string{"serverSnapshots", "userData", "serverPassword", "serverRebuild", "serverDiskSnapshots", "subnets", "spreadGroups"}},
		{"proxmox with a backup store", map[string]any{"provider": "proxmox", "backupStorage": "nas-backup"},
			[]string{"serverSnapshots", "userData", "serverPassword", "serverRebuild", "serverDiskSnapshots", "subnets", "spreadGroups", "serverBackups"}},
		{"proxmox with an import store", map[string]any{"provider": "proxmox", "importStorage": "nas-templates"},
			[]string{"serverSnapshots", "userData", "serverPassword", "serverRebuild", "serverDiskSnapshots", "subnets", "spreadGroups", "imageImport"}},
		{"proxmox with two profiles", map[string]any{
			"provider": "proxmox", "storageProfiles": map[string]any{"std": profile("local-zfs"), "nas": profile("nas")},
		}, []string{"serverSnapshots", "userData", "serverPassword", "serverRebuild", "serverDiskSnapshots", "subnets", "spreadGroups", "volumeRetype"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cloudFeatures(&externalservice.ExternalService{Config: c.cfg})
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("cloudFeatures = %v, want %v", got, c.want)
			}
		})
	}
}

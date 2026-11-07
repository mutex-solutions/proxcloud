package project

import (
	"strings"
	"testing"
)

const (
	diskA = "vm-9999-pvc-11111111-1111-1111-1111-111111111111"
	diskB = "vm-9999-pvc-22222222-2222-2222-2222-222222222222"
	diskC = "vm-9999-pvc-33333333-3333-3333-3333-333333333333"
	diskF = "vm-9999-pvc-44444444-4444-4444-4444-444444444444.raw"
)

func adoptBase() csiAdoptInput {
	return csiAdoptInput{
		ClusterID: "pc-c-mine", Region: "pve", Storages: []string{"local-zfs", "nas"},
		Others: map[string][]csiPV{},
		Disks: map[string]csiDisk{
			csiKey("local-zfs", diskA): {VolID: "local-zfs:" + diskA, Node: "pve1", Size: 1 << 30},
			csiKey("local-zfs", diskB): {VolID: "local-zfs:" + diskB, Node: "pve1", Size: 2 << 30},
			csiKey("local-zfs", diskC): {VolID: "local-zfs:" + diskC, Node: "pve2", Size: 1 << 30},
			csiKey("nas", diskF):       {VolID: "nas:9999/" + diskF, Node: "pve1", Size: 1 << 30},
		},
		Attached: map[string]int{}, Workers: map[int]bool{101: true}, Rows: map[string]string{},
	}
}

func reasonOf(t *testing.T, in csiAdoptInput, pv string) string {
	t.Helper()
	adopt, refuse, _ := planCSIAdoption(in)
	for _, a := range adopt {
		if a.PV == pv {
			return ""
		}
	}
	for _, r := range refuse {
		if r.PV == pv {
			return r.Reason
		}
	}
	t.Fatalf("pv %s is neither adopted nor refused", pv)
	return ""
}

func TestPlanCSIAdoption(t *testing.T) {
	cases := []struct {
		name   string
		mod    func(*csiAdoptInput)
		handle string
		want   string // "" = adopted; else a fragment of the reason
	}{
		{"own detached disk", nil, "pve/pve1/local-zfs/" + diskA, ""},
		{"own disk on its worker", func(in *csiAdoptInput) { in.Attached[csiKey("local-zfs", diskA)] = 101 }, "pve/pve1/local-zfs/" + diskA, ""},
		{"file store disk under the controller's directory", nil, "pve//nas/9999/" + diskF, ""},
		{"file store disk under another vmid", nil, "pve//nas/123/" + diskF, "not region/zone/storage/disk"},
		{"short handle", nil, "pve/pve1/" + diskA, "not region/zone/storage/disk"},
		{"other region", nil, "eu/pve1/local-zfs/" + diskA, "region eu"},
		{"store the cluster is not offered", nil, "pve/pve1/other/" + diskA, "storage other"},
		{"a boot disk", func(in *csiAdoptInput) {
			in.Disks[csiKey("local-zfs", "vm-101-disk-0")] = csiDisk{VolID: "local-zfs:vm-101-disk-0", Node: "pve1"}
		}, "pve/pve1/local-zfs/vm-101-disk-0", "not a name proxmox-csi gives"},
		{"disk not on PVE", nil, "pve/pve1/local-zfs/vm-9999-pvc-55555555-5555-5555-5555-555555555555", "not on PVE"},
		{"named by another cluster too (a crafted PV)", func(in *csiAdoptInput) {
			in.Others["pc-c-other"] = []csiPV{{Name: "theirs", Handle: "pve/pve1/local-zfs/" + diskA}}
		}, "pve/pve1/local-zfs/" + diskA, "also named by a PersistentVolume of pc-c-other"},
		{"registry holds it for another cluster", func(in *csiAdoptInput) { in.Rows[csiKey("local-zfs", diskA)] = "pc-c-gw" },
			"pve/pve1/local-zfs/" + diskA, "registry holds the disk for cluster pc-c-gw"},
		{"attached to a VM that is not a worker", func(in *csiAdoptInput) { in.Attached[csiKey("local-zfs", diskA)] = 202 },
			"pve/pve1/local-zfs/" + diskA, "attached to VM 202"},
		{"detached while another cluster is unread", func(in *csiAdoptInput) { in.Unread = []string{"pc-c-down"} },
			"pve/pve1/local-zfs/" + diskA, "could not be read"},
		// The old token let a cluster attach another's disk to its own worker: attached is no proof while one is unread.
		{"attached to a worker while another cluster is unread", func(in *csiAdoptInput) {
			in.Unread = []string{"pc-c-down"}
			in.Attached[csiKey("local-zfs", diskA)] = 101
		}, "pve/pve1/local-zfs/" + diskA, "pc-c-down could not be read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := adoptBase()
			if tc.mod != nil {
				tc.mod(&in)
			}
			in.PVs = []csiPV{{Name: "pv1", Handle: tc.handle}}
			got := reasonOf(t, in, "pv1")
			if tc.want == "" && got != "" {
				t.Fatalf("refused: %s", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("reason %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

func TestPlanCSIAdoptionDetails(t *testing.T) {
	in := adoptBase()
	in.PVs = []csiPV{{Name: "a", Handle: "pve/pve1/local-zfs/" + diskA}, {Name: "a-again", Handle: "pve/pve1/local-zfs/" + diskA},
		{Name: "f", Handle: "pve//nas/9999/" + diskF}, {Name: "b", Handle: "pve/pve1/local-zfs/" + diskB}}
	in.Rows[csiKey("local-zfs", diskB)] = "pc-c-mine" // a run before adopted it
	in.Attached[csiKey("local-zfs", diskC)] = 300     // a disk in use by something else, named by nobody
	in.Disks[csiKey("local-zfs", "vm-9999-pvc-66666666-6666-6666-6666-666666666666")] = csiDisk{VolID: "local-zfs:vm-9999-pvc-66666666-6666-6666-6666-666666666666", Node: "pve2"}
	adopt, refuse, orphans := planCSIAdoption(in)
	if len(refuse) != 0 {
		t.Fatalf("refused %+v", refuse)
	}
	if len(adopt) != 3 {
		t.Fatalf("a second PV of the same disk is not adopted twice: %+v", adopt)
	}
	byPV := map[string]CSIMigrationPV{}
	for _, a := range adopt {
		byPV[a.PV] = a
	}
	if f := byPV["f"]; f.VolID != "nas:9999/"+diskF || f.Node != "pve1" || f.SizeBytes != 1<<30 {
		t.Fatalf("a file store disk keeps the volid PVE lists: %+v", f)
	}
	if !byPV["b"].Existing || byPV["a"].Existing {
		t.Fatalf("existing marks only a disk the registry already holds for the cluster: %+v", adopt)
	}
	// diskC is attached to a VM: in use, so not an orphan. The 6666 disk is named by nobody and attached nowhere.
	if len(orphans) != 1 || !strings.Contains(orphans[0], "66666666") {
		t.Fatalf("orphans %v", orphans)
	}
}

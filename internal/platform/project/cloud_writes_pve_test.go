package project

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

func TestPVEPlacementPerNodeCopies(t *testing.T) {
	caps := map[string]pve.StorageCapability{"local-zfs": {Storage: "local-zfs", Nodes: map[string]pve.StorageNodeState{
		"pve1": {Active: true, Total: 100 << 30, Avail: 90 << 30},
		"pve2": {Active: true, Total: 100 << 30, Avail: 90 << 30},
		"pve3": {Active: true, Total: 100 << 30, Avail: 90 << 30},
	}}}
	profiles := map[string]externalservice.PVEStorageProfile{"std": {Slug: "std", Storages: map[string]string{"*": "local-zfs"}}}
	tpl := &externalservice.PVETemplate{Slug: "debian-13", VMID: 9001, Node: "pve1", Copies: map[string]int{"pve1": 9001, "pve2": 9002}}
	pl, herr := pvePlacement(tpl, false, "std", profiles, caps)
	if herr != nil {
		t.Fatal(herr)
	}
	if !pl.allowed["pve1"] || !pl.allowed["pve2"] || pl.allowed["pve3"] {
		t.Fatalf("allowed = %v, want the two nodes with a copy", pl.allowed)
	}
	if tpl.OnNode("pve2") != 9002 || tpl.OnNode("pve3") != 0 {
		t.Fatalf("OnNode: pve2=%d pve3=%d", tpl.OnNode("pve2"), tpl.OnNode("pve3"))
	}
}

func TestPVEPlacement(t *testing.T) {
	const g = int64(1) << 30
	roomy := pve.StorageNodeState{Active: true, Total: 1000 * g, Avail: 900 * g}
	full := pve.StorageNodeState{Active: true, Total: 1000 * g, Avail: 10 * g}
	caps := map[string]pve.StorageCapability{
		"local-zfs":     {Nodes: map[string]pve.StorageNodeState{"pve1": full, "pve2": roomy, "pve3": {Total: 1000 * g}}},
		"nas-templates": {Shared: true, Nodes: map[string]pve.StorageNodeState{"pve1": roomy, "pve2": roomy, "pve3": roomy}},
		"ceph":          {Shared: true, Nodes: map[string]pve.StorageNodeState{"pve1": full, "pve2": roomy, "pve3": roomy}},
	}
	nodes := []pve.Node{
		{Node: "pve1", Status: "online", MaxCPU: 16, MaxMem: 128 << 30},
		{Node: "pve2", Status: "online", MaxCPU: 16, MaxMem: 64 << 30},
		{Node: "pve3", Status: "online", MaxCPU: 16, MaxMem: 32 << 30},
	}
	profiles := map[string]externalservice.PVEStorageProfile{
		"std":   {Slug: "std", Storages: map[string]string{"*": "local-zfs"}},
		"ceph":  {Slug: "ceph", Storages: map[string]string{"*": "ceph"}},
		"ghost": {Slug: "ghost", Storages: map[string]string{"*": "gone"}},
		"capacity": {Slug: "capacity", Label: "Capacity HDD (NFS)", Class: pve.ClassCapacity,
			Storages: map[string]string{"*": "nas-templates"}},
		"noboot": {Slug: "noboot", NoBootDisk: true, Storages: map[string]string{"*": "local-zfs"}},
	}
	sharedCfg := pve.VMConfig{"scsi0": "nas-templates:9000/base-9000-disk-0.raw,size=10G", "ide2": "nas-templates:9000/vm-9000-cloudinit.qcow2,media=cdrom"}
	localCfg := pve.VMConfig{"scsi0": "local-zfs:base-9000-disk-0,size=10G"}
	tests := []struct {
		name     string
		tplNode  string
		cfg      pve.VMConfig
		profile  string
		wantNode string // "" = no node fits
		wantErr  string // placement 409 substring
	}{
		{"shared template + shared profile: full pool on pve1 skipped", "pve1", sharedCfg, "ceph", "pve2", ""},
		// PVE refuses a cross-node clone onto non-shared storage ("can't clone to non-shared storage").
		{"shared template + local profile: pinned to the template node", "pve2", sharedCfg, "std", "pve2", ""},
		{"shared template + local profile, full pool on its node: no capacity", "pve1", sharedCfg, "std", "", ""},
		{"shared template, no profile: any node", "pve3", sharedCfg, "", "pve1", ""},
		{"local template pins to its node", "pve2", localCfg, "std", "pve2", ""},
		{"local template + shared profile: still its node", "pve3", localCfg, "ceph", "pve3", ""},
		{"local template on a node with a full pool: no capacity", "pve1", localCfg, "std", "", ""},
		{"local template, no profile: its node only", "pve3", localCfg, "", "pve3", ""},
		{"local template where the profile is inactive: clean 409", "pve3", localCfg, "std", "", "can only be cloned on node pve3"},
		{"profile active nowhere", "pve1", sharedCfg, "ghost", "", "no active storage on any node"},
		{"capacity profile holds no boot disk", "pve1", sharedCfg, "capacity", "", `"capacity" (Capacity HDD (NFS)) is HDD capacity storage and cannot hold a boot disk`},
		{"profile downgraded bootDisk:false", "pve1", sharedCfg, "noboot", "", `"noboot" (noboot) does not allow boot disks`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tpl := &externalservice.PVETemplate{Slug: "u24", Name: "Ubuntu 24.04", VMID: 9000, Node: tt.tplNode}
			pl, herr := pvePlacement(tpl, pve.TemplateShared(tt.cfg, caps), tt.profile, profiles, caps)
			if tt.wantErr != "" {
				if herr == nil || herr.Status != http.StatusConflict || !strings.Contains(herr.Error(), tt.wantErr) {
					t.Fatalf("herr = %v, want 409 containing %q", herr, tt.wantErr)
				}
				return
			}
			if herr != nil {
				t.Fatalf("unexpected placement error %v", herr)
			}
			got, err := pve.PickNode(nodes, nil, pve.SchedReq{
				VCPUs: 2, RAMMb: 2048, AllowedNodes: pl.allowed,
				Storage: pl.storage, DiskBytes: 20 * g, HeadroomPct: 10,
			})
			if tt.wantNode == "" {
				if err == nil {
					t.Fatalf("picked %q, want no capacity", got)
				}
				return
			}
			if err != nil || got != tt.wantNode {
				t.Fatalf("picked %q err %v, want %q", got, err, tt.wantNode)
			}
			if tt.profile == "" && pl.storage != nil {
				t.Fatalf("no profile must not filter on storage, got %v", pl.storage)
			}
		})
	}
}

func TestVMNameErr(t *testing.T) {
	if herr := vmNameErr("web-1.example.org"); herr != nil {
		t.Fatal(herr)
	}
	for _, name := range []string{"", "my server", "my_server", "-web"} {
		if herr := vmNameErr(name); herr == nil || herr.Status != http.StatusBadRequest {
			t.Errorf("%q: %v", name, herr)
		}
	}
}

func TestPVEVolumeErr(t *testing.T) {
	if he := pveVolumeErr(&pve.ConflictError{Msg: "Detach the volume before deleting it"}); he.Status != http.StatusConflict || he.Msg != "Detach the volume before deleting it" {
		t.Fatalf("conflict mapped to %d %q", he.Status, he.Msg)
	}
	if he := pveVolumeErr(errors.New("pve: task failed")); he.Status != http.StatusBadGateway {
		t.Fatalf("PVE failure mapped to %d", he.Status)
	}
	// Attaching to another project's server: a refusal, not a failing backend.
	if he := pveVolumeErr(fmt.Errorf("%w: vm 100 is not in project pool pc-x", pve.ErrNotInProject)); he.Status != http.StatusNotFound {
		t.Fatalf("foreign server mapped to %d %q", he.Status, he.Msg)
	}
}

func TestPVECapacityDisk(t *testing.T) {
	classes := map[string]string{"local-zfs": pve.ClassPerformance, "nas-capacity": pve.ClassCapacity}
	classOf := func(s string) string { return classes[s] }
	tests := []struct {
		name     string
		cfg      pve.VMConfig
		wantSlot string // "" = none
	}{
		{"all local", pve.VMConfig{"scsi0": "local-zfs:vm-1-disk-0,size=32G"}, ""},
		{"attached capacity data disk", pve.VMConfig{"scsi0": "local-zfs:vm-1-disk-0,size=32G", "scsi1": "nas-capacity:1/vm-1-disk-1.raw,size=500G"}, "scsi1"},
		{"detached capacity disk ignored", pve.VMConfig{"scsi0": "local-zfs:vm-1-disk-0,size=32G", "unused0": "nas-capacity:1/vm-1-disk-1.raw"}, ""},
		{"cdrom on capacity storage ignored", pve.VMConfig{"scsi0": "local-zfs:vm-1-disk-0,size=32G", "ide2": "nas-capacity:iso/x.iso,media=cdrom"}, ""},
		{"unknown class passes", pve.VMConfig{"scsi0": "other:vm-1-disk-0,size=32G"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := pveCapacityDisk(tt.cfg, classOf)
			switch {
			case tt.wantSlot == "" && d != nil:
				t.Fatalf("got %+v, want none", d)
			case tt.wantSlot != "" && (d == nil || d.Slot != tt.wantSlot || d.Storage != "nas-capacity"):
				t.Fatalf("got %+v, want %s", d, tt.wantSlot)
			}
		})
	}
}

func TestDbaasProfileRefusal(t *testing.T) {
	profiles := map[string]externalservice.PVEStorageProfile{
		"std":      {Slug: "std", Class: pve.ClassPerformance},
		"capacity": {Slug: "capacity", Label: "Capacity HDD (NFS)", Class: pve.ClassCapacity},
		"nodb":     {Slug: "nodb", NoDBaaS: true},
	}
	tests := []struct{ class, want string }{
		{"std", ""},
		{"local-path", ""}, // not a profile
		{"capacity", "HDD capacity storage — not for databases"},
		{"nodb", "does not allow databases"},
	}
	for _, tt := range tests {
		got := dbaasProfileRefusal(profiles, tt.class)
		if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
			t.Errorf("%s: got %q, want %q", tt.class, got, tt.want)
		}
	}
}

func TestPveVolumeErrDeadNode(t *testing.T) {
	he := pveVolumeErr(fmt.Errorf("disable protection: %w", &pve.APIError{Status: 595, Message: "No route to host"}))
	if he.Status != http.StatusConflict || strings.Contains(he.Msg, "595") {
		t.Fatalf("got %d %q", he.Status, he.Msg)
	}
}

// A tenant must not learn the backend's address from a failure: transport errors name the API, proxied ones a node.
func TestPVEErrorsHideTheBackend(t *testing.T) {
	const addr = "10.0.0.5"
	raw := []error{
		errors.New(`Get "https://` + addr + `:8006/api2/json/cluster/resources": dial tcp ` + addr + `:8006: connect: refused`),
		&pve.APIError{Status: http.StatusInternalServerError, Message: "proxy handler failed: ssh: connect to host " + addr + " port 22: No route to host"},
		&pve.APIError{Status: http.StatusBadRequest, Message: "bad", Errors: map[string]string{"x": addr}}, // only the rule path reads a 400
	}
	for _, err := range raw {
		if he := pveVolumeErr(err); he.Status != http.StatusBadGateway || strings.Contains(he.Msg, addr) {
			t.Errorf("%v: %d %q", err, he.Status, he.Msg)
		}
	}
	// A refusal the tenant can act on keeps its text.
	if he := pveVolumeErr(fmt.Errorf("keep the server's volumes: %w", &pve.ConflictError{Msg: "Another volume change is running"})); he.Status != http.StatusConflict || he.Msg != "Another volume change is running" {
		t.Errorf("conflict: %d %q", he.Status, he.Msg)
	}
	if he := pveVolumeErr(fmt.Errorf("%w: nas on node pve1", pve.ErrStorageFull)); he.Status != http.StatusConflict || strings.Contains(he.Msg, "pve1") {
		t.Errorf("storage full: %d %q", he.Status, he.Msg)
	}
	if he := sgRuleErr(&pve.APIError{Status: http.StatusBadRequest, Message: "Parameter verification failed.", Errors: map[string]string{"dport": "invalid format\n"}}); he.Status != http.StatusBadRequest || !strings.Contains(he.Msg, "dport: invalid format") {
		t.Errorf("bad rule: %d %q", he.Status, he.Msg)
	}
	if he := sgRuleErr(raw[0]); he.Status != http.StatusBadGateway || strings.Contains(he.Msg, addr) {
		t.Errorf("rule, backend down: %d %q", he.Status, he.Msg)
	}
}

// Every 502 of this package goes through upstreamErr. A new site that hands err.Error() to a tenant fails here.
func TestNoRawUpstreamErrorOn502(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	raw := regexp.MustCompile(`StatusBadGateway, http\.StatusBadGateway, [^\n]*\.Error\(\)`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := raw.Find(b); m != nil {
			t.Errorf("%s: %s", f, m)
		}
	}
}

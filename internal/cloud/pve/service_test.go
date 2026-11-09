package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

func TestSizeToGiB(t *testing.T) {
	cases := map[string]int{
		"32G": 32, "8192M": 8, "1T": 1024, "1500M": 2, // rounds UP
		"0": 0, "": 0, "1073741824": 1, "512K": 1,
	}
	for in, want := range cases {
		if got := sizeToGiB(in); got != want {
			t.Errorf("sizeToGiB(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseDisks(t *testing.T) {
	cfg := VMConfig{
		"scsi0":    "local-zfs:vm-101-disk-0,discard=on,size=32G,ssd=1",
		"scsi1":    "local-zfs:vm-101-disk-1,size=100G",
		"ide2":     "local:iso/ubuntu.iso,media=cdrom,size=2G", // cdrom skipped
		"unused0":  "local-zfs:vm-101-disk-2,size=8G",
		"efidisk0": "local-zfs:vm-101-disk-3,size=1M", // not a disk slot prefix we bill
		"net0":     "virtio=AA:BB,bridge=vmbr0",
		"cores":    float64(2),
	}
	disks := ParseDisks(cfg)
	bySlot := map[string]VMDisk{}
	for _, d := range disks {
		bySlot[d.Slot] = d
	}
	if len(disks) != 3 {
		t.Fatalf("disks = %d (%v), want 3", len(disks), bySlot)
	}
	root := bySlot["scsi0"]
	if !root.Boot || root.SizeGiB != 32 || root.Storage != "local-zfs" || root.VolID != "local-zfs:vm-101-disk-0" {
		t.Fatalf("root = %+v", root)
	}
	if bySlot["scsi1"].Boot || bySlot["scsi1"].SizeGiB != 100 {
		t.Fatalf("data = %+v", bySlot["scsi1"])
	}
	if _, ok := bySlot["unused0"]; !ok {
		t.Fatal("unused disk must be listed (still billed)")
	}
}

// assertShapeStable fails when v does not survive a JSON round-trip byte-identically.
func assertShapeStable(t *testing.T, what string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal: %v", what, err)
	}
	var back any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("%s: unmarshal: %v", what, err)
	}
	again, err := json.Marshal(back)
	if err != nil {
		t.Fatalf("%s: re-marshal: %v", what, err)
	}
	if string(raw) != string(again) {
		t.Fatalf("%s is not JSON-round-trip-stable:\n before %s\n after  %s", what, raw, again)
	}
	if _, ok := back.(map[string]any); ok {
		if _, ok := v.(map[string]any); !ok {
			t.Fatalf("%s decodes to map[string]any but is stored as %T — the keyed compare will churn", what, v)
		}
	}
}

func TestServerDataContract(t *testing.T) {
	r := ClusterResource{
		VMID: 101, Name: "web-1", Node: "pve2", Status: "running",
		Tags:   "proxcloud;fl-std2;img-ubuntu24;sp-std",
		MaxCPU: 2, MaxMem: 4 << 30, MaxDisk: 32 << 30,
	}
	data := ServerData(r, map[string]any{"private": []any{map[string]any{"addr": "10.0.0.5", "version": 4}}}, nil)

	if data["flavorName"] != "std2" {
		t.Fatalf("flavorName = %v", data["flavorName"])
	}
	if data["volumeBacked"] != true {
		t.Fatal("pve servers are always volumeBacked (disks bill as VOLUME)")
	}
	srv := data["server"].(map[string]any)
	if srv["name"] != "web-1" || srv["host"] != "pve2" || srv["status"] != "ACTIVE" || srv["availabilityZone"] != "pve2" {
		t.Fatalf("server = %v", srv)
	}
	fl := srv["flavor"].(map[string]any)
	if fl["vcpus"] != 2 || fl["ram"] != int64(4096) || fl["disk"] != int64(32) || fl["original_name"] != "std2" {
		t.Fatalf("flavor = %v", fl)
	}
	if _, ok := fl["extra_specs"].(map[string]any); !ok {
		t.Fatal("extra_specs must be a (possibly empty) map — nil means failed lookup to billing")
	}
	if srv["image"].(map[string]any)["id"] != "ubuntu24" {
		t.Fatalf("image = %v", srv["image"])
	}
	assertShapeStable(t, "data.server payload", data)
}

func TestMapVMStatus(t *testing.T) {
	cases := []struct{ status, lock, want string }{
		{"running", "", "ACTIVE"},
		{"stopped", "", "SHUTOFF"},
		{"stopped", "clone", "BUILD"},
		{"running", "migrate", "MIGRATING"},
	}
	for _, c := range cases {
		if got := MapVMStatus(c.status, c.lock); got != c.want {
			t.Errorf("MapVMStatus(%q,%q) = %q, want %q", c.status, c.lock, got, c.want)
		}
	}
}

func TestAgentAddresses(t *testing.T) {
	ifaces := []AgentInterface{
		{Name: "lo", IPAddresses: []struct {
			Type    string `json:"ip-address-type"`
			Address string `json:"ip-address"`
			Prefix  int    `json:"prefix"`
		}{{Type: "ipv4", Address: "127.0.0.1"}}},
		{Name: "eth0", IPAddresses: []struct {
			Type    string `json:"ip-address-type"`
			Address string `json:"ip-address"`
			Prefix  int    `json:"prefix"`
		}{{Type: "ipv4", Address: "10.0.0.5"}, {Type: "ipv6", Address: "fe80::1"}}},
	}
	got := AgentAddresses(ifaces)
	priv, ok := got["private"].([]any)
	if !ok || len(priv) != 1 || priv[0].(map[string]any)["addr"] != "10.0.0.5" {
		t.Fatalf("addresses = %v", got)
	}
	// SHAPE round-trip stability. The sync's keyed compare tolerates number width
	// (int vs float64) but NOT a typed nested collection: map[string][]map[string]any
	// is not reflect-convertible to map[string]any, so the compare falls through to
	// DeepEqual(typed, decoded) and reports a change on EVERY pass — live-caught as a
	// SERVER row updating forever. Re-marshalling the decoded value must reproduce the
	// original bytes: only plain map[string]any / []any / scalars do that.
	assertShapeStable(t, "addresses", got)
}

// fakeService builds a Service over a fake cluster with one pool VM, one
// foreign-pool VM and one pool template.
func fakeService(t *testing.T) *Service {
	t.Helper()
	routes := map[string]func(w http.ResponseWriter, r *http.Request){
		"GET /api2/json/cluster/resources": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[
				{"id":"qemu/101","type":"qemu","vmid":101,"node":"pve1","name":"vm-a","status":"running","pool":"tp-p1","tags":"proxcloud;fl-std2;img-u24","maxcpu":2,"maxmem":4294967296,"maxdisk":34359738368},
				{"id":"qemu/102","type":"qemu","vmid":102,"node":"pve1","name":"other","status":"running","pool":"tp-OTHER","maxcpu":1,"maxmem":1073741824},
				{"id":"qemu/9000","type":"qemu","vmid":9000,"node":"pve1","name":"tmpl-u24","status":"stopped","pool":"tp-p1","template":1,"maxdisk":10737418240},
				{"id":"node/pve1","type":"node","node":"pve1","status":"online"}
			]}`)
		},
		"GET /api2/json/nodes/pve1/qemu/101/config": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":{"scsi0":"local-zfs:vm-101-disk-0,size=32G","scsi1":"local-zfs:vm-101-disk-1,size=10G","tags":"proxcloud;fl-std2;sp-std"}}`)
		},
		"GET /api2/json/nodes/pve1/qemu/101/agent/network-get-interfaces": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"errors":{"agent":"No QEMU guest agent configured"}}`)
		},
	}
	_, c := fakePVE(t, routes)
	return &Service{C: c, ServiceID: "svc-pve", PoolPrefix: "tp-"}
}

func TestServerSyncProviderPoolFilter(t *testing.T) {
	s := fakeService(t)
	p := s.ServerSyncProvider("pve", "p1", nil)
	if p.Type() != cloud.TypeServer || p.ProjectID() != "p1" {
		t.Fatal("provider identity")
	}
	rs, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// only vm 101: 102 is another pool's, 9000 is a template
	if len(rs) != 1 || rs[0].ExternalID != "101" {
		t.Fatalf("resources = %+v", rs)
	}
	if rs[0].Data["flavorName"] != "std2" {
		t.Fatalf("data = %v", rs[0].Data)
	}
}

func TestVolumeSyncProvider(t *testing.T) {
	s := fakeService(t)
	rs, err := s.VolumeSyncProvider("pve", "p1", nil).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("volumes = %d", len(rs))
	}
	byExt := map[string]cloud.CloudResource{}
	for _, r := range rs {
		byExt[r.ExternalID] = r
	}
	root := byExt["local-zfs:vm-101-disk-0"].Data["volume"].(map[string]any)
	if root["size"] != 32 || root["bootable"] != true || root["volume_type"] != "std" || root["status"] != "in-use" {
		t.Fatalf("root volume = %v", root)
	}
}

func TestVolumeSyncProviderProfileOf(t *testing.T) {
	s := fakeService(t)
	// scsi1 was retyped onto a capacity profile: its profile, not the VM's, drives volume_type.
	profileOf := func(node, storage, vmProfile string) (string, string) {
		if node != "pve1" || storage != "local-zfs" || vmProfile != "std" {
			t.Errorf("profileOf(%q, %q, %q)", node, storage, vmProfile)
		}
		return "capacity", ClassCapacity
	}
	rs, err := s.VolumeSyncProvider("pve", "p1", profileOf).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		v := r.Data["volume"].(map[string]any)
		if v["profile"] != "capacity" || v["storage_class"] != ClassCapacity || v["volume_type"] != "capacity" {
			t.Fatalf("%s: volume = %v", r.ExternalID, v)
		}
	}
	// An unresolved disk keeps the VM-tag fallback and stamps empty profile/class.
	rs, _ = s.VolumeSyncProvider("pve", "p1", func(string, string, string) (string, string) { return "", "" }).List(context.Background())
	if v := rs[0].Data["volume"].(map[string]any); v["volume_type"] != "std" || v["profile"] != "" {
		t.Fatalf("fallback volume = %v", v)
	}
}

func TestImageSyncProvider(t *testing.T) {
	s := fakeService(t)
	rs, err := s.ImageSyncProvider("pve", "p1").List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].ExternalID != "9000" {
		t.Fatalf("images = %+v", rs)
	}
	img := rs[0].Data["image"].(map[string]any)
	if img["name"] != "tmpl-u24" || img["status"] != "active" {
		t.Fatalf("image = %v", img)
	}
}

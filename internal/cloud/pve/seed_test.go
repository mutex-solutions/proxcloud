package pve

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildSeedISOReadableByXorriso(t *testing.T) {
	xorriso, err := exec.LookPath("xorriso")
	if err != nil {
		t.Skip("xorriso not installed")
	}
	files := map[string]string{"meta-data": `{"instance-id":"iid-1"}`, "user-data": strings.Repeat("x", 5000), "network-config": "{}"}
	iso, err := BuildSeedISO(files)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Cleanup(func() { // xorriso restores the image's read-only modes
		_ = filepath.WalkDir(dir, func(p string, _ os.DirEntry, _ error) error { return os.Chmod(p, 0o700) })
	})
	p := filepath.Join(dir, "seed.iso")
	if err := os.WriteFile(p, iso, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(xorriso, "-indev", p, "-osirrox", "on", "-extract", "/", filepath.Join(dir, "x")).CombinedOutput()
	if err != nil {
		t.Fatalf("xorriso: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "cidata") {
		t.Fatalf("volume label not read:\n%s", out)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, "x", name))
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		if string(got) != want {
			t.Fatalf("%s: %d bytes, want %d", name, len(got), len(want))
		}
	}
}

func TestRenderSeed(t *testing.T) {
	s, err := RenderSeed(SeedSpec{
		InstanceID: "iid-1", Hostname: "web", UserData: "#!/bin/sh\necho hi\n",
		PasswordHash: "$2a$10$x", SSHKeys: "ssh-ed25519 AAA a\n\nssh-ed25519 BBB b\n",
		Metadata: map[string]string{"instance-id": "evil", "env": "prod"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(s.MetaData), &meta); err != nil {
		t.Fatal(err)
	}
	if meta["instance-id"] != "iid-1" || meta["meta"].(map[string]any)["instance-id"] != "evil" {
		t.Fatalf("a tenant key must stay under meta: %v", meta)
	}
	if s.UserData != "#!/bin/sh\necho hi\n" {
		t.Fatalf("user-data must pass through untouched: %q", s.UserData)
	}
	var cc map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(s.VendorData, "#cloud-config\n")), &cc); err != nil {
		t.Fatalf("vendor-data: %v: %q", err, s.VendorData)
	}
	if cc["password"] != "$2a$10$x" || len(cc["ssh_authorized_keys"].([]any)) != 2 {
		t.Fatalf("vendor-data: %v", cc)
	}

	s, err = RenderSeed(SeedSpec{InstanceID: "i", Hostname: "h", PasswordHash: "$2a$10$x", LoginUser: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.VendorData, `"hashed_passwd":"$2a$10$x"`) || !strings.Contains(s.VendorData, `"name":"dev"`) || strings.Contains(s.VendorData, `"password"`) {
		t.Fatalf("a login user gets the hash, the default user nothing: %q", s.VendorData)
	}

	s, err = RenderSeed(SeedSpec{InstanceID: "i", Hostname: "h", OperatorVendorData: "#!/bin/sh\ntrue\n"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.VendorData, "Content-Type: multipart/mixed") || !strings.Contains(s.VendorData, "text/x-shellscript") || !strings.Contains(s.VendorData, "text/cloud-config") {
		t.Fatalf("operator vendor-data must join in a multipart: %q", s.VendorData)
	}
	if !strings.HasPrefix(s.UserData, "#cloud-config") {
		t.Fatalf("missing user-data must render a no-op cloud-config: %q", s.UserData)
	}
}

func TestRenderNetworkConfig(t *testing.T) {
	cfg := VMConfig{
		"net0":      "virtio=BC:24:11:AA:BB:01,bridge=v1,firewall=1",
		"ipconfig0": "ip=10.0.0.5/24,gw=10.0.0.1",
		"net1":      "virtio=BC:24:11:AA:BB:02,bridge=vmbr1,firewall=1",
		"ipconfig1": "ip=192.0.2.5/24,gw=192.0.2.1",
	}
	raw, err := RenderNetworkConfig(cfg, []string{"192.0.2.53"})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Config []map[string]any `json:"config"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Config) != 3 || got.Config[0]["name"] != "eth0" || got.Config[1]["name"] != "eth1" || got.Config[2]["type"] != "nameserver" {
		t.Fatalf("config: %s", raw)
	}
	if got.Config[1]["mac_address"] != "bc:24:11:aa:bb:02" {
		t.Fatalf("mac: %s", raw)
	}
	raw, err = RenderNetworkConfig(VMConfig{"net0": "virtio=BC:24:11:AA:BB:01,bridge=v1", "ipconfig0": "ip=dhcp"}, nil)
	if err != nil || raw != "" {
		t.Fatalf("plain DHCP on net0 stays on the fallback: %q %v", raw, err)
	}
	if _, err := RenderNetworkConfig(VMConfig{"net0": "virtio,bridge=v1", "ipconfig0": "ip=10.0.0.5/24"}, nil); err == nil {
		t.Fatal("a NIC without a MAC must fail: cloud-init could not match it")
	}
}

func TestSmbiosWithSerial(t *testing.T) {
	got := smbiosWithSerial("uuid=abc,serial=old,base64=1", SeedSerial("http://x/y/"))
	if !strings.HasPrefix(got, "uuid=abc,base64=1,serial=") || strings.Contains(got, "old") {
		t.Fatalf("smbios1 = %q", got)
	}
}

func TestPickSeedStorage(t *testing.T) {
	caps := map[string]StorageCapability{
		"local": {Content: []string{"iso"}, Nodes: map[string]StorageNodeState{"n1": {Active: true}}},
		"nas":   {Content: []string{"iso"}, Shared: true, Nodes: map[string]StorageNodeState{"n1": {Active: true}}},
		"zfs":   {Content: []string{"images"}, Nodes: map[string]StorageNodeState{"n1": {Active: true}}},
	}
	if got := PickSeedStorage(caps, "n1", ""); got != "nas" {
		t.Fatalf("shared first, got %q", got)
	}
	if got := PickSeedStorage(caps, "n1", "local"); got != "local" {
		t.Fatalf("configured wins, got %q", got)
	}
	if got := PickSeedStorage(caps, "n2", ""); got != "" {
		t.Fatalf("nothing active on n2, got %q", got)
	}
}

func TestCloudInitSlot(t *testing.T) {
	if got := cloudInitSlot(VMConfig{"ide0": "local-zfs:vm-1-cloudinit,media=cdrom"}); got != "ide0" {
		t.Fatalf("PVE drive: %q", got)
	}
	cfg := VMConfig{"ide2": "nas:iso/pcseed-1-a.iso,media=cdrom"}
	if cloudInitSlot(cfg) != "ide2" || seedVolOf(cfg) != "nas:iso/pcseed-1-a.iso" {
		t.Fatalf("seed CD: %q %q", cloudInitSlot(cfg), seedVolOf(cfg))
	}
	if seedVolOf(VMConfig{"ide2": "nas:iso/ubuntu.iso,media=cdrom"}) != "" {
		t.Fatal("a tenant CD is not a seed")
	}
}

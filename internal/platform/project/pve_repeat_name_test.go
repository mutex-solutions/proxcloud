package project

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// The proxy's opening line of a serial session is a frame termproxy's pveproxy can read: masked, binary, any length.
func TestWSClientFrame(t *testing.T) {
	for _, n := range []int{5, 125, 126, 300, 70000} {
		payload := bytes.Repeat([]byte("u"), n)
		f := wsClientFrame(payload)
		if f[0] != 0x82 || f[1]&0x80 == 0 {
			t.Fatalf("%d: not a masked binary frame: %x", n, f[:2])
		}
		size, at := int(f[1]&0x7f), 2
		switch size {
		case 126:
			size, at = int(binary.BigEndian.Uint16(f[2:])), 4
		case 127:
			size, at = int(binary.BigEndian.Uint64(f[2:])), 10
		}
		key, body := f[at:at+4], f[at+4:]
		if size != n || len(body) != n {
			t.Fatalf("%d: length %d, body %d", n, size, len(body))
		}
		for i := range body {
			body[i] ^= key[i%4]
		}
		if !bytes.Equal(body, payload) {
			t.Fatalf("%d: payload does not unmask", n)
		}
	}
}

func TestPublicNICPlugged(t *testing.T) {
	cfg := pve.VMConfig{"net1": "virtio=BC:24:11:00:00:01,bridge=vmbr1,firewall=1", "ipconfig1": "ip=10.0.0.5/24,gw=10.0.0.1"}
	if !publicNICPlugged(cfg, "vmbr1", "10.0.0.5") {
		t.Fatal("a plugged NIC must read as plugged: a repeat attach would rewrite it")
	}
	for name, c := range map[string]pve.VMConfig{
		"no NIC":         {"ipconfig1": "ip=10.0.0.5/24"},
		"other bridge":   {"net1": "virtio,bridge=vmbr10,firewall=1", "ipconfig1": "ip=10.0.0.5/24"},
		"other address":  {"net1": "virtio,bridge=vmbr1", "ipconfig1": "ip=10.0.0.50/24"},
		"no ip settings": {"net1": "virtio,bridge=vmbr1"},
	} {
		if publicNICPlugged(c, "vmbr1", "10.0.0.5") {
			t.Fatalf("%s: read as plugged, so a held address would never be plugged again", name)
		}
	}
}

func TestPVEVolumeName(t *testing.T) {
	if n, err := pveVolumeName(map[string]any{"name": "  data  "}); err != nil || n != "data" {
		t.Fatalf("%q %v", n, err)
	}
	if n, err := pveVolumeName(map[string]any{}); err != nil || n != "" {
		t.Fatalf("no name: %q %v", n, err)
	}
	for _, bad := range []string{strings.Repeat("x", 65), "a\nb"} {
		if _, err := pveVolumeName(map[string]any{"name": bad}); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	// A sync re-reads the disk with its derived name; the label wins.
	cached := map[string]any{"volume": map[string]any{pve.VolumeLabel: "data", "name": "data"}}
	fresh := map[string]any{"volume": map[string]any{"name": "vm1-scsi1"}}
	pve.CarryVolumeLabel(cached, fresh)
	if v := fresh["volume"].(map[string]any); v["name"] != "data" || v[pve.VolumeLabel] != "data" {
		t.Fatalf("%v", v)
	}
}

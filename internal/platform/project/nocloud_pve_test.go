package project

import (
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

func TestSeedHostname(t *testing.T) {
	for in, want := range map[string]string{
		"Web Server #1":                "web-server-1",
		"--x--":                        "x",
		"日本語":                          "server",
		strings.Repeat("a", 70):        strings.Repeat("a", 63),
		strings.Repeat("a", 62) + " b": strings.Repeat("a", 62),
	} {
		if got := seedHostname(in); got != want {
			t.Fatalf("seedHostname(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPveNeedsSeed(t *testing.T) {
	plain := &externalservice.ExternalService{Config: map[string]any{"provider": "proxmox"}}
	vendor := &externalservice.ExternalService{Config: map[string]any{"provider": "proxmox", "metadata": map[string]any{"vendorData": "#!/bin/sh\n"}}}
	if pveNeedsSeed(plain, pveSeedInput{Password: "x", SSHKeys: "k"}) {
		t.Fatal("a password and keys stay on PVE's own drive")
	}
	if !pveNeedsSeed(plain, pveSeedInput{UserData: "#cloud-config\n"}) {
		t.Fatal("user-data needs a seed")
	}
	if !pveNeedsSeed(plain, pveSeedInput{Metadata: map[string]string{"a": "b"}}) {
		t.Fatal("metadata needs a seed")
	}
	if !pveNeedsSeed(vendor, pveSeedInput{}) {
		t.Fatal("the operator's vendor-data needs a seed")
	}
}

package project

import (
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
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

func TestStaleSeed(t *testing.T) {
	now := time.Now().UTC()
	old, fresh := now.Add(-time.Hour), now.Add(-time.Minute)
	mine := &cloud.CloudResource{ProjectID: "a"}
	theirs := &cloud.CloudResource{ProjectID: "b"}
	for _, c := range []struct {
		name    string
		created *time.Time
		row     *cloud.CloudResource
		want    bool
	}{
		{"VM alive", &old, mine, false},
		{"VM gone", &old, nil, true},
		{"vmid reused by another project", &old, theirs, true},
		{"create still racing its row", &fresh, nil, false},
	} {
		if got := staleSeed(&ServerSeed{ProjectID: "a", CreatedAt: c.created}, c.row, now); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPveNeedsSeed(t *testing.T) {
	plain := &externalservice.ExternalService{Config: map[string]any{"provider": "proxmox"}}
	vendor := &externalservice.ExternalService{Config: map[string]any{"provider": "proxmox", "metadata": map[string]any{"vendorData": "#!/bin/sh\n"}}}
	if pveNeedsSeed(plain, pveSeedInput{SSHKeys: "k"}) {
		t.Fatal("keys stay on PVE's own drive")
	}
	if !pveNeedsSeed(plain, pveSeedInput{Password: "x"}) {
		t.Fatal("a password needs ssh_pwauth, which PVE's drive cannot carry")
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

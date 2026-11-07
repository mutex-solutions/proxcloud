package project

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

func TestLBMemberVM(t *testing.T) {
	vm := func(pool, tags string) pve.ClusterResource { return pve.ClusterResource{Pool: pool, Tags: tags} }
	stamped := pve.CreatedTag(100)
	cases := []struct {
		name   string
		vm     pve.ClusterResource
		listed bool
		id     string
		stamp  string
		ok     bool
	}{
		{"own server, new member", vm("p-a", stamped), true, "7", "", true},
		{"own server, same stamp", vm("p-a", stamped), true, "7", "100", true},
		{"another project's server", vm("p-b", stamped), true, "7", "", false},
		{"no such server", vm("", ""), false, "7", "", false},
		{"the appliance itself", vm("p-a", stamped), true, "9", "", false},
		{"template", pve.ClusterResource{Pool: "p-a", Template: 1}, true, "7", "", false},
		{"vmid reused in the project", vm("p-a", pve.CreatedTag(200)), true, "7", "100", false},
		{"dead node keeps its member", pve.ClusterResource{Pool: "p-a", NodeDown: true}, true, "7", "100", true},
	}
	for _, c := range cases {
		if _, ok := lbMemberVM(c.vm, c.listed, "p-a", c.id, "9", c.stamp); ok != c.ok {
			t.Errorf("%s: got %v", c.name, ok)
		}
	}
}

func TestLBMemberIP(t *testing.T) {
	lb := &loadBalancer{NetworkID: "vnet1", PrivateIP: "10.0.1.12", PrivatePrefix: 24}
	row := func(project, network, nic, ip string) []PublicIP {
		return []PublicIP{{ProjectID: project, NetworkID: network, NIC: nic, IP: ip}}
	}
	if got := lbMemberIP(row("a", "vnet1", "net0", "10.0.1.5"), "a", lb); got != "10.0.1.5" {
		t.Fatalf("own address refused: %q", got)
	}
	for name, held := range map[string][]PublicIP{
		"another project's row": row("b", "vnet1", "net0", "10.0.1.5"),
		"another network":       row("a", "vnet2", "net0", "10.0.1.5"),
		"a public NIC":          row("a", "vnet1", "net1", "10.0.1.5"),
		"outside the subnet":    row("a", "vnet1", "net0", "10.0.2.5"),
		"garbage":               row("a", "vnet1", "net0", "10.0.1.5\nserver x"),
	} {
		if got := lbMemberIP(held, "a", lb); got != "" {
			t.Errorf("%s: got %q", name, got)
		}
	}
}

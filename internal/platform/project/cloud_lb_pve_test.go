package project

import (
	"context"
	"errors"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
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
		{"another balancer's appliance", vm("p-a", stamped+";"+pve.LBTag("x")), true, "7", "", false},
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

func TestLBOwnerOf(t *testing.T) {
	saved := loadBalancer{ID: "a", Name: "saved", ServerID: "7"}
	building := loadBalancer{ID: "b", Name: "building"}
	server := func(tag string) *cloud.CloudResource {
		data := map[string]any{}
		if tag != "" {
			data["loadBalancerId"] = tag
		}
		return &cloud.CloudResource{Type: cloud.TypeServer, Data: map[string]any{"server": data}}
	}
	balancers := []loadBalancer{saved, building}
	cases := []struct {
		name   string
		server *cloud.CloudResource
		id     string
		owner  string
	}{
		{"the saved balancer's VM", server("a"), "7", "saved"},
		{"sampled before the balancer's row knows its VM", server("b"), "8", "building"},
		{"tag with no balancer row left", server("gone"), "8", ""},
		{"a disk on a VM with no row yet", nil, "7", "saved"},
		{"a new server on the balancer's old vmid", server(""), "7", ""},
		{"a tenant's server", server(""), "9", ""},
		{"nothing known", nil, "9", ""},
	}
	for _, c := range cases {
		got := ""
		if lb := lbOwnerOf(c.server, c.id, balancers); lb != nil {
			got = lb.Name
		}
		if got != c.owner {
			t.Errorf("%s: got %q", c.name, got)
		}
	}
}

// A row with no id must not match what an untagged VM's tag reads, so it owns nothing and a delete refuses it.
func TestLBRowWithNoID(t *testing.T) {
	for name, data := range map[string]map[string]any{
		"no data":  nil,
		"empty":    {lbDataKey: map[string]any{}},
		"blank id": {lbDataKey: map[string]any{"id": "", "name": "x", "serverId": "7"}},
	} {
		cr := &cloud.CloudResource{ExternalID: "lb-x", Type: cloud.TypeLoadBalancer, Data: data}
		if lb, err := lbOfRow(cr); err == nil {
			t.Errorf("%s: read as %+v", name, lb)
		}

		untagged := &cloud.CloudResource{Type: cloud.TypeServer, Data: map[string]any{"server": map[string]any{}}}
		if lb := lbOwnerOf(untagged, "9", lbsOfRows([]cloud.CloudResource{*cr})); lb != nil {
			t.Errorf("%s: owns an untagged server", name)
		}

		reached := false
		h := &Handler{pveFor: func(*externalservice.ExternalService) (*pve.Service, error) {
			reached = true
			return nil, errors.New("not to be reached")
		}}
		if err := h.pveDeleteLoadBalancer(context.Background(), nil, &Project{}, cr); err == nil || reached {
			t.Errorf("%s: delete went on to the cluster: %v", name, err)
		}
	}
	if lb, err := lbOfRow(&cloud.CloudResource{Data: map[string]any{lbDataKey: map[string]any{"id": "a"}}}); err != nil || lb.ID != "a" {
		t.Fatalf("good row: %+v, %v", lb, err)
	}
}

func TestLBStopWhileDisabled(t *testing.T) {
	server := &cloud.CloudResource{Type: cloud.TypeServer}
	disk := &cloud.CloudResource{Type: cloud.TypeVolume}
	disabled, enabled := &Project{Status: StatusDisabled}, &Project{Status: StatusEnabled}
	invoke := map[string]any{"action": "stop"}
	cases := []struct {
		name   string
		proj   *Project
		cr     *cloud.CloudResource
		action string
		data   map[string]any
		want   bool
	}{
		{"suspended, stop", disabled, server, "STOP", nil, true},
		{"suspended, stop through INVOKEACTION", disabled, server, "INVOKEACTION", invoke, true},
		{"suspended, set password", disabled, server, "SET_PASSWORD", nil, false},
		{"suspended, its disk", disabled, disk, "DETACH", nil, false},
		{"not suspended, stop", enabled, server, "STOP", nil, false},
	}
	for _, c := range cases {
		if got := lbStopWhileDisabled(c.proj, c.cr, c.action, c.data); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

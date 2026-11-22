package project

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// UPDATE_CONFIG takes every layer 4 option, stores it in the row as it was sent, and still reads a body from before
// v0.11.1. What no appliance serves yet is a 400 that says so.
func TestLBSpecOfRequest(t *testing.T) {
	body := func(js string) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(js), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	old, err := lbSpecOfRequest(body(`{"listeners":[{"port":80,"pool":"web"},{"port":5432,"pool":"db","sourceRanges":["198.51.100.7"]}],
		"pools":[{"name":"web","members":[{"serverId":"101","port":8080}]},
		         {"name":"db","protocol":"tcp","algorithm":"leastconn","proxyProtocol":true,"healthCheck":{"type":"http","port":8008,"path":"/ready"},"members":[]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if l := old.Listeners; l[0].Protocol != "http" || l[1].Protocol != "tcp" || old.Pools[0].Protocol != "http" || old.Pools[1].ProxyVersion != 0 {
		t.Fatalf("a body from before v0.11.1 read as %+v", old)
	}
	got, err := lbSpecOfRequest(body(`{"listeners":[{"port":443,"protocol":"tcp","pool":"web","connectionLimit":2000,"timeoutClientData":50000}],
		"pools":[{"name":"web","algorithm":"source","proxyProtocol":true,"proxyVersion":1,"timeoutMemberConnect":5000,"timeoutMemberData":50000,
		  "sessionPersistence":{"type":"source_ip","timeout":3600},
		  "healthCheck":{"type":"https","port":8443,"path":"/healthz","interval":5,"timeout":3,"rise":1,"fall":3},
		  "members":[{"serverId":"101","port":8443,"weight":2}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := pve.LBSpec{
		Listeners: []pve.LBListener{{Port: 443, Protocol: "tcp", Pool: "web", ConnectionLimit: 2000, TimeoutClientData: 50000}},
		Pools: []pve.LBPool{{Name: "web", Protocol: "tcp", Algorithm: "source", ProxyProtocol: true, ProxyVersion: 1, TimeoutMemberConnect: 5000, TimeoutMemberData: 50000,
			SessionPersistence: &pve.LBPersistence{Type: "source_ip", Timeout: 3600},
			HealthCheck:        &pve.LBHealthCheck{Type: "https", Port: 8443, Path: "/healthz", Interval: 5, Timeout: 3, Rise: 1, Fall: 3},
			Members:            []pve.LBMember{{ServerID: "101", Port: 8443, Weight: 2}}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("spec %+v\nwant %+v", got, want)
	}
	// The row holds what was asked, under the names of the request.
	row, err := json.Marshal(loadBalancer{ID: "a", LBSpec: got})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"protocol":"tcp"`, `"connectionLimit":2000`, `"timeoutClientData":50000`, `"proxyVersion":1`, `"timeoutMemberConnect":5000`,
		`"timeoutMemberData":50000`, `"sessionPersistence":{"type":"source_ip","timeout":3600}`, `"interval":5,"timeout":3,"rise":1,"fall":3`} {
		if !strings.Contains(string(row), key) {
			t.Errorf("the row lacks %s: %s", key, row)
		}
	}
	// A tenant names certificates by id; the files on an appliance are the platform's to name.
	tlsSpec, err := lbSpecOfRequest(body(`{"listeners":[{"port":443,"protocol":"https","pool":"web","certificates":["c1","c2"],"certFiles":["x"]}],"pools":[{"name":"web","members":[]}]}`))
	if err != nil || !reflect.DeepEqual(tlsSpec.Listeners[0].Certificates, []string{"c1", "c2"}) || tlsSpec.Listeners[0].CertFiles != nil || tlsSpec.Pools[0].Protocol != "http" {
		t.Errorf("an https listener: %+v %v", tlsSpec, err)
	}
	for name, c := range map[string]struct{ body, say string }{
		"udp over tcp":     {`{"listeners":[{"port":53,"protocol":"udp","pool":"dns"}],"pools":[{"name":"dns","protocol":"tcp","members":[]}]}`, "a udp listener needs a udp pool"},
		"udp with PROXY":   {`{"listeners":[{"port":53,"protocol":"udp","pool":"dns"}],"pools":[{"name":"dns","proxyProtocol":true,"members":[]}]}`, "UDP has no PROXY protocol"},
		"tls, no cert":     {`{"listeners":[{"port":443,"protocol":"tls","pool":"web"}],"pools":[{"name":"web","members":[]}]}`, "a tls listener needs a certificate"},
		"https over tcp":   {`{"listeners":[{"port":443,"protocol":"https","pool":"web","certificates":["c1"]}],"pools":[{"name":"web","protocol":"tcp","members":[]}]}`, "an https listener needs an http pool"},
		"a file named":     {`{"listeners":[{"port":443,"protocol":"https","pool":"web","certificates":["../c1"]}],"pools":[{"name":"web","members":[]}]}`, "a certificate is named by its id"},
		"mismatch":         {`{"listeners":[{"port":80,"protocol":"http","pool":"db"}],"pools":[{"name":"db","protocol":"tcp","members":[]}]}`, "an http listener needs an http pool"},
		"a limit in words": {`{"listeners":[{"port":80,"pool":"web","connectionLimit":"many"}],"pools":[{"name":"web","members":[]}]}`, "expected shape"},
		"a limit too high": {`{"listeners":[{"port":80,"pool":"web","connectionLimit":20001}],"pools":[{"name":"web","members":[]}]}`, "connection limit must be 1 to 20000"},
	} {
		_, err := lbSpecOfRequest(body(c.body))
		var he *httpx.HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusBadRequest || !strings.Contains(he.Msg, c.say) {
			t.Errorf("%s: %v, want a 400 that says %q", name, err, c.say)
		}
	}
}

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

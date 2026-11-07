package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestNicFirewallParams(t *testing.T) {
	cfg := VMConfig{
		"net0":     "virtio=AA:BB:CC:DD:EE:01,bridge=vaaa11",
		"net1":     "virtio=AA:BB:CC:DD:EE:02,bridge=vmbr0,firewall=0",
		"net2":     "virtio=AA:BB:CC:DD:EE:03,bridge=vmbr1,firewall=1",
		"netfoo":   "bridge=x",
		"scsi0":    "local:vm-1-disk-0",
		"memory":   2048,
		"ipconfig": "ip=dhcp",
	}
	want := map[string]string{
		"net0": "virtio=AA:BB:CC:DD:EE:01,bridge=vaaa11,firewall=1",
		"net1": "virtio=AA:BB:CC:DD:EE:02,bridge=vmbr0,firewall=1",
	}
	if got := nicFirewallParams(cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if got := nicBridge("virtio=AA,bridge=vaaa11,firewall=1"); got != "vaaa11" {
		t.Fatalf("nicBridge = %q", got)
	}
}

// TestExtraNICKeys: a clone keeps only net0; every other NIC (and its ipconfig)
// is deleted so no inherited public NIC/ipfilter survives into a new server.
func TestExtraNICKeys(t *testing.T) {
	cfg := VMConfig{
		"net0":      "virtio,bridge=vaaa11",
		"net1":      "virtio,bridge=vmbr1,firewall=1",
		"ipconfig1": "ip=203.0.113.5/24,gw=203.0.113.1",
		"net3":      "virtio,bridge=vmbr0",
		"ipconfig0": "ip=dhcp",
		"netfoo":    "bridge=x",
	}
	if got, want := extraNICKeys(cfg), []string{"ipconfig1", "net1", "net3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := extraNICKeys(VMConfig{"net0": "virtio,bridge=v"}); len(got) != 0 {
		t.Fatalf("net0 only: got %v", got)
	}
}

func TestDatacenterFirewallEnabled(t *testing.T) {
	for body, want := range map[string]bool{
		`{"data":{"enable":1,"policy_in":"DROP"}}`: true,
		`{"data":{"enable":0}}`:                    false,
		`{"data":{}}`:                              false, // never set = PVE default off
	} {
		_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
			"GET /api2/json/cluster/firewall/options": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) },
		})
		got, err := c.DatacenterFirewallEnabled(context.Background())
		if err != nil || got != want {
			t.Fatalf("%s: got %v, %v; want %v", body, got, err, want)
		}
	}
}

// vmFirewallFake serves vm 101 (net0 on vnet vaaa11, net1 on a public bridge)
// with the given ipfilter-net0 entries ("" = no set), recording firewall writes.
func vmFirewallFake(t *testing.T, existing string) (*Service, *[]string) {
	var calls []string
	const vm = "/api2/json/nodes/pve1/qemu/101/firewall"
	rec := func(prefix string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			r.ParseForm()
			calls = append(calls, prefix+" "+r.Form.Encode())
			fmt.Fprint(w, `{"data":null}`)
		}
	}
	sets := `{"data":[]}`
	if existing != "" {
		sets = `{"data":[{"name":"ipfilter-net0"},{"name":"other"}]}`
	}
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/nodes/pve1/qemu/101/config": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":{"net0":"virtio=AA,bridge=vaaa11,firewall=1","net1":"virtio=BB,bridge=vmbr9,firewall=1"}}`)
		},
		"GET /api2/json/cluster/sdn/vnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"vnet":"vaaa11","zone":"overlay","alias":"st:p1:web"}]}`)
		},
		"GET /api2/json/cluster/sdn/zones": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"zone":"overlay","type":"evpn"}]}`)
		},
		"GET /api2/json/cluster/sdn/vnets/vaaa11/subnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"cidr":"10.100.0.0/24"},{"cidr":"10.100.9.0/24"}]}`)
		},
		"GET " + vm + "/ipset":                  func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, sets) },
		"GET " + vm + "/ipset/ipfilter-net0":    func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, existing) },
		"POST " + vm + "/ipset":                 rec("create"),
		"POST " + vm + "/ipset/ipfilter-net0":   rec("add"),
		"DELETE " + vm + "/ipset/ipfilter-net0": rec("drop"),
		"PUT " + vm + "/options":                rec("options"),
	})
	return &Service{C: c, PoolPrefix: "tp-"}, &calls
}

func TestSecureVM(t *testing.T) {
	tests := []struct {
		name     string
		existing string
		want     []string
	}{
		{"fresh VM: vnet NIC gets its subnets, public NIC none", "", []string{
			"create comment=proxcloud+anti-spoof&name=ipfilter-net0",
			"add cidr=10.100.0.0%2F24",
			"add cidr=10.100.9.0%2F24",
			"options dhcp=1&enable=1&ipfilter=1&macfilter=1&policy_in=DROP&policy_out=ACCEPT",
		}},
		{"set already right: options only", `{"data":[{"cidr":"10.100.9.0/24"},{"cidr":"10.100.0.0/24"}]}`, []string{
			"options dhcp=1&enable=1&ipfilter=1&macfilter=1&policy_in=DROP&policy_out=ACCEPT",
		}},
		{"stale set is rebuilt", `{"data":[{"cidr":"10.100.0.0/24"},{"cidr":"10.200.0.5/32"}]}`, []string{
			"drop force=1",
			"create comment=proxcloud+anti-spoof&name=ipfilter-net0",
			"add cidr=10.100.0.0%2F24",
			"add cidr=10.100.9.0%2F24",
			"options dhcp=1&enable=1&ipfilter=1&macfilter=1&policy_in=DROP&policy_out=ACCEPT",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, calls := vmFirewallFake(t, tt.existing)
			if err := s.SecureVM(context.Background(), "pve1", 101); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*calls, tt.want) {
				t.Fatalf("calls:\n%v\nwant:\n%v", *calls, tt.want)
			}
		})
	}
}

// groupFake serves the cluster groups and records group/rule creates.
func groupFake(t *testing.T, groups string) (*Service, *[]string) {
	var calls []string
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/firewall/groups": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, groups) },
		"POST /api2/json/cluster/firewall/groups": func(w http.ResponseWriter, r *http.Request) {
			r.ParseForm()
			calls = append(calls, "group "+r.PostFormValue("comment"))
			fmt.Fprint(w, `{"data":null}`)
		},
	})
	return &Service{C: c, PoolPrefix: "tp-"}, &calls
}

func TestEnsureDefaultSecurityGroupExisting(t *testing.T) {
	s, calls := groupFake(t, `{"data":[{"group":"gother","comment":"st:p2:default"},{"group":"gdef1","comment":"st:p1:default"}]}`)
	id, err := s.EnsureDefaultSecurityGroup(context.Background(), "p1", "10.100.0.0/16")
	if err != nil || id != "gdef1" {
		t.Fatalf("id = %q, %v", id, err)
	}
	if len(*calls) != 0 {
		t.Fatalf("an existing default must not be rewritten: %v", *calls)
	}
}

func TestEnsureDefaultSecurityGroupCreates(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch {
		case r.Method == "GET" && r.URL.Path == "/api2/json/cluster/firewall/groups":
			fmt.Fprint(w, `{"data":[{"group":"gweb1","comment":"st:p1:web"},{"group":"gdef9","comment":"st:p2:default"}]}`)
			return
		case r.Method == "POST" && r.URL.Path == "/api2/json/cluster/firewall/groups":
			calls = append(calls, "group "+r.PostFormValue("comment"))
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api2/json/cluster/firewall/groups/g"):
			calls = append(calls, "rule "+r.PostFormValue("type")+" "+r.PostFormValue("action")+" src="+r.PostFormValue("source"))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
		fmt.Fprint(w, `{"data":null}`)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{APIURL: srv.URL, TokenID: "proxcloud@pve!t1", Secret: "sec"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{C: c, PoolPrefix: "tp-"}
	id, err := s.EnsureDefaultSecurityGroup(context.Background(), "p1", "10.100.0.0/16")
	if err != nil || len(id) != 8 || id[0] != 'g' {
		t.Fatalf("id = %q, %v", id, err)
	}
	want := []string{"group st:p1:default", "rule in ACCEPT src=10.100.0.0/16"}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls = %v; want %v", calls, want)
	}
}

func TestDefaultGroupRules(t *testing.T) {
	got := defaultGroupRules("10.100.0.0/16")
	if len(got) != 1 || got[0].Type != "in" || got[0].Action != "ACCEPT" || got[0].Source != "10.100.0.0/16" || got[0].DPort != "" {
		t.Fatalf("rules = %+v", got)
	}
	if got := defaultGroupRules(""); got != nil {
		t.Fatalf("no pool must never emit an any-source ACCEPT: %+v", got)
	}
}

func TestServerSecurityGroups(t *testing.T) {
	const groups = `{"data":[
		{"group":"gweb1","comment":"st:p1:web"},
		{"group":"gdb01","comment":"st:p1:db"},
		{"group":"gfor1","comment":"st:p2:admin"},
		{"group":"gunm1","comment":"hand-made"}
	]}`
	tests := []struct {
		name     string
		refs     []string
		want     []string
		notFound string
	}{
		{"by name", []string{"web"}, []string{"gweb1"}, ""},
		{"by id, deduped", []string{"gdb01", "db", "web"}, []string{"gdb01", "gweb1"}, ""},
		{"another project's group", []string{"web", "admin"}, nil, "admin"},
		{"another project's id", []string{"gfor1"}, nil, "gfor1"},
		{"unmanaged group", []string{"gunm1"}, nil, "gunm1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := groupFake(t, groups)
			got, err := s.ServerSecurityGroups(context.Background(), "p1", tt.refs, "10.100.0.0/16")
			if tt.notFound != "" {
				var nf *SecurityGroupNotFoundError
				if !errors.As(err, &nf) || nf.Ref != tt.notFound {
					t.Fatalf("err = %v; want not-found %q", err, tt.notFound)
				}
				return
			}
			if err != nil || !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

// TestDeleteProjectSecurityGroups: teardown empties and drops only the
// project's own groups.
func TestDeleteProjectSecurityGroups(t *testing.T) {
	var calls []string
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/firewall/groups": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"group":"gaaa","comment":"st:p1:default"},{"group":"gbbb","comment":"st:p2:default"},{"group":"ops","comment":"admin"}]}`)
		},
		"GET /api2/json/cluster/firewall/groups/gaaa": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"pos":0,"type":"in","action":"ACCEPT"}]}`)
		},
		"DELETE /api2/json/cluster/firewall/groups/gaaa/0": func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, "del rule gaaa 0")
			fmt.Fprint(w, `{"data":null}`)
		},
		"DELETE /api2/json/cluster/firewall/groups/gaaa": func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, "del group gaaa")
			fmt.Fprint(w, `{"data":null}`)
		},
	})
	srv := &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}
	if err := srv.DeleteProjectSecurityGroups(context.Background(), "p1"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"del rule gaaa 0", "del group gaaa"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

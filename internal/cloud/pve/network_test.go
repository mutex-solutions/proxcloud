package pve

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestOwnershipAlias(t *testing.T) {
	a := OwnershipAlias("p1", "web-net")
	pid, name, ok := aliasProject(a)
	if !ok || pid != "p1" || name != "web-net" {
		t.Fatalf("round trip: %v %v %v", pid, name, ok)
	}
	if _, _, ok := aliasProject("vmbr0"); ok {
		t.Fatal("foreign alias must not parse")
	}
	if _, _, ok := aliasProject("st:broken"); ok {
		t.Fatal("malformed alias must not parse")
	}
}

func TestFirstHostAndDHCPRange(t *testing.T) {
	_, ipnet, _ := net.ParseCIDR("10.100.5.0/24")
	if gw := firstHost(ipnet); gw != "10.100.5.1" {
		t.Fatalf("gw = %s", gw)
	}
	start, end := dhcpRange(ipnet, "10.100.5.1")
	if start != "10.100.5.10" || end != "10.100.5.254" {
		t.Fatalf("range = %s..%s", start, end)
	}
}

func TestCreateNetworkStagesVnetAndSubnet(t *testing.T) {
	var gotVnet map[string]string
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/sdn/vnets": func(w http.ResponseWriter, r *http.Request) {
			// The VNI max spans zones: a per-project EVPN zone's vnet counts too.
			fmt.Fprint(w, `{"data":[{"vnet":"vabc12","zone":"overlay","tag":100005,"alias":"st:p2:other"},{"vnet":"vdef34","zone":"pzzzzzz1","tag":100009,"alias":"st:p3:x"}]}`)
		},
		"POST /api2/json/cluster/sdn/vnets": func(w http.ResponseWriter, r *http.Request) {
			r.ParseForm()
			gotVnet = map[string]string{}
			for k := range r.PostForm {
				gotVnet[k] = r.PostFormValue(k)
			}
			fmt.Fprint(w, `{"data":null}`)
		},
	})
	srv := &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}
	// Register subnet handler via a wrapping fake is awkward — instead assert via
	// the vnet form and let subnet POST 500 (fake has no route) to exercise rollback.
	_, err := srv.CreateNetwork(context.Background(), CreateNetworkReq{
		ProjectID: "p1", Name: "web", Zone: ZoneConfig{Key: "overlay", Kind: "evpn"},
		CIDR: "10.100.0.0/24", DHCP: true, SNAT: true,
	})
	if err == nil {
		t.Fatal("expected subnet-create failure from the fake")
	}
	if gotVnet["zone"] != "overlay" || gotVnet["alias"] != "st:p1:web" {
		t.Fatalf("vnet form = %v", gotVnet)
	}
	if gotVnet["tag"] != "100010" { // cluster-wide maxTag 100009 + 1
		t.Fatalf("tag = %v", gotVnet["tag"])
	}
}

func TestNetworkSyncProviderFiltersByAlias(t *testing.T) {
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/sdn/vnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[
				{"vnet":"vaaa11","zone":"overlay","alias":"st:p1:web"},
				{"vnet":"vbbb22","zone":"overlay","alias":"st:p2:other"},
				{"vnet":"vmgmt","zone":"overlay","alias":"ops managed"}
			]}`)
		},
		"GET /api2/json/cluster/sdn/vnets/vaaa11/subnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"subnet":"overlay-10.100.0.0-24","cidr":"10.100.0.0/24","gateway":"10.100.0.1","snat":1,"dhcp-range":[{"start-address":"10.100.0.10","end-address":"10.100.0.254"}]}]}`)
		},
	})
	srv := &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}
	rs, err := srv.NetworkSyncProvider("pve", "p1").List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].ExternalID != "vaaa11" {
		t.Fatalf("rows = %+v", rs)
	}
	netMap := rs[0].Data["network"].(map[string]any)
	if netMap["name"] != "web" || netMap["cidr"] != "10.100.0.0/24" || netMap["snat"] != true || netMap["dhcp"] != true {
		t.Fatalf("network = %v", netMap)
	}
}

func TestEnsureZoneIdempotent(t *testing.T) {
	created := 0
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/sdn/zones": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"zone":"overlay","type":"evpn"}]}`)
		},
		"POST /api2/json/cluster/sdn/zones": func(w http.ResponseWriter, r *http.Request) {
			created++
			fmt.Fprint(w, `{"data":null}`)
		},
	})
	srv := &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}
	if err := srv.EnsureZone(context.Background(), ZoneConfig{Key: "overlay", Kind: "evpn"}); err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatal("existing zone must not be re-created")
	}
}

func TestZoneIDFor(t *testing.T) {
	re := regexp.MustCompile(`^p[0-9a-z]{7}$`)
	seen := map[string]string{}
	for _, pid := range []string{"6a46b1464d44db697643c5f7", "6a46b1464d44db697643c5f8", "p1", ""} {
		id := ZoneIDFor(pid)
		if !re.MatchString(id) {
			t.Fatalf("ZoneIDFor(%q) = %q, want p+7 base36 (8 chars)", pid, id)
		}
		if ZoneIDFor(pid) != id {
			t.Fatalf("ZoneIDFor(%q) not deterministic", pid)
		}
		if prev, dup := seen[id]; dup {
			t.Fatalf("ZoneIDFor collision %q/%q -> %s", prev, pid, id)
		}
		seen[id] = pid
	}
}

func TestIsolationRules(t *testing.T) {
	tests := []struct {
		name  string
		cidrs []string
		want  []FirewallRule
	}{
		{"allow own then drop pool", []string{"10.100.0.0/24", "10.100.3.0/24"}, []FirewallRule{
			{Type: "forward", Action: "ACCEPT", Source: "10.100.0.0/24,10.100.3.0/24", Dest: "10.100.0.0/24,10.100.3.0/24", Comment: isolationComment},
			{Type: "forward", Action: "DROP", Dest: "10.100.0.0/16", Comment: isolationComment},
		}},
		{"no subnets never emits an any-ACCEPT", nil, []FirewallRule{
			{Type: "forward", Action: "DROP", Dest: "10.100.0.0/16", Comment: isolationComment},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isolationRules(tt.cidrs, "10.100.0.0/16"); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// isolationFake serves two vnets of p1 (+ one of p2) and records firewall writes.
func isolationFake(t *testing.T, existing string) (*Service, *[]string) {
	var calls []string
	form := func(r *http.Request) string {
		r.ParseForm()
		return r.PostFormValue("action") + " src=" + r.PostFormValue("source") + " dst=" + r.PostFormValue("dest")
	}
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/sdn/vnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[
				{"vnet":"vaaa11","zone":"overlay","alias":"st:p1:web"},
				{"vnet":"vbbb22","zone":"overlay","alias":"st:p1:db"},
				{"vnet":"vccc33","zone":"overlay","alias":"st:p2:other"}
			]}`)
		},
		"GET /api2/json/cluster/sdn/vnets/vaaa11/subnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"cidr":"10.100.0.0/24"}]}`)
		},
		"GET /api2/json/cluster/sdn/vnets/vbbb22/subnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"cidr":"10.100.1.0/24"}]}`)
		},
		"GET /api2/json/cluster/sdn/vnets/vaaa11/firewall/rules": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, existing)
		},
		"GET /api2/json/cluster/sdn/vnets/vbbb22/firewall/rules": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, existing)
		},
		"POST /api2/json/cluster/sdn/vnets/vaaa11/firewall/rules": func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, "add vaaa11 "+form(r))
			fmt.Fprint(w, `{"data":null}`)
		},
		"POST /api2/json/cluster/sdn/vnets/vbbb22/firewall/rules": func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, "add vbbb22 "+form(r))
			fmt.Fprint(w, `{"data":null}`)
		},
		"DELETE /api2/json/cluster/sdn/vnets/vaaa11/firewall/rules/0": func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, "del vaaa11 0")
			fmt.Fprint(w, `{"data":null}`)
		},
		"DELETE /api2/json/cluster/sdn/vnets/vbbb22/firewall/rules/0": func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, "del vbbb22 0")
			fmt.Fprint(w, `{"data":null}`)
		},
		"PUT /api2/json/cluster/sdn/vnets/vaaa11/firewall/options": func(w http.ResponseWriter, r *http.Request) {
			r.ParseForm()
			calls = append(calls, "enable vaaa11 "+r.PostFormValue("enable"))
			fmt.Fprint(w, `{"data":null}`)
		},
		"PUT /api2/json/cluster/sdn/vnets/vbbb22/firewall/options": func(w http.ResponseWriter, r *http.Request) {
			r.ParseForm()
			calls = append(calls, "enable vbbb22 "+r.PostFormValue("enable"))
			fmt.Fprint(w, `{"data":null}`)
		},
	})
	return &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}, &calls
}

func TestReconcileIsolationRewritesStaleRules(t *testing.T) {
	// A stale managed rule at pos 0 (old allow-list) + an operator rule at pos 1.
	srv, calls := isolationFake(t, `{"data":[
		{"pos":0,"type":"forward","action":"ACCEPT","source":"10.100.0.0/24","dest":"10.100.0.0/24","comment":"proxcloud:isolation"},
		{"pos":1,"type":"forward","action":"ACCEPT","dest":"10.9.9.9","comment":"ops"}
	]}`)
	if err := srv.ReconcileIsolation(context.Background(), "p1", "10.100.0.0/16"); err != nil {
		t.Fatal(err)
	}
	own := "10.100.0.0/24,10.100.1.0/24"
	var want []string
	for _, v := range []string{"vaaa11", "vbbb22"} {
		want = append(want,
			"del "+v+" 0", // only the managed rule; the operator rule stays
			// bottom-up: PVE prepends, so the ACCEPT lands on top
			"add "+v+" DROP src= dst=10.100.0.0/16",
			"add "+v+" ACCEPT src="+own+" dst="+own,
			"enable "+v+" 1",
		)
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(*calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestReconcileIsolationIdempotent(t *testing.T) {
	srv, calls := isolationFake(t, `{"data":[
		{"pos":0,"type":"forward","action":"ACCEPT","source":"10.100.0.0/24,10.100.1.0/24","dest":"10.100.0.0/24,10.100.1.0/24","comment":"proxcloud:isolation"},
		{"pos":1,"type":"forward","action":"DROP","dest":"10.100.0.0/16","comment":"proxcloud:isolation"}
	]}`)
	if err := srv.ReconcileIsolation(context.Background(), "p1", "10.100.0.0/16"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"enable vaaa11 1", "enable vbbb22 1"}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls = %v, want only %v", *calls, want)
	}
}

func TestEnsureProjectZonePayload(t *testing.T) {
	var got map[string]string
	ctrlCreated := false
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/sdn/zones": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"zone":"overlay","type":"evpn","vrf-vxlan":10000},{"zone":"pother01","type":"evpn","vrf-vxlan":10007}]}`)
		},
		"GET /api2/json/cluster/sdn/controllers": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"controller":"ctl","type":"evpn"}]}`)
		},
		"POST /api2/json/cluster/sdn/controllers": func(w http.ResponseWriter, r *http.Request) {
			ctrlCreated = true
			fmt.Fprint(w, `{"data":null}`)
		},
		"POST /api2/json/cluster/sdn/zones": func(w http.ResponseWriter, r *http.Request) {
			r.ParseForm()
			got = map[string]string{}
			for k := range r.PostForm {
				got[k] = r.PostFormValue(k)
			}
			fmt.Fprint(w, `{"data":null}`)
		},
	})
	srv := &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}
	tmpl := ZoneConfig{Key: "overlay", Kind: "evpn", Controller: "tmplctl", ExitNodes: []string{"pve1"}, MTU: 1450}
	z, err := srv.EnsureProjectZone(context.Background(), "p1", tmpl, "ctl")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"zone": ZoneIDFor("p1"), "type": "evpn", "controller": "ctl", "vrf-vxlan": "10008",
		"mtu": "1450", "exitnodes": "pve1", "ipam": "pve", "dhcp": "dnsmasq",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("zone form = %v\nwant %v", got, want)
	}
	if z.Key != ZoneIDFor("p1") || ctrlCreated {
		t.Fatalf("zone = %+v, ctrlCreated = %v", z, ctrlCreated)
	}
}

func TestEnsureProjectZoneCollision(t *testing.T) {
	id := ZoneIDFor("p1")
	tests := []struct{ name, zones, vnets string }{
		{"non-evpn zone with our id", `[{"zone":"` + id + `","type":"vlan"}]`, `[]`},
		{"another owner's vnet in it", `[{"zone":"` + id + `","type":"evpn"}]`, `[{"vnet":"vx","zone":"` + id + `","alias":"st:p2:web"}]`},
		{"foreign vnet in it", `[{"zone":"` + id + `","type":"evpn"}]`, `[{"vnet":"vx","zone":"` + id + `","alias":"ops"}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
				"GET /api2/json/cluster/sdn/zones": func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, `{"data":`+tt.zones+`}`)
				},
				"GET /api2/json/cluster/sdn/vnets": func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, `{"data":`+tt.vnets+`}`)
				},
			})
			srv := &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}
			if _, err := srv.EnsureProjectZone(context.Background(), "p1", ZoneConfig{Key: "overlay", Kind: "evpn"}, "ctl"); err == nil || !strings.Contains(err.Error(), "collides") {
				t.Fatalf("err = %v, want a collision", err)
			}
		})
	}
}

func TestDeleteProjectZoneIfEmpty(t *testing.T) {
	id := ZoneIDFor("p1")
	for _, tt := range []struct {
		name, vnets string
		wantGone    bool
	}{
		{"last vnet gone", `[{"vnet":"vx","zone":"overlay","alias":"st:p2:x"}]`, true},
		{"vnet still there", `[{"vnet":"vx","zone":"` + id + `","alias":"st:p1:web"}]`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			deleted := false
			_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
				"GET /api2/json/cluster/sdn/vnets": func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, `{"data":`+tt.vnets+`}`)
				},
				"GET /api2/json/cluster/sdn/zones": func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, `{"data":[{"zone":"`+id+`","type":"evpn"}]}`)
				},
				"DELETE /api2/json/cluster/sdn/zones/" + id: func(w http.ResponseWriter, r *http.Request) {
					deleted = true
					fmt.Fprint(w, `{"data":null}`)
				},
			})
			srv := &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}
			gone, err := srv.DeleteProjectZoneIfEmpty(context.Background(), "p1")
			if err != nil || gone != tt.wantGone || deleted != tt.wantGone {
				t.Fatalf("gone=%v deleted=%v err=%v, want %v", gone, deleted, err, tt.wantGone)
			}
		})
	}
}

// TestTeardownProjectNetworks: teardown stages the delete of every vnet the
// project owns (isolation rules first, then subnet, then vnet) and never touches
// another project's or an operator's vnet.
func TestTeardownProjectNetworks(t *testing.T) {
	var calls []string
	rec := func(s string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, s)
			fmt.Fprint(w, `{"data":null}`)
		}
	}
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/sdn/vnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[
				{"vnet":"vaaa11","zone":"overlay","alias":"st:p1:web"},
				{"vnet":"vccc33","zone":"overlay","alias":"st:p2:other"},
				{"vnet":"opsnet","zone":"overlay","alias":"ops"}
			]}`)
		},
		"GET /api2/json/cluster/sdn/vnets/vaaa11/firewall/rules": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"pos":0,"type":"forward","action":"DROP","dest":"10.100.0.0/16","comment":"proxcloud:isolation"}]}`)
		},
		"DELETE /api2/json/cluster/sdn/vnets/vaaa11/firewall/rules/0": rec("del rule vaaa11 0"),
		"GET /api2/json/cluster/sdn/vnets/vaaa11/subnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"subnet":"overlay-10.100.0.0-24","cidr":"10.100.0.0/24"}]}`)
		},
		"DELETE /api2/json/cluster/sdn/vnets/vaaa11/subnets/overlay-10.100.0.0-24": rec("del subnet vaaa11"),
		"DELETE /api2/json/cluster/sdn/vnets/vaaa11":                               rec("del vnet vaaa11"),
	})
	srv := &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}
	staged, err := srv.TeardownProjectNetworks(context.Background(), "p1")
	if err != nil || !staged {
		t.Fatalf("staged=%v err=%v", staged, err)
	}
	if want := []string{"del rule vaaa11 0", "del subnet vaaa11", "del vnet vaaa11"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	if staged, err := srv.TeardownProjectNetworks(context.Background(), "p9"); err != nil || staged {
		t.Fatalf("no vnets: staged=%v err=%v, want nothing staged", staged, err)
	}
}

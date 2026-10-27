package pve

import (
	"context"
	"fmt"
	"net"
	"net/http"
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
			fmt.Fprint(w, `{"data":[{"vnet":"vabc12","zone":"overlay","tag":100005,"alias":"st:p2:other"}]}`)
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
	if gotVnet["tag"] != "100006" { // maxTag 100005 + 1
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

package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

func inventoryNetwork(id, owner, zone string, cidrs ...string) cloud.CloudResource {
	subs := make([]SDNSubnet, len(cidrs))
	for i, cidr := range cidrs {
		subs[i].CIDR = cidr
	}
	return cloud.CloudResource{ExternalID: id, ProjectID: owner, Type: cloud.TypeNetwork,
		Data: NetworkData(id, "net", zone, SubnetDetail(subs))}
}

// Every response goes through the real PVE transport and configured credential.
// A 200 empty list plus a masked 404 is still unknown without effective rights.
func TestNetworkInventoryVisibilityEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, privilege, liveOwner string
		getStatus                  int
		masked, missingZone        bool
		wantOK                     bool
	}{
		{name: "lost rights and explicit forbidden", getStatus: 403},
		{name: "lost rights and masked not found", getStatus: 404, masked: true},
		{name: "namespace rights do not override vnet NoAccess", privilege: "SDN.Audit", getStatus: 404, masked: true},
		{name: "same owner omitted by partial list", privilege: "SDN.Audit", liveOwner: "p1", getStatus: 200},
		{name: "forbidden despite baseline", privilege: "SDN.Audit", getStatus: 403},
		{name: "gone with audit proof", privilege: "SDN.Audit", getStatus: 404, wantOK: true},
		{name: "gone with allocate proof", privilege: "SDN.Allocate", getStatus: 404, wantOK: true},
		{name: "positively foreign now", privilege: "SDN.Audit", liveOwner: "p2", getStatus: 200, wantOK: true},
		{name: "unknown owner", privilege: "SDN.Audit", getStatus: 200},
		{name: "legacy missing zone cannot prove", privilege: "SDN.Audit", getStatus: 404, missingZone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			byID := 0
			_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
				"GET /api2/json/cluster/sdn/vnets": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":[]}`) },
				"GET /api2/json/access/acl":        func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":[]}`) },
				"GET /api2/json/access/permissions": func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "PVEAPIToken=proxcloud@pve!t1=sec" {
						t.Errorf("wrong credential: %q", r.Header.Get("Authorization"))
					}
					if r.URL.Query().Get("userid") != "" {
						t.Error("must ask for self effective permissions, not a user's rights")
					}
					path := r.URL.Query().Get("path")
					if path == "/access" {
						fmt.Fprint(w, `{"data":{"/access":{"Sys.Audit":0}}}`)
						return
					}
					if tc.privilege == "" || tc.masked && path == "/sdn/zones/overlay/v1" {
						fmt.Fprintf(w, `{"data":{%q:{}}}`, path)
					} else {
						fmt.Fprintf(w, `{"data":{%q:{%q:1}}}`, path, tc.privilege)
					}
				},
				"GET /api2/json/cluster/sdn/vnets/v1": func(w http.ResponseWriter, r *http.Request) {
					byID++
					w.WriteHeader(tc.getStatus)
					fmt.Fprintf(w, `{"data":{"vnet":"v1","zone":"overlay","alias":%q}}`, OwnershipAlias(tc.liveOwner, "net"))
				},
			})
			p := (&Service{C: c}).NetworkSyncProvider("r", "p1")
			list, err := p.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			zone := "overlay"
			if tc.missingZone {
				zone = ""
			}
			err = p.VerifyInventory(context.Background(), []cloud.CloudResource{inventoryNetwork("v1", "p1", zone, "10.0.0.0/24")}, list)
			var untrusted *cloud.InventoryUntrustedError
			if tc.wantOK && err != nil || !tc.wantOK && !errors.As(err, &untrusted) {
				t.Fatalf("verify = %v, wantOK=%v", err, tc.wantOK)
			}
			if tc.masked && byID != 0 {
				t.Fatal("404 attempted before permissions proof")
			}
		})
	}
}

func TestNetworkInventorySubnetLoss(t *testing.T) {
	for _, status := range []int{200, 403, 404, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
				"GET /api2/json/access/acl": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":[]}`) },
				"GET /api2/json/access/permissions": func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Query().Get("path") == "/access" {
						fmt.Fprint(w, `{"data":{"/access":{"Sys.Audit":0}}}`)
						return
					}
					fmt.Fprintf(w, `{"data":{%q:{"SDN.Audit":1}}}`, r.URL.Query().Get("path"))
				},
				"GET /api2/json/cluster/sdn/vnets/v1": func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, `{"data":{"vnet":"v1","zone":"overlay","alias":"st:p1:net"}}`)
				},
				"GET /api2/json/cluster/sdn/vnets/v1/subnets/overlay-10.0.1.0-24": func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(status)
					fmt.Fprint(w, `{"data":{"cidr":"10.0.1.0/24"}}`)
				},
			})
			p := (&Service{C: c}).NetworkSyncProvider("r", "p1")
			old := inventoryNetwork("v1", "p1", "overlay", "10.0.0.0/24", "10.0.1.0/24")
			cur := inventoryNetwork("v1", "p1", "overlay", "10.0.0.0/24")
			err := p.VerifyInventory(context.Background(), []cloud.CloudResource{old}, []cloud.CloudResource{cur})
			if (err == nil) != (status == 404) {
				t.Fatalf("verify = %v", err)
			}
		})
	}
}

func TestInventoryAbsenceRequiresExactObjectEvidence(t *testing.T) {
	for _, tc := range []struct {
		status int
		errors map[string]string
		want   bool
	}{
		{404, nil, true},
		{400, map[string]string{"vnet": "could not find vnet 'v1'"}, true},
		{400, map[string]string{"vnet": "could not find vnet 'other'"}, false},
		{400, map[string]string{"zone": "could not find zone 'overlay'"}, false},
		{500, map[string]string{"vnet": "sdn vnet 'v1' does not exist"}, false},
		{500, map[string]string{"zone": "sdn zone 'overlay' does not exist"}, false},
		{403, nil, false},
	} {
		if got := inventoryObjectAbsent(&APIError{Status: tc.status, Errors: tc.errors}, "vnet", "v1"); got != tc.want {
			t.Fatalf("%+v => %v", tc, got)
		}
	}
}

func TestNetworkListDoesNotSwallowSubnetReadFailure(t *testing.T) {
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/sdn/vnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"vnet":"v1","zone":"overlay","alias":"st:p1:net"}]}`)
		},
		"GET /api2/json/cluster/sdn/vnets/v1/subnets": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) },
	})
	_, err := (&Service{C: c}).NetworkSyncProvider("r", "p1").List(context.Background())
	var untrusted *cloud.InventoryUntrustedError
	if !errors.As(err, &untrusted) {
		t.Fatalf("list must fail closed, got %v", err)
	}
}

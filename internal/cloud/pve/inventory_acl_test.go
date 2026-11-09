package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

func TestMaskedSDNMissingNeedsCompleteStableVisibility(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		audit, propagate, hidden, mutate bool
		wantOK                           bool
	}{
		{"real PVE400 absent", true, true, false, false, true},
		{"missing ACL audit", false, true, false, false, false},
		{"baseline not propagated", true, false, false, false, false},
		{"moved into hidden zone", true, true, true, false, false},
		{"ACL revoked during get", true, true, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotObject := false
			_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
				"GET /api2/json/access/permissions": func(w http.ResponseWriter, r *http.Request) {
					path := r.URL.Query().Get("path")
					privs := map[string]int{"SDN.Audit": 1}
					if path == "/access" {
						privs = map[string]int{}
						if tc.audit {
							privs["Sys.Audit"] = 0
						}
					}
					if path == "/sdn/zones" && !tc.propagate {
						privs["SDN.Audit"] = 0
					}
					if path == "/sdn/zones/hidden" && (tc.hidden || tc.mutate && gotObject) {
						privs = map[string]int{}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{path: privs}})
				},
				"GET /api2/json/access/acl": func(w http.ResponseWriter, r *http.Request) {
					rows := []ACLEntry{{Path: "/sdn/zones", Type: "token", UGID: "portal", RoleID: "Audit", Propagate: 1}}
					if tc.hidden || tc.mutate && gotObject {
						rows = append(rows, ACLEntry{Path: "/sdn/zones/hidden", Type: "group", UGID: "restricted", RoleID: "NoAccess", Propagate: 1})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": rows})
				},
				"GET /api2/json/cluster/sdn/vnets/v1": func(w http.ResponseWriter, r *http.Request) {
					gotObject = true
					w.WriteHeader(400)
					fmt.Fprint(w, `{"errors":{"vnet":"could not find vnet 'v1'"}}`)
				},
			})
			p := (&Service{C: c}).NetworkSyncProvider("r", "p1")
			err := p.VerifyInventory(context.Background(), []cloud.CloudResource{inventoryNetwork("v1", "p1", "overlay", "10.0.0.0/24")}, nil)
			if (err == nil) != tc.wantOK {
				t.Fatalf("verify = %v", err)
			}
		})
	}
}

func TestSubnetLossAcrossZoneChangeIsUnknown(t *testing.T) {
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/access/permissions": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"data":{%q:{"SDN.Audit":1,"Sys.Audit":0}}}`, r.URL.Query().Get("path"))
		},
		"GET /api2/json/access/acl": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":[]}`) },
		"GET /api2/json/cluster/sdn/vnets/v1": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":{"vnet":"v1","zone":"newzone","alias":"st:p1:net"}}`)
		},
	})
	p := (&Service{C: c}).NetworkSyncProvider("r", "p1")
	err := p.VerifyInventory(context.Background(), []cloud.CloudResource{inventoryNetwork("v1", "p1", "oldzone", "10.0.0.0/24")}, []cloud.CloudResource{inventoryNetwork("v1", "p1", "newzone")})
	if err == nil {
		t.Fatal("old-zone subnet404 must not prove new-zone absence")
	}
}

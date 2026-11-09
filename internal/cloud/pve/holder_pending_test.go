package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestHolderCurrentConfigRejectsPendingAndLock(t *testing.T) {
	for _, mode := range []string{"current", "merged-only", "pending", "lock"} {
		t.Run(mode, func(t *testing.T) {
			_, c := fakePVE(t, fakeRoutes{
				"GET /api2/json/nodes/pve1/qemu/101/config": func(w http.ResponseWriter, r *http.Request) {
					cfg := VMConfig{"smbios1": "uuid=owned"}
					if mode != "merged-only" || r.URL.Query().Get("current") == "0" {
						cfg["scsi1"] = "nas:vm-101-disk-1,size=1G"
					}
					if mode == "lock" {
						cfg["lock"] = "migrate"
					}
					json.NewEncoder(w).Encode(map[string]any{"data": cfg})
				},
				"GET /api2/json/nodes/pve1/qemu/101/pending": func(w http.ResponseWriter, r *http.Request) {
					if mode == "pending" {
						fmt.Fprint(w, `{"data":[{"key":"scsi1","pending":"nas:vm-101-disk-1,size=1G"}]}`)
					} else {
						fmt.Fprint(w, `{"data":[{"key":"smbios1","value":"uuid=owned"}]}`)
					}
				},
			})
			s := &Service{C: c}
			_, e := s.holderCurrentConfig(context.Background(), "pve1", 101)
			if (e == nil) != (mode == "current") {
				t.Fatalf("mode %s error=%v", mode, e)
			}
		})
	}
}
func TestDurableRefusalDoesNotClearProxyTimeout(t *testing.T) {
	for _, status := range []int{408, 499, 502} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			m := newMemHolder()
			s := &Service{HolderOperations: m, HolderBudgets: smallHolderBudgets()}
			ctx, release, e := s.beginHolders(context.Background(), "p")
			if e != nil {
				t.Fatal(e)
			}
			e = s.holderTask(ctx, holderProof{}, time.Second, func(context.Context) (UPID, error) { return "", &APIError{Status: status} })
			release()
			if e == nil || m.pending == nil || m.releases != 0 {
				t.Fatalf("proxy timeout cleared: err=%v pending=%v", e, m.pending)
			}
		})
	}
}
func TestProtectionConfigUnknownStopsRetirementBeforeHAAndDelete(t *testing.T) {
	m := newMemHolder()
	destructive := 0
	routes := fakeRoutes{
		"GET /api2/json/pools/tp-p1":                 say(`{"data":{"members":[{"type":"qemu","node":"pve1","vmid":101}]}}`),
		"GET /api2/json/nodes/pve1/qemu/101/config":  say(`{"data":{"smbios1":"uuid=owned","tags":"pc-protected;proxcloud","scsi0":"nas:vm-101-disk-0,size=8G"}}`),
		"GET /api2/json/nodes/pve1/qemu/101/pending": say(`{"data":[]}`),
		"GET /api2/json/cluster/replication":         say(`{"data":[]}`),
		"GET /api2/json/nodes":                       say(`{"data":[{"node":"pve1","status":"online"}]}`),
		"POST /api2/json/nodes/pve1/qemu/101/config": refuse,
	}
	for _, p := range []string{"DELETE /api2/json/cluster/ha/rules/pc-vm-101", "DELETE /api2/json/cluster/ha/resources/vm:101", "POST /api2/json/nodes/pve1/qemu/101/status/stop", "DELETE /api2/json/nodes/pve1/qemu/101"} {
		routes[p] = func(w http.ResponseWriter, r *http.Request) { destructive++ }
	}
	s := volCluster(t, cfgAttached, map[string][]url.Values{}, routes)
	s.HolderOperations = m
	s.HolderBudgets = smallHolderBudgets()
	if e := s.RetireServer(context.Background(), "p1", "101", "", true); e == nil {
		t.Fatal("uncertain protection tag succeeded")
	}
	if m.pending == nil || destructive != 0 {
		t.Fatalf("pending=%v later destructive=%d", m.pending, destructive)
	}
}
func TestFencedRetirementWithAlreadyAbsentHA(t *testing.T) {
	m := newMemHolder()
	deleted := false
	deletes := 0
	const up = "UPID:pve1:0:0:0:qmdestroy:101:u:"
	routes := fakeRoutes{
		"GET /api2/json/pools/tp-p1": func(w http.ResponseWriter, r *http.Request) {
			if deleted {
				fmt.Fprint(w, `{"data":{"members":[]}}`)
			} else {
				fmt.Fprint(w, `{"data":{"members":[{"type":"qemu","node":"pve1","vmid":101}]}}`)
			}
		},
		"GET /api2/json/nodes/pve1/qemu/101/config": func(w http.ResponseWriter, r *http.Request) {
			if deleted {
				w.WriteHeader(403)
				return
			}
			fmt.Fprint(w, `{"data":{"smbios1":"uuid=owned","scsi0":"nas:vm-101-disk-0,size=8G"}}`)
		},
		"GET /api2/json/nodes/pve1/qemu/101/pending":        say(`{"data":[]}`),
		"GET /api2/json/nodes/pve1/qemu/101/status/current": say(`{"data":{"status":"stopped"}}`),
		"GET /api2/json/cluster/replication":                say(`{"data":[]}`),
		"GET /api2/json/cluster/ha/resources":               say(`{"data":[]}`),
		"GET /api2/json/access/permissions": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"data":{%q:{"Pool.Audit":1}}}`, r.URL.Query().Get("path"))
		},
		"DELETE /api2/json/nodes/pve1/qemu/101": func(w http.ResponseWriter, r *http.Request) {
			deleted = true
			deletes++
			fmt.Fprintf(w, `{"data":%q}`, up)
		},
		"GET /api2/json/nodes/pve1/tasks/" + up + "/status": say(`{"data":{"status":"stopped","exitstatus":"OK"}}`),
	}
	s := volCluster(t, cfgAttached, map[string][]url.Values{}, routes)
	s.HolderOperations = m
	s.HolderBudgets = smallHolderBudgets()
	if e := s.RetireServer(context.Background(), "p1", "101", "", false); e != nil {
		t.Fatal(e)
	}
	if !deleted || deletes != 1 || m.pending != nil {
		t.Fatalf("deleted=%v calls=%d pending=%v", deleted, deletes, m.pending)
	}
}
func TestDeleteSettlementWithPoolOnlyAudit(t *testing.T) {
	const up = "UPID:pve1:0:0:0:qmdestroy:101:u:"
	for _, mode := range []string{"gone", "still-member", "pool-denied", "wrong-task"} {
		t.Run(mode, func(t *testing.T) {
			_, c := fakePVE(t, fakeRoutes{
				"GET /api2/json/pools/tp-p1": func(w http.ResponseWriter, r *http.Request) {
					if mode == "pool-denied" {
						w.WriteHeader(403)
						return
					}
					if mode == "still-member" {
						fmt.Fprint(w, `{"data":{"members":[{"type":"qemu","vmid":101,"node":"pve1"}]}}`)
					} else {
						fmt.Fprint(w, `{"data":{"members":[]}}`)
					}
				},
				"GET /api2/json/access/permissions": func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprintf(w, `{"data":{%q:{"Pool.Audit":1}}}`, r.URL.Query().Get("path"))
				},
			})
			s := &Service{C: c}
			op := &HolderOperation{UPID: UPID(up), Proof: holderProof{Kind: "delete", Pool: "tp-p1", Source: holderVMIdentity{Node: "pve1", VMID: 101, UUID: "uuid=original"}}}
			if mode == "wrong-task" {
				op.UPID = UPID(moveUPID)
			}
			ok, _ := s.holderDeleteEffect(context.Background(), op)
			if ok != (mode == "gone") {
				t.Fatalf("mode %s settled=%v", mode, ok)
			}
		})
	}
}

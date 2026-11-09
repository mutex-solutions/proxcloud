package pve

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// These cases reproduce the permissive GetPool normalization bug: typed zero
// values plus a filled-in requested pool ID must never prove deletion/absence.
func TestHolderPoolMalformedPayloadRetainsDeleteFence(t *testing.T) {
	const up = "UPID:pve1:0:0:0:qmdestroy:101:u:"
	for name, body := range map[string]string{
		"null data":              `{"data":null}`,
		"missing data":           `{}`,
		"empty object":           `{"data":{}}`,
		"wrong pool":             `{"data":{"poolid":"another-project","members":[]}}`,
		"no members":             `{"data":{"poolid":"tp-p1"}}`,
		"null members":           `{"data":{"poolid":"tp-p1","members":null}}`,
		"object members":         `{"data":{"poolid":"tp-p1","members":{}}}`,
		"unknown member":         `{"data":{"poolid":"tp-p1","members":[{"id":"opaque","type":"newkind"}]}}`,
		"invalid guest identity": `{"data":{"poolid":"tp-p1","members":[{"id":"qemu/999","type":"qemu","vmid":101,"node":"pve1"}]}}`,
		"missing guest node":     `{"data":{"poolid":"tp-p1","members":[{"id":"qemu/999","type":"qemu","vmid":999}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			m := newMemHolder()
			m.pending = &HolderOperation{ID: "old-delete", State: "OBSERVING", UPID: UPID(up), Proof: holderProof{Kind: "delete", Pool: "tp-p1", Source: holderVMIdentity{Node: "pve1", VMID: 101, UUID: "uuid=old"}}}
			polls, nextWrites := 0, 0
			_, c := fakePVE(t, fakeRoutes{
				"GET /api2/json/pools/tp-p1": say(body),
				"GET /api2/json/nodes/pve1/tasks/" + up + "/status": func(w http.ResponseWriter, r *http.Request) {
					polls++
					fmt.Fprint(w, `{"data":{"status":"stopped","exitstatus":"OK"}}`)
				},
				"GET /api2/json/nodes/pve1/qemu/101/config": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) },
				"GET /api2/json/access/permissions": func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprintf(w, `{"data":{%q:{"Pool.Audit":1}}}`, r.URL.Query().Get("path"))
				},
				"POST /api2/json/nodes/pve1/qemu/101/config": func(w http.ResponseWriter, r *http.Request) { nextWrites++ },
			})
			s := &Service{C: c, PoolPrefix: "tp-", HolderOperations: m, HolderBudgets: smallHolderBudgets()}
			if _, e := s.AddVolume(context.Background(), "p1", "101", 1, "nas", 0); e == nil {
				t.Fatal("malformed pool allowed new allocation")
			}
			if polls != 1 || m.pending == nil || nextWrites != 0 || m.releases != 0 {
				t.Fatalf("polls=%d pending=%v nextwrites=%d releases=%d", polls, m.pending, nextWrites, m.releases)
			}
		})
	}
}
func TestHolderProtectionNullInventoryNeverProvesAbsence(t *testing.T) {
	for kind, path := range map[string]string{"replication-delete": "/cluster/replication", "ha-rule-delete": "/cluster/ha/rules", "ha-delete": "/cluster/ha/resources"} {
		for _, body := range []string{`{"data":null}`, `{}`, `{"data":{}}`, `{"data":[{}]}`} {
			t.Run(kind+body, func(t *testing.T) {
				_, c := fakePVE(t, fakeRoutes{
					"GET /api2/json" + path:             say(body),
					"GET /api2/json/access/permissions": say(`{"data":{"/":{"Sys.Audit":1}}}`),
				})
				s := &Service{C: c}
				ok, e := s.holderProtectionEffect(context.Background(), holderProof{Kind: kind, Params: map[string]string{"id": "101-0"}})
				if ok || e == nil {
					t.Fatalf("malformed protection inventory proved absence: ok=%v err=%v", ok, e)
				}
			})
		}
	}
}
func TestHolderPoolAcceptsExplicitEmptyAndTypedMembers(t *testing.T) {
	for _, body := range []string{
		`{"data":{"poolid":"tp-p1","members":[]}}`,
		`{"data":{"poolid":"tp-p1","comment":"own project","members":[{"id":"qemu/101","type":"qemu","vmid":101,"node":"pve1"},{"id":"storage/pve1/nas","type":"storage","storage":"nas","node":"pve1"}]}}`,
	} {
		_, c := fakePVE(t, fakeRoutes{"GET /api2/json/pools/tp-p1": say(body)})
		s := &Service{C: c}
		p, e := s.holderPool(context.Background(), "tp-p1")
		if e != nil || p == nil || p.PoolID != "tp-p1" {
			t.Fatalf("pool=%+v err=%v", p, e)
		}
	}
}

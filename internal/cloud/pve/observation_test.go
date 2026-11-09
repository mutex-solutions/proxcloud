package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

func TestObserveNodesIndependentOfflineAndUnknownGETOnly(t *testing.T) {
	for _, mode := range []string{"offline", "partial", "forbidden", "masked", "malformed", "unknown", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
				"GET /api2/json/access/permissions": func(w http.ResponseWriter, r *http.Request) {
					path := r.URL.Query().Get("path")
					if mode == "forbidden" {
						w.WriteHeader(403)
						return
					}
					b, _ := json.Marshal(map[string]any{"data": map[string]any{path: map[string]int{"Sys.Audit": 1}}})
					fmt.Fprint(w, string(b))
				},
				"GET /api2/json/access/acl": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":[]}`) },
				"GET /api2/json/nodes": func(w http.ResponseWriter, r *http.Request) {
					switch mode {
					case "partial":
						fmt.Fprint(w, `{"data":[{"node":"node1","status":"online"}]}`)
					case "masked":
						w.WriteHeader(400)
					case "malformed":
						fmt.Fprint(w, `{"data":null}`)
					case "unknown":
						fmt.Fprint(w, `{"data":[{"node":"node1","status":"unknown"}]}`)
					case "timeout":
						w.WriteHeader(595)
					default:
						fmt.Fprint(w, `{"data":[{"node":"node1","status":"online"},{"node":"node2","status":"offline"}]}`)
					}
				},
				"GET /api2/json/cluster/status": func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, `{"data":[{"type":"cluster","quorate":1},{"type":"node","name":"node1","online":1},{"type":"node","name":"node2","online":0}]}`)
				},
			})
			s := &Service{C: c}
			o := s.ObserveNodes(context.Background())
			if mode == "offline" {
				if o.Trust != ObservationVerified || len(o.Nodes) != 2 || o.Nodes[1].Status != "OBSERVED_OFFLINE" {
					t.Fatal(o)
				}
			} else if o.Trust == ObservationVerified || len(o.Nodes) != 0 {
				t.Fatal("unknown proves down", o)
			}
		})
	}
}

func TestObserveVerifiedPositivePlatformLinkAndUUIDReuse(t *testing.T) {
	ctx := context.Background()
	s, _ := fakeVMInventoryService(t, []ClusterResource{{Type: "qemu", VMID: 101, Node: "n1", Name: "vm", Pool: "pc-p1"}}, map[int]VMConfig{101: {}})
	p := s.ServerSyncProvider("region", "p1", nil)
	rows, err := p.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.VerifyInventory(ctx, nil, rows); err != nil {
		t.Fatal(err)
	}
	rows[0].ID = "platform-row"
	rows[0].ServiceID = "svc"
	o := p.ObserveVerified(ctx, rows)
	if o.VMTrust != ObservationVerified || len(o.Servers) != 1 || o.Servers[0].ServerID != "platform-row" || o.Servers[0].InstanceID != "00000000-0000-4000-8000-000000000101" {
		t.Fatal(o)
	}
	rows[0].Data[vmUUIDKey] = "00000000-0000-4000-8000-000000000999"
	if o = p.ObserveVerified(ctx, rows); o.VMTrust == ObservationVerified || len(o.Servers) != 0 {
		t.Fatal("VMID reuse exported stale row", o)
	}
}

func TestObserveVerifiedMissingReplicationCannotBecomeSharedOnLocalDisks(t *testing.T) {
	for _, storageType := range []string{"zfspool", "rbd"} {
		t.Run(storageType, func(t *testing.T) {
			ctx := context.Background()
			s, _ := fakeVMInventoryService(t, []ClusterResource{{Type: "qemu", VMID: 101, Node: "n1", Name: "vm", Pool: "pc-p1", Tags: "pc-protected"}}, map[int]VMConfig{101: {}})
			base := s.C.http.Transport
			jobs := true
			s.C.http.Transport = observationRoundTrip(func(r *http.Request) (*http.Response, error) {
				payload := ""
				switch r.URL.Path {
				case "/api2/json/cluster/replication":
					if jobs {
						payload = `{"data":[{"id":"101-0","guest":101,"target":"n2"}]}`
					} else {
						payload = `{"data":[]}`
					}
				case "/api2/json/cluster/ha/resources":
					payload = `{"data":[{"sid":"vm:101","state":"started"}]}`
				case "/api2/json/nodes/n1/replication/101-0/status":
					payload = `{"data":{"last_sync":1700000000,"fail_count":1}}`
				case "/api2/json/storage":
					payload = `{"data":[{"storage":"z","type":"` + storageType + `","shared":1,"content":"images"}]}`
				case "/api2/json/nodes/n1/storage":
					payload = `{"data":[{"storage":"z","type":"` + storageType + `","shared":1,"active":1,"enabled":1}]}`
				}
				if payload != "" {
					return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload)), Request: r}, nil
				}
				return base.RoundTrip(r)
			})
			p := s.ServerSyncProvider("region", "p1", nil)
			rows, err := p.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = p.VerifyInventory(ctx, nil, rows); err != nil {
				t.Fatal(err)
			}
			rows[0].ID, rows[0].ServiceID = "row", "svc"
			before := p.ObserveVerified(ctx, rows)
			if before.ProtectionTrust != ObservationVerified || len(before.Servers) != 1 || before.Servers[0].Mode != "replicated" || before.Servers[0].FailCount != 1 {
				t.Fatal("degraded fixture", before)
			}
			jobs = false
			for range 2 {
				o := p.ObserveVerified(ctx, rows)
				if storageType == "zfspool" {
					if o.ProtectionTrust == ObservationVerified || len(o.Servers) != 0 {
						t.Fatal("lost local-ZFS replica falsely became shared healthy", o)
					}
				} else if o.ProtectionTrust != ObservationVerified || len(o.Servers) != 1 || o.Servers[0].Mode != "shared" || !o.Servers[0].HA {
					t.Fatal("positive current shared storage not accepted", o)
				}
			}
		})
	}
}

type observationRoundTrip func(*http.Request) (*http.Response, error)

func (f observationRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestObserveVerifiedRequiresProofAndNeverLeaksGuessedRefs(t *testing.T) {
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/nodes": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"node":"node1","status":"online"}]}`)
		},
		"GET /api2/json/cluster/resources": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":[]}`) },
		"GET /api2/json/pools/pc-project1": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":{"poolid":"pc-project1","members":[]}}`)
		},
	})
	s := &Service{C: c, ServiceID: "provider", PoolPrefix: "pc-"}
	p := s.ServerSyncProvider("region", "project1", nil)
	if o := p.ObserveVerified(context.Background(), nil); o.VMTrust == ObservationVerified {
		t.Fatal("unproved scope exported", o)
	}
	p.inventory = &vmInventory{svc: s, project: "project1", page: []ClusterResource{}, pool: &PoolInfo{PoolID: "pc-project1", Members: []PoolMember{}}, configs: map[int]VMConfig{}, nodes: []string{"node1"}, online: map[string]bool{"node1": true}, verified: true}
	if o := p.ObserveVerified(context.Background(), nil); o.VMTrust != ObservationVerified || !o.LinksComplete || len(o.Servers) != 0 {
		t.Fatal(o)
	}
	for _, cr := range []cloud.CloudResource{
		{ID: "foreign", ProjectID: "foreign", ServiceID: "provider", Type: cloud.TypeServer, Region: "region", ExternalID: "1"},
		{ID: "guess", ProjectID: "project1", ServiceID: "provider", Type: cloud.TypeServer, Region: "region", ExternalID: "1", Data: map[string]any{vmUUIDKey: "cached-unproven"}},
	} {
		if o := p.ObserveVerified(context.Background(), []cloud.CloudResource{cr}); o.VMTrust == ObservationVerified || len(o.Servers) != 0 {
			t.Fatal("unproven ref", o)
		}
	}
	// All fake routes are GET-only; any mutation is an unexpected fakePVE route.
	if strings.Contains(fmt.Sprint(p.ObserveVerified(context.Background(), nil)), "secret") {
		t.Fatal("raw config exported")
	}
}

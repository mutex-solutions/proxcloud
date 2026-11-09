package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFencedConfigNormalEffectAndRecoveryDoesNotReplay(t *testing.T) {
	m := newMemHolder()
	cfg := VMConfig{"smbios1": "uuid=owned", "scsi0": "nas:vm-101-disk-0,size=8G"}
	posts := 0
	statusForbidden := false
	_, c := fakePVE(t, fakeRoutes{
		"GET /api2/json/pools/tp-p1":                 say(`{"data":{"members":[{"type":"qemu","node":"pve1","vmid":101}]}}`),
		"GET /api2/json/nodes/pve1/qemu/101/pending": say(`{"data":[]}`),
		"GET /api2/json/nodes/pve1/qemu/101/config":  func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(map[string]any{"data": cfg}) },
		"POST /api2/json/nodes/pve1/qemu/101/config": func(w http.ResponseWriter, r *http.Request) {
			posts++
			r.ParseForm()
			if v := r.PostForm.Get("scsi1"); v != "" {
				cfg["scsi1"] = "nas:vm-101-disk-1,size=1G"
			}
			fmt.Fprintf(w, `{"data":%q}`, moveUPID)
		},
		moveStatus: func(w http.ResponseWriter, r *http.Request) {
			if statusForbidden {
				w.WriteHeader(403)
				return
			}
			fmt.Fprint(w, `{"data":{"status":"stopped","exitstatus":"OK"}}`)
		},
	})
	s := &Service{C: c, ServiceID: "s", PoolPrefix: "tp-", HolderOperations: m, HolderBudgets: smallHolderBudgets()}
	vm := &ClusterResource{Node: "pve1", VMID: 101}
	ctx, release, e := s.beginHolders(context.Background(), "p1")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.holderConfig(ctx, "p1", vm, map[string]string{"scsi1": "nas:1"}); e != nil {
		t.Fatal(e)
	}
	release()
	if posts != 1 || m.pending != nil || m.releases != 1 {
		t.Fatalf("posts=%d pending=%v releases=%d", posts, m.pending, m.releases)
	}
	// The effect can already be visible when a poll is forbidden. Retrying must
	// settle only; allocating again after observation would create a second disk.
	statusForbidden = true
	ctx, release, e = s.beginHolders(context.Background(), "p1")
	if e != nil {
		t.Fatal(e)
	}
	e = s.holderConfig(ctx, "p1", vm, map[string]string{"scsi1": "nas:1"})
	if e == nil {
		t.Fatal("forbidden poll succeeded")
	}
	release()
	m.mu.Lock()
	m.now = m.now.Add(time.Hour)
	m.mu.Unlock()
	statusForbidden = false
	_, _, e = s.beginHolders(context.Background(), "p1")
	if e == nil {
		t.Fatal("recovery admitted triggering mutation")
	}
	if posts != 2 || m.pending != nil {
		t.Fatalf("recovery replayed: posts=%d pending=%v", posts, m.pending)
	}
}
func TestFencedAsyncMissingOrMalformedUPIDNeverSettles(t *testing.T) {
	for _, up := range []UPID{"", "UPID:pve1:", "UPID:pve2:0:0:0:qmconfig:101:u:", "UPID:pve1:Z:0:0:qmconfig:101:u:", "UPID:pve1:0:0:0:qmconfig:999:u:"} {
		t.Run(string(up), func(t *testing.T) {
			m := newMemHolder()
			s := &Service{HolderOperations: m, HolderBudgets: smallHolderBudgets()}
			ctx, release, e := s.beginHolders(context.Background(), "p")
			if e != nil {
				t.Fatal(e)
			}
			e = s.holderTask(ctx, holderProof{Source: holderVMIdentity{Node: "pve1", VMID: 101}}, time.Second, func(context.Context) (UPID, error) { return up, nil })
			release()
			if e == nil || m.pending == nil || m.pending.UPID != "" || m.releases != 0 {
				t.Fatalf("invalid reply settled: %v %+v", e, m.pending)
			}
		})
	}
}

type holderRoundTrip func(*http.Request) (*http.Response, error)

func (f holderRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDeleteReadbackCanonicalStatusLineAndPermissionProof(t *testing.T) {
	p := holderProof{Kind: "delete", Pool: "tp-p1", Source: holderVMIdentity{Node: "pve1", VMID: 101, UUID: "uuid=old"}}
	for _, mode := range []string{"status-line", "errors-map", "wrong-node", "hidden404", "no-privilege", "wrong-uuid"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := New(Config{APIURL: "https://fake", TokenID: "u", Secret: "s"})
			c.http = &http.Client{Transport: holderRoundTrip(func(r *http.Request) (*http.Response, error) {
				status := 200
				reason := "200 OK"
				body := `{"data":{"/vms/101":{"VM.Audit":1}}}`
				if strings.HasSuffix(r.URL.Path, "/config") {
					status = 500
					reason = "500 Configuration file 'nodes/pve1/qemu-server/101.conf' does not exist"
					body = `{"data":null}`
					switch mode {
					case "errors-map":
						reason = "500 Internal Server Error"
						body = `{"errors":{"vmid":"Configuration file 'nodes/pve1/qemu-server/101.conf' does not exist"}}`
					case "wrong-node":
						reason = "500 Configuration file 'nodes/pve2/qemu-server/101.conf' does not exist"
					case "hidden404":
						status = 404
						reason = "404 Not Found"
					case "wrong-uuid":
						status = 200
						reason = "200 OK"
						body = `{"data":{"smbios1":"uuid=reused"}}`
					}
				} else if mode == "no-privilege" {
					body = `{"data":{}}`
				}
				return &http.Response{StatusCode: status, Status: reason, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			s := &Service{C: c}
			ok, _ := s.holderEffect(context.Background(), p, false)
			want := mode == "status-line" || mode == "errors-map"
			if ok != want {
				t.Fatalf("readback=%v want=%v", ok, want)
			}
		})
	}
}
func TestNewHolderUnknownDoesNotRunDestroyDefer(t *testing.T) {
	for _, mode := range []string{"create-reply-lost", "create-empty", "create-running", "configure-reply-lost"} {
		t.Run(mode, func(t *testing.T) {
			m := newMemHolder()
			uuid := ""
			deletes, creates, configures, polls := 0, 0, 0, 0
			const createUP = "UPID:pve1:0:0:0:qmcreate:900:u:"
			routes := fakeRoutes{
				"GET /api2/json/cluster/nextid":   say(`{"data":"900"}`),
				"GET /api2/json/pools/tp-p1":      say(`{"data":{"members":[{"type":"qemu","node":"pve1","vmid":900}]}}`),
				"GET " + carrierPath + "/pending": say(`{"data":[]}`),
				"GET " + carrierPath + "/config": func(w http.ResponseWriter, r *http.Request) {
					json.NewEncoder(w).Encode(map[string]any{"data": VMConfig{"smbios1": uuid}})
				},
				"POST /api2/json/nodes/pve1/qemu": func(w http.ResponseWriter, r *http.Request) {
					creates++
					r.ParseForm()
					uuid = r.PostForm.Get("smbios1")
					if uuid == "" {
						t.Error("create omitted preselected identity")
					}
					switch mode {
					case "create-reply-lost":
						w.WriteHeader(500)
					case "create-empty":
						fmt.Fprint(w, `{"data":null}`)
					default:
						fmt.Fprintf(w, `{"data":%q}`, createUP)
					}
				},
				"POST " + carrierPath + "/config": func(w http.ResponseWriter, r *http.Request) { configures++; refuse(w, r) },
				"GET /api2/json/nodes/pve1/tasks/" + createUP + "/status": func(w http.ResponseWriter, r *http.Request) {
					polls++
					if mode == "create-running" {
						fmt.Fprint(w, `{"data":{"status":"running"}}`)
					} else {
						fmt.Fprint(w, `{"data":{"status":"stopped","exitstatus":"OK"}}`)
					}
				},
				"DELETE " + carrierPath: func(w http.ResponseWriter, r *http.Request) { deletes++ },
			}
			_, c := fakePVE(t, routes)
			s := &Service{C: c, PoolPrefix: "tp-", HolderOperations: m, HolderBudgets: smallHolderBudgets()}
			if _, e := s.newHolder(context.Background(), "p1", "pve1", tagHolder); e == nil {
				t.Fatal("unknown create/config succeeded")
			}
			wantConfig := 0
			if mode == "configure-reply-lost" {
				wantConfig = 1
			}
			if deletes != 0 || m.pending == nil || creates != 1 || configures != wantConfig {
				t.Fatalf("destructive=%d pending=%v creates=%d configures=%d polls=%d", deletes, m.pending, creates, configures, polls)
			}
			if (mode == "configure-reply-lost" || mode == "create-running") && polls == 0 {
				t.Fatal("intended task wait never reached")
			}
		})
	}
}

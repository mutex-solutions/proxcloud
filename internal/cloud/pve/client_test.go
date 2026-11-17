package pve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakePVE is a minimal Proxmox API fake: routes are matched on "METHOD path".
func fakePVE(t *testing.T, routes map[string]func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "PVEAPIToken=proxcloud@pve!t1=sec" {
			t.Errorf("bad auth header %q", got)
		}
		h, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"errors":{"path":"no fake for %s %s"}}`, r.Method, r.URL.Path)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{APIURL: srv.URL, TokenID: "proxcloud@pve!t1", Secret: "sec"})
	if err != nil {
		t.Fatal(err)
	}
	return srv, c
}

func TestEnvelopeAndAuth(t *testing.T) {
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/version": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":{"version":"8.2.4","release":"8.2"}}`)
		},
	})
	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v["version"] != "8.2.4" {
		t.Fatalf("version = %v", v["version"])
	}
}

func TestAPIErrorMapping(t *testing.T) {
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/nodes/x/qemu/1/config": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"data":null}`)
		},
	})
	// non-2xx → *APIError; the fake writes plain 500 with no message
	_, err := c.GetVMConfig(context.Background(), "x", 1)
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T %v", err, err)
	}
	if ae.Status != 500 {
		t.Fatalf("status = %d", ae.Status)
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(&APIError{Status: 404}) {
		t.Fatal("404 should be not-found")
	}
	if !IsNotFound(&APIError{Status: 500, Message: `Configuration file 'nodes/x/qemu-server/9.conf' does not exist`}) {
		t.Fatal("PVE 500 does-not-exist should be not-found")
	}
	if IsNotFound(&APIError{Status: 500, Message: "boom"}) {
		t.Fatal("plain 500 must not be not-found")
	}
	if IsNotFound(fmt.Errorf("x")) {
		t.Fatal("non-APIError must not be not-found")
	}
}

func TestMutationSendsFormAndReturnsUPID(t *testing.T) {
	upid := "UPID:pve1:0003C4D1:1A2B3C4D:65F0A1B2:qmcreate:101:proxcloud@pve!t1:"
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api2/json/nodes/pve1/qemu": func(w http.ResponseWriter, r *http.Request) {
			r.ParseForm()
			if r.PostFormValue("vmid") != "101" || r.PostFormValue("cores") != "2" {
				t.Errorf("form = %v", r.PostForm)
			}
			if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
				t.Errorf("content-type = %q", ct)
			}
			fmt.Fprintf(w, `{"data":%q}`, upid)
		},
	})
	got, err := c.CreateVM(context.Background(), "pve1", map[string]string{"vmid": "101", "cores": "2"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != upid {
		t.Fatalf("upid = %q", got)
	}
	if got.Node() != "pve1" {
		t.Fatalf("node = %q", got.Node())
	}
}

func TestWaitTask(t *testing.T) {
	upid := UPID("UPID:pve1:0:0:0:qmstart:101:u:")
	var calls atomic.Int32
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/nodes/pve1/tasks/" + string(upid) + "/status": func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) < 3 {
				fmt.Fprint(w, `{"data":{"status":"running"}}`)
				return
			}
			fmt.Fprint(w, `{"data":{"status":"stopped","exitstatus":"OK"}}`)
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.WaitTask(ctx, upid); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestWaitTaskFailure(t *testing.T) {
	upid := UPID("UPID:pve1:0:0:0:qmcreate:101:u:")
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/nodes/pve1/tasks/" + string(upid) + "/status": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":{"status":"stopped","exitstatus":"unable to create image: disk full"}}`)
		},
	})
	err := c.WaitTask(context.Background(), upid)
	if err == nil || !contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
}

func TestNextIDStringDecode(t *testing.T) {
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/nextid": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":"105"}`) // PVE returns the id as a JSON string
		},
	})
	id, err := c.NextID(context.Background())
	if err != nil || id != 105 {
		t.Fatalf("id=%d err=%v", id, err)
	}
}

// VMIDFree asks /cluster/nextid?vmid=, which reads the cluster's whole guest list (PVE answers 400
// "VM <id> already exists" for a held id); any other refusal is an error, never "free".
func TestVMIDFree(t *testing.T) {
	answer := map[string]func(http.ResponseWriter){
		"9210": func(w http.ResponseWriter) { fmt.Fprint(w, `{"data":"9210"}`) },
		"9211": func(w http.ResponseWriter) {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"errors":{"vmid":"VM 9211 already exists\n"},"data":null}`)
		},
		"9212": func(w http.ResponseWriter) {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"errors":{"vmid":"value must have a minimum value of 100"},"data":null}`)
		},
		// A 2xx that does not echo the id is not "free": a proxy or a changed endpoint.
		"9214": func(w http.ResponseWriter) { fmt.Fprint(w, `{"data":null}`) },
		"9215": func(w http.ResponseWriter) { fmt.Fprint(w, `{}`) },
		"9216": func(w http.ResponseWriter) { fmt.Fprint(w, `{"data":"100"}`) },
		"9217": func(w http.ResponseWriter) { fmt.Fprint(w, `{"data":{"vmid":9217}}`) },
		"9218": func(w http.ResponseWriter) { fmt.Fprint(w, `{"data":9218}`) },
	}
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/nextid": func(w http.ResponseWriter, r *http.Request) {
			if a, ok := answer[r.URL.Query().Get("vmid")]; ok {
				a(w)
				return
			}
			w.WriteHeader(500)
		},
	})
	for vmid, want := range map[int]bool{9210: true, 9211: false, 9218: true} {
		if free, err := c.VMIDFree(context.Background(), vmid); err != nil || free != want {
			t.Errorf("VMIDFree(%d) = %v, %v", vmid, free, err)
		}
	}
	for _, vmid := range []int{9212, 9213, 9214, 9215, 9216, 9217} {
		if free, err := c.VMIDFree(context.Background(), vmid); err == nil || free {
			t.Errorf("VMIDFree(%d): no error", vmid)
		}
	}
}

func TestCreatePoolIdempotent(t *testing.T) {
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api2/json/pools": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"errors":{"poolid":"pool 'tp-p1' already exists"}}`)
		},
	})
	if err := c.CreatePool(context.Background(), "tp-p1", ""); err != nil {
		t.Fatalf("already-exists must be idempotent, got %v", err)
	}
}

func TestGetPoolAbsentIsNil(t *testing.T) {
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/pools/tp-gone": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"errors":{"poolid":"pool 'tp-gone' does not exist"}}`)
		},
	})
	p, err := c.GetPool(context.Background(), "tp-gone")
	if err != nil || p != nil {
		t.Fatalf("p=%v err=%v", p, err)
	}
}

func TestFingerprintPinning(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"version":"8.2.4"}}`)
	}))
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().Raw)
	fp := hex.EncodeToString(sum[:])

	// correct fingerprint (colon-separated, uppercase — operator paste format)
	pretty := ""
	for i := 0; i < len(fp); i += 2 {
		if i > 0 {
			pretty += ":"
		}
		pretty += string(fp[i]) + string(fp[i+1])
	}
	c, err := New(Config{APIURL: srv.URL, TokenID: "proxcloud@pve!t1", Secret: "sec", Fingerprint: pretty})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Version(context.Background()); err != nil {
		t.Fatalf("pinned fingerprint should verify: %v", err)
	}

	// wrong fingerprint must refuse
	bad, _ := New(Config{APIURL: srv.URL, TokenID: "proxcloud@pve!t1", Secret: "sec", Fingerprint: "00" + fp[2:]})
	_, err = bad.Version(context.Background())
	if err == nil {
		t.Fatal("wrong fingerprint must fail the handshake")
	}
	// The refusal names what the server presented, in the form the pin takes.
	if got := PresentedFingerprint(err); got != strings.ToUpper(pretty) {
		t.Fatalf("presented fingerprint = %q, want %q (%v)", got, strings.ToUpper(pretty), err)
	}
	if got := PresentedFingerprint(&APIError{Status: 401}); got != "" {
		t.Fatalf("an API error presented %q", got)
	}
}

func TestVMConfigHelpers(t *testing.T) {
	cfg := VMConfig{"cores": float64(4), "memory": "8192", "name": "vm-a"}
	if cfg.Int("cores") != 4 || cfg.Int("memory") != 8192 || cfg.Str("name") != "vm-a" {
		t.Fatalf("helpers: %v %v %v", cfg.Int("cores"), cfg.Int("memory"), cfg.Str("name"))
	}
	if cfg.Str("cores") != "4" || cfg.Int("absent") != 0 || cfg.Str("absent") != "" {
		t.Fatal("coercion edge cases")
	}
}

func TestMapEntryNode(t *testing.T) {
	e := "id=10de:2204,iommugroup=27,node=pve2,path=0000:41:00,subsystem-id=10de:1454"
	if got := MapEntryNode(e); got != "pve2" {
		t.Fatalf("node = %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestManagementBridges(t *testing.T) {
	network := func(body string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("type") != "any_bridge" {
				t.Errorf("network list without type=any_bridge: %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, body)
		}
	}
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/status": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"type":"cluster","name":"lab"},{"type":"node","name":"pve1","ip":"10.0.0.11"},{"type":"node","name":"pve2","ip":"10.0.0.12"}]}`)
		},
		// pve1: vmbr0 carries the default gateway, vmbr1 is a clean public bridge.
		"GET /api2/json/nodes/pve1/network": network(`{"data":[{"iface":"vmbr0","address":"10.0.0.11","gateway":"10.0.0.1"},{"iface":"vmbr1"}]}`),
		// pve2: the node IP sits on vmbr2 with the gateway elsewhere.
		"GET /api2/json/nodes/pve2/network": network(`{"data":[{"iface":"vmbr2","address":"10.0.0.12"},{"iface":"vmbr1"}]}`),
	})
	got, err := c.ManagementBridges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"vmbr0": "pve1", "vmbr2": "pve2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("all nodes: got %v, want %v", got, want)
	}
	got, err = c.ManagementBridges(context.Background(), "pve2")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"vmbr2": "pve2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pve2 only: got %v, want %v", got, want)
	}
}

// NodeBridges lists every bridge with its management flag; NodesWithoutPackage reads the installed state per node.
func TestNodeBridgesAndPackages(t *testing.T) {
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/status": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"type":"node","name":"pve2","ip":"10.0.0.12"},{"type":"node","name":"pve1","ip":"10.0.0.11"},{"type":"node","name":"pve3","ip":"10.0.0.13"}]}`)
		},
		"GET /api2/json/nodes/pve1/network": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"iface":"vmbr1","type":"bridge"},{"iface":"vmbr0","type":"bridge","address":"10.0.0.11","gateway":"10.0.0.1","cidr":"10.0.0.11/24"}]}`)
		},
		"GET /api2/json/nodes/pve2/network": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"iface":"vmbr2","type":"OVSBridge","address":"10.0.0.12","cidr":"10.0.0.12/24"}]}`)
		},
		// pve3 does not answer its network list: it is left out.
		"GET /api2/json/nodes": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"node":"pve1","status":"online"},{"node":"pve2","status":"online"},{"node":"pve3","status":"offline"}]}`)
		},
		"GET /api2/json/nodes/pve1/apt/versions": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"Package":"frr-pythontools","CurrentState":"Installed"},{"Package":"ceph","CurrentState":"NotInstalled"}]}`)
		},
		"GET /api2/json/nodes/pve2/apt/versions": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"Package":"frr-pythontools","CurrentState":"NotInstalled"}]}`)
		},
	})
	got, err := c.NodeBridges(context.Background())
	want := []NodeBridge{
		{Node: "pve1", Iface: "vmbr0", Type: "bridge", CIDR: "10.0.0.11/24", Management: true},
		{Node: "pve1", Iface: "vmbr1", Type: "bridge"},
		{Node: "pve2", Iface: "vmbr2", Type: "OVSBridge", CIDR: "10.0.0.12/24", Management: true},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("bridges = %+v (%v), want %+v", got, err, want)
	}
	for pkg, want := range map[string][]string{"frr-pythontools": {"pve2"}, "ceph": {"pve1", "pve2"}} {
		if got, err := c.NodesWithoutPackage(context.Background(), pkg); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("nodes without %s = %v (%v), want %v", pkg, got, err, want)
		}
	}
}

func TestMissingPrivileges(t *testing.T) {
	// The token holds everything on "/" and /storage, part of /sdn, nothing on /pool.
	perms := map[string]string{
		"/":        `{"Sys.Audit":0,"Sys.Modify":0}`,
		"/sdn":     `{"SDN.Audit":1,"SDN.Use":1}`,
		"/storage": `{"Datastore.AllocateSpace":1,"Datastore.Audit":1}`,
	}
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/access/permissions": func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Query().Get("path")
			if body, ok := perms[p]; ok {
				fmt.Fprintf(w, `{"data":{%q:%s}}`, p, body)
				return
			}
			fmt.Fprint(w, `{"data":{}}`) // PVE returns an empty map when nothing applies
		},
	})
	checks := []PrivCheck{
		{Path: "/", Privs: []string{"Sys.Audit", "Sys.Modify"}},
		{Path: "/pool", Privs: []string{"Pool.Allocate", "VM.Allocate"}},
		{Path: "/sdn", Privs: []string{"SDN.Allocate", "SDN.Audit", "SDN.Use"}},
		{Path: "/storage", Privs: []string{"Datastore.AllocateSpace", "Datastore.Audit"}},
	}
	got, err := c.MissingPrivileges(context.Background(), checks)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Pool.Allocate, VM.Allocate on /pool", "SDN.Allocate on /sdn"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missing = %q, want %q", got, want)
	}
}

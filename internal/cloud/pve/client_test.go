package pve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestCreatePoolIdempotent(t *testing.T) {
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api2/json/pools": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"errors":{"poolid":"pool 'st-p1' already exists"}}`)
		},
	})
	if err := c.CreatePool(context.Background(), "st-p1", ""); err != nil {
		t.Fatalf("already-exists must be idempotent, got %v", err)
	}
}

func TestGetPoolAbsentIsNil(t *testing.T) {
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/pools/st-gone": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"errors":{"poolid":"pool 'st-gone' does not exist"}}`)
		},
	})
	p, err := c.GetPool(context.Background(), "st-gone")
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
	if _, err := bad.Version(context.Background()); err == nil {
		t.Fatal("wrong fingerprint must fail the handshake")
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

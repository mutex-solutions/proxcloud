package pve

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSeedUploadTaskUsesReportedWorkerNode(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		var data any
		switch r.URL.Path {
		case "/api2/json/nodes/pve2/storage/iso/upload":
			data = "UPID:pve1:0020BF34:01544F11:6AF17B5C:imgcopy::proxcloud@pve!platform:"
		case "/api2/json/nodes/pve1/tasks/UPID:pve1:0020BF34:01544F11:6AF17B5C:imgcopy::proxcloud@pve!platform:/status":
			data = map[string]any{"status": "stopped", "exitstatus": "OK"}
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			http.Error(w, "unexpected", 500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()
	c, err := New(Config{APIURL: srv.URL, TokenID: "t", Secret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	up, err := c.UploadISO(context.Background(), "pve2", "iso", "safe.iso", []byte("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WaitTask(context.Background(), up); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != "POST /api2/json/nodes/pve2/storage/iso/upload" || calls[1] != "GET /api2/json/nodes/pve1/tasks/"+string(up)+"/status" {
		t.Fatalf("wrong worker polling: %v", calls)
	}
	if err := seedTaskID(up, "pve2"); err == nil {
		t.Fatal("non-upload target-node validation was weakened")
	}
	if err := seedUploadTaskID("UPID:pve1:0:0:0:qmconfig:7:t:"); err == nil {
		t.Fatal("upload accepted a non-imgcopy task")
	}
}

func TestSeedTaskIdentityRejectsUnknownSuccess(t *testing.T) {
	for _, data := range []any{nil, "", "not-a-task", "UPID::0:0:0:imgcopy::t:", map[string]any{"unexpected": true}} {
		t.Run("response", func(t *testing.T) {
			mutations := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mutations++
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))
			defer srv.Close()
			c, err := New(Config{APIURL: srv.URL, TokenID: "t", Secret: "s"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.UploadISO(context.Background(), "n1", "iso", "safe.iso", bytes.Repeat([]byte("x"), 512)); err == nil {
				t.Fatal("upload without known task accepted")
			}
			if err := c.DeleteStorageVolume(context.Background(), "n1", "iso", "iso/safe.iso"); err == nil {
				t.Fatal("delete without known task accepted")
			}
			if mutations != 2 {
				t.Fatal("unknown task caused compensation or status polling")
			}
		})
	}
	if err := seedTaskID("UPID:pve1:0003C4D1:1A2B3C4D:65F0A1B2:qmcreate:101:root@pam!proxcloud:", "pve1"); err != nil {
		t.Fatal(err)
	}
}

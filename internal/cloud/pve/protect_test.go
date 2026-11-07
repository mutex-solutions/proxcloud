package pve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// protectionPVE answers the protection reads and records the writes in order.
func protectionPVE(t *testing.T, lastSync int64, managed bool) (*Service, *[]string) {
	t.Helper()
	var calls []string
	reply := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/cluster/replication", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []ReplicationJob{{ID: "7-0", Guest: 7, Target: "b", Schedule: "*/15"}})
	})
	mux.HandleFunc("/api2/json/nodes/a/replication/7-0/status", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, ReplicationStatus{LastSync: lastSync})
	})
	mux.HandleFunc("/api2/json/cluster/ha/resources", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			calls = append(calls, "resource")
			managed = true
		}
		if managed {
			reply(w, []HAResource{{SID: "vm:7"}})
			return
		}
		reply(w, []HAResource{})
	})
	mux.HandleFunc("/api2/json/cluster/ha/rules", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
			calls = append(calls, "rule "+r.Form.Get("nodes")+" strict="+r.Form.Get("strict"))
		}
		reply(w, []HARule{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	svc, err := NewService(Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	return svc, &calls
}

func TestReconcileAddsHAOnlyAfterFirstSync(t *testing.T) {
	svc, calls := protectionPVE(t, 0, false)
	pr, err := svc.ReconcileProtection(context.Background(), ClusterResource{VMID: 7, Node: "a"})
	if err != nil || pr["state"] != ProtectionSyncing || len(*calls) != 0 {
		t.Fatalf("before the first sync: %v, %v, calls %v", pr, err, *calls)
	}
	svc, calls = protectionPVE(t, time.Now().Unix(), false)
	pr, err = svc.ReconcileProtection(context.Background(), ClusterResource{VMID: 7, Node: "a"})
	if err != nil || pr["state"] != ProtectionActive {
		t.Fatalf("after the first sync: %v, %v", pr, err)
	}
	// PVE refuses a rule over an unmanaged resource, so the order matters.
	if want := "resource,rule a,b strict=1"; strings.Join(*calls, ",") != want {
		t.Fatalf("calls = %v, want %s", *calls, want)
	}
}

func TestReconcileReportsALateSyncAsDegraded(t *testing.T) {
	svc, _ := protectionPVE(t, time.Now().Unix()-3*replicationScheduleMinutes*60, true)
	pr, err := svc.ReconcileProtection(context.Background(), ClusterResource{VMID: 7, Node: "a"})
	if err != nil || pr["state"] != ProtectionDegraded {
		t.Fatalf("%v, %v", pr, err)
	}
}

func TestTags(t *testing.T) {
	if got := addTag("a;b", "c"); got != "a;b;c" {
		t.Fatal(got)
	}
	if got := addTag("", "c"); got != "c" {
		t.Fatal(got)
	}
	if got := addTag("a;c", "c"); got != "a;c" {
		t.Fatal(got)
	}
	if got := removeTag("a;c;b", "c"); got != "a;b" {
		t.Fatal(got)
	}
	if HasTag("pc-protected-not", tagProtected) || !HasTag("x;pc-protected", tagProtected) {
		t.Fatal("HasTag matched wrong")
	}
}

func TestReserveMemLeavesRoomForFailover(t *testing.T) {
	nodes := []Node{{Node: "a", Status: "online", MaxCPU: 8, MaxMem: 8 << 30}, {Node: "b", Status: "online", MaxCPU: 8, MaxMem: 8 << 30}}
	// Without a reservation a 2 GiB VM fits on both; a takes it for having more free memory.
	req := SchedReq{VCPUs: 1, RAMMb: 2048}
	if got, err := PickNode(nodes, nil, req); err != nil || (got != "a" && got != "b") {
		t.Fatalf("got %s, %v", got, err)
	}
	// a must keep 7 GiB for a failover, so only b can take it.
	req.ReserveMem = map[string]int64{"a": 7 << 30}
	if got, err := PickNode(nodes, nil, req); err != nil || got != "b" {
		t.Fatalf("got %s, %v", got, err)
	}
	req.ReserveMem = map[string]int64{"a": 7 << 30, "b": 7 << 30}
	if _, err := PickNode(nodes, nil, req); err == nil {
		t.Fatal("placed a VM into memory reserved for failover")
	}
}

// PVE keeps a deleted job listed until pvesr's next pass. A read in that window once put HA back on the VM.
func TestReconcileIgnoresAJobBeingRemoved(t *testing.T) {
	reply := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/cluster/replication", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []ReplicationJob{{ID: "7-0", Guest: 7, Target: "b", RemoveJob: "full"}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	svc, err := NewService(Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	pr, err := svc.ReconcileProtection(context.Background(), ClusterResource{VMID: 7, Node: "a"})
	if err != nil || pr != nil {
		t.Fatalf("got %v, %v", pr, err)
	}
}

// Without Datastore.Allocate PVE refuses the delete: the file must still lose its content.
func TestDeleteSeedVolumeEmptiesWhatItCannotDelete(t *testing.T) {
	reply := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	var uploaded string
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/nodes/a/storage/iso/content/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Permission check failed", http.StatusForbidden)
	})
	mux.HandleFunc("/api2/json/nodes/a/storage/iso/upload", func(w http.ResponseWriter, r *http.Request) {
		f, h, err := r.FormFile("filename")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		body, _ := io.ReadAll(f)
		if strings.Contains(string(body), "secret") {
			t.Error("the replacement must be empty")
		}
		uploaded = h.Filename
		reply(w, "UPID:a:0:0:0:imgcopy::t:")
	})
	mux.HandleFunc("/api2/json/nodes/a/tasks/", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, map[string]any{"status": "stopped", "exitstatus": "OK"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	svc, err := NewService(Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	svc.DeleteSeedVolume(context.Background(), "a", "iso:iso/pcseed-7-x.iso")
	if uploaded != "pcseed-7-x.iso" {
		t.Fatalf("uploaded %q", uploaded)
	}
}

func TestWaitReplicaRemoved(t *testing.T) {
	defer func(d time.Duration) { replicaRemovePoll = d }(replicaRemovePoll)
	replicaRemovePoll = time.Millisecond
	reads := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/cluster/replication", func(w http.ResponseWriter, _ *http.Request) {
		reads++
		jobs := []ReplicationJob{{ID: "8-0", Guest: 8}}
		if reads < 3 {
			jobs = append(jobs, ReplicationJob{ID: "7-0", Guest: 7, RemoveJob: "full"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": jobs})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	svc, err := NewService(Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	svc.waitReplicaRemoved(context.Background(), 7)
	if reads != 3 {
		t.Fatalf("a delete must wait for the job to go: %d reads", reads)
	}
}

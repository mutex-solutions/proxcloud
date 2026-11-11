package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
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
	mux.HandleFunc("/api2/json/nodes/a/qemu/7/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			calls = append(calls, "tags")
			reply(w, "UPID:a:0:0:0:qmconfig:7:t:")
			return
		}
		reply(w, map[string]any{"tags": tagProtected})
	})
	mux.HandleFunc("/api2/json/nodes/a/tasks/", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, map[string]any{"status": "stopped", "exitstatus": "OK"})
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
	pr, err := svc.ReconcileProtection(context.Background(), ClusterResource{VMID: 7, Node: "a", Tags: tagProtected})
	if err != nil || pr["state"] != ProtectionSyncing || len(*calls) != 0 {
		t.Fatalf("before the first sync: %v, %v, calls %v", pr, err, *calls)
	}
	svc, calls = protectionPVE(t, time.Now().Unix(), false)
	pr, err = svc.ReconcileProtection(context.Background(), ClusterResource{VMID: 7, Node: "a", Tags: tagProtected})
	if err != nil || pr["state"] != ProtectionActive {
		t.Fatalf("after the first sync: %v, %v", pr, err)
	}
	// PVE refuses a rule over an unmanaged resource, so the order matters.
	if want := "tags,resource,rule a,b strict=1"; strings.Join(*calls, ",") != want {
		t.Fatalf("calls = %v, want %s", *calls, want)
	}
}

func TestReconcileReportsALateSyncAsDegraded(t *testing.T) {
	svc, _ := protectionPVE(t, time.Now().Unix()-3*replicationScheduleMinutes*60, true)
	pr, err := svc.ReconcileProtection(context.Background(), ClusterResource{VMID: 7, Node: "a", Tags: tagProtected + ";pc-p-5"})
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

// Legacy CDs cannot be adopted by prefix, even when an overwrite would succeed.
func TestDeleteSeedVolumeRetainsUnjournaledLegacy(t *testing.T) {
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
	if err := svc.DeleteSeedVolume(context.Background(), "a", "iso:iso/pcseed-7-x.iso"); err == nil {
		t.Fatal("legacy file without ownership journal must be retained")
	}
	if uploaded != "" {
		t.Fatalf("unjournaled CD was overwritten: %q", uploaded)
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

// sharedProtectionPVE: no replication jobs, storages rbd (shared), zfs (local) and dir (local, not ZFS).
func sharedProtectionPVE(t *testing.T, disks map[string]any, tags string, managed bool) (*Service, *[]string) {
	t.Helper()
	var calls []string
	reply := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/cluster/replication", func(w http.ResponseWriter, _ *http.Request) { reply(w, []ReplicationJob{}) })
	mux.HandleFunc("/api2/json/storage", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []map[string]any{
			{"storage": "rbd", "type": "rbd", "shared": 1, "content": "images"},
			{"storage": "zfs", "type": "zfspool", "content": "images"},
			{"storage": "dir", "type": "dir", "content": "images"},
		})
	})
	mux.HandleFunc("/api2/json/cluster/resources", func(w http.ResponseWriter, _ *http.Request) { reply(w, []any{}) })
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
			calls = append(calls, "rule")
		}
		reply(w, []HARule{})
	})
	mux.HandleFunc("/api2/json/nodes/a/qemu/7/config", func(w http.ResponseWriter, _ *http.Request) {
		cfg := map[string]any{"tags": tags}
		for k, v := range disks {
			cfg[k] = v
		}
		reply(w, cfg)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	svc, err := NewService(Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	return svc, &calls
}

func TestProtectableDisksModes(t *testing.T) {
	for name, tt := range map[string]struct {
		disks      map[string]any
		wantShared bool
		wantErr    bool
	}{
		"all local ZFS": {map[string]any{"scsi0": "zfs:vm-7-disk-0,size=1G"}, false, false},
		"all shared":    {map[string]any{"scsi0": "rbd:vm-7-disk-0,size=1G", "ide2": "rbd:vm-7-cloudinit,media=cdrom"}, true, false},
		"a mix":         {map[string]any{"scsi0": "rbd:vm-7-disk-0,size=1G", "scsi1": "zfs:vm-7-disk-0,size=1G"}, false, true},
		"local, no ZFS": {map[string]any{"scsi0": "dir:7/vm-7-disk-0.qcow2,size=1G"}, false, true},
		"no disk":       {map[string]any{}, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			svc, _ := sharedProtectionPVE(t, nil, "", false)
			vm := &ClusterResource{VMID: 7, Node: "a"}
			cfg := VMConfig{}
			for k, v := range tt.disks {
				cfg[k] = v
			}
			_, shared, err := svc.ProtectableDisks(context.Background(), vm, cfg)
			if shared != tt.wantShared || (err != nil) != tt.wantErr {
				t.Fatalf("shared=%v err=%v", shared, err)
			}
		})
	}
}

// Nothing to wait for on shared storage: HA at the first read, no rule, and the state says so.
func TestReconcileSharedAddsHAAtOnce(t *testing.T) {
	rbd := map[string]any{"scsi0": "rbd:vm-7-disk-0,size=1G"}
	svc, calls := sharedProtectionPVE(t, rbd, tagProtected, false)
	pr, err := svc.ReconcileProtection(context.Background(), ClusterResource{VMID: 7, Node: "a", Tags: tagProtected})
	if err != nil || pr["state"] != ProtectionActive || pr["mode"] != ProtectionModeShared {
		t.Fatalf("%v, %v", pr, err)
	}
	if strings.Join(*calls, ",") != "resource" {
		t.Fatalf("calls = %v", *calls)
	}

	// Already managed: no config read, so it also answers with the node down (the fake has no node "gone").
	svc, calls = sharedProtectionPVE(t, rbd, tagProtected, true)
	pr, err = svc.ReconcileProtection(context.Background(), ClusterResource{VMID: 7, Node: "gone", Tags: tagProtected})
	if err != nil || pr["state"] != ProtectionActive || len(*calls) != 0 {
		t.Fatalf("managed: %v, %v, calls %v", pr, err, *calls)
	}
}

func TestReconcileSharedLeavesOthersAlone(t *testing.T) {
	rbd := map[string]any{"scsi0": "rbd:vm-7-disk-0,size=1G"}
	zfs := map[string]any{"scsi0": "zfs:vm-7-disk-0,size=1G"}
	for name, tt := range map[string]struct {
		disks    map[string]any
		rowTags  string
		liveTags string
	}{
		"no tag":                      {rbd, "", ""},
		"tag gone since the row read": {rbd, tagProtected, ""},
		"ZFS whose job was removed":   {zfs, tagProtected, tagProtected},
	} {
		t.Run(name, func(t *testing.T) {
			svc, calls := sharedProtectionPVE(t, tt.disks, tt.liveTags, false)
			pr, err := svc.ReconcileProtection(context.Background(), ClusterResource{VMID: 7, Node: "a", Tags: tt.rowTags})
			if err != nil || pr != nil || len(*calls) != 0 {
				t.Fatalf("%v, %v, calls %v", pr, err, *calls)
			}
		})
	}
}

func TestRefuseUnreplicableByMode(t *testing.T) {
	svc, _ := sharedProtectionPVE(t, nil, "", false)
	vm := &ClusterResource{VMID: 7, Node: "a", Tags: tagProtected}
	if err := svc.refuseUnreplicable(context.Background(), vm, "rbd"); err != nil {
		t.Fatalf("shared disk on a shared VM: %v", err)
	}
	if err := svc.refuseUnreplicable(context.Background(), vm, "zfs"); err == nil {
		t.Fatal("a local disk would stay behind on a failover")
	}
	if err := svc.refuseUnreplicable(context.Background(), &ClusterResource{VMID: 7}, "zfs"); err != nil {
		t.Fatalf("an unprotected VM takes anything: %v", err)
	}
}

// lifecyclePVE is one VM 7 on node a under protection: config, jobs and HA are state, and writes are recorded in order.
type lifecyclePVE struct {
	tags    string
	jobs    []ReplicationJob
	ha      bool
	rule    bool
	status  func() string
	calls   []string
	onWrite func(call string)
}

func (p *lifecyclePVE) service(t *testing.T) *Service {
	t.Helper()
	reply := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	rec := func(c string) {
		p.calls = append(p.calls, c)
		if p.onWrite != nil {
			p.onWrite(c)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/nodes", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []Node{{Node: "a", Status: "online"}, {Node: "b", Status: "online"}})
	})
	mux.HandleFunc("/api2/json/cluster/replication", func(w http.ResponseWriter, _ *http.Request) { reply(w, p.jobs) })
	mux.HandleFunc("/api2/json/cluster/replication/", func(w http.ResponseWriter, _ *http.Request) {
		for i := range p.jobs {
			p.jobs[i].RemoveJob = "full"
		}
		rec("job")
		reply(w, nil)
	})
	mux.HandleFunc("/api2/json/nodes/a/replication/7-0/status", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, ReplicationStatus{LastSync: 1000})
	})
	mux.HandleFunc("/api2/json/cluster/ha/resources", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			p.ha = true
			rec("ha+")
		}
		if p.ha {
			reply(w, []HAResource{{SID: "vm:7"}})
			return
		}
		reply(w, []HAResource{})
	})
	mux.HandleFunc("/api2/json/cluster/ha/resources/", func(w http.ResponseWriter, _ *http.Request) {
		p.ha = false
		rec("ha-")
		reply(w, nil)
	})
	mux.HandleFunc("/api2/json/cluster/ha/rules", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			p.rule = true
			rec("rule+")
		}
		if p.rule {
			reply(w, []HARule{{Rule: haRuleID(7)}})
			return
		}
		reply(w, []HARule{})
	})
	mux.HandleFunc("/api2/json/cluster/ha/rules/", func(w http.ResponseWriter, _ *http.Request) {
		p.rule = false
		rec("rule-")
		reply(w, nil)
	})
	mux.HandleFunc("/api2/json/nodes/a/qemu/7/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			_ = r.ParseForm()
			if _, ok := r.Form["tags"]; ok {
				p.tags = r.Form.Get("tags")
				rec("tags")
			} else {
				rec("config")
			}
			reply(w, "UPID:a:0:0:0:qmconfig:7:t:")
			return
		}
		reply(w, map[string]any{"tags": p.tags, "scsi0": "zfs:vm-7-disk-0,size=10G"})
	})
	mux.HandleFunc("/api2/json/nodes/a/qemu/7/status/current", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, map[string]any{"status": p.status()})
	})
	mux.HandleFunc("/api2/json/nodes/a/qemu/7/status/", func(w http.ResponseWriter, r *http.Request) {
		rec(strings.TrimPrefix(r.URL.Path, "/api2/json/nodes/a/qemu/7/status/"))
		reply(w, "UPID:a:0:0:0:qmpower:7:t:")
	})
	mux.HandleFunc("/api2/json/nodes/a/tasks/", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, map[string]any{"status": "stopped", "exitstatus": "OK"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected %s %s", r.Method, r.URL.Path) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	svc, err := NewService(Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// A rollback older than the enable takes the tag while the job and HA run on: disable must still undo them.
func TestDisableKeysOnTheJobNotTheTag(t *testing.T) {
	p := &lifecyclePVE{tags: "proxcloud", jobs: []ReplicationJob{{ID: "7-0", Guest: 7, Target: "b"}}, ha: true}
	svc := p.service(t)
	if err := svc.disableProtectionOn(context.Background(), &ClusterResource{VMID: 7, Node: "a"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.calls, ","); got != "job,rule-,ha-" { // tags already bare
		t.Fatalf("calls = %s", got)
	}
}

// An unprotected VM reads and writes nothing else, and a shared VM that lost its tag still loses its HA.
func TestDisableWithoutJobOrTag(t *testing.T) {
	p := &lifecyclePVE{tags: "proxcloud"}
	if err := p.service(t).disableProtectionOn(context.Background(), &ClusterResource{VMID: 7, Node: "a"}); err != nil || len(p.calls) != 0 {
		t.Fatalf("%v, calls %v", err, p.calls)
	}
	p = &lifecyclePVE{tags: "proxcloud", ha: true}
	if err := p.service(t).disableProtectionOn(context.Background(), &ClusterResource{VMID: 7, Node: "a"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.calls, ","); got != "rule-,ha-" {
		t.Fatalf("calls = %s", got)
	}
}

// Reconcile may run between any two steps of a disable and must never add HA back.
func TestDisableRacingReconcileLeavesNoHA(t *testing.T) {
	p := &lifecyclePVE{tags: "proxcloud;" + tagProtected + ";pc-p-5", jobs: []ReplicationJob{{ID: "7-0", Guest: 7, Target: "b"}}, ha: true, rule: true}
	svc := p.service(t)
	row := ClusterResource{VMID: 7, Node: "a", Tags: p.tags} // a read from before the disable
	reconciling := false
	p.onWrite = func(string) {
		if reconciling {
			return
		}
		reconciling = true
		defer func() { reconciling = false }()
		if _, err := svc.ReconcileProtection(context.Background(), row); err != nil {
			t.Error(err)
		}
	}
	if err := svc.disableProtectionOn(context.Background(), &ClusterResource{VMID: 7, Node: "a"}); err != nil {
		t.Fatal(err)
	}
	if p.ha || HasTag(p.tags, tagProtected) || slices.Contains(p.calls, "ha+") || slices.Contains(p.calls, "rule+") {
		t.Fatalf("ha=%v tags=%q calls=%v", p.ha, p.tags, p.calls)
	}
	if strings.Join(p.calls, ",") != "job,tags,rule-,ha-" {
		t.Fatalf("calls = %v", p.calls)
	}
}

// A live job with no tag gets it back, so billing and the console see protection again.
func TestStampDerivesTheTagFromTheJob(t *testing.T) {
	p := &lifecyclePVE{tags: "proxcloud", jobs: []ReplicationJob{{ID: "7-0", Guest: 7, Target: "b"}}, ha: true}
	svc := p.service(t)
	before := time.Now().Unix()
	data := map[string]any{"server": map[string]any{}}
	svc.StampProtection(context.Background(), ClusterResource{VMID: 7, Node: "a", Tags: "proxcloud"}, data)
	if !HasTag(p.tags, tagProtected) {
		t.Fatalf("tags = %q", p.tags)
	}
	pr, _ := data["server"].(map[string]any)["protection"].(map[string]any)
	since, err := time.Parse(time.RFC3339, fmt.Sprint(pr["since"]))
	if err != nil || since.Unix() < before {
		t.Fatalf("since = %v (%v)", pr["since"], err)
	}
}

// The stamp is when the first sync was seen, not PVE's last_sync (1000 in the fake), which is when that sync began: after
// a three-day sync that would bill the three days. It goes on before HA, so an active row always has it.
func TestFirstSyncStampsBeforeHA(t *testing.T) {
	p := &lifecyclePVE{tags: tagProtected, jobs: []ReplicationJob{{ID: "7-0", Guest: 7, Target: "b"}}}
	svc := p.service(t)
	before := time.Now().Unix()
	if _, err := svc.ReconcileProtection(context.Background(), ClusterResource{VMID: 7, Node: "a", Tags: tagProtected}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.calls, ","); got != "tags,ha+,rule+" {
		t.Fatalf("calls = %s", got)
	}
	since, err := strconv.ParseInt(TagValue(p.tags, tagProtectedSince), 10, 64)
	if err != nil || since < before {
		t.Fatalf("tags = %q", p.tags)
	}
	// A second read changes nothing.
	p.calls = nil
	if _, err := svc.ReconcileProtection(context.Background(), ClusterResource{VMID: 7, Node: "a", Tags: p.tags}); err != nil || len(p.calls) != 0 {
		t.Fatalf("%v, calls %v", err, p.calls)
	}
}

func TestKeepProtectionTags(t *testing.T) {
	for name, tt := range map[string]struct{ now, before, want string }{
		"snapshot older than the enable": {"fl-a;proxcloud", "fl-b;pc-protected;pc-p-9;proxcloud", "fl-a;proxcloud;pc-protected;pc-p-9"},
		"snapshot of an earlier enable":  {"pc-protected;pc-p-1;proxcloud", "pc-protected;pc-p-9;proxcloud", "proxcloud;pc-protected;pc-p-9"},
		"snapshot from before a disable": {"pc-protected;pc-p-1;proxcloud", "proxcloud", "proxcloud"},
		"same protection":                {"fl-a;pc-protected;pc-p-9", "fl-b;pc-protected;pc-p-9", "fl-a;pc-protected;pc-p-9"},
	} {
		t.Run(name, func(t *testing.T) {
			p := &lifecyclePVE{tags: tt.now}
			if err := p.service(t).keepProtectionTags(context.Background(), &ClusterResource{VMID: 7, Node: "a"}, tt.before); err != nil {
				t.Fatal(err)
			}
			if p.tags != tt.want {
				t.Fatalf("tags = %q, want %q", p.tags, tt.want)
			}
		})
	}
}

func shortStopWait(t *testing.T, wait time.Duration) {
	t.Helper()
	w, p := vmStopWait, vmStopPoll
	vmStopWait, vmStopPoll = wait, time.Millisecond
	t.Cleanup(func() { vmStopWait, vmStopPoll = w, p })
}

// An HA stop outlasting the old one-minute wait must hold the config write and the start until the VM is down.
func TestWaitStoppedHoldsUntilTheVMIsDown(t *testing.T) {
	shortStopWait(t, time.Minute)
	reads := 0
	p := &lifecyclePVE{status: func() string {
		reads++
		if reads < 5 {
			return "running"
		}
		return "stopped"
	}}
	if err := p.service(t).waitStopped(context.Background(), &ClusterResource{VMID: 7, Node: "a", Tags: tagProtected}); err != nil || reads != 5 || len(p.calls) != 0 {
		t.Fatalf("%v, reads %d, calls %v", err, reads, p.calls)
	}
}

// A VM that never stops: an error, and HA is told to keep it up, so nothing leaves it off later.
func TestWaitStoppedFailsAndKeepsTheVMUp(t *testing.T) {
	shortStopWait(t, 20*time.Millisecond)
	p := &lifecyclePVE{status: func() string { return "running" }}
	err := p.service(t).waitStopped(context.Background(), &ClusterResource{VMID: 7, Node: "a", Name: "x", Tags: tagProtected})
	if err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(p.calls, ",") != "start" {
		t.Fatalf("calls = %v", p.calls)
	}
}

// Multiple reachable nodes do not authorize overwriting an unjournaled CD.
func TestDeleteSeedVolumeRetainsSharedLegacy(t *testing.T) {
	reply := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	uploads := []string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/storage", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []map[string]any{{"storage": "iso", "type": "nfs", "shared": 1, "content": "iso"}})
	})
	mux.HandleFunc("/api2/json/cluster/resources", func(w http.ResponseWriter, _ *http.Request) { reply(w, []any{}) })
	mux.HandleFunc("/api2/json/nodes", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []Node{{Node: "a", Status: "offline"}, {Node: "b", Status: "online"}, {Node: "c", Status: "offline"}})
	})
	mux.HandleFunc("/api2/json/nodes/a/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no route to host", 595)
	})
	mux.HandleFunc("/api2/json/nodes/b/storage/iso/content/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Permission check failed", http.StatusForbidden)
	})
	mux.HandleFunc("/api2/json/nodes/b/storage/iso/upload", func(w http.ResponseWriter, _ *http.Request) {
		uploads = append(uploads, "b")
		reply(w, "UPID:b:0:0:0:imgcopy::t:")
	})
	mux.HandleFunc("/api2/json/nodes/b/tasks/", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, map[string]any{"status": "stopped", "exitstatus": "OK"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	svc, err := NewService(Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSeedVolume(context.Background(), "a", "iso:iso/pcseed-7-x.iso"); err == nil {
		t.Fatal("shared storage does not prove legacy ownership")
	}
	if len(uploads) != 0 {
		t.Fatalf("foreign shared CD overwritten: %v", uploads)
	}
}

// Nothing can take the content off: the caller has to hear it, to keep its row and retry.
func TestDeleteSeedVolumeReportsWhatItCouldNotRemove(t *testing.T) {
	reply := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/storage", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []map[string]any{{"storage": "iso", "type": "dir", "content": "iso"}})
	})
	mux.HandleFunc("/api2/json/cluster/resources", func(w http.ResponseWriter, _ *http.Request) { reply(w, []any{}) })
	mux.HandleFunc("/api2/json/nodes/a/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no route to host", 595)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	svc, err := NewService(Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSeedVolume(context.Background(), "a", "iso:iso/pcseed-7-x.iso"); err == nil {
		t.Fatal("want an error")
	}
}

package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

// pendingFake is a Proxmox that keeps a config change it cannot apply to a running VM as pending, as the real one
// does with vga: the values in effect (config?current=1) keep the old value, the default config read answers the new
// one, and /pending lists the key with a "pending" member until the VM is started again or the change is reverted.
// Every other key is applied at once (a hot-plugged disk). The fakes before it applied vga at once and served an
// empty /pending, which is how a pass that wrote running VMs passed its tests.
type pendingFake struct {
	mu       sync.Mutex
	status   map[int]string
	cfg      map[int]VMConfig
	pending  map[int]VMConfig
	writes   []string
	onConfig func(vmid int) // runs before a config write is applied
}

// newPendingCluster is pool pc-p1 on node n1 with the given VMs, all with a serial display and a serial port, over
// the routes the VM inventory needs (fakeVMInventoryService).
func newPendingCluster(t *testing.T, page []ClusterResource) (*pendingFake, *Service) {
	f := &pendingFake{status: map[int]string{}, cfg: map[int]VMConfig{}, pending: map[int]VMConfig{}}
	routes := fakeRoutes{}
	for _, r := range page {
		id := r.VMID
		f.status[id], f.cfg[id], f.pending[id] = r.Status, VMConfig{"vga": "serial0", "serial0": "socket"}, VMConfig{}
		path := fmt.Sprintf("/api2/json/nodes/n1/qemu/%d", id)
		upid := fmt.Sprintf("UPID:n1:0:0:0:qmconfig:%d:u:", id) // a fenced write checks the task's VM id
		routes["GET /api2/json/nodes/n1/tasks/"+upid+"/status"] = say(`{"data":{"status":"stopped","exitstatus":"OK"}}`)
		reply := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
		routes["GET "+path+"/config"] = func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			cfg := maps.Clone(f.cfg[id])
			if r.URL.Query().Get("current") != "1" {
				maps.Copy(cfg, f.pending[id])
			}
			reply(w, cfg)
		}
		routes["GET "+path+"/pending"] = func(w http.ResponseWriter, _ *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			rows := []map[string]any{}
			for k, v := range f.cfg[id] {
				row := map[string]any{"key": k, "value": v}
				if p, ok := f.pending[id][k]; ok {
					row["pending"] = p
				}
				rows = append(rows, row)
			}
			reply(w, rows)
		}
		routes["GET "+path+"/status/current"] = func(w http.ResponseWriter, _ *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			reply(w, map[string]any{"status": f.status[id], "qmpstatus": f.status[id], "name": f.cfg[id]["name"], "tags": f.cfg[id]["tags"]})
		}
		routes["POST "+path+"/config"] = func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.onConfig != nil {
				f.onConfig(id)
			}
			f.writes = append(f.writes, fmt.Sprintf("config %d %s", id, r.PostForm.Encode()))
			for k := range r.PostForm {
				v := r.PostForm.Get(k)
				switch {
				case k == "revert":
					delete(f.pending[id], v)
				case k == "vga" && f.status[id] == "running":
					f.pending[id][k] = v
				default:
					f.cfg[id][k] = v
					maps.DeleteFunc(f.cfg[id], func(o string, ov any) bool { return strings.HasPrefix(o, "unused") && ov == v })
				}
			}
			reply(w, upid)
		}
		routes["POST "+path+"/status/start"] = func(w http.ResponseWriter, _ *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.writes = append(f.writes, fmt.Sprintf("start %d", id))
			f.status[id] = "running"
			maps.Copy(f.cfg[id], f.pending[id])
			clear(f.pending[id])
			reply(w, upid)
		}
	}
	s, _ := fakeVMInventoryService(t, page, f.cfg, routes)
	s.HolderOperations, s.HolderBudgets = newMemHolder(), smallHolderBudgets()
	return f, s
}

// vmsTrusted is what a pending change breaks: the project's VM inventory (every sync starts with it) and a fenced
// write on the server, here the attach of its detached disk.
func vmsTrusted(s *Service, attach bool) error {
	ctx := context.Background()
	if _, err := s.ServerSyncProvider("pve", "p1", nil).List(ctx); err != nil {
		return fmt.Errorf("inventory: %w", err)
	}
	if attach {
		if _, err := s.AttachVolume(ctx, "p1", 101, "z:vm-101-disk-1", ""); err != nil {
			return fmt.Errorf("attach: %w", err)
		}
	}
	return nil
}

func pendingPage() []ClusterResource {
	return []ClusterResource{
		{Type: "qemu", VMID: 101, Name: "web", Node: "n1", Pool: "pc-p1", Status: "running"},
		{Type: "qemu", VMID: 102, Name: "db", Node: "n1", Pool: "pc-p1", Status: "stopped"},
		{Type: "qemu", VMID: 103, Name: "image", Node: "n1", Pool: "pc-p1", Status: "stopped", Template: 1},
		{Type: "qemu", VMID: 104, Name: "c1-default-n1-abc", Node: "n1", Pool: "pc-p1", Status: "running", Tags: WorkerTagPrefix + "c1"},
		{Type: "qemu", VMID: 105, Name: "c1-default-n1-def", Node: "n1", Pool: "pc-p1", Status: "stopped", Tags: WorkerTagPrefix + "c1"},
	}
}

// The sync's pass writes a stopped server and a template once, where the change lands at once, and never a running
// VM or a Kubernetes worker. After it no VM has a pending change, so the inventory and a fenced attach on the running
// server still pass.
func TestShowGraphicalDisplayLeavesNoPendingChange(t *testing.T) {
	ctx := context.Background()
	f, s := newPendingCluster(t, pendingPage())
	f.cfg[101]["unused0"] = "z:vm-101-disk-1"
	for pass := 1; pass <= 2; pass++ {
		f.writes = nil
		if err := s.ShowGraphicalDisplay(ctx, "p1"); err != nil {
			t.Fatal(err)
		}
		want := []string{"config 102 vga=std", "config 103 vga=std"}
		if pass == 2 {
			want = nil
		}
		if slices.Sort(f.writes); !slices.Equal(f.writes, want) {
			t.Fatalf("pass %d wrote %v, want %v", pass, f.writes, want)
		}
	}
	for id, want := range map[int]string{101: "serial0", 102: "std", 103: "std", 104: "serial0", 105: "serial0"} {
		if got := f.cfg[id].Str("vga"); got != want || len(f.pending[id]) != 0 || f.cfg[id].Str("serial0") != "socket" {
			t.Errorf("VM %d: vga %q (want %q), pending %v, serial0 %v", id, got, want, f.pending[id], f.cfg[id]["serial0"])
		}
	}
	if err := vmsTrusted(s, true); err != nil {
		t.Fatalf("after the pass: %v", err)
	}
}

// The control for the test above: the write the pass must not make. On a running VM it stays pending, and then the
// inventory and the fenced attach refuse; taking it back makes both pass again.
func TestPendingDisplayIsRefusedByTheStrictReaders(t *testing.T) {
	ctx := context.Background()
	f, s := newPendingCluster(t, pendingPage())
	f.cfg[101]["unused0"] = "z:vm-101-disk-1"
	if _, err := s.C.SetVMConfig(ctx, "n1", 101, map[string]string{"vga": "std"}); err != nil {
		t.Fatal(err)
	}
	if f.cfg[101].Str("vga") != "serial0" || f.pending[101].Str("vga") != "std" {
		t.Fatalf("the fake applied vga on a running VM: %v, pending %v", f.cfg[101], f.pending[101])
	}
	if err := vmsTrusted(s, false); err == nil {
		t.Fatal("the inventory accepted a VM with a pending change")
	}
	if _, err := s.AttachVolume(ctx, "p1", 101, "z:vm-101-disk-1", ""); err == nil {
		t.Fatal("a fenced attach accepted a VM with a pending change")
	}
	if _, err := s.C.SetVMConfig(ctx, "n1", 101, map[string]string{"revert": "vga"}); err != nil {
		t.Fatal(err)
	}
	if err := vmsTrusted(s, true); err != nil {
		t.Fatalf("after the revert: %v", err)
	}
}

// A start from the portal sets the display first, while the VM is stopped, then starts it: the VM boots with it and
// nothing is pending. A start of a VM that runs, or that has the display, writes no config.
func TestStartSetsTheGraphicalDisplayFirst(t *testing.T) {
	ctx := context.Background()
	f, s := newPendingCluster(t, pendingPage())
	if err := s.PowerAction(ctx, "p1", "102", "START"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"config 102 vga=std", "start 102"}; !slices.Equal(f.writes, want) {
		t.Fatalf("wrote %v, want %v", f.writes, want)
	}
	if f.cfg[102].Str("vga") != "std" || f.cfg[102].Str("serial0") != "socket" || len(f.pending[102]) != 0 {
		t.Fatalf("after the start: %v, pending %v", f.cfg[102], f.pending[102])
	}
	f.writes = nil
	for _, id := range []int{101, 102} { // 101 runs with a serial display, 102 has the graphical one
		if _, err := s.C.StartVM(ctx, "n1", id); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"start 101", "start 102"}; !slices.Equal(f.writes, want) {
		t.Fatalf("wrote %v, want %v", f.writes, want)
	}
	if err := vmsTrusted(s, false); err != nil {
		t.Fatalf("after the starts: %v", err)
	}
}

// A VM someone starts between the status read and the write: the change is pending, and the pass takes it back and
// says so, so that the next sync still trusts the VM.
func TestShowGraphicalTakesBackAPendingWrite(t *testing.T) {
	f, s := newPendingCluster(t, pendingPage())
	f.onConfig = func(id int) {
		if len(f.writes) == 0 {
			f.status[id] = "running"
		}
	}
	err := s.C.ShowGraphicalIfStopped(context.Background(), "n1", 102)
	if err == nil || !strings.Contains(err.Error(), "taken back") {
		t.Fatalf("err = %v", err)
	}
	if want := []string{"config 102 vga=std", "config 102 revert=vga"}; !slices.Equal(f.writes, want) {
		t.Fatalf("wrote %v, want %v", f.writes, want)
	}
	if f.cfg[102].Str("vga") != "serial0" || len(f.pending[102]) != 0 {
		t.Fatalf("after the revert: %v, pending %v", f.cfg[102], f.pending[102])
	}
}

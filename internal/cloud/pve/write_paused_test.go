package pve

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A VM paused by a suspension reads "paused". Stop and delete must still reach it: they are what a suspended tenant has left.
func TestStopAndDeleteReachAPausedVM(t *testing.T) {
	const upid = `{"data":"UPID:pve1:0:0:0:x:101:u:"}`
	routes := func(calls *[]string) fakeRoutes {
		note := func(name string) func(http.ResponseWriter, *http.Request) {
			return func(w http.ResponseWriter, _ *http.Request) {
				*calls = append(*calls, name)
				fmt.Fprint(w, upid)
			}
		}
		return fakeRoutes{
			"GET /api2/json/cluster/resources":                                say(`{"data":[{"type":"qemu","vmid":101,"node":"pve1","pool":"tp-p1","status":"paused","name":"a"}]}`),
			"GET /api2/json/nodes/pve1/qemu/101/status/current":               say(`{"data":{"status":"running","qmpstatus":"paused","name":"a"}}`),
			"POST /api2/json/nodes/pve1/qemu/101/status/shutdown":             note("shutdown"),
			"POST /api2/json/nodes/pve1/qemu/101/status/stop":                 note("stop"),
			"DELETE /api2/json/nodes/pve1/qemu/101":                           note("delete"),
			"GET /api2/json/nodes/pve1/tasks/UPID:pve1:0:0:0:x:101:u:/status": say(`{"data":{"status":"stopped","exitstatus":"OK"}}`),
		}
	}
	t.Run("stop", func(t *testing.T) {
		var calls []string
		_, c := fakePVE(t, routes(&calls))
		s := &Service{C: c, PoolPrefix: "tp-"}
		if err := s.PowerAction(context.Background(), "p1", "101", "STOP"); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(calls) != "[shutdown]" {
			t.Fatalf("calls %v", calls)
		}
	})
	t.Run("detach says to stop it first", func(t *testing.T) {
		var calls []string
		r := routes(&calls)
		r["GET /api2/json/nodes/pve1/qemu/101/config"] = say(`{"data":{"name":"a","scsi0":"zfs:vm-101-disk-0,size=10G","scsi1":"zfs:vm-101-disk-1,size=1G"}}`)
		_, c := fakePVE(t, r)
		s := &Service{C: c, PoolPrefix: "tp-"}
		err := s.DetachVolume(context.Background(), "p1", 101, "zfs:vm-101-disk-1")
		if !strings.Contains(fmt.Sprint(err), "Stop it first") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("delete", func(t *testing.T) {
		var calls []string
		_, c := fakePVE(t, routes(&calls))
		s := &Service{C: c, PoolPrefix: "tp-"}
		if err := s.DeleteServer(context.Background(), "p1", "101"); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(calls) != "[stop delete]" {
			t.Fatalf("calls %v", calls)
		}
	})
}

// What else a paused VM can meet once its project is up again, a resume having missed it: a config change needs it off first, and
// nothing brings it back running on its own.
func TestPausedVMNeedsStoppingBeforeItsConfigChanges(t *testing.T) {
	const upid = `{"data":"UPID:pve1:0:0:0:x:101:u:"}`
	routes := func(calls *[]string) fakeRoutes {
		note := func(name string) func(http.ResponseWriter, *http.Request) {
			return func(w http.ResponseWriter, _ *http.Request) {
				*calls = append(*calls, name)
				fmt.Fprint(w, upid)
			}
		}
		return fakeRoutes{
			"GET /api2/json/cluster/resources":                                say(`{"data":[{"type":"qemu","vmid":101,"node":"pve1","pool":"tp-p1","status":"running","name":"a"},{"type":"qemu","vmid":900,"node":"pve1","pool":"tp-p1","status":"stopped","name":"pc-holder-pve1","tags":"pc-holder;proxcloud"}]}`),
			"GET /api2/json/nodes/pve1/qemu/101/status/current":               say(`{"data":{"status":"running","qmpstatus":"paused","name":"a"}}`),
			"GET /api2/json/nodes/pve1/qemu/101/config":                       say(`{"data":{"name":"a","scsi0":"zfs:vm-101-disk-0,size=10G","scsi1":"zfs:vm-101-disk-1,size=1G"}}`),
			"GET /api2/json/nodes/pve1/qemu/900/config":                       say(`{"data":{"name":"pc-holder-pve1","tags":"pc-holder;proxcloud"}}`),
			"POST /api2/json/nodes/pve1/qemu/101/status/stop":                 note("stop"),
			"POST /api2/json/nodes/pve1/qemu/101/status/shutdown":             note("shutdown"),
			"POST /api2/json/nodes/pve1/qemu/101/status/start":                note("start"),
			"POST /api2/json/nodes/pve1/qemu/101/status/resume":               note("resume"),
			"POST /api2/json/nodes/pve1/qemu/101/config":                      note("config"),
			"POST /api2/json/nodes/pve1/qemu/101/move_disk":                   note("move"),
			"GET /api2/json/nodes/pve1/tasks/UPID:pve1:0:0:0:x:101:u:/status": say(`{"data":{"status":"stopped","exitstatus":"OK"}}`),
		}
	}
	run := func(t *testing.T, op func(s *Service) error) string {
		t.Helper()
		var calls []string
		_, c := fakePVE(t, routes(&calls))
		if err := op(&Service{C: c, PoolPrefix: "tp-"}); err != nil {
			t.Fatalf("%v, calls %v", err, calls)
		}
		return fmt.Sprint(calls)
	}
	t.Run("keeping its data volume on delete", func(t *testing.T) {
		got := run(t, func(s *Service) error { return s.KeepServerVolumes(context.Background(), "p1", "101") })
		if got != "[stop config move]" {
			t.Fatalf("calls %s", got)
		}
	})
	t.Run("resize stays off", func(t *testing.T) {
		got := run(t, func(s *Service) error {
			return s.ResizeServer(context.Background(), "p1", "101", ServerSpec{Slug: "s", VCPUs: 1, RAMMb: 512})
		})
		if got != "[shutdown config]" {
			t.Fatalf("calls %s", got)
		}
	})
	t.Run("start resumes it", func(t *testing.T) {
		got := run(t, func(s *Service) error { return s.PowerAction(context.Background(), "p1", "101", "START") })
		if got != "[resume]" {
			t.Fatalf("calls %s", got)
		}
	})
}

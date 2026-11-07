package pve

import (
	"context"
	"fmt"
	"net/http"
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
			"GET /api2/json/cluster/resources":                                say(`{"data":[{"type":"qemu","vmid":101,"node":"pve1","pool":"st-p1","status":"paused","name":"a"}]}`),
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
		s := &Service{C: c, PoolPrefix: "st-"}
		if err := s.PowerAction(context.Background(), "p1", "101", "STOP"); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(calls) != "[shutdown]" {
			t.Fatalf("calls %v", calls)
		}
	})
	t.Run("delete", func(t *testing.T) {
		var calls []string
		_, c := fakePVE(t, routes(&calls))
		s := &Service{C: c, PoolPrefix: "st-"}
		if err := s.DeleteServer(context.Background(), "p1", "101"); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(calls) != "[stop delete]" {
			t.Fatalf("calls %v", calls)
		}
	})
}

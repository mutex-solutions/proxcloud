package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// rebuildCluster fakes VM 101 on pve1 (pool tp-p1) with the given snapshots, a donor clone that gets
// vmid 200, and records every config write. moveFails makes the disk move fail.
func rebuildCluster(t *testing.T, snapshots string, moveFails bool, writes *[]url.Values, stopped *[]string) *Service {
	return rebuildClusterQMP(t, "running", snapshots, moveFails, writes, stopped)
}

func rebuildClusterQMP(t *testing.T, qmp, snapshots string, moveFails bool, writes *[]url.Values, stopped *[]string) *Service {
	const upid = "UPID:pve1:0:0:0:x:101:u:"
	task := func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"data":%q}`, upid) }
	cfg101 := `{"scsi0":"local-zfs:vm-101-disk-0,discard=on,size=8G","tags":"fl-a;img-old;proxcloud"}`
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/resources": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"type":"qemu","vmid":101,"node":"pve1","pool":"tp-p1","name":"web","status":"running"}]}`)
		},
		"GET /api2/json/nodes/pve1/qemu/101/status/current": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"data":{"status":"running","qmpstatus":%q,"name":"web"}}`, qmp)
		},
		"GET /api2/json/nodes/pve1/qemu/101/snapshot": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"data":[%s]}`, snapshots)
		},
		"GET /api2/json/nodes/pve1/qemu/101/config": func(w http.ResponseWriter, r *http.Request) {
			// After the detach the old disk sits in unused0, once the move is done the new one in scsi0.
			moved := false
			for _, f := range *writes {
				moved = moved || f.Get("__moved") != ""
			}
			if moved {
				fmt.Fprint(w, `{"data":{"scsi0":"local-zfs:vm-101-disk-1,size=4G","unused0":"local-zfs:vm-101-disk-0","tags":"fl-a;img-old;proxcloud"}}`)
				return
			}
			fmt.Fprintf(w, `{"data":%s}`, cfg101)
		},
		"GET /api2/json/nodes/pve1/qemu/200/config": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":{"scsi0":"local-zfs:vm-200-disk-0,size=4G"}}`)
		},
		"GET /api2/json/cluster/nextid": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":"200"}`) },
		"POST /api2/json/nodes/pve1/qemu/9000/clone": func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			if !strings.HasPrefix(r.PostForm.Get("name"), imageBuildPrefix) || r.PostForm.Get("storage") != "local-zfs" {
				t.Errorf("clone form %v", r.PostForm)
			}
			task(w, r)
		},
		"POST /api2/json/nodes/pve1/qemu/101/config": func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			*writes = append(*writes, r.PostForm)
			task(w, r)
		},
		"POST /api2/json/nodes/pve1/qemu/200/move_disk": func(w http.ResponseWriter, r *http.Request) {
			if moveFails {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			*writes = append(*writes, url.Values{"__moved": {"1"}})
			task(w, r)
		},
		"PUT /api2/json/nodes/pve1/qemu/101/resize": task,
		"POST /api2/json/nodes/pve1/qemu/101/status/stop": func(w http.ResponseWriter, r *http.Request) {
			*stopped = append(*stopped, "101")
			task(w, r)
		},
		"POST /api2/json/nodes/pve1/qemu/101/status/start": task,
		"POST /api2/json/nodes/pve1/qemu/200/status/stop": func(w http.ResponseWriter, r *http.Request) {
			*stopped = append(*stopped, "200")
			task(w, r)
		},
		"DELETE /api2/json/nodes/pve1/qemu/200": task,
		"GET /api2/json/nodes/pve1/tasks/" + upid + "/status": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":{"status":"stopped","exitstatus":"OK"}}`)
		},
	})
	return &Service{C: c, PoolPrefix: "tp-"}
}

func TestRebuildServer(t *testing.T) {
	req := RebuildReq{ProjectID: "p1", ServerID: "101", TemplateVMID: 9000, TemplateNode: "pve1", ImageSlug: "new"}
	current := `{"name":"current"}`

	t.Run("swaps the disk, retags, drops the old disk", func(t *testing.T) {
		var writes []url.Values
		var stopped []string
		s := rebuildCluster(t, current, false, &writes, &stopped)
		if err := s.RebuildServer(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		last := writes[len(writes)-1]
		if !strings.Contains(last.Get("tags"), "img-new") || strings.Contains(last.Get("tags"), "img-old") {
			t.Fatalf("tags %q", last.Get("tags"))
		}
		if last.Get("delete") != "unused0" {
			t.Fatalf("the old disk must go once the new one is in: %v", last)
		}
	})

	t.Run("a paused server stops before its disk is detached", func(t *testing.T) {
		var writes []url.Values
		var stopped []string
		s := rebuildClusterQMP(t, "paused", current, false, &writes, &stopped)
		if err := s.RebuildServer(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if len(stopped) == 0 || stopped[0] != "101" {
			t.Fatalf("stops %v", stopped)
		}
	})

	t.Run("a failed move puts the old disk back", func(t *testing.T) {
		var writes []url.Values
		var stopped []string
		s := rebuildCluster(t, current, true, &writes, &stopped)
		if err := s.RebuildServer(context.Background(), req); err == nil {
			t.Fatal("want the move's error")
		}
		last := writes[len(writes)-1]
		if last.Get("scsi0") != "local-zfs:vm-101-disk-0,discard=on,size=8G" {
			t.Fatalf("old disk not restored: %v", last)
		}
		if stopped[len(stopped)-1] != "200" {
			t.Fatalf("the donor must be destroyed: %v", stopped)
		}
	})

	t.Run("snapshots refuse it before anything happens", func(t *testing.T) {
		var writes []url.Values
		var stopped []string
		s := rebuildCluster(t, current+`,{"name":"s1"}`, false, &writes, &stopped)
		var ce *ConflictError
		if err := s.RebuildServer(context.Background(), req); !errors.As(err, &ce) {
			t.Fatalf("want a conflict, got %v", err)
		}
		if len(writes) != 0 || len(stopped) != 0 {
			t.Fatalf("nothing may happen: %v %v", writes, stopped)
		}
	})
}

func TestSnapshotGuards(t *testing.T) {
	var writes []url.Values
	var stopped []string
	s := rebuildCluster(t, `{"name":"current","parent":"s2"},{"name":"s2","snaptime":2},{"name":"s1","snaptime":1},{"name":"__replicate_x","snaptime":3}`, false, &writes, &stopped)
	ctx := context.Background()

	got, err := s.ListServerSnapshots(ctx, "p1", "101")
	if err != nil || len(got) != 2 || got[0].Name != "s2" || !got[0].Latest || got[1].Latest {
		t.Fatalf("list %v %v", got, err)
	}
	var ce *ConflictError
	if err := s.RollbackServerSnapshot(ctx, "p1", "101", "s1", func(st string) bool { return st == "local-zfs" }); !errors.As(err, &ce) {
		t.Fatalf("older snapshot on ZFS must be refused, got %v", err)
	}
	if err := s.CreateServerSnapshot(ctx, "p1", "101", "s3", "", func(string) bool { return false }); !errors.As(err, &ce) || !strings.Contains(ce.Msg, "no snapshots") {
		t.Fatalf("a storage without snapshots must refuse, got %v", err)
	}
	if err := s.DeleteServerSnapshot(ctx, "p1", "101", "__replicate_x"); !errors.As(err, &ce) {
		t.Fatalf("a replication snapshot must be refused, got %v", err)
	}
	for _, bad := range []string{"", "1a", "a", "current", "__x", "vzdump", "has space"} {
		if ValidSnapshotName(bad) {
			t.Fatalf("%q must be invalid", bad)
		}
	}
	if !ValidSnapshotName("before-upgrade_1") {
		t.Fatal("a plain name must be valid")
	}
}

func TestSubnetHolds(t *testing.T) {
	for _, c := range []struct {
		cidr, within string
		want         bool
	}{
		{"10.0.1.0/24", "10.0.1.0/24", true},
		{"10.0.1.0/24", "10.0.1.9", true},
		{"10.0.1.0/24", "10.0.2.9", false},
		{"10.0.1.0/24", "10.0.2.0/24", false},
	} {
		if got := SubnetHolds(c.cidr, c.within); got != c.want {
			t.Fatalf("SubnetHolds(%s, %s) = %v", c.cidr, c.within, got)
		}
	}
}

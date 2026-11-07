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

// backupCluster fakes VM 101 on pve1 (pool st-p1) with one archive of p1 and one of another project on nas-backup.
type backupCluster struct {
	s        *Service
	vzdump   url.Values
	restored url.Values
	deleted  []string
	stopped  int
	started  int
	cfg      string
	archive  string
	deny     string // the route answering 403
}

func newBackupCluster(t *testing.T) *backupCluster {
	const upid = "UPID:pve1:0:0:0:x:101:u:"
	b := &backupCluster{
		cfg:     `{"scsi0":"z:vm-101-disk-0,size=8G","net0":"virtio=AA:BB,bridge=v1,firewall=1","ide2":"z:vm-101-cloudinit,media=cdrom"}`,
		archive: "scsi0: z:vm-101-disk-0,size=8G\nnet0: virtio=AA:BB,bridge=v1,firewall=1\n#qmdump#map:scsi0:drive-scsi0:z:raw:\n\n[snap]\nscsi5: z:old\n",
	}
	task := func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintf(w, `{"data":%q}`, upid) }
	guard := func(route string, next func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			if b.deny == route {
				http.Error(w, "Permission check failed", http.StatusForbidden)
				return
			}
			next(w, r)
		}
	}
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/resources": func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":[{"type":"qemu","vmid":101,"node":"pve1","pool":"st-p1","name":"web","status":"running"}]}`)
		},
		"GET /api2/json/nodes/pve1/qemu/101/status/current": func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":{"status":"running","name":"web"}}`)
		},
		"GET /api2/json/nodes/pve1/qemu/101/config": func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"data":%s}`, b.cfg)
		},
		"GET /api2/json/nodes/pve1/storage/nas-backup/content": func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":[
				{"volid":"nas-backup:backup/old","ctime":10,"size":5,"notes":"proxcloud project=p1 server=web"},
				{"volid":"nas-backup:backup/new","ctime":20,"size":6,"notes":"proxcloud project=p1 server=web"},
				{"volid":"nas-backup:backup/theirs","ctime":30,"size":7,"notes":"proxcloud project=p11 server=web"},
				{"volid":"nas-backup:backup/manual","ctime":40,"size":8}]}`)
		},
		"POST /api2/json/nodes/pve1/vzdump": guard("vzdump", func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			b.vzdump = r.PostForm
			task(w, r)
		}),
		"GET /api2/json/nodes/pve1/vzdump/extractconfig": guard("extract", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"data":%q}`, b.archive)
		}),
		"POST /api2/json/nodes/pve1/qemu": guard("restore", func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			b.restored = r.PostForm
			task(w, r)
		}),
		"POST /api2/json/nodes/pve1/qemu/101/status/stop": func(w http.ResponseWriter, r *http.Request) {
			b.stopped++
			task(w, r)
		},
		"POST /api2/json/nodes/pve1/qemu/101/status/start": func(w http.ResponseWriter, r *http.Request) {
			b.started++
			task(w, r)
		},
		"DELETE /api2/json/nodes/pve1/storage/nas-backup/content/nas-backup:backup/old": guard("delete", func(w http.ResponseWriter, r *http.Request) {
			b.deleted = append(b.deleted, "old")
			task(w, r)
		}),
		"DELETE /api2/json/nodes/pve1/storage/nas-backup/content/nas-backup:backup/new": func(w http.ResponseWriter, r *http.Request) {
			b.deleted = append(b.deleted, "new")
			task(w, r)
		},
		"GET /api2/json/nodes": func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":[{"node":"pve2","status":"offline"},{"node":"pve1","status":"online"}]}`)
		},
		"GET /api2/json/nodes/pve1/tasks/" + upid + "/status": func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":{"status":"stopped","exitstatus":"OK"}}`)
		},
	})
	b.s = &Service{C: c, ServiceID: "svc", PoolPrefix: "st-"}
	return b
}

func wantConflict(t *testing.T, err error, part string) {
	t.Helper()
	var ce *ConflictError
	if !errors.As(err, &ce) || !strings.Contains(ce.Msg, part) {
		t.Fatalf("want a conflict with %q, got %v", part, err)
	}
}

var goodStore = StorageCapability{Shared: true, Content: []string{"backup"}, Nodes: map[string]StorageNodeState{"pve1": {Active: true, Total: 100 << 30, Avail: 90 << 30}}}

func TestBackupOwnership(t *testing.T) {
	b := newBackupCluster(t)
	ctx := context.Background()
	got, err := b.s.ListServerBackups(ctx, "p1", "101", "nas-backup")
	if err != nil || len(got) != 2 || got[0].ID != "nas-backup:backup/new" || got[1].SizeBytes != 5 {
		t.Fatalf("only this project's archives, newest first: %+v %v", got, err)
	}
	for _, id := range []string{"nas-backup:backup/theirs", "nas-backup:backup/manual", "nas-backup:backup/none", ""} {
		wantConflict(t, b.s.DeleteServerBackup(ctx, "p1", "101", "nas-backup", id), "no backup")
		wantConflict(t, b.s.RestoreServerBackup(ctx, "p1", "101", "nas-backup", id), "no backup")
	}
	if b.stopped != 0 || b.restored != nil || len(b.deleted) != 0 {
		t.Fatalf("a foreign archive must touch nothing: %+v", b)
	}
	// A VM in another project's pool does not resolve at all.
	if _, err := b.s.ListServerBackups(ctx, "p2", "101", "nas-backup"); err == nil {
		t.Fatal("another project's server must not resolve")
	}
	if err := b.s.CreateServerBackup(ctx, "p2", "101", "nas-backup", goodStore, 10); err == nil || b.vzdump != nil {
		t.Fatalf("another project's server must not be backed up: %v", err)
	}
	if err := b.s.DeleteServerBackup(ctx, "p2", "101", "nas-backup", "nas-backup:backup/old"); err == nil || len(b.deleted) != 0 {
		t.Fatalf("another project's archive must not go: %v", err)
	}
}

func TestCreateServerBackup(t *testing.T) {
	b := newBackupCluster(t)
	ctx := context.Background()
	if err := b.s.CreateServerBackup(ctx, "p1", "101", "nas-backup", goodStore, 10); err != nil {
		t.Fatal(err)
	}
	if b.vzdump.Get("notes-template") != "proxcloud project=p1 server={{guestname}}" || b.vzdump.Get("storage") != "nas-backup" || b.vzdump.Get("vmid") != "101" {
		t.Fatalf("%v", b.vzdump)
	}
	if b.vzdump.Get("remove") != "0" || b.vzdump.Has("prune-backups") {
		t.Fatalf("the store's retention must not touch tenant archives: %v", b.vzdump)
	}
	b.vzdump = nil
	wantConflict(t, b.s.CreateServerBackup(ctx, "p1", "101", "nas-backup", StorageCapability{Content: []string{"backup"}}, 10), "shared")
	wantConflict(t, b.s.CreateServerBackup(ctx, "p1", "101", "nas-backup", StorageCapability{Shared: true, Content: []string{"images"}}, 10), "backup content")
	full := goodStore
	full.Nodes = map[string]StorageNodeState{"pve1": {Active: true, Total: 100 << 30, Avail: 12 << 30}}
	wantConflict(t, b.s.CreateServerBackup(ctx, "p1", "101", "nas-backup", full, 10), "no room")
	if b.vzdump != nil {
		t.Fatalf("a refused backup must not start: %v", b.vzdump)
	}
	b.deny = "vzdump"
	wantConflict(t, b.s.CreateServerBackup(ctx, "p1", "101", "nas-backup", goodStore, 10), "VM.Backup")
}

func TestRestoreServerBackup(t *testing.T) {
	b := newBackupCluster(t)
	ctx := context.Background()
	if err := b.s.RestoreServerBackup(ctx, "p1", "101", "nas-backup", "nas-backup:backup/old"); err != nil {
		t.Fatal(err)
	}
	if b.restored.Get("archive") != "nas-backup:backup/old" || b.restored.Get("force") != "1" || b.restored.Get("vmid") != "101" {
		t.Fatalf("%v", b.restored)
	}
	if b.stopped != 1 || b.started != 1 {
		t.Fatalf("a running server comes back running: stop %d start %d", b.stopped, b.started)
	}

	// Without the privilege: refused at the config read, and the server was not even stopped.
	b = newBackupCluster(t)
	b.deny = "extract"
	wantConflict(t, b.s.RestoreServerBackup(ctx, "p1", "101", "nas-backup", "nas-backup:backup/old"), "VM.Backup")
	if b.stopped != 0 || b.restored != nil {
		t.Fatalf("nothing may change: %+v", b)
	}

	// Refused by the restore itself: the server goes back up.
	b = newBackupCluster(t)
	b.deny = "restore"
	wantConflict(t, b.s.RestoreServerBackup(ctx, "p1", "101", "nas-backup", "nas-backup:backup/old"), "VM.Backup")
	if b.stopped != 1 || b.started != 1 {
		t.Fatalf("stop %d start %d", b.stopped, b.started)
	}

	// A disk the archive lacks would be destroyed.
	b = newBackupCluster(t)
	b.cfg = `{"scsi0":"z:vm-101-disk-0,size=8G","scsi1":"z:vm-101-disk-1,size=4G","net0":"virtio=AA:BB,bridge=v1"}`
	wantConflict(t, b.s.RestoreServerBackup(ctx, "p1", "101", "nas-backup", "nas-backup:backup/old"), "scsi1")
	b.cfg = `{"scsi0":"z:vm-101-disk-0,size=8G","unused0":"z:vm-101-disk-1","net0":"virtio=AA:BB,bridge=v1"}`
	wantConflict(t, b.s.RestoreServerBackup(ctx, "p1", "101", "nas-backup", "nas-backup:backup/old"), "unused0")
	b.cfg = `{"scsi0":"z:vm-101-disk-0,size=8G","net0":"virtio=CC:DD,bridge=v1"}`
	wantConflict(t, b.s.RestoreServerBackup(ctx, "p1", "101", "nas-backup", "nas-backup:backup/old"), "network interfaces")
	if b.stopped != 0 || b.restored != nil {
		t.Fatalf("a refused restore changes nothing: %+v", b)
	}

	b = newBackupCluster(t)
	if cfg := parseVMConf(b.archive); cfg["scsi5"] != nil || cfg["scsi0"] == nil {
		t.Fatalf("a snapshot's section is not the current config: %+v", cfg)
	}
}

func TestDeleteBackups(t *testing.T) {
	b := newBackupCluster(t)
	ctx := context.Background()
	if err := b.s.DeleteServerBackups(ctx, "p1", "101", "nas-backup"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(b.deleted, ",") != "new,old" {
		t.Fatalf("every archive of this project and no other: %v", b.deleted)
	}
	b = newBackupCluster(t)
	if err := b.s.DeleteProjectBackups(ctx, "p1", "nas-backup"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(b.deleted, ",") != "new,old" {
		t.Fatalf("teardown: %v", b.deleted)
	}
	// A failure stops it: the server delete and the teardown must not go on past an archive left behind.
	b = newBackupCluster(t)
	b.deny = "delete"
	wantConflict(t, b.s.DeleteServerBackups(ctx, "p1", "101", "nas-backup"), "VM.Backup")
	if err := b.s.DeleteProjectBackups(ctx, "p1", "nas-backup"); err == nil {
		t.Fatal("teardown must report what it could not remove")
	}
}

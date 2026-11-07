package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// heldVolumes is a guard over the fake VM's volumes: on the server since before any archive.
func heldVolumes(context.Context, VMConfig, VMConfig) (map[string]RestoreVolume, error) {
	return map[string]RestoreVolume{
		"z:vm-101-disk-0": {Name: "web-scsi0", Since: time.Unix(1, 0)},
		"z:vm-101-disk-1": {Name: "web-scsi1", Since: time.Unix(1, 0)},
	}, nil
}

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
	// destroyedAfter is how many archives were gone when the VM was destroyed, -1 while it stands.
	destroyedAfter int
}

func newBackupCluster(t *testing.T) *backupCluster {
	const upid = "UPID:pve1:0:0:0:x:101:u:"
	b := &backupCluster{
		destroyedAfter: -1,
		cfg:            `{"scsi0":"z:vm-101-disk-0,size=8G","net0":"virtio=AA:BB,bridge=v1,firewall=1","ide2":"z:vm-101-cloudinit,media=cdrom"}`,
		archive:        "scsi0: z:vm-101-disk-0,size=8G\nnet0: virtio=AA:BB,bridge=v1,firewall=1\n#qmdump#map:scsi0:drive-scsi0:z:raw:\n\n[snap]\nscsi5: z:old\n",
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
		"DELETE /api2/json/nodes/pve1/qemu/101": func(w http.ResponseWriter, r *http.Request) {
			b.destroyedAfter = len(b.deleted)
			task(w, r)
		},
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
		_, err := b.s.RestoreServerBackup(ctx, "p1", "101", "nas-backup", id, heldVolumes)
		wantConflict(t, err, "no backup")
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

func wantRestoreRefused(t *testing.T, b *backupCluster, part string) {
	t.Helper()
	_, err := b.s.RestoreServerBackup(context.Background(), "p1", "101", "nas-backup", "nas-backup:backup/old", heldVolumes)
	wantConflict(t, err, part)
}

func TestRestoreServerBackup(t *testing.T) {
	b := newBackupCluster(t)
	ctx := context.Background()
	at, err := b.s.RestoreServerBackup(ctx, "p1", "101", "nas-backup", "nas-backup:backup/old", heldVolumes)
	if err != nil || at.Unix() != 10 {
		t.Fatal(at, err)
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
	wantRestoreRefused(t, b, "VM.Backup")
	if b.stopped != 0 || b.restored != nil {
		t.Fatalf("nothing may change: %+v", b)
	}

	// Refused by the restore itself: the server goes back up.
	b = newBackupCluster(t)
	b.deny = "restore"
	wantRestoreRefused(t, b, "VM.Backup")
	if b.stopped != 1 || b.started != 1 {
		t.Fatalf("stop %d start %d", b.stopped, b.started)
	}

	// A disk the archive lacks would be destroyed.
	b = newBackupCluster(t)
	b.cfg = `{"scsi0":"z:vm-101-disk-0,size=8G","scsi1":"z:vm-101-disk-1,size=4G","net0":"virtio=AA:BB,bridge=v1"}`
	wantRestoreRefused(t, b, "web-scsi1")
	b.cfg = `{"scsi0":"z:vm-101-disk-0,size=8G","unused0":"z:vm-101-disk-1","net0":"virtio=AA:BB,bridge=v1"}`
	wantRestoreRefused(t, b, "web-scsi1")
	b.cfg = `{"scsi0":"z:vm-101-disk-0,size=8G","net0":"virtio=CC:DD,bridge=v1"}`
	wantRestoreRefused(t, b, "network interfaces")
	if b.stopped != 0 || b.restored != nil {
		t.Fatalf("a refused restore changes nothing: %+v", b)
	}

	b = newBackupCluster(t)
	if cfg := parseVMConf(b.archive); cfg["scsi5"] != nil || cfg["scsi0"] == nil {
		t.Fatalf("a snapshot's section is not the current config: %+v", cfg)
	}
}

// The archive has scsi1=V1. V1 left, and V2 took its slot: all the forms of that must refuse by name.
func TestRestoreConflictVolumeIdentity(t *testing.T) {
	const archivedAt = 1000
	before, after := time.Unix(archivedAt-5, 0), time.Unix(archivedAt+5, 0)
	archive := parseVMConf("scsi0: z:vm-101-disk-0,size=8G\nscsi1: z:vm-101-disk-1,size=4G\nscsi2: z:vm-101-disk-2,backup=0,size=4G\n")
	boot := map[string]any{"scsi0": "z:vm-101-disk-0,size=8G"}
	with := func(kv ...string) VMConfig {
		cfg := VMConfig{}
		for k, v := range boot {
			cfg[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			cfg[kv[i]] = kv[i+1]
		}
		return cfg
	}
	for _, tt := range []struct {
		name string
		cur  VMConfig
		vols map[string]RestoreVolume
		want string // part of the refusal, "" = allowed
	}{
		{"same volume, row older than the backup", with("scsi1", "z:vm-101-disk-1,size=4G"),
			map[string]RestoreVolume{"z:vm-101-disk-1": {Name: "web-data", Since: before}}, ""},
		{"same volume in another slot", with("scsi7", "z:vm-101-disk-1,size=4G"),
			map[string]RestoreVolume{"z:vm-101-disk-1": {Name: "web-data", Since: before}}, ""},
		{"other volid on the archive's slot", with("scsi1", "z:vm-101-disk-3,size=4G"),
			map[string]RestoreVolume{"z:vm-101-disk-3": {Name: "new-data", Since: before}}, "new-data"},
		{"PVE reused the name for a disk made after the backup", with("scsi1", "z:vm-101-disk-1,size=4G"),
			map[string]RestoreVolume{"z:vm-101-disk-1": {Name: "new-data", Since: after}}, "new-data"},
		{"a disk the portal has no row for", with("scsi1", "z:vm-101-disk-1,size=4G"), nil, "scsi1"},
		{"a disk with backup=0 is not in the archive", with("scsi2", "z:vm-101-disk-2,size=4G"),
			map[string]RestoreVolume{"z:vm-101-disk-2": {Name: "scratch", Since: before}}, "scratch"},
		{"a detached disk stays refused", with("unused0", "z:vm-101-disk-1"),
			map[string]RestoreVolume{"z:vm-101-disk-1": {Name: "web-data", Since: before}}, "web-data"},
		{"the boot disk rolls back, even rebuilt", VMConfig{"scsi0": "z:vm-101-disk-9,size=8G"}, nil, ""},
		{"a boot disk the archive lacks", with("virtio0", "z:vm-101-disk-8,size=8G"), nil, "virtio0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := restoreConflict(tt.cur, archive, archivedAt, tt.vols)
			if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRestoreGuard(t *testing.T) {
	ctx := context.Background()
	// A quota refusal arrives as is, with nothing stopped.
	b := newBackupCluster(t)
	refused := errors.New("quota")
	_, err := b.s.RestoreServerBackup(ctx, "p1", "101", "nas-backup", "nas-backup:backup/old", func(context.Context, VMConfig, VMConfig) (map[string]RestoreVolume, error) {
		return nil, refused
	})
	if !errors.Is(err, refused) || b.stopped != 0 || b.restored != nil {
		t.Fatalf("%v stopped=%d restored=%v", err, b.stopped, b.restored)
	}
	// What changed while the server stopped refuses too, and the server comes back.
	b = newBackupCluster(t)
	calls := 0
	_, err = b.s.RestoreServerBackup(ctx, "p1", "101", "nas-backup", "nas-backup:backup/old", func(ctx context.Context, cur, a VMConfig) (map[string]RestoreVolume, error) {
		if calls++; calls == 2 {
			return nil, refused
		}
		return heldVolumes(ctx, cur, a)
	})
	if !errors.Is(err, refused) || b.restored != nil || b.stopped != 1 || b.started != 1 {
		t.Fatalf("%v restored=%v stop %d start %d", err, b.restored, b.stopped, b.started)
	}
}

func TestRestoreDemand(t *testing.T) {
	cur := VMConfig{"cores": float64(2), "memory": "2048", "scsi0": "z:a,size=8G", "scsi1": "z:b,size=4G"}
	archive := parseVMConf("cores: 4\nsockets: 2\nmemory: current=8192\nscsi0: z:a,size=8G\nscsi1: z:c,size=20G\nscsi2: z:d,backup=0,size=100G\nunused0: z:e\n")
	if v, r, d := RestoreDemand(cur, archive); v != 6 || r != 6144 || d != 16 {
		t.Fatalf("vcpus %d ram %d disk %d", v, r, d)
	}
	if v, r, d := RestoreDemand(archive, cur); v != -6 || r != -6144 || d != -116 {
		t.Fatalf("a smaller archive frees: vcpus %d ram %d disk %d", v, r, d)
	}
}

// Whatever refuses leaves the archives in place, and the destroy comes after the last of them.
func TestRetireServer(t *testing.T) {
	ctx := context.Background()
	b := newBackupCluster(t)
	if err := b.s.RetireServer(ctx, "p1", "101", "nas-backup", true); err != nil {
		t.Fatal(err)
	}
	if b.destroyedAfter != 2 {
		t.Fatalf("archives gone at the destroy: %d", b.destroyedAfter)
	}
	const (
		noMoveOff = `{"scsi0":"z:vm-101-disk-0,size=8G","scsi1":"z:vm-101-disk-1,size=4G"}` // moving the disk finds no route
		protected = `{"scsi0":"z:vm-101-disk-0,size=8G","tags":"pc-protected"}`             // removing HA finds no route
		locked    = `{"scsi0":"z:vm-101-disk-0,size=8G","lock":"backup"}`
	)
	for name, cfg := range map[string]string{"volume move": noMoveOff, "protection": protected, "lock": locked} {
		t.Run(name, func(t *testing.T) {
			b := newBackupCluster(t)
			b.cfg = cfg
			if err := b.s.RetireServer(ctx, "p1", "101", "nas-backup", true); err == nil {
				t.Fatal("must refuse")
			}
			if len(b.deleted) != 0 || b.destroyedAfter != -1 {
				t.Fatalf("archives %v destroyed %d", b.deleted, b.destroyedAfter)
			}
		})
	}
}

// While a restore runs the VM has no disks in its config. Their rows must wait, or the bill stops and the row's age is lost.
func TestVolumeRowsSurviveARestore(t *testing.T) {
	for _, cfg := range []string{`{"lock":"create","name":"web"}`, `{"memory":"128"}`} {
		b := newBackupCluster(t)
		b.cfg = cfg
		p := b.s.VolumeSyncProvider("r", "p1", nil)
		rows, err := p.List(context.Background())
		if err != nil || len(rows) != 0 {
			t.Fatalf("%v %v", rows, err)
		}
		row := &cloud.CloudResource{Data: map[string]any{"volume": map[string]any{"vmid": 101}}}
		if keep, _ := p.KeepUnread(row); !keep {
			t.Fatalf("the row of a VM being restored must stay: %s", cfg)
		}
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

//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
)

// rowsPVE fakes the pool pc-p1 on one node: each VM is its config's disk entries (slot → value). A disk reassign
// (move_disk with target-vmid) takes the disk out of its slot and puts it into the target's under name moveTo,
// unless moveFails.
type rowsPVE struct {
	mu        sync.Mutex
	vms       map[int]map[string]string
	moveTo    string
	moveFails bool
}

func (f *rowsPVE) set(vms map[int]map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vms = vms
}

func (f *rowsPVE) service(t *testing.T) *pve.Service {
	t.Helper()
	const upid = "UPID:pve1:0:0:0:qmmove:101:u:"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		ids := make([]int, 0, len(f.vms))
		for id := range f.vms {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")
		parts := strings.Split(strings.Trim(path, "/"), "/")
		data := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
		switch {
		case path == "/cluster/resources" && r.URL.Query().Get("type") == "storage":
			data([]map[string]any{{"type": "storage", "storage": "local-zfs", "node": "pve1", "status": "available", "maxdisk": 1 << 40, "disk": 0}})
		case path == "/cluster/resources":
			out := []map[string]any{}
			for _, id := range ids {
				out = append(out, map[string]any{"id": "qemu/" + strconv.Itoa(id), "type": "qemu", "vmid": id, "name": "srv" + strconv.Itoa(id), "node": "pve1",
					"pool": "pc-p1", "status": "stopped", "maxcpu": 1, "maxmem": 1 << 30, "maxdisk": 10 << 30})
			}
			data(out)
		case path == "/cluster/status":
			data([]map[string]any{{"type": "node", "name": "pve1", "online": 1, "ip": "192.0.2.11"}})
		case path == "/nodes":
			data([]map[string]any{{"node": "pve1", "status": "online"}})
		case path == "/pools/pc-p1":
			members := []map[string]any{}
			for _, id := range ids {
				members = append(members, map[string]any{"id": "qemu/" + strconv.Itoa(id), "type": "qemu", "vmid": id, "node": "pve1"})
			}
			data(map[string]any{"poolid": "pc-p1", "members": members})
		case path == "/access/permissions":
			data(map[string]any{r.URL.Query().Get("path"): map[string]int{"VM.Audit": 1, "Sys.Audit": 1}})
		case path == "/storage":
			data([]map[string]any{{"storage": "local-zfs", "type": "zfspool", "content": "images"}})
		case len(parts) == 6 && parts[2] == "qemu" && parts[4] == "status":
			data(map[string]any{"status": "stopped"})
		case len(parts) == 5 && parts[2] == "qemu" && parts[4] == "pending":
			data([]any{})
		case len(parts) == 5 && parts[2] == "qemu" && parts[4] == "snapshot":
			data([]map[string]any{{"name": "current", "description": "You are here!"}})
		case len(parts) == 5 && parts[2] == "qemu" && parts[4] == "config":
			id, _ := strconv.Atoi(parts[3])
			vm, ok := f.vms[id]
			if !ok {
				http.Error(w, "no such VM", http.StatusInternalServerError)
				return
			}
			cfg := map[string]any{"name": "srv" + parts[3], "smbios1": fmt.Sprintf("uuid=11111111-1111-4111-8111-%012d", id), "boot": "order=scsi0"}
			for slot, v := range vm {
				cfg[slot] = v
			}
			data(cfg)
		case r.Method == http.MethodPost && len(parts) == 5 && parts[4] == "move_disk":
			if f.moveFails {
				http.Error(w, "storage migration failed", http.StatusInternalServerError)
				return
			}
			_ = r.ParseForm()
			id, _ := strconv.Atoi(parts[3])
			target, _ := strconv.Atoi(r.Form.Get("target-vmid"))
			_, size, _ := strings.Cut(f.vms[id][r.Form.Get("disk")], ",")
			delete(f.vms[id], r.Form.Get("disk"))
			f.vms[target][r.Form.Get("target-disk")] = f.moveTo + "," + size
			data(upid)
		case strings.HasSuffix(path, "/status") && strings.Contains(path, "/tasks/"):
			data(map[string]any{"status": "stopped", "exitstatus": "OK"})
		default:
			t.Logf("rowsPVE: no fake for %s %s", r.Method, r.URL)
			http.Error(w, "no fake", http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

type volRow struct {
	volID, name string
	size        int
	vmid        int
}

// The actions that replace, drop or resize a server's disk (REBUILD, RESTORE_BACKUP, ROLLBACK_SNAPSHOT, RESIZE) end
// with ReconcileVolumeRows. The inline sync they ran before only added and updated rows, so on a site without the
// scheduled sync the row of the destroyed disk stayed: the production acceptance showed two boot volume rows after a
// rebuild and the quota counting 20 GB twice (D3). Each case is the cluster as that action leaves it.
func TestVolumeRowsEqualDisksAfterADiskIsReplaced(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := cloud.NewRepo(db)
	h := project.NewHandler(project.NewService(project.NewRepo(db), nil, nil, nil, nil), nil, nil, nil, nil, nil, repo, nil, nil, nil, "r")
	es := &externalservice.ExternalService{ID: "svc"}
	proj := &project.Project{ID: "p1"}
	const boot, data = "local-zfs:vm-101-disk-0", "local-zfs:vm-101-disk-1"
	before := map[string]string{"scsi0": boot + ",size=20G", "scsi1": data + ",size=30G"}

	for _, tt := range []struct {
		name  string
		after map[string]string
		want  map[string]int // volid → GB
		gone  []string
	}{
		// The boot disk is a new one under the next free name; the old one is destroyed.
		{"rebuild", map[string]string{"scsi0": "local-zfs:vm-101-disk-2,size=20G", "scsi1": data + ",size=30G"},
			map[string]int{"local-zfs:vm-101-disk-2": 20, data: 30}, []string{boot}},
		// An archive taken before the data disk was added: the restore destroyed it.
		{"restore", map[string]string{"scsi0": boot + ",size=20G"}, map[string]int{boot: 20}, []string{data}},
		// The snapshot's config is back: the data disk has the size it had then.
		{"rollback", map[string]string{"scsi0": boot + ",size=20G", "scsi1": data + ",size=10G"}, map[string]int{boot: 20, data: 10}, nil},
		// A larger flavor grew the boot disk.
		{"resize", map[string]string{"scsi0": boot + ",size=40G", "scsi1": data + ",size=30G"}, map[string]int{boot: 40, data: 30}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, c := range []string{"cloudResource", "cloudResourceHistory"} {
				if _, err := db.C(c).DeleteMany(ctx, pgdoc.M{}); err != nil {
					t.Fatal(err)
				}
			}
			f := &rowsPVE{}
			ps := f.service(t)
			f.set(map[int]map[string]string{101: before})
			h.ReconcileVolumeRows(ctx, ps, es, proj)
			rows, err := repo.FindByServiceProjectAndType(ctx, "svc", "p1", cloud.TypeVolume)
			if err != nil || len(rows) != 2 {
				t.Fatalf("rows before: %+v, err %v", rows, err)
			}
			ids := map[string]string{}
			for _, r := range rows {
				ids[r.ExternalID] = r.ID
			}

			f.set(map[int]map[string]string{101: tt.after})
			h.ReconcileVolumeRows(ctx, ps, es, proj)

			rows, err = repo.FindByServiceProjectAndType(ctx, "svc", "p1", cloud.TypeVolume)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]int{}
			for _, r := range rows {
				vol, _ := r.Data["volume"].(map[string]any)
				got[r.ExternalID] = int(asFloat(vol["size"]))
				if id, was := ids[r.ExternalID]; was && id != r.ID {
					t.Errorf("the row of %s, a disk that stayed, changed its id", r.ExternalID)
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("rows = %v, want the disks %v", got, tt.want)
			}
			for _, volID := range tt.gone {
				if n, err := db.C("cloudResourceHistory").Count(ctx, pgdoc.M{"cloudResourceId": ids[volID]}); err != nil || n != 1 {
					t.Errorf("the row of %s must be archived once: %d, %v", volID, n, err)
				}
			}
		})
	}
}

func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return -1
}

// A tenant's volume attached to another server gets a new volid from Proxmox. Its row was archived and the sync
// inserted one for the new volid: the volume lost its name ("t-vol1" became "t-srv2-scsi1", then "t-srv2-unused0"),
// its id and its creation date, and its charge was split over two bill items (production acceptance, D4). The row
// follows the disk, as a retype's does.
func TestAttachKeepsTheVolumeRow(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := cloud.NewRepo(db)
	h := project.NewHandler(project.NewService(project.NewRepo(db), nil, nil, nil, nil), nil, nil, nil, nil, nil, repo, nil, nil, nil, "r")
	es := &externalservice.ExternalService{ID: "svc"}
	proj := &project.Project{ID: "p1"}
	const oldVol, newVol = "local-zfs:vm-101-disk-1", "local-zfs:vm-102-disk-1"

	seed := func(t *testing.T, f *rowsPVE) (*pve.Service, *cloud.CloudResource) {
		t.Helper()
		for _, c := range []string{"cloudResource", "cloudResourceHistory"} {
			if _, err := db.C(c).DeleteMany(ctx, pgdoc.M{}); err != nil {
				t.Fatal(err)
			}
		}
		ps := f.service(t)
		f.set(map[int]map[string]string{
			101: {"scsi0": "local-zfs:vm-101-disk-0,size=10G", "unused0": oldVol + ",size=5G"},
			102: {"scsi0": "local-zfs:vm-102-disk-0,size=10G"},
		})
		h.ReconcileVolumeRows(ctx, ps, es, proj)
		row, err := repo.FindByServiceIDAndExternalID(ctx, "svc", oldVol)
		if err != nil || row == nil {
			t.Fatalf("the volume's row: %+v, %v", row, err)
		}
		// The name the tenant gave it, and an age: what the row must keep.
		past := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Millisecond)
		vol, _ := row.Data["volume"].(map[string]any)
		vol[pve.VolumeLabel], vol["name"] = "t-vol1", "t-vol1"
		row.CreatedAt = &past
		if row, err = repo.Insert(ctx, row); err != nil {
			t.Fatal(err)
		}
		return ps, row
	}
	volume := func(t *testing.T, row *cloud.CloudResource) map[string]any {
		t.Helper()
		got, err := repo.FindByID(ctx, row.ID)
		if err != nil || got == nil {
			t.Fatalf("the row is gone: %v", err)
		}
		if got.ExternalID != row.ExternalID || got.MovingSince != nil || got.CreatedAt == nil || !got.CreatedAt.Equal(*row.CreatedAt) {
			t.Fatalf("row = %+v, want it on %s, unmarked, created %s", got, row.ExternalID, row.CreatedAt)
		}
		vol, _ := got.Data["volume"].(map[string]any)
		return vol
	}

	t.Run("attached to another server", func(t *testing.T) {
		f := &rowsPVE{moveTo: newVol}
		ps, row := seed(t, f)
		if err := h.AttachVolumeRow(ctx, ps, es, proj, row, 101, "102"); err != nil {
			t.Fatal(err)
		}
		h.ReconcileVolumeRows(ctx, ps, es, proj) // as the ATTACH action does next
		if row.ExternalID != newVol {
			t.Fatalf("the row is on %s, want %s", row.ExternalID, newVol)
		}
		if vol := volume(t, row); vol["name"] != "t-vol1" || vol["status"] != "in-use" || asFloat(vol["vmid"]) != 102 || vol["slot"] != "scsi1" {
			t.Fatalf("volume after the attach: %+v", vol)
		}
		rows, err := repo.FindByServiceProjectAndType(ctx, "svc", "p1", cloud.TypeVolume)
		if err != nil || len(rows) != 3 {
			t.Fatalf("want the two boot disks and the volume, one row each: %+v, %v", rows, err)
		}
		if n, err := db.C("cloudResourceHistory").Count(ctx, pgdoc.M{}); err != nil || n != 0 {
			t.Fatalf("no row may be archived by an attach: %d, %v", n, err)
		}
		// Detached again: the volid stays, and so does the name.
		f.set(map[int]map[string]string{
			101: {"scsi0": "local-zfs:vm-101-disk-0,size=10G"},
			102: {"scsi0": "local-zfs:vm-102-disk-0,size=10G", "unused0": newVol + ",size=5G"},
		})
		h.ReconcileVolumeRows(ctx, ps, es, proj)
		if vol := volume(t, row); vol["name"] != "t-vol1" || vol["status"] != "available" {
			t.Fatalf("volume after the detach: %+v", vol)
		}
	})

	// A refused attach leaves the disk where it was: the row stays on its volid, and the mark is off.
	t.Run("attach failed", func(t *testing.T) {
		f := &rowsPVE{moveTo: newVol, moveFails: true}
		ps, row := seed(t, f)
		if err := h.AttachVolumeRow(ctx, ps, es, proj, row, 101, "102"); err == nil {
			t.Fatal("a failed move answered success")
		}
		h.ReconcileVolumeRows(ctx, ps, es, proj)
		if vol := volume(t, row); row.ExternalID != oldVol || vol["name"] != "t-vol1" {
			t.Fatalf("row %s after a failed attach: %+v", row.ExternalID, vol)
		}
	})
}

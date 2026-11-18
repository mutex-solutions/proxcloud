//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// rowsPVE fakes the pool pc-p1 on one node: each VM is its config's entries (slot → value). A disk reassign
// (move_disk with target-vmid) takes the disk out of its slot and puts it into the target's under name moveTo (the
// target's next free disk name when empty), unless moveFails. It also fakes what a rebuild and a rollback do to a
// config: a clone (VM 200 with one 20G disk), config writes (a deleted disk slot parks its disk as unused0, a deleted
// unusedN destroys it), a VM's delete, and a rollback to the config in snap.
type rowsPVE struct {
	mu        sync.Mutex
	vms       map[int]map[string]string
	snap      map[string]string // the config a rollback of VM 101 brings back
	moveTo    string
	moveFails bool
	taskFails bool   // a task that ran is reported as failed: what a wait that ran out looks like to the caller
	afterMove func() // runs once a reassign is done, before its answer
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
					"pool": "pc-p1", "status": "stopped", "maxcpu": 1, "maxmem": 1 << 30, "maxdisk": 10 << 30, "tags": f.vms[id]["tags"]})
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
		case r.Method == http.MethodGet && len(parts) == 6 && parts[2] == "qemu" && parts[4] == "status":
			data(map[string]any{"status": "stopped"})
		case r.Method == http.MethodPost && len(parts) == 6 && parts[2] == "qemu" && parts[4] == "status":
			data(upid)
		case path == "/cluster/replication":
			data([]any{})
		case path == "/cluster/nextid":
			data("200")
		case r.Method == http.MethodPost && len(parts) == 5 && parts[4] == "clone":
			f.vms[200] = map[string]string{"scsi0": "local-zfs:vm-200-disk-0,size=20G"}
			data(upid)
		case r.Method == http.MethodDelete && len(parts) == 4 && parts[2] == "qemu":
			id, _ := strconv.Atoi(parts[3])
			delete(f.vms, id)
			data(upid)
		case r.Method == http.MethodPut && len(parts) == 5 && parts[4] == "resize":
			data(upid)
		case r.Method == http.MethodPost && len(parts) == 5 && parts[2] == "qemu" && parts[4] == "config":
			_ = r.ParseForm()
			id, _ := strconv.Atoi(parts[3])
			for _, slot := range strings.Split(r.PostForm.Get("delete"), ",") {
				if v := f.vms[id][slot]; v != "" && !strings.HasPrefix(slot, "unused") && strings.Contains(v, ":vm-") {
					f.vms[id]["unused0"] = v
				}
				delete(f.vms[id], slot)
			}
			for k, v := range r.PostForm {
				if k != "delete" {
					f.vms[id][k] = v[0]
				}
			}
			data(upid)
		case len(parts) == 7 && parts[4] == "snapshot" && parts[6] == "config":
			data(f.snap)
		case r.Method == http.MethodPost && len(parts) == 7 && parts[4] == "snapshot" && parts[6] == "rollback":
			f.vms[101] = f.snap
			data(upid)
		case len(parts) == 5 && parts[2] == "qemu" && parts[4] == "pending":
			data([]any{})
		case len(parts) == 5 && parts[2] == "qemu" && parts[4] == "snapshot":
			out := []map[string]any{{"name": "current", "description": "You are here!"}}
			if f.snap != nil && parts[3] == "101" {
				out = []map[string]any{{"name": "s1", "snaptime": 1}, {"name": "current", "parent": "s1", "description": "You are here!"}}
			}
			data(out)
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
			to := f.moveTo
			for n := 0; to == ""; n++ {
				to = fmt.Sprintf("local-zfs:vm-%d-disk-%d", target, n)
				for _, v := range f.vms[target] {
					if strings.HasPrefix(v, to+",") || v == to {
						to = ""
					}
				}
			}
			f.vms[target][r.Form.Get("target-disk")] = to + "," + size
			if f.afterMove != nil {
				f.mu.Unlock()
				f.afterMove()
				f.mu.Lock()
			}
			data(upid)
		case strings.HasSuffix(path, "/status") && strings.Contains(path, "/tasks/"):
			if f.taskFails {
				data(map[string]any{"status": "stopped", "exitstatus": "interrupted by signal"})
				return
			}
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

// The actions that replace or drop a server's disk (REBUILD, RESTORE_BACKUP, ROLLBACK_SNAPSHOT) end with
// ReconcileVolumeRows. The inline sync they ran before only added and updated rows, so on a site without the
// scheduled sync the row of the destroyed disk stayed: the production acceptance showed two boot volume rows after a
// rebuild and the quota counting 20 GB twice (D3). Each case is the cluster as that action leaves it; the actions
// themselves are driven in TestVolumeActionsKeepRowsEqualDisks.
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
		// A snapshot taken before the data disk was added: the rollback's config no longer names it.
		{"rollback", map[string]string{"scsi0": boot + ",size=20G"}, map[string]int{boot: 20}, []string{data}},
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

	// A sync between the reassign and the follow sees the old volid gone. The move mark keeps the row; without it the
	// row was archived there and the follow found nothing to keep.
	t.Run("a sync during the move", func(t *testing.T) {
		f := &rowsPVE{moveTo: newVol}
		ps, row := seed(t, f)
		f.afterMove = func() { h.ReconcileVolumeRows(ctx, ps, es, proj) }
		if err := h.AttachVolumeRow(ctx, ps, es, proj, row, 101, "102"); err != nil {
			t.Fatal(err)
		}
		h.ReconcileVolumeRows(ctx, ps, es, proj)
		if vol := volume(t, row); row.ExternalID != newVol || vol["name"] != "t-vol1" {
			t.Fatalf("row %s after a sync during the move: %+v", row.ExternalID, vol)
		}
		if rows, err := repo.FindByServiceProjectAndType(ctx, "svc", "p1", cloud.TypeVolume); err != nil || len(rows) != 3 {
			t.Fatalf("want one row a disk: %+v, %v", rows, err)
		}
	})

	// The reassign ran but its task answered an error (a wait that ran out while PVE finished): the disk is on the
	// other server all the same, the row follows and the attach is a success.
	t.Run("the disk moved despite an error", func(t *testing.T) {
		f := &rowsPVE{moveTo: newVol, taskFails: true}
		ps, row := seed(t, f)
		if err := h.AttachVolumeRow(ctx, ps, es, proj, row, 101, "102"); err != nil {
			t.Fatalf("the disk is on its new server: %v", err)
		}
		h.ReconcileVolumeRows(ctx, ps, es, proj)
		if vol := volume(t, row); row.ExternalID != newVol || vol["name"] != "t-vol1" {
			t.Fatalf("row %s: %+v", row.ExternalID, vol)
		}
		if n, err := db.C("cloudResourceHistory").Count(ctx, pgdoc.M{}); err != nil || n != 0 {
			t.Fatalf("no row may be archived: %d, %v", n, err)
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

// The handlers themselves, over POST /project/{id}/cloud/{resourceId}/action: each must end with the call that keeps
// the rows equal to the disks. Without the ATTACH handler's AttachVolumeRow the volume's row is archived (D4); without
// the reconcile after REBUILD and ROLLBACK_SNAPSHOT the row of the disk that is gone stays (D3).
func TestVolumeActionsKeepRowsEqualDisks(t *testing.T) {
	ctx := context.Background()
	const sub = "owner-sub"
	const boot, vol = "local-zfs:vm-101-disk-0", "local-zfs:vm-101-disk-1"

	setup := func(t *testing.T, f *rowsPVE, vms map[int]map[string]string) (*cloud.Repo, map[string]string, func(rowID, body string) *httptest.ResponseRecorder) {
		t.Helper()
		db := freshPG(t)
		orgRepo, billingRepo, users, repo := org.NewRepo(db), billing.NewRepo(db), user.NewRepo(db), cloud.NewRepo(db)
		esSvc := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("test-key"))
		h := project.NewHandler(project.NewService(project.NewRepo(db), orgRepo, billingRepo, users, nil), project.NewPolicy(org.NewPolicy(orgRepo)),
			org.NewService(orgRepo, billingRepo, nil), users, audit.NewService(audit.NewRepo(db), slog.Default()), billingRepo, repo, esSvc, nil, nil, "r")
		ps := f.service(t)
		f.set(vms)
		h.SetProxmox(func(*externalservice.ExternalService) (*pve.Service, error) { return ps, nil })
		router := chi.NewRouter()
		h.Routes(router)
		for coll, doc := range map[string]pgdoc.M{
			"users":                {"sub": sub, "email": "o@example.org"},
			"organization":         {"_id": "o1", "name": "o"},
			"organization_members": {"organizationId": "o1", "sub": sub, "roles": []string{"OWNER"}},
			"externalService": {"_id": "svc", "name": "pve", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
				"config": pgdoc.M{"provider": "proxmox", "templates": pgdoc.M{"debian-12": pgdoc.M{"vmid": float64(9000), "node": "pve1"}}}},
			"project": {"_id": "p1", "name": "p", "status": project.StatusEnabled, "organizationId": "o1",
				"memberships": []pgdoc.M{{"sub": sub, "role": project.RoleOwner}},
				"services":    []any{pgdoc.M{"serviceId": "svc", "provider": "proxmox", "region": "r", "pool": "pc-p1"}}},
		} {
			mustInsert(t, db, coll, doc)
		}
		// The rows as a sync left them, plus the SERVER rows the actions run on.
		h.ReconcileVolumeRows(ctx, ps, &externalservice.ExternalService{ID: "svc"}, &project.Project{ID: "p1"})
		ids := map[string]string{}
		for id := range vms {
			row, err := repo.Insert(ctx, &cloud.CloudResource{ServiceID: "svc", ProjectID: "p1", Region: "r", Type: cloud.TypeServer,
				ExternalID: strconv.Itoa(id), Data: map[string]any{"server": map[string]any{"name": "srv" + strconv.Itoa(id)}}})
			if err != nil {
				t.Fatal(err)
			}
			ids[row.ExternalID] = row.ID
		}
		rows, err := repo.FindByServiceProjectAndType(ctx, "svc", "p1", cloud.TypeVolume)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			ids[r.ExternalID] = r.ID
		}
		return repo, ids, func(rowID, body string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, "/project/p1/cloud/"+rowID+"/action", strings.NewReader(body))
			req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			return w
		}
	}
	// volumes answers volid → row id.
	volumes := func(t *testing.T, repo *cloud.Repo) map[string]string {
		t.Helper()
		rows, err := repo.FindByServiceProjectAndType(ctx, "svc", "p1", cloud.TypeVolume)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, r := range rows {
			out[r.ExternalID] = r.ID
		}
		return out
	}

	t.Run("ATTACH to another server keeps the row", func(t *testing.T) {
		f := &rowsPVE{}
		repo, ids, act := setup(t, f, map[int]map[string]string{
			101: {"scsi0": boot + ",size=10G", "unused0": vol + ",size=5G"},
			102: {"scsi0": "local-zfs:vm-102-disk-0,size=10G"},
		})
		if w := act(ids[vol], `{"action":"ATTACH","data":{"serverId":"`+ids["102"]+`"}}`); w.Code != http.StatusOK {
			t.Fatalf("attach: %d %s", w.Code, w.Body.String())
		}
		got := volumes(t, repo)
		if len(got) != 3 || got["local-zfs:vm-102-disk-1"] != ids[vol] {
			t.Fatalf("rows %v: want the volume's row %s on the disk's new volid", got, ids[vol])
		}
	})

	rebuild := func(t *testing.T, f *rowsPVE) (*cloud.Repo, map[string]string) {
		t.Helper()
		repo, ids, act := setup(t, f, map[int]map[string]string{
			101: {"scsi0": boot + ",size=20G", "scsi1": vol + ",size=30G"},
			102: {"scsi0": "local-zfs:vm-102-disk-0,size=10G"},
		})
		if w := act(ids["101"], `{"action":"REBUILD","data":{"imageId":"debian-12"}}`); w.Code != http.StatusOK {
			t.Fatalf("rebuild: %d %s", w.Code, w.Body.String())
		}
		return repo, ids
	}
	t.Run("REBUILD archives the old boot disk's row", func(t *testing.T) {
		repo, ids := rebuild(t, &rowsPVE{})
		got := volumes(t, repo)
		if _, stays := got[boot]; stays || len(got) != 3 || got[vol] != ids[vol] || got["local-zfs:vm-101-disk-2"] == "" {
			t.Fatalf("rows %v: want the new boot disk, the data disk and server 102's, and no row for %s", got, boot)
		}
	})
	// Another server of the project is under a backup: the project's disks cannot be listed, so the reconcile does
	// not run. The rebuild destroyed the old boot disk itself, and its row goes all the same. The new disk's row
	// waits for the next sync.
	t.Run("REBUILD while another server is locked", func(t *testing.T) {
		f := &rowsPVE{}
		f.afterMove = func() { // from the swap on, so the fixture's own first sync still runs
			f.mu.Lock()
			f.vms[102]["lock"] = "backup"
			f.mu.Unlock()
		}
		repo, ids := rebuild(t, f)
		got := volumes(t, repo)
		if _, stays := got[boot]; stays || len(got) != 2 || got[vol] != ids[vol] {
			t.Fatalf("rows %v: want the old boot disk's row gone and the others untouched", got)
		}
	})

	t.Run("ROLLBACK_SNAPSHOT archives the row of a disk the snapshot does not have", func(t *testing.T) {
		f := &rowsPVE{snap: map[string]string{"scsi0": boot + ",size=20G"}}
		repo, ids, act := setup(t, f, map[int]map[string]string{101: {"scsi0": boot + ",size=20G", "scsi1": vol + ",size=30G"}})
		if w := act(ids["101"], `{"action":"ROLLBACK_SNAPSHOT","data":{"name":"s1"}}`); w.Code != http.StatusOK {
			t.Fatalf("rollback: %d %s", w.Code, w.Body.String())
		}
		if got := volumes(t, repo); len(got) != 1 || got[boot] != ids[boot] {
			t.Fatalf("rows %v: want only the boot disk's row", got)
		}
	})
}

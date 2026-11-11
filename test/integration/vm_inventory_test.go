//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/cloud/syncjob"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

type vmInventoryWire struct {
	mode            string
	full            bool
	reads           int
	mutate          func()
	mutations       int
	aclReads        int
	protectionReads int
}

func vmWireRow(id int, template bool) map[string]any {
	t := 0
	if template {
		t = 1
	}
	return map[string]any{"id": fmt.Sprintf("qemu/%d", id), "type": "qemu", "vmid": id, "node": "n1", "pool": "pc-p", "name": fmt.Sprintf("vm-%d", id), "status": "running", "template": t, "tags": "proxcloud;pc-c-1", "maxcpu": 2, "maxmem": int64(2 << 30), "maxdisk": int64(8 << 30)}
}
func (f *vmInventoryWire) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		f.mutations++
	}
	if r.Method != "GET" || r.Header.Get("Authorization") != "PVEAPIToken=portal@pve!inventory=key" || r.URL.Query().Get("userid") != "" {
		w.WriteHeader(403)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	write := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	gone := f.mode == "empty" || f.mode == "denied" || f.mode == "masked" || f.mode == "hidden" || f.mode == "partial-nodes"
	template := f.mode == "template"
	switch {
	case strings.HasSuffix(path, "/agent/network-get-interfaces"):
		switch f.mode {
		case "agent-null":
			write(nil)
		case "agent-missing":
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case "agent-object":
			write(map[string]any{})
		case "agent-result-null":
			write(map[string]any{"result": nil})
		case "agent-empty-interface":
			write(map[string]any{"result": []any{map[string]any{}}})
		case "agent-null-addresses":
			write(map[string]any{"result": []any{map[string]any{"name": "eth0", "hardware-address": "AA:BB:CC:DD:EE:01", "ip-addresses": nil}}})
		case "agent-403":
			w.WriteHeader(403)
		default:
			write(map[string]any{"result": []any{}})
		}
		return
	case strings.Contains(path, "/replication/") && strings.HasSuffix(path, "/status"):
		f.protectionReads++
		write(map[string]any{"last_sync": 1, "fail_count": 0})
		return
	case path == "/cluster/replication":
		if f.mode == "partial-protected" {
			write([]any{map[string]any{"id": "102-0", "guest": 102, "target": "n2", "schedule": "*/15"}})
			return
		}
		if f.mode == "protection-null" {
			write(nil)
		} else {
			write([]any{})
		}
		return
	case strings.HasSuffix(path, "/snapshot"):
		if f.mode == "snapshot-null" {
			write(nil)
		} else if f.mode == "snapshot-403" {
			w.WriteHeader(403)
		} else {
			write([]any{})
		}
		return
	case strings.Contains(path, "/storage/"):
		if f.mode == "storage-403" {
			w.WriteHeader(403)
		} else if f.mode == "storage-null" {
			write(nil)
		} else {
			write([]any{})
		}
		return
	case path == "/cluster/resources":
		if r.URL.Query().Get("type") == "node" {
			write([]any{map[string]any{"type": "node", "node": "n1", "status": "online"}})
			return
		}
		if f.mode == "null-page" {
			write(nil)
			return
		}
		if f.mode == "partial" || f.mode == "partial-protected" {
			write([]any{vmWireRow(102, false)})
			return
		}
		if f.mode == "foreign-ip" {
			row := vmWireRow(102, false)
			row["pool"] = "pc-foreign"
			write([]any{row})
			return
		}
		if gone {
			write([]any{})
			return
		}
		row := vmWireRow(101, template)
		if f.mode == "page-template-null" {
			row["template"] = nil
		}
		if f.mode == "page-template-two" {
			row["template"] = 2
		}
		if f.mode == "page-template-string" {
			row["template"] = "1"
		}
		if f.mode == "page-null-cpu" {
			row["maxcpu"] = nil
		}
		if f.mode == "page-no-status" {
			delete(row, "status")
		}
		if f.mode == "holder" {
			row["tags"] = "proxcloud;pc-holder"
		}
		if f.mode == "migration" {
			row["lock"] = "migrate"
		}
		write([]any{row})
	case path == "/nodes":
		status := "online"
		if f.mode == "offline" {
			status = "offline"
		}
		write([]any{map[string]any{"node": "n1", "status": status}})
	case path == "/cluster/status":
		out := []any{map[string]any{"type": "cluster", "quorate": 1}, map[string]any{"type": "node", "name": "n1", "online": 1}}
		if f.mode == "partial-nodes" {
			out = append(out, map[string]any{"type": "node", "name": "n2", "online": 1})
		}
		write(out)
	case path == "/pools/pc-p":
		if f.mode == "bad-pool" {
			write(map[string]any{"poolid": "pc-p"})
			return
		}
		members := []any{}
		if !gone && f.mode != "foreign-ip" {
			id := 101
			if f.mode == "partial" || f.mode == "partial-protected" {
				id = 102
			}
			members = append(members, map[string]any{"type": "qemu", "id": fmt.Sprintf("qemu/%d", id), "vmid": id, "node": "n1"})
		}
		write(map[string]any{"poolid": "pc-p", "members": members})
	case path == "/access/permissions":
		p := r.URL.Query().Get("path")
		privs := map[string]int{}
		if p == "/access" || p == "/" {
			if f.full {
				privs["Sys.Audit"] = 1
			}
		} else if strings.HasPrefix(p, "/nodes") {
			privs["Sys.Audit"] = 1
		} else {
			privs["VM.Audit"] = 1
			privs["Pool.Audit"] = 1
		}
		if f.mode == "hidden" && p == "/vms/101" {
			privs = map[string]int{}
		}
		write(map[string]any{p: privs})
	case path == "/access/acl":
		f.aclReads++
		rows := []any{}
		if f.mode == "hidden" {
			rows = append(rows, map[string]any{"path": "/vms/101", "type": "token", "ugid": "portal@pve!inventory", "roleid": "NoAccess", "propagate": 1})
		}
		write(rows)
	case strings.HasSuffix(path, "/config"):
		parts := strings.Split(path, "/")
		id, _ := strconv.Atoi(parts[4])
		if gone || f.mode == "foreign-ip" && id == 101 {
			if f.mode == "denied" {
				w.WriteHeader(403)
				return
			}
			if f.mode == "masked" {
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(map[string]any{"errors": map[string]string{"vmid": fmt.Sprintf("could not find vmid '%d'", id)}})
				return
			}
			w.WriteHeader(500)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": map[string]string{"vmid": fmt.Sprintf("Configuration file 'nodes/n1/qemu-server/%d.conf' does not exist", id)}})
			return
		}
		f.reads++
		if f.mutate != nil && f.reads == 3 {
			f.mutate()
		}
		uuid := "11111111-1111-4111-8111-111111111111"
		if f.mode == "reuse" {
			uuid = "22222222-2222-4222-8222-222222222222"
		}
		t := 0
		if template {
			t = 1
		}
		cfg := map[string]any{"name": fmt.Sprintf("vm-%d", id), "smbios1": "uuid=" + uuid, "tags": "proxcloud;pc-c-1", "template": t, "scsi0": fmt.Sprintf("z:vm-%d-disk-0,size=8G", id), "net0": "virtio=AA:BB:CC:DD:EE:01,bridge=v1"}
		if f.mode == "cfg-template-null" {
			cfg["template"] = nil
		}
		if f.mode == "cfg-template-two" {
			cfg["template"] = 2
		}
		if f.mode == "cfg-template-string" {
			cfg["template"] = "1"
		}
		if f.mode == "holder" {
			cfg["tags"] = "proxcloud;pc-holder"
		}
		if f.mode == "storage-403" || f.mode == "storage-null" || f.mode == "storage-empty" {
			cfg["unused0"] = "z:vm-101-disk-1"
		}
		if f.mode == "foreign-ip" {
			cfg["ipconfig1"] = "ip=198.51.100.3/24"
			cfg["net1"] = "virtio=AA:BB:CC:DD:EE:02,bridge=vmbr1"
		}
		if f.mode == "net-null" {
			cfg["net0"] = nil
		}
		if f.mode == "ip-object" {
			cfg["ipconfig0"] = map[string]any{"ip": "192.0.2.1"}
		}
		if f.mode == "restore-cut-down" {
			delete(cfg, "scsi0")
			delete(cfg, "net0")
			delete(cfg, "smbios1")
			cfg["memory"] = "128"
		}
		if f.mode == "disk-null" {
			cfg["scsi0"] = nil
		}
		if f.mode == "disk-number" {
			cfg["scsi0"] = 7
		}
		if f.mode == "disk-object" {
			cfg["scsi0"] = map[string]any{"file": "z:x"}
		}
		write(cfg)
	case strings.HasSuffix(path, "/pending"):
		if f.mode == "pending-empty-object" {
			write([]any{map[string]any{}})
		} else if f.mode == "pending-null-key" {
			write([]any{map[string]any{"key": nil}})
		} else {
			write([]any{})
		}
	case strings.HasSuffix(path, "/status/current"):
		write(map[string]any{"status": "running", "qmpstatus": "running"})
	default:
		write([]any{}) // optional agent/protection/snapshot reads carry no proof
	}
}
func vmTestService(t *testing.T, f *vmInventoryWire) *pve.Service {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := pve.New(pve.Config{APIURL: srv.URL, TokenID: "portal@pve!inventory", Secret: "key"})
	if err != nil {
		t.Fatal(err)
	}
	return &pve.Service{C: c, ServiceID: "s", PoolPrefix: "pc-"}
}
func vmProvider(s *pve.Service, typ string) providers.Provider {
	switch typ {
	case cloud.TypeServer:
		return s.ServerSyncProvider("r", "p", nil)
	case cloud.TypeVolume:
		return s.VolumeSyncProvider("r", "p", nil)
	default:
		return s.ImageSyncProvider("r", "p")
	}
}
func vmCached(t *testing.T, db *pgdoc.DB, typ string) *cloud.CloudResource {
	t.Helper()
	past := time.Now().UTC().Add(-time.Hour)
	cr := &cloud.CloudResource{ProjectID: "p", ServiceID: "s", ExternalID: "101", Region: "r", Type: typ, CreatedAt: &past, UpdatedAt: &past, Data: map[string]any{"identity": "c-1", "pveUUID": "11111111-1111-4111-8111-111111111111", "server": map[string]any{"status": "ACTIVE", "host": "n1", "addresses": map[string]any{"private": []any{"keep"}}}}}
	if typ == cloud.TypeVolume {
		cr.ExternalID = "z:vm-101-disk-0"
		cr.Data = map[string]any{"volume": map[string]any{"vmid": 101, "pveUUID": "11111111-1111-4111-8111-111111111111", "identity": "c-1", "size": 8, "name": "keep"}}
	}
	if typ == cloud.TypeImage {
		cr.Data = map[string]any{"pveUUID": "11111111-1111-4111-8111-111111111111", "image": map[string]any{"name": "keep"}}
	}
	out, err := cloud.NewRepo(db).Insert(context.Background(), cr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func exactVMRow(t *testing.T, repo *cloud.Repo, old *cloud.CloudResource) {
	t.Helper()
	got, err := repo.FindByID(context.Background(), old.ID)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := json.Marshal(old)
	y, _ := json.Marshal(got)
	if !bytes.Equal(x, y) {
		t.Fatalf("row changed\nold%s\nnew%s", x, y)
	}
	h, err := repo.FindArchivedSince(context.Background(), "p", "s", old.CreatedAt.Add(-time.Minute))
	if err != nil || len(h) != 0 {
		t.Fatalf("history changed: %d %v", len(h), err)
	}
}

func TestVMInventoryUnknownTwoRoundsAtomicScope(t *testing.T) {
	for _, mode := range []string{"empty", "partial", "denied", "masked", "hidden", "null-page", "bad-pool", "offline", "migration", "reuse", "template", "holder", "disk-null", "disk-number", "disk-object", "page-null-cpu", "page-no-status", "partial-nodes", "pending-empty-object", "pending-null-key", "net-null", "ip-object", "page-template-null", "page-template-two", "page-template-string", "cfg-template-null", "cfg-template-two", "cfg-template-string"} {
		for _, typ := range []string{cloud.TypeServer, cloud.TypeVolume, cloud.TypeImage} {
			t.Run(mode+"/"+typ, func(t *testing.T) {
				if mode == "template" && typ == cloud.TypeImage || mode == "holder" && typ == cloud.TypeVolume {
					t.Skip("positive same-identity classification is not an unknown")
				}
				ctx := context.Background()
				db := freshPG(t)
				repo := cloud.NewRepo(db)
				old := vmCached(t, db, typ)
				f := &vmInventoryWire{mode: mode, full: mode == "hidden" || mode == "partial-nodes"}
				s := vmTestService(t, f)
				// An image source is a template normally; test IMAGE against the same opaque
				// omission/reuse paths without pretending a server is an authoritative image.
				for round := 0; round < 2; round++ {
					st, err := providers.Reconcile(ctx, vmProvider(s, typ), repo, "s", time.Now().UTC())
					var unknown *cloud.InventoryUntrustedError
					if !errors.As(err, &unknown) || st != (providers.ReconcileStats{}) {
						t.Fatalf("round%d: stats%+v err%v", round, st, err)
					}
					exactVMRow(t, repo, old)
				}
			})
		}
	}
}
func TestVMInventoryKnownRefreshWithoutGlobalReaderAndAuthorityDeletion(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := cloud.NewRepo(db)
	old := vmCached(t, db, cloud.TypeServer)
	f := &vmInventoryWire{}
	s := vmTestService(t, f)
	st, err := providers.Reconcile(ctx, vmProvider(s, cloud.TypeServer), repo, "s", time.Now().UTC())
	if err != nil || st.Deleted != 0 || st.Created != 0 {
		t.Fatalf("normal pool-only pass: %+v %v", st, err)
	}
	got, _ := repo.FindByID(ctx, old.ID)
	if got == nil {
		t.Fatal("normal refresh replaced row")
	}
	if _, err := providers.Reconcile(ctx, vmProvider(s, cloud.TypeImage), repo, "s", time.Now().UTC()); err != nil || f.aclReads != 0 {
		t.Fatalf("empty image forced global ACL: calls%d err%v", f.aclReads, err)
	}
	f.mode = "empty"
	f.full = true
	st, err = providers.Reconcile(ctx, vmProvider(s, cloud.TypeServer), repo, "s", time.Now().UTC())
	if err != nil || st.Deleted != 1 {
		t.Fatalf("authoritative deletion: %+v %v", st, err)
	}
}
func TestVMInventoryWholeRowCASAndProtectedVolumes(t *testing.T) {
	for _, mode := range []string{"same-timestamp", "moving", "csi-collision", "carrier"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			db := freshPG(t)
			repo := cloud.NewRepo(db)
			old := vmCached(t, db, cloud.TypeVolume)
			switch mode {
			case "moving":
				old.MovingSince = old.CreatedAt
				_, _ = db.C("cloudResource").SetByID(ctx, old.ID, pgdoc.M{"movingSince": old.CreatedAt}, nil)
			case "csi-collision":
				old.Data["volume"].(map[string]any)["k8sCluster"] = "cluster"
				_, _ = db.C("cloudResource").SetByID(ctx, old.ID, pgdoc.M{"data": old.Data}, nil)
			case "carrier":
				old.Data["volume"].(map[string]any)["holderCarrier"] = "pending"
				_, _ = db.C("cloudResource").SetByID(ctx, old.ID, pgdoc.M{"data": old.Data}, nil)
			}
			f := &vmInventoryWire{}
			s := vmTestService(t, f)
			if mode == "same-timestamp" {
				f.mutate = func() {
					old.Data["volume"].(map[string]any)["name"] = "concurrent"
					_, err := db.C("cloudResource").SetByID(ctx, old.ID, pgdoc.M{"data": old.Data}, nil)
					if err != nil {
						t.Error(err)
					}
				}
			}
			st, err := providers.Reconcile(ctx, vmProvider(s, cloud.TypeVolume), repo, "s", time.Now().UTC())
			if mode != "same-timestamp" {
				var unknown *cloud.InventoryUntrustedError
				if !errors.As(err, &unknown) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if st != (providers.ReconcileStats{}) {
				t.Fatalf("protected write: %+v", st)
			}
			exactVMRow(t, repo, old)
		})
	}
}
func TestVMInventorySamplerRetainsIPsBeforeRowAndReleasesOnlyAuthority(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := cloud.NewRepo(db)
	old := vmCached(t, db, cloud.TypeServer)
	f := &vmInventoryWire{mode: "partial"}
	s := vmTestService(t, f)
	mustInsert(t, db, "project", pgdoc.M{"_id": "p", "name": "fixture", "status": "ENABLED", "services": []any{pgdoc.M{"serviceId": "s"}}})
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "s", "name": "fixture", "type": "CLOUD", "status": "PUBLIC", "config": pgdoc.M{"provider": "proxmox", "region": "r"}})
	ips := project.NewPublicIPRepo(db)
	past := old.CreatedAt
	for _, a := range []project.PublicIP{{ID: "s/v1/192.0.2.3", ServiceID: "s", ProjectID: "p", ServerID: "101", NetworkID: "v1", IP: "192.0.2.3", NIC: "net0", CreatedAt: past}, {ID: "s/pub/198.51.100.3", ServiceID: "s", ProjectID: "p", ServerID: "101", NetworkID: "pub", IP: "198.51.100.3", NIC: "net1", CreatedAt: past}, {ID: "s/pub/198.51.100.4", ServiceID: "s", ProjectID: "p", ServerID: "101", NetworkID: "pub", IP: "198.51.100.4", NIC: "net1", BeforeRow: true, CreatedAt: past}} {
		_, err := db.C("pvePublicIp").InsertOne(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
	}
	es := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("k"))
	bulk := 0
	j := syncjob.New(project.NewRepo(db), es, repo, nil).WithProxmoxFactory(func(*externalservice.ExternalService) (*pve.Service, error) { return s, nil }).WithPublicIPs(ips.ForProject, func(context.Context, string, string, string) error { bulk++; return nil }).WithPublicIPInventory(ips)
	for round := 0; round < 2; round++ {
		_, err := j.RunPVEState(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held, _ := ips.ForProject(ctx, "s", "p")
		if len(held) != 3 || bulk != 0 {
			t.Fatalf("unknown released allocations: %v bulk%d", held, bulk)
		}
		exactVMRow(t, repo, old)
	}
	f.mode = "empty"
	f.full = true
	_, err := j.RunPVEState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	held, _ := ips.ForProject(ctx, "s", "p")
	if len(held) != 1 || !held[0].BeforeRow || bulk != 0 {
		t.Fatalf("authority release: %+v bulk%d", held, bulk)
	}
}

func TestVMInventoryIPReleaseWholeSnapshotAndAllocationCAS(t *testing.T) {
	for _, mode := range []string{"same", "info-only", "updated-only", "new-allocation", "new-cache-generation", "legacy-archive"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			db := freshPG(t)
			repo := cloud.NewRepo(db)
			old := vmCached(t, db, cloud.TypeServer)
			ips := project.NewPublicIPRepo(db)
			alloc := project.PublicIP{ID: "s/pub/198.51.100.3", ServiceID: "s", ProjectID: "p", ServerID: "101", NetworkID: "pub", IP: "198.51.100.3", NIC: "net1", CreatedAt: old.CreatedAt}
			if _, err := db.C("pvePublicIp").InsertOne(ctx, alloc); err != nil {
				t.Fatal(err)
			}
			expected := old
			if mode == "info-only" || mode == "updated-only" {
				if mode == "info-only" {
					_, _ = db.C("cloudResource").SetByID(ctx, old.ID, pgdoc.M{"info": pgdoc.M{"operation": "concurrent"}}, nil)
				} else {
					now := time.Now().UTC()
					_, _ = db.C("cloudResource").SetByID(ctx, old.ID, pgdoc.M{"updatedAt": now}, nil)
				}
				var err error
				expected, err = repo.FindByID(ctx, old.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "legacy-archive" {
				if err := repo.DeleteAndArchive(ctx, expected, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			} else {
				deleted, err := repo.DeleteAndArchiveIfUnchanged(ctx, expected, time.Now().UTC())
				if err != nil || !deleted {
					t.Fatalf("archive: %v %v", deleted, err)
				}
			}
			if mode == "new-allocation" {
				now := time.Now().UTC()
				_, _ = db.C("pvePublicIp").SetByID(ctx, alloc.ID, pgdoc.M{"createdAt": now}, nil)
			}
			if mode == "new-cache-generation" {
				_, err := repo.Insert(ctx, &cloud.CloudResource{ServiceID: "s", ProjectID: "p", ExternalID: "101", Type: cloud.TypeServer, Data: map[string]any{"pveUUID": "replacement"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			released, err := ips.ReleaseIfUnchanged(ctx, alloc, old)
			if err != nil {
				t.Fatal(err)
			}
			if released != (mode == "same") {
				t.Fatalf("release mode%s=%v", mode, released)
			}
			held, _ := ips.ForProject(ctx, "s", "p")
			if mode != "same" && len(held) != 1 {
				t.Fatalf("allocation lost: %+v", held)
			}
		})
	}
}

func TestVMInventoryProjectionFailuresDoNotEraseRowsOrHeal(t *testing.T) {
	for _, mode := range []string{"snapshot-null", "snapshot-403", "protection-null", "storage-403", "storage-null", "storage-empty"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			db := freshPG(t)
			repo := cloud.NewRepo(db)
			typ := cloud.TypeServer
			if strings.HasPrefix(mode, "storage") {
				typ = cloud.TypeVolume
			}
			old := vmCached(t, db, typ)
			if typ == cloud.TypeServer {
				srv := old.Data["server"].(map[string]any)
				srv["protection"] = map[string]any{"state": "active", "diskGb": 8}
				srv["snapshots"] = map[string]any{"diskGb": 8, "items": []any{map[string]any{"name": "keep"}}}
				_, _ = db.C("cloudResource").SetByID(ctx, old.ID, pgdoc.M{"data": old.Data}, nil)
			}
			f := &vmInventoryWire{mode: mode, full: true}
			s := vmTestService(t, f)
			st, err := providers.Reconcile(ctx, vmProvider(s, typ), repo, "s", time.Now().UTC())
			var unknown *cloud.InventoryUntrustedError
			if !errors.As(err, &unknown) || st != (providers.ReconcileStats{}) || f.mutations != 0 {
				t.Fatalf("projection failure: %+v %v writes%d", st, err, f.mutations)
			}
			exactVMRow(t, repo, old)
		})
	}
	t.Run("visible-protected-with-omitted-cached", func(t *testing.T) {
		ctx := context.Background()
		db := freshPG(t)
		repo := cloud.NewRepo(db)
		old := vmCached(t, db, cloud.TypeServer)
		f := &vmInventoryWire{mode: "partial-protected"}
		s := vmTestService(t, f)
		list, err := vmProvider(s, cloud.TypeServer).List(ctx)
		if err != nil || len(list) != 1 || f.protectionReads == 0 {
			t.Fatalf("fixture did not reach protected VM mapping: %v %+v reads%d", err, list, f.protectionReads)
		}
		protection, _ := list[0].Data["server"].(map[string]any)["protection"].(map[string]any)
		if protection["ha"] != false || protection["state"] != "syncing" {
			t.Fatalf("fixture does not require protection heal: %+v", protection)
		}
		_, err = providers.Reconcile(ctx, vmProvider(s, cloud.TypeServer), repo, "s", time.Now().UTC())
		if err == nil || f.mutations != 0 {
			t.Fatalf("unknown scope healed: %v %d", err, f.mutations)
		}
		exactVMRow(t, repo, old)
	})
}

func TestVMInventoryForeignNICPreventsIPReuseAndLegacySampleLearnsUUID(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	old := vmCached(t, db, cloud.TypeServer)
	f := &vmInventoryWire{mode: "foreign-ip", full: true}
	s := vmTestService(t, f)
	if err := s.ProveVMAndAddressUnused(ctx, "p", old, "198.51.100.3"); err == nil {
		t.Fatal("foreign guest NIC/address cannot be considered unused")
	}
	f.mode = ""
	f.full = false
	f.aclReads = 0
	repo := cloud.NewRepo(db)
	delete(old.Data, "pveUUID")
	_, _ = db.C("cloudResource").SetByID(ctx, old.ID, pgdoc.M{"data": old.Data}, nil)
	page, err := s.C.ClusterResources(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	p := s.SampledServerProvider("r", "p", page, map[string]*cloud.CloudResource{"101": old})
	st, err := providers.Reconcile(ctx, p, repo, "s", time.Now().UTC())
	if err != nil || st.Deleted != 0 || st.Created != 0 || f.aclReads != 0 {
		t.Fatalf("legacy learn: %+v %v ACL%d", st, err, f.aclReads)
	}
	row, _ := repo.FindByID(ctx, old.ID)
	if row == nil || row.Data["pveUUID"] == "" {
		t.Fatal("legacy row failed to learn UUID from full mapping")
	}
}

func TestVMInventoryUnknownAgentPreservesDynamicAddressesAndLegacy(t *testing.T) {
	for _, mode := range []string{"agent-null", "agent-missing", "agent-object", "agent-result-null", "agent-empty-interface", "agent-null-addresses", "agent-403"} {
		for _, legacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/legacy=%v", mode, legacy), func(t *testing.T) {
				ctx := context.Background()
				db := freshPG(t)
				repo := cloud.NewRepo(db)
				old := vmCached(t, db, cloud.TypeServer)
				old.Data["server"].(map[string]any)["addresses"] = map[string]any{"private": []any{map[string]any{"addr": "192.0.2.42", "version": 4}}}
				if legacy {
					delete(old.Data, "pveUUID")
				}
				_, err := db.C("cloudResource").SetByID(ctx, old.ID, pgdoc.M{"data": old.Data}, nil)
				if err != nil {
					t.Fatal(err)
				}
				f := &vmInventoryWire{mode: mode}
				s := vmTestService(t, f)
				for round := 0; round < 2; round++ {
					st, err := providers.Reconcile(ctx, vmProvider(s, cloud.TypeServer), repo, "s", time.Now().UTC())
					var unknown *cloud.InventoryUntrustedError
					if !errors.As(err, &unknown) || st != (providers.ReconcileStats{}) {
						t.Fatalf("round%d: %+v %v", round, st, err)
					}
					exactVMRow(t, repo, old)
				}
				f.mode = "" // explicit result[] is positive readable agent evidence
				st, err := providers.Reconcile(ctx, vmProvider(s, cloud.TypeServer), repo, "s", time.Now().UTC())
				if err != nil || st.Created != 0 || st.Deleted != 0 {
					t.Fatalf("recovery: %+v %v", st, err)
				}
			})
		}
	}
}

func TestVMInventoryDurableUnknownAndRestoreVetoes(t *testing.T) {
	for _, mode := range []string{"seed", "holder", "restore-cut-down"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			db := freshPG(t)
			repo := cloud.NewRepo(db)
			old := vmCached(t, db, cloud.TypeVolume)
			f := &vmInventoryWire{}
			s := vmTestService(t, f)
			switch mode {
			case "seed":
				mustInsert(t, db, "pveSeedArtifact", pgdoc.M{"_id": "fixture-seed", "serviceId": "s", "serverId": "101", "mutationPending": true, "state": "prepared"})
				s.SeedArtifacts = pve.NewSeedArtifactRepo(db)
			case "holder":
				ops := pve.NewHolderOperationRepo(db)
				if err := ops.Ensure(ctx); err != nil {
					t.Fatal(err)
				}
				_, err := db.Pool.Exec(ctx, `INSERT INTO "pveHolderOperation" (id,scope,owner,epoch,state,proof) VALUES ('11111111-1111-4111-8111-111111111111','pve-holders/s/pc-p','22222222-2222-4222-8222-222222222222',1,'DISPATCHING','{}')`)
				if err != nil {
					t.Fatal(err)
				}
				s.HolderOperations = ops
			default:
				f.mode = mode
			}
			for round := 0; round < 2; round++ {
				st, err := providers.Reconcile(ctx, vmProvider(s, cloud.TypeVolume), repo, "s", time.Now().UTC())
				var unknown *cloud.InventoryUntrustedError
				if !errors.As(err, &unknown) || st != (providers.ReconcileStats{}) || f.mutations != 0 {
					t.Fatalf("round%d: %+v %v writes%d", round, st, err, f.mutations)
				}
				exactVMRow(t, repo, old)
			}
		})
	}
}

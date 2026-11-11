//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/cloud/syncjob"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveincident"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

type wireRecorder struct {
	mu        sync.Mutex
	begins    int
	snapshots []pveincident.SnapshotResult
}

func (r *wireRecorder) BeginCapture(_ context.Context, p, g, region string) (pveincident.Capture, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.begins++
	return pveincident.Capture{ProviderID: p, Generation: g, Region: region, Epoch: int64(r.begins), StartedAt: time.Now()}, nil
}
func (r *wireRecorder) Observe(_ context.Context, x pveincident.SnapshotResult) (pveincident.ObserveResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshots = append(r.snapshots, x)
	return pveincident.ObserveResult{Accepted: true}, nil
}
func TestPVEIncidentWireAggregatesProjectsAndRecordsUntrustedGETOnly(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	const uuid1 = "00000000-0000-4000-8000-000000000101"
	const uuid2 = "00000000-0000-4000-8000-000000000102"
	var mu sync.Mutex
	lost, pageFailure, offline := false, false, false
	mutations := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != "GET" {
			mutations++
			w.WriteHeader(500)
			return
		}
		write := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")
		switch path {
		case "/nodes":
			status := "online"
			if offline {
				status = "offline"
			}
			write([]any{map[string]any{"node": "n1", "status": status}})
		case "/cluster/status":
			flag := 1
			if offline {
				flag = 0
			}
			write([]any{map[string]any{"type": "cluster", "quorate": 1}, map[string]any{"type": "node", "name": "n1", "online": flag}})
		case "/access/acl":
			write([]any{})
		case "/access/permissions":
			write(map[string]any{r.URL.Query().Get("path"): map[string]int{"Sys.Audit": 1, "VM.Audit": 1}})
		case "/cluster/resources":
			if pageFailure {
				w.WriteHeader(403)
				return
			}
			page := []map[string]any{}
			for i, pid := range []string{"project1", "project2"} {
				if lost && pid == "project2" {
					continue
				}
				id := 101 + i
				page = append(page, map[string]any{"id": "qemu/" + []string{"101", "102"}[i], "type": "qemu", "vmid": id, "node": "n1", "name": "server" + pid, "status": "running", "pool": "pc-" + pid, "maxcpu": 1, "maxmem": 1 << 30, "maxdisk": 8 << 30, "tags": ""})
			}
			write(page)
		case "/pools/pc-project1", "/pools/pc-project2":
			pid := "project1"
			id := 101
			if strings.HasSuffix(path, "project2") {
				pid, id = "project2", 102
			}
			write(map[string]any{"poolid": "pc-" + pid, "members": []any{map[string]any{"id": "qemu/" + map[int]string{101: "101", 102: "102"}[id], "type": "qemu", "vmid": id, "node": "n1"}}})
		case "/nodes/n1/qemu/101/config", "/nodes/n1/qemu/102/config":
			pid, id := "project1", uuid1
			if strings.Contains(path, "102") {
				pid, id = "project2", uuid2
			}
			write(map[string]any{"name": "server" + pid, "smbios1": "uuid=" + id, "scsi0": "z:disk,size=8G", "tags": ""})
		case "/nodes/n1/qemu/101/pending", "/nodes/n1/qemu/102/pending", "/cluster/replication":
			write([]any{})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	for _, pid := range []string{"project1", "project2"} {
		if _, err := db.C("project").InsertOne(ctx, pgdoc.M{"_id": pid, "status": "ENABLED", "services": []any{map[string]any{"serviceId": "provider"}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.C("externalService").InsertOne(ctx, pgdoc.M{"_id": "provider", "type": "CLOUD", "status": "PUBLIC", "config": pgdoc.M{"provider": "proxmox", "apiUrl": srv.URL, "region": "region", "services": pgdoc.M{"compute": pgdoc.M{"region": true}}}}); err != nil {
		t.Fatal(err)
	}
	cloudRepo := cloud.NewRepo(db)
	for i, pid := range []string{"project1", "project2"} {
		_, err := cloudRepo.Insert(ctx, &cloud.CloudResource{ID: "row" + pid, ServiceID: "provider", ProjectID: pid, Region: "region", Type: cloud.TypeServer, ExternalID: []string{"101", "102"}[i], Data: map[string]any{"pveUUID": []string{uuid1, uuid2}[i], "server": map[string]any{"status": "ACTIVE"}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	c, err := pve.New(pve.Config{APIURL: srv.URL, TokenID: "fixture@pve!observer", Secret: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &wireRecorder{}
	job := syncjob.New(project.NewRepo(db), externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("fixture")), cloudRepo, nil).WithProxmoxFactory(func(es *externalservice.ExternalService) (*pve.Service, error) {
		return &pve.Service{C: c, ServiceID: es.ID, PoolPrefix: "pc-"}, nil
	}).WithPVEIncidents(recorder)
	for round := 0; round < 4; round++ {
		mu.Lock()
		lost = round == 1
		pageFailure = round == 2
		offline = round == 3
		mu.Unlock()
		if _, err = job.RunPVEState(ctx); err != nil {
			t.Fatal(err)
		}
		x := recorder.snapshots[round]
		if recorder.begins != round+1 || len(recorder.snapshots) != round+1 {
			t.Fatal("per-project epochs", recorder)
		}
		if x.Trust.Nodes != pveincident.Verified || len(x.Nodes) != 1 {
			t.Fatal("independent node visibility", x)
		}
		if round == 0 {
			if x.Trust.VMs != pveincident.Verified || len(x.Links) != 2 || len(x.Protection) != 2 {
				t.Fatal("provider aggregate", x)
			}
		} else if x.Trust.VMs != pveincident.Untrusted || x.Trust.Protection != pveincident.Untrusted || x.LinksComplete {
			t.Fatal("unknown became healthy", x)
		}
		if round == 3 && x.Nodes[0].Status != pveincident.Offline {
			t.Fatal("explicit offline not observed", x)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if mutations != 0 {
		t.Fatal("observation mutated PVE", mutations)
	}
}

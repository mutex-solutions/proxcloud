//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

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

const volUPID = "UPID:n1:0:0:0:qmconfig:101:u:"

// volumePVE fakes one project VM (101 "web" on n1, pool pc-p1) whose config gains scsi1 once a data disk is added, and
// the reads the volume sync needs to list it.
func volumePVE(t *testing.T) *pve.Service {
	t.Helper()
	var added atomic.Bool
	write := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	vmRow := map[string]any{"id": "qemu/101", "type": "qemu", "vmid": 101, "node": "n1", "pool": "pc-p1", "name": "web",
		"status": "running", "template": 0, "tags": "proxcloud", "maxcpu": 2, "maxmem": int64(2 << 30), "maxdisk": int64(8 << 30)}
	config := func() map[string]any {
		cfg := map[string]any{"name": "web", "tags": "proxcloud", "template": 0,
			"smbios1": "uuid=11111111-1111-4111-8111-111111111111", "scsi0": "local-zfs:vm-101-disk-0,size=8G"}
		if added.Load() {
			cfg["scsi1"] = "local-zfs:vm-101-disk-1,size=20G"
		}
		return cfg
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api2/json/cluster/resources", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("type") {
		case "storage":
			write(w, []any{map[string]any{"type": "storage", "storage": "local-zfs", "node": "n1", "status": "available", "maxdisk": int64(100 << 30), "disk": 0}})
		default:
			write(w, []any{vmRow})
		}
	})
	mux.HandleFunc("GET /api2/json/nodes", func(w http.ResponseWriter, _ *http.Request) {
		write(w, []any{map[string]any{"node": "n1", "status": "online"}})
	})
	mux.HandleFunc("GET /api2/json/pools/pc-p1", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"poolid": "pc-p1", "members": []any{map[string]any{"type": "qemu", "id": "qemu/101", "vmid": 101, "node": "n1"}}})
	})
	mux.HandleFunc("GET /api2/json/access/permissions", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{r.URL.Query().Get("path"): map[string]int{"VM.Audit": 1, "Sys.Audit": 1}})
	})
	mux.HandleFunc("GET /api2/json/nodes/n1/qemu/101/status/current", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"status": "running", "qmpstatus": "running", "name": "web", "tags": "proxcloud"})
	})
	mux.HandleFunc("GET /api2/json/nodes/n1/qemu/101/config", func(w http.ResponseWriter, _ *http.Request) { write(w, config()) })
	mux.HandleFunc("GET /api2/json/nodes/n1/qemu/101/pending", func(w http.ResponseWriter, _ *http.Request) { write(w, []any{}) })
	mux.HandleFunc("POST /api2/json/nodes/n1/qemu/101/config", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if got := r.PostForm.Get("scsi1"); got != "local-zfs:20" {
			t.Errorf("scsi1 = %q, want local-zfs:20", got)
		}
		added.Store(true)
		write(w, volUPID)
	})
	mux.HandleFunc("GET /api2/json/nodes/n1/tasks/"+volUPID+"/status", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"status": "stopped", "exitstatus": "OK"})
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern == "" {
			t.Errorf("unexpected PVE call %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "pve", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// TestVolumeCreateReturnsTheVolume (#218): POST /project/{id}/cloud {type: VOLUME} answered an empty result, so a client
// had to list volumes to find the one it made. It now answers the created volume's row: its id (usable at once with
// /cloud/{id}), externalId (the PVE volid), and data.volume with the tenant's name, size, status.
func TestVolumeCreateReturnsTheVolume(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := project.NewRepo(db)
	orgRepo := org.NewRepo(db)
	billingRepo := billing.NewRepo(db)
	users := user.NewRepo(db)
	cloudRepo := cloud.NewRepo(db)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("test-key"))
	h := project.NewHandler(project.NewService(repo, orgRepo, billingRepo, users, nil), project.NewPolicy(org.NewPolicy(orgRepo)),
		org.NewService(orgRepo, billingRepo, nil), users, audit.NewService(audit.NewRepo(db), slog.Default()), billingRepo,
		cloudRepo, esSvc, nil, nil, "r1")
	ps := volumePVE(t)
	h.SetProxmox(func(*externalservice.ExternalService) (*pve.Service, error) { return ps, nil })
	router := chi.NewRouter()
	h.Routes(router)

	const sub = "owner-sub"
	for coll, doc := range map[string]pgdoc.M{
		"users":                {"sub": sub, "email": "o@example.org"},
		"organization":         {"_id": "o1", "name": "o"},
		"organization_members": {"organizationId": "o1", "sub": sub, "roles": []string{"OWNER"}},
		"externalService": {"_id": "pve", "name": "pve", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
			"config": pgdoc.M{"provider": "proxmox", "storageHeadroomPct": float64(0),
				"storageProfiles": pgdoc.M{"std": pgdoc.M{"label": "Standard", "storages": pgdoc.M{"*": "local-zfs"}}}}},
		"project": {"_id": "p1", "name": "p", "status": project.StatusEnabled, "organizationId": "o1",
			"memberships": []pgdoc.M{{"sub": sub, "role": project.RoleOwner}},
			"services":    []any{pgdoc.M{"serviceId": "pve", "provider": "proxmox", "region": "r1", "pool": "pc-p1"}}},
	} {
		mustInsert(t, db, coll, doc)
	}

	req := httptest.NewRequest(http.MethodPost, "/project/p1/cloud",
		strings.NewReader(`{"type":"VOLUME","data":{"size":20,"name":"data-disk","serverId":"101"}}`))
	req.Header.Set("x-service-id", "pve")
	req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			ID         string `json:"id"`
			ExternalID string `json:"externalId"`
			Type       string `json:"type"`
			ProjectID  string `json:"projectId"`
			ServiceID  string `json:"serviceId"`
			Data       struct {
				Volume struct {
					Name   string `json:"name"`
					Size   int    `json:"size"`
					Status string `json:"status"`
					Slot   string `json:"slot"`
				} `json:"volume"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s: %v", w.Body.String(), err)
	}
	d := env.Data
	if d.ID == "" || d.ExternalID != "local-zfs:vm-101-disk-1" || d.Type != cloud.TypeVolume || d.ProjectID != "p1" || d.ServiceID != "pve" ||
		d.Data.Volume.Name != "data-disk" || d.Data.Volume.Size != 20 || d.Data.Volume.Status != "in-use" || d.Data.Volume.Slot != "scsi1" {
		t.Fatalf("create answered %s, want the new volume's row", w.Body.String())
	}
	// The id is the cache row's: it reads back as that volume, named.
	row, err := cloudRepo.FindByID(ctx, d.ID)
	if err != nil || row == nil || row.ExternalID != d.ExternalID {
		t.Fatalf("returned id %s does not name the stored row: %+v %v", d.ID, row, err)
	}
	if vol, _ := row.Data["volume"].(map[string]any); vol["name"] != "data-disk" || vol[pve.VolumeLabel] != "data-disk" {
		t.Fatalf("stored row lost the name: %v", row.Data)
	}
}

//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// consolePVE fakes one node with the pool pc-p1: VM 101 "web" on vnet v1 and VM 102 "db" on vnet v10. A route in
// routes ("METHOD /path" below /api2/json) answers first; any other read answers an empty list, any other write a task
// that ended OK. writes records every write, in order.
type consolePVE struct {
	mu     sync.Mutex
	routes map[string]func(w http.ResponseWriter, r *http.Request)
	writes []string
}

func (f *consolePVE) service(t *testing.T) *pve.Service {
	t.Helper()
	const upid = "UPID:pve1:0:0:0:x:101:u:"
	data := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")
		if r.Method != http.MethodGet {
			f.writes = append(f.writes, r.Method+" "+path)
		}
		if route, ok := f.routes[r.Method+" "+path]; ok {
			route(w, r)
			return
		}
		switch {
		case path == "/cluster/resources" && r.URL.Query().Get("type") == "storage":
			data(w, []map[string]any{{"type": "storage", "storage": "nas-backup", "node": "pve1", "status": "available", "maxdisk": 1 << 40, "disk": 0}})
		case path == "/cluster/resources":
			data(w, []map[string]any{
				{"id": "qemu/101", "type": "qemu", "vmid": 101, "name": "web", "node": "pve1", "pool": "pc-p1", "status": "running"},
				{"id": "qemu/102", "type": "qemu", "vmid": 102, "name": "db", "node": "pve1", "pool": "pc-p1", "status": "running"},
			})
		case path == "/cluster/status":
			data(w, []map[string]any{{"type": "node", "name": "pve1", "online": 1, "ip": "192.0.2.11"}})
		case path == "/nodes/pve1/network":
			data(w, []map[string]any{{"iface": "vmbr0", "type": "bridge", "cidr": "192.0.2.11/24", "address": "192.0.2.11", "gateway": "192.0.2.1"}})
		case path == "/storage":
			data(w, []map[string]any{{"storage": "nas-backup", "type": "nfs", "content": "backup", "shared": 1}})
		case path == "/nodes/pve1/qemu/101/status/current", path == "/nodes/pve1/qemu/102/status/current":
			data(w, map[string]any{"status": "running", "qmpstatus": "running", "name": "web", "uptime": 30})
		case path == "/nodes/pve1/qemu/101/config":
			data(w, map[string]any{"name": "web", "scsi0": "z:vm-101-disk-0,size=8G", "net0": "virtio=AA:BB:CC:00:00:01,bridge=v1,firewall=1", "vga": "serial0", "serial0": "socket"})
		case path == "/nodes/pve1/qemu/102/config":
			data(w, map[string]any{"name": "db", "scsi0": "z:vm-102-disk-0,size=8G", "net0": "virtio=AA:BB:CC:00:00:02,bridge=v10,firewall=1"})
		case strings.Contains(path, "/tasks/") && strings.HasSuffix(path, "/status"):
			data(w, map[string]any{"status": "stopped", "exitstatus": "OK"})
		case r.Method == http.MethodGet:
			data(w, []any{})
		default:
			data(w, upid)
		}
	}))
	t.Cleanup(srv.Close)
	ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// consoleSite is a project p1 of one owner on the Proxmox provider svc (config pveConfig) behind the real router, with
// the SERVER rows of VM 101 and 102 and the NETWORK row of vnet v1. call sends a POST as the owner.
func consoleSite(t *testing.T, f *consolePVE, pveConfig pgdoc.M) (repo *cloud.Repo, ids map[string]string, call func(path, body string, header ...string) (int, map[string]any)) {
	t.Helper()
	ctx := context.Background()
	const sub = "owner-sub"
	db := freshPG(t)
	orgRepo, billingRepo, users := org.NewRepo(db), billing.NewRepo(db), user.NewRepo(db)
	repo = cloud.NewRepo(db)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("test-key"))
	h := project.NewHandler(project.NewService(project.NewRepo(db), orgRepo, billingRepo, users, nil), project.NewPolicy(org.NewPolicy(orgRepo)),
		org.NewService(orgRepo, billingRepo, nil), users, audit.NewService(audit.NewRepo(db), slog.Default()), billingRepo, repo, esSvc, nil, nil, "r")
	ps := f.service(t)
	h.SetProxmox(func(*externalservice.ExternalService) (*pve.Service, error) { return ps, nil })
	h.SetSDNLock(lock.New(db))
	router := chi.NewRouter()
	h.Routes(router)
	pveConfig["provider"] = "proxmox"
	for coll, doc := range map[string]pgdoc.M{
		"users":                {"sub": sub, "email": "o@example.org"},
		"organization":         {"_id": "o1", "name": "o"},
		"organization_members": {"organizationId": "o1", "sub": sub, "roles": []string{"OWNER"}},
		"externalService":      {"_id": "svc", "name": "pve", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic, "config": pveConfig},
		"project": {"_id": "p1", "name": "p", "status": project.StatusEnabled, "organizationId": "o1",
			"memberships": []pgdoc.M{{"sub": sub, "role": project.RoleOwner}},
			"services": []any{
				pgdoc.M{"serviceId": "svc", "provider": "proxmox", "region": "r", "pool": "pc-p1"},
				pgdoc.M{"serviceId": "nas", "provider": "truenas", "region": "r"},
			}},
	} {
		mustInsert(t, db, coll, doc)
	}
	// The file server sits on the nodes' management network (192.0.2.0/24 in the fake).
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "nas", "name": "nas", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
		"config": pgdoc.M{"provider": "truenas", "apiUrl": "https://192.0.2.100", "parentDataset": "tank/pc"}})
	ids = map[string]string{}
	for _, row := range []cloud.CloudResource{
		{Type: cloud.TypeServer, ExternalID: "101", Data: map[string]any{"server": map[string]any{"name": "web"}}},
		{Type: cloud.TypeServer, ExternalID: "102", Data: map[string]any{"server": map[string]any{"name": "db"}}},
		{Type: cloud.TypeNetwork, ExternalID: "v1", Data: map[string]any{"network": map[string]any{"name": "net", "zone": "pc"}}},
	} {
		row.ServiceID, row.ProjectID, row.Region = "svc", "p1", "r"
		saved, err := repo.Insert(ctx, &row)
		if err != nil {
			t.Fatal(err)
		}
		ids[saved.ExternalID] = saved.ID
	}
	return repo, ids, func(path, body string, header ...string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/project/p1"+path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var env struct {
			Data   map[string]any `json:"data"`
			Errors map[string]any `json:"errors"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if env.Data == nil {
			env.Data = env.Errors
		}
		return w.Code, env.Data
	}
}

// The customer console defects of the production acceptance on v0.8.0 that an API answer decides
// (run-1/host105/acceptance-console.md, table "Product defects"). Each case is the request the console sends.
func TestCustomerConsoleAcceptanceDefects(t *testing.T) {
	ctx := context.Background()

	// D1: the network page's Servers tab asks GET_SERVERS, which a Proxmox provider answered with 400 "not supported".
	// The servers are the rows whose VM has a NIC on the vnet: 101 sits on v1, and 102 on v10, which "bridge=v1" is a
	// prefix of.
	t.Run("D1 GET_SERVERS lists the servers on the network", func(t *testing.T) {
		_, ids, call := consoleSite(t, &consolePVE{}, pgdoc.M{})
		code, data := call("/cloud/"+ids["v1"]+"/action", `{"action":"GET_SERVERS"}`)
		rows, _ := data["result"].([]any)
		if code != http.StatusOK || len(rows) != 1 {
			t.Fatalf("GET_SERVERS: %d %v", code, data)
		}
		if row, _ := rows[0].(map[string]any); row["id"] != ids["101"] {
			t.Fatalf("server on v1: %v, want row %s", rows[0], ids["101"])
		}
	})

	// D2: the Subnets tab reads data.network.subnetDetails, which only the sync wrote: on a site without the scheduled
	// sync a new network showed no subnet, so its SNAT could not be changed and the subnet not removed.
	t.Run("D2 a new network's row has its subnet details", func(t *testing.T) {
		f := &consolePVE{}
		repo, _, call := consoleSite(t, f, pgdoc.M{"network": pgdoc.M{
			"defaultZone": "pc", "cidrPool": "10.100.0.0/16", "zones": pgdoc.M{"pc": pgdoc.M{"kind": "simple"}},
		}})
		code, data := call("/cloud", `{"type":"NETWORK","data":{"name":"demo-net"}}`)
		if code != http.StatusOK {
			t.Fatalf("create network: %d %v (writes %v)", code, data, f.writes)
		}
		row, err := repo.FindByID(ctx, fmt.Sprint(data["id"]))
		if err != nil || row == nil {
			t.Fatalf("the network's row: %v, %v", row, err)
		}
		net, _ := row.Data["network"].(map[string]any)
		details, _ := net["subnetDetails"].([]any)
		if len(details) != 1 {
			t.Fatalf("subnetDetails: %v", net)
		}
		if d, _ := details[0].(map[string]any); d["cidr"] != "10.100.0.0/24" || d["gateway"] != "10.100.0.1" || d["snat"] != true || d["cidr"] != net["cidr"] {
			t.Fatalf("subnet detail %v of network %v", details[0], net)
		}
	})

	// D8: a snapshot delete 4 s after a rollback and its restart were done failed on production with the node task
	// "can't lock file ... got timeout" (PVE waits 10 s for the VM's config lock), which the API answers as 409 busy.
	// The delete asks again, three times in all.
	t.Run("D8 a snapshot delete waits for the config lock", func(t *testing.T) {
		for _, tt := range []struct {
			name   string
			locked int // how many delete tasks end on the lock timeout
			code   int
			tries  int
		}{
			{"lock free on the third try", 2, http.StatusOK, 3},
			{"lock held throughout", 9, http.StatusConflict, 3},
		} {
			t.Run(tt.name, func(t *testing.T) {
				f := &consolePVE{}
				deletes := 0
				f.routes = map[string]func(http.ResponseWriter, *http.Request){
					"DELETE /nodes/pve1/qemu/101/snapshot/t-snap1": func(w http.ResponseWriter, _ *http.Request) {
						deletes++
						fmt.Fprintf(w, `{"data":"UPID:pve1:0:0:0:qmdelsnapshot:101:u:%d:"}`, deletes)
					},
				}
				for n := 1; n <= tt.locked; n++ {
					f.routes[fmt.Sprintf("GET /nodes/pve1/tasks/UPID:pve1:0:0:0:qmdelsnapshot:101:u:%d:/status", n)] = func(w http.ResponseWriter, _ *http.Request) {
						fmt.Fprint(w, `{"data":{"status":"stopped","exitstatus":"can't lock file '/var/lock/qemu-server/lock-101.conf' - got timeout"}}`)
					}
				}
				_, ids, call := consoleSite(t, f, pgdoc.M{})
				code, data := call("/cloud/"+ids["101"]+"/action", `{"action":"DELETE_SNAPSHOT","data":{"name":"t-snap1"}}`)
				if code != tt.code || deletes != tt.tries {
					t.Fatalf("delete: %d %v after %d tries, want %d after %d", code, data, deletes, tt.code, tt.tries)
				}
				if msg := fmt.Sprint(data["message"]); tt.code == http.StatusConflict && !strings.Contains(msg, "busy with another operation") {
					t.Fatalf("refusal: %v", data)
				}
			})
		}
	})

	// D12: CREATE_BACKUP answered {"id":""}. The answer names the archive vzdump just wrote: the newest of this server
	// and this project on the store.
	t.Run("D12 CREATE_BACKUP answers with the new archive's id", func(t *testing.T) {
		f := &consolePVE{}
		archives := `{"volid":"nas-backup:backup/old","ctime":10,"size":5,"notes":"proxcloud project=p1 server=web"}`
		f.routes = map[string]func(http.ResponseWriter, *http.Request){
			"GET /nodes/pve1/storage/nas-backup/content": func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"data":[%s]}`, archives)
			},
			"POST /nodes/pve1/vzdump": func(w http.ResponseWriter, _ *http.Request) {
				archives += `,{"volid":"nas-backup:backup/new","ctime":20,"size":6,"notes":"proxcloud project=p1 server=web"}` +
					`,{"volid":"nas-backup:backup/theirs","ctime":30,"size":7,"notes":"proxcloud project=p11 server=web"}`
				fmt.Fprint(w, `{"data":"UPID:pve1:0:0:0:vzdump:101:u:"}`)
			},
		}
		_, ids, call := consoleSite(t, f, pgdoc.M{"backupStorage": "nas-backup"})
		code, data := call("/cloud/"+ids["101"]+"/action", `{"action":"CREATE_BACKUP"}`)
		result, _ := data["result"].(map[string]any)
		if code != http.StatusOK || result["id"] != "nas-backup:backup/new" {
			t.Fatalf("CREATE_BACKUP: %d %v", code, data)
		}
	})

	// D9: a share made on a file server inside a network the compute provider keeps tenants out of cannot be mounted
	// from the project's networks. The create dialog asks first; 192.0.2.100 is on the node's management network.
	t.Run("D9 the share provider says when tenant networks cannot reach it", func(t *testing.T) {
		_, _, call := consoleSite(t, &consolePVE{}, pgdoc.M{})
		code, data := call("/cloud/action", `{"action":"LIST_SHARE_WARNINGS"}`, "x-service-id", "nas")
		warnings, _ := data["result"].([]any)
		if code != http.StatusOK || len(warnings) != 1 || !strings.Contains(fmt.Sprint(warnings[0]), "192.0.2.100") {
			t.Fatalf("LIST_SHARE_WARNINGS: %d %v", code, data)
		}
	})

	// D13 and the console's first tab: the page asks what its socket cannot tell it. VM 101 shows its display on the
	// serial port (vga serial0), VM 102 does not.
	t.Run("GET_CONSOLE_STATE says where the display is and since when the server runs", func(t *testing.T) {
		_, ids, call := consoleSite(t, &consolePVE{}, pgdoc.M{})
		for vmid, serial := range map[string]bool{"101": true, "102": false} {
			code, data := call("/cloud/"+ids[vmid]+"/action", `{"action":"GET_CONSOLE_STATE"}`)
			result, _ := data["result"].(map[string]any)
			if code != http.StatusOK || result["serialDisplay"] != serial || result["status"] != "running" || result["uptime"] != float64(30) {
				t.Fatalf("GET_CONSOLE_STATE of %s: %d %v", vmid, code, data)
			}
		}
	})
}

//go:build integration

package project

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// A SERVER row lost its pveUUID after a Stop then Start from the portal (seen on a site on v0.10.0). The row is
// refreshed after every action, and on every look at the server, from what Proxmox says about the VM now; that read
// does not ask for the smbios uuid or the `meta` identity, which only the sync reads, and it replaced the whole row.
// The fake answers as Proxmox does for a stopped and for a started VM: /cluster/resources a few seconds behind,
// status/current with qmpstatus (and a pid only while running), the guest agent refusing a stopped VM, the snapshot
// list with its `current` entry, and a config whose vga line and digest changed at the Start (the display write of
// v0.10.0), which never held a pveUUID key.
func TestPVEServerRefreshKeepsTheRowIdentity(t *testing.T) {
	ctx := context.Background()
	pgc, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("srv"), tcpostgres.WithUsername("u"), tcpostgres.WithPassword("p"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = testcontainers.TerminateContainer(pgc) }()
	dsn, _ := pgc.ConnectionString(ctx, "sslmode=disable")
	db, err := pgdoc.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)

	const uuid, identity = "d30166a5-3c1e-4b6f-9d55-0f6d3a1b2c4e", "m-1763604000"
	var mu sync.Mutex
	running := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		up := running
		mu.Unlock()
		const sizes = `"cpus":2,"maxmem":4294967296,"maxdisk":21474836480,"tags":"proxcloud","name":"demo-srv1","vmid":111`
		switch r.Method + " " + r.URL.Path {
		case "GET /api2/json/cluster/resources": // lags the task: it still shows the state before the action
			was := "running"
			if up {
				was = "stopped"
			}
			fmt.Fprintf(w, `{"data":[{"id":"qemu/111","type":"qemu","vmid":111,"node":"pve","name":"demo-srv1","status":%q,"pool":"pc-p1","template":0,"tags":"proxcloud","maxcpu":2,"maxmem":4294967296,"maxdisk":21474836480}]}`, was)
		case "GET /api2/json/nodes/pve/qemu/111/status/current":
			if up {
				fmt.Fprint(w, `{"data":{"status":"running","qmpstatus":"running","pid":932566,"uptime":4,"agent":1,"running-qemu":"10.0.2","ha":{"managed":0},`+sizes+`}}`)
			} else {
				fmt.Fprint(w, `{"data":{"status":"stopped","qmpstatus":"stopped","uptime":0,"agent":1,"ha":{"managed":0},`+sizes+`}}`)
			}
		case "GET /api2/json/nodes/pve/qemu/111/config":
			vga, digest := "serial0", "0d5a3f6c1f0e7a9b2c4d6e8f0a1b3c5d7e9f1a2b"
			if up { // the Start wrote vga std on the stopped VM first
				vga, digest = "std", "7c1e9b3a5d2f4e6a8b0c1d3e5f7a9b2c4d6e8f01"
			}
			fmt.Fprintf(w, `{"data":{"name":"demo-srv1","tags":"proxcloud","cores":2,"memory":"4096","agent":"1","smbios1":"uuid=%s","meta":"creation-qemu=10.0.2,ctime=1763604000","vga":%q,"serial0":"socket","scsi0":"local-lvm:vm-111-disk-0,size=20G","net0":"virtio=BC:24:11:00:00:6F,bridge=v1","ipconfig0":"ip=10.100.0.11/24,gw=10.100.0.1","boot":"order=scsi0","digest":%q}}`, uuid, vga, digest)
		case "GET /api2/json/nodes/pve/qemu/111/agent/network-get-interfaces":
			if !up {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"data":null,"message":"VM 111 is not running\n"}`)
				return
			}
			fmt.Fprint(w, `{"data":{"result":[{"name":"lo","ip-addresses":[{"ip-address":"127.0.0.1","ip-address-type":"ipv4","prefix":8}]},{"name":"eth0","hardware-address":"bc:24:11:00:00:6f","ip-addresses":[{"ip-address":"10.100.0.11","ip-address-type":"ipv4","prefix":24}]}]}}`)
		case "GET /api2/json/nodes/pve/qemu/111/snapshot":
			fmt.Fprint(w, `{"data":[{"name":"current","description":"You are here!","running":0}]}`)
		case "GET /api2/json/cluster/replication":
			fmt.Fprint(w, `{"data":[]}`)
		default:
			t.Errorf("reached PVE: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer srv.Close()
	ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cloud: cloud.NewRepo(db)}
	// The row as the sync wrote it while the VM ran.
	cr := &cloud.CloudResource{ServiceID: "svc", ProjectID: "p1", Type: cloud.TypeServer, ExternalID: "111",
		Data: map[string]any{"pveUUID": uuid, "identity": identity, "flavorName": "", "volumeBacked": true,
			"server": map[string]any{"name": "demo-srv1", "status": "ACTIVE", "host": "pve"}}}
	for _, step := range []struct {
		up     bool
		status string
	}{{false, "SHUTOFF"}, {true, "ACTIVE"}} {
		mu.Lock()
		running = step.up
		mu.Unlock()
		h.pveRefreshServerRow(ctx, ps, &Project{ID: "p1"}, cr)
		stored, err := h.cloud.FindByID(ctx, cr.ID)
		if err != nil || stored == nil {
			t.Fatalf("stored row: %v, %v", stored, err)
		}
		server, _ := stored.Data["server"].(map[string]any)
		if server["status"] != step.status {
			t.Fatalf("running %v: the row was not refreshed: %v", step.up, server)
		}
		if stored.Data["pveUUID"] != uuid || stored.Data["identity"] != identity {
			t.Fatalf("running %v: the refresh dropped the row's identity: pveUUID %v, identity %v", step.up, stored.Data["pveUUID"], stored.Data["identity"])
		}
	}
}

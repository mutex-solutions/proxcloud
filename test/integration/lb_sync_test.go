//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/cloud/syncjob"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// syncVM is one VM of the Proxmox cluster syncWire fakes, as the v0.11.0-rc.1 drill met them (N2-1).
type syncVM struct {
	pool, tags, own string   // its project pool, its tags, the address the product configured on net0
	power           string   // "" running, "stopped" or "paused": Proxmox says so, and the guest agent of such a VM answers nobody
	agentRefuses    bool     // it runs and its agent does not answer (403)
	extra           []string // what its running guest reports on top of own: a pair's address on the master, a container bridge
	role            string   // an appliance's keepalived state while it runs
	starts          int
}

// syncWire is one Proxmox cluster with two project pools, as far as a services sync and the load balancer pass reach:
// the inventory proof of each project (page, pool, nodes, permissions, config, pending, status, agent), the start of a
// VM, and an appliance's firewall and guest. Every write is logged; one the fake does not know fails the test.
type syncWire struct {
	t      *testing.T
	mu     sync.Mutex
	vms    map[int]*syncVM
	writes []string
}

func (f *syncWire) wrote(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, w := range f.writes {
		if strings.HasPrefix(w, prefix) {
			n++
		}
	}
	return n
}

func (f *syncWire) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	write := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	ids := make([]int, 0, len(f.vms))
	for id := range f.vms {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+path)
	}
	if vmPath, ok := strings.CutPrefix(path, "/nodes/n1/qemu/"); ok {
		idText, rest, _ := strings.Cut(vmPath, "/")
		id, _ := strconv.Atoi(idText)
		vm := f.vms[id]
		if vm == nil {
			w.WriteHeader(500)
			return
		}
		mac := func(nic int) string { return fmt.Sprintf("BC:24:11:00:%02d:%02d", nic, id%100) }
		if strings.HasPrefix(rest, "agent/") {
			switch {
			case vm.power != "":
				w.WriteHeader(500)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": nil, "message": "QEMU guest agent is not running\n"})
				return
			case vm.agentRefuses:
				w.WriteHeader(403)
				return
			}
		}
		switch r.Method + " " + rest {
		case "GET config":
			cfg := map[string]any{"name": fmt.Sprintf("vm-%d", id), "smbios1": fmt.Sprintf("uuid=11111111-1111-4111-8111-%012d", id), "tags": vm.tags,
				"template": 0, "vga": "std", "scsi0": fmt.Sprintf("z:vm-%d-disk-0,size=8G", id), "net0": "virtio=" + mac(0) + ",bridge=v1", "ipconfig0": "ip=" + vm.own + "/24,gw=10.0.0.1"}
			if pve.LBOfTags(vm.tags) != "" { // a pair's public NIC: no address of its own
				cfg["net1"] = "virtio=" + mac(1) + ",bridge=vmbr1"
			}
			write(cfg)
		case "GET pending", "GET snapshot", "GET firewall/rules", "GET firewall/ipset/ipfilter-net1":
			write([]any{})
		case "GET firewall/ipset":
			write([]any{map[string]any{"name": "ipfilter-net1"}})
		case "GET status/current":
			switch vm.power {
			case "stopped":
				write(map[string]any{"status": "stopped", "qmpstatus": "stopped"})
			case "paused":
				write(map[string]any{"status": "running", "qmpstatus": "paused"})
			default:
				write(map[string]any{"status": "running", "qmpstatus": "running"})
			}
		case "POST status/start":
			vm.power = ""
			vm.starts++
			write(fmt.Sprintf("UPID:n1:00000001:00000002:00000003:qmstart:%d:t@pve:", id))
		case "GET agent/network-get-interfaces":
			addrs := func(ips ...string) []any {
				out := []any{}
				for _, ip := range ips {
					out = append(out, map[string]any{"ip-address": ip, "ip-address-type": "ipv4", "prefix": 24})
				}
				return out
			}
			write(map[string]any{"result": []any{
				map[string]any{"name": "eth0", "hardware-address": mac(0), "ip-addresses": addrs(vm.own)},
				map[string]any{"name": "eth1", "hardware-address": mac(1), "ip-addresses": addrs(vm.extra...)},
			}})
		case "POST agent/exec":
			write(map[string]any{"pid": id})
		case "GET agent/exec-status": // every guest command succeeds; a state read hears the appliance's role
			holds := 0
			if vm.role == "MASTER" {
				holds = 1
			}
			write(map[string]any{"exited": 1, "exitcode": 0, "out-data": fmt.Sprintf("%s 1 %d 1", vm.role, holds)})
		case "POST agent/file-write", "POST firewall/rules", "POST firewall/ipset/ipfilter-net1", "PUT firewall/options", "PUT config":
			write(nil)
		default:
			if r.Method != http.MethodGet {
				f.t.Errorf("unexpected write %s %s", r.Method, path)
			}
			write([]any{})
		}
		return
	}
	switch {
	case path == "/cluster/resources":
		if r.URL.Query().Get("type") == "node" {
			write([]any{map[string]any{"type": "node", "node": "n1", "status": "online"}})
			return
		}
		rows := []any{}
		for _, id := range ids {
			vm := f.vms[id]
			status := "running"
			if vm.power == "stopped" {
				status = "stopped"
			}
			rows = append(rows, map[string]any{"id": fmt.Sprintf("qemu/%d", id), "type": "qemu", "vmid": id, "node": "n1", "pool": vm.pool,
				"name": fmt.Sprintf("vm-%d", id), "status": status, "template": 0, "tags": vm.tags, "maxcpu": 2, "maxmem": int64(2 << 30), "maxdisk": int64(8 << 30)})
		}
		write(rows)
	case path == "/nodes":
		write([]any{map[string]any{"node": "n1", "status": "online"}})
	case path == "/cluster/status":
		write([]any{map[string]any{"type": "cluster", "quorate": 1}, map[string]any{"type": "node", "name": "n1", "online": 1, "ip": "192.0.2.11"}})
	case path == "/nodes/n1/network":
		write([]any{map[string]any{"iface": "vmbr0", "address": "192.0.2.11", "gateway": "192.0.2.1", "cidr": "192.0.2.11/24"}})
	case strings.HasPrefix(path, "/pools/"):
		pool := strings.TrimPrefix(path, "/pools/")
		members := []any{}
		for _, id := range ids {
			if f.vms[id].pool == pool {
				members = append(members, map[string]any{"type": "qemu", "id": fmt.Sprintf("qemu/%d", id), "vmid": id, "node": "n1"})
			}
		}
		write(map[string]any{"poolid": pool, "members": members})
	case path == "/access/permissions":
		p := r.URL.Query().Get("path")
		write(map[string]any{p: map[string]int{"VM.Audit": 1, "Pool.Audit": 1, "Sys.Audit": 1, "SDN.Audit": 1}})
	case strings.HasPrefix(path, "/nodes/n1/tasks/"):
		write(map[string]any{"status": "stopped", "exitstatus": "OK"})
	case r.Method == http.MethodPost && path == "/cluster/firewall/rules":
		write(nil)
	default:
		if r.Method != http.MethodGet {
			f.t.Errorf("unexpected write %s %s", r.Method, path)
		}
		write([]any{})
	}
}

// N2-1 of the v0.11.0-rc.1 drill, at the services sync (D3: "the sync restarts a stopped appliance" failed in 5 of 6
// tries). A pair's master held the balancer's address when a sync read its guest agent, so its SERVER row cached it;
// then the VM was stopped. Its project's inventory was untrusted at every sync from then on, and the sync left out
// the load balancer pass, the one that starts the VM, and with it everything else it does for the provider.
//
// "the drill": one sync starts the stopped master, takes the address out of its row, and trusts the provider.
// "another project is unknown": a tenant's running VM whose agent does not answer keeps its own project's rows as they
// are, which is the design (docs/vm-inventory-safety.md), and holds back the seed sweep. The balancer of the other
// project is still repaired, that project is still synced and gets its isolation rules, and the worker firewall pass
// still runs, whichever of the two projects the sync reaches first.
func TestSyncRepairsAStoppedApplianceWhateverElseIsUnknown(t *testing.T) {
	const vip, dynamic = "198.51.100.9", "172.17.0.1"
	for _, tc := range []struct {
		name         string
		projects     []string // in the order the sync reaches them
		agentRefuses bool
	}{
		{"the drill", []string{"pl", "pu"}, false},
		{"another project is unknown, synced first", []string{"pu", "pl"}, true},
		{"another project is unknown, synced last", []string{"pl", "pu"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := freshPG(t)
			repo := cloud.NewRepo(db)
			f := &syncWire{t: t, vms: map[int]*syncVM{
				101: {pool: "pc-pu", tags: "proxcloud", own: "10.0.0.5", extra: []string{dynamic}, agentRefuses: tc.agentRefuses},
				301: {pool: "pc-pl", tags: "proxcloud;pc-lb-abc", own: "10.0.0.31", role: "MASTER", extra: []string{vip}}, // it took over
				302: {pool: "pc-pl", tags: "proxcloud;pc-lb-abc", own: "10.0.0.32", role: "BACKUP", power: "stopped"},
			}}
			srv := httptest.NewServer(f)
			t.Cleanup(srv.Close)
			mustInsert(t, db, "externalService", pgdoc.M{"_id": "s", "name": "fixture", "type": "CLOUD", "status": "PUBLIC", "config": pgdoc.M{
				"provider": "proxmox", "apiUrl": srv.URL, "region": "r", "poolPrefix": "pc-", "network": pgdoc.M{"cidrPool": "10.100.0.0/16",
					"publicNetworks": []any{pgdoc.M{"id": "vmbr1", "cidr": "198.51.100.0/24", "gateway": "198.51.100.1", "ranges": []any{"198.51.100.5-198.51.100.20"}}}}}})
			for _, id := range tc.projects {
				mustInsert(t, db, "project", pgdoc.M{"_id": id, "name": id, "status": "ENABLED", "services": []any{pgdoc.M{"serviceId": "s"}}})
			}
			past := time.Now().UTC().Add(-time.Hour)
			addrs := func(ips ...string) map[string]any {
				list := []any{}
				for _, ip := range ips {
					list = append(list, map[string]any{"addr": ip, "version": 4})
				}
				return map[string]any{"private": list}
			}
			rows := map[string]*cloud.CloudResource{}
			for id, row := range map[string]struct {
				project string
				addrs   map[string]any
			}{
				"101": {"pu", addrs("10.0.0.5", dynamic)},
				"301": {"pl", addrs("10.0.0.31")},
				"302": {"pl", addrs("10.0.0.32", vip)}, // what the last sync's agent read left: the master held the address
			} {
				n, _ := strconv.Atoi(id)
				cr, err := repo.Insert(ctx, &cloud.CloudResource{ProjectID: row.project, ServiceID: "s", ExternalID: id, Region: "r", Type: cloud.TypeServer,
					CreatedAt: &past, UpdatedAt: &past, Data: map[string]any{"pveUUID": fmt.Sprintf("11111111-1111-4111-8111-%012d", n),
						"server": map[string]any{"status": "ACTIVE", "host": "n1", "addresses": row.addrs}}})
				if err != nil {
					t.Fatal(err)
				}
				rows[id] = cr
			}
			// The pair as its last apply left it: 302 the master, holding the address.
			if _, err := repo.Insert(ctx, &cloud.CloudResource{ProjectID: "pl", ServiceID: "s", ExternalID: "lb-abc", Region: "r", Type: cloud.TypeLoadBalancer,
				CreatedAt: &past, UpdatedAt: &past, Data: map[string]any{"loadbalancer": map[string]any{
					"id": "abc", "name": "web", "status": "ACTIVE", "networkId": "v1", "publicNetworkId": "vmbr1", "ha": true, "vips": []any{vip},
					"privateIp": "10.0.0.31", "privatePrefix": 24,
					"appliances": []any{
						map[string]any{"serverId": "301", "node": "n1", "privateIp": "10.0.0.31", "tenantIface": "eth0", "publicIface": "eth1", "role": "BACKUP", "state": "up"},
						map[string]any{"serverId": "302", "node": "n1", "privateIp": "10.0.0.32", "tenantIface": "eth0", "publicIface": "eth1", "role": "MASTER", "holdsVip": true, "state": "up"},
					}}}}); err != nil {
				t.Fatal(err)
			}
			ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "portal@pve!sync", Secret: "key"}, "s", "pc-")
			if err != nil {
				t.Fatal(err)
			}
			pveFor := func(*externalservice.ExternalService) (*pve.Service, error) { return ps, nil }
			es := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("k"))
			locks := lock.New(db)
			h := project.NewHandler(project.NewService(project.NewRepo(db), nil, nil, nil, nil), nil, nil, nil, nil, nil, repo, es, nil, nil, "")
			h.SetProxmox(pveFor)
			h.SetSDNLock(locks)
			h.SetPublicIPs(project.NewPublicIPRepo(db))
			seeds, workerFW := 0, 0
			j := syncjob.New(project.NewRepo(db), es, repo, nil).WithProxmoxFactory(pveFor).WithSDNLock(locks).
				WithLoadBalancers(h.ReconcileLoadBalancers).
				WithStaleSeeds(func(context.Context, *pve.Service, *externalservice.ExternalService) error { seeds++; return nil }).
				WithWorkerFirewalls(func(context.Context, *pve.Service, *externalservice.ExternalService) error { workerFW++; return nil })

			if _, err := j.Run(ctx); err != nil {
				t.Fatal(err)
			}

			// The load balancer pass ran and started the stopped master; its row says what is going on.
			if f.vms[302].starts != 1 {
				t.Fatalf("the stopped master was started %d times, want once\nwrites: %v", f.vms[302].starts, f.writes)
			}
			lb, err := repo.FindByServiceIDAndExternalID(ctx, "s", "lb-abc")
			if err != nil || lb == nil {
				t.Fatalf("balancer row: %v", err)
			}
			data, _ := lb.Data["loadbalancer"].(map[string]any)
			if msg, _ := data["statusMessage"].(string); data["status"] == "ACTIVE" || !strings.Contains(msg, "appliance 302: its VM is stopped; starting it") {
				t.Fatalf("balancer row after the pass: %v %q", data["status"], msg)
			}
			// The master's SERVER row was read, not kept: it is off, and the balancer's address is out of it.
			if got := serverAddrs(t, repo, rows["302"].ID); !slices.Equal(got, []string{"10.0.0.32"}) {
				t.Fatalf("the master's row still holds %v", got)
			}
			if row, _ := repo.FindByID(ctx, rows["302"].ID); row.Data["server"].(map[string]any)["status"] != "SHUTOFF" {
				t.Fatalf("the master's row: status %v", row.Data["server"].(map[string]any)["status"])
			}
			if got := serverAddrs(t, repo, rows["301"].ID); !slices.Equal(got, []string{"10.0.0.31"}) {
				t.Fatalf("the new master's row took the balancer's address: %v", got)
			}
			// The project of the balancer got its isolation rules, and the provider's other passes ran.
			if f.wrote("POST /cluster/firewall/rules") == 0 || workerFW != 1 {
				t.Fatalf("isolation rule writes %d, worker firewall passes %d\nwrites: %v", f.wrote("POST /cluster/firewall/rules"), workerFW, f.writes)
			}
			if !tc.agentRefuses {
				if seeds != 1 {
					t.Fatalf("a stopped appliance made the provider untrusted: seed sweeps %d", seeds)
				}
				if got := serverAddrs(t, repo, rows["101"].ID); !slices.Equal(got, []string{"10.0.0.5", dynamic}) {
					t.Fatalf("the tenant VM's row: %v", got)
				}
				// The next sync finds the pair whole again, and the address stays out of both rows.
				if _, err := j.Run(ctx); err != nil {
					t.Fatal(err)
				}
				lb, _ = repo.FindByServiceIDAndExternalID(ctx, "s", "lb-abc")
				if data, _ := lb.Data["loadbalancer"].(map[string]any); data["status"] != "ACTIVE" {
					t.Fatalf("after the restart: %v %v", data["status"], data["statusMessage"])
				}
				for id, own := range map[string]string{"301": "10.0.0.31", "302": "10.0.0.32"} {
					if got := serverAddrs(t, repo, rows[id].ID); !slices.Equal(got, []string{own}) {
						t.Fatalf("appliance %s's row: %v", id, got)
					}
				}
				return
			}
			// The unknown VM's project keeps its row exactly, and the one pass that deletes on a row's absence waits.
			kept, err := repo.FindByID(ctx, rows["101"].ID)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(rows["101"])
			after, _ := json.Marshal(kept)
			if string(before) != string(after) || seeds != 0 {
				t.Fatalf("the unknown project: seed sweeps %d, row\n%s\nwant\n%s", seeds, after, before)
			}
		})
	}
}

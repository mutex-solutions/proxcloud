//go:build integration

package project

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

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
)

// pairWire fakes the Proxmox API around two appliances, 301 on n1 and 302 on n2, as far as an apply reaches: their
// state, config and NICs, their firewall, and the guest agent. It logs every write in order, and answers the VRRP
// state read from vrrp[vmid].
type pairWire struct {
	t     *testing.T
	tag   string // the appliances' balancer tag, pc-lb-abc when empty
	gone  bool   // no VM is listed any more
	mu    sync.Mutex
	log   []string
	rules map[string][]pve.FirewallRule
	sets  map[string][]string
	vrrp  map[string]string
	runs  map[int]string
	files []string // the HAProxy configs written, in order
}

func (f *pairWire) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	write := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	if r.URL.Path == "/api2/json/cluster/resources" {
		tag := f.tag
		if tag == "" {
			tag = "pc-lb-abc"
		}
		if f.gone {
			write([]map[string]any{})
			return
		}
		write([]map[string]any{
			{"id": "qemu/301", "type": "qemu", "vmid": 301, "node": "n1", "pool": "pc-p1", "status": "running", "tags": "proxcloud;" + tag},
			{"id": "qemu/302", "type": "qemu", "vmid": 302, "node": "n2", "pool": "pc-p1", "status": "running", "tags": "proxcloud;" + tag},
		})
		return
	}
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/api2/json/nodes/"), "/") // n, qemu, vmid, ...
	if len(p) < 4 || p[1] != "qemu" {
		f.t.Errorf("reached PVE: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	vm, rest := p[2], strings.Join(p[3:], "/")
	mac := func(nic string) string { return fmt.Sprintf("bc:24:11:00:%s:%s", nic, vm[1:]) }
	switch r.Method + " " + rest {
	case "GET status/current":
		write(map[string]any{"status": "running", "qmpstatus": "running"})
	case "GET config":
		write(map[string]any{"net0": "virtio=" + mac("00") + ",bridge=v1", "net1": "virtio=" + mac("01") + ",bridge=vmbr1"})
	case "GET agent/network-get-interfaces":
		write(map[string]any{"result": []map[string]any{{"name": "lo"}, {"name": "ens18", "hardware-address": mac("00")}, {"name": "ens19", "hardware-address": mac("01")}}})
	case "GET firewall/rules":
		out := slices.Clone(f.rules[vm])
		for i := range out {
			out[i].Pos = i
		}
		write(out)
	case "POST firewall/rules":
		f.rules[vm] = append([]pve.FirewallRule{{Type: r.Form.Get("type"), Action: r.Form.Get("action"), Proto: r.Form.Get("proto"),
			Source: r.Form.Get("source"), Dest: r.Form.Get("dest"), DPort: r.Form.Get("dport"), Comment: r.Form.Get("comment")}}, f.rules[vm]...)
		f.log = append(f.log, vm+" rule "+r.Form.Get("proto"))
		write(nil)
	case "GET firewall/ipset":
		write([]map[string]any{{"name": "ipfilter-net1"}})
	case "GET firewall/ipset/ipfilter-net1":
		var out []map[string]any
		for _, c := range f.sets[vm] {
			out = append(out, map[string]any{"cidr": c})
		}
		write(out)
	case "POST firewall/ipset/ipfilter-net1":
		f.sets[vm] = append(f.sets[vm], r.Form.Get("cidr"))
		f.log = append(f.log, vm+" ipset "+r.Form.Get("cidr"))
		write(nil)
	case "POST agent/file-write":
		if strings.HasSuffix(r.Form.Get("file"), "haproxy.cfg.new") {
			f.files = append(f.files, r.Form.Get("content"))
		}
		f.log = append(f.log, vm+" write "+r.Form.Get("file"))
		write(nil)
	case "POST agent/exec":
		cmd := strings.Join(r.Form["command"], " ")
		pid := len(f.runs) + 1
		out := ""
		switch {
		case strings.Contains(cmd, "proxcloud-lb.state"):
			out = f.vrrp[vm]
		case strings.Contains(cmd, "keepalived -t"):
			f.log = append(f.log, vm+" keepalived reload")
		case strings.Contains(cmd, "haproxy -c"):
			f.log = append(f.log, vm+" haproxy reload")
		}
		f.runs[pid] = out
		write(map[string]any{"pid": pid})
	case "GET agent/exec-status":
		pid, _ := strconv.Atoi(r.Form.Get("pid"))
		write(map[string]any{"exited": 1, "exitcode": 0, "out-data": f.runs[pid]})
	default:
		switch {
		case r.Method == http.MethodDelete && strings.HasPrefix(rest, "firewall/rules/"):
			pos, _ := strconv.Atoi(strings.TrimPrefix(rest, "firewall/rules/"))
			f.rules[vm] = slices.Delete(f.rules[vm], pos, pos+1)
			f.log = append(f.log, vm+" rule delete")
			write(nil)
		case r.Method == http.MethodDelete && strings.HasPrefix(rest, "firewall/ipset/ipfilter-net1/"):
			cidr := strings.TrimPrefix(rest, "firewall/ipset/ipfilter-net1/")
			f.sets[vm] = slices.DeleteFunc(f.sets[vm], func(c string) bool { return strings.TrimSuffix(c, "/32") == cidr })
			f.log = append(f.log, vm+" ipset delete "+cidr)
			write(nil)
		default:
			f.t.Errorf("reached PVE: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}
}

func (f *pairWire) first(entry string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Index(f.log, entry)
}

// One apply of a pair: the vrrp rule and the VIP in both appliances' sets before keepalived anywhere (C14), the
// standby before the appliance holding the address, ACTIVE when one of two answering appliances holds it; nothing
// written when nothing changed; DEGRADED with the split named when both hold it.
func TestLBPairApplyOrderAndStatus(t *testing.T) {
	ctx := context.Background()
	pgc, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("lb"), tcpostgres.WithUsername("u"), tcpostgres.WithPassword("p"), tcpostgres.BasicWaitStrategies())
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

	f := &pairWire{t: t, rules: map[string][]pve.FirewallRule{}, sets: map[string][]string{},
		vrrp: map[string]string{"301": "BACKUP 1 0", "302": "MASTER 1 1"}, runs: map[int]string{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cloud: cloud.NewRepo(db), sdnLock: lock.New(db), publicIPs: NewPublicIPRepo(db),
		pveFor: func(*externalservice.ExternalService) (*pve.Service, error) { return ps, nil }}
	es := &externalservice.ExternalService{ID: "svc", Config: map[string]any{"network": map[string]any{"publicNetworks": []any{
		map[string]any{"id": "vmbr1", "cidr": "203.0.113.0/24", "gateway": "203.0.113.1", "ranges": []any{"203.0.113.10-203.0.113.20"}}}}}}
	proj := &Project{ID: "p1"}
	lb := &loadBalancer{ID: "abc", Name: "web", Status: lbStatusBuild, NetworkID: "v1", PublicNetworkID: "vmbr1", HA: true,
		ServerID: "301", PrivateIP: "10.0.0.31", PrivatePrefix: 24, PublicIP: "203.0.113.10", VIPs: []string{"203.0.113.10"},
		Appliances: []lbAppliance{{ServerID: "301", PrivateIP: "10.0.0.31"}, {ServerID: "302", PrivateIP: "10.0.0.32", HoldsVIP: true}},
		LBSpec:     pve.LBSpec{Listeners: []pve.LBListener{{Port: 80, Pool: "web"}}, Pools: []pve.LBPool{{Name: "web", Protocol: "http", Algorithm: "roundrobin"}}}}
	row := &cloud.CloudResource{ExternalID: "lb-abc", ServiceID: "svc", ProjectID: "p1", Type: cloud.TypeLoadBalancer}
	if err := setLBRow(row, lb); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cloud.Insert(ctx, row); err != nil {
		t.Fatal(err)
	}

	status, err := h.pveLBApply(ctx, es, proj, "lb-abc")
	if err != nil || status != lbStatusActive {
		t.Fatalf("apply: %s %v\n%s", status, err, strings.Join(f.log, "\n"))
	}
	t.Logf("writes of the first apply:\n%s", strings.Join(f.log, "\n"))
	firstKeepalived := min(f.first("301 write /etc/keepalived/keepalived.conf.new"), f.first("302 write /etc/keepalived/keepalived.conf.new"))
	for _, before := range []string{"301 rule vrrp", "302 rule vrrp", "301 ipset 203.0.113.10", "302 ipset 203.0.113.10"} {
		if i := f.first(before); i < 0 || i > firstKeepalived {
			t.Errorf("%q at %d, keepalived first written at %d:\n%s", before, i, firstKeepalived, strings.Join(f.log, "\n"))
		}
	}
	if f.first("301 keepalived reload") > f.first("302 keepalived reload") || f.first("301 haproxy reload") > f.first("302 haproxy reload") {
		t.Errorf("the appliance holding the address was pushed before the standby:\n%s", strings.Join(f.log, "\n"))
	}
	for _, vm := range []string{"301", "302"} {
		peer := map[string]string{"301": "10.0.0.32", "302": "10.0.0.31"}[vm]
		if !slices.ContainsFunc(f.rules[vm], func(r pve.FirewallRule) bool { return r.Proto == "vrrp" && r.Source == peer }) ||
			!slices.ContainsFunc(f.rules[vm], func(r pve.FirewallRule) bool { return r.Dest == "203.0.113.10" && r.DPort == "80" }) {
			t.Errorf("%s rules: %+v", vm, f.rules[vm])
		}
	}
	saved, _ := h.cloud.FindByServiceIDAndExternalID(ctx, "svc", "lb-abc")
	got, _ := lbOfRow(saved)
	for _, a := range got.Appliances {
		if a.State != lbAppUp || a.HAProxyHash == "" || a.KeepalivedHash == "" || a.TenantIface != "ens18" || a.PublicIface != "ens19" {
			t.Errorf("appliance after apply: %+v", a)
		}
	}

	// Nothing changed: nothing is written.
	f.mu.Lock()
	writes := len(f.log)
	f.mu.Unlock()
	if status, err := h.pveLBApply(ctx, es, proj, "lb-abc"); err != nil || status != lbStatusActive || len(f.log) != writes {
		t.Fatalf("second apply: %s %v, writes %v", status, err, f.log[writes:])
	}

	// Both hold the address: DEGRADED, said so, nothing done about it.
	f.mu.Lock()
	f.vrrp["301"] = "MASTER 1 1"
	f.mu.Unlock()
	status, _ = h.pveLBApply(ctx, es, proj, "lb-abc")
	saved, _ = h.cloud.FindByServiceIDAndExternalID(ctx, "svc", "lb-abc")
	got, _ = lbOfRow(saved)
	if status != lbStatusDegraded || !strings.Contains(got.StatusMessage, "Both appliances hold the address") || len(f.log) != writes {
		t.Fatalf("split: %s %q, writes %v", status, got.StatusMessage, f.log[writes:])
	}

	// A replace is refused while one runs, and so is a delete, which would leave the VM being built to nobody; a
	// mark from a portal that died long ago holds nothing.
	got.Replacing = time.Now().UTC().Format(time.RFC3339)
	if err := h.lbSave(ctx, saved, got); err != nil {
		t.Fatal(err)
	}
	if herr := h.pveLBReplace(ctx, nil, es, proj, saved); herr == nil || herr.Status != http.StatusConflict || !strings.Contains(herr.Msg, "already being replaced") {
		t.Fatalf("second replace: %v", herr)
	}
	if err := h.pveDeleteLoadBalancer(ctx, es, proj, saved); err == nil || !strings.Contains(err.Error(), "being replaced") {
		t.Fatalf("delete during a replace: %v", err)
	}
	// A single appliance has nothing to replace.
	single := &cloud.CloudResource{ExternalID: "lb-def", ServiceID: "svc", ProjectID: "p1", Type: cloud.TypeLoadBalancer}
	if err := setLBRow(single, &loadBalancer{ID: "def", Name: "one", Status: lbStatusActive, ServerID: "401"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cloud.Insert(ctx, single); err != nil {
		t.Fatal(err)
	}
	if herr := h.pveLBReplace(ctx, nil, es, proj, single); herr == nil || herr.Status != http.StatusConflict || !strings.Contains(herr.Msg, "one VM") {
		t.Fatalf("replace on a single appliance: %v", herr)
	}
}

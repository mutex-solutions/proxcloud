package platformlife

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// probeEnv is an installed platform on pve1 with a published Ubuntu template (9200) there, one listener in the load
// balancer range (10.200.200.200:6443, as the ProxCloud host sees it), and a fake Proxmox whose guest agent answers
// the connect test with guest (address:port → result).
type probeEnv struct {
	t      *testing.T
	pve    *fakePVE
	x      *TenantProber
	in     *Installation
	saved  *TenantProbe
	guest  map[string]string
	listen []string
	// what the fake saw
	guestAddr       string // the address the guest reports on the test network
	exec            []string
	snat, vnetAlias string
	dhcpRange       bool
}

func newProbeEnv(t *testing.T) *probeEnv {
	t.Helper()
	e := &probeEnv{t: t, pve: newFakePVE(t), in: validInstallation(t), listen: []string{"10.200.200.200:6443"},
		guest: map[string]string{"10.200.200.200:6443": "open", "10.200.200.20:6443": "none"}, guestAddr: "10.100.255.242"}
	e.in.ID, e.in.Status.BOM = inID, &BOMRef{Release: "0.9.0", SHA256: "x"}
	for i := range e.in.Spec.Clusters[0].NodeGroups {
		e.in.Spec.Clusters[0].NodeGroups[i].Nodes = []string{"pve1"}
	}
	f := e.pve
	f.agent = true
	f.templates = []pve.ClusterResource{{Type: "qemu", Node: "pve1", VMID: 9200, Name: "pc-ubuntu-2604", Template: 1, Pool: pve.TemplatePool,
		Tags: "pc-img-ubuntu-2604;pc-public;pc-v-202611161812;proxcloud"}}
	f.probe = e
	ts := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(ts.Close)
	c, err := pve.New(pve.Config{APIURL: ts.URL, TokenID: pve.DefaultPlatformTokenID, Secret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	e.x = &TenantProber{Portal: &pve.Service{C: c, ServiceID: "svc-pve"}, Poll: time.Millisecond,
		Net: externalservice.PVENetwork{DefaultZone: "pcm0", CIDRPool: "10.100.0.0/16", Isolation: pve.IsolationShared,
			Zones: map[string]pve.ZoneConfig{"pcm0": {Key: "pcm0", Kind: "simple", NoDHCP: true}}},
		Load: func(context.Context, string) (*TenantProbe, error) { return e.saved, nil },
		Save: func(_ context.Context, p *TenantProbe) error { c := *p; e.saved = &c; return nil },
		Dial: func(_ context.Context, address string) error {
			if slices.Contains(e.listen, address) {
				return nil
			}
			return errors.New("timeout")
		}}
	return e
}

// run starts and runs a probe on node; the refusal when it was not started.
func (e *probeEnv) run(node string) (*TenantProbe, error) {
	p, err := e.x.Start(context.Background(), e.in, node, "ops")
	if err != nil {
		return nil, err
	}
	e.x.Run(context.Background(), p)
	return e.saved, nil
}

// nothingLeft: no probe VM and no probe network is left on the fake.
func (e *probeEnv) nothingLeft(name string) {
	e.t.Helper()
	for id, vm := range e.pve.vms {
		e.t.Errorf("%s: VM %d (%v) is left", name, id, vm.cfg["name"])
	}
	if len(e.pve.vnets) != 0 || e.pve.writeCount("PUT /cluster/sdn") < 2 {
		e.t.Errorf("%s: vnets %+v left, or the removal was not applied: %v", name, e.pve.vnets, e.pve.writes)
	}
}

// The open path: one clone on the chosen node, on a throwaway network of the default tenant zone with SNAT, the
// connect test through the guest agent, then VM and network destroyed. The platform's own API address is tested as
// an address tenants must not reach.
func TestTenantProbePasses(t *testing.T) {
	e := newProbeEnv(t)
	var net0, ipconfig string
	e.pve.onWrite = func(w string) {
		if vm := e.pve.vms[120]; vm != nil {
			if s, _ := vm.cfg["net0"].(string); s != "" {
				net0, _ = vm.cfg["net0"].(string)
				ipconfig, _ = vm.cfg["ipconfig0"].(string)
			}
		}
	}
	p, err := e.run("")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != ProbePassed || p.Node != "pve1" || p.Template != 9200 || p.VMID != 120 || p.FinishedAt == nil || len(p.Left) != 0 ||
		!strings.Contains(p.Message, "The path is open") || !strings.Contains(p.Message, "10.200.200.200:6443") {
		t.Fatalf("probe %+v", p)
	}
	want := []ProbeTarget{{"10.200.200.200", 6443, "open", "open"}, {"10.200.200.20", 6443, "closed", "none"}}
	if !slices.Equal(p.Targets, want) {
		t.Errorf("targets %+v, want %+v", p.Targets, want)
	}
	if e.pve.writeCount("POST /nodes/pve1/qemu/9200/clone") != 1 {
		t.Errorf("one clone: %v", e.pve.writes)
	}
	// The last /28 of the tenant pool, the VM on the address after the gateway.
	if p.Source != "10.100.255.242" || !strings.Contains(net0, ",bridge=v") || ipconfig != "ip=10.100.255.242/28,gw=10.100.255.241" {
		t.Errorf("source %s net0 %q ipconfig0 %q", p.Source, net0, ipconfig)
	}
	if e.snat != "1" || e.vnetAlias != "st:pc-probe-"+inID+":tenant-path-probe" {
		t.Errorf("the test network: snat %q alias %q", e.snat, e.vnetAlias)
	}
	e.nothingLeft("pass")
}

// In a zone with a DHCP server the test network gets a range and the VM asks for its address, as a tenant's does.
func TestTenantProbeInADHCPZone(t *testing.T) {
	e := newProbeEnv(t)
	e.pve.zones = []pve.SDNZone{{Zone: "pcm0", Type: "simple", DHCP: "dnsmasq"}}
	e.guestAddr = "10.100.255.245"
	var ipconfig string
	e.pve.onWrite = func(string) {
		if vm := e.pve.vms[120]; vm != nil && vm.cfg["ipconfig0"] != nil {
			ipconfig, _ = vm.cfg["ipconfig0"].(string)
		}
	}
	p, err := e.run("pve1")
	if err != nil || p.State != ProbePassed || p.Source != "10.100.255.245" || ipconfig != "ip=dhcp" || !e.dhcpRange {
		t.Fatalf("probe %+v, ipconfig0 %q, dhcp range %v: %v", p, ipconfig, e.dhcpRange, err)
	}
	e.nothingLeft("dhcp")
}

// The closed path (production, 2026-11-18): the ProxCloud host opens the address, the tenant network does not.
func TestTenantProbeFindsTheClosedPath(t *testing.T) {
	e := newProbeEnv(t)
	e.guest["10.200.200.200:6443"] = "none"
	p, err := e.run("pve1")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != ProbeFailed || !strings.Contains(p.Message, "The path is closed") || !strings.Contains(p.Message, "never join") || !strings.Contains(p.Message, tenantPathGuide) {
		t.Errorf("probe %+v", p)
	}
	e.nothingLeft("closed")

	// A reset is an answer: the path is open. An answer from the platform's own API address is an isolation failure.
	e = newProbeEnv(t)
	e.guest = map[string]string{"10.200.200.200:6443": "refused", "10.200.200.20:6443": "open"}
	if p, _ = e.run("pve1"); p.State != ProbeFailed || !strings.Contains(p.Message, "also reached the platform's own API address 10.200.200.20:6443") {
		t.Errorf("isolation: %+v", p)
	}
	e.nothingLeft("isolation")
}

// Every refusal comes before the first write to Proxmox, and says what is missing.
func TestTenantProbeRefusals(t *testing.T) {
	for code, mut := range map[string]func(*probeEnv){
		"not_installed":  func(e *probeEnv) { e.in.Status.BOM = nil },
		"no_tenant_zone": func(e *probeEnv) { e.x.Net.DefaultZone = "" },
		"invalid_node":   func(e *probeEnv) { e.in.Spec.Seed.Node = "pve9" }, // run("pve9") below
		"probe_running":  func(e *probeEnv) { e.saved = &TenantProbe{ID: inID, State: ProbeRunning, StartedAt: time.Now()} },
		"no_os_template": func(e *probeEnv) { e.pve.templates[0].Tags = "pc-tu-k8s;proxcloud" },
		// The zone as Proxmox has it, whatever the provider's config says: a VLAN zone has no node that translates.
		"vlan_zone": func(e *probeEnv) { e.pve.zones = []pve.SDNZone{{Zone: "pcm0", Type: "vlan"}} },
		"no_target": func(e *probeEnv) { e.listen = nil },
	} {
		e := newProbeEnv(t)
		mut(e)
		node := "pve1"
		if code == "invalid_node" {
			node = "pve9"
		}
		_, err := e.run(node)
		var refused *ProbeRefused
		if !errors.As(err, &refused) || refused.Code != code {
			t.Errorf("%s: %v", code, err)
		}
		if n := e.pve.writeCount(""); n != 0 {
			t.Errorf("%s: writes to Proxmox before the refusal: %v", code, e.pve.writes)
		}
	}
	// A probe recorded as running long ago died with its process: the next one starts, and removes what it left.
	e := newProbeEnv(t)
	e.saved = &TenantProbe{ID: inID, State: ProbeRunning, StartedAt: time.Now().Add(-time.Hour)}
	e.pve.vms[115] = &fakeVM{cfg: map[string]any{"name": ProbeVMName(inID)}}
	e.pve.templates = append(e.pve.templates, pve.ClusterResource{Type: "qemu", Node: "pve1", VMID: 115, Name: ProbeVMName(inID), Pool: pve.TemplatePool})
	e.pve.vnets = []pve.SDNVnet{{Vnet: "vold1", Zone: "pcm0", Alias: "st:pc-probe-" + inID + ":tenant-path-probe"}, {Vnet: "vten1", Zone: "pcm0", Alias: "st:p1:net"}}
	p, err := e.run("pve1")
	if err != nil || p.State != ProbePassed {
		t.Fatalf("after a dead probe: %+v %v", p, err)
	}
	if e.pve.vms[115] != nil || len(e.pve.vnets) != 1 || e.pve.vnets[0].Vnet != "vten1" {
		t.Errorf("the dead probe's VM or network is left, or a tenant's network went: %v %+v", e.pve.vms, e.pve.vnets)
	}
}

// A probe that cannot be done says that nothing is known, and still removes what it made.
func TestTenantProbeCleansUpAfterAFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(*probeEnv)
		want string
	}{
		"the clone fails": {func(e *probeEnv) {
			e.pve.failOnce["POST /nodes/pve1/qemu/9200/clone"] = `{"errors":{"storage":"no space left"}}`
		}, "clone of template 9200 on pve1"},
		"the config is refused": {func(e *probeEnv) {
			e.pve.failOnce["POST /nodes/pve1/qemu/120/config"] = `{"errors":{"net0":"permission denied"}}`
		}, "configuration of VM 120"},
		"the agent gives no result": {func(e *probeEnv) { e.guest = map[string]string{} }, "gave no result for 10.200.200.200:6443"},
		"the SDN apply is refused":  {func(e *probeEnv) { e.pve.failOnce["PUT /cluster/sdn"] = `{"errors":{"sdn":"reload failed"}}` }, "reload failed"},
		"the start is refused": {func(e *probeEnv) {
			e.pve.failOnce["POST /nodes/pve1/qemu/120/status/start"] = `{"errors":{"kvm":"out of memory"}}`
		}, "start of VM 120"},
		"the subnet list is refused": {func(e *probeEnv) { e.pve.failOnce["GET /cluster/sdn/vnets"] = `{"errors":{"sdn":"busy"}}` }, "nothing is known about the path"},
	} {
		e := newProbeEnv(t)
		tc.mut(e)
		p, err := e.run("pve1")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p.State != ProbeError || !strings.Contains(p.Message, tc.want) || !strings.Contains(p.Message, "nothing is known about the path") {
			t.Errorf("%s: %s %q", name, p.State, p.Message)
		}
		if len(e.pve.vms) != 0 || len(e.pve.vnets) != 0 || len(p.Left) != 0 {
			t.Errorf("%s: left VMs %v, vnets %+v, %v", name, e.pve.vms, e.pve.vnets, p.Left)
		}
	}
	// What the cleanup cannot remove is named, with the command.
	e := newProbeEnv(t)
	e.pve.keepVM = true
	p, _ := e.run("pve1")
	if len(p.Left) != 1 || !strings.Contains(p.Left[0], "qm destroy 120 --purge") || !strings.Contains(p.Message, "left something behind") {
		t.Errorf("left: %+v", p)
	}
}

func TestProbeCIDR(t *testing.T) {
	for _, tc := range []struct {
		pool string
		used []string
		want string
	}{
		{"10.100.0.0/16", nil, "10.100.255.240/28"},
		{"10.100.0.0/16", []string{"10.100.255.0/24", "10.100.0.0/24"}, "10.100.254.240/28"},
		{"10.100.0.0/28", nil, "10.100.0.0/28"},
		{"10.100.0.0/28", []string{"10.100.0.0/24"}, ""},
		{"10.100.0.0/29", nil, ""},
	} {
		got, err := probeCIDR(tc.pool, tc.used)
		if got != tc.want || (err == nil) != (tc.want != "") {
			t.Errorf("%s used %v: %q %v, want %q", tc.pool, tc.used, got, err, tc.want)
		}
	}
}

// probeRoutes are the fake Proxmox routes only the probe uses; false when path is not one.
func (f *fakePVE) probeRoutes(w http.ResponseWriter, r *http.Request, path string, data func(any)) bool {
	e := f.probe
	if e == nil {
		return false
	}
	switch {
	case path == "/cluster/resources":
		rows := slices.Clone(f.templates)
		for id, vm := range f.vms {
			if name, _ := vm.cfg["name"].(string); name == ProbeVMName(inID) && !slices.ContainsFunc(rows, func(r pve.ClusterResource) bool { return r.VMID == id }) {
				rows = append(rows, pve.ClusterResource{Type: "qemu", Node: "pve1", VMID: id, Name: name, Pool: pve.TemplatePool})
			}
		}
		rows = slices.DeleteFunc(rows, func(r pve.ClusterResource) bool { return r.Template != 1 && f.vms[r.VMID] == nil })
		data(rows)
	case path == "/nodes/pve1/qemu/9200/clone":
		id, _ := strconv.Atoi(r.Form.Get("newid"))
		f.vms[id] = &fakeVM{cfg: map[string]any{"name": r.Form.Get("name"), "scsi0": fmt.Sprintf("local-lvm:vm-%d-disk-0,size=6G", id)}}
		data(f.upid("qmclone", 9200))
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/subnets"):
		e.snat, e.dhcpRange = r.Form.Get("snat"), r.Form.Get("dhcp-range") != ""
		return false // the SDN fake stores it
	case r.Method == http.MethodPost && path == "/cluster/sdn/vnets":
		e.vnetAlias = r.Form.Get("alias")
		return false
	case r.Method == http.MethodDelete && f.keepVM && strings.HasPrefix(path, "/nodes/pve1/qemu/"):
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"errors":{"vmid":"VM is locked"}}`)
	case strings.HasSuffix(path, "/agent/exec"):
		e.exec = r.Form["command"]
		data(map[string]any{"pid": 7})
	case strings.HasSuffix(path, "/agent/exec-status"):
		out := ""
		if len(e.exec) > 0 && e.exec[0] == "ip" {
			out = "1: lo    inet 127.0.0.1/8 scope host lo\n2: eth0    inet " + e.guestAddr + "/28 brd 10.100.255.255 scope global eth0\n"
		} else if len(e.exec) > 4 {
			for _, at := range e.exec[4:] {
				if res, ok := e.guest[at]; ok {
					out += at + " " + res + "\n"
				}
			}
		}
		data(map[string]any{"exited": 1, "exitcode": 0, "out-data": out})
	default:
		return false
	}
	return true
}

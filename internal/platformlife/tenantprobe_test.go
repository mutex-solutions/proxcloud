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
	cloneStore      string // the storage the clone was asked onto
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
		// Template 9200 is one this portal imported; a tenant server's disk goes to the store "tank".
		Imported:  func(r pve.ClusterResource) bool { return r.Node == "pve1" && r.VMID == 9200 },
		DiskStore: func(string) string { return "tank" },
		Load:      func(context.Context, string) (*TenantProbe, error) { return e.saved, nil },
		Save:      func(_ context.Context, p *TenantProbe) error { c := *p; e.saved = &c; return nil },
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
	if p.State != ProbePassed || p.Node != "pve1" || p.Template != 9200 || p.VMID != 120 || p.FinishedAt == nil || len(p.Left) != 0 || len(p.Notes) != 0 ||
		!strings.Contains(p.Message, "The path is open") || !strings.Contains(p.Message, "10.200.200.200:6443") {
		t.Fatalf("probe %+v", p)
	}
	// The result names the image, its template and the node, and the clone went to the provider's disk store.
	if p.Image != "ubuntu-2604" || p.Storage != "tank" || e.cloneStore != "tank" || !strings.Contains(p.Message, "a clone of the image ubuntu-2604 (template 9200) on pve1") {
		t.Errorf("image %q, storage %q, cloned onto %q: %s", p.Image, p.Storage, e.cloneStore, p.Message)
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

// In a zone with a DHCP server the test network gets a range, as a tenant's does, but the VM does not ask for its
// address: it gets the fixed one over cloud-init, so the test does not depend on a DHCP answer (lab round of
// 2026-11-19: no address in a DHCP zone, no verdict). What Proxmox may keep of the NIC's DHCP mapping is listed.
func TestTenantProbeInADHCPZone(t *testing.T) {
	e := newProbeEnv(t)
	e.pve.zones = []pve.SDNZone{{Zone: "pcm0", Type: "simple", DHCP: "dnsmasq"}}
	var ipconfig string
	e.pve.onWrite = func(string) {
		if vm := e.pve.vms[120]; vm != nil && vm.cfg["ipconfig0"] != nil {
			ipconfig, _ = vm.cfg["ipconfig0"].(string)
		}
	}
	p, err := e.run("pve1")
	if err != nil || p.State != ProbePassed || p.Source != "10.100.255.242" || ipconfig != "ip=10.100.255.242/28,gw=10.100.255.241" || !e.dhcpRange {
		t.Fatalf("probe %+v, ipconfig0 %q, dhcp range %v: %v", p, ipconfig, e.dhcpRange, err)
	}
	if len(p.Notes) != 1 || !strings.Contains(p.Notes[0], "/etc/dnsmasq.d/pcm0/ethers") || !strings.Contains(p.Notes[0], "10.100.255.240/28") || !strings.Contains(p.Notes[0], "on pve1") || len(p.Left) != 0 {
		t.Errorf("the DHCP mapping Proxmox may keep: notes %q, left %q", p.Notes, p.Left)
	}
	e.nothingLeft("dhcp")
}

// The image is one of this portal's own, chosen as a tenant's server create resolves it; a published template that
// someone else put in the pool is never cloned (lab round of 2026-11-19, Q1: the first Ubuntu slug won, a template of
// another party on a shared cluster).
func TestTenantProbeImage(t *testing.T) {
	foreign := pve.ClusterResource{Type: "qemu", Node: "pve1", VMID: 9150, Name: "pc-ubuntu-2404", Template: 1, Pool: pve.TemplatePool,
		Tags: "pc-img-ubuntu-2404;pc-public;pc-v-202611061924;proxcloud"}
	e := newProbeEnv(t)
	e.pve.templates = append([]pve.ClusterResource{foreign}, e.pve.templates...)
	started, err := e.x.Start(context.Background(), e.in, "pve1", "ops")
	if err != nil || started.Image != "ubuntu-2604" || started.Template != 9200 || started.Node != "pve1" || !strings.Contains(started.Message, "image ubuntu-2604 (template 9200)") {
		t.Fatalf("the started probe does not name this portal's image: %+v %v", started, err)
	}
	e.saved = nil
	if p, err := e.run("pve1"); err != nil || p.State != ProbePassed || e.pve.writeCount("POST /nodes/pve1/qemu/9150/clone") != 0 || e.pve.writeCount("POST /nodes/pve1/qemu/9200/clone") != 1 {
		t.Errorf("probe %+v, writes %v", p, e.pve.writes)
	}
	// Only the other party's template is there: refused with its name, before any write.
	e = newProbeEnv(t)
	e.pve.templates = []pve.ClusterResource{foreign}
	var refused *ProbeRefused
	if _, err := e.run("pve1"); !errors.As(err, &refused) || refused.Code != "no_os_template" || !strings.Contains(refused.Message, "ubuntu-2404 (VM 9150)") ||
		!strings.Contains(refused.Message, "Template updates") || e.pve.writeCount("") != 0 {
		t.Errorf("only a template that is not this portal's: %v, writes %v", err, e.pve.writes)
	}
	// A template the provider's config pins by VM id is the operator's choice, and counts.
	e = newProbeEnv(t)
	e.pve.templates, e.x.Imported = []pve.ClusterResource{foreign}, nil
	e.x.Catalog = (&externalservice.ExternalService{Config: map[string]any{"templates": map[string]any{"ubuntu-2404": map[string]any{"vmid": float64(9150), "node": "pve1"}}}}).PVEImageCatalog
	if p, err := e.x.Start(context.Background(), e.in, "pve1", "ops"); err != nil || p.Template != 9150 || p.Image != "ubuntu-2404" {
		t.Errorf("a pinned template: %+v %v", p, err)
	}

	// The order among this portal's own: Ubuntu before Debian, then the newest build; a copy on another node is none.
	tpl := func(slug string, copies map[string]int) externalservice.PVETemplate {
		return externalservice.PVETemplate{Slug: slug, OSDistro: strings.Split(slug, "-")[0], Copies: copies}
	}
	rows := []pve.ClusterResource{{Node: "pve1", VMID: 1, Tags: "pc-v-202611010000"}, {Node: "pve1", VMID: 2, Tags: "pc-v-202611160000"}, {Node: "pve1", VMID: 3, Tags: "pc-v-202612010000"}}
	catalog := []externalservice.PVETemplate{tpl("debian-13", map[string]int{"pve1": 3}), tpl("ubuntu-2404", map[string]int{"pve1": 1}), tpl("ubuntu-2604", map[string]int{"pve1": 2, "pve2": 9}),
		tpl("rocky-9", map[string]int{"pve1": 4})}
	all := func(pve.ClusterResource) bool { return true }
	if slug, id, _ := probeImage(catalog, rows, "pve1", all); slug != "ubuntu-2604" || id != 2 {
		t.Errorf("newest Ubuntu: %s %d", slug, id)
	}
	if slug, id, _ := probeImage(catalog, rows, "pve1", func(r pve.ClusterResource) bool { return r.Node == "pve1" && r.VMID == 3 }); slug != "debian-13" || id != 3 {
		t.Errorf("Debian when no Ubuntu is this portal's: %s %d", slug, id)
	}
	if slug, id, others := probeImage(catalog, rows, "pve2", nil); slug != "" || id != 0 || !slices.Equal(others, []string{"ubuntu-2604 (VM 9)"}) {
		t.Errorf("no receipt at all: %s %d %v", slug, id, others)
	}
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
	e.pve.templates = append(e.pve.templates, pve.ClusterResource{Type: "qemu", Node: "pve1", VMID: 115, Name: ProbeVMName(inID), Pool: pve.TemplatePool, Tags: "proxcloud;pc-probe"})
	var order []string
	e.pve.onWrite = func(w string) {
		if w == "DELETE /nodes/pve1/qemu/115" || strings.HasSuffix(w, "/clone") {
			order = append(order, w)
		}
	}
	e.pve.vnets = []pve.SDNVnet{{Vnet: "vold1", Zone: "pcm0", Alias: "st:pc-probe-" + inID + ":tenant-path-probe"}, {Vnet: "vten1", Zone: "pcm0", Alias: "st:p1:net"}}
	p, err := e.run("pve1")
	if err != nil || p.State != ProbePassed {
		t.Fatalf("after a dead probe: %+v %v", p, err)
	}
	if e.pve.vms[115] != nil || len(e.pve.vnets) != 1 || e.pve.vnets[0].Vnet != "vten1" {
		t.Errorf("the dead probe's VM or network is left, or a tenant's network went: %v %+v", e.pve.vms, e.pve.vnets)
	}
	// Before this test makes anything, not only when it ends.
	if !slices.Equal(order, []string{"DELETE /nodes/pve1/qemu/115", "POST /nodes/pve1/qemu/9200/clone"}) {
		t.Errorf("the leftover goes before the clone: %v", order)
	}
}

// The cleanup destroys only what is the probe's: a VM with the probe's name in pool pc-templates that is no template
// and carries the tag pc-probe. A template, a VM of another pool and an untagged VM of that name stay; the untagged
// one is named, because it may be a clone whose run died before it was tagged.
func TestTenantProbeCleanupGuards(t *testing.T) {
	e := newProbeEnv(t)
	name := ProbeVMName(inID)
	for id, r := range map[int]pve.ClusterResource{
		201: {Template: 1, Pool: pve.TemplatePool, Tags: "proxcloud;pc-probe"},
		202: {Pool: "tp-someone", Tags: "proxcloud;pc-probe"},
		203: {Pool: pve.TemplatePool, Tags: "proxcloud"},
		204: {Pool: pve.TemplatePool, Tags: "proxcloud;pc-probe"},
	} {
		r.Type, r.Node, r.VMID, r.Name = "qemu", "pve1", id, name
		e.pve.vms[id] = &fakeVM{cfg: map[string]any{"name": "kept"}} // the row below is what the cleanup reads
		e.pve.templates = append(e.pve.templates, r)
	}
	left := e.x.cleanup(context.Background(), inID, 0)
	if len(left) != 1 || !strings.Contains(left[0], "VM 203") || !strings.Contains(left[0], "not the tag pc-probe") {
		t.Errorf("left %v", left)
	}
	if e.pve.vms[201] == nil || e.pve.vms[202] == nil || e.pve.vms[203] == nil || e.pve.vms[204] != nil {
		t.Errorf("only VM 204 is the probe's: %v, writes %v", e.pve.vms, e.pve.writes)
	}
	// The VM this run cloned is its own before the first config write tags it.
	if left := e.x.cleanup(context.Background(), inID, 203); len(left) != 0 || e.pve.vms[203] != nil {
		t.Errorf("own clone: left %v, VMs %v", left, e.pve.vms)
	}
}

// The test has a limit: a guest agent that never answers ends it as an error, and the VM and network still go.
func TestTenantProbeDeadline(t *testing.T) {
	e := newProbeEnv(t)
	e.pve.agent, e.x.Deadline = false, 50*time.Millisecond
	p, err := e.run("pve1")
	if err != nil || p.State != ProbeError || !strings.Contains(p.Message, "did not finish in 50ms") || !strings.Contains(p.Message, "guest agent") {
		t.Fatalf("probe %+v %v", p, err)
	}
	e.nothingLeft("deadline")
}

// A panic inside the test does not leave the goroutine (it would end the API process): it is the probe's error, and
// the cleanup still runs.
func TestTenantProbeSurvivesAPanic(t *testing.T) {
	e := newProbeEnv(t)
	save := e.x.Save
	e.x.Save = func(ctx context.Context, p *TenantProbe) error {
		if p.Step == "waiting for the test VM's guest agent and address" {
			panic("boom")
		}
		return save(ctx, p)
	}
	p, err := e.run("pve1")
	if err != nil || p.State != ProbeError || !strings.Contains(p.Message, "internal error: boom") {
		t.Fatalf("probe %+v %v", p, err)
	}
	e.nothingLeft("panic")
}

// A probe recorded as running for longer than a probe can run was cut off by an API restart: readers get an error
// that names what may be left, and the stored document is not touched.
func TestTenantProbeCutOffView(t *testing.T) {
	now := time.Now()
	live := &TenantProbe{ID: inID, State: ProbeRunning, StartedAt: now.Add(-probeDeadline), Step: "cloning the test VM"}
	if live.View(now) != live {
		t.Error("a probe inside its time is shown as it is")
	}
	dead := &TenantProbe{ID: inID, State: ProbeRunning, StartedAt: now.Add(-probeDeadline - probeCleanup), Step: "cloning the test VM"}
	v := dead.View(now)
	if v.State != ProbeError || v.Step != "" || !strings.Contains(v.Message, "restarted while it ran") || len(v.Left) != 1 ||
		!strings.Contains(v.Left[0], ProbeVMName(inID)) || !strings.Contains(v.Left[0], "st:pc-probe-"+inID+":tenant-path-probe") {
		t.Errorf("view %+v", v)
	}
	if dead.State != ProbeRunning {
		t.Error("the stored probe was changed")
	}
	done := &TenantProbe{ID: inID, State: ProbePassed, StartedAt: now.Add(-time.Hour)}
	if done.View(now) != done {
		t.Error("a finished probe is shown as it is")
	}
}

// A tenant cluster's API is the target when one answers: a managed database may drop the test network by its
// allow-list. When only a database answers and the VM cannot open it, the verdict says what else that can mean.
func TestTenantProbePrefersClusterAPIs(t *testing.T) {
	e := newProbeEnv(t)
	e.listen = append(e.listen, "10.200.200.201:5432")
	p, err := e.run("pve1")
	if err != nil || p.State != ProbePassed || len(p.Targets) != 2 || p.Targets[0].Port != 6443 || p.Targets[1].Expect != "closed" {
		t.Fatalf("with both: %+v %v", p, err)
	}
	e = newProbeEnv(t)
	e.listen, e.guest = []string{"10.200.200.201:5432"}, map[string]string{"10.200.200.201:5432": "none", "10.200.200.20:6443": "none"}
	p, err = e.run("pve1")
	if err != nil || p.State != ProbeFailed || !strings.Contains(p.Message, "allowedCidrs") || !strings.Contains(p.Message, "does not prove the path closed") {
		t.Fatalf("a database only: %+v %v", p, err)
	}
	// A cluster API that does not open is the closed path, without that caveat.
	e = newProbeEnv(t)
	e.guest["10.200.200.200:6443"] = "none"
	if p, _ = e.run("pve1"); strings.Contains(p.Message, "allowedCidrs") {
		t.Errorf("a closed cluster API: %s", p.Message)
	}
}

// What Proxmox or the guest says goes into the stored probe clipped, as a stage's error does.
func TestTenantProbeClipsErrors(t *testing.T) {
	e := newProbeEnv(t)
	e.pve.failOnce["POST /nodes/pve1/qemu/9200/clone"] = `{"errors":{"storage":"` + strings.Repeat("x", 5000) + `"}}`
	p, err := e.run("pve1")
	if err != nil || p.State != ProbeError || len(p.Message) > 700 {
		t.Fatalf("message of %d bytes: %v", len(p.Message), err)
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
		}, "clone of the image ubuntu-2604 (template 9200) on pve1"},
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
				tags, _ := vm.cfg["tags"].(string)
				rows = append(rows, pve.ClusterResource{Type: "qemu", Node: "pve1", VMID: id, Name: name, Pool: pve.TemplatePool, Tags: tags})
			}
		}
		rows = slices.DeleteFunc(rows, func(r pve.ClusterResource) bool { return r.Template != 1 && f.vms[r.VMID] == nil })
		data(rows)
	case path == "/nodes/pve1/qemu/9200/clone":
		e.cloneStore = r.Form.Get("storage")
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

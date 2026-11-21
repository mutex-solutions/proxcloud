package project

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// lbApplyFixture is a handler in front of the pair fake, and a pair's row as its create leaves it.
func lbApplyFixture(t *testing.T) (*Handler, *pairWire, *externalservice.ExternalService, *loadBalancer) {
	t.Helper()
	f := newPairWire(t, "")
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{pveFor: func(*externalservice.ExternalService) (*pve.Service, error) { return ps, nil }}
	es := &externalservice.ExternalService{ID: "svc", Config: map[string]any{"network": map[string]any{"publicNetworks": []any{
		map[string]any{"id": "vmbr1", "cidr": "203.0.113.0/24", "gateway": "203.0.113.1", "ranges": []any{"203.0.113.10-203.0.113.20"}}}}}}
	lb := &loadBalancer{ID: "abc", Name: "web", Status: lbStatusBuild, NetworkID: "v1", PublicNetworkID: "vmbr1", HA: true,
		ServerID: "301", PrivateIP: "10.0.0.31", PrivatePrefix: 24, PublicIP: "203.0.113.10", VIPs: []string{"203.0.113.10"},
		Appliances: []lbAppliance{{ServerID: "301", PrivateIP: "10.0.0.31"}, {ServerID: "302", PrivateIP: "10.0.0.32", HoldsVIP: true}},
		LBSpec:     pve.LBSpec{Listeners: []pve.LBListener{{Port: 80, Pool: "web"}}, Pools: []pve.LBPool{{Name: "web", Protocol: "http", Algorithm: "roundrobin"}}}}
	return h, f, es, lb
}

// A pair's hash is recorded only for a config the appliance was seen to serve, and a recorded one is checked on
// every pass (drill N2-2: the hash was stored while HAProxy ran the package's config, the pair refused connections
// for 458 s and read ACTIVE; N2-5: a stopped keepalived or HAProxy was never started again).
func TestLBPairRecordsOnlyWhatIsServed(t *testing.T) {
	ctx := context.Background()
	h, f, es, lb := lbApplyFixture(t)
	proj, cr := &Project{ID: "p1"}, &cloud.CloudResource{}
	apply := func() {
		t.Helper()
		if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
			t.Fatal(err)
		}
	}
	app := func(id string) *lbAppliance {
		for i := range lb.Appliances {
			if lb.Appliances[i].ServerID == id {
				return &lb.Appliances[i]
			}
		}
		t.Fatalf("no appliance %s", id)
		return nil
	}

	// The binaries are there and dpkg still configures the packages: nothing is written, nothing recorded.
	for _, vm := range []string{"301", "302"} {
		f.set(vm, func(g *lbGuest) { *g = lbGuest{packages: "unpacked"} })
	}
	apply()
	if lb.Status != lbStatusBuild || app("301").HAProxyHash != "" || app("301").KeepalivedHash != "" || len(f.files) != 0 || f.count("301 install") == 0 {
		t.Fatalf("while dpkg configures: %s, %+v, files %d, log %v", lb.Status, lb.Appliances, len(f.files), f.log)
	}
	// dpkg is done on both; on 302 the apply cannot prove that HAProxy runs the file: not applied, not recorded.
	f.set("301", func(g *lbGuest) { g.packages = "installed" })
	f.set("302", func(g *lbGuest) {
		g.packages, g.role, g.holds, g.applyFails = "installed", "MASTER", true, "HAProxy does not listen on 203.0.113.10:80"
	})
	apply()
	if app("301").HAProxyHash == "" || app("302").HAProxyHash != "" || app("302").KeepalivedHash == "" ||
		lb.Status != lbStatusError || !strings.Contains(lb.StatusMessage, "does not listen on 203.0.113.10:80") || lb.AppliedHash != "" {
		t.Fatalf("an apply whose proof failed: %s %q applied %q, %+v", lb.Status, lb.StatusMessage, lb.AppliedHash, lb.Appliances)
	}
	// The next pass pushes it again, and records it when it is served.
	f.set("302", func(g *lbGuest) { g.applyFails = "" })
	apply()
	if lb.Status != lbStatusActive || app("302").HAProxyHash == "" || f.count("302 haproxy reload") != 2 || f.count("301 haproxy reload") != 1 {
		t.Fatalf("the pass after: %s %q, %+v, log %v", lb.Status, lb.StatusMessage, lb.Appliances, f.log)
	}
	// Nothing changed: nothing written, one state read per appliance.
	writes, reads := len(f.log), f.reads
	apply()
	if lb.Status != lbStatusActive || len(f.log) != writes || f.reads != reads+2 {
		t.Fatalf("a quiet pass: %s, wrote %v, %d state reads", lb.Status, f.log[writes:], f.reads-reads)
	}

	// The drill's appliance: the hash is recorded, HAProxy's master has a worker, and it runs the package's config.
	// While the package manager runs it cannot be pushed, and the row says what is wrong, not ACTIVE.
	f.set("301", func(g *lbGuest) { g.loaded, g.aptRunning = "", true })
	apply()
	if lb.Status != lbStatusDegraded || !strings.Contains(lb.StatusMessage, "appliance 301: HAProxy does not serve its configuration") ||
		strings.Contains(lb.StatusMessage, "missing") || app("301").State != lbAppDown {
		t.Fatalf("HAProxy on the package's config: %s %q", lb.Status, lb.StatusMessage)
	}
	// Then the same pass that sees it loads the config again.
	f.set("301", func(g *lbGuest) { g.aptRunning = false })
	apply()
	if lb.Status != lbStatusActive || f.count("301 haproxy reload") != 2 || f.count("302 haproxy reload") != 2 || !f.guests["301"].serving() {
		t.Fatalf("after the push: %s %q, log %v", lb.Status, lb.StatusMessage, f.log)
	}

	// HAProxy stopped on a running appliance: started with the config in the pass that sees it.
	f.set("301", func(g *lbGuest) { g.haproxy = false })
	apply()
	if lb.Status != lbStatusActive || f.count("301 haproxy reload") != 3 || !f.guests["301"].haproxy {
		t.Fatalf("HAProxy stopped: %s %q", lb.Status, lb.StatusMessage)
	}
	// keepalived stopped on a running appliance: the same.
	f.set("301", func(g *lbGuest) { g.keepalived = false })
	before := f.count("301 keepalived reload")
	apply()
	if lb.Status != lbStatusActive || f.count("301 keepalived reload") != before+1 || !f.guests["301"].keepalived {
		t.Fatalf("keepalived stopped: %s %q", lb.Status, lb.StatusMessage)
	}
	// keepalived stopped and it cannot be pushed now: the cause and what happens next, and no "missing" appliance.
	f.set("301", func(g *lbGuest) { g.keepalived, g.aptRunning = false, true })
	apply()
	if lb.Status != lbStatusDegraded || !strings.Contains(lb.StatusMessage, "appliance 301: keepalived is stopped") ||
		!strings.Contains(lb.StatusMessage, "The platform puts the other one right") || strings.Contains(lb.StatusMessage, "missing") {
		t.Fatalf("keepalived stopped, not startable yet: %s %q", lb.Status, lb.StatusMessage)
	}
	f.set("301", func(g *lbGuest) { g.aptRunning = false })
	apply()

	// A stopped VM has no agent: it is started, said so, and nothing is asked of its guest.
	f.set("301", func(g *lbGuest) { g.stopped = true })
	reads = f.reads
	apply()
	if lb.Status != lbStatusDegraded || !strings.Contains(lb.StatusMessage, "appliance 301: its VM is stopped; starting it") ||
		strings.Contains(lb.StatusMessage, "missing") || f.count("301 start") != 1 || f.reads != reads+1 {
		t.Fatalf("a stopped VM: %s %q, %d state reads, log %v", lb.Status, lb.StatusMessage, f.reads-reads, f.log)
	}
	apply()
	if lb.Status != lbStatusActive {
		t.Fatalf("after its start: %s %q", lb.Status, lb.StatusMessage)
	}

	// The health read of the drill's appliance: the stored listener is named as not loaded.
	f.set("302", func(g *lbGuest) { g.loaded = "" })
	out, herr := h.lbPairHealth(ctx, es, proj, lb)
	if herr != nil {
		t.Fatal(herr)
	}
	if nl, _ := out["notLoaded"].([]string); len(nl) != 1 || nl[0] != "80" || !strings.Contains(out["message"].(string), "listener 80 is not loaded") {
		t.Fatalf("health: %+v", out)
	}
	if apps := out["appliances"].([]lbAppliance); apps[1].State != lbAppDown || apps[1].Reason == "" {
		t.Fatalf("health names the appliance up: %+v", apps[1])
	}
}

// A single appliance: the same rule, and a balancer that is in line is asked and not pushed (its hash and its config
// are what they were before this check existed).
func TestLBSingleRecordsOnlyWhatIsServed(t *testing.T) {
	ctx := context.Background()
	h, f, es, _ := lbApplyFixture(t)
	f.mu.Lock()
	delete(f.guests, "302")
	*f.guests["301"] = lbGuest{packages: "unpacked"}
	f.mu.Unlock()
	proj, cr := &Project{ID: "p1"}, &cloud.CloudResource{}
	lb := &loadBalancer{ID: "abc", Name: "web", Status: lbStatusBuild, NetworkID: "v1", PublicNetworkID: "vmbr1", ServerID: "301",
		PrivateIP: "10.0.0.31", PublicIP: "203.0.113.10",
		LBSpec: pve.LBSpec{Listeners: []pve.LBListener{{Port: 80, Pool: "web"}}, Pools: []pve.LBPool{{Name: "web", Protocol: "http", Algorithm: "roundrobin"}}}}
	apply := func() {
		t.Helper()
		if err := h.lbApplyLocked(ctx, es, proj, cr, lb); err != nil {
			t.Fatal(err)
		}
	}
	apply() // dpkg still configures HAProxy
	if lb.Status != lbStatusBuild || lb.AppliedHash != "" || len(f.files) != 0 {
		t.Fatalf("while dpkg configures: %s %q, files %d", lb.Status, lb.AppliedHash, len(f.files))
	}
	f.set("301", func(g *lbGuest) { g.packages, g.applyFails = "installed", "HAProxy does not listen on 0.0.0.0:80" })
	apply()
	if lb.Status != lbStatusError || lb.AppliedHash != "" || !strings.Contains(lb.StatusMessage, "does not listen") {
		t.Fatalf("an apply whose proof failed: %s %q %q", lb.Status, lb.StatusMessage, lb.AppliedHash)
	}
	f.set("301", func(g *lbGuest) { g.applyFails = "" })
	apply()
	hash := lb.AppliedHash
	if lb.Status != lbStatusActive || hash == "" || hash != lbHash(pve.RenderHAProxy(lb.LBSpec)) {
		t.Fatalf("applied: %s %q", lb.Status, hash)
	}
	// In line: asked once, nothing written.
	writes, reads := len(f.log), f.reads
	apply()
	if lb.Status != lbStatusActive || len(f.log) != writes || f.reads != reads+1 || lb.AppliedHash != hash {
		t.Fatalf("a quiet pass: %s, wrote %v, %d reads", lb.Status, f.log[writes:], f.reads-reads)
	}
	// The agent does not answer: nothing new is known, nothing changes.
	f.set("301", func(g *lbGuest) { g.agentDown = true })
	apply()
	if lb.Status != lbStatusActive || len(f.log) != writes {
		t.Fatalf("no answer: %s %q, wrote %v", lb.Status, lb.StatusMessage, f.log[writes:])
	}
	// HAProxy stopped, and the config cannot be loaded now: DOWN with the cause, not ACTIVE.
	f.set("301", func(g *lbGuest) { g.agentDown, g.haproxy, g.aptRunning = false, false, true })
	apply()
	if lb.Status != lbStatusDown || !strings.Contains(lb.StatusMessage, "HAProxy is not running") || lb.AppliedHash != hash {
		t.Fatalf("HAProxy stopped: %s %q", lb.Status, lb.StatusMessage)
	}
	// Then it is loaded again: started, ACTIVE, the same hash.
	f.set("301", func(g *lbGuest) { g.aptRunning = false })
	apply()
	if lb.Status != lbStatusActive || lb.AppliedHash != hash || !f.guests["301"].serving() || f.count("301 haproxy reload") != 3 {
		t.Fatalf("started again: %s %q, log %v", lb.Status, lb.StatusMessage, f.log)
	}
	// HAProxy on the package's config with the hash recorded (the drill's state): loaded again.
	f.set("301", func(g *lbGuest) { g.loaded = "" })
	apply()
	if lb.Status != lbStatusActive || !f.guests["301"].serving() || f.count("301 haproxy reload") != 4 {
		t.Fatalf("the package's config: %s %q", lb.Status, lb.StatusMessage)
	}
}

// During a replace the row lists one appliance for a while: it says so, and never ACTIVE (drill N2-7).
func TestLBReplaceRestatesTheRow(t *testing.T) {
	lb := &loadBalancer{ID: "abc", HA: true, Status: lbStatusActive, ServerID: "301", Appliances: []lbAppliance{
		{ServerID: "301", PrivateIP: "10.0.0.31", State: lbAppUp, Role: "BACKUP", HAProxyHash: "h"},
		{ServerID: "302", PrivateIP: "10.0.0.32", State: lbAppUp, Role: "MASTER", HoldsVIP: true, HAProxyHash: "h"}}}
	lbTakeOut(lb, "301")
	if lb.Status != lbStatusDegraded || len(lb.Appliances) != 1 || lb.ServerID != "302" ||
		lb.StatusMessage != "One appliance carries the traffic while the other is being replaced." {
		t.Fatalf("after the old appliance left: %s %q %+v", lb.Status, lb.StatusMessage, lb.Appliances)
	}
	lbPutIn(lb, lbAppliance{ServerID: "303", PrivateIP: "10.0.0.33", State: lbAppBuilding})
	if lb.Status != lbStatusDegraded || len(lb.Appliances) != 2 || !strings.Contains(lb.StatusMessage, "being replaced (appliance 303: it is booting)") {
		t.Fatalf("with the new appliance booting: %s %q", lb.Status, lb.StatusMessage)
	}
}

// The row's own hash is what a cluster's Services are called ready by, so it follows an appliance that serves the
// config, not the status alone: two appliances that both hold the address read DEGRADED whatever they run.
func TestLBPairRowHashNeedsAnApplianceThatServes(t *testing.T) {
	ctx := context.Background()
	h, f, es, lb := lbApplyFixture(t)
	proj, cr := &Project{ID: "p1"}, &cloud.CloudResource{}
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
		t.Fatal(err)
	}
	served := lb.AppliedHash
	if lb.Status != lbStatusActive || served == "" {
		t.Fatalf("first apply: %s %q", lb.Status, served)
	}
	// A change neither appliance can take yet, while they cannot hear each other and both hold the address.
	lb.Listeners = append(lb.Listeners, pve.LBListener{Port: 81, Pool: "web"})
	for _, vm := range []string{"301", "302"} {
		f.set(vm, func(g *lbGuest) { g.role, g.holds, g.aptRunning = "MASTER", true, true })
	}
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
		t.Fatal(err)
	}
	if lb.Status != lbStatusDegraded || lb.AppliedHash != served {
		t.Fatalf("no appliance serves the new config: %s %q, row hash moved: %v", lb.Status, lb.StatusMessage, lb.AppliedHash != served)
	}
	for _, vm := range []string{"301", "302"} {
		f.set(vm, func(g *lbGuest) { g.aptRunning = false })
	}
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
		t.Fatal(err)
	}
	if lb.AppliedHash == served || lb.AppliedHash != lbHash(lbRender(lb)) {
		t.Fatalf("once it is served: %s %q", lb.Status, lb.AppliedHash)
	}
}

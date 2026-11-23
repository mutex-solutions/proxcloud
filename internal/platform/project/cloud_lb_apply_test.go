package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/lbgw"
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

// The services sync runs the load balancer pass whether or not it could prove a project's inventory (drill N2-1: a
// stopped appliance made the inventory unproved, and the skipped pass was the one that starts it). The VM list is
// filtered by what the token may see, so the pass proves the one absence it acts on itself: an appliance missing from
// the list is gone only when its VMID is free. One that is merely out of sight is not called gone: the row does not
// tell the tenant to delete or replace a balancer that may be serving, and a cluster's pair does not count as
// settled, which is what gives a removed Service's address back to the pool.
func TestLBApplianceOutOfSightIsNotGone(t *testing.T) {
	ctx := context.Background()
	h, f, es, lb := lbApplyFixture(t)
	proj, cr := &Project{ID: "p1"}, &cloud.CloudResource{}
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil || lb.Status != lbStatusActive || !lb.settled {
		t.Fatalf("a whole pair: %s %q settled %v, %v", lb.Status, lb.StatusMessage, lb.settled, err)
	}
	writes := len(f.log)
	f.mu.Lock()
	f.unseen = true
	f.mu.Unlock()
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
		t.Fatal(err)
	}
	for _, a := range lb.Appliances {
		if a.State != lbAppDown || a.Reason != "the platform cannot see its VM" {
			t.Fatalf("appliance %s out of sight: %s %q", a.ServerID, a.State, a.Reason)
		}
	}
	if lb.Status != lbStatusDown || lb.settled || strings.Contains(lb.StatusMessage, "gone") || strings.Contains(lb.StatusMessage, "Replace") || len(f.log) != writes {
		t.Fatalf("a pair out of sight: %s %q settled %v, wrote %v", lb.Status, lb.StatusMessage, lb.settled, f.log[writes:])
	}
	// The VMs really are gone: their numbers are free, and the row says so, as before.
	f.mu.Lock()
	f.unseen, f.gone = false, true
	f.mu.Unlock()
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
		t.Fatal(err)
	}
	if lb.Status != lbStatusError || !strings.Contains(lb.StatusMessage, "Both appliances are gone") {
		t.Fatalf("a pair that is gone: %s %q", lb.Status, lb.StatusMessage)
	}

	// A single appliance: the same proof.
	h, f, es, _ = lbApplyFixture(t)
	f.mu.Lock()
	delete(f.guests, "302")
	f.unseen = true
	f.mu.Unlock()
	single := &loadBalancer{ID: "abc", Name: "web", Status: lbStatusActive, NetworkID: "v1", PublicNetworkID: "vmbr1", ServerID: "301",
		PrivateIP: "10.0.0.31", PublicIP: "203.0.113.10", AppliedHash: "x"}
	if err := h.lbApplyLocked(ctx, es, proj, cr, single); err != nil {
		t.Fatal(err)
	}
	if single.Status != lbStatusDown || strings.Contains(single.StatusMessage, "Delete") || single.AppliedHash != "x" || len(f.log) != 0 {
		t.Fatalf("a single appliance out of sight: %s %q, wrote %v", single.Status, single.StatusMessage, f.log)
	}
	f.mu.Lock()
	f.unseen, f.gone = false, true
	f.mu.Unlock()
	if err := h.lbApplyLocked(ctx, es, proj, cr, single); err != nil {
		t.Fatal(err)
	}
	if single.Status != lbStatusError || !strings.Contains(single.StatusMessage, "is gone. Delete this load balancer") {
		t.Fatalf("a single appliance that is gone: %s %q", single.Status, single.StatusMessage)
	}
}

// Drill N3-8 of v0.11.0: a reload started every member UP, so a member that was DOWN (a node without an endpoint of a
// policy Local Service, a dead node) took its share of the requests again after every change of any Service of the
// cluster, until its checks had failed: 2.2 to 2.4 s, 11 or 12 requests each time. Now the apply saves the members'
// state and the config reads it back, under names that do not move when another Service's pools come and go. The
// fake's HAProxy keeps a state per member and loses it at a reload exactly as the real one does (lbGuest).
func TestLBReloadKeepsADownMemberDown(t *testing.T) {
	ctx := context.Background()
	h, f, es, lb := lbApplyFixture(t)
	proj, cr := &Project{ID: "p1"}, &cloud.CloudResource{}
	nodes := []string{"10.0.0.250", "10.0.0.249"}
	web := lbgw.Service{UID: "u-web", Ports: []lbgw.Port{{Port: 80, NodePort: 30080}}}
	local := lbgw.Service{UID: "u-local", Ports: []lbgw.Port{{Port: 80, NodePort: 31080}}, ExternalTrafficPolicy: "Local", HealthCheckNodePort: 32000}
	lb.VIPs = []string{"203.0.113.10", "203.0.113.11"}
	spec := func() pve.LBSpec {
		return lbClusterServiceSpec(lbClusterServiceSpec(pve.LBSpec{}, web, "203.0.113.10", nodes), local, "203.0.113.11", nodes)
	}
	lb.LBSpec = spec()
	apply := func() {
		t.Helper()
		if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil || lb.Status != lbStatusActive {
			t.Fatalf("apply: %s %q %v", lb.Status, lb.StatusMessage, err)
		}
	}
	// What each appliance's HAProxy says of the Local Service's two members right now, with no check run since.
	members := func(when string, want ...string) {
		t.Helper()
		ps, _ := h.pveFor(es)
		for i, vm := range []int{301, 302} {
			health, notLoaded, err := ps.LBHealth(ctx, fmt.Sprintf("n%d", i+1), vm, lb.LBSpec)
			if err != nil || len(notLoaded) != 0 {
				t.Fatalf("%s: health of %d: %q %v", when, vm, notLoaded, err)
			}
			var got []string
			for _, m := range health {
				if m.Port == 31080 {
					got = append(got, m.Address+" "+m.Status)
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("%s: appliance %d says %v, want %v", when, vm, got, want)
			}
		}
	}
	// The second node has no endpoint of the Local Service: its check on the health port fails.
	for _, vm := range []string{"301", "302"} {
		f.set(vm, func(g *lbGuest) { g.dead = []string{"10.0.0.249:31080"} })
	}
	apply()
	members("a first load: nothing to carry over, every member starts UP", "10.0.0.250 UP", "10.0.0.249 UP")
	for _, vm := range []string{"301", "302"} {
		f.set(vm, func(g *lbGuest) { g.check() })
	}
	members("after its checks", "10.0.0.250 UP", "10.0.0.249 DOWN")

	// The other Service gets a port, the drill's change: its new pool comes before the Local Service's.
	web.Ports = append(web.Ports, lbgw.Port{Port: 8080, NodePort: 30081})
	lb.LBSpec = spec()
	reloads := f.count("302 haproxy reload")
	apply()
	if f.count("302 haproxy reload") != reloads+1 {
		t.Fatalf("the change did not reload the appliance that holds the address: %v", f.log)
	}
	members("right after the reload", "10.0.0.250 UP", "10.0.0.249 DOWN")
	// And loses it, the same way round.
	web.Ports = web.Ports[:1]
	lb.LBSpec = spec()
	apply()
	members("right after the second reload", "10.0.0.250 UP", "10.0.0.249 DOWN")

	// The fake does tell: a HAProxy that was stopped has no state to save, and starts every member UP.
	f.set("301", func(g *lbGuest) { g.haproxy = false })
	apply()
	if got := f.guests["301"].members; len(got) != 4 || slices.Contains(slices.Collect(maps.Values(got)), "DOWN") {
		t.Fatalf("a HAProxy started from nothing: %v", got)
	}
}

// A config that the standby's HAProxy does not take never reaches the appliance that holds the address: the release
// that changes what every balancer's config says pushes every balancer once (v0.11.1), the standby first.
func TestLBRefusedConfigStopsAtTheStandby(t *testing.T) {
	ctx := context.Background()
	h, f, es, lb := lbApplyFixture(t)
	proj, cr := &Project{ID: "p1"}, &cloud.CloudResource{}
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil || lb.Status != lbStatusActive {
		t.Fatalf("first apply: %s %v", lb.Status, err)
	}
	served, running := lb.AppliedHash, f.guests["302"].loaded
	lb.Listeners = append(lb.Listeners, pve.LBListener{Port: 81, Pool: "web"})
	f.set("301", func(g *lbGuest) { g.applyFails = "HAProxy does not listen on 203.0.113.10:81" })
	for range 2 { // and not at the pass after either
		if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
			t.Fatal(err)
		}
		if lb.Status != lbStatusError || !strings.Contains(lb.StatusMessage, "does not listen on 203.0.113.10:81") || lb.AppliedHash != served ||
			f.count("302 haproxy reload") != 1 || f.guests["302"].loaded != running || !f.guests["302"].serving() {
			t.Fatalf("the standby refused the config: %s %q, holder reloads %d, log %v", lb.Status, lb.StatusMessage, f.count("302 haproxy reload"), f.log)
		}
	}
	f.set("301", func(g *lbGuest) { g.applyFails = "" })
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil || lb.Status != lbStatusActive || lb.AppliedHash == served || f.count("302 haproxy reload") != 2 {
		t.Fatalf("once the standby takes it: %s %q %v, log %v", lb.Status, lb.StatusMessage, err, f.log)
	}
}

// Drill N3-6: the master of a pair was paused, the standby took the address, and the row read "Both appliances hold
// the address: they cannot hear each other", from what the paused one held before. The row names the pause.
func TestLBPairWithAPausedMaster(t *testing.T) {
	ctx := context.Background()
	h, f, es, lb := lbApplyFixture(t)
	proj, cr := &Project{ID: "p1"}, &cloud.CloudResource{}
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil || lb.Status != lbStatusActive || !lb.Appliances[1].HoldsVIP {
		t.Fatalf("a whole pair: %s %v %+v", lb.Status, err, lb.Appliances)
	}
	f.set("302", func(g *lbGuest) { g.paused = true })
	f.set("301", func(g *lbGuest) { g.role, g.holds = "MASTER", true })
	writes := len(f.log)
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
		t.Fatal(err)
	}
	if lb.Status != lbStatusDegraded || !strings.Contains(lb.StatusMessage, "One appliance carries the traffic, with no standby (appliance 302: its VM is paused)") ||
		!strings.Contains(lb.StatusMessage, "resumed") || strings.Contains(lb.StatusMessage, "Both appliances hold") ||
		lb.Appliances[1].HoldsVIP || !lb.Appliances[0].HoldsVIP || len(f.log) != writes {
		t.Fatalf("a paused master: %s %q, %+v, wrote %v", lb.Status, lb.StatusMessage, lb.Appliances, f.log[writes:])
	}
	// Resumed: it comes back as the standby, and the pair is whole.
	f.set("302", func(g *lbGuest) { g.paused, g.role, g.holds = false, "BACKUP", false })
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil || lb.Status != lbStatusActive {
		t.Fatalf("after the resume: %s %q %v", lb.Status, lb.StatusMessage, err)
	}
}

// Drill N3-2: the health read of an appliance whose HAProxy is stopped says that, not that the VM did not answer.
func TestLBHealthOfAStoppedHAProxy(t *testing.T) {
	ctx := context.Background()
	h, f, es, lb := lbApplyFixture(t)
	if err := h.lbApplyPair(ctx, es, &Project{ID: "p1"}, &cloud.CloudResource{}, lb); err != nil {
		t.Fatal(err)
	}
	ps, _ := h.pveFor(es)
	f.set("301", func(g *lbGuest) { g.haproxy = false })
	if _, _, err := ps.LBHealth(ctx, "n1", 301, lb.LBSpec); !errors.Is(err, pve.ErrLBNoHAProxy) {
		t.Fatalf("HAProxy stopped: %v", err)
	}
	f.set("301", func(g *lbGuest) { g.agentDown = true })
	if _, _, err := ps.LBHealth(ctx, "n1", 301, lb.LBSpec); err == nil || errors.Is(err, pve.ErrLBNoHAProxy) {
		t.Fatalf("the VM does not answer: %v", err)
	}
}

// Drill Q1-4 of v0.11.1-rc.1: "an appliance's keepalived config is not the one its row records" was logged at every
// planned change of a pair's address list, since the file on the appliance was compared with the config the pass was
// about to push. It is compared with the config the row records: the line means a file changed behind the product,
// and only that. Both are shown, for a row that records the config's own hash and for one of before that field.
func TestLBKeepalivedWarnMeansAHandEdit(t *testing.T) {
	ctx := context.Background()
	h, f, es, lb := lbApplyFixture(t)
	proj, cr := &Project{ID: "p1"}, &cloud.CloudResource{}
	var logged bytes.Buffer
	before := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(before)
	const line = "keepalived config is not the one its row records"
	// pass runs an apply and answers how often the line was logged and how often keepalived was reloaded in it.
	pass := func() (warned, reloads int) {
		t.Helper()
		logged.Reset()
		was := f.count("301 keepalived reload") + f.count("302 keepalived reload")
		if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
			t.Fatal(err)
		}
		if lb.Status != lbStatusActive {
			t.Fatalf("after the pass: %s %q", lb.Status, lb.StatusMessage)
		}
		return strings.Count(logged.String(), line), f.count("301 keepalived reload") + f.count("302 keepalived reload") - was
	}
	const conf = "/etc/keepalived/keepalived.conf"
	if warned, reloads := pass(); warned != 0 || reloads != 2 {
		t.Fatalf("the first apply: %d warnings, %d keepalived reloads", warned, reloads)
	}
	for _, a := range lb.Appliances {
		if a.KeepalivedConfHash != lbHash(f.guests[a.ServerID].disk[conf]) || a.KeepalivedConfHash == a.KeepalivedHash {
			t.Fatalf("appliance %s records %q for its config file", a.ServerID, a.KeepalivedConfHash)
		}
	}
	for _, legacy := range []bool{false, true} {
		forget := func() {
			if legacy { // a row written before the field existed
				for i := range lb.Appliances {
					lb.Appliances[i].KeepalivedConfHash = ""
				}
			}
		}
		// A planned change: the balancer gains an address, then loses it. Both appliances get the new config, and
		// nothing is said of a config that is not the row's.
		for _, vips := range [][]string{{"203.0.113.10", "203.0.113.11"}, {"203.0.113.10"}} {
			forget()
			lb.VIPs = vips
			if warned, reloads := pass(); warned != 0 || reloads != 2 {
				t.Fatalf("legacy row %v, the address list became %v: %d warnings, %d keepalived reloads, want none and 2\n%s", legacy, vips, warned, reloads, logged.String())
			}
		}
		if warned, reloads := pass(); warned != 0 || reloads != 0 {
			t.Fatalf("legacy row %v, nothing changed: %d warnings, %d keepalived reloads", legacy, warned, reloads)
		}
		// A file changed behind the product: named once, for that appliance, and put back.
		forget()
		good := f.guests["301"].disk[conf]
		f.set("301", func(g *lbGuest) { g.disk[conf] = good + "# edited by hand\n" })
		if warned, reloads := pass(); warned != 1 || reloads != 1 || !strings.Contains(logged.String(), "vm=301") || f.guests["301"].disk[conf] != good {
			t.Fatalf("legacy row %v, a config edited by hand: %d warnings, %d keepalived reloads, want one of each for vm 301\n%s", legacy, warned, reloads, logged.String())
		}
		if warned, reloads := pass(); warned != 0 || reloads != 0 {
			t.Fatalf("legacy row %v, after the repair: %d warnings, %d keepalived reloads", legacy, warned, reloads)
		}
	}
}

// The upgrade to v0.11.1-rc.2 changes two of keepalived's support files (the track and the notify script) and
// nothing else an existing pair runs: its row records another hash for keepalived's files, so the first pass gives
// each appliance keepalived's files and one reload of keepalived, the standby first, and no HAProxy config: that
// one is the same to the byte, and HAProxy is not reloaded. A pair without a UDP listener is asked nothing of the
// UDP service. (TestLBPairWithUDPKnowsItsHolder shows the same pass for a pair with one.)
func TestLBUpgradePushesKeepalivedAlone(t *testing.T) {
	ctx := context.Background()
	h, f, es, lb := lbApplyFixture(t)
	proj, cr := &Project{ID: "p1"}, &cloud.CloudResource{}
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil || lb.Status != lbStatusActive {
		t.Fatalf("first apply: %s %v", lb.Status, err)
	}
	config, applied := f.guests["302"].file, lb.AppliedHash
	// The row as the release before left it: keepalived's files hashed with that release's support files.
	for i := range lb.Appliances {
		lb.Appliances[i].KeepalivedHash, lb.Appliances[i].KeepalivedConfHash = "the-hash-of-the-release-before", ""
	}
	f.mu.Lock()
	mark := len(f.log)
	f.mu.Unlock()
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil || lb.Status != lbStatusActive || lb.AppliedHash != applied {
		t.Fatalf("the first pass after the upgrade: %s %q %v", lb.Status, lb.StatusMessage, err)
	}
	var did []string
	for _, l := range f.log[mark:] {
		if !strings.Contains(l, " write ") {
			did = append(did, l)
		} else if !strings.HasPrefix(strings.SplitN(l, " write ", 2)[1], "/etc/keepalived/") && !slices.Contains(lbGuestPairFiles, strings.TrimSuffix(strings.SplitN(l, " write ", 2)[1], ".new")) {
			t.Errorf("the first pass after the upgrade wrote %q: only keepalived's config and support files change", l)
		}
	}
	if want := []string{"301 keepalived reload", "302 keepalived reload"}; !slices.Equal(did, want) {
		t.Fatalf("the first pass after the upgrade did %q, want one keepalived reload per appliance, the standby first: %q", did, want)
	}
	if f.guests["301"].file != config || f.guests["302"].file != config || lbHash(lbRender(lb)) != lbHash(config) {
		t.Fatal("HAProxy's config changed at the upgrade")
	}
	for _, a := range lb.Appliances {
		if a.KeepalivedHash == "the-hash-of-the-release-before" || a.KeepalivedConfHash == "" {
			t.Fatalf("appliance %s after the pass: %+v", a.ServerID, a)
		}
	}
	mark = len(f.log)
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil || len(f.log) != mark {
		t.Fatalf("the pass after that wrote %q (%v)", f.log[mark:], err)
	}
}

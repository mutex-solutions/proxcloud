package project

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// A pair takes a change while the image's package manager runs, and waits in one case only (drill R3-F1 and R2b-1 of
// v0.11.1-rc.2). The guests' ready check runs as its own text (pairWire.readyCheck). Shown:
//   - apt runs on both appliances, whose packages are installed: the change is loaded on both, the standby first;
//   - dpkg is at work on HAProxy's own package on the appliance that holds the address: the standby takes the change,
//     the holder is up on the config its row records, and the row reads ACTIVE with what waits and why;
//   - on both: nothing is pushed, both are up, the row is not DOWN, its hash stays what is served, and GET_HEALTH
//     answers;
//   - on the standby alone: the holder is not pushed ahead of it, until the retry window is over.
func TestLBPairTakesAChangeWhileAptRuns(t *testing.T) {
	ctx := context.Background()
	h, f, es, lb := lbApplyFixture(t)
	proj, cr := &Project{ID: "p1"}, &cloud.CloudResource{}
	port := 80
	// change adds a listener and applies; it answers what the pass wrote.
	change := func() []string {
		t.Helper()
		port++
		lb.Listeners = append(lb.Listeners, pve.LBListener{Port: port, Pool: "web"})
		f.mu.Lock()
		mark := len(f.log)
		f.mu.Unlock()
		if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		return append([]string(nil), f.log[mark:]...)
	}
	reloads := func(wrote []string) string {
		var out []string
		for _, l := range wrote {
			if strings.HasSuffix(l, " haproxy reload") {
				out = append(out, strings.TrimSuffix(l, " haproxy reload"))
			}
		}
		return strings.Join(out, ",")
	}
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil || lb.Status != lbStatusActive {
		t.Fatalf("first apply: %s %v", lb.Status, err)
	}
	standby, holder := &lb.Appliances[0], &lb.Appliances[1]

	// 1. Ubuntu's apt timer on both appliances. With the ready check of v0.11.1-rc.2 both were "busy".
	for _, vm := range []string{"301", "302"} {
		f.set(vm, func(g *lbGuest) { g.aptRunning = true })
	}
	if got := reloads(change()); got != "301,302" {
		t.Fatalf("a change while apt runs was loaded on %q, want the standby 301 then the holder 302", got)
	}
	if lb.Status != lbStatusActive || lb.StatusMessage != "" || lb.AppliedHash != lbHash(lbRender(lb)) || lb.waiting || lb.HeldSince != "" {
		t.Fatalf("after a change while apt runs: %s %q, waiting %v", lb.Status, lb.StatusMessage, lb.waiting)
	}

	// 2. dpkg is at work on HAProxy's own package on the holder: the one case that waits.
	f.set("302", func(g *lbGuest) { g.upgrading = true })
	if got := reloads(change()); got != "301" {
		t.Fatalf("with the holder's HAProxy package being upgraded the change was loaded on %q, want 301 alone", got)
	}
	if holder.State != lbAppUp || !lbAppWaits(*holder) || !strings.Contains(holder.Reason, "its package manager is at work on a package the load balancer runs") {
		t.Fatalf("the holder that waits: %+v", *holder)
	}
	if lb.Status != lbStatusActive || !lb.waiting || lb.HeldSince == "" ||
		!strings.Contains(lb.StatusMessage, "A change waits (appliance 302: a change is not loaded on it yet: its package manager is at work") ||
		!strings.Contains(lb.StatusMessage, "the platform tries again within half a minute") {
		t.Fatalf("the row while the holder waits: %s %q, waiting %v since %q", lb.Status, lb.StatusMessage, lb.waiting, lb.HeldSince)
	}
	f.set("302", func(g *lbGuest) { g.upgrading = false })
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil || lb.Status != lbStatusActive || lb.StatusMessage != "" || lb.waiting || lb.HeldSince != "" || holder.Reason != "" {
		t.Fatalf("the pass after the upgrade: %s %q %v, holder %+v", lb.Status, lb.StatusMessage, err, *holder)
	}

	// 3. On both (the lab's R2b-1, where the row read DOWN "No appliance answers" while both served).
	for _, vm := range []string{"301", "302"} {
		f.set(vm, func(g *lbGuest) { g.upgrading = true })
	}
	served := lb.AppliedHash
	if wrote := change(); strings.Contains(strings.Join(wrote, "\n"), " write ") || reloads(wrote) != "" { // the firewall rule of the new port is no file
		t.Fatalf("with both appliances held the pass wrote %v", wrote)
	}
	if lb.Status != lbStatusActive || standby.State != lbAppUp || holder.State != lbAppUp || lb.AppliedHash != served ||
		!strings.Contains(lb.StatusMessage, "appliance 301: a change is not loaded on it yet: its package manager") ||
		!strings.Contains(lb.StatusMessage, "appliance 302: a change is not loaded on it yet: appliance 301 takes every change first") {
		t.Fatalf("both appliances held: %s %q, %+v, row hash moved %v", lb.Status, lb.StatusMessage, lb.Appliances, lb.AppliedHash != served)
	}
	out, herr := h.lbPairHealth(ctx, es, proj, lb)
	if herr != nil {
		t.Fatalf("GET_HEALTH while both appliances wait: %v", herr)
	}
	if apps := out["appliances"].([]lbAppliance); apps[0].State != lbAppUp || apps[1].State != lbAppUp || !apps[1].HoldsVIP {
		t.Fatalf("GET_HEALTH while both appliances wait lists %+v", apps)
	}
	// It answers from the appliance that holds the address, and names the listener of the change as not loaded yet.
	if nl, _ := out["notLoaded"].([]string); len(nl) != 1 || nl[0] != "83" {
		t.Fatalf("GET_HEALTH while both appliances wait: %+v", out)
	}

	// 4. The standby alone is held: the holder is not pushed ahead of it (the lab: the holder got the change 21 s
	// before the standby), until the retry window is over. Then the holder goes ahead, as beside a standby that
	// is down.
	f.set("302", func(g *lbGuest) { g.upgrading = false })
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
		t.Fatal(err)
	}
	if f.guests["302"].loaded == lbRender(lb) || !lbAppWaits(*holder) || !strings.Contains(holder.Reason, "appliance 301 takes every change first and cannot take one now") {
		t.Fatalf("the holder was pushed ahead of a standby that is held: %+v", *holder)
	}
	lb.HeldSince = time.Now().Add(-lbHoldWindow() - time.Second).UTC().Format(time.RFC3339)
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
		t.Fatal(err)
	}
	if f.guests["302"].loaded != lbRender(lb) || lbAppWaits(*holder) || !lbAppWaits(*standby) || lb.Status != lbStatusActive || !lb.waiting {
		t.Fatalf("after the retry window: %s %q, holder %+v", lb.Status, lb.StatusMessage, *holder)
	}
	f.set("301", func(g *lbGuest) { g.upgrading = false })
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil || lb.StatusMessage != "" || lb.waiting || lb.HeldSince != "" || f.guests["301"].loaded != lbRender(lb) {
		t.Fatalf("once the standby takes it: %s %q %v", lb.Status, lb.StatusMessage, err)
	}
}

// A balancer on one VM: a change while apt runs is loaded, and one that waits says what waits and why and asks for
// the platform's own retry.
func TestLBSingleTakesAChangeWhileAptRuns(t *testing.T) {
	ctx := context.Background()
	h, f, es, _ := lbApplyFixture(t)
	f.mu.Lock()
	delete(f.guests, "302")
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
	apply()
	served := lb.AppliedHash
	if lb.Status != lbStatusActive || served == "" {
		t.Fatalf("first apply: %s", lb.Status)
	}
	f.set("301", func(g *lbGuest) { g.aptRunning = true })
	lb.Listeners = append(lb.Listeners, pve.LBListener{Port: 81, Pool: "web"})
	apply()
	if lb.Status != lbStatusActive || lb.AppliedHash == served || lb.StatusMessage != "" || lb.waiting {
		t.Fatalf("a change while apt runs: %s %q, applied moved %v", lb.Status, lb.StatusMessage, lb.AppliedHash != served)
	}
	served = lb.AppliedHash
	f.set("301", func(g *lbGuest) { g.upgrading = true })
	lb.Listeners = append(lb.Listeners, pve.LBListener{Port: 82, Pool: "web"})
	apply()
	if lb.Status != lbStatusActive || lb.AppliedHash != served || !lb.waiting ||
		!strings.Contains(lb.StatusMessage, "A change waits for the load balancer's VM: its package manager is at work on a package the load balancer runs") {
		t.Fatalf("a change during an upgrade of HAProxy: %s %q, waiting %v", lb.Status, lb.StatusMessage, lb.waiting)
	}
	f.set("301", func(g *lbGuest) { g.upgrading = false })
	apply()
	if lb.Status != lbStatusActive || lb.AppliedHash == served || lb.StatusMessage != "" || lb.waiting {
		t.Fatalf("the pass after: %s %q", lb.Status, lb.StatusMessage)
	}
}

// The platform's own retries: one loop per balancer, the first lbRetryEvery after the pass that was held, ended by
// the first pass that leaves nothing waiting and after lbRetryTries of them at the latest.
func TestLBRetryIsBoundedAndSingle(t *testing.T) {
	every, tries := lbRetryEvery, lbRetryTries
	lbRetryEvery, lbRetryTries = 5*time.Millisecond, 3
	defer func() { lbRetryEvery, lbRetryTries = every, tries }()
	h := &Handler{}
	idle := func(key string) {
		t.Helper()
		for range 400 {
			if _, running := h.lbRetries.Load(key); !running {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("the retries of %s did not end", key)
	}
	var passes atomic.Int32
	var bound atomic.Bool
	still := func(ctx context.Context) bool {
		if _, has := ctx.Deadline(); has {
			bound.Store(true)
		}
		passes.Add(1)
		return true
	}
	h.lbRetry("lb-a", still)
	h.lbRetry("lb-a", still) // one loop per balancer
	idle("lb-a")
	if got := passes.Load(); got != 3 {
		t.Fatalf("a change that keeps waiting was tried %d times, want %d", got, 3)
	}
	// A cluster's pass may build an appliance, which takes longer than any bound a retry would set, and a build that
	// is cut off cannot remove its VM: the retry's pass has no deadline, as the sync's and the controller's have none.
	if bound.Load() {
		t.Fatal("a retry's pass was given a deadline")
	}
	passes.Store(0)
	h.lbRetry("lb-a", func(context.Context) bool { return passes.Add(1) < 2 })
	idle("lb-a")
	if got := passes.Load(); got != 2 {
		t.Fatalf("the retries went on after the change was taken: %d passes", got)
	}
	if lbHoldWindow() != 10*time.Millisecond {
		t.Fatalf("the hold window is %s, want the time up to the last retry", lbHoldWindow())
	}
}

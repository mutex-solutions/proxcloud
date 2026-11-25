package project

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// The UDP listeners of a pair through an apply, against the pair fake (the guest itself is the container test's,
// test/integration TestLBApplianceUDP; here the fake keeps the files the agent wrote and sums them as the guest
// does). A pair without a UDP listener is asked and given nothing of the UDP service; with one, each appliance gets
// the files after its firewall rule, the standby first, and the row records a hash only for an appliance that was
// seen to serve; a file changed behind the product, or a stopped service, is pushed again; a config the standby
// refuses does not reach the appliance that holds the address; the last UDP listener takes everything off again.
func TestLBPairUDPListeners(t *testing.T) {
	ctx := context.Background()
	h, f, es, lb := lbApplyFixture(t)
	proj, cr := &Project{ID: "p1"}, &cloud.CloudResource{}
	apply := func() {
		t.Helper()
		if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
			t.Fatal(err)
		}
	}
	logged := func(part string) []string {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []string
		for _, l := range f.log {
			if strings.Contains(l, part) {
				out = append(out, l)
			}
		}
		return out
	}
	writes := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.log)
	}

	// 1. No UDP listener: the pair is what it was before the UDP service existed.
	apply()
	if got := append(append(logged("lvs"), logged("proxcloud-lb/")...), logged("install")...); len(got) != 0 {
		t.Fatalf("a pair without a UDP listener was given something of the UDP service: %q", got)
	}
	tcpHash := lb.AppliedHash
	if lb.Status != lbStatusActive || tcpHash != lbHash(lbRender(lb)) || lb.Appliances[0].LVSHash != "" || lb.Appliances[1].LVSHash != "" {
		t.Fatalf("a pair without a UDP listener: %s, applied %s, appliances %+v", lb.Status, tcpHash, lb.Appliances)
	}
	keepalivedHashes := []string{lb.Appliances[0].KeepalivedHash, lb.Appliances[1].KeepalivedHash}

	// 2. The first UDP listener, on a standby whose image lacks nftables: it installs, and nothing of the listener is
	// recorded for it meanwhile.
	tcp := lb.LBSpec
	// (Members by address, as a cluster's workers are: a server's address is looked up from its VM at every apply.)
	lb.Listeners = append(slices.Clone(tcp.Listeners), pve.LBListener{Port: 53, Protocol: "udp", Pool: "dns"})
	lb.Pools = append(slices.Clone(tcp.Pools), pve.LBPool{Name: "dns", Protocol: "udp", Algorithm: "roundrobin",
		Members: []pve.LBMember{{Node: true, Port: 53, Weight: 1, Address: "10.0.0.11"}, {Node: true, Port: 53, Weight: 1, Address: "10.0.0.12"}}})
	lvs := pve.RenderLVS(lb.LBSpec, lb.VIPs)
	if lvs == "" {
		t.Fatal("no LVS config for a UDP listener")
	}
	f.set("301", func(g *lbGuest) { g.noNft = true })
	apply()
	// It serves the TCP listeners its row records meanwhile, so it is up, with what waits and why; the appliance
	// that holds the address is not given the UDP listeners ahead of it; and the row's hash stays the config that
	// is served (R2b-1 of v0.11.1-rc.2: such an appliance read "down").
	if got := logged("301 install"); len(got) != 1 || lb.Appliances[0].LVSHash != "" || !lbAppWaits(lb.Appliances[0]) ||
		!strings.Contains(lb.Appliances[0].Reason, "it is installing what the load balancer runs") {
		t.Fatalf("a standby without nftables: installs %q, appliance %+v", got, lb.Appliances[0])
	}
	if lb.Appliances[1].LVSHash != "" || !lbAppWaits(lb.Appliances[1]) || lb.Status != lbStatusActive || lb.AppliedHash != tcpHash ||
		!strings.Contains(lb.StatusMessage, "A change waits (appliance 301: ") || !lb.waiting || lb.HeldSince == "" {
		t.Fatalf("while the standby installs: %s %q, applied moved %v, holder %+v", lb.Status, lb.StatusMessage, lb.AppliedHash != tcpHash, lb.Appliances[1])
	}
	apply()
	want := pve.LVSHash(lvs, "ens18")
	for _, a := range lb.Appliances {
		if a.LVSHash != want || a.State != lbAppUp {
			t.Errorf("appliance after the UDP apply: %+v, want the UDP files %s", a, want)
		}
	}
	if lb.Status != lbStatusActive || lb.AppliedHash != lbHash(lbRender(lb)+lvs) || lb.AppliedHash == tcpHash {
		t.Errorf("after the UDP apply: %s, applied %s", lb.Status, lb.AppliedHash)
	}
	for _, vm := range []string{"301", "302"} {
		rule, load := f.first(vm+" rule udp"), f.first(vm+" lvs reload")
		if rule < 0 || load < rule {
			t.Errorf("%s: its UDP rule at %d, its UDP service loaded at %d: the rule comes first", vm, rule, load)
		}
		if !slices.ContainsFunc(f.rules[vm], func(r pve.FirewallRule) bool { return r.Proto == "udp" && r.Dest == "203.0.113.10" && r.DPort == "53" }) {
			t.Errorf("%s rules: %+v", vm, f.rules[vm])
		}
	}
	// The VRRP keepalived was not written again, and HAProxy's config did not move.
	if got := logged("keepalived reload"); len(got) != 2 {
		t.Errorf("keepalived reloads: %q, want the two of the first apply", got)
	}
	if got := logged("haproxy reload"); len(got) != 2 || lb.Appliances[0].KeepalivedHash != keepalivedHashes[0] || lb.Appliances[1].KeepalivedHash != keepalivedHashes[1] {
		t.Errorf("a UDP listener moved the other two files: haproxy reloads %q", got)
	}

	// 3. Nothing changed: nothing is written.
	n := writes()
	apply()
	if writes() != n {
		t.Fatalf("an apply with nothing to do wrote: %q", f.log[n:])
	}

	// 4. The members as the kernel's table holds them, next to HAProxy's.
	ps, _ := h.pveFor(es)
	f.set("302", func(g *lbGuest) { g.dead = []string{"10.0.0.12:53"} })
	health, notLoaded, err := ps.LVSHealth(ctx, "n2", 302, lb.LBSpec, lb.VIPs)
	if err != nil || len(notLoaded) != 0 || len(health) != 2 || health[0].Status != "UP" || health[1].Status != "DOWN" || health[1].Pool != "dns" {
		t.Fatalf("UDP health: %+v %q %v", health, notLoaded, err)
	}
	f.set("302", func(g *lbGuest) { g.dead = nil })

	// 5. A file changed by hand on one appliance: that one is pushed again, the other is left alone.
	f.set("301", func(g *lbGuest) { g.disk["/etc/proxcloud-lb/lvs.nft"] += "# by hand\n" })
	apply()
	if a, b := f.count("301 lvs reload"), f.count("302 lvs reload"); a != 2 || b != 1 || lb.Appliances[0].State != lbAppUp {
		t.Fatalf("after a file changed by hand on 301: %d and %d loads, %+v", a, b, lb.Appliances[0])
	}
	// And a stopped service is started.
	f.set("302", func(g *lbGuest) { g.lvs = false })
	apply()
	if b := f.count("302 lvs reload"); b != 2 || lb.Appliances[1].State != lbAppUp || lb.Status != lbStatusActive {
		t.Fatalf("after the UDP service stopped on 302: %d loads, %+v, %s", b, lb.Appliances[1], lb.Status)
	}

	// 6. A config keepalived refuses on the standby goes no further, and no hash is recorded for it.
	lb.Pools[1].Members = lb.Pools[1].Members[:1]
	f.set("301", func(g *lbGuest) { g.lvsFails = "keepalived: bad configuration" })
	apply()
	if a, b := f.count("301 lvs reload"), f.count("302 lvs reload"); a != 3 || b != 2 || lb.Appliances[0].LVSHash != want || lb.Appliances[1].LVSHash != want {
		t.Fatalf("a refused UDP config: %d and %d loads, hashes %s and %s", a, b, lb.Appliances[0].LVSHash, lb.Appliances[1].LVSHash)
	}
	// The appliance serves the listeners of before, and is up as that: with the change it did not take, in a row
	// that reads ERROR with the refusal, and never as one that runs the new config.
	if !lbAppWaits(lb.Appliances[0]) || !strings.Contains(lb.Appliances[0].Reason, "it did not take it") || lb.Status != lbStatusError ||
		lb.AppliedHash == lbAppliedHash(lb) || lb.waiting {
		t.Fatalf("a refused UDP config reads as applied: %+v, %s, applied %s", lb.Appliances[0], lb.Status, lb.AppliedHash)
	}
	f.set("301", func(g *lbGuest) { g.lvsFails = "" })
	apply()
	if lb.Appliances[0].LVSHash == want || lb.Appliances[0].LVSHash != lb.Appliances[1].LVSHash || lb.AppliedHash != lbAppliedHash(lb) {
		t.Fatalf("after the config was taken: %+v", lb.Appliances)
	}

	// 7. The last UDP listener goes: the service leaves both appliances, and the row is a TCP pair's again.
	lb.LBSpec = tcp
	apply()
	if got := logged("lvs remove"); len(got) != 2 || lb.Appliances[0].LVSHash != "" || lb.Appliances[1].LVSHash != "" || lb.AppliedHash != tcpHash || lb.Status != lbStatusActive {
		t.Fatalf("after the last UDP listener went: removals %q, appliances %+v, applied %s, %s", got, lb.Appliances, lb.AppliedHash, lb.Status)
	}
	for _, vm := range []string{"301", "302"} {
		if slices.ContainsFunc(f.rules[vm], func(r pve.FirewallRule) bool { return r.Proto == "udp" }) || f.guests[vm].lvsSum() != "-" {
			t.Errorf("%s keeps something of UDP: rules %+v", vm, f.rules[vm])
		}
	}
	n = writes()
	apply()
	if writes() != n {
		t.Fatalf("an apply after the removal wrote: %q", f.log[n:])
	}

	// 8. A UDP service the row records nothing of (the push's answer was lost and the listener went before the next
	// pass): the state read shows its files, and it is taken off, on that appliance alone.
	f.set("301", func(g *lbGuest) {
		for _, path := range lbGuestLVSFiles {
			g.disk[path] = "left behind\n"
		}
		g.lvs = true
	})
	apply()
	if a, b := f.count("301 lvs remove"), f.count("302 lvs remove"); a != 2 || b != 1 || f.guests["301"].lvsSum() != "-" || lb.Appliances[0].LVSHash != "" || lb.Status != lbStatusActive {
		t.Fatalf("a UDP service the row does not record: %d and %d removals, files %s, %+v, %s", a, b, f.guests["301"].lvsSum(), lb.Appliances[0], lb.Status)
	}
}

// Drill Q1-2 of v0.11.1-rc.1: once a pair had a UDP listener the state read answered "holds nothing" for both
// appliances (pve's lvsserving() reused the script's variable). The row stayed DEGRADED "Neither appliance holds the
// address yet", the pushes went to the appliances in the row's order, which may be the holder first, and GET_HEALTH
// found no appliance to ask. The fake runs the read's script (pairWire.stateRead), so this is the product's own
// answer: with a UDP listener the pair is ACTIVE and knows its holder, GET_HEALTH answers both pools' members, a
// change goes to the standby first, and a row rc.1 left DEGRADED is put right by the first pass that reads it, which
// pushes keepalived's files alone (the track script is a new one), the standby first, and leaves HAProxy and the UDP
// service as they are.
func TestLBPairWithUDPKnowsItsHolder(t *testing.T) {
	ctx := context.Background()
	h, f, es, lb := lbApplyFixture(t)
	proj, cr := &Project{ID: "p1"}, &cloud.CloudResource{}
	apply := func() {
		t.Helper()
		if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
			t.Fatal(err)
		}
	}
	// since is what the fake logged after mark, of the lines that contain part.
	since := func(mark int, part string) []string {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []string
		for _, l := range f.log[mark:] {
			if strings.Contains(l, part) {
				out = append(out, l)
			}
		}
		return out
	}
	mark := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.log)
	}
	// The holder is the first appliance of the row: the row's order is not the push order.
	f.set("301", func(g *lbGuest) { g.role, g.holds = "MASTER", true })
	f.set("302", func(g *lbGuest) { g.role, g.holds = "BACKUP", false })
	lb.Appliances[1].HoldsVIP = false
	lb.Pools[0].Members = []pve.LBMember{{Node: true, Port: 8080, Weight: 1, Address: "10.0.0.11"}}
	lb.Listeners = append(lb.Listeners, pve.LBListener{Port: 53, Protocol: "udp", Pool: "dns"})
	lb.Pools = append(lb.Pools, pve.LBPool{Name: "dns", Protocol: "udp", Algorithm: "roundrobin",
		Members: []pve.LBMember{{Node: true, Port: 53, Weight: 1, Address: "10.0.0.11"}, {Node: true, Port: 53, Weight: 1, Address: "10.0.0.12"}}})
	apply()
	served := func(when string) {
		t.Helper()
		if lb.Status != lbStatusActive || !lb.Appliances[0].HoldsVIP || lb.Appliances[1].HoldsVIP || lb.Appliances[0].Role != "MASTER" ||
			lb.Appliances[0].LVSHash == "" || lb.AppliedHash != lbAppliedHash(lb) {
			t.Fatalf("%s: %s %q, appliances %+v", when, lb.Status, lb.StatusMessage, lb.Appliances)
		}
		out, herr := h.lbPairHealth(ctx, es, proj, lb)
		if herr != nil {
			t.Fatalf("%s: GET_HEALTH: %v", when, herr)
		}
		health, _ := out["result"].([]pve.LBMemberHealth)
		var got []string
		for _, m := range health {
			got = append(got, m.Pool+" "+m.Address+" "+m.Status)
		}
		if want := []string{"web 10.0.0.11 UP", "dns 10.0.0.11 UP", "dns 10.0.0.12 UP"}; !slices.Equal(got, want) {
			t.Fatalf("%s: GET_HEALTH says %q, want %q", when, got, want)
		}
	}
	served("a pair with a UDP listener")

	// A change while the UDP listener is served: the standby first, HAProxy and the UDP service alike. Each appliance
	// takes each of the two once, and the standby has taken both before the appliance that holds the address is
	// given either. Which of the two an appliance takes first is not the product's to say: they are started
	// together (lbApplyPair), so the two loads of one appliance are compared as a set.
	m := mark()
	lb.Listeners = append(lb.Listeners, pve.LBListener{Port: 81, Pool: "web"}, pve.LBListener{Port: 54, Protocol: "udp", Pool: "dns"})
	apply()
	got, want := since(m, " reload"), []string{"302 haproxy reload", "302 lvs reload", "301 haproxy reload", "301 lvs reload"}
	perAppliance := slices.Clone(got)
	if len(got) == len(want) {
		slices.Sort(perAppliance[:2])
		slices.Sort(perAppliance[2:])
	}
	if !slices.Equal(perAppliance, want) {
		t.Fatalf("a change with a UDP listener served went %q, want the standby's two loads and then the holder's two, in either order on each: %q", got, want)
	}
	served("after the change")

	// The row as v0.11.1-rc.1 left it, and the appliances as they are: serving, with the keepalived files of rc.1.
	for i := range lb.Appliances {
		lb.Appliances[i].HoldsVIP, lb.Appliances[i].KeepalivedHash = false, "the-hash-of-rc.1's-files"
	}
	lb.Status, lb.StatusMessage, lb.AppliedHash = lbStatusDegraded, "Neither appliance holds the address yet: they are choosing which one does.", ""
	m = mark()
	apply()
	if got, want := since(m, " reload"), []string{"302 keepalived reload", "301 keepalived reload"}; !slices.Equal(got, want) {
		t.Fatalf("the first pass after the upgrade reloaded %q, want keepalived alone, the standby first: %q", got, want)
	}
	if got := append(since(m, "haproxy.cfg"), since(m, "proxcloud-lb/")...); len(got) != 0 {
		t.Fatalf("the first pass after the upgrade wrote %q: HAProxy's config and the UDP service's files have not changed", got)
	}
	served("the first pass after the upgrade")
	m = mark()
	apply()
	if got := since(m, ""); len(got) != 0 {
		t.Fatalf("the pass after that wrote %q", got)
	}
}

// UDP is a pair's: a single appliance has no keepalived, and says so by name when asked for a UDP listener.
func TestLBSingleRefusesUDP(t *testing.T) {
	spec, err := lbSpecOfRequest(map[string]any{
		"listeners": []any{map[string]any{"port": float64(53), "protocol": "udp", "pool": "dns"}},
		"pools":     []any{map[string]any{"name": "dns", "members": []any{}}}})
	if err != nil || !spec.HasUDP() || spec.Pools[0].Protocol != "udp" {
		t.Fatalf("a UDP listener in a request: %+v %v", spec, err)
	}
	if why := lbUDPNeedsPair(spec, false); !strings.Contains(why, "UDP needs a high-availability load balancer") {
		t.Fatalf("a single appliance and a UDP listener: %q", why)
	}
	if why := lbUDPNeedsPair(spec, true); why != "" {
		t.Fatalf("a pair and a UDP listener: %q", why)
	}
	if why := lbUDPNeedsPair(pve.LBSpec{Listeners: []pve.LBListener{{Port: 80, Protocol: "tcp", Pool: "w"}}}, false); why != "" {
		t.Fatalf("a single appliance without UDP: %q", why)
	}
}

// What a change of a pair with a UDP listener costs and what waits for what (drill R2a-1 of v0.11.1-rc.2: a TCP and
// a UDP port added in one change answered over UDP after 33 s, and a member change wrote five files on each
// appliance). Counted here against the fake pair:
//
//   - a member that comes to a UDP pool writes one file on each appliance, lvs.conf, where it wrote five, with the
//     same ten guest commands (two state reads, three for each push, two reads back);
//   - a TCP and a UDP port added in one change: on each appliance the UDP push is asked for while HAProxy's is at
//     work, and the appliance that holds the address is given nothing before both pushes of the standby answered.
//
// The fake's guest commands take no time, so what the overlap saves on a cluster (HAProxy's reload took 4 to 5 s on
// the lab) is the lab's to show.
func TestLBPairUDPChangeCost(t *testing.T) {
	ctx := context.Background()
	h, f, es, lb := lbApplyFixture(t)
	proj, cr := &Project{ID: "p1"}, &cloud.CloudResource{}
	// apply runs a pass and answers the files it wrote and the guest commands it ran.
	apply := func() (writes []string, execs int) {
		t.Helper()
		f.mu.Lock()
		n, e := len(f.log), f.execs
		f.mu.Unlock()
		if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, l := range f.log[n:] {
			if strings.Contains(l, " write ") {
				writes = append(writes, l)
			}
		}
		return writes, f.execs - e
	}
	apply()
	lb.Listeners = append(lb.Listeners, pve.LBListener{Port: 53, Protocol: "udp", Pool: "dns"})
	lb.Pools = append(lb.Pools, pve.LBPool{Name: "dns", Protocol: "udp", Algorithm: "roundrobin",
		Members: []pve.LBMember{{Node: true, Port: 53, Weight: 1, Address: "10.0.0.11"}}})
	if writes, _ := apply(); len(writes) != 2*len(lbGuestLVSFiles) || lb.Status != lbStatusActive {
		t.Fatalf("the first UDP listener: %s, wrote %q, want every file of the UDP service on each appliance", lb.Status, writes)
	}

	// A member comes.
	lb.Pools[1].Members = append(lb.Pools[1].Members, pve.LBMember{Node: true, Port: 53, Weight: 1, Address: "10.0.0.12"})
	writes, execs := apply()
	if want := []string{"301 write /etc/keepalived/lvs.conf.new", "302 write /etc/keepalived/lvs.conf.new"}; !slices.Equal(writes, want) || execs != 10 || lb.Status != lbStatusActive {
		t.Fatalf("a member added to a UDP pool: %s, wrote %q with %d guest commands, want %q with 10", lb.Status, writes, execs, want)
	}
	if hash := pve.LVSHash(pve.RenderLVS(lb.LBSpec, lb.VIPs), "ens18"); lb.Appliances[0].LVSHash != hash || lb.Appliances[1].LVSHash != hash || f.guests["301"].lvsSum() != hash {
		t.Fatalf("after the member: the row records %s and %s, the standby's files sum to %s, want %s", lb.Appliances[0].LVSHash, lb.Appliances[1].LVSHash, f.guests["301"].lvsSum(), hash)
	}

	// A TCP and a UDP port in one change. meet makes each appliance's HAProxy apply wait for its UDP apply to be
	// asked for: pushes made one after the other fail here.
	f.mu.Lock()
	f.meet = map[string]chan struct{}{"301": make(chan struct{}), "302": make(chan struct{})}
	n := len(f.log)
	f.mu.Unlock()
	lb.Listeners = append(lb.Listeners, pve.LBListener{Port: 8080, Pool: "web"}, pve.LBListener{Port: 54, Protocol: "udp", Pool: "dns"})
	apply()
	f.mu.Lock()
	defer f.mu.Unlock()
	last301, first302 := -1, -1
	for i, l := range f.log[n:] {
		switch {
		case strings.HasPrefix(l, "301 ") && strings.HasSuffix(l, " reload"):
			last301 = i
		case strings.HasPrefix(l, "302 ") && strings.HasSuffix(l, " reload") && first302 < 0:
			first302 = i
		}
	}
	if len(f.meet) != 0 || last301 < 0 || first302 < last301 {
		t.Fatalf("a TCP and a UDP port in one change: %d appliances whose two pushes did not meet; the standby's last load at %d and the holder's first at %d of:\n%s",
			len(f.meet), last301, first302, strings.Join(f.log[n:], "\n"))
	}
	for _, a := range lb.Appliances {
		if a.HAProxyHash != lbHash(lbRender(lb)) || a.LVSHash != pve.LVSHash(pve.RenderLVS(lb.LBSpec, lb.VIPs), "ens18") || a.State != lbAppUp {
			t.Errorf("after the change: %+v", a)
		}
	}
	if lb.Status != lbStatusActive {
		t.Errorf("after the change: %s %q", lb.Status, lb.StatusMessage)
	}
}

// A pair whose kernel holds a connection limit the config does not have, which is what a pair of v0.11.1-rc.2 holds
// after a limit was changed on a UDP listener (drill R3-F2: keepalived's reload left the old figure). It answers UDP
// on both appliances, so it is a change that is not loaded and not an appliance that is down: the row stays ACTIVE
// and says that a change waits, the standby is pushed first, and while the standby cannot take a push the appliance
// that holds the address is given none. The table is the fake's (lbGuest.staleLimit); the state read's script runs
// as the text it is.
func TestLBPairUDPTableThatDiffers(t *testing.T) {
	ctx := context.Background()
	h, f, es, lb := lbApplyFixture(t)
	proj, cr := &Project{ID: "p1"}, &cloud.CloudResource{}
	// apply runs a pass and answers what the appliances were asked to load.
	apply := func() (loads []string) {
		t.Helper()
		f.mu.Lock()
		n := len(f.log)
		f.mu.Unlock()
		if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, l := range f.log[n:] {
			if strings.HasSuffix(l, " reload") || strings.Contains(l, " write ") {
				loads = append(loads, l)
			}
		}
		return loads
	}
	apply()
	lb.Listeners = append(lb.Listeners, pve.LBListener{Port: 53, Protocol: "udp", Pool: "dns"})
	lb.Pools = append(lb.Pools, pve.LBPool{Name: "dns", Protocol: "udp", Algorithm: "roundrobin",
		Members: []pve.LBMember{{Node: true, Port: 53, Weight: 1, Address: "10.0.0.11"}}})
	if apply(); lb.Status != lbStatusActive || lb.StatusMessage != "" {
		t.Fatalf("the first UDP listener: %s %q", lb.Status, lb.StatusMessage)
	}
	if loads := apply(); len(loads) != 0 {
		t.Fatalf("a pass over a pair that is in order loaded %q", loads)
	}

	// Both appliances hold the stale limit, and the standby (301) cannot take a push: dpkg is at work on it.
	f.set("301", func(g *lbGuest) { g.staleLimit, g.upgrading = 5, true })
	f.set("302", func(g *lbGuest) { g.staleLimit = 5 })
	loads := apply()
	if len(loads) != 0 {
		t.Errorf("while the standby holds its push, the pass loaded %q: the appliance that holds the address takes no change before the standby", loads)
	}
	for _, a := range lb.Appliances {
		if a.State != lbAppUp || !lbAppWaits(a) {
			t.Errorf("appliance %s answers UDP with the limit of before: %s %q, want up and waiting for a change", a.ServerID, a.State, a.Reason)
		}
	}
	if lb.Status != lbStatusActive || !strings.Contains(lb.StatusMessage, "A change waits") || !lb.waiting {
		t.Errorf("the pair serves and has a change to load: %s %q (tried again by the platform: %v), want ACTIVE with what waits", lb.Status, lb.StatusMessage, lb.waiting)
	}

	// The standby is free again: it is pushed first, then the holder, with no file to write, and the row is in order.
	f.set("301", func(g *lbGuest) { g.upgrading = false })
	if loads := apply(); !slices.Equal(loads, []string{"301 lvs reload", "302 lvs reload"}) {
		t.Errorf("the pass after the standby was free loaded %q, want the UDP service of the standby and then of the holder", loads)
	}
	if lb.Status != lbStatusActive || lb.StatusMessage != "" || lb.waiting || f.guests["301"].staleLimit != 0 || f.guests["302"].staleLimit != 0 {
		t.Errorf("after the push: %s %q waiting=%v", lb.Status, lb.StatusMessage, lb.waiting)
	}
	if loads := apply(); len(loads) != 0 {
		t.Errorf("the pass after that loaded %q", loads)
	}

	// The holder alone: it is pushed, the standby is not.
	f.set("302", func(g *lbGuest) { g.staleLimit = 5 })
	if loads := apply(); !slices.Equal(loads, []string{"302 lvs reload"}) || lb.Status != lbStatusActive || lb.StatusMessage != "" {
		t.Errorf("a table that differs on the holder alone: loaded %q, %s %q", loads, lb.Status, lb.StatusMessage)
	}
}

// A replace's planned switch with a UDP listener (drill R3-F3 of v0.11.1-rc.2: the address moved to the new
// appliance before any member was in its UDP table; UDP lost 2.4 s where TCP lost 0.8 s, and one port had no member
// for 5.2 s). keepalived is stopped on the appliance that goes only when the kernel's table of the one that takes
// the address holds every member the other one holds, or after lbUDPMembersWait, and then the log says so.
//
// The tables here are the fake's, in the text the kernel prints: that keepalived fills a table only after a
// member's first good check, and how long that takes, is the real-guest test's (test/integration/lb_udp_test.go
// and lb_udp_changes_test.go).
func TestLBPlannedSwitchWaitsForUDPMembers(t *testing.T) {
	ctx := context.Background()
	h, f, es, lb := lbApplyFixture(t)
	ps, _ := h.pveFor(es)
	stopped := func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Contains(f.log, "302 keepalived stop")
	}
	execs := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.execs
	}
	again := func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.log = slices.DeleteFunc(f.log, func(l string) bool { return l == "302 keepalived stop" })
		g, o := f.guests["302"], f.guests["301"]
		g.keepalived, g.role, g.holds, o.role, o.holds = true, "MASTER", true, "BACKUP", false
	}
	apply := func(when string) {
		t.Helper()
		if err := h.lbApplyPair(ctx, es, &Project{ID: "p1"}, &cloud.CloudResource{}, lb); err != nil || lb.Status != lbStatusActive {
			t.Fatalf("%s: %s %q %v", when, lb.Status, lb.StatusMessage, err)
		}
	}
	apply("the pair")
	// Without a UDP listener nothing is asked: the one command is the stop.
	n := execs()
	lbPlannedSwitch(ctx, ps, lb, "n2", 302, "n1", 301)
	if !stopped() || execs() != n+1 {
		t.Fatalf("a pair without a UDP listener: stopped %v with %d guest commands, want the stop alone", stopped(), execs()-n)
	}
	again()

	lb.Listeners = append(lb.Listeners, pve.LBListener{Port: 53, Protocol: "udp", Pool: "dns"})
	lb.Pools = append(lb.Pools, pve.LBPool{Name: "dns", Protocol: "udp", Algorithm: "roundrobin", Members: []pve.LBMember{
		{Node: true, Port: 53, Weight: 1, Address: "10.0.0.11"}, {Node: true, Port: 53, Weight: 1, Address: "10.0.0.12"}, {Node: true, Port: 53, Weight: 1, Address: "10.0.0.13"}}})
	apply("the UDP listener")
	// The appliance that goes, 302, holds two of the three members (the third fails its check everywhere). The
	// one that takes the address was started a moment ago: its table holds the listener and no member yet. Both
	// read "up".
	f.set("302", func(g *lbGuest) { g.dead = []string{"10.0.0.13:53"} })
	f.set("301", func(g *lbGuest) { g.dead = []string{"10.0.0.11:53", "10.0.0.12:53", "10.0.0.13:53"} })
	done := make(chan struct{})
	go func() { lbPlannedSwitch(ctx, ps, lb, "n2", 302, "n1", 301); close(done) }()
	time.Sleep(1500 * time.Millisecond)
	if stopped() {
		t.Fatal("keepalived was stopped on the appliance that holds the address while the other one had no UDP member in its table")
	}
	// One member in is not what the other one holds.
	f.set("301", func(g *lbGuest) { g.dead = []string{"10.0.0.12:53", "10.0.0.13:53"} })
	time.Sleep(1500 * time.Millisecond)
	if stopped() {
		t.Fatal("keepalived was stopped while the appliance that takes the address had one of the two members the other one holds")
	}
	// Both in; the member that is down on the one that goes is not waited for.
	f.set("301", func(g *lbGuest) { g.dead = []string{"10.0.0.13:53"} })
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the switch did not go on once the appliance that takes the address held the members")
	}
	if !stopped() {
		t.Fatal("the planned switch ended without stopping keepalived")
	}
	again()

	// Bounded: an appliance that never gets the members is switched to all the same.
	defer func(d time.Duration) { lbUDPMembersWait = d }(lbUDPMembersWait)
	lbUDPMembersWait = 2 * time.Second
	f.set("301", func(g *lbGuest) { g.dead = []string{"10.0.0.11:53", "10.0.0.12:53", "10.0.0.13:53"} })
	start := time.Now()
	lbPlannedSwitch(ctx, ps, lb, "n2", 302, "n1", 301)
	if took := time.Since(start); !stopped() || took < 2*time.Second || took > 8*time.Second {
		t.Fatalf("an appliance that never gets its members: stopped %v after %s, want the switch after the wait of %s", stopped(), took, lbUDPMembersWait)
	}
}

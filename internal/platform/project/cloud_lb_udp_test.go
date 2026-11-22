package project

import (
	"context"
	"slices"
	"strings"
	"testing"

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
	if got := logged("301 install"); len(got) != 1 || lb.Appliances[0].LVSHash != "" || lb.Appliances[0].State == lbAppUp {
		t.Fatalf("a standby without nftables: installs %q, appliance %+v", got, lb.Appliances[0])
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
	if lb.Appliances[0].State == lbAppUp || lb.AppliedHash == lbAppliedHash(lb) {
		t.Fatalf("a refused UDP config reads as applied: %+v, applied %s", lb.Appliances[0], lb.AppliedHash)
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

package project

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// Review of #446: an appliance that joins is given keepalived after the one that served alone was told of it
// (lbPushOrder). When that one refused the push, the pass went on to the new appliance all the same: it heard no
// advert, became a second master and held the addresses beside the first until the next pass. Now the new one gets
// no keepalived in such a pass, reads "building" with the appliance it waits for, and the next pass tries both, in
// the order. The fake's guests elect as keepalived does (cloud_lb_wire_test.go): one that starts takes the addresses
// unless a master whose config names it runs.
//
// Each case starts from an appliance that serves alone (301) and a second one in the row, fresh from its image
// (302). What stays as it was: with no refusal both are pushed in one pass (the control); a first appliance that
// holds its push is waited for one retry window and no longer; one whose keepalived is stopped holds no address, so
// the new one is pushed and takes over.
func TestLBJoinWaitsForAPeerThatRefused(t *testing.T) {
	ctx := context.Background()
	proj, cr := &Project{ID: "p1"}, &cloud.CloudResource{}
	const refusal = "keepalived did not take the change: a-word-of-the-guest"
	const reason = "it waits for appliance 301, which did not take the change that tells it of this one"
	// join answers a pair whose first appliance served alone for a pass and whose second is in the row, and apply,
	// which runs a pass and answers the keepalived reloads it made, in their order.
	join := func(t *testing.T) (*pairWire, *loadBalancer, func() string) {
		t.Helper()
		h, f, es, lb := lbApplyFixture(t)
		apply := func() string {
			t.Helper()
			f.mu.Lock()
			mark := len(f.log)
			f.mu.Unlock()
			if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			var out []string
			for _, l := range f.log[mark:] {
				if vm, ok := strings.CutSuffix(l, " keepalived reload"); ok {
					out = append(out, vm)
				}
			}
			return strings.Join(out, ",")
		}
		lb.Appliances = lb.Appliances[:1]
		f.mu.Lock()
		delete(f.guests, "302")
		*f.guests["301"] = lbGuest{packages: "installed", haproxy: true}
		f.mu.Unlock()
		if got := apply(); got != "301" || lb.Status != lbStatusDegraded || lb.Appliances[0].KeepalivedHash == "" || !lb.Appliances[0].HoldsVIP {
			t.Fatalf("the first appliance alone: keepalived on %q, %s %q, %+v", got, lb.Status, lb.StatusMessage, lb.Appliances[0])
		}
		f.mu.Lock()
		f.guests["302"] = &lbGuest{packages: "installed", haproxy: true}
		f.mu.Unlock()
		lbPutIn(lb, lbAppliance{ServerID: "302", PrivateIP: "10.0.0.32"})
		return f, lb, apply
	}
	// masters lists the guests that hold the addresses.
	masters := func(f *pairWire) string {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []string
		for _, vm := range []string{"301", "302"} {
			if g := f.guests[vm]; g.keepalived && g.holds {
				out = append(out, vm)
			}
		}
		return strings.Join(out, ",")
	}

	t.Run("control: no refusal, both in one pass", func(t *testing.T) {
		f, lb, apply := join(t)
		if got := apply(); got != "301,302" || masters(f) != "301" || lb.Status != lbStatusActive || lb.Appliances[1].Role != "BACKUP" {
			t.Fatalf("keepalived on %q, the addresses on %q, %s %q, %+v", got, masters(f), lb.Status, lb.StatusMessage, lb.Appliances)
		}
	})

	t.Run("the first appliance refuses to be told", func(t *testing.T) {
		f, lb, apply := join(t)
		f.set("301", func(g *lbGuest) { g.vrrpFails = "a-word-of-the-guest" })
		for range 2 { // and not at the pass after either, while it still refuses
			got := apply()
			a, b := lb.Appliances[0], lb.Appliances[1]
			if got != "" || masters(f) != "301" {
				t.Fatalf("the pass in which 301 refused gave keepalived to %q, and the addresses are on %q", got, masters(f))
			}
			if lb.Status != lbStatusError || lb.StatusMessage != refusal || b.State != lbAppBuilding || b.Reason != reason || b.KeepalivedHash != "" || b.HAProxyHash != "" ||
				a.State != lbAppUp || !a.HoldsVIP || lb.HeldSince != "" {
				t.Fatalf("the row of that pass: %s %q held since %q, %+v", lb.Status, lb.StatusMessage, lb.HeldSince, lb.Appliances)
			}
		}
		f.set("301", func(g *lbGuest) { g.vrrpFails = "" })
		got := apply()
		a, b := lb.Appliances[0], lb.Appliances[1]
		if got != "301,302" || masters(f) != "301" || lb.Status != lbStatusActive || lb.StatusMessage != "" ||
			a.Role != "MASTER" || b.Role != "BACKUP" || b.State != lbAppUp || b.Reason != "" {
			t.Fatalf("the pass after: keepalived on %q, the addresses on %q, %s %q, %+v", got, masters(f), lb.Status, lb.StatusMessage, lb.Appliances)
		}
	})

	t.Run("a first appliance that holds its push is waited for one window", func(t *testing.T) {
		f, lb, apply := join(t)
		f.set("301", func(g *lbGuest) { g.upgrading = true })
		if got, b := apply(), lb.Appliances[1]; got != "" || lb.HeldSince == "" || b.State != lbAppBuilding ||
			b.Reason != "it waits for appliance 301, which takes every change first" {
			t.Fatalf("inside the window: keepalived on %q, held since %q, %+v", got, lb.HeldSince, b)
		}
		lb.HeldSince = time.Now().Add(-lbHoldWindow() - time.Second).UTC().Format(time.RFC3339)
		if got := apply(); got != "302" || !slices.Contains(strings.Split(masters(f), ","), "302") || lb.Appliances[1].KeepalivedHash == "" {
			t.Fatalf("after the window: keepalived on %q, the addresses on %q, %+v", got, masters(f), lb.Appliances[1])
		}
	})

	t.Run("a first appliance whose keepalived is stopped is not waited for", func(t *testing.T) {
		f, lb, apply := join(t)
		f.set("301", func(g *lbGuest) { g.keepalived, g.holds, g.vrrpFails = false, false, "a-word-of-the-guest" })
		if got := apply(); got != "302" || masters(f) != "302" || lb.Status != lbStatusError || !lb.Appliances[1].HoldsVIP {
			t.Fatalf("keepalived on %q, the addresses on %q, %s %q, %+v", got, masters(f), lb.Status, lb.StatusMessage, lb.Appliances)
		}
	})

	// A pair built at once: keepalived ran on neither, so the one that refuses holds no address the other could
	// take beside it.
	t.Run("a pair whose first push of all is refused", func(t *testing.T) {
		h, f, es, lb := lbApplyFixture(t)
		f.mu.Lock()
		for _, vm := range []string{"301", "302"} {
			*f.guests[vm] = lbGuest{packages: "installed", haproxy: true}
		}
		f.guests["301"].vrrpFails = "a-word-of-the-guest"
		f.mu.Unlock()
		lb.Appliances[1].HoldsVIP = false
		if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
			t.Fatal(err)
		}
		if f.count("302 keepalived reload") != 1 || masters(f) != "302" || lb.Status != lbStatusError || lb.Appliances[0].Reason == reason {
			t.Fatalf("the addresses are on %q, %s %q, %+v", masters(f), lb.Status, lb.StatusMessage, lb.Appliances)
		}
	})
}

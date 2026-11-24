package project

import (
	"context"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// GET_HEALTH reads each appliance's role at the call and asks the appliance that holds the address now (drill R2b-o2
// and R3 of v0.11.1-rc.2: after a failover the row was right only from the next pass, about 70 s later, and until
// then GET_HEALTH answered 409 or listed two masters). The row here is the one of before the failover.
func TestLBPairHealthAfterAFailover(t *testing.T) {
	ctx := context.Background()
	h, f, es, lb := lbApplyFixture(t)
	proj, cr := &Project{ID: "p1"}, &cloud.CloudResource{}
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil || lb.Status != lbStatusActive || !lb.Appliances[1].HoldsVIP {
		t.Fatalf("first apply: %s %v", lb.Status, err)
	}
	health := func() (map[string]any, []lbAppliance) {
		t.Helper()
		out, herr := h.lbPairHealth(ctx, es, proj, lb)
		if herr != nil {
			t.Fatalf("GET_HEALTH: %v", herr)
		}
		return out, out["appliances"].([]lbAppliance)
	}
	// The master's VM was stopped (qm stop) and the standby took the address; no pass has run since.
	f.set("302", func(g *lbGuest) { g.stopped = true })
	f.set("301", func(g *lbGuest) { g.role, g.holds = "MASTER", true })
	out, apps := health()
	if apps[0].Role != "MASTER" || !apps[0].HoldsVIP || apps[0].State != lbAppUp {
		t.Errorf("the appliance that took the address: %+v", apps[0])
	}
	if apps[1].Role != "" || apps[1].HoldsVIP || apps[1].State != lbAppDown || apps[1].Reason != lbReasonUnreachable {
		t.Errorf("the stopped appliance is listed with the row's role: %+v", apps[1])
	}
	if _, has := out["result"].([]pve.LBMemberHealth); !has {
		t.Errorf("no members in the answer: %+v", out)
	}
	// Its guest agent alone is gone (a guest shutdown under way): the same.
	f.set("302", func(g *lbGuest) { g.stopped, g.agentDown = false, true })
	if _, apps = health(); apps[1].Role != "" || apps[1].HoldsVIP || apps[1].State != lbAppDown {
		t.Errorf("the appliance whose agent is gone: %+v", apps[1])
	}
	// A row a health read marked (lbCorrectRoles), and the appliance answers again, as the standby: it is listed as
	// what it is now, not as "cannot be reached" until the next pass.
	lb.Appliances[1].Role, lb.Appliances[1].HoldsVIP, lb.Appliances[1].State, lb.Appliances[1].Reason = "", false, lbAppDown, lbReasonUnreachable
	f.set("302", func(g *lbGuest) { g.agentDown, g.role, g.holds = false, "BACKUP", false })
	if _, apps = health(); apps[1].State != lbAppUp || apps[1].Reason != "" || apps[1].Role != "BACKUP" {
		t.Errorf("the appliance that answers again: %+v", apps[1])
	}
	// The pass's marks for an appliance it could not read are taken back the same way (lab finding T2-o1 of
	// v0.11.1-rc.3: after a failover the appliance the pass started read "its VM is stopped; starting it", and the
	// balancer DEGRADED, until the next pass, with the appliance back as the standby).
	for _, reason := range []string{lbReasonStarting, lbReasonNoAnswer} {
		lb.Appliances[1].State, lb.Appliances[1].Reason = lbAppDown, reason
		if _, apps = health(); apps[1].State != lbAppUp || apps[1].Reason != "" || apps[1].Role != "BACKUP" {
			t.Errorf("the appliance the pass marked %q, which answers and serves: %+v", reason, apps[1])
		}
		// Still booting: its agent does not answer yet, and the mark stays.
		f.set("302", func(g *lbGuest) { g.agentDown = true })
		lb.Appliances[1].State, lb.Appliances[1].Reason = lbAppDown, reason
		if _, apps = health(); apps[1].State != lbAppDown || apps[1].Reason != reason {
			t.Errorf("the appliance the pass marked %q, which does not answer yet: %+v", reason, apps[1])
		}
		// It answers and HAProxy does not run: not taken back.
		f.set("302", func(g *lbGuest) { g.agentDown, g.haproxy = false, false })
		if _, apps = health(); apps[1].State != lbAppDown || apps[1].Reason != reason {
			t.Errorf("the appliance the pass marked %q, which answers and does not serve: %+v", reason, apps[1])
		}
		f.set("302", func(g *lbGuest) { g.haproxy = true })
	}
	// A pair with a UDP listener (the row records the UDP files' hash): the mark is taken back only when the UDP
	// service serves too. An appliance that is back with HAProxy serving and its UDP unit not running does not
	// serve what its row records, and stays down until a pass has loaded it.
	lb.Appliances[1].State, lb.Appliances[1].Reason, lb.Appliances[1].LVSHash = lbAppDown, lbReasonStarting, "recorded"
	if _, apps = health(); apps[1].State != lbAppDown || apps[1].Reason != lbReasonStarting {
		t.Errorf("the appliance the pass started, whose UDP service does not serve: %+v", apps[1])
	}
	f.set("302", func(g *lbGuest) { g.lvs, g.disk[lbGuestLVSFiles[0]] = true, "instance lvs\n" })
	if _, apps = health(); apps[1].State != lbAppUp || apps[1].Reason != "" {
		t.Errorf("the appliance the pass started, whose UDP service serves: %+v", apps[1])
	}
	f.set("302", func(g *lbGuest) { g.lvs = false; delete(g.disk, lbGuestLVSFiles[0]) })
	lb.Appliances[1].LVSHash = ""
	// One that is down for another reason stays so: a finding of the pass is the pass's to take back.
	lb.Appliances[1].State, lb.Appliances[1].Reason = lbAppDown, "keepalived is fault"
	if _, apps = health(); apps[1].State != lbAppDown || apps[1].Reason != "keepalived is fault" {
		t.Errorf("an appliance the pass found down: %+v", apps[1])
	}
	lb.Appliances[1].State, lb.Appliances[1].Reason = lbAppUp, ""
	// The seconds of a failover: nobody holds the address yet. An appliance that answers is asked all the same.
	f.set("301", func(g *lbGuest) { g.role, g.holds = "BACKUP", false })
	if out, apps = health(); apps[0].HoldsVIP || out["result"] == nil {
		t.Errorf("with no holder: %+v", out)
	}
	if _, ok := out["notLoaded"]; ok {
		t.Errorf("with no holder the answer names listeners as not loaded: %+v", out)
	}
}

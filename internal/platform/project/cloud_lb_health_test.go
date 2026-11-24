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
	// The seconds of a failover: nobody holds the address yet. An appliance that answers is asked all the same.
	f.set("301", func(g *lbGuest) { g.role, g.holds = "BACKUP", false })
	if out, apps = health(); apps[0].HoldsVIP || out["result"] == nil {
		t.Errorf("with no holder: %+v", out)
	}
	if _, ok := out["notLoaded"]; ok {
		t.Errorf("with no holder the answer names listeners as not loaded: %+v", out)
	}
}

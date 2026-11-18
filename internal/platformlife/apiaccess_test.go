package platformlife

import (
	"slices"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// The sources are the seed and the node range, exactly: the MetalLB range and the API address call nothing.
func TestAPIAccessOfAnInstallation(t *testing.T) {
	in := validInstallation(t)
	src, dest, port := in.APIAccess()
	want := []string{"10.200.200.10/32", "10.200.200.30/31", "10.200.200.32/28", "10.200.200.48/31"}
	if !slices.Equal(src, want) || dest != "10.200.200.2" || port != "8006" {
		t.Fatalf("sources %v, dest %q, port %q", src, dest, port)
	}
	in.Spec.PVEAPIURL = "https://pve.example.net"
	if _, dest, port := in.APIAccess(); dest != "" || port != "443" {
		t.Fatalf("a host name: dest %q, port %q", dest, port)
	}
}

// Stage 3 writes the rule before the seed is cloned and says so; an apply of an installed platform puts back a rule
// deleted by hand and writes nothing when it is there; Remove deletes it and leaves every other rule.
func TestSeedVMWritesTheAPIRuleAndRemoveDeletesIt(t *testing.T) {
	ops := pve.FirewallRule{Type: "in", Action: "ACCEPT", Dest: "10.9.9.9", Comment: "ops", Enable: 1}
	e := newEnv(t, RunApply)
	e.only(StageSeedVM)
	e.pve.clusterRules = []pve.FirewallRule{ops}
	e.once()
	mine := func() []pve.FirewallRule {
		return slices.DeleteFunc(slices.Clone(e.pve.clusterRules), func(r pve.FirewallRule) bool { return r.Comment != pve.APIAccessComment(inID) })
	}
	got := mine()
	if len(got) != 1 || got[0].Action != "ACCEPT" || got[0].Proto != "tcp" || got[0].DPort != "8006" || got[0].Dest != "10.200.200.2" ||
		got[0].Source != "10.200.200.10/32,10.200.200.30/31,10.200.200.32/28,10.200.200.48/31" {
		t.Fatalf("rule after stage 3: %+v", e.pve.clusterRules)
	}
	if it := item(e.stage(StageSeedVM), "pve-api-rule"); it == nil || it.State != "written" {
		t.Fatalf("stage items: %+v", e.stage(StageSeedVM).Items)
	}
	if i := slices.Index(e.pve.writes, "POST /cluster/firewall/rules"); i != 0 {
		t.Fatalf("the rule is not the first write: %v", e.pve.writes)
	}

	// An installed platform: the rule deleted by hand comes back, and a second apply writes nothing.
	for turn, want := range []int{1, 0} {
		e = newEnv(t, RunApply)
		e.only(StageSeedVM)
		e.seedAt(120, 1, nil, true)
		e.mutate(func(_ *Run, _ *Installation, sec *Secrets) {
			sec.SeedKubeconfig = &SeedKubeconfig{Server: "https://x", CA: "c", Token: runSA}
		})
		if turn == 1 {
			e.pve.clusterRules = got
		}
		e.until(5)
		if n := e.pve.writeCount("POST /cluster/firewall/rules"); n != want || len(mine()) != 1 {
			t.Fatalf("apply %d: %d rule writes, rules %+v", turn, n, e.pve.clusterRules)
		}
	}

	e = newEnv(t, RunRemove)
	e.seedAt(120, 1, nil, true)
	e.pve.clusterRules = []pve.FirewallRule{pve.APIAccessRule(inID, []string{"10.200.200.10/32"}, "10.200.200.2", "8006"), ops}
	if end := e.until(5); end != "complete" {
		t.Fatalf("remove: %s", end)
	}
	if len(e.pve.clusterRules) != 1 || e.pve.clusterRules[0].Comment != "ops" {
		t.Fatalf("rules after Remove: %+v", e.pve.clusterRules)
	}
}

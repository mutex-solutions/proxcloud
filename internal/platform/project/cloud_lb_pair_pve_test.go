package project

import (
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
)

func TestLBPairStatus(t *testing.T) {
	up := func(id string, holds bool) lbAppliance {
		return lbAppliance{ServerID: id, State: lbAppUp, HoldsVIP: holds, HAProxyHash: "h", Role: map[bool]string{true: "MASTER", false: "BACKUP"}[holds]}
	}
	down := func(id, reason string) lbAppliance {
		return lbAppliance{ServerID: id, State: lbAppDown, Reason: reason, HAProxyHash: "h"}
	}
	building := lbAppliance{ServerID: "2", State: lbAppBuilding, Reason: "it is installing HAProxy and keepalived"}
	for _, c := range []struct {
		name   string
		apps   []lbAppliance
		stuck  bool
		status string
		says   string
	}{
		{"both answer, one holds", []lbAppliance{up("1", true), up("2", false)}, false, lbStatusActive, ""},
		{"both hold: split", []lbAppliance{up("1", true), up("2", true)}, false, lbStatusDegraded, "Both appliances hold the address"},
		{"a split with one down still shows", []lbAppliance{up("1", true), {ServerID: "2", State: lbAppDown, HoldsVIP: true, HAProxyHash: "h"}}, false, lbStatusDegraded, "Both appliances hold"},
		{"one stopped", []lbAppliance{up("1", true), down("2", "its VM is stopped; starting it")}, false, lbStatusDegraded, "appliance 2: its VM is stopped"},
		{"one gone", []lbAppliance{up("1", true), {ServerID: "2", State: lbAppGone, Reason: "its VM is gone"}}, false, lbStatusDegraded, "Replace appliances"},
		{"electing", []lbAppliance{up("1", false), up("2", false)}, false, lbStatusDegraded, "choosing"},
		{"none answers", []lbAppliance{down("1", "HAProxy is not running"), down("2", "it does not answer")}, false, lbStatusDown, "HAProxy is not running"},
		{"both gone", []lbAppliance{{ServerID: "1", State: lbAppGone}, {ServerID: "2", State: lbAppGone}}, false, lbStatusError, "Both appliances are gone"},
		{"first build", []lbAppliance{{ServerID: "1", State: lbAppBuilding}, {ServerID: "2", State: lbAppBuilding}}, false, lbStatusBuild, ""},
		{"first build past the limit", []lbAppliance{{ServerID: "1", State: lbAppBuilding}, {ServerID: "2", State: lbAppBuilding}}, true, lbStatusError, "keepalived"},
		{"only one appliance in the row (replace)", []lbAppliance{up("1", true)}, false, lbStatusDegraded, "no standby"},
		// Drill N3-6: a paused VM holds nothing (lbApplyPair clears what the row said), and the row names the pause.
		{"one paused", []lbAppliance{up("1", true), down("2", lbReasonPaused)}, false, lbStatusDegraded, "no standby (appliance 2: its VM is paused). A paused VM serves again when it is resumed"},
		// Drill N3-4: the master could not take a change yet and serves the config of before; the standby has it.
		{"the holder is the one behind", []lbAppliance{up("1", false), {ServerID: "2", State: lbAppDown, HoldsVIP: true, HAProxyHash: "old", Reason: "it runs an older configuration"}}, false,
			lbStatusDegraded, "The appliance that holds the address is the one that is not in order (appliance 2: it runs an older configuration); the other one is ready, as its standby"},
	} {
		status, msg := lbPairStatus(c.apps, c.stuck, false)
		if status != c.status || !strings.Contains(msg, c.says) {
			t.Errorf("%s: %s %q", c.name, status, msg)
		}
	}
	// The remedy fits the cause (drill N2-5): Replace appliances is named only when an appliance is missing. A stopped
	// service, a stopped VM and a first build are put right by the platform, and nothing is missing.
	for _, c := range []struct {
		name      string
		apps      []lbAppliance
		replacing bool
		says, not string
	}{
		{"keepalived stopped", []lbAppliance{up("1", true), down("2", "keepalived is stopped")}, false, "appliance 2: keepalived is stopped). The platform puts the other one right", "missing"},
		{"HAProxy stopped", []lbAppliance{up("1", true), down("2", "HAProxy is not running")}, false, "appliance 2: HAProxy is not running). The platform puts", "missing"},
		{"HAProxy runs another config", []lbAppliance{up("1", true), down("2", "HAProxy does not serve its configuration")}, false, "does not serve its configuration", "missing"},
		{"VM stopped", []lbAppliance{up("1", true), down("2", "its VM is stopped; starting it")}, false, "starting it). The platform puts", "missing"},
		{"the second still installs", []lbAppliance{up("1", true), building}, false, "still being prepared", "Replace appliances"},
		{"one gone", []lbAppliance{up("1", true), {ServerID: "2", State: lbAppGone, Reason: "its VM is gone"}}, false, "builds the missing one again", "next pass"},
		{"one appliance in the row", []lbAppliance{up("1", true)}, false, "builds the missing one again", "next pass"},
		// During a replace (drill N2-7: the row read ACTIVE with one appliance listed).
		{"replace took the old one out", []lbAppliance{up("1", true)}, true, "while the other is being replaced.", "Replace appliances"},
		{"replace: the new one boots", []lbAppliance{up("1", true), {ServerID: "2", State: lbAppBuilding, Reason: "it is booting"}}, true, "being replaced (appliance 2: it is booting)", "Replace appliances"},
	} {
		status, msg := lbPairStatus(c.apps, false, c.replacing)
		if status != lbStatusDegraded || !strings.Contains(msg, c.says) || strings.Contains(msg, c.not) {
			t.Errorf("%s: %s %q", c.name, status, msg)
		}
	}
}

// Configs reach the standby first: a config that breaks HAProxy shows there before it can reach the traffic.
func TestLBPushOrder(t *testing.T) {
	a, b := &lbAppliance{ServerID: "1", HoldsVIP: true}, &lbAppliance{ServerID: "2"}
	got := lbPushOrder([]*lbAppliance{a, b})
	if got[0] != b || got[1] != a {
		t.Fatalf("order = %s, %s", got[0].ServerID, got[1].ServerID)
	}
	if got := lbPushOrder([]*lbAppliance{b, a}); got[0] != b {
		t.Fatal("order depends on the input")
	}
}

// A pair asks the quota for two servers of its size and one address, checked before anything is built.
func TestLBPairDemand(t *testing.T) {
	d := lbPairDemand(&pveflavor.Flavor{VCPUs: 1, RAMMb: 1024, DiskGb: 10})
	if d != (pveQuotaDemand{VCPUs: 2, RAMMb: 2048, Servers: 2, DiskGb: 20, FloatingIPs: 1}) {
		t.Fatalf("demand = %+v", d)
	}
}

func TestLBRouterIDInRange(t *testing.T) {
	for _, id := range []string{"000000000000", "ffffffffffff", "a1b2c3d4e5f6", "not-hex"} {
		if n := lbRouterID(id); n < 1 || n > 255 || n != lbRouterID(id) {
			t.Errorf("%s: %d", id, n)
		}
	}
}

// The pair's VIP row, both appliances and their disks are the balancer's: a tenant's direct action on them answers
// 409, as for a single appliance's.
func TestLBOwnerOfPair(t *testing.T) {
	pair := loadBalancer{ID: "abc", Name: "pair", HA: true, ServerID: "301", Appliances: []lbAppliance{{ServerID: "301"}, {ServerID: "302"}}}
	single := loadBalancer{ID: "def", Name: "single", ServerID: "401"}
	tagged := &cloud.CloudResource{Type: cloud.TypeServer, Data: map[string]any{"server": map[string]any{"loadBalancerId": "abc"}}}
	for _, c := range []struct {
		name   string
		server *cloud.CloudResource
		id     string
		owner  string
	}{
		{"the VIP's FLOATING_IP row", nil, "lb:abc", "pair"},
		{"the second appliance, no row yet", nil, "302", "pair"},
		{"an appliance's row", tagged, "302", "pair"},
		{"another balancer's VIP owner on a single balancer", nil, "lb:def", ""},
		{"a tenant's server", nil, "999", ""},
	} {
		got := ""
		if lb := lbOwnerOf(c.server, c.id, []loadBalancer{pair, single}); lb != nil {
			got = lb.Name
		}
		if got != c.owner {
			t.Errorf("%s: got %q", c.name, got)
		}
	}
}

// A replace destroys the VM the single appliance's fields name; they move to an appliance still in the pair, so a
// tenant's next server on that reused vmid is not taken for the balancer's.
func TestLBPairLegacyFieldsFollowAReplace(t *testing.T) {
	lb := &loadBalancer{HA: true, ServerID: "301", PrivateIP: "10.0.0.5", Appliances: []lbAppliance{{ServerID: "302", PrivateIP: "10.0.0.6"}}}
	lbPairLegacyFields(lb)
	if lb.ServerID != "302" || lb.PrivateIP != "10.0.0.6" || lb.owns("301") {
		t.Fatalf("after the old one left: %s %s, owns 301 %v", lb.ServerID, lb.PrivateIP, lb.owns("301"))
	}
	lb.Appliances = append(lb.Appliances, lbAppliance{ServerID: "303", PrivateIP: "10.0.0.7"})
	lbPairLegacyFields(lb)
	if lb.ServerID != "302" {
		t.Fatalf("a field still naming an appliance moved to %s", lb.ServerID)
	}
}

func TestLBReplacing(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for mark, want := range map[string]bool{
		"": false, "garbage": false,
		now.Add(-time.Minute).Format(time.RFC3339):                   true,
		now.Add(-lbReplaceMaxFor - time.Second).Format(time.RFC3339): false, // a portal that died mid-replace
	} {
		if got := lbReplacing(&loadBalancer{Replacing: mark}, now); got != want {
			t.Errorf("%q: %v", mark, got)
		}
	}
}

package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The fence gate reads; it passes only for a worker the pass left fenced and pinned to its Node's address.
func TestCheckSandboxFence(t *testing.T) {
	ctx := context.Background()
	f, s := newSandboxFake(t)
	vm := f.addVM(121, "w1", "proxcloud-sandbox", "virtio=BC:24:11:00:00:01,bridge=pcsbx")
	f.addVM(122, "w2", "proxcloud-sandbox", "virtio=BC:24:11:00:00:02,bridge=pcsbx")
	p, err := s.StartSandboxPass(ctx, testSandbox())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]SandboxWorker{} // the pass lists the pool in the fake's map order
	for _, w := range p.Workers {
		byName[w.Name] = w
	}
	if _, err := p.Secure(ctx, byName["w1"], "10.110.0.11"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Secure(ctx, byName["w2"], ""); err != nil { // fenced, not pinned yet (its Node is still tainted)
		t.Fatal(err)
	}
	before := len(f.writes)
	if err := s.CheckSandboxFence(ctx, testSandbox(), map[string]string{"w1": "10.110.0.11"}); err != nil {
		t.Fatalf("fenced and pinned: %v", err)
	}
	for name, open := range map[string]map[string]string{
		"no worker open":          {},
		"another address":         {"w1": "10.110.0.12"},
		"open but not pinned":     {"w1": "10.110.0.11", "w2": "10.110.0.12"},
		"a Node outside the pool": {"w1": "10.110.0.11", "rogue": "10.110.0.13"},
	} {
		if err := s.CheckSandboxFence(ctx, testSandbox(), open); err == nil {
			t.Errorf("%s: the gate passed", name)
		}
	}
	vm.opts["ipfilter"] = "0" // a hand edit
	if err := s.CheckSandboxFence(ctx, testSandbox(), map[string]string{"w1": "10.110.0.11"}); err == nil || !strings.Contains(err.Error(), "does not carry the current firewall") {
		t.Errorf("ipfilter off by hand: %v", err)
	}
	vm.opts["ipfilter"] = "1"
	f.groups[p.Group()] = f.groups[p.Group()][1:] // a rule deleted from the group
	if err := s.CheckSandboxFence(ctx, testSandbox(), map[string]string{"w1": "10.110.0.11"}); !errors.Is(err, ErrSandboxDrift) {
		t.Errorf("group changed by hand: %v", err)
	}
	f.dcOff = true
	if err := s.CheckSandboxFence(ctx, testSandbox(), map[string]string{"w1": "10.110.0.11"}); !errors.Is(err, ErrSandboxUnenforced) {
		t.Errorf("datacenter firewall off: %v", err)
	}
	if len(f.writes) != before {
		t.Fatalf("the gate wrote to PVE: %v", f.writes[before:])
	}
}

// The isolation gate passes on the rules ReconcileIsolation wrote for the sandbox network, and on nothing less.
func TestCheckSandboxIsolation(t *testing.T) {
	ctx := context.Background()
	dc := 1
	opts := map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/firewall/options": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"data":{"enable":%d}}`, dc)
		},
	}
	sbx := []string{"10.110.0.0/24"}
	srv, rules, writes := isolationFakeNodes(t, nil, opts)
	if err := srv.CheckSandboxIsolation(ctx, "10.100.0.0/16", nil, sbx); err == nil {
		t.Fatal("no rules in place: the gate passed")
	}
	if err := srv.ReconcileIsolation(ctx, "p1", "10.100.0.0/16", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := srv.CheckSandboxIsolation(ctx, "10.100.0.0/16", nil, sbx); err == nil {
		t.Fatal("rules without the sandbox network: the gate passed")
	}
	if err := srv.ReconcileIsolation(ctx, "p1", "10.100.0.0/16", nil, sbx); err != nil {
		t.Fatal(err)
	}
	for i := range *rules {
		(*rules)[i].Enable = 1 // PVE reads back what ruleForm sends
	}
	n := *writes
	if err := srv.CheckSandboxIsolation(ctx, "10.100.0.0/16", nil, sbx); err != nil {
		t.Fatalf("rules in place: %v", err)
	}
	if err := srv.CheckSandboxIsolation(ctx, "10.100.0.0/16", nil, nil); err == nil {
		t.Fatal("no sandboxCidrs: the gate passed")
	}
	if *writes != n {
		t.Fatal("the gate wrote to PVE")
	}
	(*rules)[len(*rules)-1].Enable = 0
	if err := srv.CheckSandboxIsolation(ctx, "10.100.0.0/16", nil, sbx); err == nil {
		t.Fatal("a disabled DROP: the gate passed")
	}
	(*rules)[len(*rules)-1].Enable = 1
	dc = 0
	if err := srv.CheckSandboxIsolation(ctx, "10.100.0.0/16", nil, sbx); err == nil {
		t.Fatal("datacenter firewall off: the gate passed")
	}
}

// The managed-services check: with no rule of this portal and no tenant network it is pending, not passed, with or
// without sandbox networks; with tenant networks and no rules it fails; once ReconcileIsolation wrote them it passes.
// It never writes.
func TestCheckIsolation(t *testing.T) {
	ctx := context.Background()
	vnets := `{"data":[{"vnet":"vaaa11","zone":"overlay","alias":"st:p1:web"}]}`
	opts := map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/firewall/options": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":{"enable":1}}`) },
		"GET /api2/json/cluster/sdn/vnets":        func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, vnets) },
		"GET /api2/json/cluster/sdn/vnets/vaaa11/subnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"cidr":"10.100.1.0/24"}]}`)
		},
	}
	srv, rules, writes := isolationFakeNodes(t, nil, opts)
	vnets = `{"data":[{"vnet":"pcplat","zone":"overlay","alias":"not a tenant network"}]}`
	if err := srv.CheckIsolation(ctx, "10.100.0.0/16", nil, nil); !errors.Is(err, ErrIsolationPending) {
		t.Fatalf("no tenant network yet: %v", err)
	}
	// A pool over a management network is not pending: the sync refuses to write its rules.
	if err := srv.CheckIsolation(ctx, "192.0.0.0/16", nil, nil); err == nil || !strings.Contains(err.Error(), "overlaps the management network 192.0.2.0/24") {
		t.Fatalf("cidrPool over a management network, no tenant network: %v", err)
	}
	// With sandbox networks too: the sandbox gate, which refuses every sandbox create until the rules are in, keeps
	// failing.
	sbx := []string{"10.110.0.0/24"}
	if err := srv.CheckIsolation(ctx, "10.100.0.0/16", nil, sbx); !errors.Is(err, ErrIsolationPending) {
		t.Fatalf("no tenant network yet, sandboxCidrs set: %v", err)
	}
	if err := srv.CheckSandboxIsolation(ctx, "10.100.0.0/16", nil, sbx); err == nil || errors.Is(err, ErrIsolationPending) {
		t.Fatalf("the sandbox gate with no rules: %v", err)
	}
	vnets = `{"data":[{"vnet":"vaaa11","zone":"overlay","alias":"st:p1:web"}]}`
	if err := srv.CheckIsolation(ctx, "10.100.0.0/16", nil, nil); err == nil || errors.Is(err, ErrIsolationPending) {
		t.Fatalf("a tenant network without rules: %v", err)
	}
	if *writes != 0 {
		t.Fatal("the check wrote to PVE")
	}
	if err := srv.ReconcileIsolation(ctx, "p1", "10.100.0.0/16", nil, nil); err != nil {
		t.Fatal(err)
	}
	for i := range *rules {
		(*rules)[i].Enable = 1
	}
	if err := srv.CheckIsolation(ctx, "10.100.0.0/16", nil, nil); err != nil {
		t.Fatalf("rules in place: %v", err)
	}
	if err := srv.CheckIsolation(ctx, "10.100.0.0/16", []string{"10.200.200.0/24"}, nil); err == nil {
		t.Fatal("a blocked network the rules do not drop: the check passed")
	}
}

// A cluster two portals share (lab M0): the other portal's tenant networks end the pending case, so a portal with no
// project fails the check until the provider-level reconcile, which needs no project, has written its rules. It
// writes this portal's DROPs only: no project ACCEPT, and nothing of the other portal changes.
func TestProviderIsolationOnASharedCluster(t *testing.T) {
	ctx := context.Background()
	opts := map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/firewall/options": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":{"enable":1}}`) },
		"GET /api2/json/cluster/sdn/vnets": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"vnet":"vother1","zone":"pc8","alias":"st:other:web"}]}`)
		},
	}
	other := []FirewallRule{
		{Type: "forward", Action: "ACCEPT", Source: "10.108.1.0/24", Dest: "10.108.1.0/24", Enable: 1, Comment: "proxcloud:isolation:other"},
		{Type: "forward", Action: "DROP", Source: "10.108.0.0/16", Dest: "10.108.0.0/16", Enable: 1, Comment: "proxcloud:isolation/pc8-"},
	}
	srv, rules, writes := isolationFakeNodes(t, slices.Clone(other), opts)
	srv.PoolPrefix = "pcm-"
	blocked := []string{"10.90.0.0/26"}
	err := srv.CheckIsolation(ctx, "10.121.0.0/16", blocked, nil)
	if err == nil || errors.Is(err, ErrIsolationPending) || !strings.Contains(err.Error(), "a provider sync writes them") {
		t.Fatalf("another portal's tenant network, none of this portal's rules: %v", err)
	}
	// A pool over a management network: nothing is written, and the check names that cause, not a sync.
	if err := srv.ReconcileProviderIsolation(ctx, "192.0.0.0/16", blocked, nil); err == nil || *writes != 0 {
		t.Fatalf("cidrPool over a management network: err %v, %d writes", err, *writes)
	}
	if err := srv.CheckIsolation(ctx, "192.0.0.0/16", blocked, nil); err == nil || !strings.Contains(err.Error(), "the provider sync writes no rule") {
		t.Fatalf("cidrPool over a management network: %v", err)
	}
	if err := srv.ReconcileProviderIsolation(ctx, "10.121.0.0/16", blocked, nil); err != nil {
		t.Fatal(err)
	}
	if err := srv.CheckIsolation(ctx, "10.121.0.0/16", blocked, nil); err != nil {
		t.Fatalf("rules written with no project: %v", err)
	}
	var mine int
	for _, r := range *rules {
		switch r.Comment {
		case "proxcloud:isolation/pcm-":
			mine++
		case "proxcloud:isolation:other", "proxcloud:isolation/pc8-":
		default:
			t.Errorf("unexpected rule %+v", r)
		}
	}
	if mine != 4 || len(*rules) != 6 || !slices.EqualFunc((*rules)[4:], other, sameLiveRule) {
		t.Fatalf("want 4 pcm- DROPs above the other portal's 2 untouched rules, got %+v", *rules)
	}
	// Idempotent, and a rule another owner holds never satisfies the check.
	n := *writes
	if err := srv.ReconcileProviderIsolation(ctx, "10.121.0.0/16", blocked, nil); err != nil || *writes != n {
		t.Fatalf("second run: err %v, %d more writes", err, *writes-n)
	}
	for i := range *rules {
		if (*rules)[i].Comment == "proxcloud:isolation/pcm-" {
			(*rules)[i].Comment = "proxcloud:isolation/pc8-"
		}
	}
	if err := srv.CheckIsolation(ctx, "10.121.0.0/16", blocked, nil); err == nil || errors.Is(err, ErrIsolationPending) {
		t.Fatalf("the same DROPs under another portal's comment: %v", err)
	}
}

// Two portals with the same pool prefix (pc- is the default) share one rule owner. The provider-level reconcile of the
// one with no project must not replace the other's DROPs with its own: it writes nothing and says why, and so does its
// check.
func TestProviderIsolationKeepsAnotherPoolsRules(t *testing.T) {
	ctx := context.Background()
	a, rules, writes := isolationFakeNodes(t, nil, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/firewall/options": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":{"enable":1}}`) },
	})
	if err := a.ReconcileIsolation(ctx, "p1", "10.100.0.0/16", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckIsolation(ctx, "10.100.0.0/16", nil, nil); err != nil {
		t.Fatal(err)
	}
	before, n := slices.Clone(*rules), *writes
	b := &Service{C: a.C, ServiceID: "svc2", PoolPrefix: "pc-"}
	err := b.ReconcileProviderIsolation(ctx, "10.121.0.0/16", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "another pool (10.100.0.0/16)") || !strings.Contains(err.Error(), "poolPrefix") {
		t.Fatalf("rules of another pool under the same owner: %v", err)
	}
	if *writes != n || !slices.EqualFunc(*rules, before, sameLiveRule) {
		t.Fatalf("the other portal's rules changed (%d writes): %+v", *writes-n, *rules)
	}
	if err := a.CheckIsolation(ctx, "10.100.0.0/16", nil, nil); err != nil {
		t.Fatalf("the first portal after the second one's sync: %v", err)
	}
	if err := b.CheckIsolation(ctx, "10.121.0.0/16", nil, nil); err == nil || !strings.Contains(err.Error(), "poolPrefix") || strings.Contains(err.Error(), "a provider sync writes them") {
		t.Fatalf("the second portal's check: %v", err)
	}
	if !errors.Is(err, ErrIsolationRefused) {
		t.Errorf("a refusal the sync logs as a warning: %v", err)
	}
}

// The same owner and the same pool, but the rules in place fence more than the provider with no project would: a
// blocked network and a sandbox network of the other one. Its sync removes no DROP, so the other's gate keeps passing
// (a wider set: TestProviderIsolationLeavesASetAProjectMaintains, TestProviderIsolationWidensItsOwnSet).
func TestProviderIsolationRemovesNoDrop(t *testing.T) {
	ctx := context.Background()
	a, rules, writes := isolationFakeNodes(t, nil, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/firewall/options": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":{"enable":1}}`) },
	})
	blocked, sbx := []string{"10.90.0.0/26"}, []string{"10.110.0.0/24"}
	if err := a.ReconcileIsolation(ctx, "p1", "10.100.0.0/16", blocked, sbx); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckSandboxIsolation(ctx, "10.100.0.0/16", blocked, sbx); err != nil {
		t.Fatal(err)
	}
	before, n := slices.Clone(*rules), *writes
	b := &Service{C: a.C, ServiceID: "svc2", PoolPrefix: "pc-"}
	for range 2 {
		err := b.ReconcileProviderIsolation(ctx, "10.100.0.0/16", nil, nil)
		if !errors.Is(err, ErrIsolationRefused) || !strings.Contains(err.Error(), "removes no DROP in place") || !strings.Contains(err.Error(), "poolPrefix") {
			t.Fatalf("a set that fences less than the one in place: %v", err)
		}
		if err := a.ReconcileProviderIsolation(ctx, "10.100.0.0/16", blocked, sbx); err != nil {
			t.Fatal(err)
		}
	}
	if *writes != n || !slices.EqualFunc(*rules, before, sameLiveRule) {
		t.Fatalf("the rules changed (%d writes): %+v", *writes-n, *rules)
	}
	if err := a.CheckSandboxIsolation(ctx, "10.100.0.0/16", blocked, sbx); err != nil {
		t.Fatalf("the first portal's gate after the second one's sync: %v", err)
	}
	err := b.CheckIsolation(ctx, "10.100.0.0/16", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "removes no DROP in place") || strings.Contains(err.Error(), "a provider sync writes them") {
		t.Fatalf("the second portal's check: %v", err)
	}
	// Only the blocked network kept, the sandbox fence not: still refused.
	if err := b.ReconcileProviderIsolation(ctx, "10.100.0.0/16", blocked, nil); !errors.Is(err, ErrIsolationRefused) || *writes != n {
		t.Fatalf("a set without the sandbox fence: err %v, %d writes", err, *writes-n)
	}
}

// A provider with no project whose cidrPool changed after its first sync: the sync leaves its old rules, and the sync
// and the check both name the repair that works without a project.
func TestProviderIsolationAfterAPoolChange(t *testing.T) {
	ctx := context.Background()
	a, _, writes := isolationFakeNodes(t, nil, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/firewall/options": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":{"enable":1}}`) },
	})
	if err := a.ReconcileProviderIsolation(ctx, "10.100.0.0/16", nil, nil); err != nil {
		t.Fatal(err)
	}
	n := *writes
	const repair = "with no project, delete the proxcloud:isolation rules under Datacenter > Firewall and run a provider sync"
	if err := a.ReconcileProviderIsolation(ctx, "10.121.0.0/16", nil, nil); err == nil || !strings.Contains(err.Error(), repair) || *writes != n {
		t.Fatalf("sync after a pool change: err %v, %d writes", err, *writes-n)
	}
	if err := a.CheckIsolation(ctx, "10.121.0.0/16", nil, nil); err == nil || !strings.Contains(err.Error(), repair) {
		t.Fatalf("check after a pool change: %v", err)
	}
}

// A node that does not answer may be the one whose management network the pool overlaps, and the DROP to it would be
// missing: the provider-level call, which no tenant waits for, writes nothing until every node has reported. A
// project's first network is still not held up.
func TestProviderIsolationNeedsEveryNode(t *testing.T) {
	ctx := context.Background()
	srv, rules, writes := isolationFakeNodes(t, nil, nil, `{"type":"node","name":"pve2","ip":"10.100.0.12"}`)
	err := srv.ReconcileProviderIsolation(ctx, "10.100.0.0/16", nil, nil)
	if !errors.Is(err, ErrIsolationRefused) || !strings.Contains(err.Error(), "did not report its networks") || *writes != 0 {
		t.Fatalf("a node that did not report: err %v, %d writes", err, *writes)
	}
	if err := srv.ReconcileIsolation(ctx, "p1", "10.100.0.0/16", nil, nil); err != nil || len(*rules) == 0 {
		t.Fatalf("a project's first network with a node down: err %v, rules %+v", err, *rules)
	}
}

// Rules a project wrote for a pool over a management network, the project gone, the blocked list changed: the provider
// sync refuses that pool, so the check must not say that a provider sync writes them.
func TestCheckIsolationPoolOverManagementWithRulesInPlace(t *testing.T) {
	ctx := context.Background()
	srv, _, _ := isolationFakeNodes(t, nil, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/firewall/options": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":{"enable":1}}`) },
	})
	if err := srv.ReconcileIsolation(ctx, "p1", "192.0.0.0/16", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := srv.CheckIsolation(ctx, "192.0.0.0/16", nil, nil); err != nil {
		t.Fatalf("the rules the project wrote: %v", err)
	}
	err := srv.CheckIsolation(ctx, "192.0.0.0/16", []string{"10.90.0.0/26"}, nil)
	if err == nil || !strings.Contains(err.Error(), "overlaps the management network 192.0.2.0/24") || strings.Contains(err.Error(), "a provider sync writes them") {
		t.Fatalf("a changed blocked list on a pool over a management network: %v", err)
	}
}

// Same owner, same pool, and the provider with no project wants a wider set than the one a project's sync of the
// other maintains (the managed-services preflight makes it add the seed, the API VIP and the node range). Writing it
// would start a rewrite in turn on every cycle, with the other's sandbox gate failing in between and its project
// ACCEPTs deleted and re-added twice a cycle: with a project's rules for the pool in place it writes nothing.
func TestProviderIsolationLeavesASetAProjectMaintains(t *testing.T) {
	ctx := context.Background()
	a, rules, writes := isolationFakeNodes(t, nil, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/firewall/options": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":{"enable":1}}`) },
	})
	blocked, wider, sbx := []string{"10.90.0.0/26"}, []string{"10.90.0.0/26", "10.95.0.0/24"}, []string{"10.110.0.0/24"}
	if err := a.ReconcileIsolation(ctx, "p1", "10.100.0.0/16", blocked, sbx); err != nil {
		t.Fatal(err)
	}
	before, n := slices.Clone(*rules), *writes
	b := &Service{C: a.C, ServiceID: "svc2", PoolPrefix: "pc-"}
	for cycle := range 3 {
		err := b.ReconcileProviderIsolation(ctx, "10.100.0.0/16", wider, sbx)
		if !errors.Is(err, ErrIsolationRefused) || !strings.Contains(err.Error(), "poolPrefix") {
			t.Fatalf("cycle %d: a wider set over one a project maintains: %v", cycle, err)
		}
		if err := a.CheckSandboxIsolation(ctx, "10.100.0.0/16", blocked, sbx); err != nil {
			t.Fatalf("cycle %d: the other's sandbox gate: %v", cycle, err)
		}
		if err := a.ReconcileIsolation(ctx, "p1", "10.100.0.0/16", blocked, sbx); err != nil {
			t.Fatal(err)
		}
		if *writes != n || !slices.EqualFunc(*rules, before, sameLiveRule) {
			t.Fatalf("cycle %d: the rules changed (%d writes): %+v", cycle, *writes-n, *rules)
		}
		cerr := b.CheckIsolation(ctx, "10.100.0.0/16", wider, sbx)
		if cerr == nil || cerr.Error() != err.Error() {
			t.Fatalf("cycle %d: the check must give the sync's sentence %q, got %v", cycle, err, cerr)
		}
	}
	// A missing rule is not put back either while a project's sync maintains the set: that sync does it.
	*rules = slices.Clone(before[:len(before)-1])
	if err := b.ReconcileProviderIsolation(ctx, "10.100.0.0/16", blocked, sbx); !errors.Is(err, ErrIsolationRefused) || *writes != n {
		t.Fatalf("a repair of a set a project maintains: err %v, %d writes", err, *writes-n)
	}
}

// With no project's rules for the pool in place, a provider with no project still widens and repairs its own set.
func TestProviderIsolationWidensItsOwnSet(t *testing.T) {
	ctx := context.Background()
	a, rules, writes := isolationFakeNodes(t, nil, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/firewall/options": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":{"enable":1}}`) },
	})
	sbx := []string{"10.110.0.0/24"}
	// One more blocked network, then one that covers the range in place: every DROP in place is kept, so it is written.
	for _, blocked := range [][]string{{"10.90.0.0/26"}, {"10.90.0.0/26", "10.91.0.0/24"}, {"10.90.0.0/24", "10.91.0.0/24"}} {
		n := *writes
		if err := a.ReconcileProviderIsolation(ctx, "10.100.0.0/16", blocked, sbx); err != nil || *writes == n {
			t.Fatalf("a set that fences more (%v): err %v, %d writes", blocked, err, *writes-n)
		}
		if err := a.CheckSandboxIsolation(ctx, "10.100.0.0/16", blocked, sbx); err != nil {
			t.Fatalf("blocked %v: %v", blocked, err)
		}
	}
	(*rules)[len(*rules)-1].Enable = 0
	*rules = (*rules)[1:]
	if err := a.ReconcileProviderIsolation(ctx, "10.100.0.0/16", []string{"10.90.0.0/24", "10.91.0.0/24"}, sbx); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckSandboxIsolation(ctx, "10.100.0.0/16", []string{"10.90.0.0/24", "10.91.0.0/24"}, sbx); err != nil {
		t.Fatalf("after a rule was deleted and one disabled by hand: %v", err)
	}
}

// The sandbox endpoint ACCEPTs carry the owner comment too and sit above the sandbox DROPs. A provider with no project
// that shares the owner replaces none: not the other's endpoint with nothing (its build pods would lose the build
// bucket), and not with its own (the other's sandbox network would reach an address its set dropped).
func TestProviderIsolationReplacesNoAccept(t *testing.T) {
	ctx := context.Background()
	a, rules, writes := isolationFakeNodes(t, nil, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/firewall/options": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":{"enable":1}}`) },
	})
	blocked, sbx := []string{"10.90.0.0/26"}, []string{"10.110.0.0/24"}
	if err := a.ReconcileProviderIsolation(ctx, "10.100.0.0/16", blocked, sbx, "10.88.0.41:9000"); err != nil {
		t.Fatal(err)
	}
	before, n := slices.Clone(*rules), *writes
	b := &Service{C: a.C, ServiceID: "svc2", PoolPrefix: "pc-"}
	for _, allow := range [][]string{nil, {"192.0.2.11:8006"}, {"10.88.0.41:9000", "192.0.2.11:8006"}} {
		err := b.ReconcileProviderIsolation(ctx, "10.100.0.0/16", blocked, sbx, allow...)
		if !errors.Is(err, ErrIsolationRefused) || !strings.Contains(err.Error(), "ACCEPT") || !strings.Contains(err.Error(), "poolPrefix") {
			t.Fatalf("endpoints %v over 10.88.0.41:9000 in place: %v", allow, err)
		}
		if *writes != n || !slices.EqualFunc(*rules, before, sameLiveRule) {
			t.Fatalf("endpoints %v: the rules changed (%d writes): %+v", allow, *writes-n, *rules)
		}
		if cerr := b.CheckIsolation(ctx, "10.100.0.0/16", blocked, sbx, allow...); cerr == nil || cerr.Error() != err.Error() {
			t.Fatalf("endpoints %v: the check must give the sync's sentence %q, got %v", allow, err, cerr)
		}
	}
	// The set in place is still its writer's own.
	if err := a.ReconcileProviderIsolation(ctx, "10.100.0.0/16", blocked, sbx, "10.88.0.41:9000"); err != nil || *writes != n {
		t.Fatalf("the writer's own set: err %v, %d writes", err, *writes-n)
	}
}

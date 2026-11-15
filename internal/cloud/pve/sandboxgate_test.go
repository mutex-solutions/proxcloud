package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	// With sandbox networks too: the sync writes nothing before a tenant network, and the sandbox gate, which refuses
	// every sandbox create until the rules are in, keeps failing.
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

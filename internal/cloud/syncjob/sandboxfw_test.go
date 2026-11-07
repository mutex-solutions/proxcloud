package syncjob

import (
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

func sandboxNode(addr string, created time.Time, taints ...string) map[string]any {
	var ts []any
	for _, k := range taints {
		ts = append(ts, map[string]any{"key": k, "effect": "NoSchedule"})
	}
	return map[string]any{
		"metadata": map[string]any{"name": "w1", "creationTimestamp": created.Format(time.RFC3339), "resourceVersion": "7"},
		"spec":     map[string]any{"taints": ts},
		"status":   map[string]any{"addresses": []any{map[string]any{"type": "Hostname", "address": "w1"}, map[string]any{"type": "InternalIP", "address": addr}}},
	}
}

// The taint is the only thing keeping tenant pods off a worker, so every condition must hold before it comes off.
func TestClearForTenants(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	old, young := now.Add(-16*time.Minute), now.Add(-14*time.Minute)
	fenced := pve.SandboxWorkerState{Pinned: "10.110.0.11", Secured: true}
	cases := []struct {
		name  string
		st    pve.SandboxWorkerState
		node  map[string]any
		ok    bool
		alarm bool
	}{
		{"fenced, pinned, old enough", fenced, sandboxNode("10.110.0.11", old, pve.SandboxUnsecuredTaint), true, false},
		{"younger than the join token", fenced, sandboxNode("10.110.0.11", young, pve.SandboxUnsecuredTaint), false, false},
		{"firewall not read back in place", pve.SandboxWorkerState{Pinned: "10.110.0.11"}, sandboxNode("10.110.0.11", old, pve.SandboxUnsecuredTaint), false, false},
		{"filter still admits the whole range", pve.SandboxWorkerState{Secured: true}, sandboxNode("10.110.0.11", old, pve.SandboxUnsecuredTaint), false, true},
		{"Node claims another address", fenced, sandboxNode("10.110.0.12", old, pve.SandboxUnsecuredTaint), false, true},
		{"no taint to take off", fenced, sandboxNode("10.110.0.11", old), false, false},
		{"no Node", fenced, nil, false, false},
	}
	for _, c := range cases {
		ok, why := clearForTenants(c.st, c.node, now, 15*time.Minute)
		if ok != c.ok || (why != "") != c.alarm {
			t.Errorf("%s: ok=%v why=%q", c.name, ok, why)
		}
	}
}

func TestWithoutTaintKeepsTheOthers(t *testing.T) {
	n := sandboxNode("10.110.0.11", time.Now(), "node.cilium.io/agent-not-ready", pve.SandboxUnsecuredTaint)
	got := withoutTaint(n, pve.SandboxUnsecuredTaint)
	if len(got) != 1 || got[0].(map[string]any)["key"] != "node.cilium.io/agent-not-ready" {
		t.Fatalf("got %v", got)
	}
	if got := withoutTaint(sandboxNode("x", time.Now(), pve.SandboxUnsecuredTaint), pve.SandboxUnsecuredTaint); got == nil || len(got) != 0 {
		t.Fatalf("an emptied list must be [] (a merge patch null would mean something else): %v", got)
	}
}

func TestAPIEndpoint(t *testing.T) {
	if h, p, err := apiEndpoint("https://10.20.0.214:6443"); err != nil || h != "10.20.0.214" || p != 6443 {
		t.Fatalf("got %s %d %v", h, p, err)
	}
	if _, p, _ := apiEndpoint("https://192.0.2.5"); p != 443 {
		t.Fatalf("default port %d", p)
	}
	if _, _, err := apiEndpoint("https://cp.example.org:6443"); err == nil {
		t.Fatal("a name cannot go in a firewall rule")
	}
}

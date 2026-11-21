package pve

import (
	"slices"
	"strings"
	"testing"
)

// A cluster's balancer: each listener binds its own Service's address, two Services share port 80, workers are
// members by address, policy Local is checked over HTTP on the health port, PROXY v2 goes to the members only, and
// connections may stay idle for an hour (C5).
func TestRenderClusterHAProxy(t *testing.T) {
	nodes := func(port int) []LBMember {
		return []LBMember{{Node: true, Address: "10.0.0.250", Port: port}, {Node: true, Address: "10.0.0.249", Port: port},
			{Node: true, Address: "not-an-address", Port: port}}
	}
	s := LBSpec{
		Listeners: []LBListener{
			{Port: 80, Pool: "s0-0", VIP: "203.0.113.10"},
			{Port: 443, Pool: "s0-1", VIP: "203.0.113.10", SourceRanges: []string{"198.51.100.0/24"}},
			{Port: 80, Pool: "s1-0", VIP: "203.0.113.11"},
			{Port: 81, Pool: "s1-0", VIP: "2001:db8::1"},
		},
		Pools: []LBPool{
			{Name: "s0-0", Protocol: "tcp", Algorithm: "roundrobin", Members: nodes(30080)},
			{Name: "s0-1", Protocol: "tcp", Algorithm: "roundrobin", Members: nodes(30443)},
			{Name: "s1-0", Protocol: "tcp", Algorithm: "roundrobin", Members: nodes(31080), ProxyProtocol: true,
				HealthCheck: &LBHealthCheck{Type: "http", Port: 32000, Path: "/"}},
		},
	}
	vips := []string{"203.0.113.10", "203.0.113.11"}
	got := RenderClusterHAProxy(s, vips)
	golden(t, "lb-cluster.cfg", got)
	if !strings.Contains(got, "timeout client 1h") || strings.Contains(RenderHAProxyOn(s, vips), "timeout client 1h") {
		t.Fatal("only a cluster's balancer gets the hour")
	}

	rules := LBListenerRules(s.Listeners, vips)
	var keys []string
	for _, r := range rules {
		keys = append(keys, r.Dest+" "+r.Source+" "+r.DPort)
	}
	// The source list of one Service's port never opens the other Service's address, and an address the balancer
	// does not hold opens nothing.
	if want := []string{"203.0.113.10  80", "203.0.113.10 198.51.100.0/24 443", "203.0.113.11  80"}; !slices.Equal(keys, want) {
		t.Fatalf("rules %q, want %q", keys, want)
	}
}

// A tenant cannot set what only the platform sets: a listener's own address, a member named by address.
func TestValidateClearsPlatformFields(t *testing.T) {
	s := LBSpec{
		Listeners: []LBListener{{Port: 80, Pool: "web", VIP: "198.51.100.1"}},
		Pools:     []LBPool{{Name: "web", Members: []LBMember{{ServerID: "101", Port: 80, Node: true, Address: "10.9.9.9"}}}},
	}
	if msg := s.Validate(); msg != "" {
		t.Fatal(msg)
	}
	if m := s.Pools[0].Members[0]; s.Listeners[0].VIP != "" || m.Node || m.Address != "" {
		t.Fatalf("after Validate: %+v %+v", s.Listeners[0], m)
	}
	noServer := LBSpec{Pools: []LBPool{{Name: "web", Members: []LBMember{{Node: true, Address: "10.9.9.9", Port: 80}}}}}
	if msg := noServer.Validate(); msg == "" {
		t.Fatal("a member without a server passed")
	}
}

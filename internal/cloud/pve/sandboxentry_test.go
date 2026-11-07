package pve

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The entry adds one rule, appliance to proxy only; a provider without an entry keeps its generation (no roll).
func TestSandboxEntryRule(t *testing.T) {
	base, err := testSandbox().generation()
	if err != nil {
		t.Fatal(err)
	}
	f := testSandbox()
	f.ProxyAddress, f.EntryFrom = "10.110.0.200", "10.110.0.5"
	g, err := f.generation()
	if err != nil {
		t.Fatal(err)
	}
	if g.Group == base.Group || len(g.Rules) != len(base.Rules)+1 {
		t.Fatalf("entry rule missing: %d rules vs %d", len(g.Rules), len(base.Rules))
	}
	var entry []FirewallRule
	for _, r := range g.Rules {
		if r.Type == "in" && r.Source == "10.110.0.5" {
			entry = append(entry, r)
		}
	}
	if len(entry) != 1 || entry[0].Dest != "10.110.0.200" || entry[0].DPort != "443" || entry[0].Proto != "tcp" {
		t.Fatalf("entry rule = %+v", entry)
	}
	if again, _ := testSandbox().generation(); again.Group != base.Group {
		t.Fatal("no entry must keep the generation of the workers")
	}
	for name, bad := range map[string][2]string{
		"proxy off the load balancers": {"10.110.0.50", "10.110.0.5"},
		"entry is a worker":            {"10.110.0.200", "10.110.0.12"},
		"entry is a load balancer":     {"10.110.0.200", "10.110.0.201"},
		"entry outside the subnet":     {"10.110.0.200", "10.88.0.5"},
		"entry missing":                {"10.110.0.200", ""},
	} {
		f := testSandbox()
		f.ProxyAddress, f.EntryFrom = bad[0], bad[1]
		if _, err := f.generation(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSandboxEntryConfig(t *testing.T) {
	c := SandboxEntryConfig("10.110.0.200", 443)
	for _, want := range []string{"bind :443\n  mode tcp", "server m0p443 10.110.0.200:443 check"} {
		if !strings.Contains(c, want) {
			t.Fatalf("config lacks %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "forwardfor") {
		t.Fatal("TLS ends in sandbox-proxy: the entry must pass TCP through")
	}
	// A tenant's WebSocket or SSE stream may stay quiet for minutes: the P8-1 default of 60 s would cut it.
	if !strings.Contains(c, "timeout client 1h\n  timeout server 1h\n") || strings.Contains(c, "60s") {
		t.Fatalf("the entry keeps idle streams open for an hour:\n%s", c)
	}
}

// A VM that is not the appliance (a recycled vmid, a tenant's VM that its owner tagged) is never written to.
func TestSandboxEntryRefusesOtherVMs(t *testing.T) {
	writes := 0
	for name, row := range map[string]string{
		"untagged":         `{"id":"qemu/9110","type":"qemu","vmid":9110,"node":"pve1","status":"running","pool":"proxcloud-sandbox-entry","tags":"proxcloud"}`,
		"tagged by tenant": `{"id":"qemu/9110","type":"qemu","vmid":9110,"node":"pve1","status":"running","pool":"tp-p1","tags":"proxcloud-sandbox-entry"}`,
		"tagged, no pool":  `{"id":"qemu/9110","type":"qemu","vmid":9110,"node":"pve1","status":"running","tags":"proxcloud-sandbox-entry"}`,
		"in sandbox pool":  `{"id":"qemu/9110","type":"qemu","vmid":9110,"node":"pve1","status":"running","pool":"proxcloud-sandbox","tags":"proxcloud-sandbox-entry"}`,
		"a template":       `{"id":"qemu/9110","type":"qemu","vmid":9110,"node":"pve1","status":"stopped","template":1,"pool":"proxcloud-sandbox-entry","tags":"proxcloud-sandbox-entry"}`,
		"gone":             ``,
	} {
		_, c := fakePVE(t, fakeRoutes{
			"GET /api2/json/cluster/resources": say(`{"data":[` + row + `]}`),
			"PUT /api2/json/nodes/pve1/qemu/9110/firewall/options": func(w http.ResponseWriter, _ *http.Request) {
				writes++
				say(`{"data":null}`)(w, nil)
			},
		})
		if err := (&Service{C: c}).SandboxEntry(context.Background(), 9110, "proxcloud-sandbox-entry", "proxcloud-sandbox", "10.110.0.200", 443); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// No pool configured, or the sandbox pool named as the appliance's: refused before anything is read.
	for _, pool := range []string{"", "proxcloud-sandbox"} {
		_, c := fakePVE(t, fakeRoutes{})
		if err := (&Service{C: c}).SandboxEntry(context.Background(), 9110, pool, "proxcloud-sandbox", "10.110.0.200", 443); err == nil {
			t.Errorf("entry pool %q: accepted", pool)
		}
	}
	if writes != 0 {
		t.Fatalf("a refused VM was written to %d times", writes)
	}
}

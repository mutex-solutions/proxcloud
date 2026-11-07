package pve

import (
	"strings"
	"testing"
)

func TestLBSpecValidate(t *testing.T) {
	good := func() LBSpec {
		return LBSpec{
			Listeners: []LBListener{{Port: 80, Pool: "web"}},
			Pools:     []LBPool{{Name: "web", Members: []LBMember{{ServerID: "101", Port: 8080}}}},
		}
	}
	s := good()
	if msg := s.Validate(); msg != "" {
		t.Fatal(msg)
	}
	if s.Pools[0].Protocol != "http" || s.Pools[0].Algorithm != "roundrobin" || s.Pools[0].Members[0].Weight != 1 {
		t.Fatalf("defaults not filled: %+v", s.Pools[0])
	}
	bad := map[string]func(*LBSpec){
		"newline in pool name": func(s *LBSpec) { s.Pools[0].Name = "web\n  bind :22"; s.Listeners[0].Pool = s.Pools[0].Name },
		"space in pool name":   func(s *LBSpec) { s.Pools[0].Name = "a b" },
		"protocol":             func(s *LBSpec) { s.Pools[0].Protocol = "http\nglobal" },
		"algorithm":            func(s *LBSpec) { s.Pools[0].Algorithm = "first" },
		"port":                 func(s *LBSpec) { s.Listeners[0].Port = 70000 },
		"dangling pool":        func(s *LBSpec) { s.Listeners[0].Pool = "other" },
		"member id":            func(s *LBSpec) { s.Pools[0].Members[0].ServerID = "101 check" },
		"member port":          func(s *LBSpec) { s.Pools[0].Members[0].Port = 0 },
		"weight":               func(s *LBSpec) { s.Pools[0].Members[0].Weight = 300 },
		"twice":                func(s *LBSpec) { s.Pools[0].Members = append(s.Pools[0].Members, s.Pools[0].Members[0]) },
	}
	for name, breakIt := range bad {
		s := good()
		breakIt(&s)
		if s.Validate() == "" {
			t.Errorf("%s: accepted", name)
		}
	}
	// An address in a request is dropped: only the platform fills it in.
	s = good()
	s.Pools[0].Members[0].Address = "203.0.113.9"
	if msg := s.Validate(); msg != "" || s.Pools[0].Members[0].Address != "" {
		t.Fatalf("request address kept: %+v %q", s.Pools[0].Members[0], msg)
	}
}

// The renderer sees stored rows, so it must be safe on its own, even with a row validation never saw.
func TestRenderHAProxyNoInjection(t *testing.T) {
	evil := "x\n  bind :22\nglobal\n"
	cfg := RenderHAProxy(LBSpec{
		Listeners: []LBListener{{Port: 80, Pool: evil}},
		Pools: []LBPool{{
			Name: evil, Protocol: "tcp\n" + evil, Algorithm: "source\n" + evil,
			Members: []LBMember{
				{ServerID: "101", Port: 8080, Address: "10.0.0.5"},
				{ServerID: "102", Port: 8080, Address: "10.0.0.6\nserver x 1.1.1.1:1"},
				{ServerID: "103\n" + evil, Port: 8080, Address: "10.0.0.7"},
				{ServerID: "104", Port: 8080, Address: "10.0.0.8", Missing: true},
			},
		}},
	})
	for _, bad := range []string{":22", "1.1.1.1", "10.0.0.7", "10.0.0.8", evil} {
		if strings.Contains(cfg, bad) {
			t.Errorf("config holds %q:\n%s", bad, cfg)
		}
	}
	if !strings.Contains(cfg, "server m101p8080 10.0.0.5:8080 check weight 1") {
		t.Errorf("good member missing:\n%s", cfg)
	}
}

func TestParseHAProxyStat(t *testing.T) {
	spec := LBSpec{Pools: []LBPool{{Name: "web", Members: []LBMember{{ServerID: "101", Port: 80}, {ServerID: "102", Port: 80}, {ServerID: "103", Port: 80}}}}}
	out := "# pxname,svname,status,weight\nl80,FRONTEND,OPEN,\np0,m101p80,UP,1\np0,m102p80,DOWN,1\np0,BACKEND,UP,1\n"
	got, err := ParseHAProxyStat(out, spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"UP", "DOWN", ""}
	for i, h := range got {
		if h.Status != want[i] || h.Pool != "web" {
			t.Errorf("member %d: %+v", i, h)
		}
	}
}

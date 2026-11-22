package pve

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
)

// l4GoldenSpec uses every layer 4 option once, next to a listener and a pool that use none.
func l4GoldenSpec() LBSpec {
	member := func(id, addr string, port int) LBMember {
		return LBMember{ServerID: id, Port: port, Weight: 1, Address: addr}
	}
	return LBSpec{
		Listeners: []LBListener{
			{Port: 80, Protocol: "http", Pool: "web", ConnectionLimit: 500, TimeoutClientData: 30000},
			{Port: 443, Protocol: "tcp", Pool: "tls", TimeoutClientData: 3600000},
			{Port: 5432, Protocol: "tcp", Pool: "db", ConnectionLimit: 100},
			{Port: 6379, Protocol: "tcp", Pool: "cache"},
			{Port: 8080, Pool: "plain"}, // a listener from before v0.11.1: its pool's protocol
		},
		Pools: []LBPool{
			{Name: "web", Protocol: "http", Algorithm: "roundrobin", Members: []LBMember{member("101", "10.0.0.11", 8443), member("102", "10.0.0.12", 8443)},
				HealthCheck:        &LBHealthCheck{Type: "https", Path: "/healthz", Interval: 5, Timeout: 2, Rise: 3, Fall: 4},
				SessionPersistence: &LBPersistence{Type: "source_ip", Timeout: 600}, TimeoutMemberConnect: 1000, TimeoutMemberData: 30000},
			{Name: "tls", Protocol: "tcp", Algorithm: "source", Members: []LBMember{member("101", "10.0.0.11", 443)},
				HealthCheck: &LBHealthCheck{Type: "tls-hello", Fall: 3}, ProxyProtocol: true, ProxyVersion: 1, TimeoutMemberData: 3600000},
			{Name: "db", Protocol: "tcp", Algorithm: "leastconn", Members: []LBMember{member("104", "10.0.0.14", 5432)},
				HealthCheck: &LBHealthCheck{Type: "tcp", Port: 8008, Interval: 10}, ProxyProtocol: true, ProxyVersion: 2,
				SessionPersistence: &LBPersistence{Type: "source_ip"}},
			{Name: "cache", Protocol: "tcp", Algorithm: "roundrobin", Members: []LBMember{member("105", "10.0.0.15", 6379)},
				HealthCheck: &LBHealthCheck{Type: "none"}},
			{Name: "plain", Protocol: "http", Algorithm: "roundrobin", Members: []LBMember{member("106", "10.0.0.16", 8080)}},
		},
	}
}

// The layer 4 options as each kind of balancer renders them. The files are checked by the HAProxy of both appliance
// images in test/integration (TestLBPairConfigsPassTheirChecks reads every lb*.cfg here).
func TestRenderHAProxyLayer4(t *testing.T) {
	s := l4GoldenSpec()
	if msg := s.Validate(); msg != "" { // what the goldens hold is what a tenant may ask for
		t.Fatal(msg)
	}
	s = l4GoldenSpec()
	golden(t, "lb-l4.cfg", RenderHAProxy(s))
	golden(t, "lb-l4-pair.cfg", RenderHAProxyOn(s, []string{"203.0.113.10", "203.0.113.11"}))
	// A cluster's: the same options on listeners with their own address and workers as members.
	workers := []LBMember{{Node: true, Address: "10.0.0.250", Port: 30080}, {Node: true, Address: "10.0.0.249", Port: 30080}}
	cluster := LBSpec{
		Listeners: []LBListener{
			{Port: 80, Protocol: "tcp", Pool: "s1a2b3c4d-30080", VIP: "203.0.113.10", ConnectionLimit: 1000, TimeoutClientData: 50000},
			{Port: 80, Protocol: "http", Pool: "s9f8e7d6c-30080", VIP: "203.0.113.11"},
		},
		Pools: []LBPool{
			{Name: "s1a2b3c4d-30080", Protocol: "tcp", Algorithm: "leastconn", Members: workers, ProxyProtocol: true, ProxyVersion: 1,
				HealthCheck:        &LBHealthCheck{Type: "http", Port: 32000, Path: "/healthz", Interval: 5, Timeout: 3, Rise: 1, Fall: 3},
				SessionPersistence: &LBPersistence{Type: "source_ip", Timeout: 10800}, TimeoutMemberConnect: 5000, TimeoutMemberData: 50000},
			{Name: "s9f8e7d6c-30080", Protocol: "http", Algorithm: "roundrobin", Members: workers, HealthCheck: &LBHealthCheck{Type: "none"}},
		},
	}
	golden(t, "lb-l4-cluster.cfg", RenderClusterHAProxy(cluster, []string{"203.0.113.10", "203.0.113.11"}))
}

// Each option alone changes its own lines of the config and no other: what a field at its default writes is nothing,
// so a balancer made before the field keeps its bytes (and is not pushed again by the upgrade).
func TestRenderHAProxyEachOption(t *testing.T) {
	base := strings.Split(RenderHAProxy(legacyGoldenSpec()), "\n")
	hc := func(c LBHealthCheck) func(*LBSpec) { return func(s *LBSpec) { s.Pools[1].HealthCheck = &c } }
	const member = "  server m104p5432 10.0.0.14:5432"
	for name, c := range map[string]struct {
		set       func(*LBSpec)
		add, gone []string
	}{
		"listener protocol written out": {set: func(s *LBSpec) { s.Listeners[0].Protocol, s.Listeners[1].Protocol = "http", "tcp" }},
		"connection limit":              {set: func(s *LBSpec) { s.Listeners[1].ConnectionLimit = 250 }, add: []string{"  maxconn 250"}},
		"client timeout":                {set: func(s *LBSpec) { s.Listeners[1].TimeoutClientData = 1500 }, add: []string{"  timeout client 1500ms"}},
		"member timeouts": {set: func(s *LBSpec) { s.Pools[1].TimeoutMemberConnect, s.Pools[1].TimeoutMemberData = 1000, 86400000 },
			add: []string{"  timeout connect 1000ms", "  timeout server 86400000ms"}},
		"persistence": {set: func(s *LBSpec) { s.Pools[1].SessionPersistence = &LBPersistence{Type: "source_ip", Timeout: 60} },
			add: []string{"  stick-table type ip size 100k expire 60s", "  stick on src"}},
		"persistence, no time": {set: func(s *LBSpec) { s.Pools[1].SessionPersistence = &LBPersistence{Type: "source_ip"} },
			add: []string{"  stick-table type ip size 100k expire 10800s", "  stick on src"}},
		"PROXY v1": {set: func(s *LBSpec) { s.Pools[1].ProxyProtocol, s.Pools[1].ProxyVersion = true, 1 },
			add: []string{member + " check weight 1 send-proxy"}, gone: []string{member + " check weight 1"}},
		"PROXY v2 by number": {set: func(s *LBSpec) { s.Pools[1].ProxyProtocol, s.Pools[1].ProxyVersion = true, 2 },
			add: []string{member + " check weight 1 send-proxy-v2"}, gone: []string{member + " check weight 1"}},
		"PROXY as before": {set: func(s *LBSpec) { s.Pools[1].ProxyProtocol = true },
			add: []string{member + " check weight 1 send-proxy-v2"}, gone: []string{member + " check weight 1"}},
		"a version without PROXY": {set: func(s *LBSpec) { s.Pools[1].ProxyVersion = 1 }},
		"check: tcp, as none set": {set: hc(LBHealthCheck{Type: "tcp"})},
		"check: https": {set: hc(LBHealthCheck{Type: "https", Port: 8443, Path: "/ready"}),
			add: []string{"  option httpchk GET /ready", member + " check port 8443 check-ssl verify none weight 1"}, gone: []string{member + " check weight 1"}},
		"check: tls-hello": {set: hc(LBHealthCheck{Type: "tls-hello"}), add: []string{"  option ssl-hello-chk"}},
		"check: none":      {set: hc(LBHealthCheck{Type: "none"}), add: []string{member + " weight 1"}, gone: []string{member + " check weight 1"}},
		"check: interval":  {set: hc(LBHealthCheck{Type: "tcp", Interval: 10}), add: []string{"  default-server inter 10s fall 2 rise 2"}},
		"check: rise":      {set: hc(LBHealthCheck{Type: "tcp", Rise: 5}), add: []string{"  default-server inter 3s fall 2 rise 5"}},
		"check: fall":      {set: hc(LBHealthCheck{Type: "tcp", Fall: 1}), add: []string{"  default-server inter 3s fall 1 rise 2"}},
		"check: timeout":   {set: hc(LBHealthCheck{Type: "tcp", Timeout: 2}), add: []string{"  timeout check 2s"}},
		// A stored row no validation saw: a figure off its range writes nothing.
		"off the ranges": {set: func(s *LBSpec) {
			s.Listeners[1].ConnectionLimit, s.Listeners[1].TimeoutClientData = lbMaxConnections+1, 5
			s.Pools[1].TimeoutMemberConnect, s.Pools[1].TimeoutMemberData, s.Pools[1].ProxyVersion = -1, lbMaxTimeout+1, 7
			s.Pools[1].HealthCheck = &LBHealthCheck{Type: "tcp\n  bind :22", Interval: 301, Timeout: -2, Rise: 11, Fall: 99}
			s.Pools[1].SessionPersistence = &LBPersistence{Type: "cookie\n  bind :22"}
		}},
	} {
		s := legacyGoldenSpec()
		c.set(&s)
		got := strings.Split(RenderHAProxy(s), "\n")
		var add, gone []string
		for _, l := range got {
			if !slices.Contains(base, l) {
				add = append(add, l)
			}
		}
		for _, l := range base {
			if !slices.Contains(got, l) {
				gone = append(gone, l)
			}
		}
		if !slices.Equal(add, c.add) || !slices.Equal(gone, c.gone) {
			t.Errorf("%s: lines added %q, gone %q; want %q and %q", name, add, gone, c.add, c.gone)
		}
	}
	if !strings.Contains(haproxyGlobal, "\n  maxconn 20000\n") || lbMaxConnections != 20000 {
		t.Error("a listener's largest limit is not the appliance's global maxconn")
	}
}

// A row from before v0.11.1 names the protocol on the pool alone. It reads as it did, its JSON gains no key, and
// the config of the same balancer saved again (Validate writes each listener's protocol out) is the same bytes.
func TestOldRowsKeepTheirConfig(t *testing.T) {
	old := `{"listeners":[{"port":80,"pool":"web"},{"port":5432,"pool":"db"}],"pools":[` +
		`{"name":"web","protocol":"http","algorithm":"roundrobin","members":[{"serverId":"101","port":8080,"weight":1,"address":"10.0.0.11"},{"serverId":"102","port":8080,"weight":3,"address":"10.0.0.12"},{"serverId":"103","port":8080,"weight":1,"address":"10.0.0.13","missing":true}]},` +
		`{"name":"db","protocol":"tcp","algorithm":"leastconn","members":[{"serverId":"104","port":5432,"weight":1,"address":"10.0.0.14"}]}]}`
	var s LBSpec
	if err := json.Unmarshal([]byte(old), &s); err != nil {
		t.Fatal(err)
	}
	golden(t, "lb-legacy.cfg", RenderHAProxy(s))
	if again, err := json.Marshal(s); err != nil || string(again) != old {
		t.Fatalf("an old row's JSON changed:\n%s\n%v", again, err)
	}
	addrs := map[string]string{}
	for _, p := range s.Pools {
		for _, m := range p.Members {
			addrs[m.ServerID] = m.Address
		}
	}
	s.Pools[0].Members = s.Pools[0].Members[:2] // the one whose server left
	if msg := s.Validate(); msg != "" {
		t.Fatal(msg)
	}
	if s.Listeners[0].Protocol != "http" || s.Listeners[1].Protocol != "tcp" {
		t.Fatalf("listeners after Validate: %+v", s.Listeners)
	}
	for i := range s.Pools { // Validate clears what the platform fills in
		for j := range s.Pools[i].Members {
			s.Pools[i].Members[j].Address = addrs[s.Pools[i].Members[j].ServerID]
		}
	}
	golden(t, "lb-legacy.cfg", RenderHAProxy(s))
}

// The validation matrix and every figure's range.
func TestLBSpecValidateLayer4(t *testing.T) {
	base := func() LBSpec {
		return LBSpec{
			Listeners: []LBListener{{Port: 80, Pool: "web"}},
			Pools:     []LBPool{{Name: "web", Members: []LBMember{{ServerID: "101", Port: 8080}}}},
		}
	}
	// Who names the protocol: the pool (a request from before v0.11.1), the listener, both, or nobody.
	for name, c := range map[string]struct{ listener, pool, wantListener, wantPool, refused string }{
		"nobody":               {wantListener: "http", wantPool: "http"},
		"the pool, tcp":        {pool: "tcp", wantListener: "tcp", wantPool: "tcp"},
		"the pool, http":       {pool: "http", wantListener: "http", wantPool: "http"},
		"the listener, tcp":    {listener: "tcp", wantListener: "tcp", wantPool: "tcp"},
		"the listener, http":   {listener: "http", wantListener: "http", wantPool: "http"},
		"both, tcp":            {listener: "tcp", pool: "tcp", wantListener: "tcp", wantPool: "tcp"},
		"tcp in front of http": {listener: "tcp", pool: "http", refused: "a tcp listener needs a tcp pool, and pool web is http"},
		"http in front of tcp": {listener: "http", pool: "tcp", refused: "an http listener needs an http pool, and pool web is tcp"},
		"udp":                  {listener: "udp", refused: lbNotServed["udp"]},
		"udp, both":            {listener: "udp", pool: "udp", refused: "Pool web: " + lbNotServed["udp"]},
		"a udp pool":           {pool: "udp", refused: lbNotServed["udp"]},
		"tls":                  {listener: "tls", pool: "tcp", refused: lbNotServed["tls"]},
		"https":                {listener: "https", refused: lbNotServed["https"]},
		"sctp":                 {listener: "sctp", refused: "protocol must be tcp or http"},
		"upper case":           {listener: "TCP", refused: "protocol must be tcp or http"},
		"a pool off the list":  {pool: "https", refused: "protocol must be http or tcp"},
	} {
		s := base()
		s.Listeners[0].Protocol, s.Pools[0].Protocol = c.listener, c.pool
		msg := s.Validate()
		switch {
		case c.refused != "" && !strings.Contains(msg, c.refused):
			t.Errorf("%s: answer %q, want %q", name, msg, c.refused)
		case c.refused == "" && (msg != "" || s.Listeners[0].Protocol != c.wantListener || s.Pools[0].Protocol != c.wantPool):
			t.Errorf("%s: %q, listener %q pool %q", name, msg, s.Listeners[0].Protocol, s.Pools[0].Protocol)
		}
	}
	// One pool serves listeners of one protocol, and one port holds one TCP listener.
	two := base()
	two.Listeners = []LBListener{{Port: 80, Protocol: "http", Pool: "web"}, {Port: 81, Protocol: "tcp", Pool: "web"}}
	if msg := two.Validate(); !strings.Contains(msg, "Listener on port 81: a tcp listener needs a tcp pool") {
		t.Errorf("one pool behind two protocols: %q", msg)
	}
	two = base()
	two.Listeners = []LBListener{{Port: 80, Protocol: "http", Pool: "web"}, {Port: 80, Pool: "web"}}
	if msg := two.Validate(); msg != "Port 80 has two listeners" {
		t.Errorf("two listeners on a port: %q", msg)
	}

	hc := func(c LBHealthCheck) func(*LBSpec) { return func(s *LBSpec) { s.Pools[0].HealthCheck = &c } }
	good := map[string]func(*LBSpec){
		"limits at their ends": func(s *LBSpec) {
			s.Listeners[0].ConnectionLimit, s.Listeners[0].TimeoutClientData = lbMaxConnections, lbMinTimeout
			s.Pools[0].TimeoutMemberConnect, s.Pools[0].TimeoutMemberData = lbMinTimeout, lbMaxTimeout
		},
		"one connection":        func(s *LBSpec) { s.Listeners[0].ConnectionLimit = 1 },
		"PROXY v1":              func(s *LBSpec) { s.Pools[0].ProxyProtocol, s.Pools[0].ProxyVersion = true, 1 },
		"PROXY v2":              func(s *LBSpec) { s.Pools[0].ProxyProtocol, s.Pools[0].ProxyVersion = true, 2 },
		"persistence":           func(s *LBSpec) { s.Pools[0].SessionPersistence = &LBPersistence{Type: "source_ip"} },
		"persistence for a day": func(s *LBSpec) { s.Pools[0].SessionPersistence = &LBPersistence{Type: "source_ip", Timeout: 86400} },
		"https check":           hc(LBHealthCheck{Type: "https", Port: 8443, Path: "/healthz", Interval: 300, Timeout: 300, Rise: 10, Fall: 10}),
		"tls-hello check":       hc(LBHealthCheck{Type: "tls-hello", Port: 443, Interval: 1, Timeout: 1, Rise: 1, Fall: 1}),
		"no check":              hc(LBHealthCheck{Type: "none"}),
	}
	for name, set := range good {
		s := base()
		set(&s)
		if msg := s.Validate(); msg != "" {
			t.Errorf("%s: refused: %s", name, msg)
		}
	}
	bad := map[string]func(*LBSpec){
		"limit below":                 func(s *LBSpec) { s.Listeners[0].ConnectionLimit = -1 },
		"limit past the appliance's":  func(s *LBSpec) { s.Listeners[0].ConnectionLimit = lbMaxConnections + 1 },
		"client timeout under 1 s":    func(s *LBSpec) { s.Listeners[0].TimeoutClientData = 999 },
		"client timeout over a day":   func(s *LBSpec) { s.Listeners[0].TimeoutClientData = lbMaxTimeout + 1 },
		"connect timeout under 1 s":   func(s *LBSpec) { s.Pools[0].TimeoutMemberConnect = 1 },
		"member timeout over a day":   func(s *LBSpec) { s.Pools[0].TimeoutMemberData = lbMaxTimeout + 1 },
		"member timeout below":        func(s *LBSpec) { s.Pools[0].TimeoutMemberData = -5 },
		"PROXY v3":                    func(s *LBSpec) { s.Pools[0].ProxyProtocol, s.Pools[0].ProxyVersion = true, 3 },
		"a version, PROXY off":        func(s *LBSpec) { s.Pools[0].ProxyVersion = 1 },
		"cookie persistence":          func(s *LBSpec) { s.Pools[0].SessionPersistence = &LBPersistence{Type: "http_cookie"} },
		"persistence without a type":  func(s *LBSpec) { s.Pools[0].SessionPersistence = &LBPersistence{Timeout: 60} },
		"persistence over a day":      func(s *LBSpec) { s.Pools[0].SessionPersistence = &LBPersistence{Type: "source_ip", Timeout: 86401} },
		"persistence below":           func(s *LBSpec) { s.Pools[0].SessionPersistence = &LBPersistence{Type: "source_ip", Timeout: -1} },
		"ping check":                  hc(LBHealthCheck{Type: "ping"}),
		"udp-connect check":           hc(LBHealthCheck{Type: "udp-connect"}),
		"no check, with a port":       hc(LBHealthCheck{Type: "none", Port: 80}),
		"no check, with an interval":  hc(LBHealthCheck{Type: "none", Interval: 5}),
		"tls-hello with a path":       hc(LBHealthCheck{Type: "tls-hello", Path: "/"}),
		"https with a path of spaces": hc(LBHealthCheck{Type: "https", Path: "/a b"}),
		"interval over 300 s":         hc(LBHealthCheck{Type: "tcp", Interval: 301}),
		"interval below":              hc(LBHealthCheck{Type: "tcp", Interval: -1}),
		"timeout over 300 s":          hc(LBHealthCheck{Type: "http", Timeout: 301}),
		"rise over 10":                hc(LBHealthCheck{Type: "tcp", Rise: 11}),
		"fall below":                  hc(LBHealthCheck{Type: "tcp", Fall: -1}),
		"certificates on tcp":         func(s *LBSpec) { s.Listeners[0].Certificates = []string{"c1"} },
	}
	for name, set := range bad {
		s := base()
		set(&s)
		if s.Validate() == "" {
			t.Errorf("%s: accepted", name)
		}
	}
}

// lbNotServed is the whole refusal of UDP and of TLS: with a protocol taken out of it (what the change that serves
// the protocol does), the model, the matrix, the listener key and the firewall already carry it, and HAProxy gets
// no frontend it cannot serve.
func TestNotServedIsTheOnlyRefusal(t *testing.T) {
	saved := maps.Clone(lbNotServed)
	t.Cleanup(func() { lbNotServed = saved })
	lbNotServed = map[string]string{}
	s := LBSpec{
		Listeners: []LBListener{
			{Port: 53, Protocol: "tcp", Pool: "dns-tcp"},
			{Port: 53, Protocol: "udp", Pool: "dns-udp", SourceRanges: []string{"198.51.100.0/24"}},
			{Port: 443, Protocol: "https", Pool: "web", Certificates: []string{"c1", "c2"}},
			{Port: 8443, Protocol: "tls", Pool: "dns-tcp", Certificates: []string{"c1"}},
		},
		Pools: []LBPool{
			{Name: "dns-tcp", Members: []LBMember{{ServerID: "101", Port: 53}}},
			{Name: "dns-udp", Members: []LBMember{{ServerID: "101", Port: 53}}},
			{Name: "web", Members: []LBMember{{ServerID: "102", Port: 8080}}},
		},
	}
	if msg := s.Validate(); msg != "" {
		t.Fatalf("refused with nothing in lbNotServed: %s", msg)
	}
	if got := []string{s.Pools[0].Protocol, s.Pools[1].Protocol, s.Pools[2].Protocol}; !slices.Equal(got, []string{"tcp", "udp", "http"}) {
		t.Fatalf("pool protocols %q", got)
	}
	for name, breakIt := range map[string]func(*LBSpec){
		"two udp listeners on a port": func(s *LBSpec) {
			s.Listeners = append(s.Listeners, LBListener{Port: 53, Protocol: "udp", Pool: "dns-udp"})
		},
		"https and tcp on a port": func(s *LBSpec) { s.Listeners[2].Port = 53 },
		"udp in front of tcp":     func(s *LBSpec) { s.Listeners[1].Pool = "dns-tcp" },
		"tls in front of http":    func(s *LBSpec) { s.Listeners[3].Pool = "web" },
		"certificates on udp":     func(s *LBSpec) { s.Listeners[1].Certificates = []string{"c1"} },
	} {
		bad := LBSpec{Listeners: slices.Clone(s.Listeners), Pools: slices.Clone(s.Pools)}
		breakIt(&bad)
		if bad.Validate() == "" {
			t.Errorf("%s: accepted", name)
		}
	}
	// The firewall opens each port for its own transport.
	var rules []string
	for _, r := range LBListenerRules(s.Listeners, []string{"203.0.113.10"}) {
		rules = append(rules, r.Proto+" "+r.Source+" "+r.DPort)
	}
	if want := []string{"tcp  53,443,8443", "udp 198.51.100.0/24 53"}; !slices.Equal(rules, want) {
		t.Errorf("rules %q, want %q", rules, want)
	}
	// HAProxy gets a frontend for the TCP listener alone, and the health read misses none of the others.
	for i := range s.Pools {
		s.Pools[i].Members[0].Address = "10.0.0.11"
	}
	cfg := RenderHAProxy(s)
	if n := strings.Count(cfg, "\nfrontend "); n != 1 || !strings.Contains(cfg, "\nfrontend l53\n  bind :53\n  mode tcp\n") {
		t.Errorf("frontends: %d\n%s", n, cfg)
	}
	_, notLoaded, err := ParseHAProxyStat("# pxname,svname,status\nl53,FRONTEND,OPEN\n", s)
	if err != nil || len(notLoaded) != 0 {
		t.Errorf("listeners read as not loaded: %q %v", notLoaded, err)
	}
}

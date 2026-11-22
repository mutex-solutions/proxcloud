package pve

import (
	"regexp"
	"strings"
	"testing"
)

// lvsGoldenSpec uses every UDP option once, next to a TCP listener on the same port, which is HAProxy's.
func lvsGoldenSpec() LBSpec {
	member := func(id, addr string, port, weight int) LBMember {
		return LBMember{ServerID: id, Port: port, Weight: weight, Address: addr}
	}
	return LBSpec{
		Listeners: []LBListener{
			{Port: 53, Protocol: "tcp", Pool: "dns-tcp"},
			{Port: 53, Protocol: "udp", Pool: "dns", ConnectionLimit: 1000, SourceRanges: []string{"198.51.100.0/24"}},
			{Port: 5000, Protocol: "udp", Pool: "echo"},
			{Port: 5060, Protocol: "udp", Pool: "sip"},
			{Port: 6000, Protocol: "udp", Pool: "syslog"},
			{Port: 7000, Protocol: "udp", Pool: "game"},
			{Port: 9000, Protocol: "udp", Pool: "empty"}, // no member: no virtual server
		},
		Pools: []LBPool{
			{Name: "dns-tcp", Protocol: "tcp", Algorithm: "roundrobin", Members: []LBMember{member("101", "10.0.0.11", 53, 1)}},
			{Name: "dns", Protocol: "udp", Algorithm: "roundrobin", Members: []LBMember{member("101", "10.0.0.11", 53, 1), member("102", "10.0.0.12", 53, 256)},
				HealthCheck: &LBHealthCheck{Type: "udp-connect", Payload: "AA0001000001000000000000000001000100", RequireReply: true, Interval: 10, Timeout: 3, Fall: 2}},
			{Name: "echo", Protocol: "udp", Algorithm: "leastconn", Members: []LBMember{member("101", "10.0.0.11", 5000, 1), {ServerID: "103", Port: 5000, Weight: 1, Missing: true}}},
			{Name: "sip", Protocol: "udp", Algorithm: "source", Members: []LBMember{member("104", "10.0.0.14", 5060, 2)},
				HealthCheck: &LBHealthCheck{Type: "tcp", Port: 5061, Fall: 1}, SessionPersistence: &LBPersistence{Type: "source_ip", Timeout: 360}},
			{Name: "syslog", Protocol: "udp", Algorithm: "roundrobin", Members: []LBMember{member("105", "10.0.0.15", 514, 1)},
				HealthCheck: &LBHealthCheck{Type: "http", Port: 8080, Path: "/healthz"}, SessionPersistence: &LBPersistence{Type: "source_ip"}},
			{Name: "game", Protocol: "udp", Algorithm: "roundrobin", Members: []LBMember{member("106", "10.0.0.16", 7000, 1)},
				HealthCheck: &LBHealthCheck{Type: "none"}},
			{Name: "empty", Protocol: "udp", Algorithm: "roundrobin"},
		},
	}
}

// The LVS config as each kind of pair renders it. The files are checked by the keepalived of both appliance images
// in test/integration (TestLBPairConfigsPassTheirChecks reads every lvs*.conf here).
func TestRenderLVS(t *testing.T) {
	s := lvsGoldenSpec()
	s.Pools[2].Members = s.Pools[2].Members[:1] // Missing is the platform's mark, not a tenant's
	if msg := s.Validate(); msg != "" {         // what the goldens hold is what a tenant may ask for
		t.Fatal(msg)
	}
	golden(t, "lvs-one.conf", RenderLVS(LBSpec{
		Listeners: []LBListener{{Port: 5000, Protocol: "udp", Pool: "echo"}},
		Pools: []LBPool{{Name: "echo", Protocol: "udp", Algorithm: "roundrobin", Members: []LBMember{
			{ServerID: "101", Port: 5000, Weight: 1, Address: "10.0.0.11"}, {ServerID: "102", Port: 5000, Weight: 1, Address: "10.0.0.12"}}}},
	}, []string{"203.0.113.10"}))
	golden(t, "lvs-options.conf", RenderLVS(lvsGoldenSpec(), []string{"203.0.113.10"}))
	// A cluster's: each listener on its Service's own address, the workers as members, and TCP 53 beside UDP 53.
	workers := func(port int) []LBMember {
		return []LBMember{{Node: true, Address: "10.0.0.250", Port: port}, {Node: true, Address: "10.0.0.249", Port: port}}
	}
	cluster := LBSpec{
		Listeners: []LBListener{
			{Port: 53, Protocol: "udp", Pool: "s1a2b3c4d-udp-30053", VIP: "203.0.113.10"},
			{Port: 53, Protocol: "tcp", Pool: "s1a2b3c4d-30053", VIP: "203.0.113.10"},
			{Port: 53, Protocol: "udp", Pool: "s9f8e7d6c-udp-31053", VIP: "203.0.113.11"},
			{Port: 54, Protocol: "udp", Pool: "s9f8e7d6c-udp-31053", VIP: "203.0.113.99"}, // an address the balancer does not hold
		},
		Pools: []LBPool{
			{Name: "s1a2b3c4d-udp-30053", Protocol: "udp", Algorithm: "roundrobin", Members: workers(30053)},
			{Name: "s1a2b3c4d-30053", Protocol: "tcp", Algorithm: "roundrobin", Members: workers(30053)},
			{Name: "s9f8e7d6c-udp-31053", Protocol: "udp", Algorithm: "roundrobin", Members: workers(31053),
				HealthCheck: &LBHealthCheck{Type: "http", Port: 32000, Path: "/healthz"}, SessionPersistence: &LBPersistence{Type: "source_ip", Timeout: 10800}},
		},
	}
	golden(t, "lvs-cluster.conf", RenderLVS(cluster, []string{"203.0.113.10", "203.0.113.11"}))

	// A plain balancer of two addresses serves a listener on each.
	two := RenderLVS(LBSpec{Listeners: lvsGoldenSpec().Listeners[2:3], Pools: lvsGoldenSpec().Pools}, []string{"203.0.113.10", "203.0.113.11", "bad"})
	if strings.Count(two, "virtual_server ") != 2 || !strings.Contains(two, "virtual_server 203.0.113.11 5000 {") {
		t.Errorf("a listener on two addresses:\n%s", two)
	}
	// The whole spec a tenant may send does not fit the agent's file write: the size is checked where it is taken.
	big := LBSpec{}
	for i := range lbMaxListeners {
		p := LBPool{Name: "p" + string(rune('a'+i)), Protocol: "udp"}
		for j := range lbMaxMembers {
			p.Members = append(p.Members, LBMember{ServerID: "1", Port: 1000 + j, Address: "10.0.0.11"})
		}
		big.Pools = append(big.Pools, p)
		big.Listeners = append(big.Listeners, LBListener{Port: 1000 + i, Protocol: "udp", Pool: p.Name})
	}
	if n := len(RenderLVS(big, []string{"203.0.113.10"})); n <= LBMaxConfig {
		t.Errorf("the largest spec renders to %d bytes: the size check in UPDATE_CONFIG is never needed", n)
	}
}

// A pair without a UDP listener that can be served has no LVS config at all, so nothing of the UDP service reaches
// it, and a UDP listener changes no byte of the HAProxy config: the hashes of an existing pair do not move.
func TestNoUDPNoLVS(t *testing.T) {
	vips := []string{"203.0.113.10"}
	for name, s := range map[string]LBSpec{
		"the legacy golden":   legacyGoldenSpec(),
		"the layer 4 golden":  l4GoldenSpec(),
		"nothing":             {},
		"a udp pool alone":    {Pools: lvsGoldenSpec().Pools},
		"a listener, no pool": {Listeners: []LBListener{{Port: 53, Protocol: "udp", Pool: "gone"}}},
		"udp in front of tcp": {Listeners: []LBListener{{Port: 53, Protocol: "udp", Pool: "dns-tcp"}}, Pools: lvsGoldenSpec().Pools},
		"tcp in front of udp": {Listeners: []LBListener{{Port: 53, Protocol: "tcp", Pool: "dns"}}, Pools: lvsGoldenSpec().Pools},
		"no member":           {Listeners: []LBListener{{Port: 9000, Protocol: "udp", Pool: "empty"}}, Pools: lvsGoldenSpec().Pools},
		"a member that left": {Listeners: []LBListener{{Port: 53, Protocol: "udp", Pool: "p"}},
			Pools: []LBPool{{Name: "p", Protocol: "udp", Members: []LBMember{{ServerID: "101", Port: 53, Address: "10.0.0.11", Missing: true}}}}},
		"port out of range": {Listeners: []LBListener{{Port: 70000, Protocol: "udp", Pool: "dns"}}, Pools: lvsGoldenSpec().Pools},
	} {
		if got := RenderLVS(s, vips); got != "" {
			t.Errorf("%s: an LVS config:\n%s", name, got)
		}
	}
	if got := RenderLVS(lvsGoldenSpec(), nil); got != "" {
		t.Errorf("a balancer without an address has an LVS config:\n%s", got)
	}
	with, without := lvsGoldenSpec(), lvsGoldenSpec()
	without.Listeners, without.Pools = without.Listeners[:1], without.Pools[:1]
	if got, want := RenderHAProxyOn(with, vips), RenderHAProxyOn(without, vips); got != want {
		t.Errorf("UDP listeners and pools changed the HAProxy config:\n--- with\n%s\n--- without\n%s", got, want)
	}
	// HAProxy's health read holds no member of a UDP pool and waits for no frontend of a UDP listener.
	stat := "# pxname,svname,status\nl203.0.113.10_53,FRONTEND,OPEN\np-dns-tcp,BACKEND,UP\np-dns-tcp,m101p53,UP\n"
	health, notLoaded, err := ParseHAProxyStat(stat, with)
	if err != nil || len(notLoaded) != 0 || len(health) != 1 || health[0].Pool != "dns-tcp" || health[0].Status != "UP" {
		t.Errorf("HAProxy's health of a spec with UDP: %+v %q %v", health, notLoaded, err)
	}
}

// Nothing a stored row holds becomes a directive: every line of the file is one of the renderer's own shapes.
func TestRenderLVSNoInjection(t *testing.T) {
	evil := "x\n  real_server 198.51.100.1 22 {\n"
	s := LBSpec{
		Listeners: []LBListener{{Port: 53, Protocol: "udp", Pool: "p", VIP: "203.0.113.10", ConnectionLimit: 1 << 30}},
		Pools: []LBPool{{Name: "p", Protocol: "udp", Algorithm: evil, Members: []LBMember{
			{ServerID: "101", Port: 53, Weight: 9999, Address: "10.0.0.11"},
			{ServerID: "102", Port: 53, Address: "10.0.0.12\n}"}, {ServerID: evil, Port: 53, Address: "10.0.0.13"}},
			HealthCheck:        &LBHealthCheck{Type: "udp-connect", Path: evil, Payload: "70 }\nalpha", Port: 99999, Interval: 9999, Timeout: -4, Fall: 99},
			SessionPersistence: &LBPersistence{Type: "source_ip", Timeout: 1 << 30}}},
	}
	got := RenderLVS(s, []string{"203.0.113.10"})
	line := regexp.MustCompile(`^(instance lvs|global_defs \{|  router_id proxcloud-lvs|  lvs_flush_on_stop|\}||virtual_server 203\.0\.113\.10 53 \{|  protocol UDP|  lvs_method NAT|  lvs_sched wrr|  alpha|  delay_loop 5|  retry 2|  delay_before_retry 2|  persistence_timeout 10800|  real_server 10\.0\.0\.11 53 \{|    weight 1|    UDP_CHECK \{|      connect_timeout [25]|    \}|    PING_CHECK \{|      retry 2|      delay_before_retry 1|  \})$`)
	for _, l := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if !line.MatchString(l) {
			t.Errorf("line %q is none of the renderer's own:\n%s", l, got)
		}
	}
	if strings.Count(got, "real_server") != 1 {
		t.Errorf("want the one member with a plain address:\n%s", got)
	}
	// A path and a check type off the lists read as the defaults.
	s.Pools[0].HealthCheck = &LBHealthCheck{Type: "http", Path: "/ok\n  status_code 500"}
	if got := RenderLVS(s, []string{"203.0.113.10"}); !strings.Contains(got, "        path /\n        status_code 200\n") || strings.Contains(got, "500") {
		t.Errorf("a path off the pattern:\n%s", got)
	}
	s.Pools[0].HealthCheck = &LBHealthCheck{Type: "tls-hello"}
	if got := RenderLVS(s, []string{"203.0.113.10"}); !strings.Contains(got, "UDP_CHECK") || !strings.Contains(got, "PING_CHECK") {
		t.Errorf("a check off the list:\n%s", got)
	}
}

func TestLBSpecValidateUDP(t *testing.T) {
	base := func() LBSpec {
		return LBSpec{
			Listeners: []LBListener{{Port: 53, Protocol: "udp", Pool: "dns"}},
			Pools:     []LBPool{{Name: "dns", Members: []LBMember{{ServerID: "101", Port: 53}}}},
		}
	}
	hc := func(c LBHealthCheck) func(*LBSpec) { return func(s *LBSpec) { s.Pools[0].HealthCheck = &c } }
	for name, set := range map[string]func(*LBSpec){
		"nothing more":            func(*LBSpec) {},
		"the three algorithms":    func(s *LBSpec) { s.Pools[0].Algorithm = "source" },
		"least connections":       func(s *LBSpec) { s.Pools[0].Algorithm = "leastconn" },
		"persistence":             func(s *LBSpec) { s.Pools[0].SessionPersistence = &LBPersistence{Type: "source_ip", Timeout: 360} },
		"a limit":                 func(s *LBSpec) { s.Listeners[0].ConnectionLimit = 1000 },
		"source ranges":           func(s *LBSpec) { s.Listeners[0].SourceRanges = []string{"198.51.100.0/24"} },
		"weights":                 func(s *LBSpec) { s.Pools[0].Members[0].Weight = 256 },
		"udp-connect, typed":      hc(LBHealthCheck{Type: "udp-connect", Port: 5353, Interval: 300, Timeout: 300, Fall: 10}),
		"a check without a type":  hc(LBHealthCheck{Interval: 5}),
		"a payload":               hc(LBHealthCheck{Type: "udp-connect", Payload: "70696E67"}),
		"a required reply":        hc(LBHealthCheck{Type: "udp-connect", Payload: "70696e67", RequireReply: true}),
		"a reply to random bytes": hc(LBHealthCheck{Type: "udp-connect", RequireReply: true}),
		"a tcp check":             hc(LBHealthCheck{Type: "tcp", Port: 53}),
		"an http check":           hc(LBHealthCheck{Type: "http", Port: 8080, Path: "/healthz"}),
		"no check":                hc(LBHealthCheck{Type: "none"}),
		"tcp beside it, same port": func(s *LBSpec) {
			s.Listeners = append(s.Listeners, LBListener{Port: 53, Protocol: "tcp", Pool: "dns-tcp"})
			s.Pools = append(s.Pools, LBPool{Name: "dns-tcp", Members: []LBMember{{ServerID: "101", Port: 53}}})
		},
	} {
		s := base()
		set(&s)
		if msg := s.Validate(); msg != "" || s.Pools[0].Protocol != "udp" || !s.HasUDP() {
			t.Errorf("%s: %q, pool protocol %q", name, msg, s.Pools[0].Protocol)
		}
	}
	if s := base(); s.Validate() != "" || s.Pools[0].HealthCheck != nil {
		t.Errorf("a pool without a check got one written: %+v", s.Pools[0].HealthCheck)
	}
	s := base()
	hc(LBHealthCheck{Interval: 5})(&s)
	if s.Validate() != "" || s.Pools[0].HealthCheck.Type != "udp-connect" {
		t.Errorf("a UDP pool's check without a type is %q, want udp-connect", s.Pools[0].HealthCheck.Type)
	}
	for name, c := range map[string]struct {
		set  func(*LBSpec)
		want string
	}{
		"PROXY protocol":          {func(s *LBSpec) { s.Pools[0].ProxyProtocol = true }, "UDP has no PROXY protocol"},
		"a connect timeout":       {func(s *LBSpec) { s.Pools[0].TimeoutMemberConnect = 2000 }, "takes no member timeouts"},
		"a member timeout":        {func(s *LBSpec) { s.Pools[0].TimeoutMemberData = 2000 }, "takes no member timeouts"},
		"a client timeout":        {func(s *LBSpec) { s.Listeners[0].TimeoutClientData = 2000 }, "a UDP listener takes no client timeout"},
		"certificates":            {func(s *LBSpec) { s.Listeners[0].Certificates = []string{"c1"} }, "only a tls or https listener"},
		"an https check":          {hc(LBHealthCheck{Type: "https"}), "must be udp-connect, tcp, http or none"},
		"a tls-hello check":       {hc(LBHealthCheck{Type: "tls-hello"}), "must be udp-connect, tcp, http or none"},
		"a ping check":            {hc(LBHealthCheck{Type: "ping"}), "must be udp-connect, tcp, http or none"},
		"rise":                    {hc(LBHealthCheck{Type: "udp-connect", Rise: 2}), "takes no rise"},
		"a payload on tcp":        {hc(LBHealthCheck{Type: "tcp", Payload: "00"}), "only a udp-connect health check"},
		"a reply on http":         {hc(LBHealthCheck{Type: "http", RequireReply: true}), "only a udp-connect health check"},
		"a payload of text":       {hc(LBHealthCheck{Type: "udp-connect", Payload: "ping"}), "hexadecimal digits"},
		"half a byte":             {hc(LBHealthCheck{Type: "udp-connect", Payload: "abc"}), "hexadecimal digits"},
		"a payload with a brace":  {hc(LBHealthCheck{Type: "udp-connect", Payload: "00}\n"}), "hexadecimal digits"},
		"a payload over 128":      {hc(LBHealthCheck{Type: "udp-connect", Payload: strings.Repeat("00", 129)}), "hexadecimal digits"},
		"no check, with a reply":  {hc(LBHealthCheck{Type: "none", RequireReply: true}), "only a udp-connect health check"},
		"a limit past the most":   {func(s *LBSpec) { s.Listeners[0].ConnectionLimit = lbMaxConnections + 1 }, "connection limit"},
		"two udp listeners on 53": {func(s *LBSpec) { s.Listeners = append(s.Listeners, LBListener{Port: 53, Protocol: "udp", Pool: "dns"}) }, "Port 53 has two listeners"},
		"a tcp pool behind it":    {func(s *LBSpec) { s.Pools[0].Protocol = "tcp" }, "a udp listener needs a udp pool"},
	} {
		s := base()
		c.set(&s)
		if msg := s.Validate(); !strings.Contains(msg, c.want) {
			t.Errorf("%s: answer %q, want %q", name, msg, c.want)
		}
	}
	// udp-connect, a payload and a required reply are a UDP pool's: a TCP pool takes none of them.
	for name, c := range map[string]LBHealthCheck{"udp-connect": {Type: "udp-connect"}, "a payload": {Type: "tcp", Payload: "00"}, "a reply": {Type: "http", RequireReply: true}} {
		s := LBSpec{Listeners: []LBListener{{Port: 80, Pool: "web"}}, Pools: []LBPool{{Name: "web", Protocol: "tcp", HealthCheck: &c}}}
		if s.Validate() == "" || s.HasUDP() {
			t.Errorf("a TCP pool with %s: accepted", name)
		}
	}
}

func TestParseLVSTable(t *testing.T) {
	spec := lvsGoldenSpec()
	vips := []string{"203.0.113.10"}
	// As /proc/net/ip_vs prints it: a TCP server of somebody else's, dns with one of two members, echo whole,
	// syslog and game whole, sip not there, and a UDP server no listener of this spec has.
	table := `IP Virtual Server version 1.2.1 (size=4096)
Prot LocalAddress:Port Scheduler Flags
  -> RemoteAddress:Port Forward Weight ActiveConn InActConn
TCP  CB00710A:0035 wrr
  -> 0A00000C:0035      Masq    1      0          0
UDP  CB00710A:0035 wrr
  -> 0A00000B:0035      Masq    1      0          3
UDP  CB00710A:1388 lc
  -> 0A00000B:1388      Masq    1      0          0
UDP  CB00710A:1770 wrr persistent 10800 FFFFFFFF
  -> 0A00000F:0202      Masq    1      0          0
UDP  CB00710A:1B58 wrr
  -> 0A000010:1B58      Masq    1      0          0
UDP  CB007163:0035 wrr
  -> 0A00000C:0035      Masq    1      0          0
`
	health, notLoaded := ParseLVSTable(table, spec, vips)
	got := map[string]string{}
	for _, h := range health {
		got[h.Pool+"/"+h.ServerID] = h.Status
	}
	want := map[string]string{"dns/101": "UP", "dns/102": "DOWN", "echo/101": "UP", "echo/103": "", "sip/104": LBNotLoaded, "syslog/105": "UP", "game/106": "UP"}
	if len(got) != len(want) {
		t.Errorf("members %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s is %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if len(notLoaded) != 1 || notLoaded[0] != "5060/udp" {
		t.Errorf("listeners not in the table: %q, want 5060/udp", notLoaded)
	}
	// No table at all (the module is not loaded, or the service was never started): nothing is loaded.
	health, notLoaded = ParseLVSTable("", spec, vips)
	if len(notLoaded) != 5 || health[0].Status != LBNotLoaded {
		t.Errorf("an empty table: %+v %q", health, notLoaded)
	}
	// A cluster's: workers by address, and the listener named with its address.
	cluster := LBSpec{
		Listeners: []LBListener{{Port: 53, Protocol: "udp", Pool: "a", VIP: "203.0.113.10"}, {Port: 53, Protocol: "udp", Pool: "b", VIP: "203.0.113.11"}},
		Pools: []LBPool{{Name: "a", Protocol: "udp", Members: []LBMember{{Node: true, Address: "10.0.0.250", Port: 30053}}},
			{Name: "b", Protocol: "udp", Members: []LBMember{{Node: true, Address: "10.0.0.250", Port: 31053}}}},
	}
	health, notLoaded = ParseLVSTable("UDP  CB00710A:0035 wrr\n  -> 0A0000FA:7565      Masq    1      0          0\n", cluster, []string{"203.0.113.10", "203.0.113.11"})
	if len(health) != 2 || health[0].Status != "UP" || health[0].Address != "10.0.0.250" || health[1].Status != LBNotLoaded || len(notLoaded) != 1 || notLoaded[0] != "203.0.113.11:53/udp" {
		t.Errorf("a cluster's table: %+v %q", health, notLoaded)
	}
}

// A UDP listener opens its port for UDP only, per address and source set, next to a TCP listener on the same port.
func TestLBListenerRulesUDP(t *testing.T) {
	rules := LBListenerRules(lvsGoldenSpec().Listeners, []string{"203.0.113.10"})
	var got []string
	for _, r := range rules {
		got = append(got, strings.Join([]string{r.Proto, r.Source, r.Dest, r.DPort}, " "))
	}
	want := []string{"tcp  203.0.113.10 53", "udp  203.0.113.10 5000,5060,6000,7000,9000", "udp 198.51.100.0/24 203.0.113.10 53"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// What the support files must hold whatever else changes in them (plan 4.2, K1, K2, spike changes 1 and 2).
func TestLVSFiles(t *testing.T) {
	files := map[string]string{}
	for _, f := range lvsFiles("ens18") {
		files[f[0]] = f[1]
	}
	nft, sysctl, net, unit := files[lvsNft], files[lvsSysctl], files[lvsNet], files[lvsUnit]
	for _, want := range []string{
		"table ip proxcloud-lb\ndelete table ip proxcloud-lb\ntable ip proxcloud-lb {\n", // one transaction replaces the table
		"    oifname \"ens18\" meta l4proto udp counter masquerade\n",                    // the guest's own name of the tenant NIC
		"    type filter hook forward priority 0; policy drop;\n    ct state established,related counter accept\n",
		"    type filter hook prerouting priority -300;\n    meta l4proto tcp notrack\n",
		"    type filter hook output priority -300;\n    meta l4proto tcp notrack\n",
	} {
		if !strings.Contains(nft, want) {
			t.Errorf("lvs.nft lacks %q:\n%s", want, nft)
		}
	}
	// One statement a line, and no chain named by a word nft reads as a keyword (spike: "fwd").
	for _, l := range strings.Split(nft, "\n") {
		if strings.Contains(l, "{") && strings.Contains(l, "}") {
			t.Errorf("a one-line block in lvs.nft: %q", l)
		}
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "chain" && !strings.HasPrefix(f[1], "lb-") {
			t.Errorf("chain %q is not a name of the product's own", f[1])
		}
	}
	for _, want := range []string{"net.ipv4.vs.conntrack = 1\n", "net.ipv4.vs.snat_reroute = 1\n", "net.ipv4.vs.expire_nodest_conn = 1\n", "net.ipv4.vs.expire_quiescent_template = 1\n",
		"net.ipv4.conf.all.send_redirects = 0\n", "net.ipv4.conf.ens18.send_redirects = 0\n"} {
		if !strings.Contains(sysctl, want) {
			t.Errorf("lvs.sysctl lacks %q", want)
		}
	}
	// Forwarding is the unit's to switch, after the table: in no file a boot applies by itself.
	if strings.Contains(sysctl, "ip_forward") || strings.HasPrefix(lvsSysctl, "/etc/sysctl.d/") {
		t.Errorf("ip_forward is set from a sysctl file: %s\n%s", lvsSysctl, sysctl)
	}
	table, on := strings.Index(net, "nft -f "+lvsNft), strings.Index(net, "net.ipv4.ip_forward=1")
	if table < 0 || on < table || strings.Count(net, "net.ipv4.ip_forward=1") != 1 || !strings.Contains(net, "sysctl -q -w net.ipv4.ip_forward=0\n  nft delete table") {
		t.Errorf("the net script must load the table before it switches forwarding on, and switch it off before the table goes:\n%s", net)
	}
	for _, want := range []string{"ExecStartPre=" + lvsNet + " up\n", "ExecStopPost=" + lvsNet + " down\n", "keepalived --check --dont-fork -f " + lvsConfig + " "} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit lacks %q", want)
		}
	}
	for _, f := range files {
		if strings.Contains(f, "net0") || strings.Contains(f, "eth0") {
			t.Errorf("an interface name is written out:\n%s", f)
		}
	}
	// The hash moves with the config, with the interface and with nothing else.
	a := LVSHash("x", "ens18")
	if a != LVSHash("x", "ens18") || a == LVSHash("y", "ens18") || a == LVSHash("x", "eth0") || len(a) != 64 {
		t.Errorf("LVSHash: %s", a)
	}
}

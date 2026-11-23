package pve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve/lbguesttest"
)

// scriptGuest is a guest the product's own calls reach through the guest agent API, and whose scripts run as text
// in shell (lbguesttest).
func scriptGuest(t *testing.T, shell string) (*Service, *lbguesttest.Guest, *lbguesttest.Agent) {
	t.Helper()
	g := lbguesttest.New(t)
	a := &lbguesttest.Agent{Guest: g, Shell: shell}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	c, err := New(Config{APIURL: srv.URL, TokenID: "proxcloud@pve!t1", Secret: "sec"})
	if err != nil {
		t.Fatal(err)
	}
	return &Service{C: c}, g, a
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// scriptHAProxy is a pair's config with two listeners on the address.
const scriptHAProxy = "global\n  stats socket /run/haproxy/admin.sock mode 660 level admin\n\nfrontend l80\n  bind 203.0.113.10:80\n\nfrontend l443\n  bind 203.0.113.10:443\n"

// holder puts a guest where the appliance that holds a pair's address is: keepalived master, HAProxy with a worker
// and its listeners, the address on the public NIC.
func holder(g *lbguesttest.Guest) {
	g.Write(lbStateFile, "MASTER\n")
	g.Write(haproxyConfig, scriptHAProxy)
	g.Write("/etc/keepalived/keepalived.conf", "vrrp_instance lb {\n}\n")
	g.Stub("active", "haproxy\nkeepalived\n")
	g.Stub("socat", "#<PID>          <type>          <reloads>       <uptime>        <version>\n7               master          0 [failed: 0]   0d00h01m00s     3.2.4\n# workers\n9               worker          0               0d00h01m00s     3.2.4\n")
	g.Stub("ip", "1: lo    inet 127.0.0.1/8 scope host lo\n2: ten0    inet 10.77.0.5/24 brd 10.77.0.255 scope global ten0\n3: pub0    inet 203.0.113.10/24 scope global pub0\n")
	g.Listen("203.0.113.10:80", "203.0.113.10:443")
}

// udp gives the guest the UDP service as PushLVS leaves it, with these virtual servers ("203.0.113.10 53").
func udp(g *lbguesttest.Guest, servers ...string) {
	conf, table := "instance lvs\n", "IP Virtual Server version 1.2.1 (size=4096)\nProt LocalAddress:Port Scheduler Flags\n"
	for _, s := range servers {
		conf += "\nvirtual_server " + s + " {\n  protocol UDP\n  real_server 10.77.0.11 5000 {\n  }\n}\n"
		addr, port, _ := strings.Cut(s, " ")
		p, _ := strconv.Atoi(port)
		table += "UDP  " + lvsKey(netip.MustParseAddr(addr), p) + " wrr \n  -> 0A4D000B:1388      Masq    1      0          0\n"
	}
	g.Write(lvsConfig, conf)
	for _, f := range lvsFiles("ten0") {
		g.Write(f[0], f[1])
	}
	g.Write("/proc/net/ip_vs", table)
	g.Write("/proc/sys/net/ipv4/vs/conntrack", "1\n")
	g.Write("/proc/sys/net/ipv4/vs/snat_reroute", "1\n")
	g.Write("/proc/sys/net/ipv4/ip_forward", "1\n")
	g.Stub("nft", "table ip proxcloud-lb {\n\tchain lb-forward {\n\t\ttype filter hook forward priority filter; policy drop;\n\t}\n}\n")
	g.Stub("active", "haproxy\nkeepalived\n"+lvsService+"\n")
}

// The state read's script, run as the text it is, in the shell of the appliance's images (dash) and in bash: what it
// answers for the appliance that holds the address must not depend on whether the pair has a UDP listener. In
// v0.11.1-rc.1 it did (drill Q1-2): lvsserving()'s loop variable was v, the script's own answer to "holds the
// address", so with any virtual server the holder read as holding nothing, the pair stayed DEGRADED, no Service made
// after it became ready, the pushes went to the holder first and GET_HEALTH was empty. The fakes of the other tests
// answered the read from what it should say; this one runs it.
func TestVRRPStateScriptWithAndWithoutUDP(t *testing.T) {
	shells := lbguesttest.Shells()
	if len(shells) == 0 {
		t.Skip("no dash or bash on this machine")
	}
	const vip = "203.0.113.10"
	for _, shell := range shells {
		t.Run(shell, func(t *testing.T) {
			ctx := context.Background()
			ps, g, _ := scriptGuest(t, shell)
			read := func() VRRPState {
				t.Helper()
				st, err := ps.ReadVRRPState(ctx, "n1", 100, vip)
				if err != nil {
					t.Fatal(err)
				}
				return st
			}
			holder(g)
			want := VRRPState{State: "MASTER", HAProxy: true, HoldsVIP: true, Serving: true,
				HAProxySum: sha256Hex(scriptHAProxy), KeepalivedSum: sha256Hex("vrrp_instance lb {\n}\n"), LVSSum: "-"}
			if got := read(); got != want {
				t.Fatalf("the holder, without UDP: %+v, want %+v", got, want)
			}

			// One virtual server, then three: the read differs in the UDP words only.
			for _, servers := range [][]string{{vip + " 53"}, {vip + " 53", vip + " 5000", "203.0.113.11 53"}} {
				udp(g, servers...)
				config := ""
				for _, s := range servers {
					config += "\nvirtual_server " + s + " {\n  protocol UDP\n  real_server 10.77.0.11 5000 {\n  }\n}\n"
				}
				want.LVS, want.LVSSum = true, LVSHash("instance lvs\n"+config, "ten0")
				if got := read(); got != want {
					t.Fatalf("the holder, serving %d UDP listener(s): %+v, want %+v", len(servers), got, want)
				}
			}

			// The standby of the same pair: it holds nothing, with UDP as without.
			g.Write(lbStateFile, "BACKUP\n")
			g.Stub("ip", "1: lo    inet 127.0.0.1/8 scope host lo\n2: ten0    inet 10.77.0.6/24 brd 10.77.0.255 scope global ten0\n")
			standby := want
			standby.State, standby.HoldsVIP = "BACKUP", false
			if got := read(); got != standby {
				t.Fatalf("the standby, serving UDP: %+v, want %+v", got, standby)
			}
			holder(g)
			udp(g, vip+" 53", vip+" 5000", "203.0.113.11 53")

			// Each helper's "no" is still a no, and costs the other answers nothing: a listener HAProxy does not
			// have, a virtual server the table does not hold, an HAProxy without a worker, a stopped keepalived.
			g.Listen("203.0.113.10:80")
			if got := read(); got.Serving || !got.HoldsVIP || !got.LVS || !got.HAProxy {
				t.Fatalf("HAProxy lacks a listener: %+v", got)
			}
			g.Listen("203.0.113.10:80", "203.0.113.10:443")
			g.Write("/proc/net/ip_vs", "UDP  CB00710A:0035 wrr \n")
			if got := read(); got.LVS || !got.HoldsVIP || !got.Serving {
				t.Fatalf("the IPVS table lacks two virtual servers: %+v", got)
			}
			udp(g, vip+" 53")
			g.Unstub("socat")
			if got := read(); got.HAProxy || !got.HoldsVIP || !got.LVS {
				t.Fatalf("HAProxy's master does not answer: %+v", got)
			}
			holder(g)
			udp(g, vip+" 53")
			g.Stub("active", "haproxy\n"+lvsService+"\n")
			if got := read(); got.State != "STOPPED" || !got.HoldsVIP || !got.LVS {
				t.Fatalf("keepalived stopped: %+v", got)
			}
		})
	}
}

// What serving() and lvsserving() print when they say no is what a tenant reads in the balancer's message: it comes
// out of the subshell they now run in, with their answer.
func TestServingScriptsSayWhatIsMissing(t *testing.T) {
	for _, shell := range lbguesttest.Shells() {
		t.Run(shell, func(t *testing.T) {
			g := lbguesttest.New(t)
			holder(g)
			udp(g, "203.0.113.10 53")
			for _, c := range []struct {
				name, script string
				change       func()
				exit         int
				out          string
			}{
				{"serving", lbServingFn + "serving", func() {}, 0, ""},
				{"lvsserving", lvsServingFn + "lvsserving", func() {}, 0, ""},
				{"a listener is missing", lbServingFn + "serving", func() { g.Listen("203.0.113.10:80") }, 1, "HAProxy does not listen on 203.0.113.10:443\n"},
				{"a virtual server is missing", lvsServingFn + "lvsserving", func() { g.Write("/proc/net/ip_vs", "") }, 1, "the UDP listener 203.0.113.10:53 is not in the IPVS table\n"},
				{"forwarding is off", lvsServingFn + "lvsserving", func() { g.Write("/proc/sys/net/ipv4/ip_forward", "0\n") }, 1, "forwarding is off\n"},
				// A caller's variables stay the caller's, whatever a helper names its own.
				{"the caller's variables", lbServingFn + lbWorkerFn + lvsServingFn + "v=1 l=2 b=3 have=4 out=5; serving; lvsserving; worker; echo $v $l $b $have $out",
					func() {
						g.Write("/proc/sys/net/ipv4/ip_forward", "1\n")
						udp(g, "203.0.113.10 53")
						g.Listen("203.0.113.10:80", "203.0.113.10:443")
					}, 0, "1 2 3 4 5\n"},
			} {
				c.change()
				if exit, out, stderr := g.Run(shell, c.script); exit != c.exit || out != c.out {
					t.Errorf("%s: exit %d, said %q (stderr %q), want exit %d and %q", c.name, exit, out, stderr, c.exit, c.out)
				}
			}
		})
	}
}

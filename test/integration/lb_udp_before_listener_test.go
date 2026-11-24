//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// lbUDPLoop is a client that keeps ONE socket: a datagram to <address> <port> every 0.2 s from 198.51.100.7, and a
// line for each when its 0.2 s are over, "<the kernel's uptime at the send> <the answer, or ->". To IPVS and to
// connection tracking it is one flow that never goes silent, which is what a syslog, flow or metrics exporter or a
// VPN peer is, and what socat's one datagram per process (ask, flows) is not. perl-base has no Time::HiRes, so the
// clock is /proc/uptime, which the test reads too.
const lbUDPLoop = `cat > /usr/local/bin/lb-loop <<'EOF'
#!/usr/bin/perl
use Socket; use IO::Socket::INET; use IO::Select;
my $s = IO::Socket::INET->new(LocalAddr => "198.51.100.7", Proto => "udp") or die $!;
my $dst = sockaddr_in($ARGV[1], inet_aton($ARGV[0]));
my $sel = IO::Select->new($s);
sub up { open(my $f, "<", "/proc/uptime") or die $!; my ($u) = split " ", <$f>; $u }
$| = 1;
while (1) {
  my ($t, $a) = (up(), "-");
  $s->send("q", 0, $dst);
  while ((my $left = $t + 0.2 - up()) > 0) {
    if ($a ne "-") { select(undef, undef, undef, $left) }
    elsif ($sel->can_read($left)) { my $p = $s->recv(my $d, 2048); if (defined $p && $p eq $dst) { chomp $d; $a = $d } }
  }
  print "$t $a\n";
}
EOF
chmod 0755 /usr/local/bin/lb-loop`

// A UDP client that sends before its listener is there, in a real guest of each image: the distribution's systemd,
// keepalived and nftables, the host's kernel for IPVS and connection tracking, the product's own pushes, and the
// answers counted on the client's socket. Lab finding T2-F1 of v0.11.1-rc.3: a client that was sending from one
// source port when the virtual server of its port appeared on the appliance holding the address got 0 answers of
// 2584 datagrams in the 8 minutes after, while the balancer read ACTIVE, its members UP, and a new socket was
// answered. A datagram that met the address without a virtual server left a connection entry without the
// masquerade, and every later datagram of the flow refreshed it.
//
// The three ways in, each with its client sending first: a listener added to a balancer in use, the UDP unit
// restarted with nftables.service (and nftables.service reloaded), and an address that is on the appliance seconds
// before its virtual server (a Service made again on a kept address). Each client is answered within 5 s of the
// listener being served; a flow on a listener that was there first loses nothing when the listener is added, at a
// reload of the table and at a member change.
//
// The control is the last step: the same harness with the input chain taken out of the loaded table, which is the
// table of the releases before. The client is then not answered, a new socket is, and the state read says the
// table is not the product's.
func TestLBApplianceUDPClientBeforeListener(t *testing.T) {
	for _, image := range []string{"debian:trixie", "ubuntu:26.04"} {
		t.Run(image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
			defer cancel()
			const vip, vip2 = "203.0.113.10", "203.0.113.20"
			g, ps := lbUDPGuest(t, ctx, image, vip)
			diag := func() string {
				_, out, _ := g.sh(ctx, "diag", "sh", "-c", "ipvsadm -Ln; ipvsadm -Lnc | head -20; nft list ruleset; ip -4 addr; ip rule; sysctl net.ipv4.ip_forward; systemctl status proxcloud-lvs --no-pager -n 20; tail -n 5 /tmp/lb-loop-*.log")
				return out
			}
			// Every listener balances the members' one port: 5000.
			spec := func(weight int, ports ...int) pve.LBSpec {
				s := pve.LBSpec{Pools: []pve.LBPool{{Name: "udp", Protocol: "udp", Algorithm: "roundrobin", Members: []pve.LBMember{
					{ServerID: "101", Port: 5000, Weight: weight, Address: "10.77.0.11"}, {ServerID: "102", Port: 5000, Weight: 1, Address: "10.77.0.12"}}}}}
				for _, p := range ports {
					s.Listeners = append(s.Listeners, pve.LBListener{Port: p, Protocol: "udp", Pool: "udp"})
				}
				return s
			}
			// push answers once the kernel's table holds every virtual server of the config: "the listener is served".
			push := func(when string, s pve.LBSpec, vips ...string) {
				t.Helper()
				for deadline := time.Now().Add(5 * time.Minute); ; time.Sleep(time.Second) {
					err := ps.PushLVS(ctx, "n1", 100, pve.RenderLVS(s, vips), "ten0")
					if err == nil {
						return
					}
					if !errors.Is(err, pve.ErrLBNotReady) || time.Now().After(deadline) {
						t.Fatalf("%s: push: %v\n%s", when, err, diag())
					}
				}
			}
			state := func() pve.VRRPState {
				t.Helper()
				st, err := ps.ReadVRRPState(ctx, "n1", 100, vip)
				if err != nil {
					t.Fatal(err)
				}
				return st
			}
			now := func() float64 {
				t.Helper()
				u, err := strconv.ParseFloat(g.run(ctx, "cut -d' ' -f1 /proc/uptime"), 64)
				if err != nil {
					t.Fatal(err)
				}
				return u
			}
			// members waits until the kernel's table holds n members in all, and answers the uptime then.
			members := func(when string, n int) float64 {
				t.Helper()
				for start := time.Now(); ; time.Sleep(100 * time.Millisecond) {
					if got := g.run(ctx, "grep -c '^  -> [0-9A-F]\\{8\\}:' /proc/net/ip_vs || true"); got == strconv.Itoa(n) {
						return now()
					} else if time.Since(start) > 30*time.Second {
						t.Fatalf("%s: the table holds %s members after 30 s, want %d\n%s", when, got, n, diag())
					}
				}
			}
			start := func(name, addr string, port int) {
				t.Helper()
				g.run(ctx, fmt.Sprintf("systemd-run -q --unit=lb-loop-%s -p StandardOutput=file:/tmp/lb-loop-%s.log ip netns exec pub /usr/local/bin/lb-loop %s %d", name, name, addr, port))
			}
			// count is a client's datagrams sent in [from, to): how many, how many were answered, and how long after
			// from the first answered one was sent (-1: none). An answer is a member's, and names the address the
			// member was spoken to from: the appliance's own on the tenant network, or the path is not the product's.
			count := func(name string, from, to float64) (sent, answered int, first float64) {
				t.Helper()
				first = -1
				for _, line := range strings.Split(g.run(ctx, "cat /tmp/lb-loop-"+name+".log"), "\n") {
					at, answer, _ := strings.Cut(line, " ")
					u, err := strconv.ParseFloat(at, 64)
					if err != nil || u < from || u >= to {
						continue
					}
					sent++
					if answer == "-" {
						continue
					}
					if !strings.HasSuffix(answer, " 10.77.0.5") {
						t.Fatalf("client %s was answered %q: the member did not see the appliance's tenant address\n%s", name, answer, diag())
					}
					if answered++; first < 0 {
						first = u - from
					}
				}
				return sent, answered, first
			}
			const forever = 1e12
			// answeredWithin: the client gets an answer to a datagram it sent no later than 5 s after from.
			answeredWithin := func(when, name string, from float64) float64 {
				t.Helper()
				for {
					if _, _, first := count(name, from, forever); first >= 0 && first <= 5 {
						return first
					}
					if now() > from+6 {
						sent, answered, first := count(name, from, forever)
						t.Fatalf("%s: client %s sent %d datagrams and got %d answers (the first after %.1f s), want an answer within 5 s\n%s", when, name, sent, answered, first, diag())
					}
					time.Sleep(300 * time.Millisecond)
				}
			}
			lostNothing := func(when, name string, from, to float64) {
				t.Helper()
				if sent, answered, _ := count(name, from, to); sent < 5 || answered != sent {
					t.Fatalf("%s: client %s got %d answers to %d datagrams, want every one answered\n%s", when, name, answered, sent, diag())
				}
			}
			dropped := func() int {
				t.Helper()
				m := regexp.MustCompile(`ct state new counter packets (\d+)`).FindStringSubmatch(g.run(ctx, "nft list chain ip proxcloud-lb lb-input"))
				if m == nil {
					t.Fatalf("the input chain has no drop rule\n%s", diag())
				}
				n, _ := strconv.Atoi(m[1])
				return n
			}

			// 1. A balancer in use: one UDP listener, and a client on it that never stops.
			push("the first listener", spec(1, 5001), vip)
			g.run(ctx, lbUDPRoute)
			g.run(ctx, lbUDPLoop)
			members("the first listener", 2)
			start("first", vip, 5001)
			time.Sleep(2 * time.Second)
			_, _, wait := count("first", 0, forever)
			if wait < 0 {
				t.Fatalf("the client of the listener that was there first is not answered\n%s", diag())
			}
			firstFrom := wait + 0.001 // its first answered datagram (count's from was 0)
			if st := state(); !st.LVS || st.LVSDiffers {
				t.Fatalf("the state read after the first push: %+v, want UDP served by the product's table", st)
			}

			// 2. A listener added while its client sends. Before it: no answer, and each datagram dropped in the input
			// chain, so none left a connection entry.
			before := dropped()
			start("added", vip, 5000)
			time.Sleep(3 * time.Second)
			if sent, answered, _ := count("added", 0, forever); sent < 10 || answered != 0 || dropped()-before < sent-1 {
				t.Fatalf("before its listener the client sent %d datagrams, got %d answers, and the input chain dropped %d, want none answered and each dropped\n%s", sent, answered, dropped()-before, diag())
			}
			push("a listener added", spec(1, 5001, 5000), vip)
			served := now()
			took := answeredWithin("a listener added while its client sends", "added", served)
			addedFrom := served + took
			t.Logf("a listener added while its client sends: answered %.1f s after the push", took)

			// 3. A reload of the table and a member change, with both clients sending: nothing is lost, from the
			// first listener's first answer to here, the added listener's push included.
			g.run(ctx, "systemctl reload proxcloud-lvs")
			push("a member's weight", spec(2, 5001, 5000), vip)
			time.Sleep(time.Second)
			to := now()
			lostNothing("across a listener added, a reload of the table and a member change", "first", firstFrom, to)
			lostNothing("across a reload of the table and a member change", "added", addedFrom, to)

			// 4. nftables.service restarted: the UDP unit stops and starts with it, and both clients send through
			// it. Each is a client that sends before its virtual server is there again.
			g.run(ctx, "systemctl restart nftables")
			back := members("after a restart of nftables.service", 4)
			for _, name := range []string{"first", "added"} {
				took := answeredWithin("nftables.service restarted while its client sends", name, back)
				t.Logf("nftables.service restarted: client %s answered %.1f s after the members were back in the table", name, took)
			}
			// A reload of nftables.service empties the ruleset and the unit's reload loads the table again.
			g.run(ctx, "systemctl reload nftables")
			for start := time.Now(); g.run(ctx, "nft list chain ip proxcloud-lb lb-input 2>/dev/null | grep -c 'ct state new' || true") != "1"; time.Sleep(100 * time.Millisecond) {
				if time.Since(start) > 5*time.Second {
					t.Fatalf("5 s after a reload of nftables.service the table is not loaded\n%s", diag())
				}
			}
			back = now()
			for _, name := range []string{"first", "added"} {
				answeredWithin("nftables.service reloaded while its client sends", name, back)
			}

			// 5. An address that is on the appliance before its virtual server: the pair gets a second address
			// (keepalived's push), a client sends to it for 7 s, and only then the UDP push names it.
			start("second", vip2, 5000)
			keepalived, err := pve.RenderKeepalived(pve.LBKeepalived{RouterID: 42, Self: "10.77.0.5", Peer: "10.77.0.6", TenantIface: "ten0", PublicIface: "pub0",
				VIPs: []string{vip, vip2}, Prefix: 24, Gateway: "203.0.113.1"})
			if err != nil {
				t.Fatal(err)
			}
			if err := ps.PushKeepalived(ctx, "n1", 100, keepalived, "pub0"); err != nil {
				t.Fatalf("the second address: %v", err)
			}
			for start := time.Now(); g.run(ctx, "ip -4 -o addr show dev pub0 | grep -c ' "+vip2+"/' || true") != "1"; time.Sleep(200 * time.Millisecond) {
				if time.Since(start) > 30*time.Second {
					t.Fatalf("the appliance did not take the second address\n%s", diag())
				}
			}
			arrived := now()
			time.Sleep(7 * time.Second)
			if sent, answered, _ := count("second", arrived, forever); sent < 25 || answered != 0 {
				t.Fatalf("on an address without a virtual server the client sent %d datagrams and got %d answers, want 25 and more sent and none answered\n%s", sent, answered, diag())
			}
			push("the second address's listeners", spec(2, 5001, 5000), vip, vip2)
			served = now()
			took = answeredWithin("an address that came before its virtual server", "second", served)
			t.Logf("an address 7 s before its virtual server: answered %.1f s after the push", took)
			// The two clients of the first address through the address and its listeners coming.
			time.Sleep(time.Second)
			for _, name := range []string{"first", "added"} {
				lostNothing("across a second address and its listeners", name, back+5, now())
			}

			// 6. The control: the table of the releases before, which has no input chain. The state read says that the
			// table is not the product's, and a client that sends before its listener is caught: it is not answered
			// once the listener is served, while a new socket is. The push loads the product's table again, which
			// does not free a client that was caught before it.
			g.run(ctx, "nft delete chain ip proxcloud-lb lb-input")
			if st := state(); !st.LVS || !st.LVSDiffers {
				t.Fatalf("the state read of a table without the input chain: %+v, want UDP served by a table that differs", st)
			}
			start("control", vip, 5002)
			time.Sleep(2 * time.Second)
			push("the control's listener", spec(2, 5001, 5000, 5002), vip, vip2)
			served = now()
			if st := state(); !st.LVS || st.LVSDiffers {
				t.Fatalf("the state read after the push that followed: %+v, want the product's table again", st)
			}
			time.Sleep(lbUDPControlWatch)
			if sent, answered, _ := count("control", served, forever); sent < 25 || answered != 0 {
				t.Fatalf("control: without the input chain a client that sent before its listener got %d answers to %d datagrams in %s, want none: this guest does not show the finding\n%s", answered, sent, lbUDPControlWatch, diag())
			}
			_, out, _ := g.sh(ctx, "ask", "sh", "-c", "echo hi | ip netns exec pub socat -t1 - UDP4:"+vip+":5002,bind=198.51.100.7,shut-none")
			if !strings.HasSuffix(strings.TrimSpace(out), " 10.77.0.5") {
				t.Fatalf("control: a new socket on the caught client's port was answered %q, want a member's answer\n%s", out, diag())
			}
			g.run(ctx, "systemctl stop 'lb-loop-*'")
			t.Logf("%s; %s; kernel %s", g.run(ctx, "keepalived --version 2>&1 | head -1"), g.run(ctx, "nft --version"), g.run(ctx, "uname -r"))
		})
	}
}

// lbUDPControlWatch is how long the control's client is watched after its listener is served. The lab's client was
// not answered for 8 minutes, and this test's for 62 s on both images when the constant was that.
const lbUDPControlWatch = 8 * time.Second

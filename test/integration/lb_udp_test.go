//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
)

// lbUDPNet builds, around the guest, the networks an appliance of a pair sits in. The guest is the appliance: pub0 is
// its public NIC, with no address of its own, and ten0 its tenant NIC. Everything else is a network namespace:
//
//	pub   the public gateway 203.0.113.1, and behind it a client with an internet address, 198.51.100.7
//	sw    the tenant network's switch (a bridge)
//	m1    member 10.77.0.11, m2 member 10.77.0.12: on port 5000 a UDP and a TCP service that answer the member's name
//	      and the address it was spoken to from; their default route is the tenant gateway, not the appliance
//	gw    the tenant gateway 10.77.0.1, which routes, and counts every packet that reaches it
const lbUDPNet = `set -e
for n in pub sw m1 m2 gw; do ip netns add $n; ip -n $n link set lo up; done
ip link add pub0 type veth peer name p0 netns pub
ip link set pub0 up
ip -n pub link set p0 up
ip -n pub addr add 203.0.113.1/24 dev p0
ip -n pub addr add 198.51.100.7/32 dev lo
ip -n sw link add br0 type bridge
ip -n sw link set br0 up
ip link add ten0 type veth peer name s-app netns sw
ip addr add 10.77.0.5/24 dev ten0
ip link set ten0 up
ip -n sw link set s-app master br0 up
for m in m1:11 m2:12 gw:1; do
  n=${m%%:*}
  ip -n sw link add s-$n type veth peer name eth0 netns $n
  ip -n sw link set s-$n master br0 up
  ip -n $n addr add 10.77.0.${m##*:}/24 dev eth0
  ip -n $n link set eth0 up
done
ip -n m1 route add default via 10.77.0.1
ip -n m2 route add default via 10.77.0.1
ip netns exec gw sysctl -q -w net.ipv4.ip_forward=1
sysctl -q -w net.ipv4.ip_forward=0
printf '#!/bin/sh\necho "$1 $SOCAT_PEERADDR"\n' > /usr/local/bin/lb-echo
cat > /usr/local/bin/lb-udp-echo <<'EOF'
#!/usr/bin/perl
use Socket; use IO::Socket::INET;
my $s = IO::Socket::INET->new(LocalPort => 5000, Proto => "udp") or die $!;
while (my $peer = $s->recv(my $d, 2048)) { my ($port, $ip) = sockaddr_in($peer); $s->send("$ARGV[0] " . inet_ntoa($ip) . "\n", 0, $peer); }
EOF
chmod 0755 /usr/local/bin/lb-echo /usr/local/bin/lb-udp-echo
for n in m1 m2; do
  systemd-run -q --unit=lb-udp-$n ip netns exec $n /usr/local/bin/lb-udp-echo $n
  systemd-run -q --unit=lb-tcp-$n ip netns exec $n socat TCP4-LISTEN:5000,fork,reuseaddr SYSTEM:"/usr/local/bin/lb-echo $n"
done`

// lbSum is the sha256 of a config, as the appliance sums its file.
func lbSum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// UDP listeners on an appliance, in a real guest of each image an appliance is built from: the distribution's
// systemd, keepalived, nftables and HAProxy, the host's kernel for IPVS, connection tracking and routing, and the
// product's own calls through the guest agent API (PushKeepalived, PushPairHAProxy, PushLVS, ReadVRRPState,
// LVSHealth, RemoveLVS). Nothing of the guest is modelled. What it shows is in the steps; the stop conditions of
// the plan it holds are: a datagram to the address is answered through a member with the address as its source, the
// member sees the appliance's tenant address, nothing goes to the tenant gateway, a dead member leaves the table,
// and nothing sent from the public side to a tenant address is forwarded.
//
// Where the runner's kernel has no IPVS the test is skipped, the workflow's step writes a warning on the run, and the
// lab drill is the gate. Where that step loaded the module (LB_UDP_REQUIRE_IPVS) the test does not skip.
func TestLBApplianceUDP(t *testing.T) {
	for _, image := range []string{"debian:trixie", "ubuntu:26.04"} {
		t.Run(image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
			defer cancel()
			// iproute2 is on every image a VM boots from; socat here is the members' echo.
			c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
				ContainerRequest: testcontainers.ContainerRequest{
					Image:      image,
					Cmd:        []string{"sh", "-c", "apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq systemd-sysv procps iproute2 socat >/dev/null 2>&1 && rm -f /usr/sbin/policy-rc.d && exec /sbin/init"},
					Privileged: true,
					Tmpfs:      map[string]string{"/run": "", "/run/lock": ""},
				},
				Started: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Terminate(context.Background()) })
			g := &guestAgent{t: t, c: c, runs: map[int]*guestRun{}}
			for deadline := time.Now().Add(5 * time.Minute); ; time.Sleep(2 * time.Second) {
				if code, out, _ := g.sh(ctx, "boot", "systemctl", "is-system-running"); code == 0 || strings.TrimSpace(out) == "degraded" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("systemd did not come up in the container")
				}
			}
			srv := httptest.NewServer(g)
			defer srv.Close()
			ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
			if err != nil {
				t.Fatal(err)
			}
			g.run(ctx, lbUDPNet)

			const vip = "203.0.113.10"
			vips := []string{vip}
			keepalived, err := pve.RenderKeepalived(pve.LBKeepalived{RouterID: 42, Self: "10.77.0.5", Peer: "10.77.0.6", TenantIface: "ten0", PublicIface: "pub0",
				VIPs: vips, Prefix: 24, Gateway: "203.0.113.1"})
			if err != nil {
				t.Fatal(err)
			}
			members := []pve.LBMember{{ServerID: "101", Port: 5000, Weight: 1, Address: "10.77.0.11"}, {ServerID: "102", Port: 5000, Weight: 1, Address: "10.77.0.12"}}
			// TCP 5000 and UDP 5000 on one address: HAProxy has the first, LVS the second.
			spec := func(check *pve.LBHealthCheck, more ...pve.LBMember) pve.LBSpec {
				return pve.LBSpec{
					Listeners: []pve.LBListener{{Port: 5000, Protocol: "tcp", Pool: "tcp"}, {Port: 5000, Protocol: "udp", Pool: "udp"}},
					Pools: []pve.LBPool{{Name: "tcp", Protocol: "tcp", Algorithm: "roundrobin", Members: members},
						{Name: "udp", Protocol: "udp", Algorithm: "roundrobin", Members: append(members[:2:2], more...), HealthCheck: check}},
				}
			}
			tcpOnly := pve.LBSpec{Listeners: spec(nil).Listeners[:1], Pools: spec(nil).Pools[:1]}
			haproxy := pve.RenderHAProxyOn(spec(nil), vips)
			if haproxy != pve.RenderHAProxyOn(tcpOnly, vips) || strings.Contains(haproxy, "udp") {
				t.Fatalf("a UDP listener changed the HAProxy config:\n%s", haproxy)
			}
			if got := pve.RenderLVS(tcpOnly, vips); got != "" {
				t.Fatalf("a balancer without a UDP listener has an LVS config:\n%s", got)
			}
			lvs := pve.RenderLVS(spec(nil), vips)
			state := func() pve.VRRPState {
				t.Helper()
				st, err := ps.ReadVRRPState(ctx, "n1", 100, vip)
				if err != nil {
					t.Fatal(err)
				}
				return st
			}
			diag := func() string {
				_, out, _ := g.sh(ctx, "diag", "sh", "-c", "cat /proc/net/ip_vs; nft list ruleset; ip rule; ip route show table all | grep -v 'table local'; sysctl net.ipv4.ip_forward; systemctl status proxcloud-lvs --no-pager -n 30; journalctl -u keepalived --no-pager | tail -15")
				return out
			}

			// 1. The pair as it is without UDP, through the product's own install: nothing of the UDP service is on
			// the appliance, and the state read says what its two files hash to.
			for deadline := time.Now().Add(8 * time.Minute); ; time.Sleep(time.Second) {
				err := ps.PushKeepalived(ctx, "n1", 100, keepalived, "pub0")
				if err == nil {
					err = ps.PushPairHAProxy(ctx, "n1", 100, haproxy)
				}
				if err == nil {
					break
				}
				if !errors.Is(err, pve.ErrLBNotReady) || time.Now().After(deadline) {
					t.Fatalf("first push: %v", err)
				}
			}
			t.Logf("%s; %s; kernel %s", g.run(ctx, "haproxy -v | head -1"), g.run(ctx, "keepalived --version 2>&1 | head -1"), g.run(ctx, "uname -r"))
			for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(500 * time.Millisecond) {
				if st := state(); st.State == "MASTER" && st.HoldsVIP && st.Serving {
					break
				} else if time.Now().After(deadline) {
					t.Fatalf("the appliance did not take the address: %+v\n%s", st, diag())
				}
			}
			before := state()
			if before.HAProxySum != lbSum(haproxy) || before.KeepalivedSum != lbSum(keepalived) || before.LVSSum != "-" || before.LVS {
				t.Fatalf("a pair without UDP: %+v, want its two files' hashes %s and %s and nothing of LVS", before, lbSum(haproxy), lbSum(keepalived))
			}
			if got := g.run(ctx, "ls /etc/keepalived/lvs.conf /etc/proxcloud-lb /usr/local/sbin/proxcloud-lvs-net /etc/systemd/system/proxcloud-lvs.service 2>/dev/null | wc -l; cat /proc/sys/net/ipv4/ip_forward"); got != "0\n0" {
				t.Fatalf("a pair without UDP has files of the UDP service, or forwards: %q", got)
			}
			transitions := func() string { return g.run(ctx, "journalctl -u keepalived --no-pager | grep -c 'Entering' || true") }
			vrrpBefore := transitions()

			// The kernel: a container has no modules of its own, so the host loads ip_vs when its netlink family is
			// asked for (ipvsadm comes with keepalived on both images). Without IPVS on the host there is nothing to
			// test here.
			if code, _, _ := g.sh(ctx, "ipvs", "sh", "-c", "ipvsadm -Ln >/dev/null 2>&1; test -e /proc/sys/net/ipv4/vs/conntrack"); code != 0 {
				// go test shows nothing a passing package prints, so the skip is made visible by the workflow: its step
				// loads ip_vs and sets LB_UDP_REQUIRE_IPVS, and then there is no skipping; where the module cannot be
				// loaded that step writes the warning on the run.
				if os.Getenv("LB_UDP_REQUIRE_IPVS") != "" {
					t.Fatalf("ip_vs is loaded on this runner (LB_UDP_REQUIRE_IPVS) and the guest has no IPVS\n%s", diag())
				}
				t.Skip("the kernel has no IPVS (ip_vs): the lab drill is the gate for UDP listeners")
			}

			// 2. The first UDP listener: nftables through the product's own install line where the image lacks it,
			// the table before forwarding, and the IPVS table holding the listener before the push answers.
			for deadline := time.Now().Add(5 * time.Minute); ; time.Sleep(time.Second) {
				err := ps.PushLVS(ctx, "n1", 100, lvs, "ten0")
				if err == nil {
					break
				}
				if !errors.Is(err, pve.ErrLBNotReady) || time.Now().After(deadline) {
					t.Fatalf("first UDP push: %v\n%s", err, diag())
				}
			}
			t.Log(g.run(ctx, "nft --version"))
			if got := g.run(ctx, "awk '$1==\"UDP\"{print $2, $3}' /proc/net/ip_vs"); got != "CB00710A:1388 wrr" {
				t.Fatalf("the IPVS table after the push: %q\n%s", got, diag())
			}
			st := state()
			if !st.LVS || st.LVSSum != pve.LVSHash(lvs, "ten0") || st.HAProxySum != before.HAProxySum || st.KeepalivedSum != before.KeepalivedSum {
				t.Fatalf("after the first UDP push: %+v, want LVS serving with files %s and the other two files as they were", st, pve.LVSHash(lvs, "ten0"))
			}
			if got := g.run(ctx, "cat /proc/sys/net/ipv4/ip_forward /proc/sys/net/ipv4/vs/conntrack /proc/sys/net/ipv4/vs/snat_reroute /proc/sys/net/ipv4/conf/all/send_redirects /proc/sys/net/ipv4/conf/ten0/send_redirects | tr '\\n' ' '"); got != "1 1 1 0 0" {
				t.Fatalf("sysctls after the push: %q", got)
			}
			if got := g.run(ctx, "grep -l ip_forward /etc/sysctl.d/* /etc/sysctl.conf 2>/dev/null | grep -c proxcloud || true"); got != "0" {
				t.Fatalf("ip_forward sits in a sysctl.d file of the product's: the boot would set it before the table is loaded")
			}
			// From here the appliance routes as a VM does: its default route is the tenant gateway, and only the
			// rule of the address sends anything out of the public NIC.
			// A VM has spoken to its gateway and knows its MAC; this guest never did, and the gateway would not answer
			// an ARP request sent from the public address.
			g.run(ctx, "ip route replace default via 10.77.0.1 dev ten0 && ip neigh replace 10.77.0.1 lladdr $(ip netns exec gw cat /sys/class/net/eth0/address) dev ten0")
			g.run(ctx, `for n in gw m1 m2; do ip netns exec $n nft -f - <<'EOF'
table ip t {
  chain c {
    type filter hook prerouting priority 0;
    icmp type redirect counter comment "redirects"
    ip saddr { 203.0.113.0/24, 198.51.100.0/24 } counter comment "from-public"
    ip daddr { 203.0.113.0/24, 198.51.100.0/24 } counter comment "to-public"
  }
}
EOF
done`)
			counter := func(ns, table, chain, comment string) int {
				t.Helper()
				out := g.run(ctx, fmt.Sprintf("ip netns exec %s nft list chain %s %s", ns, table, chain))
				m := regexp.MustCompile(`counter packets (\d+) bytes \d+[^\n]*comment "` + comment + `"`).FindStringSubmatch(out)
				if m == nil {
					t.Fatalf("no counter %q in:\n%s", comment, out)
				}
				n, _ := strconv.Atoi(m[1])
				return n
			}
			// What reached the tenant gateway with a public address on it: an answer on its way out by the tenant
			// network, or a client's datagram that kept its source.
			routed := func() int { return counter("gw", "ip t", "c", "from-public") + counter("gw", "ip t", "c", "to-public") }
			// forwarded and dropped are the appliance's own counters, in the product's table.
			forward := func() (accepted, dropped int) {
				t.Helper()
				out := g.run(ctx, "nft list chain ip proxcloud-lb lb-forward")
				a := regexp.MustCompile(`established,related counter packets (\d+)`).FindStringSubmatch(out)
				d := regexp.MustCompile(`counter packets (\d+) bytes \d+ comment "dropped"`).FindStringSubmatch(out)
				if a == nil || d == nil {
					t.Fatalf("the forward chain:\n%s", out)
				}
				accepted, _ = strconv.Atoi(a[1])
				dropped, _ = strconv.Atoi(d[1])
				return accepted, dropped
			}
			// ask sends one datagram from the public side, from a socket connected to the destination: it takes an
			// answer only from that address and port. "" is no answer. (shut-none: socat would send an empty second
			// datagram when its input ends.)
			ask := func(from, to string) string {
				_, out, _ := g.sh(ctx, "ask", "sh", "-c", fmt.Sprintf("echo hi | ip netns exec pub socat -t0.7 - UDP4:%s,bind=%s,shut-none", to, from))
				return strings.TrimSpace(out)
			}
			health := func(s pve.LBSpec) string {
				t.Helper()
				hs, notLoaded, err := ps.LVSHealth(ctx, "n1", 100, s, vips)
				if err != nil || len(notLoaded) != 0 {
					t.Fatalf("UDP health: %q %v\n%s", notLoaded, err, diag())
				}
				var out []string
				for _, m := range hs {
					out = append(out, m.ServerID+" "+m.Status)
				}
				return strings.Join(out, ", ")
			}
			waitHealth := func(when string, s pve.LBSpec, want string, within time.Duration) time.Duration {
				t.Helper()
				start := time.Now()
				for health(s) != want {
					if time.Since(start) > within {
						t.Fatalf("%s: members are %q after %s, want %q\n%s", when, health(s), within, want, diag())
					}
					time.Sleep(500 * time.Millisecond)
				}
				return time.Since(start)
			}

			// 3. "alpha": a member is in the table after its first good check, not before.
			t.Logf("both members in the table %s after the push", waitHealth("after the first push", spec(nil), "101 UP, 102 UP", 30*time.Second))

			// 3a. Drill Q1-2 (v0.11.1-rc.1): while a UDP listener was served, the state read said of the appliance
			// that holds the address that it held nothing (lvsserving()'s loop variable was the script's answer), so
			// a pair's row read "neither appliance holds the address", its pushes went to the holder first and
			// GET_HEALTH asked nobody. This test read HoldsVIP before the first UDP push and after the last one only.
			// Here, with the listener served and both members in the table: the read still names the holder; a pass's
			// pushes go to the other appliance first (this guest's peer does not exist, so the standby's read is
			// written out: it holds nothing); and what GET_HEALTH asks the holder for, HAProxy's members and the
			// kernel table's, answers for both pools.
			holder := state()
			if holder.State != "MASTER" || !holder.HoldsVIP || !holder.HAProxy || !holder.Serving || !holder.LVS {
				t.Fatalf("with a UDP listener served, the appliance that holds %s reads: %+v\n%s", vip, holder, diag())
			}
			if got := g.run(ctx, "ip -o -4 addr show dev pub0 | grep -c ' "+vip+"/'"); got != "1" {
				t.Fatalf("the guest does not hold %s on its public NIC: the read above says nothing", vip)
			}
			standby := pve.VRRPState{State: "BACKUP", HAProxy: true, Serving: true, LVS: true, HAProxySum: holder.HAProxySum, KeepalivedSum: holder.KeepalivedSum, LVSSum: holder.LVSSum}
			if order := project.LBPushOrder([]pve.VRRPState{holder, standby}); !slices.Equal(order, []int{1, 0}) {
				t.Fatalf("with a UDP listener served, a pass pushes in the order %v of (holder, standby): want the standby first", order)
			}
			for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(500 * time.Millisecond) {
				hs, notLoaded, err := ps.LBHealth(ctx, "n1", 100, spec(nil))
				var got []string
				for _, m := range hs {
					got = append(got, m.Pool+" "+m.ServerID+" "+m.Status)
				}
				if err == nil && len(notLoaded) == 0 && slices.Equal(got, []string{"tcp 101 UP", "tcp 102 UP"}) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("GET_HEALTH of the holder, HAProxy's part: %q, not loaded %q, %v", got, notLoaded, err)
				}
			}
			if got := health(spec(nil)); got != "101 UP, 102 UP" {
				t.Fatalf("GET_HEALTH of the holder, the UDP pool: %q", got)
			}

			// 4. The reply path, for a client behind the public gateway. Each datagram comes from a new port, so
			// round robin takes the members in turn; the answer names the member and the address it saw.
			seen := map[string]int{}
			for i := range 6 {
				got := ask("198.51.100.7", vip+":5000")
				if got != "m1 10.77.0.5" && got != "m2 10.77.0.5" {
					t.Fatalf("datagram %d to the address: answer %q, want a member's, sent to the appliance's tenant address, with the address as its source\n%s", i+1, got, diag())
				}
				seen[got]++
			}
			if len(seen) != 2 {
				t.Fatalf("round robin used one member only: %v", seen)
			}
			if n, r1, r2 := routed(), counter("m1", "ip t", "c", "redirects"), counter("m2", "ip t", "c", "redirects"); n != 0 || r1 != 0 || r2 != 0 {
				t.Fatalf("%d packets with a public address reached the tenant gateway, and the members got %d and %d ICMP redirects: want none", n, r1, r2)
			}
			if n := counter("m1", "ip t", "c", "from-public") + counter("m2", "ip t", "c", "from-public"); n != 0 {
				t.Fatalf("the members saw %d packets with a public source: want the appliance's tenant address on every one", n)
			}
			if accepted, dropped := forward(); accepted < 6 || dropped != 0 {
				t.Fatalf("the forward chain let %d packets through and dropped %d: want the 6 answers through and nothing dropped", accepted, dropped)
			}
			// The control: without vs.snat_reroute the answer leaves by the tenant gateway (the spike saw it on the
			// wire), so the assertions above can fail.
			g.run(ctx, "sysctl -q -w net.ipv4.vs.snat_reroute=0")
			lost := ask("198.51.100.7", vip+":5000")
			if n := routed(); lost != "" || n == 0 {
				t.Fatalf("control: with vs.snat_reroute=0 the answer was %q and %d packets reached the tenant gateway: this network does not show a wrong reply path\n%s", lost, n, diag())
			}
			if st := state(); st.LVS {
				t.Fatalf("control: the state read calls an appliance that does not reroute its answers serving: %+v", st)
			}
			// The product puts it right: an appliance that does not serve is pushed again, and the reload sets it.
			if err := ps.PushLVS(ctx, "n1", 100, lvs, "ten0"); err != nil {
				t.Fatalf("push again: %v", err)
			}
			if got := ask("198.51.100.7", vip+":5000"); !strings.HasSuffix(got, " 10.77.0.5") {
				t.Fatalf("after the push that put snat_reroute back: %q\n%s", got, diag())
			}

			// 5. TCP on the same port goes through HAProxy, and is not tracked: the connection table stays flat. The
			// control, with the two notrack rules gone, tracks two entries a connection.
			tcp := func(n int) int {
				t.Helper()
				start := g.run(ctx, "cat /proc/sys/net/netfilter/nf_conntrack_count")
				g.run(ctx, fmt.Sprintf(`i=0; while [ $i -lt %d ]; do i=$((i+1)); r=$(ip netns exec pub socat -u TCP4:%s:5000,bind=198.51.100.7,connect-timeout=5 -); case "$r" in "m1 10.77.0.5"|"m2 10.77.0.5") ;; *) echo "request $i: $r"; exit 1 ;; esac; done`, n, vip))
				a, _ := strconv.Atoi(start)
				b, _ := strconv.Atoi(g.run(ctx, "cat /proc/sys/net/netfilter/nf_conntrack_count"))
				return b - a
			}
			if grew := tcp(40); grew > 8 {
				t.Fatalf("40 TCP requests through the address grew the connection table by %d entries: TCP is tracked\n%s", grew, diag())
			}
			g.run(ctx, "nft flush chain ip proxcloud-lb lb-prerouting && nft flush chain ip proxcloud-lb lb-output")
			if grew := tcp(40); grew < 40 {
				t.Fatalf("control: without the notrack rules 40 TCP requests grew the connection table by %d entries only", grew)
			}
			g.run(ctx, "systemctl reload proxcloud-lvs") // loads the table again, as every reload does
			if got := ask("198.51.100.7", vip+":5000"); !strings.HasSuffix(got, " 10.77.0.5") {
				t.Fatalf("UDP after a reload of the table: %q", got)
			}

			// 6. A dead member (its link goes down, as a VM that was stopped) leaves the table, by the ping beside
			// the UDP check, and comes back.
			g.run(ctx, "ip -n sw link set s-m2 down")
			took := waitHealth("a dead member", spec(nil), "101 UP, 102 DOWN", 25*time.Second)
			for i := range 4 {
				if got := ask("198.51.100.7", vip+":5000"); got != "m1 10.77.0.5" {
					t.Fatalf("datagram %d with one member dead: %q", i+1, got)
				}
			}
			g.run(ctx, "ip -n sw link set s-m2 up")
			t.Logf("a dead member left the table after %s and was back %s after its link", took, waitHealth("the member is back", spec(nil), "101 UP, 102 UP", 30*time.Second))

			// 7. A member change is a reload: the members that are up stay in the table, and a new member that does
			// not answer never enters it ("alpha"), so no datagram is lost right after the push (the spike lost 78
			// of 286 without it).
			third := spec(nil, pve.LBMember{ServerID: "103", Port: 5000, Weight: 1, Address: "10.77.0.13"})
			if err := ps.PushLVS(ctx, "n1", 100, pve.RenderLVS(third, vips), "ten0"); err != nil {
				t.Fatalf("push a third member: %v\n%s", err, diag())
			}
			for i := range 6 {
				if got := ask("198.51.100.7", vip+":5000"); !strings.HasSuffix(got, " 10.77.0.5") {
					t.Fatalf("datagram %d right after a member was added: %q, want an answer\n%s", i+1, got, diag())
				}
			}
			if got := health(third); got != "101 UP, 102 UP, 103 DOWN" {
				t.Fatalf("after a member that does not answer was added: %s", got)
			}
			// A check with a payload and a required reply: a member whose port closes, which the plain check does not
			// see under traffic, leaves the table.
			reply := spec(&pve.LBHealthCheck{Type: "udp-connect", Payload: "70696e67", RequireReply: true})
			if err := ps.PushLVS(ctx, "n1", 100, pve.RenderLVS(reply, vips), "ten0"); err != nil {
				t.Fatalf("push the reply check: %v\n%s", err, diag())
			}
			t.Logf("right after the check itself changed the members are %q; both up %s later", health(reply), waitHealth("the reply check", reply, "101 UP, 102 UP", 30*time.Second))
			g.run(ctx, "systemctl stop lb-udp-m1")
			took = waitHealth("a member whose port closed", reply, "101 DOWN, 102 UP", 25*time.Second)
			g.run(ctx, "systemd-run -q --unit=lb-udp-m1 ip netns exec m1 /usr/local/bin/lb-udp-echo m1")
			t.Logf("a member whose port closed left the table after %s and was back %s after it opened", took, waitHealth("the port is open again", reply, "101 UP, 102 UP", 30*time.Second))

			// 7b. A Kubernetes Service's UDP port as the portal makes it for externalTrafficPolicy Local and
			// sessionAffinity ClientIP (project.lbClusterServiceSpec; its golden is lvs-k8s-services.conf): the listener
			// is on the Service's own address, the members are nodes by address, and keepalived checks them over HTTP
			// on the health check node port, where kube-proxy answers 200 on a node that holds a pod of the Service
			// and 503 on one that holds none. Here m1 holds a pod and m2 none; then m2 gets one.
			g.run(ctx, `cat > /usr/local/bin/lb-hc <<'HC'
#!/bin/sh
while IFS= read -r l; do [ "$l" = "$(printf '\r')" ] && break; done
printf 'HTTP/1.1 %s kube-proxy\r\nContent-Length: 0\r\nConnection: close\r\n\r\n' "$1"
HC
chmod 0755 /usr/local/bin/lb-hc
systemd-run -q --unit=lb-hc-m1 ip netns exec m1 socat TCP4-LISTEN:32000,fork,reuseaddr SYSTEM:"/usr/local/bin/lb-hc 200"
systemd-run -q --unit=lb-hc-m2 ip netns exec m2 socat TCP4-LISTEN:32000,fork,reuseaddr SYSTEM:"/usr/local/bin/lb-hc 503"`)
			local := pve.LBSpec{
				Listeners: []pve.LBListener{{Port: 5000, Protocol: "udp", Pool: "s0-5000-udp", VIP: vip}},
				Pools: []pve.LBPool{{Name: "s0-5000-udp", Protocol: "udp", Algorithm: "roundrobin",
					HealthCheck:        &pve.LBHealthCheck{Type: "http", Port: 32000, Path: "/"},
					SessionPersistence: &pve.LBPersistence{Type: "source_ip", Timeout: 600},
					Members:            []pve.LBMember{{Node: true, Address: "10.77.0.11", Port: 5000, Weight: 1}, {Node: true, Address: "10.77.0.12", Port: 5000, Weight: 1}}}},
			}
			nodes := func() string {
				t.Helper()
				hs, notLoaded, err := ps.LVSHealth(ctx, "n1", 100, local, vips)
				if err != nil || len(notLoaded) != 0 {
					t.Fatalf("UDP health of the Service: %q %v\n%s", notLoaded, err, diag())
				}
				var out []string
				for _, m := range hs {
					out = append(out, m.Address+" "+m.Status)
				}
				return strings.Join(out, ", ")
			}
			waitNodes := func(when, want string) time.Duration {
				t.Helper()
				start := time.Now()
				for nodes() != want {
					if time.Since(start) > 40*time.Second {
						t.Fatalf("%s: nodes are %q after 40 s, want %q\n%s", when, nodes(), want, diag())
					}
					time.Sleep(500 * time.Millisecond)
				}
				return time.Since(start)
			}
			if err := ps.PushLVS(ctx, "n1", 100, pve.RenderLVS(local, vips), "ten0"); err != nil {
				t.Fatalf("push the Service's UDP port: %v\n%s", err, diag())
			}
			took = waitNodes("policy Local, a pod on m1 only", "10.77.0.11 UP, 10.77.0.12 DOWN")
			for i := range 4 {
				if got := ask("198.51.100.7", vip+":5000"); got != "m1 10.77.0.5" {
					t.Fatalf("policy Local, datagram %d: %q, want the node that holds the pod\n%s", i+1, got, diag())
				}
			}
			g.run(ctx, "systemctl stop lb-hc-m2; systemd-run -q --unit=lb-hc-m2b ip netns exec m2 socat TCP4-LISTEN:32000,fork,reuseaddr SYSTEM:'/usr/local/bin/lb-hc 200'")
			t.Logf("policy Local over UDP: the node with the pod alone was in the table after %s; the second node %s after its health check port answered 200",
				took, waitNodes("a pod on both nodes", "10.77.0.11 UP, 10.77.0.12 UP"))
			// The affinity: every datagram of one client address reaches one node, whatever port it comes from (step
			// 4 shows the members taken in turn without it). The first client stays where it was sent before the
			// second node came.
			for _, client := range []string{"198.51.100.7", "203.0.113.1"} {
				first := ask(client, vip+":5000")
				if !strings.HasSuffix(first, " 10.77.0.5") || (client == "198.51.100.7" && first != "m1 10.77.0.5") {
					t.Fatalf("affinity, client %s: first answer %q\n%s", client, first, diag())
				}
				for i := range 5 {
					if got := ask(client, vip+":5000"); got != first {
						t.Fatalf("affinity, client %s, datagram %d: %q, the first went to %q\n%s", client, i+2, got, first, diag())
					}
				}
			}
			g.run(ctx, "systemctl stop lb-hc-m1 lb-hc-m2b")
			if err := ps.PushLVS(ctx, "n1", 100, pve.RenderLVS(reply, vips), "ten0"); err != nil {
				t.Fatalf("push the reply check again: %v\n%s", err, diag())
			}
			waitHealth("back on the reply check", reply, "101 UP, 102 UP", 30*time.Second)

			// 8. The appliance is no router. From the public side, a datagram and a TCP connection for a member's own
			// address, handed to the appliance: dropped, counted, never seen by the member. The control, with the
			// chain's policy set to accept, serves the datagram.
			g.run(ctx, "ip -n pub route add 10.77.0.0/24 via "+vip)
			_, droppedBefore := forward()
			if got := ask("203.0.113.1", "10.77.0.11:5000"); got != "" {
				t.Fatalf("a datagram from the public side to a tenant address was answered: %q\n%s", got, diag())
			}
			if code, out, _ := g.sh(ctx, "fwd", "sh", "-c", "ip netns exec pub socat -u TCP4:10.77.0.11:5000,bind=203.0.113.1,connect-timeout=2 -"); code == 0 {
				t.Fatalf("a TCP connection from the public side to a tenant address was made: %q", out)
			}
			if _, dropped := forward(); dropped < droppedBefore+2 || counter("m1", "ip t", "c", "from-public") != 0 {
				t.Fatalf("the forward chain dropped %d packets (before: %d) and the member saw %d packets with a public source: want both attempts dropped and none seen",
					dropped, droppedBefore, counter("m1", "ip t", "c", "from-public"))
			}
			g.run(ctx, "nft 'add chain ip proxcloud-lb lb-forward { policy accept; }'")
			if got := ask("203.0.113.1", "10.77.0.11:5000"); got != "m1 10.77.0.5" {
				t.Fatalf("control: with the forward chain's policy set to accept the datagram was not served (%q): the step proves nothing\n%s", got, diag())
			}
			if st := state(); st.LVS {
				t.Fatalf("control: the state read calls an appliance whose forward chain accepts serving: %+v", st)
			}
			if err := ps.PushLVS(ctx, "n1", 100, pve.RenderLVS(reply, vips), "ten0"); err != nil {
				t.Fatalf("push again: %v", err)
			}
			if got := ask("203.0.113.1", "10.77.0.11:5000"); got != "" {
				t.Fatalf("after the push that loaded the table again, a datagram to a tenant address was answered: %q", got)
			}
			g.run(ctx, "ip -n pub route del 10.77.0.0/24")

			// 9. The files. What the agent wrote 0666 is root's, 0644, the script 0755; a file changed by hand shows
			// in the state read, which is what makes a pass push it again; a config keepalived refuses leaves the
			// running one, its file and its traffic.
			if err := ps.PushLVS(ctx, "n1", 100, lvs, "ten0"); err != nil {
				t.Fatalf("push: %v", err)
			}
			for path, mode := range map[string]string{"/etc/keepalived/lvs.conf": "644", "/etc/proxcloud-lb/lvs.nft": "644", "/etc/proxcloud-lb/lvs.sysctl": "644",
				"/etc/systemd/system/proxcloud-lvs.service": "644", "/usr/local/sbin/proxcloud-lvs-net": "755"} {
				if got := g.run(ctx, "stat -c %a:%U:%G "+path); got != mode+":root:root" {
					t.Errorf("%s is %s", path, got)
				}
			}
			for _, file := range []string{"/etc/keepalived/lvs.conf", "/etc/proxcloud-lb/lvs.nft"} {
				g.run(ctx, "cp "+file+" /root/kept && echo '# by hand' >> "+file)
				if st := state(); st.LVSSum == pve.LVSHash(lvs, "ten0") {
					t.Fatalf("%s was changed by hand and the state read gives the recorded hash", file)
				}
				g.run(ctx, "cat /root/kept > "+file)
			}
			if st := state(); !st.LVS || st.LVSSum != pve.LVSHash(lvs, "ten0") {
				t.Fatalf("the files as pushed: %+v", st)
			}
			err = ps.PushLVS(ctx, "n1", 100, lvs+"virtual_server 203.0.113.10 {\n", "ten0")
			if err == nil || errors.Is(err, pve.ErrLBNotReady) {
				t.Fatalf("a config keepalived cannot read was taken: %v", err)
			}
			if st := state(); !st.LVS || st.LVSSum != pve.LVSHash(lvs, "ten0") {
				t.Fatalf("after a refused config: %+v, want the last one in place and served", st)
			}
			if got := ask("198.51.100.7", vip+":5000"); !strings.HasSuffix(got, " 10.77.0.5") {
				t.Fatalf("after a refused config: %q", got)
			}

			// 10. Forwarding is never on without the chain. The unit stopped: forwarding off, table and IPVS servers
			// gone. A start that fails at the table (a broken file): forwarding stays off. The push repairs both.
			off := func(when string) {
				t.Helper()
				if got := g.run(ctx, "cat /proc/sys/net/ipv4/ip_forward; nft list table ip proxcloud-lb 2>/dev/null | wc -l; grep -c '^UDP' /proc/net/ip_vs || true"); got != "0\n0\n0" {
					t.Fatalf("%s: forwarding, lines of the table, UDP servers = %q, want all 0\n%s", when, got, diag())
				}
			}
			g.run(ctx, "systemctl stop proxcloud-lvs")
			off("the unit stopped")
			g.run(ctx, "echo 'table ip proxcloud-lb { chain fwd {' > /etc/proxcloud-lb/lvs.nft; systemctl start proxcloud-lvs || true")
			off("a start that failed at the table")
			if st := state(); st.LVS {
				t.Fatalf("a unit that did not start reads as serving: %+v", st)
			}
			if err := ps.PushLVS(ctx, "n1", 100, lvs, "ten0"); err != nil {
				t.Fatalf("push after a failed start: %v\n%s", err, diag())
			}
			waitHealth("after the repair", spec(nil), "101 UP, 102 UP", 30*time.Second)
			if got := ask("198.51.100.7", vip+":5000"); !strings.HasSuffix(got, " 10.77.0.5") {
				t.Fatalf("after the repair: %q\n%s", got, diag())
			}

			// 11. The last UDP listener goes: everything of the service leaves, TCP is served on, and is not tracked
			// (nothing tracks any more). The VRRP keepalived never noticed any of the steps above.
			if err := ps.RemoveLVS(ctx, "n1", 100); err != nil {
				t.Fatalf("remove: %v\n%s", err, diag())
			}
			off("after the removal")
			if got := g.run(ctx, "ls /etc/keepalived/lvs.conf* /etc/proxcloud-lb /usr/local/sbin/proxcloud-lvs-net /etc/systemd/system/proxcloud-lvs.service* 2>/dev/null | wc -l; systemctl is-enabled proxcloud-lvs 2>/dev/null || true"); got != "0" && got != "0\nnot-found" {
				t.Fatalf("after the removal: %q", got)
			}
			if got := ask("198.51.100.7", vip+":5000"); got != "" {
				t.Fatalf("a datagram after the removal was answered: %q", got)
			}
			if grew := tcp(20); grew > 8 {
				t.Fatalf("after the removal 20 TCP requests grew the connection table by %d entries", grew)
			}
			after := state()
			if after.State != "MASTER" || !after.HoldsVIP || !after.Serving || after.HAProxySum != before.HAProxySum || after.KeepalivedSum != before.KeepalivedSum || after.LVSSum != "-" {
				t.Fatalf("the pair after its UDP listeners came and went: %+v, before: %+v", after, before)
			}
			if got := transitions(); got != vrrpBefore {
				t.Fatalf("the VRRP keepalived logged %s state changes, %s before the UDP steps:\n%s", got, vrrpBefore, g.run(ctx, "journalctl -u keepalived --no-pager | grep Entering"))
			}
		})
	}
}

//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// lbUDPGuest is a real guest of image with the networks of lbUDPNet around it and the pair's own packages and
// keepalived on it, through the product's install: the appliance holds vip. The kernel is the host's; where it has no IPVS the test is skipped as TestLBApplianceUDP is.
func lbUDPGuest(t *testing.T, ctx context.Context, image, vip string) (*guestAgent, *pve.Service) {
	t.Helper()
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
	t.Cleanup(srv.Close)
	ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	g.run(ctx, lbUDPNet)
	keepalived, err := pve.RenderKeepalived(pve.LBKeepalived{RouterID: 42, Self: "10.77.0.5", Peer: "10.77.0.6", TenantIface: "ten0", PublicIface: "pub0",
		VIPs: []string{vip}, Prefix: 24, Gateway: "203.0.113.1"})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(8 * time.Minute); ; time.Sleep(time.Second) {
		err := ps.PushKeepalived(ctx, "n1", 100, keepalived, "pub0")
		if err == nil {
			break
		}
		if !errors.Is(err, pve.ErrLBNotReady) || time.Now().After(deadline) {
			t.Fatalf("first push: %v", err)
		}
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		if st, err := ps.ReadVRRPState(ctx, "n1", 100, vip); err == nil && st.State == "MASTER" && st.HoldsVIP {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("the appliance did not take the address: %+v %v", st, err)
		}
	}
	if code, _, _ := g.sh(ctx, "ipvs", "sh", "-c", "ipvsadm -Ln >/dev/null 2>&1; test -e /proc/sys/net/ipv4/vs/conntrack"); code != 0 {
		if os.Getenv("LB_UDP_REQUIRE_IPVS") != "" {
			t.Fatal("ip_vs is loaded on this runner (LB_UDP_REQUIRE_IPVS) and the guest has no IPVS")
		}
		t.Skip("the kernel has no IPVS (ip_vs): the lab drill is the gate for UDP listeners")
	}
	return g, ps
}

// lbUDPRoute makes the guest route as a VM does: its default route is the tenant gateway, and only the rule of the
// address sends anything out of the public NIC. A VM has spoken to its gateway and knows its MAC; this guest never
// did. It runs after the first UDP push, which may install nftables from the distribution's mirror.
const lbUDPRoute = "ip route replace default via 10.77.0.1 dev ten0 && ip neigh replace 10.77.0.1 lladdr $(ip netns exec gw cat /sys/class/net/eth0/address) dev ten0"

// A change of a UDP listener that stays, in a real guest of each image: the kernel's own table is read after every
// push, and datagrams are sent through it. Drill R3-F2 of v0.11.1-rc.2: a connection limit added to, raised on or
// removed from a listener was in lvs.conf on both appliances, the row read ACTIVE, and the kernel kept the figure of
// before: keepalived 2.3.x rewrites a real server at a reload only when its weight changed. A removed limit of 5
// kept refusing new flows (20 of 30 unanswered).
//
// Each step's control is in the step: what keepalived does with the same file when the product's edit is left out
// (the unit's own reload), and what the state read says of a table that differs.
func TestLBApplianceUDPChanges(t *testing.T) {
	for _, image := range []string{"debian:trixie", "ubuntu:26.04"} {
		t.Run(image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
			defer cancel()
			const vip = "203.0.113.10"
			vips := []string{vip}
			g, ps := lbUDPGuest(t, ctx, image, vip)
			diag := func() string {
				_, out, _ := g.sh(ctx, "diag", "sh", "-c", "ipvsadm -Ln; ipvsadm -Ln --thresholds; ipvsadm -Lnc | head -40; nft list ruleset; sysctl net.ipv4.ip_forward; systemctl status proxcloud-lvs --no-pager -n 30")
				return out
			}
			type opts struct {
				limit, weight1, persistence int
				algorithm                   string
			}
			spec := func(o opts) pve.LBSpec {
				p := pve.LBPool{Name: "udp", Protocol: "udp", Algorithm: "roundrobin", Members: []pve.LBMember{
					{ServerID: "101", Port: 5000, Weight: max(o.weight1, 1), Address: "10.77.0.11"}, {ServerID: "102", Port: 5000, Weight: 1, Address: "10.77.0.12"}}}
				if o.algorithm != "" {
					p.Algorithm = o.algorithm
				}
				if o.persistence > 0 {
					p.SessionPersistence = &pve.LBPersistence{Type: "source_ip", Timeout: o.persistence}
				}
				return pve.LBSpec{Listeners: []pve.LBListener{{Port: 5000, Protocol: "udp", Pool: "udp", ConnectionLimit: o.limit}}, Pools: []pve.LBPool{p}}
			}
			push := func(when string, o opts) {
				t.Helper()
				for deadline := time.Now().Add(5 * time.Minute); ; time.Sleep(time.Second) {
					err := ps.PushLVS(ctx, "n1", 100, pve.RenderLVS(spec(o), vips), "ten0")
					if err == nil {
						return
					}
					if !errors.Is(err, pve.ErrLBNotReady) || time.Now().After(deadline) {
						t.Fatalf("%s: push: %v\n%s", when, err, diag())
					}
				}
			}
			// kernel is the table as the kernel has it: the virtual server's scheduler and persistence, then each
			// member's weight and upper threshold, "wrr | 10.77.0.11:5000 w1 u0 | 10.77.0.12:5000 w1 u0".
			kernel := func() string {
				t.Helper()
				return g.run(ctx, `{ ipvsadm -Ln; echo ==; ipvsadm -Ln --thresholds; } | awk '$1=="=="{t=1} $1=="UDP"&&!t{$1=$2="";sub(/^ +/,"");printf "%s",$0} $1=="->"&&$2~/^[0-9]/{if(!t)w[$2]=$4;else printf " | %s w%s u%s",$2,w[$2],$3}'`)
			}
			serving := func() bool {
				t.Helper()
				st, err := ps.ReadVRRPState(ctx, "n1", 100, vip)
				if err != nil {
					t.Fatal(err)
				}
				return st.LVS
			}
			// expect: the kernel has it, and the state read calls it served.
			expect := func(when, want string) {
				t.Helper()
				if got := kernel(); got != want || !serving() {
					t.Fatalf("%s: the kernel has %q (the state read says serving=%v), want %q\n%s", when, got, serving(), want, diag())
				}
			}
			// flows sends one datagram from each of n new source ports at once and answers how many were answered by
			// a member. Each is a new flow to IPVS.
			port := 20000
			flows := func(n int) int {
				t.Helper()
				from := port
				port += n
				out := g.run(ctx, fmt.Sprintf(`for p in $(seq %d %d); do (echo hi | ip netns exec pub socat -t1 - UDP4:%s:5000,bind=198.51.100.7,sourceport=$p,shut-none) & done; wait`, from, port-1, vip))
				return strings.Count(out, " 10.77.0.5")
			}
			// held are four flows opened before any change: each keeps the member that answered it first, whatever
			// is changed on either member (an edit of a real server drops no flow, its own or another server's).
			held := map[int]string{}
			ask := func(sourceport int) string {
				_, out, _ := g.sh(ctx, "ask", "sh", "-c", fmt.Sprintf("echo hi | ip netns exec pub socat -t0.7 - UDP4:%s:5000,bind=198.51.100.7,sourceport=%d,shut-none", vip, sourceport))
				return strings.TrimSpace(out)
			}
			stillHeld := func(when string) {
				t.Helper()
				for p, first := range held {
					if got := ask(p); got != first {
						t.Fatalf("%s: the flow from port %d, opened before the changes and answered %q then, is answered %q now\n%s", when, p, first, got, diag())
					}
				}
			}

			// 1. No limit: every flow is answered.
			push("the first UDP push", opts{})
			g.run(ctx, lbUDPRoute)
			for deadline := time.Now().Add(30 * time.Second); kernel() != "wrr | 10.77.0.11:5000 w1 u0 | 10.77.0.12:5000 w1 u0"; time.Sleep(500 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("both members in the table: %q\n%s", kernel(), diag())
				}
			}
			expect("without a limit", "wrr | 10.77.0.11:5000 w1 u0 | 10.77.0.12:5000 w1 u0")
			for p := 19001; p <= 19004; p++ {
				held[p] = ask(p)
				if !strings.HasSuffix(held[p], " 10.77.0.5") {
					t.Fatalf("a flow before any change: %q\n%s", held[p], diag())
				}
			}
			if got := flows(8); got != 8 {
				t.Fatalf("without a limit %d of 8 flows were answered\n%s", got, diag())
			}

			// 2. A limit of 5 is added to the listener: the kernel has it on every member, and it holds: the twelve
			// flows above count, so of 20 new ones a member takes at most the one that puts it over.
			push("a limit of 5 added", opts{limit: 5})
			expect("a limit of 5 added", "wrr | 10.77.0.11:5000 w1 u5 | 10.77.0.12:5000 w1 u5")
			if got := flows(20); got > 10 {
				t.Fatalf("with a limit of 5 a member, %d of 20 new flows were answered: the limit does not hold\n%s", got, diag())
			}
			stillHeld("a limit of 5 added")

			// 3. Raised to 50: the kernel has it, and flows above the old limit are answered.
			push("the limit raised to 50", opts{limit: 50})
			expect("the limit raised to 50", "wrr | 10.77.0.11:5000 w1 u50 | 10.77.0.12:5000 w1 u50")
			if got := flows(20); got != 20 {
				t.Fatalf("with the limit raised to 50, %d of 20 new flows were answered\n%s", got, diag())
			}
			stillHeld("the limit raised to 50")
			// The control, which is the drill's finding: the file with a limit of 5 and the unit's own reload, which
			// is all a push did until v0.11.1-rc.2. keepalived leaves the kernel's 50, and the state read, which
			// said "serving" of it then, now says it is not: that is what makes a pass push again.
			g.run(ctx, "sed -i 's/uthreshold 50/uthreshold 5/' /etc/keepalived/lvs.conf && systemctl reload proxcloud-lvs && sleep 2")
			if got := kernel(); got != "wrr | 10.77.0.11:5000 w1 u50 | 10.77.0.12:5000 w1 u50" {
				t.Fatalf("control: keepalived's own reload wrote the changed limit to the kernel (%q): this keepalived does not show the finding, and the steps above prove nothing of the product's edit", got)
			}
			if serving() {
				t.Fatalf("control: the kernel keeps a limit of 50 where the file says 5, and the state read calls it served\n%s", diag())
			}
			// The push again that follows such a read: nothing of the files differs from what is on the appliance.
			push("pushed again after a read that says not served", opts{limit: 5})
			expect("pushed again", "wrr | 10.77.0.11:5000 w1 u5 | 10.77.0.12:5000 w1 u5")

			// 4. The limit removed: the kernel has none, and flows above the old limit are answered (the drill: 20
			// of 30 were not).
			push("the limit removed", opts{})
			expect("the limit removed", "wrr | 10.77.0.11:5000 w1 u0 | 10.77.0.12:5000 w1 u0")
			if got := flows(30); got != 30 {
				t.Fatalf("with the limit removed, %d of 30 new flows were answered\n%s", got, diag())
			}
			stillHeld("the limit removed")

			// 5. The same class, everything else the config can change on a server that stays: a member's weight,
			// the algorithm, the persistence timeout. keepalived writes these itself at a reload (both images); the
			// kernel is read after each all the same, and a limit set in the same change as a weight reaches the
			// member whose weight did not change too.
			for _, c := range []struct {
				when string
				o    opts
				want string
			}{
				{"a member's weight and a limit in one change", opts{weight1: 3, limit: 40}, "wrr | 10.77.0.11:5000 w3 u40 | 10.77.0.12:5000 w1 u40"},
				{"the algorithm", opts{weight1: 3, limit: 40, algorithm: "leastconn"}, "lc | 10.77.0.11:5000 w3 u40 | 10.77.0.12:5000 w1 u40"},
				{"persistence added", opts{weight1: 3, limit: 40, algorithm: "leastconn", persistence: 600}, "lc persistent 600 | 10.77.0.11:5000 w3 u40 | 10.77.0.12:5000 w1 u40"},
				{"the persistence timeout", opts{weight1: 3, limit: 40, algorithm: "source", persistence: 300}, "sh persistent 300 | 10.77.0.11:5000 w3 u40 | 10.77.0.12:5000 w1 u40"},
				{"all of it back", opts{}, "wrr | 10.77.0.11:5000 w1 u0 | 10.77.0.12:5000 w1 u0"},
			} {
				push(c.when, c.o)
				expect(c.when, c.want)
				stillHeld(c.when)
			}
			// A table changed by hand, each of the four: the state read says it is not served, and the push that
			// follows puts it back without a file to write.
			for _, by := range []string{
				"-E -u " + vip + ":5000 -s lc",
				"-E -u " + vip + ":5000 -s wrr -p 120",
				"-e -u " + vip + ":5000 -r 10.77.0.12:5000 -m -w 7",
				"-e -u " + vip + ":5000 -r 10.77.0.11:5000 -m -w 1 -x 3",
			} {
				g.run(ctx, "ipvsadm "+by)
				if serving() {
					t.Fatalf("after ipvsadm %s by hand the state read calls the table served: %q", by, kernel())
				}
				push("after ipvsadm "+by, opts{})
				expect("after ipvsadm "+by+" and a push", "wrr | 10.77.0.11:5000 w1 u0 | 10.77.0.12:5000 w1 u0")
			}
			stillHeld("after the table was changed by hand and put back")
			t.Logf("%s; %s; kernel %s", g.run(ctx, "keepalived --version 2>&1 | head -1"), g.run(ctx, "ipvsadm --version"), g.run(ctx, "uname -r"))
		})
	}
}

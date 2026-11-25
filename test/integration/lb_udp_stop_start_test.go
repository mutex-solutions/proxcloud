//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// lbUDPFlows is a set of clients that each keep ONE socket, as lb-loop does, in one process: lb-flows <address>
// <port>:<period>:<count>... sends from <count> sockets to <port> every <period> s, from 198.51.100.7. Every 0.25 s it
// writes a line a socket: "<the kernel's uptime> <socket> <sent> <answered> <answered wrongly>" for the 0.25 s before.
// An answer counts when it comes from the address and port the datagram went to and its member names the appliance's
// tenant address as the one it was spoken to from; any other is a wrong one.
const lbUDPFlows = `cat > /usr/local/bin/lb-flows <<'EOF'
#!/usr/bin/perl
use Socket; use IO::Socket::INET; use IO::Select;
my $addr = shift;
sub up { open(my $f, "<", "/proc/uptime") or die $!; my ($u) = split " ", <$f>; $u }
my $sel = IO::Select->new; my (@c, %by);
my $t = up();
for (@ARGV) {
  my ($port, $per, $n) = split /:/;
  for (1 .. $n) {
    my $s = IO::Socket::INET->new(LocalAddr => "198.51.100.7", Proto => "udp") or die $!;
    $sel->add($s);
    push @c, { s => $s, dst => scalar sockaddr_in($port, inet_aton($addr)), per => $per, next => $t + rand($per), sent => 0, ans => 0, bad => 0 };
    $by{fileno $s} = $c[-1];
  }
}
$| = 1;
my $report = $t + 0.25;
while (1) {
  $t = up();
  for my $c (@c) {
    next if $t < $c->{next};
    $c->{s}->send("q", 0, $c->{dst}); $c->{sent}++;
    $c->{next} += $c->{per}; $c->{next} = $t if $c->{next} < $t - 1;
  }
  if ($t >= $report) {
    for my $i (0 .. $#c) { my $c = $c[$i]; print "$t $i $c->{sent} $c->{ans} $c->{bad}\n"; @$c{qw(sent ans bad)} = (0, 0, 0); }
    $report += 0.25;
  }
  for my $fh ($sel->can_read(0.005)) {
    my $c = $by{fileno $fh};
    my $p = $fh->recv(my $d, 2048);
    next unless defined $p;
    chomp $d;
    $p eq $c->{dst} && $d =~ / 10\.77\.0\.5$/ ? $c->{ans}++ : $c->{bad}++;
  }
}
EOF
chmod 0755 /usr/local/bin/lb-flows`

// lbUDPStopStartCycles is how often each event is looped in a guest. The defect is a race of some milliseconds: one
// stop and start caught one client in ten at 5 datagrams a second, so one try proves nothing.
const lbUDPStopStartCycles = 30

// lbUDPEmptyConntrack empties the guest's connection table, as a boot does: the message "conntrack -F" sends.
const lbUDPEmptyConntrack = `perl -e 'socket(S,16,3,12) && send(S,pack("LSSLLCCn",20,258,5,1,0,0,0,0),0,pack("SSLL",16,0,0,0)) && defined recv(S,$r,4096,0) or exit 1; exit(unpack("x16l",$r) == 0 ? 0 : 1)'`

// No UDP client is left unanswered when the UDP service stops and starts while it sends, in a real guest of each
// image: the distribution's systemd, keepalived and nftables, the host's kernel for IPVS and connection tracking,
// the product's own pushes, and the answers counted on each client's socket. CI on the release PR of v0.11.1: a
// client that sent through a restart of nftables.service was not answered for as long as it kept sending, while the
// balancer read ACTIVE and its members UP. The unit's stop deletes the nftables table, connection tracking stays on
// for some milliseconds more, and a datagram in that moment is tracked without the masquerade; a second, rarer
// moment sits inside the table's load at a start. Every later datagram of the client refreshes that entry.
//
// Each event is looped lbUDPStopStartCycles times with four clients at 5 datagrams a second and four at 50, each on
// one socket that never goes silent, and ONE client without an answer within 5 s of the members being back fails
// the test:
//
//   - the balancer's last UDP listener removed and UDP listeners added again (RemoveLVS, PushLVS);
//   - the unit's keepalived killed, which systemd answers with the unit's stop and start. The main process alone:
//     "systemctl kill" also kills the unit's stop script, and nothing of the stop then happens;
//   - nftables.service restarted by hand;
//   - the unit started with clients already sending and an empty connection table, as at a boot (PushLVS on a
//     stopped unit), with 32 more clients at 50 a second: this moment is far shorter than the stop's.
//
// The control is the last step: the same guest with a perl that fails in the unit's path, so the script's flush of
// the connection table fails. The unit starts all the same and its journal says that the table was not emptied;
// restarts of nftables.service then leave a client unanswered for 5 s and more while a new socket is answered, and
// the first restart with the flush back answers that client again.
func TestLBApplianceUDPStopStart(t *testing.T) {
	for _, image := range []string{"debian:trixie", "ubuntu:26.04"} {
		t.Run(image, func(t *testing.T) {
			t.Parallel() // the two guests side by side: the loops are most of the test's time
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()
			const vip = "203.0.113.10"
			g, ps := lbUDPGuest(t, ctx, image, vip)
			diag := func() string {
				_, out, _ := g.sh(ctx, "diag", "sh", "-c", "ipvsadm -Ln; ipvsadm -Lnc | head -20; nft list ruleset; sysctl net.ipv4.ip_forward net.netfilter.nf_conntrack_count; systemctl status proxcloud-lvs --no-pager -n 30; tail -n 16 /tmp/lb-flows.log")
				return out
			}
			spec := pve.LBSpec{Pools: []pve.LBPool{{Name: "udp", Protocol: "udp", Algorithm: "roundrobin", Members: []pve.LBMember{
				{ServerID: "101", Port: 5000, Weight: 1, Address: "10.77.0.11"}, {ServerID: "102", Port: 5000, Weight: 1, Address: "10.77.0.12"}}}},
				Listeners: []pve.LBListener{{Port: 5000, Protocol: "udp", Pool: "udp"}, {Port: 5001, Protocol: "udp", Pool: "udp"}}}
			push := func(when string) {
				t.Helper()
				for deadline := time.Now().Add(5 * time.Minute); ; time.Sleep(time.Second) {
					err := ps.PushLVS(ctx, "n1", 100, pve.RenderLVS(spec, []string{vip}), "ten0")
					if err == nil {
						return
					}
					if !errors.Is(err, pve.ErrLBNotReady) || time.Now().After(deadline) {
						t.Fatalf("%s: push: %v\n%s", when, err, diag())
					}
				}
			}
			now := func() float64 {
				t.Helper()
				u, err := strconv.ParseFloat(g.run(ctx, "cut -d' ' -f1 /proc/uptime"), 64)
				if err != nil {
					t.Fatal(err)
				}
				return u
			}
			// back waits until the kernel's table holds both members of both listeners, and answers the uptime then.
			back := func(when string) float64 {
				t.Helper()
				for start := time.Now(); ; time.Sleep(50 * time.Millisecond) {
					if got := g.run(ctx, "grep -c '^  -> [0-9A-F]\\{8\\}:' /proc/net/ip_vs || true"); got == "4" {
						return now()
					} else if time.Since(start) > 30*time.Second {
						t.Fatalf("%s: the table holds %s members after 30 s, want 4\n%s", when, got, diag())
					}
				}
			}
			// unanswered are the sockets of a log that sent in [from, to) and got no answer to anything they sent
			// then; a wrong answer at any time fails the test.
			unanswered := func(when, log string, from, to float64) []string {
				t.Helper()
				var none []string
				for _, line := range strings.Split(g.run(ctx, fmt.Sprintf("awk -v from=%f -v to=%f '$5 > 0 { bad[$2] += $5 } $1 - 0.25 >= from && $1 < to { s[$2] += $3; a[$2] += $4 } END { for (i in s) print i, s[i], a[i], bad[i] + 0 }' %s", from, to, log)), "\n") {
					f := strings.Fields(line)
					if len(f) != 4 {
						continue
					}
					if f[3] != "0" {
						t.Fatalf("%s: socket %s of %s got %s answers that were not a member's through the appliance's tenant address and from the balancer's address\n%s", when, f[0], log, f[3], diag())
					}
					if sent, _ := strconv.Atoi(f[1]); sent >= 2 && f[2] == "0" {
						none = append(none, f[0])
					}
				}
				return none
			}
			// stuck are the sockets that are not answered within 5 s of the members being back at uptime at. A look
			// after 1 s (three reports and more: three datagrams of the slowest client) lets every cycle but a
			// failing one go on.
			stuck := func(when string, at float64, logs ...string) []string {
				t.Helper()
				var all []string
				for _, watch := range []float64{1, 5.5} {
					for now() < at+watch {
						time.Sleep(100 * time.Millisecond)
					}
					all = nil
					for _, log := range logs {
						for _, s := range unanswered(when, log, at, at+watch) {
							all = append(all, log+" socket "+s)
						}
					}
					if len(all) == 0 {
						break
					}
				}
				return all
			}
			journal := func(what string) int {
				t.Helper()
				n, _ := strconv.Atoi(g.run(ctx, "journalctl -u proxcloud-lvs --no-pager -o cat | grep -c '"+what+"' || true"))
				return n
			}
			const emptied, notEmptied = "the connection table was emptied", "the connection table could not be emptied"

			push("the first listeners")
			g.run(ctx, lbUDPRoute)
			g.run(ctx, lbUDPFlows)
			at := back("the first listeners")
			const flows, swarm = "/tmp/lb-flows.log", "/tmp/lb-swarm.log"
			g.run(ctx, "systemd-run -q --unit=lb-flows -p StandardOutput=file:"+flows+" ip netns exec pub /usr/local/bin/lb-flows "+vip+" 5000:0.2:2 5001:0.2:2 5000:0.02:2 5001:0.02:2")
			if s := stuck("before any event", at+1, flows); len(s) != 0 {
				t.Fatalf("before any event the clients are not answered: %v\n%s", s, diag())
			}

			events := []struct {
				name string
				do   func()
			}{
				{"the last UDP listener removed and UDP listeners added again", func() {
					if err := ps.RemoveLVS(ctx, "n1", 100); err != nil {
						t.Fatalf("remove: %v\n%s", err, diag())
					}
					push("listeners added again")
				}},
				{"the UDP service's keepalived killed", func() {
					pid := g.run(ctx, "systemctl show -p MainPID --value proxcloud-lvs")
					g.run(ctx, "kill -KILL "+pid)
					for start := time.Now(); ; time.Sleep(100 * time.Millisecond) {
						if got := g.run(ctx, "systemctl is-active -q proxcloud-lvs && systemctl show -p MainPID --value proxcloud-lvs || true"); got != "" && got != "0" && got != pid {
							return
						} else if time.Since(start) > 20*time.Second {
							t.Fatalf("20 s after its keepalived was killed systemd has not started the UDP service again\n%s", diag())
						}
					}
				}},
				{"nftables.service restarted", func() { g.run(ctx, "systemctl restart nftables") }},
				{"the UDP service started with clients already sending", func() {
					g.run(ctx, "systemctl stop proxcloud-lvs; sleep 0.3; "+lbUDPEmptyConntrack)
					push("the stopped unit")
				}},
			}
			for n, e := range events {
				logs := []string{flows}
				if n == len(events)-1 {
					g.run(ctx, "systemd-run -q --unit=lb-swarm -p StandardOutput=file:"+swarm+" ip netns exec pub /usr/local/bin/lb-flows "+vip+" 5000:0.02:16 5001:0.02:16")
					logs = append(logs, swarm)
				}
				began := time.Now()
				for i := 1; i <= lbUDPStopStartCycles; i++ {
					e.do()
					if s := stuck(e.name, back(e.name), logs...); len(s) != 0 {
						t.Fatalf("%s, cycle %d of %d: not answered within 5 s of the members being back: %v\n%s", e.name, i, lbUDPStopStartCycles, s, diag())
					}
				}
				// Every cycle of every event starts the unit without its table: the script says so each time, and
				// never that it could not empty the connection table.
				if got, want := journal(emptied), (n+1)*lbUDPStopStartCycles; got < want || journal(notEmptied) != 0 {
					t.Fatalf("%s: the unit's journal says %d times that the connection table was emptied, want %d and more, and %d times that it could not be\n%s", e.name, got, want, journal(notEmptied), diag())
				}
				t.Logf("%s: %d cycles in %s, no client unanswered", e.name, lbUDPStopStartCycles, time.Since(began).Round(time.Second))
			}
			g.run(ctx, "systemctl stop lb-swarm")

			// The control: the flush fails. The unit's path has /usr/local/sbin first; the clients name /usr/bin/perl.
			g.run(ctx, "printf '#!/bin/sh\\nexit 1\\n' > /usr/local/sbin/perl && chmod 0755 /usr/local/sbin/perl")
			var caught []string
			cycles := 0
			for cycles < 20 && len(caught) == 0 {
				cycles++
				g.run(ctx, "systemctl restart nftables")
				at = back("the control")
				caught = stuck("the control", at, flows)
			}
			if journal(notEmptied) != cycles {
				t.Fatalf("control: %d restarts with a flush that fails, and the unit's journal says %d times that the connection table could not be emptied\n%s", cycles, journal(notEmptied), diag())
			}
			if st, err := ps.ReadVRRPState(ctx, "n1", 100, vip); err != nil || !st.LVS || st.LVSDiffers {
				t.Fatalf("control: the state read with a flush that fails: %+v %v, want UDP served: the unit starts all the same", st, err)
			}
			if len(caught) == 0 {
				t.Fatalf("control: %d restarts of nftables.service without the flush left no client unanswered: this guest does not show the defect\n%s", cycles, diag())
			}
			_, out, _ := g.sh(ctx, "ask", "sh", "-c", "echo hi | ip netns exec pub socat -t1 - UDP4:"+vip+":5000,bind=198.51.100.7,shut-none")
			if !strings.HasSuffix(strings.TrimSpace(out), " 10.77.0.5") {
				t.Fatalf("control: a new socket was answered %q while %v are not, want a member's answer\n%s", out, caught, diag())
			}
			g.run(ctx, "rm /usr/local/sbin/perl && systemctl restart nftables")
			if s := stuck("the control, the flush back", back("the control, the flush back"), flows); len(s) != 0 {
				t.Fatalf("control: with the flush back a restart of nftables.service still leaves %v unanswered\n%s", s, diag())
			}
			t.Logf("control: without the flush %d restart(s) of nftables.service left %v unanswered for 5 s; the next restart, with it, answered them", cycles, caught)
			g.run(ctx, "systemctl stop lb-flows")
			t.Logf("%s; %s; kernel %s", g.run(ctx, "keepalived --version 2>&1 | head -1"), g.run(ctx, "nft --version"), g.run(ctx, "uname -r"))
		})
	}
}

//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve/lbcerttest"
)

// lbLoneNet wires the guest. The guest itself is the first appliance: pub0 its public NIC, ten0 its tenant NIC
// (10.77.0.5).
//
//	pub   the public network: a bridge with the gateway's address 203.0.113.1, which is also the client's
//	sw    the tenant network's switch (a bridge)
//	m1    member 10.77.0.11: on port 5000 a TCP and a UDP service that answer its name and who spoke to it
//	peer  the second appliance, 10.77.0.6, with a tenant and a public NIC of its own and nothing running yet
const lbLoneNet = `set -e
for n in pub sw m1 peer; do ip netns add $n; ip -n $n link set lo up; done
ip -n pub link add br0 type bridge
ip -n pub link set br0 up
ip -n pub addr add 203.0.113.1/24 dev br0
ip -n sw link add br0 type bridge
ip -n sw link set br0 up
ip link add pub0 type veth peer name p-a netns pub
ip -n pub link set p-a master br0 up
ip link add ten0 type veth peer name s-a netns sw
ip addr add 10.77.0.5/24 dev ten0
ip link set ten0 up
ip -n sw link set s-a master br0 up
ip -n peer link add pub0 type veth peer name p-b netns pub
ip -n peer link set pub0 up
ip -n pub link set p-b master br0 up
ip -n peer link add ten0 type veth peer name s-b netns sw
ip -n peer addr add 10.77.0.6/24 dev ten0
ip -n peer link set ten0 up
ip -n sw link set s-b master br0 up
ip -n sw link add s-m1 type veth peer name eth0 netns m1
ip -n sw link set s-m1 master br0 up
ip -n m1 addr add 10.77.0.11/24 dev eth0
ip -n m1 link set eth0 up
printf '#!/bin/sh\necho "$1 $SOCAT_PEERADDR"\n' > /usr/local/bin/lb-echo
cat > /usr/local/bin/lb-udp-echo <<'EOF'
#!/usr/bin/perl
use Socket; use IO::Socket::INET;
my $s = IO::Socket::INET->new(LocalPort => 5000, Proto => "udp") or die $!;
while (my $peer = $s->recv(my $d, 2048)) { my ($port, $ip) = sockaddr_in($peer); $s->send("$ARGV[0] " . inet_ntoa($ip) . "\n", 0, $peer); }
EOF
printf '#!/bin/sh\nexit 0\n' > /usr/local/sbin/peer-check
printf '#!/bin/sh\necho "$3" > /run/peer.state\n' > /usr/local/sbin/peer-notify
chmod 0755 /usr/local/bin/lb-echo /usr/local/bin/lb-udp-echo /usr/local/sbin/peer-check /usr/local/sbin/peer-notify
systemd-run -q --unit=lb-udp-m1 ip netns exec m1 /usr/local/bin/lb-udp-echo m1
systemd-run -q --unit=lb-tcp-m1 ip netns exec m1 socat TCP4-LISTEN:5000,fork,reuseaddr SYSTEM:"/usr/local/bin/lb-echo m1"`

// The first appliance of a pair serves before the second exists (drill Q0b-1 of v0.11.1: on the lab the address of
// four Services answered nobody for 6 min 50 s after it was shown, until the second appliance had joined). In a
// real guest of each image an appliance is built from: the distribution's systemd, keepalived, HAProxy and OpenSSL,
// the host's kernel, and the product's own calls through the guest agent API. Nothing of the first appliance is
// modelled. The second one is a keepalived of the same package in a network namespace of the same guest, with a
// tenant and a public NIC of its own, run on the config the product renders for it (its two script paths changed:
// the two share a filesystem).
//
// Shown, in order: the control, HAProxy on an appliance without keepalived refuses the pair's config, as the lab
// saw; with the config of an appliance that has no peer it becomes master, holds the address, and a client on the
// public network is answered over TCP, over TLS with the certificate that was pushed, and over UDP (where the
// kernel has IPVS); no VRRP packet leaves it meanwhile; the control of the order, a peer started before the first
// appliance was told of it takes the address too; in the product's order the first appliance is told, reloads and
// stays master with the address untouched, the peer starts as its standby, and a client that asks ten times a
// second is answered every time; and the pair is one: when keepalived stops on the first, the peer takes the
// address.
func TestLBFirstApplianceServesAlone(t *testing.T) {
	for _, image := range []string{"debian:trixie", "ubuntu:26.04"} {
		t.Run(image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
			defer cancel()
			// socat and openssl are the members and the client, tcpdump the wire; nftables is what the UDP service's
			// install line would bring.
			c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
				ContainerRequest: testcontainers.ContainerRequest{
					Image:      image,
					Cmd:        []string{"sh", "-c", "apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq systemd-sysv procps iproute2 socat openssl tcpdump nftables >/dev/null 2>&1 && rm -f /usr/sbin/policy-rc.d && exec /sbin/init"},
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
			g.run(ctx, lbLoneNet)

			const vip = "203.0.113.10"
			vips := []string{vip}
			side := func(self, peer string) string {
				t.Helper()
				conf, err := pve.RenderKeepalived(pve.LBKeepalived{RouterID: 42, Self: self, Peer: peer, TenantIface: "ten0", PublicIface: "pub0",
					VIPs: vips, Prefix: 24, Gateway: "203.0.113.1"})
				if err != nil {
					t.Fatal(err)
				}
				return conf
			}
			alone, paired := side("10.77.0.5", ""), side("10.77.0.5", "10.77.0.6")
			info, bundle, warnings, refused := func() (pve.LBCertificate, string, []string, string) {
				crt := lbcerttest.New(t, lbcerttest.RSA(t, 2048), time.Now().Add(72*time.Hour), "a.example.org")
				return pve.CheckLBCertificate(crt.Chain, crt.Key, time.Now())
			}()
			if refused != "" || len(warnings) != 0 {
				t.Fatalf("certificate: %q %q", refused, warnings)
			}
			cert := pve.LBCertFile{Name: info.File, PEM: bundle}
			member := []pve.LBMember{{ServerID: "101", Port: 5000, Weight: 1, Address: "10.77.0.11"}}
			spec := pve.LBSpec{
				Listeners: []pve.LBListener{{Port: 5000, Protocol: "tcp", Pool: "tcp"},
					{Port: 8443, Protocol: "tls", Pool: "tcp", Certificates: []string{"a"}, CertFiles: []string{cert.Name}},
					{Port: 5000, Protocol: "udp", Pool: "udp"}},
				Pools: []pve.LBPool{{Name: "tcp", Protocol: "tcp", Algorithm: "roundrobin", Members: member},
					{Name: "udp", Protocol: "udp", Algorithm: "roundrobin", Members: member}},
			}
			haproxy := pve.RenderHAProxyOn(spec, vips)
			state := func() pve.VRRPState {
				t.Helper()
				st, err := ps.ReadVRRPState(ctx, "n1", 100, vip)
				if err != nil {
					t.Fatal(err)
				}
				return st
			}
			diag := func() string {
				_, out, _ := g.sh(ctx, "diag", "sh", "-c", "ip -o -4 addr; ip -n peer -o -4 addr; cat /run/proxcloud-lb.state /run/peer.state; ip -n pub neigh; journalctl -u keepalived -u haproxy --no-pager | tail -30")
				return out
			}
			// What a client on the public network hears: "" is no answer.
			tcp := func() string {
				_, out, _ := g.sh(ctx, "tcp", "sh", "-c", "ip netns exec pub socat -t2 - TCP4:"+vip+":5000,connect-timeout=2 </dev/null")
				return strings.TrimSpace(out)
			}
			const answer = "m1 10.77.0.5" // the member's name, and the appliance's tenant address as the one that spoke to it
			answered := func(when string, within time.Duration) {
				t.Helper()
				for deadline := time.Now().Add(within); tcp() != answer; time.Sleep(300 * time.Millisecond) {
					if time.Now().After(deadline) {
						t.Fatalf("%s: a client on the public network hears %q from %s:5000, want %q\n%s", when, tcp(), vip, answer, diag())
					}
				}
			}
			// vrrpOnTheWire counts the VRRP packets the appliance puts on its tenant NIC in four seconds.
			vrrpOnTheWire := func() (n int, others string) {
				t.Helper()
				out := g.run(ctx, "timeout 4 tcpdump -nli ten0 'ip proto 112 or arp' 2>/dev/null || true")
				for _, line := range strings.Split(out, "\n") {
					switch {
					case strings.Contains(line, "VRRP"):
						n++
					case strings.TrimSpace(line) != "" && !strings.Contains(others, line[strings.IndexByte(line, ' ')+1:]):
						others += line[strings.IndexByte(line, ' ')+1:] + "; "
					}
				}
				return n, others
			}

			// 1. The control, the lab's finding in a real guest: the pair's HAProxy config on an appliance keepalived
			// was never pushed to. HAProxy cannot bind an address the guest does not hold, and refuses.
			for deadline := time.Now().Add(8 * time.Minute); ; time.Sleep(time.Second) {
				err := ps.PushPairHAProxy(ctx, "n1", 100, haproxy, cert)
				if err != nil && !errors.Is(err, pve.ErrLBNotReady) {
					if !strings.Contains(err.Error(), "cannot bind socket") {
						t.Fatalf("HAProxy without keepalived: %v, want it to refuse the bind", err)
					}
					break
				}
				if err == nil || time.Now().After(deadline) {
					t.Fatalf("HAProxy without keepalived took the pair's config, or the appliance never became ready: %v", err)
				}
			}
			t.Logf("%s; %s; kernel %s", g.run(ctx, "haproxy -v | head -1"), g.run(ctx, "keepalived --version 2>&1 | head -1"), g.run(ctx, "uname -r"))

			// 2. Alone, in the order of an apply: keepalived with no peer, then HAProxy with its certificate.
			if err := ps.PushKeepalived(ctx, "n1", 100, alone, "pub0"); err != nil {
				t.Fatalf("keepalived with no peer: %v", err)
			}
			if err := ps.PushPairHAProxy(ctx, "n1", 100, haproxy, cert); err != nil {
				t.Fatalf("HAProxy on the appliance that is alone: %v\n%s", err, diag())
			}
			for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(500 * time.Millisecond) {
				if st := state(); st.State == "MASTER" && st.HoldsVIP && st.Serving {
					break
				} else if time.Now().After(deadline) {
					t.Fatalf("the appliance did not take the address: %+v\n%s", st, diag())
				}
			}
			if st := state(); st.HAProxySum != lbSum(haproxy) || st.KeepalivedSum != lbSum(alone) {
				t.Fatalf("the files on the appliance: %+v", st)
			}
			answered("alone", 15*time.Second)
			if got := g.run(ctx, "echo hi | ip netns exec pub openssl s_client -quiet -connect "+vip+":8443 -servername a.example.org 2>/dev/null"); got != answer {
				t.Fatalf("over TLS: %q, want %q\n%s", got, answer, diag())
			}
			if got := g.run(ctx, "ip netns exec pub openssl s_client -connect "+vip+":8443 -servername a.example.org </dev/null 2>/dev/null | openssl x509 -noout -ext subjectAltName"); !strings.Contains(got, "a.example.org") {
				t.Fatalf("the certificate a client is shown: %q", got)
			}
			if n, others := vrrpOnTheWire(); n != 0 {
				t.Fatalf("an appliance with no peer put %d VRRP packets on the tenant network", n)
			} else {
				t.Logf("alone: no VRRP packet on the tenant NIC in 4 s; what was: %s", others)
			}

			// 3. UDP: the second keepalived fills the kernel's table, and the address is on this appliance, so a
			// datagram to it is answered.
			if code, _, _ := g.sh(ctx, "ipvs", "sh", "-c", "ipvsadm -Ln >/dev/null 2>&1; test -e /proc/sys/net/ipv4/vs/conntrack"); code != 0 {
				if os.Getenv("LB_UDP_REQUIRE_IPVS") != "" {
					t.Fatalf("ip_vs is loaded on this runner (LB_UDP_REQUIRE_IPVS) and the guest has no IPVS\n%s", diag())
				}
				t.Log("the kernel has no IPVS (ip_vs): UDP on an appliance that is alone is not shown here")
			} else {
				lvs := pve.RenderLVS(spec, vips)
				if err := ps.PushLVS(ctx, "n1", 100, lvs, "ten0"); err != nil {
					t.Fatalf("the UDP service on the appliance that is alone: %v\n%s", err, diag())
				}
				got := ""
				for deadline := time.Now().Add(30 * time.Second); got != answer && time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
					_, out, _ := g.sh(ctx, "udp", "sh", "-c", "echo hi | ip netns exec pub socat -t0.7 - UDP4:"+vip+":5000,shut-none")
					got = strings.TrimSpace(out)
				}
				// HoldsVIP with a UDP listener: the state read lost it to a variable of the UDP check (found by this test).
				if st := state(); got != answer || !st.LVS || st.LVSSum != pve.LVSHash(lvs, "ten0") || !st.HoldsVIP || st.State != "MASTER" {
					t.Fatalf("a datagram to the address: %q, want %q; state %+v, want the UDP service running and the address held\n%s", got, answer, st, diag())
				}
			}

			// The peer's keepalived: the product's config for it, with the scripts of a second guest.
			peerConf := strings.NewReplacer("/usr/local/sbin/proxcloud-lb-check", "/usr/local/sbin/peer-check",
				"/usr/local/sbin/proxcloud-lb-notify", "/usr/local/sbin/peer-notify").Replace(side("10.77.0.6", "10.77.0.5"))
			if err := c.CopyToContainer(ctx, []byte(peerConf), "/root/peer.conf", 0o644); err != nil {
				t.Fatal(err)
			}
			const startPeer = "ip netns exec peer keepalived -f /root/peer.conf -p /run/peer.pid -r /run/peer-vrrp.pid -c /run/peer-checkers.pid"
			peer := func() (role string, holds bool) {
				_, out, _ := g.sh(ctx, "peer", "sh", "-c", "cat /run/peer.state 2>/dev/null; ip -n peer -o -4 addr show dev pub0 | grep -c ' "+vip+"/'")
				f := strings.Fields(out)
				if len(f) != 2 {
					return "", false
				}
				return f[0], f[1] == "1"
			}

			// 4. The control of the order: the peer started while the first appliance still has no peer. It hears
			// nothing, and takes the address too.
			g.run(ctx, startPeer)
			for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(500 * time.Millisecond) {
				if role, holds := peer(); role == "MASTER" && holds && state().HoldsVIP {
					break
				} else if time.Now().After(deadline) {
					t.Fatalf("a peer the first appliance does not know of is %q (holds the address: %v), the first appliance %+v: the control wants two masters\n%s", role, holds, state(), diag())
				}
			}
			g.run(ctx, "kill $(cat /run/peer.pid); n=0; while [ -e /run/peer.pid ] && [ $n -lt 50 ]; do n=$((n+1)); sleep 0.1; done; rm -f /run/peer.state")
			if _, holds := peer(); holds {
				t.Fatalf("the peer kept the address after its keepalived stopped\n%s", diag())
			}
			answered("after the control", 20*time.Second) // the first appliance announces the address again within garp_master_refresh

			// 5. The product's order: the appliance that serves is told of its peer first. It reloads, stays master,
			// and the address never leaves its NIC; the peer then starts as its standby. A client asks ten times a
			// second throughout.
			entered := func() string { return g.run(ctx, "journalctl -u keepalived --no-pager | grep -c 'Entering' || true") }
			before := entered()
			g.run(ctx, `systemd-run -q --unit=lb-addr-watch sh -c 'exec ip monitor address dev pub0 > /root/addr.log 2>&1'
systemd-run -q --unit=lb-client sh -c 'while :; do r=$(ip netns exec pub socat -t1 - TCP4:`+vip+`:5000,connect-timeout=1 </dev/null 2>/dev/null); [ "$r" = "`+answer+`" ] && echo ok || echo "fail $r"; sleep 0.1; done > /root/client.log'
sleep 1`)
			if err := ps.PushKeepalived(ctx, "n1", 100, paired, "pub0"); err != nil {
				t.Fatalf("keepalived with its peer: %v\n%s", err, diag())
			}
			g.run(ctx, startPeer)
			for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(500 * time.Millisecond) {
				if role, _ := peer(); role == "BACKUP" {
					break
				} else if time.Now().After(deadline) {
					t.Fatalf("the peer is %q, want BACKUP\n%s", role, diag())
				}
			}
			time.Sleep(6 * time.Second) // longer than the 3.6 s after which a standby that hears nothing takes over
			g.run(ctx, "systemctl stop lb-client lb-addr-watch")
			role, holds := peer()
			if st := state(); st.State != "MASTER" || !st.HoldsVIP || !st.Serving || st.KeepalivedSum != lbSum(paired) || role != "BACKUP" || holds {
				t.Fatalf("after the peer joined: first appliance %+v, peer %q (holds the address: %v)\n%s", st, role, holds, diag())
			}
			if after := entered(); after != before {
				t.Fatalf("keepalived on the first appliance changed state while the peer joined: %s transitions before, %s after\n%s", before, after, diag())
			}
			if moved := g.run(ctx, "cat /root/addr.log"); moved != "" {
				t.Fatalf("an address of the public NIC changed while the peer joined:\n%s", moved)
			}
			ok, _ := strconv.Atoi(g.run(ctx, "grep -c '^ok' /root/client.log || true"))
			if failed := g.run(ctx, "grep -v '^ok' /root/client.log || true"); failed != "" || ok < 30 {
				t.Fatalf("the client while the peer joined: %d answers, and these were not:\n%s", ok, failed)
			}
			n, _ := vrrpOnTheWire()
			if n == 0 {
				t.Fatalf("no VRRP packet on the tenant NIC with a peer: the count of step 2 proves nothing\n%s", diag())
			}
			t.Logf("the peer joined: %d answers of %d, no address change, no state change; %d adverts to the peer in 4 s", ok, ok, n)

			// 6. And it is a pair: keepalived stops on the first appliance, the peer takes the address.
			g.run(ctx, "systemctl stop keepalived")
			for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(300 * time.Millisecond) {
				if role, holds := peer(); role == "MASTER" && holds {
					break
				} else if time.Now().After(deadline) {
					t.Fatalf("the peer did not take the address: %q, holds %v\n%s", role, holds, diag())
				}
			}
			if st := state(); st.HoldsVIP {
				t.Fatalf("the first appliance still holds the address: %+v", st)
			}
		})
	}
}

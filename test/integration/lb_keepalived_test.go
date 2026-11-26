//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// The configs of a load balancer pair must pass `haproxy -c` and `keepalived -t` as Debian 13 (2.3.3) and Ubuntu 26.04
// (2.3.4) ship it, with no VIP, one and fifty (spike S1: an instance with every VIP excluded, or a rule without a
// preference, fails there). The container has one interface: the tenant side is eth0 and the public side lo; -t
// checks the syntax and the values, not the wiring.
func TestLBPairConfigsPassTheirChecks(t *testing.T) {
	for _, image := range []string{"debian:trixie", "ubuntu:26.04"} {
		t.Run(image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()
			c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
				ContainerRequest: testcontainers.ContainerRequest{Image: lbImage(image), Cmd: []string{"sleep", "600"}},
				Started:          true,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Terminate(context.Background()) })
			run := func(cmd string) string {
				t.Helper()
				code, r, err := c.Exec(ctx, []string{"sh", "-c", cmd}, tcexec.Multiplexed())
				var out bytes.Buffer
				if r != nil {
					_, _ = io.Copy(&out, r)
				}
				if err != nil || code != 0 {
					t.Fatalf("%s: exit %d %v\n%s", cmd, code, err, out.String())
				}
				return out.String()
			}
			run(lbInstall("keepalived", "haproxy"))
			t.Log(run("keepalived --version 2>&1 | head -1"))
			// The scripts keepalived runs must exist, or -t disables the track script and fails.
			run("printf '#!/bin/sh\\n' > /usr/local/sbin/proxcloud-lb-check && printf '#!/bin/sh\\n' > /usr/local/sbin/proxcloud-lb-notify && chmod 0755 /usr/local/sbin/proxcloud-lb-*")
			// The control: keepalived -t here does refuse what the spike saw refused, a rule with no preference.
			bad, _ := pve.RenderKeepalived(pve.LBKeepalived{RouterID: 42, Self: "10.0.0.5", Peer: "10.0.0.6",
				TenantIface: "eth0", PublicIface: "lo", VIPs: []string{"203.0.113.10"}, Prefix: 24, Gateway: "203.0.113.1"})
			if err := c.CopyToContainer(ctx, []byte(strings.ReplaceAll(bad, " preference 100", "")), "/tmp/bad.conf", 0o644); err != nil {
				t.Fatal(err)
			}
			if code, _, err := c.Exec(ctx, []string{"keepalived", "-t", "-f", "/tmp/bad.conf"}); err != nil || code == 0 {
				t.Fatalf("keepalived -t took a rule with no preference: exit %d %v", code, err)
			}
			// And the HAProxy config both appliances run, with the pair's binds, an HTTP check on its own port and PROXY v2.
			spec := pve.LBSpec{
				Listeners: []pve.LBListener{{Port: 80, Pool: "web"}, {Port: 5432, Pool: "db"}},
				Pools: []pve.LBPool{
					{Name: "web", Protocol: "http", Algorithm: "roundrobin", HealthCheck: &pve.LBHealthCheck{Type: "http", Port: 10256, Path: "/healthz"},
						Members: []pve.LBMember{{ServerID: "101", Port: 8080, Weight: 1, Address: "10.0.0.11"}}},
					{Name: "db", Protocol: "tcp", Algorithm: "leastconn", ProxyProtocol: true,
						Members: []pve.LBMember{{ServerID: "104", Port: 5432, Weight: 1, Address: "10.0.0.14"}}},
				},
			}
			if err := c.CopyToContainer(ctx, []byte(pve.RenderHAProxyOn(spec, []string{"203.0.113.10", "203.0.113.11"})), "/tmp/h.cfg", 0o644); err != nil {
				t.Fatal(err)
			}
			run("mkdir -p /run/haproxy && haproxy -c -f /tmp/h.cfg")
			// A cluster's: one address per Service, two Services on port 80, workers by address, an hour's idle timeout.
			workers := []pve.LBMember{{Node: true, Address: "10.0.0.250", Port: 30080}, {Node: true, Address: "10.0.0.249", Port: 30080}}
			cluster := pve.LBSpec{
				Listeners: []pve.LBListener{{Port: 80, Pool: "s0-0", VIP: "203.0.113.10"}, {Port: 80, Pool: "s1-0", VIP: "203.0.113.11", SourceRanges: []string{"198.51.100.0/24"}}},
				Pools: []pve.LBPool{{Name: "s0-0", Protocol: "tcp", Algorithm: "roundrobin", Members: workers},
					{Name: "s1-0", Protocol: "tcp", Algorithm: "roundrobin", ProxyProtocol: true, Members: workers,
						HealthCheck: &pve.LBHealthCheck{Type: "http", Port: 32000, Path: "/"}}},
			}
			if err := c.CopyToContainer(ctx, []byte(pve.RenderClusterHAProxy(cluster, []string{"203.0.113.10", "203.0.113.11"})), "/tmp/c.cfg", 0o644); err != nil {
				t.Fatal(err)
			}
			run("haproxy -c -f /tmp/c.cfg")
			// Every config the unit tests hold a golden of, the layer 4 options among them: no error and, with -dW,
			// no warning either, from the HAProxy this image ships.
			goldens, err := filepath.Glob("../../internal/cloud/pve/testdata/lb*.cfg")
			if err != nil || len(goldens) < 6 {
				t.Fatalf("goldens: %q %v", goldens, err)
			}
			for _, g := range goldens {
				cfg, err := os.ReadFile(g)
				if err != nil {
					t.Fatal(err)
				}
				if err := c.CopyToContainer(ctx, cfg, "/tmp/golden.cfg", 0o644); err != nil {
					t.Fatal(err)
				}
				if code, r, err := c.Exec(ctx, []string{"haproxy", "-dW", "-c", "-f", "/tmp/golden.cfg"}); err != nil || code != 0 {
					var out bytes.Buffer
					if r != nil {
						_, _ = io.Copy(&out, r)
					}
					t.Errorf("%s: haproxy -dW -c exit %d %v\n%s", filepath.Base(g), code, err, out.String())
				}
			}
			lbLayer4Behaves(ctx, t, c, run)
			for _, n := range []int{0, 1, 50} {
				var vips []string
				for i := range n {
					vips = append(vips, fmt.Sprintf("203.0.113.%d", 10+i))
				}
				conf, err := pve.RenderKeepalived(pve.LBKeepalived{RouterID: 42, Self: "10.0.0.5", Peer: "10.0.0.6",
					TenantIface: "eth0", PublicIface: "lo", VIPs: vips, Prefix: 24, Gateway: "203.0.113.1"})
				if err != nil {
					t.Fatal(err)
				}
				if err := c.CopyToContainer(ctx, []byte(conf), "/tmp/k.conf", 0o644); err != nil {
					t.Fatal(err)
				}
				code, r, err := c.Exec(ctx, []string{"keepalived", "-t", "-f", "/tmp/k.conf"})
				var out bytes.Buffer
				if r != nil {
					_, _ = io.Copy(&out, r)
				}
				if err != nil || code != 0 {
					t.Errorf("%d VIPs: keepalived -t exit %d %v\n%s\n%s", n, code, err, out.String(), conf)
				} else if s := out.String(); strings.Contains(s, "WARNING") {
					t.Logf("%d VIPs: %s", n, s)
				}
			}
			// The config of the second keepalived, the one that fills the kernel's IPVS table for UDP listeners
			// (pve.RenderLVS): every golden of the unit tests is read by this image's keepalived without an error and
			// without a keyword it does not know. The control: the same file with a server that names no port.
			lvsGoldens, err := filepath.Glob("../../internal/cloud/pve/testdata/lvs-*.conf")
			if err != nil || len(lvsGoldens) < 3 {
				t.Fatalf("LVS goldens: %q %v", lvsGoldens, err)
			}
			check := func(conf []byte) (int, string) {
				t.Helper()
				if err := c.CopyToContainer(ctx, conf, "/tmp/lvs.conf", 0o644); err != nil {
					t.Fatal(err)
				}
				code, r, err := c.Exec(ctx, []string{"keepalived", "-t", "-f", "/tmp/lvs.conf"}, tcexec.Multiplexed())
				if err != nil {
					t.Fatal(err)
				}
				var out bytes.Buffer
				if r != nil {
					_, _ = io.Copy(&out, r)
				}
				return code, out.String()
			}
			for _, g := range lvsGoldens {
				conf, err := os.ReadFile(g)
				if err != nil {
					t.Fatal(err)
				}
				if code, out := check(conf); code != 0 || strings.Contains(out, "Unknown keyword") || strings.Contains(out, "nknown") {
					t.Errorf("%s: keepalived -t exit %d\n%s", filepath.Base(g), code, out)
				}
				if code, out := check(append(conf, "virtual_server 203.0.113.10 {\n"...)); code == 0 {
					t.Errorf("%s with a server that names no port: keepalived -t took it\n%s", filepath.Base(g), out)
				}
			}
		})
	}
}

// lbLayer4Behaves runs the image's HAProxy on a rendered config and asks it what the layer 4 options do, with members
// in the same container: two plain ones that answer their name, one that keeps the first line it is sent, and two
// that speak TLS (openssl s_server, one of them TLS 1.3 only). Shown: a PROXY v1 header reaches the member; source
// persistence keeps a client on one member where round robin uses both; an HTTPS check and a TLS hello check take a
// TLS member for up and a plain one for down; a pool without a check reads "no check" and serves, whatever the state
// saved before the reload says of its member; a listener's connection limit is the frontend's.
func lbLayer4Behaves(ctx context.Context, t *testing.T, c testcontainers.Container, run func(string) string) {
	t.Helper()
	run(lbInstall("openssl", "socat"))
	run("openssl req -x509 -newkey rsa:2048 -nodes -keyout /tmp/k.pem -out /tmp/c.pem -days 1 -subj /CN=member >/dev/null 2>&1")
	run(`printf '#!/bin/sh\nhead -n1 >> /tmp/$1.in\necho $1\n' > /tmp/answer && chmod 0755 /tmp/answer`)
	for name, port := range map[string]int{"a": 9081, "b": 9082, "c": 9083} {
		run(fmt.Sprintf(`setsid socat TCP-LISTEN:%d,fork,reuseaddr SYSTEM:"/tmp/answer %s" >/dev/null 2>&1 &`, port, name))
	}
	run("setsid openssl s_server -accept 9443 -cert /tmp/c.pem -key /tmp/k.pem -www >/dev/null 2>&1 &")
	run("setsid openssl s_server -accept 9444 -cert /tmp/c.pem -key /tmp/k.pem -tls1_3 -www >/dev/null 2>&1 &")
	at := func(id string, port int) pve.LBMember {
		return pve.LBMember{ServerID: id, Port: port, Weight: 1, Address: "127.0.0.1"}
	}
	fast := func(kind string) *pve.LBHealthCheck {
		return &pve.LBHealthCheck{Type: kind, Interval: 1, Timeout: 1, Rise: 1, Fall: 1}
	}
	spec := pve.LBSpec{
		Listeners: []pve.LBListener{
			{Port: 8001, Protocol: "tcp", Pool: "proxy", ConnectionLimit: 7},
			{Port: 8002, Protocol: "tcp", Pool: "stick"},
			{Port: 8003, Protocol: "tcp", Pool: "spread"},
		},
		Pools: []pve.LBPool{
			{Name: "proxy", Protocol: "tcp", Algorithm: "roundrobin", Members: []pve.LBMember{at("103", 9083)},
				ProxyProtocol: true, ProxyVersion: 1, HealthCheck: &pve.LBHealthCheck{Type: "none"}},
			{Name: "stick", Protocol: "tcp", Algorithm: "roundrobin", Members: []pve.LBMember{at("101", 9081), at("102", 9082)},
				SessionPersistence: &pve.LBPersistence{Type: "source_ip", Timeout: 60}},
			{Name: "spread", Protocol: "tcp", Algorithm: "roundrobin", Members: []pve.LBMember{at("101", 9081), at("102", 9082)}},
			{Name: "https", Protocol: "tcp", Algorithm: "roundrobin", Members: []pve.LBMember{at("201", 9443), at("101", 9081)}, HealthCheck: fast("https")},
			{Name: "hello", Protocol: "tcp", Algorithm: "roundrobin", Members: []pve.LBMember{at("201", 9443), at("202", 9444), at("101", 9081)}, HealthCheck: fast("tls-hello")},
		},
	}
	if asked := l4Copy(spec); asked.Validate() != "" { // a copy: Validate clears the addresses
		t.Fatal(asked.Validate())
	}
	if err := c.CopyToContainer(ctx, []byte(pve.RenderHAProxy(spec)), "/tmp/l4.cfg", 0o644); err != nil {
		t.Fatal(err)
	}
	// What the apply saved before this reload says the member of the pool without a check was down (it had a check
	// then, and failed it). Nothing would ever bring it back, so that pool must not read the saved state.
	run(`mkdir -p /var/lib/haproxy && printf '1\n# be_id be_name srv_id srv_name srv_addr srv_op_state srv_admin_state srv_uweight srv_iweight srv_time_since_last_change srv_check_status srv_check_result srv_check_health srv_check_state srv_agent_state bk_f_forced_id srv_f_forced_id srv_fqdn srv_port srvrecord srv_use_ssl srv_check_port srv_check_addr srv_agent_addr srv_agent_port\n` +
		`5 p-proxy 1 m103p9083 127.0.0.1 0 0 1 1 4 8 2 0 6 0 0 0 - 9083 - 0 0 - - 0\n' > /var/lib/haproxy/server-state`)
	run("mkdir -p /run/haproxy && haproxy -D -f /tmp/l4.cfg")
	ask := func(port int) string {
		return strings.TrimSpace(run(fmt.Sprintf("echo hello | socat -t3 - TCP:127.0.0.1:%d", port)))
	}
	var stick, spread string
	for range 6 {
		stick += ask(8002)
		spread += ask(8003)
	}
	if stick != "aaaaaa" && stick != "bbbbbb" {
		t.Errorf("source persistence: one client reached %q", stick)
	}
	if !strings.Contains(spread, "a") || !strings.Contains(spread, "b") {
		t.Errorf("round robin without persistence: %q", spread)
	}
	if got := ask(8001); got != "c" {
		t.Errorf("the PROXY pool answered %q", got)
	}
	if line := run("cat /tmp/c.in"); !strings.HasPrefix(line, "PROXY TCP4 127.0.0.1 127.0.0.1 ") {
		t.Errorf("the member's first line is %q, not a PROXY v1 header", line)
	}
	state := map[string]string{}
	deadline := time.Now().Add(30 * time.Second)
	for {
		stat := run(`echo "show stat" | socat - /run/haproxy/admin.sock`)
		health, notLoaded, err := pve.ParseHAProxyStat(stat, spec)
		if err != nil || len(notLoaded) != 0 {
			t.Fatalf("health: %v, not loaded %q", err, notLoaded)
		}
		for _, h := range health {
			state[h.Pool+"/"+h.ServerID] = h.Status
		}
		up := func(k string) bool { return strings.HasPrefix(state[k], "UP") }
		down := func(k string) bool { return strings.HasPrefix(state[k], "DOWN") }
		if up("https/201") && down("https/101") && up("hello/201") && up("hello/202") && down("hello/101") && state["proxy/103"] == "no check" {
			if !strings.Contains(stat, "\nl8001,FRONTEND,") {
				t.Fatalf("no frontend l8001:\n%s", stat)
			}
			rows := strings.Split(stat, "\n")
			slim := slices.Index(strings.Split(strings.TrimPrefix(rows[0], "# "), ","), "slim")
			for _, row := range rows {
				if f := strings.Split(row, ","); f[0] == "l8001" && f[1] == "FRONTEND" && f[slim] != "7" {
					t.Errorf("listener 8001 holds %s connections, want its limit of 7", f[slim])
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("members read %v", state)
		}
		time.Sleep(time.Second)
	}
	run("pkill haproxy; pkill socat; pkill openssl; true")
}

// l4Copy is a spec whose pools and members are its own.
func l4Copy(s pve.LBSpec) pve.LBSpec {
	out := pve.LBSpec{Listeners: slices.Clone(s.Listeners), Pools: slices.Clone(s.Pools)}
	for i := range out.Pools {
		out.Pools[i].Members = slices.Clone(out.Pools[i].Members)
	}
	return out
}

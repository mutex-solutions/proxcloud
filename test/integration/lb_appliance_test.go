//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// guestAgent is the Proxmox guest agent API of one VM, served by a container that runs systemd as the appliance's
// image does: exec runs the command in the guest, file-write creates the file with mode 0666 as the real agent does
// (drill N2-6) and leaves the mode of a file that exists, which it empties and fills (qemu-ga's guest-file-open
// creates with O_EXCL and sets the mode only then), and exec-status answers when the command ended. Nothing of the
// guest is modelled: HAProxy, keepalived, systemd, dpkg and ss are the distribution's own.
type guestAgent struct {
	t    *testing.T
	c    testcontainers.Container
	mu   sync.Mutex
	runs map[int]*guestRun
	// written lists the files the product wrote through the agent, in order.
	written []string
	wmu     sync.Mutex // one file-write at a time: they share the path the content travels by
}

// writtenSince answers the files written through the agent after the first n, and how many there are now.
func (g *guestAgent) writtenSince(n int) ([]string, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.written[n:]), len(g.written)
}

type guestRun struct {
	done     bool
	code     int
	out, err string
}

// sh runs a command in the guest and answers its exit code, stdout and stderr.
func (g *guestAgent) sh(ctx context.Context, tag string, args ...string) (int, string, string) {
	errFile := "/tmp/agent-" + tag + ".err"
	code, r, err := g.c.Exec(ctx, append([]string{"sh", "-c", `"$@" 2>"$0"`, errFile}, args...), tcexec.Multiplexed())
	if err != nil {
		return -1, "", err.Error()
	}
	out, _ := io.ReadAll(r)
	_, r, err = g.c.Exec(ctx, []string{"cat", errFile}, tcexec.Multiplexed())
	stderr := []byte{}
	if err == nil {
		stderr, _ = io.ReadAll(r)
	}
	return code, string(out), string(stderr)
}

// run is a test's own command in the guest; it must succeed.
func (g *guestAgent) run(ctx context.Context, cmd string) string {
	g.t.Helper()
	code, out, stderr := g.sh(ctx, "test", "sh", "-c", cmd)
	if code != 0 {
		g.t.Fatalf("%s: exit %d\n%s\n%s", cmd, code, out, stderr)
	}
	return strings.TrimSpace(out)
}

func (g *guestAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	write := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	const base = "/api2/json/nodes/n1/qemu/100/agent/"
	switch r.Method + " " + strings.TrimPrefix(r.URL.Path, base) {
	case "POST exec":
		g.mu.Lock()
		pid := len(g.runs) + 1
		run := &guestRun{}
		g.runs[pid] = run
		g.mu.Unlock()
		args := r.Form["command"]
		go func() {
			code, out, stderr := g.sh(context.Background(), strconv.Itoa(pid), args...)
			g.mu.Lock()
			run.done, run.code, run.out, run.err = true, code, out, stderr
			g.mu.Unlock()
		}()
		write(map[string]any{"pid": pid})
	case "GET exec-status":
		pid, _ := strconv.Atoi(r.Form.Get("pid"))
		g.mu.Lock()
		run := *g.runs[pid]
		g.mu.Unlock()
		if !run.done {
			write(map[string]any{"exited": 0})
			return
		}
		write(map[string]any{"exited": 1, "exitcode": run.code, "out-data": run.out, "err-data": run.err})
	case "POST file-write":
		g.mu.Lock()
		g.written = append(g.written, r.Form.Get("file"))
		g.mu.Unlock()
		// The real agent opens the file, which empties it, and then fills it. So the product writes no file onto
		// its place, where keepalived runs it or systemd reads it: it writes <path>.new and its apply moves that in
		// by a rename (drill Q1-o2 of v0.11.1: the track script was seen empty). docker cp replaces a file in one
		// step and would not show the difference, so the rule is held here, for every push of every test.
		if !strings.HasSuffix(r.Form.Get("file"), ".new") {
			g.t.Errorf("the agent was asked to write %s onto its place: a file of the product's is written as <path>.new and moved in", r.Form.Get("file"))
		}
		// The content travels beside the guest's files and is written by the guest itself, as the agent writes: a
		// new file is created with mode 0666, an existing one keeps its mode and its inode and is emptied and filled.
		// (docker cp onto the path would replace the file and give it the mode of the copy, whatever was there.)
		g.wmu.Lock()
		defer g.wmu.Unlock()
		err := g.c.CopyToContainer(r.Context(), []byte(r.Form.Get("content")), "/root/.agent-write", 0o600)
		if err == nil {
			if code, _, stderr := g.sh(r.Context(), "write", "sh", "-c", `[ -e "$1" ] || { (umask 0; : > "$1") && chmod 0666 "$1"; } && cat /root/.agent-write > "$1"`, "sh", r.Form.Get("file")); code != 0 {
				err = fmt.Errorf("exit %d %s", code, stderr)
			}
		}
		if err != nil {
			g.t.Errorf("file-write %s: %v", r.Form.Get("file"), err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		write(nil)
	default:
		g.t.Errorf("reached PVE: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

const (
	// lbPackageConfig stands for the config the haproxy package ships: no proxy of anyone's.
	lbPackageConfig = "global\n  stats socket /run/haproxy/admin.sock mode 660 level admin\n\ndefaults\n  mode http\n  timeout connect 5s\n  timeout client 50s\n  timeout server 50s\n"
	// lbPackageStart puts HAProxy where the package's own first start has it while dpkg still configures the package
	// (drill N2-2, d5-diag-127.txt): the process has read the package's config, and the unit is still "activating".
	lbPackageStart = `systemctl stop haproxy
cat > /etc/haproxy/haproxy.cfg <<'EOF'
` + lbPackageConfig + `EOF
mkdir -p /etc/systemd/system/haproxy.service.d
printf '[Service]\nExecStartPost=/bin/sleep 6\n' > /etc/systemd/system/haproxy.service.d/slow.conf
systemctl daemon-reload
systemctl start --no-block haproxy
n=0; until [ -S /run/haproxy-master.sock ] && [ "$(systemctl is-active haproxy)" = activating ]; do n=$((n+1)); [ $n -lt 50 ] || exit 1; sleep 0.1; done`
	// lbRC1Apply is the apply of v0.11.0-rc.1, the control: on a start that is under way it answers 0 and HAProxy
	// keeps the package's config.
	lbRC1Apply = `set -e
F=/etc/haproxy/haproxy.cfg
haproxy -c -q -f $F.new
cp $F $F.old
mv $F.new $F
swap() {
  systemctl is-active -q haproxy || { systemctl start haproxy; return; }
  out=$(printf 'reload\n' | socat -t30 - /run/haproxy-master.sock)
  case "$out" in Success=1*) ;; *) echo "$out" | grep ALERT >&2; return 1 ;; esac
}
swap || { mv $F.old $F; swap; exit 1; }`
	lbListening = `ss -Hltnp | awk '/"haproxy"/{print $4}' | sort -u | tr '\n' ' '`
	// lbRC2Ready is the ready check of v0.11.1-rc.2, the control: it asks "does a package manager run" first, and
	// answers 3 (busy) on an appliance whose packages were installed long ago.
	lbRC2Ready = `pgrep -x 'apt-get|apt|dpkg' >/dev/null && exit 3
for p in haproxy socat keepalived; do [ "$(dpkg-query -W -f='${Status}' $p 2>/dev/null)" = 'install ok installed' ] || exit 1; done
command -v ss >/dev/null && systemctl cat haproxy.service keepalived.service >/dev/null 2>&1`
	// lbSSHListens prints how many sockets listen on port 22.
	lbSSHListens = `ss -Hltn 'sport = :22' | wc -l`
)

// An appliance's config is recorded only when the running HAProxy serves it. The guest is a real one of each image an
// appliance is built from, with the distribution's systemd, dpkg, HAProxy and keepalived; the product's own calls
// (PushKeepalived, PushPairHAProxy, PushHAProxy, ReadVRRPState, HAProxyServing, LBHealth) reach it through the guest
// agent API. Shown: the first push through the product's own install, never taken before dpkg is done; the package's
// first start still under way, where the apply of v0.11.0-rc.1 answered success with nothing listening, and where
// keepalived now gives the address up (drill N3-3); a stopped HAProxy and a stopped keepalived on a running guest; an
// apply whose proof fails; a member that does not answer, which costs no request and stays DOWN over a reload (drill
// N3-8); the single appliance's config; and the mode of every file the agent wrote.
//
// From the drill of v0.11.1-rc.2: a change is taken while a package manager runs, and waits in the one case that dpkg
// is at work on HAProxy's own package, until dpkg is done with it (R3-F1, R2b-1); and sshd, which the image runs,
// is masked by the product's install and stays off over a reboot, where a unit that is only disabled is started
// again by whatever asks for it (R4-o1).
//
// This is the test of the product's own install, and the one real-guest test that runs on the distribution's plain
// image whatever LB_TEST_IMAGE_* names (lb_guest_test.go): the pair's install line at its start, the UDP service's
// at its end. It needs the distribution's mirror, and nothing lets it skip.
func TestLBApplianceServesWhatIsRecorded(t *testing.T) {
	for _, image := range []string{"debian:trixie", "ubuntu:26.04"} {
		t.Run(image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 16*time.Minute)
			defer cancel()
			// The plain image, with systemd as PID 1 and nothing else: the product installs the rest.
			c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
				ContainerRequest: testcontainers.ContainerRequest{
					Image:      image,
					Cmd:        lbInit("systemd-sysv", "procps"),
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
			g.booted(ctx)
			srv := httptest.NewServer(g)
			defer srv.Close()
			ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
			if err != nil {
				t.Fatal(err)
			}
			self, err := c.ContainerIP(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// The peer does not exist, so this appliance takes the address by itself. The public side is lo, as in
			// TestLBPairConfigsPassTheirChecks: the container has one interface.
			const vip = "203.0.113.10"
			peer := self[:strings.LastIndexByte(self, '.')] + ".250"
			keepalived, err := pve.RenderKeepalived(pve.LBKeepalived{RouterID: 42, Self: self, Peer: peer, TenantIface: "eth0", PublicIface: "lo",
				VIPs: []string{vip}, Prefix: 24, Gateway: "203.0.113.1"})
			if err != nil {
				t.Fatal(err)
			}
			spec := func(ports ...int) pve.LBSpec {
				s := pve.LBSpec{Pools: []pve.LBPool{{Name: "web", Protocol: "http", Algorithm: "roundrobin",
					Members: []pve.LBMember{{ServerID: "101", Port: 8080, Weight: 1, Address: "10.0.0.11"}}}}}
				for _, p := range ports {
					s.Listeners = append(s.Listeners, pve.LBListener{Port: p, Pool: "web"})
				}
				return s
			}
			pair := pve.RenderHAProxyOn(spec(80), []string{vip})
			state := func() pve.VRRPState {
				t.Helper()
				st, err := ps.ReadVRRPState(ctx, "n1", 100, vip)
				if err != nil {
					t.Fatal(err)
				}
				return st
			}
			listens := func(when, want string) {
				t.Helper()
				if got := g.run(ctx, lbListening); got != want {
					t.Fatalf("%s: HAProxy listens on %q, want %q", when, got, want)
				}
			}

			// The image runs sshd, as a cloud image does: here it is installed and listens before the product's first
			// command (on Ubuntu through its socket).
			// (No ss yet: iproute2 comes with the product's install. g.run fails the test when sshd is not up.)
			g.run(ctx, "DEBIAN_FRONTEND=noninteractive apt-get install -y -qq openssh-server >/dev/null 2>&1; systemctl start ssh.socket 2>/dev/null || systemctl start ssh.service; systemctl is-active ssh.service ssh.socket | grep -qx active")

			// 1. The first push, through the product's own install line. Asked as fast as the agent answers, so the
			// push lands as early as the ready check lets it. Every refusal while the binaries already exist is one
			// the check of v0.11.0-rc.1 (command -v) would have let through, into dpkg's configure.
			early := 0
			for deadline := time.Now().Add(8 * time.Minute); ; time.Sleep(200 * time.Millisecond) {
				err := ps.PushKeepalived(ctx, "n1", 100, keepalived, "lo")
				if err == nil {
					_, err = ps.PushPairHAProxy(ctx, "n1", 100, pair)
				}
				if err == nil {
					break
				}
				if !errors.Is(err, pve.ErrLBNotReady) {
					t.Fatalf("first push: %v", err)
				}
				if code, _, _ := g.sh(ctx, "early", "sh", "-c", "command -v haproxy && command -v socat && command -v keepalived"); code == 0 {
					early++
				}
				if time.Now().After(deadline) {
					t.Fatalf("the appliance did not become ready: %v%s", err, g.mirror(ctx))
				}
			}
			t.Logf("%s; %s; refused %d times while the binaries were already there",
				g.run(ctx, "haproxy -v | head -1"), g.run(ctx, "keepalived --version 2>&1 | head -1"), early)
			listens("after the first push", vip+":80")
			// The install masked sshd: nothing listens, and nothing can start it.
			sshOff := func(when string) {
				t.Helper()
				if got := g.run(ctx, "systemctl is-enabled ssh.service ssh.socket 2>/dev/null | sort -u; "+lbSSHListens); got != "masked\n0" {
					t.Fatalf("%s: sshd's units are %q, want both masked and nothing listening on 22", when, got)
				}
				if code, _, _ := g.sh(ctx, "ssh", "sh", "-c", "systemctl start ssh.service || systemctl start ssh.socket || systemctl start sshd.service"); code == 0 {
					t.Fatalf("%s: sshd could be started", when)
				}
			}
			sshOff("after the product's install")
			if st := state(); !st.HAProxy || !st.Serving {
				t.Fatalf("after the first push: %+v", st)
			}
			if got := g.run(ctx, "dpkg-query -W -f='${Status}' haproxy"); got != "install ok installed" {
				t.Fatalf("pushed while dpkg said %q", got)
			}

			// 2. The drill's race, held still: the package's own start has read the package's config and is not done.
			// The control first: rc.1's apply answers 0, nothing listens, and the state read now says so.
			g.run(ctx, lbPackageStart)
			if err := g.c.CopyToContainer(ctx, []byte(pair), "/etc/haproxy/haproxy.cfg.new", 0o666); err != nil {
				t.Fatal(err)
			}
			if code, _, stderr := g.sh(ctx, "rc1", "sh", "-c", lbRC1Apply); code != 0 {
				t.Fatalf("control: rc.1's apply failed, so this is not the drill's case: %s", stderr)
			}
			listens("control: after rc.1's apply", "")
			if st := state(); !st.HAProxy || st.Serving {
				t.Fatalf("control: a master with a worker and no listener must read as running and not serving: %+v", st)
			}
			if _, notLoaded, err := ps.LBHealth(ctx, "n1", 100, spec(80)); err != nil || len(notLoaded) != 1 || notLoaded[0] != "80" {
				t.Fatalf("control: health of an appliance on the package's config: %q %v", notLoaded, err)
			}
			// Drill N3-3: such an appliance kept the address for 25 s, until the next pass, since keepalived's track
			// script only asked for HAProxy's worker. It asks for the config's first listener too, once the file is
			// ten seconds old: FAULT, with HAProxy running, and the address is given up for the peer to take. It is
			// asked 15 s on: the stop a moment ago made a FAULT of its own, which is over by then (with the script
			// of v0.11.0 the appliance is MASTER and holds the address again, and this step fails).
			for asked, deadline := time.Now().Add(15*time.Second), time.Now().Add(45*time.Second); ; time.Sleep(500 * time.Millisecond) {
				if st := state(); time.Now().After(asked) && st.State == "FAULT" && !st.HoldsVIP && st.HAProxy && !st.Serving {
					break
				} else if time.Now().After(deadline) {
					t.Fatalf("an appliance whose HAProxy runs without the balancer's listeners kept the address: %+v\n%s", st,
						g.run(ctx, "cat /usr/local/sbin/proxcloud-lb-check; journalctl -u keepalived --no-pager | tail -20"))
				}
			}
			// The same start, and the product's apply: it waits for the start, reloads, and listens.
			g.run(ctx, lbPackageStart)
			if _, err := ps.PushPairHAProxy(ctx, "n1", 100, pair); err != nil {
				t.Fatalf("push during the package's start: %v", err)
			}
			listens("after a push during the package's start", vip+":80")
			if got := g.run(ctx, `echo "show proc" | socat - /run/haproxy-master.sock | awk '$2=="master"{print $3}'`); got == "0" {
				t.Fatalf("HAProxy's master counts no reload: it runs what it started with")
			}
			g.run(ctx, "rm /etc/systemd/system/haproxy.service.d/slow.conf && systemctl daemon-reload")

			// 3. A service stopped on a running guest: the state read says which, the same push starts it.
			g.run(ctx, "systemctl stop haproxy")
			if st := state(); st.HAProxy || st.Serving {
				t.Fatalf("HAProxy stopped: %+v", st)
			}
			if _, err := ps.PushPairHAProxy(ctx, "n1", 100, pair); err != nil {
				t.Fatalf("push with HAProxy stopped: %v", err)
			}
			listens("after a push with HAProxy stopped", vip+":80")
			g.run(ctx, "systemctl stop keepalived")
			if st := state(); st.State != "STOPPED" {
				t.Fatalf("keepalived stopped: %+v", st)
			}
			if err := ps.PushKeepalived(ctx, "n1", 100, keepalived, "lo"); err != nil {
				t.Fatalf("push with keepalived stopped: %v", err)
			}
			for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(500 * time.Millisecond) {
				if st := state(); st.State == "MASTER" && st.HoldsVIP && st.HAProxy && st.Serving {
					break
				} else if time.Now().After(deadline) {
					t.Fatalf("keepalived did not take the address again: %+v", st)
				}
			}

			// 3a. A reload on the appliance that holds the address is no failed run of keepalived's track script
			// (drill Q1-o1 of v0.11.1-rc.1: one failed run at the reload of a push, on the master, with "fall 2" as
			// the only thing between a second one and the addresses moving at every push). HAProxy's master execs
			// itself at a reload and its socket refuses connections meanwhile; the script asks again. Twenty pushes
			// in a row, each one reload. keepalived's own runs come once a second and may miss that moment, so the
			// script file is also run without a pause for as long as the pushes last, which meets every reload: the
			// script of rc.1 fails there dozens of times. Then HAProxy is stopped for good, and the address is
			// given up as before, within today's two intervals and the script's tries.
			journal := func(what string) string {
				return g.run(ctx, "journalctl -u keepalived --no-pager | grep -c '"+what+"' || true")
			}
			failedBefore, enteredBefore := journal("now returning 1"), journal("Entering")
			g.run(ctx, `cat > /root/track-probe <<'EOF'
#!/bin/sh
f=0; r=0
trap 'echo "$r $f" > /root/track-probe.out; exit 0' TERM
while :; do /usr/local/sbin/proxcloud-lb-check || f=$((f+1)); r=$((r+1)); done
EOF
chmod 0755 /root/track-probe && : > /root/track-probe.out && systemd-run -q --unit=lb-track-probe -p KillMode=process /root/track-probe`)
			// (KillMode=process: the stop below ends the loop after the run under way, and does not kill that run.)
			for i := range 20 {
				cfg := pair
				if i%2 == 0 { // a changed config and an unchanged one are the same reload
					cfg = pve.RenderHAProxyOn(spec(80, 8082), []string{vip})
				}
				if _, err := ps.PushPairHAProxy(ctx, "n1", 100, cfg); err != nil {
					t.Fatalf("reload %d on the master: %v", i+1, err)
				}
			}
			g.run(ctx, "systemctl stop lb-track-probe; n=0; until [ -s /root/track-probe.out ]; do n=$((n+1)); [ $n -lt 50 ] || exit 1; sleep 0.1; done")
			var runs, failed int
			if _, err := fmt.Sscan(g.run(ctx, "cat /root/track-probe.out"), &runs, &failed); err != nil || runs < 100 || failed != 0 {
				t.Fatalf("the track script, run without a pause over 20 reloads: %d runs, %d failed (%v): want hundreds of runs and none failed", runs, failed, err)
			}
			if f, e := journal("now returning 1"), journal("Entering"); f != failedBefore || e != enteredBefore {
				t.Fatalf("over 20 reloads keepalived logged %s failed runs of its track script (%s before) and %s state changes (%s before)\n%s",
					f, failedBefore, e, enteredBefore, g.run(ctx, "journalctl -u keepalived --no-pager | tail -20"))
			}
			if st := state(); st.State != "MASTER" || !st.HoldsVIP || !st.Serving {
				t.Fatalf("after 20 reloads: %+v", st)
			}
			t.Logf("20 reloads on the master: %d runs of the track script, none failed", runs)
			// HAProxy gone for good: the address is given up. Today's worst case is two runs a second apart; each
			// run now takes up to 0.6 s more to fail, which delays the second one by that much.
			out := g.run(ctx, `systemctl stop haproxy; t0=$(date +%s%N)
until [ "$(cat /run/proxcloud-lb.state)" = FAULT ] && ! ip -o -4 addr show | grep -qF ' `+vip+`/'; do
  [ $(( ($(date +%s%N) - t0) / 1000000 )) -lt 15000 ] || { echo never; exit 0; }
  sleep 0.05
done
echo $(( ($(date +%s%N) - t0) / 1000000 ))`)
			if ms, err := strconv.Atoi(out); err != nil || ms > 4000 {
				t.Fatalf("with HAProxy stopped the appliance gave the address up after %s ms, want within 4000 (two intervals, the tries of one run, and the stop itself)\n%s",
					out, g.run(ctx, "journalctl -u keepalived --no-pager | tail -20"))
			} else {
				t.Logf("HAProxy stopped: the address was given up after %d ms", ms)
			}
			if _, err := ps.PushPairHAProxy(ctx, "n1", 100, pair); err != nil {
				t.Fatalf("push with HAProxy stopped, after the reloads: %v", err)
			}
			for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(500 * time.Millisecond) {
				if st := state(); st.State == "MASTER" && st.HoldsVIP && st.HAProxy && st.Serving {
					break
				} else if time.Now().After(deadline) {
					t.Fatalf("keepalived did not take the address again after the reloads: %+v", st)
				}
			}
			listens("after the reloads", vip+":80")

			// 4. The proof fails: HAProxy answers the reload and does not run the pushed file (here it is started on
			// another one). Not applied, said why, and the file in use before is back.
			g.run(ctx, "cp /etc/haproxy/haproxy.cfg /etc/haproxy/other.cfg && echo CONFIG=/etc/haproxy/other.cfg >> /etc/default/haproxy && systemctl restart haproxy")
			_, err = ps.PushPairHAProxy(ctx, "n1", 100, pve.RenderHAProxyOn(spec(80, 8081), []string{vip}))
			if err == nil || errors.Is(err, pve.ErrLBNotReady) || !strings.Contains(err.Error(), "does not listen on "+vip+":8081") {
				t.Fatalf("push HAProxy does not load: %v", err)
			}
			if got := g.run(ctx, "grep -c 8081 /etc/haproxy/haproxy.cfg || true"); got != "0" {
				t.Fatalf("the file that was not loaded stayed in place")
			}
			g.run(ctx, "sed -i '/^CONFIG=/d' /etc/default/haproxy && systemctl restart haproxy")
			if _, err := ps.PushPairHAProxy(ctx, "n1", 100, pve.RenderHAProxyOn(spec(80, 8081), []string{vip})); err != nil {
				t.Fatalf("push once HAProxy loads its file again: %v", err)
			}
			listens("after the second listener", vip+":80 "+vip+":8081")
			health, notLoaded, err := ps.LBHealth(ctx, "n1", 100, spec(80, 8081))
			if err != nil || len(notLoaded) != 0 || len(health) != 1 || health[0].Status == "" || health[0].Status == pve.LBNotLoaded {
				t.Fatalf("health of a serving appliance: %+v %q %v", health, notLoaded, err)
			}

			// 5. Members and reloads (drill N3-8 of v0.11.0: 2.2 to 10.3 s of lost requests at every reload or node
			// change). A pool of two members: one answers (a listener in the guest), one never does (its address is
			// routed into the guest's loopback, where nobody has it: a connection to it gets no answer, as to a VM
			// that was just stopped).
			g.run(ctx, "ip route replace 10.0.0.12/32 dev lo && systemd-run -q --unit=lb-test-member socat TCP-LISTEN:18080,bind=127.0.0.1,fork,reuseaddr SYSTEM:'echo ok'")
			members := func(listen80 bool) pve.LBSpec {
				s := pve.LBSpec{}
				if listen80 { // a pool before the one this step looks at, which comes and goes
					s = spec(80)
				}
				s.Pools = append(s.Pools, pve.LBPool{Name: "tcp", Protocol: "tcp", Algorithm: "roundrobin", Members: []pve.LBMember{
					{ServerID: "201", Port: 18080, Weight: 1, Address: "127.0.0.1"}, {ServerID: "202", Port: 18080, Weight: 1, Address: "10.0.0.12"}}})
				s.Listeners = append(s.Listeners, pve.LBListener{Port: 8082, Pool: "tcp"})
				return s
			}
			// What HAProxy says of the two members right now: UP or DOWN. Its own words say more ("UP 1/2" is a
			// member new to a config, which goes down at its first failed check).
			says := func(s pve.LBSpec) string {
				t.Helper()
				health, notLoaded, err := ps.LBHealth(ctx, "n1", 100, s)
				if err != nil || len(notLoaded) != 0 {
					t.Fatalf("health: %q %v", notLoaded, err)
				}
				var out []string
				for _, m := range health {
					if m.Pool == "tcp" {
						out = append(out, m.ServerID+" "+strings.Join(strings.Fields(m.Status)[:1], ""))
					}
				}
				return strings.Join(out, ", ")
			}
			// n requests through the balancer's address, one after the other; each must get the member's answer.
			requests := func(when string, n int) time.Duration {
				t.Helper()
				start := time.Now()
				for i := range n {
					if code, out, stderr := g.sh(ctx, "req", "socat", "-u", "TCP:"+vip+":8082,connect-timeout=10", "-"); code != 0 || strings.TrimSpace(out) != "ok" {
						t.Fatalf("%s: request %d of %d was lost: exit %d %q %s\n%s", when, i+1, n, code, out, stderr, says(members(true)))
					}
				}
				return time.Since(start)
			}
			redispatched := func() string {
				return g.run(ctx, `echo "show stat" | socat - /run/haproxy/admin.sock | awk -F, 'NR==1{for(i=1;i<=NF;i++) if($i=="wredis") c=i} $1=="p-tcp" && $2=="BACKEND"{print $c}'`)
			}
			for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(500 * time.Millisecond) { // the steps before restarted HAProxy
				if st := state(); st.State == "MASTER" && st.HoldsVIP {
					break
				} else if time.Now().After(deadline) {
					t.Fatalf("the appliance does not hold the address: %+v", st)
				}
			}
			if _, err := ps.PushPairHAProxy(ctx, "n1", 100, pve.RenderHAProxyOn(members(true), []string{vip})); err != nil {
				t.Fatalf("push the pool of two members: %v", err)
			}
			// Both members are new to the config, so both start UP: the one that never answers gets every second
			// request until its first check has failed. None is lost: its connect times out and the request goes to
			// the other member.
			if got := says(members(true)); got != "201 UP, 202 UP" {
				t.Fatalf("members new to a config: %s", got)
			}
			took := requests("a member that does not answer, not found out yet", 4)
			if n := redispatched(); n == "" || n == "0" {
				t.Fatalf("no request went to the member that does not answer (redispatched %q in %s): the step proves nothing", n, took)
			}
			t.Logf("4 requests while a member that does not answer still counted as UP: all answered, in %s, %s of them on a second member", took, redispatched())
			for deadline := time.Now().Add(40 * time.Second); says(members(true)) != "201 UP, 202 DOWN"; time.Sleep(500 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("the member that does not answer was never found out: %s", says(members(true)))
				}
			}
			// A reload that changes the pools before it (the listener on 80 and its pool go): the member that was
			// DOWN is DOWN from the first moment, under a backend whose name did not move, and no request waits.
			if _, err := ps.PushPairHAProxy(ctx, "n1", 100, pve.RenderHAProxyOn(members(false), []string{vip})); err != nil {
				t.Fatalf("reload: %v", err)
			}
			if got := says(members(false)); got != "201 UP, 202 DOWN" {
				t.Fatalf("right after a reload: %s, want the member that was DOWN still DOWN\n%s", got, g.run(ctx, "cat /var/lib/haproxy/server-state"))
			}
			if took := requests("right after a reload", 6); redispatched() != "0" || took > 3*time.Second {
				t.Fatalf("after a reload a request went to the member that was DOWN: %s redispatched, 6 requests took %s", redispatched(), took)
			}
			listens("after the reload that kept the members' state", vip+":8082")
			if got := g.run(ctx, "stat -c %a:%U:%G /var/lib/haproxy/server-state"); got != "644:root:root" {
				t.Fatalf("the state file is %s", got)
			}
			// The control: the same reload with no state saved starts the member UP again, as every reload did
			// before. It also shows that HAProxy loads a config whose state file is not there.
			g.run(ctx, `rm /var/lib/haproxy/server-state && printf 'reload\n' | socat -t30 - /run/haproxy-master.sock | grep -q 'Success=1'`)
			if got := says(members(false)); got != "201 UP, 202 UP" {
				t.Fatalf("control: a reload with no saved state: %s, want every member UP", got)
			}
			requests("control: after a reload with no saved state", 2)
			if n := redispatched(); n == "0" {
				t.Fatalf("control: no request went to the member a reload without state brought back")
			}
			// A HAProxy that was stopped has no state: the apply leaves no stale file for the start to read.
			for deadline := time.Now().Add(40 * time.Second); says(members(false)) != "201 UP, 202 DOWN"; time.Sleep(500 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("control: the member was never found out again: %s", says(members(false)))
				}
			}
			if _, err := ps.PushPairHAProxy(ctx, "n1", 100, pve.RenderHAProxyOn(members(true), []string{vip})); err != nil {
				t.Fatalf("reload: %v", err)
			}
			g.run(ctx, "test -s /var/lib/haproxy/server-state && systemctl stop haproxy")
			if _, err := ps.PushPairHAProxy(ctx, "n1", 100, pve.RenderHAProxyOn(members(true), []string{vip})); err != nil {
				t.Fatalf("push with HAProxy stopped: %v", err)
			}
			if got := says(members(true)); got != "201 UP, 202 UP" {
				t.Fatalf("a HAProxy started from nothing read a state: %s", got)
			}
			g.run(ctx, "systemctl stop lb-test-member")

			// 6. A single appliance's config binds every address.
			if _, err := ps.PushHAProxy(ctx, "n1", 100, pve.RenderHAProxy(spec(80))); err != nil {
				t.Fatalf("single appliance: %v", err)
			}
			listens("single appliance", "0.0.0.0:80")
			if ok, why, err := ps.HAProxyServing(ctx, "n1", 100, lbSum(pve.RenderHAProxy(spec(80)))); err != nil || !ok {
				t.Fatalf("single appliance serving: %v %q %v", ok, why, err)
			}
			// A file changed by hand is served, and is not the one that was applied (spike of v0.11.1, SP-2): the
			// state read says so, which makes the next pass push it again.
			g.run(ctx, "echo '# by hand' >> /etc/haproxy/haproxy.cfg")
			if ok, why, err := ps.HAProxyServing(ctx, "n1", 100, lbSum(pve.RenderHAProxy(spec(80)))); err != nil || ok || !strings.Contains(why, "not the one that was applied") {
				t.Fatalf("single appliance, file changed by hand: %v %q %v", ok, why, err)
			}
			g.run(ctx, "systemctl stop haproxy")
			if ok, why, err := ps.HAProxyServing(ctx, "n1", 100, ""); err != nil || ok || why != "HAProxy is not running" {
				t.Fatalf("single appliance stopped: %v %q %v", ok, why, err)
			}
			if _, err := ps.PushHAProxy(ctx, "n1", 100, pve.RenderHAProxy(spec(80))); err != nil {
				t.Fatalf("single appliance, HAProxy stopped: %v", err)
			}
			listens("single appliance started again", "0.0.0.0:80")

			// 7. The agent made every file 0666 (the control); what the product left is root's, 0644, scripts 0755.
			if err := ps.C.GuestWriteFile(ctx, "n1", 100, "/root/agent-wrote.new", "x"); err != nil {
				t.Fatal(err)
			}
			if got := g.run(ctx, "stat -c %a /root/agent-wrote.new"); got != "666" {
				t.Fatalf("control: the agent's file has mode %s", got)
			}
			want := map[string]string{
				"/etc/haproxy/haproxy.cfg": "644", "/etc/haproxy/haproxy.cfg.old": "644", "/etc/keepalived/keepalived.conf": "644",
				"/etc/sysctl.d/90-proxcloud-lb.conf": "644", "/etc/systemd/system/keepalived.service.d/proxcloud.conf": "644",
				"/usr/local/sbin/proxcloud-lb-check": "755", "/usr/local/sbin/proxcloud-lb-notify": "755",
			}
			for path, mode := range want {
				if got := g.run(ctx, "stat -c %a:%U:%G "+path); got != fmt.Sprintf("%s:root:root", mode) {
					t.Errorf("%s is %s", path, got)
				}
			}
			// Each of them was moved in from <path>.new by its apply: none is left behind, and what is in place is
			// what the product renders (the track and notify scripts of this build, whole).
			if got := g.run(ctx, "ls /etc/haproxy/*.new /etc/keepalived/*.new /etc/sysctl.d/*.new /etc/systemd/system/keepalived.service.d/*.new /usr/local/sbin/*.new 2>/dev/null | wc -l"); got != "0" {
				t.Errorf("%s files written beside their place were not moved in:\n%s", got, g.run(ctx, "ls -l /etc/haproxy /etc/keepalived /etc/sysctl.d /etc/systemd/system/keepalived.service.d /usr/local/sbin"))
			}
			if got := g.run(ctx, "cat /etc/keepalived/keepalived.conf; for f in /etc/sysctl.d/90-proxcloud-lb.conf /etc/systemd/system/keepalived.service.d/proxcloud.conf /usr/local/sbin/proxcloud-lb-check /usr/local/sbin/proxcloud-lb-notify; do echo $f; cat $f; done"); got != strings.TrimSpace(keepalived+pve.LBPairFilesDigest("lo")) {
				t.Errorf("keepalived's config and support files on the guest are not the ones the product renders:\n%s", got)
			}
			// The notify script writes keepalived's state by a rename too: no temporary file stays beside it. HAProxy
			// was stopped and started a moment ago (step 6), which takes keepalived through FAULT and BACKUP: it is
			// MASTER again about 4 s after HAProxy listens, so the state is waited for and not read once.
			for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(500 * time.Millisecond) {
				got := g.run(ctx, "cat /run/proxcloud-lb.state; ls /run/proxcloud-lb.state.* 2>/dev/null | wc -l")
				if got == "MASTER\n0" {
					break
				}
				if time.Now().After(deadline) {
					t.Errorf("the state file and what is beside it: %q", got)
					break
				}
			}

			// 8. A change while the image's package manager runs (drill R3-F1 and R2b-1 of v0.11.1-rc.2: Ubuntu's apt
			// timers ran 2 to 6.5 minutes on each appliance, and every change waited for them). A process named
			// apt-get runs, as apt-daily's does; the packages are installed. The check of v0.11.1-rc.2 says "busy"
			// (the control); the product's push is taken and served.
			// (The process is a shell that names itself: sleep is one multi-call binary on Ubuntu 26.04 and runs under
			// no other name.)
			g.run(ctx, `printf '%s\n' 'printf %s "$1" > /proc/$$/comm; while :; do sleep 60; done' > /root/named.sh
systemd-run --quiet --unit=fake-apt sh /root/named.sh apt-get && sleep 0.3 && pgrep -x apt-get >/dev/null`)
			if code, _, _ := g.sh(ctx, "rc2", "sh", "-c", lbRC2Ready); code != 3 {
				t.Fatalf("control: the ready check of v0.11.1-rc.2 answered %d while apt runs, want 3 (busy)", code)
			}
			if _, err := ps.PushPairHAProxy(ctx, "n1", 100, pve.RenderHAProxyOn(spec(80, 8082), []string{vip})); err != nil {
				t.Fatalf("a change while apt runs: %v", err)
			}
			listens("a change while apt runs", vip+":80 "+vip+":8082")
			if err := ps.PushKeepalived(ctx, "n1", 100, keepalived, "lo"); err != nil {
				t.Fatalf("keepalived's files while apt runs: %v", err)
			}
			// The one case that waits: dpkg is at work on HAProxy's own package. dpkg's own record says so, to the
			// distribution's dpkg-query: the package is unpacked and not configured, as it is between the unpack of
			// an upgrade and the end of its postinst, which restarts the service.
			g.run(ctx, `systemd-run --quiet --unit=fake-dpkg sh /root/named.sh dpkg && sleep 0.3 && pgrep -x dpkg >/dev/null
sed -i '/^Package: haproxy$/,/^$/ s/^Status: install ok installed$/Status: install ok unpacked/' /var/lib/dpkg/status
[ "$(dpkg-query -W -f='${Status}' haproxy)" = 'install ok unpacked' ]`)
			_, before := g.writtenSince(0)
			_, err = ps.PushPairHAProxy(ctx, "n1", 100, pve.RenderHAProxyOn(spec(80, 8083), []string{vip}))
			if !errors.Is(err, pve.ErrLBPackageBusy) || pve.LBHoldClass(err) != "package-manager" {
				t.Fatalf("a change while dpkg is at work on haproxy: %v", err)
			}
			if err := ps.PushKeepalived(ctx, "n1", 100, keepalived, "lo"); !errors.Is(err, pve.ErrLBPackageBusy) {
				t.Fatalf("keepalived's files while dpkg is at work on haproxy: %v", err)
			}
			if wrote, _ := g.writtenSince(before); len(wrote) != 0 {
				t.Fatalf("a push that waits wrote %v", wrote)
			}
			listens("while dpkg is at work on haproxy", vip+":80 "+vip+":8082")
			// dpkg ends its work with the package's own postinst. Then the change is taken.
			g.run(ctx, "systemctl stop fake-dpkg fake-apt; DEBIAN_FRONTEND=noninteractive dpkg --force-confold --configure haproxy >/dev/null 2>&1; [ \"$(dpkg-query -W -f='${Status}' haproxy)\" = 'install ok installed' ]")
			for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(500 * time.Millisecond) {
				if _, err = ps.PushPairHAProxy(ctx, "n1", 100, pve.RenderHAProxyOn(spec(80, 8083), []string{vip})); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the change after dpkg was done with haproxy: %v", err)
				}
			}
			listens("after dpkg was done with haproxy", vip+":80 "+vip+":8083")

			// 9. sshd over a reboot (drill R4-o1 of v0.11.1-rc.2: with both units disabled, cloud-init started
			// ssh.service at boot and sshd listened until the next HAProxy reload). The control is what v0.11.1-rc.2
			// did, a disable: the unit starts for whoever asks. Then any push of the product's masks it again, which
			// is how an appliance of an older release gets the mask; and it holds over a boot.
			g.run(ctx, "systemctl unmask ssh.service ssh.socket >/dev/null 2>&1; systemctl disable --now ssh ssh.socket >/dev/null 2>&1; systemctl start ssh.service")
			if got := g.run(ctx, lbSSHListens); got == "0" {
				t.Fatal("control: a disabled sshd did not start when asked")
			}
			if err := ps.PushKeepalived(ctx, "n1", 100, keepalived, "lo"); err != nil {
				t.Fatal(err)
			}
			sshOff("after a push on an appliance of an older release")
			if err := c.Stop(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if err := c.Start(ctx); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(5 * time.Minute); ; time.Sleep(2 * time.Second) {
				if code, out, _ := g.sh(ctx, "reboot", "systemctl", "is-system-running"); code == 0 || strings.TrimSpace(out) == "degraded" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("systemd did not come up after the reboot")
				}
			}
			sshOff("after a reboot")
			// And the product's own services came back by themselves.
			for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(time.Second) {
				if got := g.run(ctx, "systemctl is-active haproxy keepalived | sort -u"); got == "active" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("HAProxy and keepalived after a reboot: %s", g.run(ctx, "systemctl is-active haproxy keepalived | tr '\\n' ' '"))
				}
			}

			// 10. The install line of the first UDP listener, on the plain image: nftables, which the image lacks
			// (every other UDP test runs on the baked image, where it is in). The appliance serves when the line
			// runs, so it names nftables alone and upgrades nothing: HAProxy and keepalived keep their processes,
			// and sshd stays off. What the push does once the package is in is TestLBApplianceUDP's subject and
			// needs that test's networks and the kernel's IPVS: here its answer is only waited for.
			pids := func() string {
				return g.run(ctx, "systemctl show -p MainPID haproxy keepalived | tr '\\n' ' '")
			}
			had, pidsBefore := g.run(ctx, "dpkg-query -W -f='${Status}' nftables 2>/dev/null || true"), pids()
			lvs := pve.RenderLVS(pve.LBSpec{
				Listeners: []pve.LBListener{{Port: 5000, Protocol: "udp", Pool: "udp"}},
				Pools: []pve.LBPool{{Name: "udp", Protocol: "udp", Algorithm: "roundrobin",
					Members: []pve.LBMember{{ServerID: "101", Port: 5000, Weight: 1, Address: "10.0.0.11"}}}},
			}, []string{vip})
			for deadline := time.Now().Add(8 * time.Minute); ; time.Sleep(time.Second) {
				err := ps.PushLVS(ctx, "n1", 100, lvs, "eth0")
				if !errors.Is(err, pve.ErrLBNotReady) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the UDP service's install did not end: %v%s", err, g.mirror(ctx))
				}
			}
			if got := g.run(ctx, "dpkg-query -W -f='${Status}' nftables"); got != "install ok installed" {
				t.Fatalf("after the UDP service's install dpkg says %q of nftables", got)
			}
			if got := pids(); got != pidsBefore {
				t.Fatalf("the UDP service's install restarted HAProxy or keepalived: %s, before %s", got, pidsBefore)
			}
			sshOff("after the UDP service's install")
			t.Logf("the UDP service's install: nftables was %q before it; %s", had, g.run(ctx, "nft --version"))
		})
	}
}

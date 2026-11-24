package pve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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

// nftListed is what the nft stub prints for every call: the product's table as nft lists the two chains the state
// read looks at. nftListedBefore is the table of the releases before the input chain (v0.11.1-rc.1 to rc.3).
const (
	nftListedBefore = "table ip proxcloud-lb {\n\tchain lb-forward {\n\t\ttype filter hook forward priority filter; policy drop;\n\t}\n}\n"
	nftListed       = "table ip proxcloud-lb {\n\tchain lb-input {\n\t\ttype filter hook input priority 150; policy accept;\n" +
		"\t\tiifname != \"lo\" meta pkttype host meta l4proto udp ct state new counter packets 7 bytes 203 drop\n\t}\n" +
		"\tchain lb-forward {\n\t\ttype filter hook forward priority filter; policy drop;\n\t}\n}\n"
)

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
	g.Stub("nft", nftListed)
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
			// A table that holds the listener with a connection limit the config does not have (drill R3-F2 of
			// v0.11.1-rc.2): the appliance serves UDP and has a change it did not load, which is not "not served".
			holder(g)
			udp(g, vip+" 53")
			g.Stub("ipvsadm", "UDP  203.0.113.10:53 wrr\n  -> 10.77.0.11:5000              Masq    1      0          0\n")
			g.Stub("ipvsadm-thresholds", "UDP  203.0.113.10:53 wrr\n  -> 10.77.0.11:5000              5          0          0          0\n")
			if got := read(); !got.LVS || !got.LVSDiffers || !got.HoldsVIP || !got.Serving {
				t.Fatalf("a UDP table with a limit the config does not have: %+v, want served and differing", got)
			}
			g.Stub("ipvsadm-thresholds", "UDP  203.0.113.10:53 wrr\n  -> 10.77.0.11:5000              0          0          0          0\n")
			if got := read(); !got.LVS || got.LVSDiffers {
				t.Fatalf("a UDP table as the config has it: %+v", got)
			}
			// One that lacks a listener is not served, whatever else differs.
			g.Stub("ipvsadm-thresholds", "UDP  203.0.113.10:53 wrr\n  -> 10.77.0.11:5000              5          0          0          0\n")
			g.Write("/proc/net/ip_vs", "")
			if got := read(); got.LVS || got.LVSDiffers {
				t.Fatalf("a UDP table without the listener: %+v, want not served", got)
			}
			g.Unstub("ipvsadm")
			g.Unstub("ipvsadm-thresholds")
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
				// A table without the input chain's drop (a release before, or changed by hand) serves, and is not the
				// product's: its own answer, which makes the pass load the file again (lab finding T2-F1).
				{"the table of a release before", lvsServingFn + "lvsserving", func() {
					g.Write("/proc/sys/net/ipv4/ip_forward", "1\n")
					udp(g, "203.0.113.10 53")
					g.Stub("nft", nftListedBefore)
				}, 2, "the nftables table is not the one of the UDP service's file\n"},
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

// The track script keepalived runs every second, as the text the appliance gets, in dash and in bash. Drill Q1-o1 of
// v0.11.1-rc.1: at a reload HAProxy's master execs itself and its socket refuses connections for a moment; the
// script asked once, so a push that met that moment was one failed run on the master, and "fall 2" was all that
// kept the addresses there. It asks again now: a master that answers at the second, third or fourth try is no
// failed run, and one that never answers still is, within the second keepalived gives a run.
func TestTrackScriptAsksAgainDuringAReload(t *testing.T) {
	script := lbPairFiles("pub0")[lbCheckScript]
	for _, shell := range lbguesttest.Shells() {
		t.Run(shell, func(t *testing.T) {
			t.Parallel()
			g := lbguesttest.New(t)
			holder(g)
			run := func() (int, time.Duration) {
				start := time.Now()
				exit, _, _ := g.Run(shell, script)
				return exit, time.Since(start)
			}
			if exit, _ := run(); exit != 0 {
				t.Fatalf("HAProxy runs: exit %d", exit)
			}
			if asked, _ := os.ReadFile(g.Path("stub/socat-calls")); string(asked) != "x\n" {
				t.Fatalf("HAProxy runs: its master was asked %d times, want once", strings.Count(string(asked), "\n"))
			}
			for refused := 1; refused < lbWorkerTries; refused++ {
				g.Stub("socat-refuses", strconv.Itoa(refused))
				if exit, took := run(); exit != 0 {
					t.Fatalf("the master's socket refused %d connection(s), as during a reload: the run failed (exit %d after %s)", refused, exit, took)
				}
			}
			// Gone for good: the run fails after its tries. Their pauses add up to less than the second keepalived
			// gives a run (0.6 s: the real-guest test holds the time a stopped HAProxy costs; a loaded machine makes
			// a wall clock bound here a flaky one).
			g.Unstub("socat")
			g.Unstub("socat-calls")
			exit, took := run()
			asked, _ := os.ReadFile(g.Path("stub/socat-calls"))
			if exit != 1 || strings.Count(string(asked), "\n") != lbWorkerTries || took < 500*time.Millisecond {
				t.Fatalf("HAProxy's master never answers: exit %d after %s and %d questions, want 1 after %d", exit, took, strings.Count(string(asked), "\n"), lbWorkerTries)
			}
			if pauses := time.Duration(lbWorkerTries-1) * 200 * time.Millisecond; !strings.Contains(script, "sleep 0.2\n") || pauses >= 800*time.Millisecond {
				t.Fatalf("the tries pause for %s in all: a run must end well within keepalived's interval of 1 s", pauses)
			}
			holder(g)
			g.Stub("socat", "7 master 0 [failed: 0] 0d00h01m00s 3.2.4\n")
			if exit, _ := run(); exit != 1 {
				t.Fatalf("a master without a worker: exit %d", exit)
			}

			// The listener part is as it was: not asked for while a config is ten seconds new, asked for after.
			holder(g)
			g.Listen("203.0.113.10:443")
			if exit, _ := run(); exit != 0 {
				t.Fatalf("a config put in place just now, its first listener not up yet: exit %d", exit)
			}
			old := time.Now().Add(-time.Minute)
			if err := os.Chtimes(g.Path(haproxyConfig), old, old); err != nil {
				t.Fatal(err)
			}
			if exit, _ := run(); exit != 1 {
				t.Fatalf("HAProxy runs without the config's first listener: exit %d", exit)
			}
			g.Listen("203.0.113.10:80", "203.0.113.10:443")
			if exit, _ := run(); exit != 0 {
				t.Fatalf("HAProxy runs and listens: exit %d", exit)
			}
		})
	}
}

// Drill Q1-o2 of v0.11.1-rc.1: keepalived's support files were written onto their place by the guest agent, which
// empties a file and then fills it, and creates it with mode 0666; the track script, which keepalived runs every
// second, was seen empty on the lab. Every file of a push is now written beside its place, as <path>.new, and the
// apply moves it in by a rename with its mode set. Here the product's own pushes run against a guest whose apply
// scripts are run as text: no write of the agent's names a file's place, each file ends in place, whole, with its
// mode, nothing stays beside it, and a config keepalived refuses leaves the one in use.
func TestPushesMoveEveryFileIn(t *testing.T) {
	for _, shell := range lbguesttest.Shells() {
		t.Run(shell, func(t *testing.T) {
			ctx := context.Background()
			ps, g, a := scriptGuest(t, shell)
			holder(g)
			conf, err := RenderKeepalived(keepalivedFor(1))
			if err != nil {
				t.Fatal(err)
			}
			// placed: path is in place with content and mode, and nothing is left beside it.
			placed := func(when, path, content string, mode os.FileMode) {
				t.Helper()
				got, err := os.ReadFile(g.Path(path))
				st, serr := os.Stat(g.Path(path))
				if err != nil || serr != nil || string(got) != content || st.Mode().Perm() != mode {
					t.Errorf("%s: %s is %q with mode %v (%v), want the pushed file with mode %v", when, path, got, st.Mode().Perm(), err, mode)
				}
				if _, err := os.Stat(g.Path(path + ".new")); err == nil {
					t.Errorf("%s: %s.new was not moved in", when, path)
				}
			}
			onlyBeside := func(when string) {
				t.Helper()
				for _, w := range a.Writes {
					if !strings.HasSuffix(w, ".new") {
						t.Errorf("%s: the agent wrote %s onto its place", when, w)
					}
				}
				a.Writes = nil
			}
			// The files of the release before are in place and in use.
			const before = "of the release before\n"
			for path := range lbPairFiles("pub0") {
				g.Write(path, before)
			}
			g.Write("/etc/keepalived/keepalived.conf", before)

			// keepalived refuses the config: the one in use stays, and the push says so.
			g.Stub("keepalived-refuses", "(keepalived.conf.new: Line 3) Unknown keyword\n")
			if err := ps.PushKeepalived(ctx, "n1", 100, conf, "pub0"); err == nil || !strings.Contains(err.Error(), "Unknown keyword") {
				t.Fatalf("a config keepalived refuses: %v", err)
			}
			placedConf, _ := os.ReadFile(g.Path("/etc/keepalived/keepalived.conf"))
			if string(placedConf) != before {
				t.Fatalf("a config keepalived refused replaced the one in use: %q", placedConf)
			}
			onlyBeside("a refused config")
			g.Unstub("keepalived-refuses")

			if err := ps.PushKeepalived(ctx, "n1", 100, conf, "pub0"); err != nil {
				t.Fatalf("push keepalived: %v", err)
			}
			onlyBeside("keepalived's push")
			placed("keepalived's push", "/etc/keepalived/keepalived.conf", conf, 0o644)
			for path, content := range lbPairFiles("pub0") {
				mode := os.FileMode(0o644)
				if path == lbCheckScript || path == lbNotifyScript {
					mode = 0o755
				}
				placed("keepalived's push", path, content, mode)
			}

			// The UDP service's files, the same way. Its apply proves that the table holds the config's servers.
			udp(g, "203.0.113.10 53")
			lvs, _ := os.ReadFile(g.Path(lvsConfig))
			g.Write(lvsNft, before)
			if err := ps.PushLVS(ctx, "n1", 100, string(lvs), "ten0"); err != nil {
				t.Fatalf("push the UDP service: %v", err)
			}
			onlyBeside("the UDP service's push")
			placed("the UDP service's push", lvsConfig, string(lvs), 0o644)
			for _, f := range lvsFiles("ten0") {
				mode := os.FileMode(0o644)
				if f[0] == lvsNet {
					mode = 0o755
				}
				placed("the UDP service's push", f[0], f[1], mode)
			}
			if got := LVSHash(string(lvs), "ten0"); func() VRRPState { st, _ := ps.ReadVRRPState(ctx, "n1", 100, "203.0.113.10"); return st }().LVSSum != got {
				t.Errorf("after the push the guest's files do not sum to %s", got)
			}
		})
	}
}

// The notify script writes keepalived's state where the state read finds it, by a rename: no read meets an empty
// file, and nothing stays beside it.
func TestNotifyScriptWritesTheStateByARename(t *testing.T) {
	for _, shell := range lbguesttest.Shells() {
		g := lbguesttest.New(t)
		g.Write(lbStateFile, "BACKUP\n")
		script := lbPairFiles("pub0")[lbNotifyScript]
		if exit, out, stderr := g.Run(shell, script, "sh", "INSTANCE", "lb", "MASTER", "100"); exit != 0 {
			t.Fatalf("%s: exit %d %q %q", shell, exit, out, stderr)
		}
		got, _ := os.ReadFile(g.Path(lbStateFile))
		left, _ := filepath.Glob(g.Path(lbStateFile) + ".*")
		if string(got) != "MASTER\n" || len(left) != 0 || !strings.Contains(script, "mv -f") {
			t.Fatalf("%s: the state file holds %q, beside it %v", shell, got, left)
		}
	}
}

// keepalived's support files, as every appliance of a pair gets them. Their text is part of the hash a pair's row
// records (LBPairFilesDigest), so a change to this golden is a keepalived push to every appliance of every pair at
// the upgrade that carries it: one reload of keepalived each, the standby first. Change it knowingly, and say so in
// the release's upgrade note. v0.11.1-rc.2 changed the track script (it asks HAProxy's master again during a
// reload, drill Q1-o1) and the notify script (it writes the state by a rename); the sysctl file and the unit drop-in
// are those of v0.11.0.
func TestLBPairFilesGolden(t *testing.T) {
	golden(t, "lb-pair-files.txt", LBPairFilesDigest("ens19"))
	files := lbPairFiles("ens19")
	if got := files[lbSysctlFile] + files[lbDropIn]; got != "net.ipv4.ip_nonlocal_bind = 1\nnet.ipv4.conf.all.rp_filter = 2\nnet.ipv4.conf.default.rp_filter = 2\n"+
		"[Service]\nExecStartPre=/usr/sbin/ip link set dev ens19 up\n" {
		t.Errorf("the sysctl file or the unit drop-in changed:\n%s", got)
	}
}

// lvsdrift() and lvsset(), run as the text they are on a config RenderLVS made and on tables as ipvsadm prints them
// (the listings are copied from keepalived 2.3.3 and 2.3.4 with ipvsadm 1.31, Debian 13 and Ubuntu 26.04). Drill
// R3-F2 of v0.11.1-rc.2: a connection limit changed on a listener stayed in the file, the kernel kept the old one,
// the state read said "serving" and the row ACTIVE. What the kernel itself does with an edit is the real-guest
// test's (test/integration/lb_udp_test.go, TestLBApplianceUDPChanges).
func TestLVSDriftScript(t *testing.T) {
	spec := LBSpec{
		Listeners: []LBListener{{Port: 53, Protocol: "udp", Pool: "dns", ConnectionLimit: 50}},
		Pools: []LBPool{{Name: "dns", Protocol: "udp", Algorithm: "leastconn", SessionPersistence: &LBPersistence{Type: "source_ip", Timeout: 300},
			Members: []LBMember{{ServerID: "101", Port: 53, Weight: 3, Address: "10.77.0.11"}, {ServerID: "102", Port: 53, Weight: 1, Address: "10.77.0.12"},
				{ServerID: "103", Port: 53, Weight: 1, Address: "10.77.0.13"}}}},
	}
	config := RenderLVS(spec, []string{"203.0.113.10"})
	// table is the two listings of a table with these figures; the third member is out of it (its check fails).
	table := func(sched string, w1, u1, w2, u2 int) (plain, thresholds string) {
		head := "IP Virtual Server version 1.2.1 (size=4096)\nProt LocalAddress:Port Scheduler Flags\n"
		tcp := "TCP  203.0.113.10:53 wrr\n  -> 10.77.0.11:53                Masq    7      0          0         \n"
		plain = head + "  -> RemoteAddress:Port           Forward Weight ActiveConn InActConn\n" + tcp + "UDP  203.0.113.10:53 " + sched + "\n" +
			fmt.Sprintf("  -> 10.77.0.11:53                Masq    %d      0          0         \n  -> 10.77.0.12:53                Masq    %d      2          0         \n", w1, w2)
		thresholds = head + "  -> RemoteAddress:Port           Uthreshold Lthreshold ActiveConn InActConn\n" + strings.Replace(tcp, "Masq    7", "9          0", 1) + "UDP  203.0.113.10:53 " + sched + "\n" +
			fmt.Sprintf("  -> 10.77.0.11:53                %d         0          0          0         \n  -> 10.77.0.12:53                %d         0          2          0         \n", u1, u2)
		return plain, thresholds
	}
	const m1, m2 = "-e -u 203.0.113.10:53 -r 10.77.0.11:53 -m -w 3 -x 50 -y 0\n", "-e -u 203.0.113.10:53 -r 10.77.0.12:53 -m -w 1 -x 50 -y 0\n"
	for _, shell := range lbguesttest.Shells() {
		t.Run(shell, func(t *testing.T) {
			g := lbguesttest.New(t)
			udp(g, "203.0.113.10 53")
			g.Write(lvsConfig, config)
			for _, c := range []struct {
				name, sched    string
				w1, u1, w2, u2 int
				want           string
			}{
				{"the table as the config has it", "lc persistent 300", 3, 50, 1, 50, ""},
				{"a limit the kernel never got (R3-F2: added)", "lc persistent 300", 3, 0, 1, 0, m1 + m2},
				{"a limit the kernel kept (R3-F2: raised from 5)", "lc persistent 300", 3, 5, 1, 50, m1},
				{"a weight", "lc persistent 300", 3, 50, 2, 50, m2},
				{"the algorithm", "wrr persistent 300", 3, 50, 1, 50, "-E -u 203.0.113.10:53 -s lc -p 300\n"},
				{"the persistence timeout", "lc persistent 600", 3, 50, 1, 50, "-E -u 203.0.113.10:53 -s lc -p 300\n"},
				{"no persistence", "lc", 3, 50, 1, 50, "-E -u 203.0.113.10:53 -s lc -p 300\n"},
			} {
				plain, thresholds := table(c.sched, c.w1, c.u1, c.w2, c.u2)
				g.Stub("ipvsadm", plain)
				g.Stub("ipvsadm-thresholds", thresholds)
				g.Unstub("ipvsadm-calls")
				if exit, out, stderr := g.Run(shell, lvsDriftFn+"lvsdrift"); exit != 0 || out != c.want {
					t.Errorf("%s: lvsdrift exit %d said %q (stderr %q), want %q", c.name, exit, out, stderr, c.want)
				}
				// The state read: a table that differs is its own answer, 2, which is what makes a pass push
				// again; 1 is a listener that is not served (TestVRRPStateScriptWithAndWithoutUDP reads both).
				wantExit, wantSaid := 0, ""
				if c.want != "" {
					wantExit, wantSaid = 2, "the IPVS table does not hold a connection limit, a weight, an algorithm or a persistence as the UDP configuration has it\n"
				}
				if exit, out, _ := g.Run(shell, lvsServingFn+"lvsserving"); exit != wantExit || out != wantSaid {
					t.Errorf("%s: lvsserving exit %d said %q, want %d %q", c.name, exit, out, wantExit, wantSaid)
				}
				// The apply's part: one edit for each line, and never an error of its own.
				exit, _, stderr := g.Run(shell, "set -e\n"+lvsDriftFn+lvsSetFn+"lvsset")
				calls, _ := os.ReadFile(g.Path("stub/ipvsadm-calls"))
				if exit != 0 || string(calls) != c.want {
					t.Errorf("%s: lvsset exit %d (stderr %q) ran %q, want %q", c.name, exit, stderr, calls, c.want)
				}
			}
			// A listener without a limit and without persistence asks the kernel to have none: the removal of R3-F2.
			spec.Listeners[0].ConnectionLimit, spec.Pools[0].SessionPersistence = 0, nil
			g.Write(lvsConfig, RenderLVS(spec, []string{"203.0.113.10"}))
			plain, thresholds := table("lc persistent 300", 3, 5, 1, 5)
			g.Stub("ipvsadm", plain)
			g.Stub("ipvsadm-thresholds", thresholds)
			want := "-E -u 203.0.113.10:53 -s lc\n" + strings.ReplaceAll(m1+m2, "-x 50", "-x 0")
			if _, out, _ := g.Run(shell, lvsDriftFn+"lvsdrift"); out != want {
				t.Errorf("a removed limit and persistence: lvsdrift said %q, want %q", out, want)
			}
			// A member that enters between the two listings has no weight in the first: it is not compared, so a
			// state read never calls a table in motion a table that differs.
			g.Stub("ipvsadm", strings.Replace(plain, "  -> 10.77.0.12:53                Masq    1      2          0         \n", "", 1))
			if _, out, _ := g.Run(shell, lvsDriftFn+"lvsdrift"); out != "-E -u 203.0.113.10:53 -s lc\n"+strings.ReplaceAll(m1, "-x 50", "-x 0") {
				t.Errorf("a member in one listing only: lvsdrift said %q", out)
			}
		})
	}
}

// A push of the UDP service writes the files that are not on the appliance as they should be, and no other (drill
// R2a-1 of v0.11.1-rc.2: a member that came or went wrote all five files on each appliance, eight guest calls a
// push, and a UDP port added with a TCP port answered after 33 s). The product's own push runs against a guest
// whose scripts are run as text, and the guest's calls are counted: a member change is one write and three
// commands (ready, what is there, apply), where it was five writes and three commands.
//
// What the count cannot show is what a call costs on a real cluster (0.5 to 1 s each on the lab, by
// chain2-agentcalls.txt of the drill): that is the lab's.
func TestPushLVSWritesOnlyWhatDiffers(t *testing.T) {
	for _, shell := range lbguesttest.Shells() {
		t.Run(shell, func(t *testing.T) {
			ctx := context.Background()
			ps, g, a := scriptGuest(t, shell)
			holder(g)
			udp(g, "203.0.113.10 53") // the kernel's side, and files that are replaced below
			for _, f := range append(lvsFiles("ten0"), [2]string{lvsConfig, ""}) {
				g.Remove(f[0])
			}
			// A wait in an apply is no wait here.
			if err := os.WriteFile(g.Path("bin/sleep"), []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			spec := LBSpec{Listeners: []LBListener{{Port: 53, Protocol: "udp", Pool: "dns"}},
				Pools: []LBPool{{Name: "dns", Protocol: "udp", Algorithm: "roundrobin", Members: []LBMember{{ServerID: "101", Port: 53, Weight: 1, Address: "10.77.0.11"}}}}}
			one := RenderLVS(spec, []string{"203.0.113.10"})
			spec.Pools[0].Members = append(spec.Pools[0].Members, LBMember{ServerID: "102", Port: 53, Weight: 1, Address: "10.77.0.12"})
			two := RenderLVS(spec, []string{"203.0.113.10"})
			// push answers the files written and the commands run.
			push := func(config string) (writes []string, execs int, err error) {
				t.Helper()
				a.Writes = nil
				n := a.Execs()
				err = ps.PushLVS(ctx, "n1", 100, config, "ten0")
				return a.Writes, a.Execs() - n, err
			}
			placed := func(when, config string) {
				t.Helper()
				for _, f := range append(lvsFiles("ten0"), [2]string{lvsConfig, config}) {
					mode := os.FileMode(0o644)
					if f[0] == lvsNet {
						mode = 0o755
					}
					got, _ := os.ReadFile(g.Path(f[0]))
					st, err := os.Stat(g.Path(f[0]))
					if err != nil || string(got) != f[1] || st.Mode().Perm() != mode {
						t.Errorf("%s: %s is not the pushed file with mode %v (%v)", when, f[0], mode, err)
					}
					if _, err := os.Stat(g.Path(f[0] + ".new")); err == nil {
						t.Errorf("%s: %s.new was not moved in", when, f[0])
					}
				}
			}
			want := func(when string, writes []string, execs int, err error, files ...string) {
				t.Helper()
				for i := range files {
					files[i] += ".new"
				}
				if err != nil || !slices.Equal(writes, files) || execs != 3 {
					t.Errorf("%s: wrote %q with %d commands (%v), want %q with 3", when, writes, execs, err, files)
				}
			}
			// The first push: every file.
			writes, execs, err := push(one)
			want("the first push", writes, execs, err, lvsNft, lvsSysctl, lvsNet, lvsUnit, lvsDropIn, lvsConfig)
			placed("the first push", one)
			// A member comes: the config alone.
			writes, execs, err = push(two)
			want("a member added", writes, execs, err, lvsConfig)
			placed("a member added", two)
			// Pushed again with nothing to write, as after a state read that said "not served".
			writes, execs, err = push(two)
			want("pushed again", writes, execs, err)
			// A file changed by hand, and what an interrupted push left beside another: the one file is written
			// again, and the leftover never reaches its place.
			g.Write(lvsNft, "by hand\n")
			g.Write(lvsSysctl+".new", "half of a file\n")
			writes, execs, err = push(two)
			want("a file changed by hand", writes, execs, err, lvsNft)
			placed("a file changed by hand", two)
			// The unit restarted once, with the unit file of the first push, and reloaded for every push after it.
			if calls, _ := os.ReadFile(g.Path("stub/systemctl-calls")); strings.Count(string(calls), "restart "+lvsService) != 1 || strings.Count(string(calls), "reload "+lvsService) != 3 {
				t.Errorf("the unit's restarts and reloads over a first push and three more:\n%s", calls)
			}

			// A push the appliance does not come to serve. With no new config (pushed again, the table does not
			// load): the config in use stays, and the unit is left alone. With a new one: the one before is back.
			g.Unstub("nft")
			if _, _, err = push(two); err == nil || !strings.Contains(err.Error(), "the forwarding filter is not loaded") {
				t.Fatalf("a push that is not served: %v", err)
			}
			placed("a push again that was not served", two)
			if _, _, err = push(one); err == nil {
				t.Fatal("a changed config that is not served was taken")
			}
			placed("a changed config that was not served", two)
			if calls, _ := os.ReadFile(g.Path("stub/systemctl-calls")); strings.Contains(string(calls), "disable -q --now "+lvsService) {
				t.Errorf("a push that was not served disabled a unit that has a config to go back to:\n%s", calls)
			}
			// A config keepalived refuses: nothing moves.
			g.Stub("nft", nftListed)
			g.Stub("keepalived-refuses", "(lvs.conf.new: Line 3) Unknown keyword\n")
			if _, _, err = push(one); err == nil || !strings.Contains(err.Error(), "Unknown keyword") {
				t.Fatalf("a config keepalived refuses: %v", err)
			}
			if got, _ := os.ReadFile(g.Path(lvsConfig)); string(got) != two {
				t.Errorf("a config keepalived refused replaced the one in use")
			}
		})
	}
}

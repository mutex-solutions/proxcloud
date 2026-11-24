package pve

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve/lbguesttest"
)

// lbReadyRC2 is the ready check of v0.11.1-rc.2, the control: "a package manager runs" is asked first, so an
// appliance whose packages were installed long ago is busy for as long as the image's own apt timers run.
func lbReadyRC2(packages, units string) string {
	return "pgrep -x 'apt-get|apt|dpkg' >/dev/null && exit 3\n" +
		"for p in " + packages + "; do [ \"$(dpkg-query -W -f='${Status}' $p 2>/dev/null)\" = 'install ok installed' ] || exit 1; done\n" +
		"command -v ss >/dev/null && systemctl cat " + units + " >/dev/null 2>&1"
}

// The ready check's own text decides whether a change is taken, so it runs here as the text it is (dash and bash):
// installed is asked before busy. An installed appliance is ready while apt runs (drill R3-F1 and R2b-1 of
// v0.11.1-rc.2), and waits in one case only, dpkg at work on a package of its own; the first install waits for the
// package manager as it did.
func TestReadyCheckAsksInstalledBeforeBusy(t *testing.T) {
	shells := lbguesttest.Shells()
	if len(shells) == 0 {
		t.Skip("no dash or bash on this machine")
	}
	if strconv.Itoa(lbBusyExit) != lbBusyExitText {
		t.Fatalf("lbBusyExit %d and its text %q differ", lbBusyExit, lbBusyExitText)
	}
	cases := []struct {
		name      string
		apt       bool              // a process named apt-get, apt or dpkg runs
		packages  map[string]string // what dpkg says of a package, "install ok installed" when not named
		want, rc2 int
	}{
		{"installed, nothing runs", false, nil, 0, 0},
		{"installed, the image's apt timer runs", true, nil, 0, 3},
		{"dpkg has unpacked a new haproxy", true, map[string]string{"haproxy": "install ok unpacked"}, lbBusyExit, 3},
		{"dpkg configures keepalived", true, map[string]string{"keepalived": "install ok half-configured"}, lbBusyExit, 3},
		{"first install under way", true, map[string]string{"haproxy": "", "socat": "", "keepalived": ""}, lbBusyExit, 3},
		{"nothing installed, nothing runs: install", false, map[string]string{"haproxy": "", "socat": "", "keepalived": ""}, 1, 1},
		{"a package left half-configured, nothing runs: install", false, map[string]string{"haproxy": "install ok half-configured"}, 1, 1},
	}
	for _, shell := range shells {
		for _, c := range cases {
			t.Run(shell+"/"+c.name, func(t *testing.T) {
				g := lbguesttest.New(t)
				if c.apt {
					g.Stub("pgrep", "apt-get\n")
				}
				for p, status := range c.packages {
					g.Stub("dpkg-"+p, status+"\n")
				}
				if exit, _, stderr := g.Run(shell, lbPairReady); exit != c.want {
					t.Errorf("the ready check answered %d, want %d (%s)", exit, c.want, stderr)
				}
				// The control: the check of v0.11.1-rc.2 on the same guest.
				if exit, _, _ := g.Run(shell, lbReadyRC2("haproxy socat keepalived", "haproxy.service keepalived.service")); exit != c.rc2 {
					t.Errorf("the check of v0.11.1-rc.2 answered %d, want %d", exit, c.rc2)
				}
			})
		}
	}
}

// aptGuest is an appliance that serves old, on which a push of scriptHAProxy can be proven: HAProxy's master answers a
// reload with Success=1 and the new config's listeners are there.
func aptGuest(t *testing.T, shell, old string) (*Service, *lbguesttest.Guest, *lbguesttest.Agent) {
	t.Helper()
	ps, g, a := scriptGuest(t, shell)
	g.Write(haproxyConfig, old)
	g.Stub("active", "haproxy\nkeepalived\n")
	g.Stub("socat", "Success=1\n")
	g.Listen("203.0.113.10:80", "203.0.113.10:443")
	return ps, g, a
}

// A change is taken while the image's package manager runs, through the product's own push and with every script run
// as its text. It waits in the one case a running dpkg matters: the package of the service the apply is about to
// reload is being upgraded, which ends with the package's own restart. Then nothing is written; an apply that meets
// the upgrade after its ready check touches nothing; and a swap that failed while dpkg was at the package is held and
// tried again, not reported as a config HAProxy refused.
func TestChangeIsTakenWhileThePackageManagerRuns(t *testing.T) {
	shells := lbguesttest.Shells()
	if len(shells) == 0 {
		t.Skip("no dash or bash on this machine")
	}
	const old = "global\n\nfrontend l80\n  bind 203.0.113.10:80\n"
	read := func(g *lbguesttest.Guest, path string) string {
		raw, _ := os.ReadFile(g.Path(path))
		return string(raw)
	}
	for _, shell := range shells {
		t.Run(shell, func(t *testing.T) {
			ctx := context.Background()
			ps, g, a := aptGuest(t, shell, old)

			// apt-daily runs, the packages are installed: the push is taken and served.
			g.Stub("pgrep", "apt-get\n")
			if err := ps.PushPairHAProxy(ctx, "n1", 100, scriptHAProxy); err != nil {
				t.Fatalf("a push while apt runs: %v", err)
			}
			if read(g, haproxyConfig) != scriptHAProxy || !slices.Equal(a.Writes, []string{haproxyConfig + ".new"}) {
				t.Fatalf("the config was not moved in: wrote %v", a.Writes)
			}
			// dpkg has unpacked a new haproxy and has not configured it yet: held, as that and nothing else, and
			// nothing is written.
			g.Write(haproxyConfig, old)
			g.Stub("dpkg-haproxy", "install ok unpacked\n")
			err := ps.PushPairHAProxy(ctx, "n1", 100, scriptHAProxy)
			if !errors.Is(err, ErrLBPackageBusy) || !errors.Is(err, ErrLBNotReady) || LBHoldClass(err) != "package-manager" {
				t.Fatalf("a push during an upgrade of haproxy: %v", err)
			}
			if len(a.Writes) != 1 || read(g, haproxyConfig) != old {
				t.Fatalf("a held push wrote %v", a.Writes[1:])
			}

			// The upgrade starts between the ready check and the apply: the apply itself asks, and touches nothing.
			g.Write(haproxyConfig+".new", scriptHAProxy)
			if exit, _, stderr := g.Run(shell, haproxyReload); exit != lbBusyExit {
				t.Fatalf("an apply during an upgrade of haproxy: exit %d %s", exit, stderr)
			}
			if read(g, haproxyConfig) != old || read(g, haproxyConfig+".new") != scriptHAProxy {
				t.Fatal("an apply that waits changed a file")
			}

			// The swap meets the package's own restart: HAProxy's master does not take the reload, and dpkg is at
			// the package by then. The old file is back, and the answer is "held".
			g.Stub("socat", "Success=0\n")
			g.Stub("dpkg-haproxy", "install ok installed\ninstall ok half-configured\n")
			if exit, _, stderr := g.Run(shell, haproxyReload); exit != lbBusyExit {
				t.Fatalf("a swap that lost to the package's restart: exit %d %s", exit, stderr)
			}
			if read(g, haproxyConfig) != old {
				t.Fatal("the old config is not back after a swap that failed")
			}
			// The control: the same failed swap with no dpkg at the package is a config HAProxy did not take.
			g.Unstub("dpkg-haproxy")
			g.Write(haproxyConfig+".new", scriptHAProxy)
			if exit, _, _ := g.Run(shell, haproxyReload); exit != 1 {
				t.Fatalf("a swap HAProxy refused: exit %d, want 1", exit)
			}

			// keepalived's two applies wait for keepalived's own package, and move nothing in.
			g.Stub("dpkg-keepalived", "install ok half-configured\n")
			g.Write("/etc/keepalived/keepalived.conf.new", "vrrp_instance lb {\n}\n")
			g.Write(lvsConfig+".new", "instance lvs\n")
			for name, script := range map[string]string{"keepalived": lbKeepalivedApply, "udp": lvsApply} {
				if exit, _, stderr := g.Run(shell, script); exit != lbBusyExit {
					t.Errorf("the %s apply during an upgrade of keepalived: exit %d %s", name, exit, stderr)
				}
			}
			if read(g, "/etc/keepalived/keepalived.conf") != "" || read(g, lvsConfig) != "" {
				t.Error("an apply that waits moved a file in")
			}
			if err := ps.PushKeepalived(ctx, "n1", 100, "vrrp_instance lb {\n}\n", "pub0"); !errors.Is(err, ErrLBPackageBusy) {
				t.Errorf("PushKeepalived during an upgrade of keepalived: %v", err)
			}
		})
	}
}

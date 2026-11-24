package project

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// lbDigestRe finds a SHA-256 digest in a text.
var lbDigestRe = regexp.MustCompile(`[0-9a-f]{64}`)

// The log has one line for every push an appliance took (which balancer, which VM, what, keepalived's role, the place
// in the order) and one for every push held or refused, with the class of the reason (drill R1-o1 of v0.11.1-rc.2:
// the order of a push could only be read from Proxmox's access log). No line holds a config's text or a digest: the
// reason of a refusal is the guest's own words, which may quote a config, and it goes into the row's message only.
// (That no line holds a key is TestLBCertificatesReachThePair's, which pushes certificates.)
func TestLBPushesAreLogged(t *testing.T) {
	ctx := context.Background()
	h, f, es, lb := lbApplyFixture(t)
	proj, cr := &Project{ID: "p1"}, &cloud.CloudResource{}
	var logged bytes.Buffer
	before := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(before)
	port := 80
	change := func() string {
		t.Helper()
		port++
		lb.Listeners = append(lb.Listeners, pve.LBListener{Port: port, Pool: "web"})
		logged.Reset()
		if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil {
			t.Fatal(err)
		}
		return logged.String()
	}
	has := func(text string, lines ...string) {
		t.Helper()
		for _, want := range lines {
			if !strings.Contains(text, want) {
				t.Errorf("the log lacks %s:\n%s", want, text)
			}
		}
	}
	// The first apply: keepalived and HAProxy on each appliance, the standby first.
	logged.Reset()
	if err := h.lbApplyPair(ctx, es, proj, cr, lb); err != nil || lb.Status != lbStatusActive {
		t.Fatalf("first apply: %s %v", lb.Status, err)
	}
	first := logged.String()
	has(first, `level=INFO msg="load balancer: push taken" lb=abc vm=301 push=keepalived`, `vm=301 push=haproxy`, `vm=302 push=keepalived`, `vm=302 push=haproxy`)
	if strings.Index(first, "vm=301 push=haproxy") > strings.Index(first, "vm=302 push=keepalived") {
		t.Errorf("the log shows the holder before the standby:\n%s", first)
	}
	// A change: one line per appliance, with the role it was read with and its place.
	has(change(),
		`level=INFO msg="load balancer: push taken" lb=abc vm=301 push=haproxy role=BACKUP holdsAddress=false position=1 of=2`,
		`level=INFO msg="load balancer: push taken" lb=abc vm=302 push=haproxy role=MASTER holdsAddress=true position=2 of=2`)
	// Held: dpkg is at work on HAProxy's own package on the holder.
	f.set("302", func(g *lbGuest) { g.upgrading = true })
	has(change(), `vm=301 push=haproxy role=BACKUP`, `level=WARN msg="load balancer: push held" lb=abc vm=302 push=haproxy role=MASTER class=package-manager`)
	// Its guest agent does not answer.
	f.set("302", func(g *lbGuest) { g.upgrading, g.agentDown = false, true })
	held := change()
	f.set("302", func(g *lbGuest) { g.agentDown = false })
	// Refused: the guest's words quote the config, and stay out of the log.
	f.set("301", func(g *lbGuest) {
		g.applyFails = "[ALERT] (1) : config : parsing [/etc/haproxy/haproxy.cfg:17] : 'bind 203.0.113.10:99' refused"
	})
	refused := change()
	has(refused, `vm=301 push=haproxy role=BACKUP class=refused`)
	if !strings.Contains(lb.StatusMessage, "bind 203.0.113.10:99") {
		t.Errorf("the reason of the refusal is not in the row: %q", lb.StatusMessage)
	}
	f.set("301", func(g *lbGuest) { g.applyFails = "" })
	all := first + held + refused + change()
	for _, secret := range []string{"bind ", "frontend", "ALERT", "server-state", lb.AppliedHash, lb.Appliances[0].KeepalivedHash} {
		if strings.Contains(all, secret) {
			t.Errorf("a log line holds %q:\n%s", secret, all)
		}
	}
	if d := lbDigestRe.FindString(all); d != "" {
		t.Errorf("a log line holds a digest (%s...):\n%s", d[:8], all)
	}

	// A balancer on one VM: the same lines.
	f.mu.Lock()
	delete(f.guests, "302")
	f.mu.Unlock()
	one := &loadBalancer{ID: "abc", Name: "one", Status: lbStatusBuild, NetworkID: "v1", PublicNetworkID: "vmbr1", ServerID: "301",
		PrivateIP: "10.0.0.31", PublicIP: "203.0.113.10",
		LBSpec: pve.LBSpec{Listeners: []pve.LBListener{{Port: 80, Pool: "web"}}, Pools: []pve.LBPool{{Name: "web", Protocol: "http", Algorithm: "roundrobin"}}}}
	logged.Reset()
	if err := h.lbApplyLocked(ctx, es, proj, cr, one); err != nil || one.Status != lbStatusActive {
		t.Fatalf("a balancer on one VM: %s %q %v", one.Status, one.StatusMessage, err)
	}
	has(logged.String(), `level=INFO msg="load balancer: push taken" lb=abc vm=301 push=haproxy role=single position=1 of=1`)
	f.set("301", func(g *lbGuest) { g.upgrading = true })
	one.Listeners = append(one.Listeners, pve.LBListener{Port: 81, Pool: "web"})
	logged.Reset()
	if err := h.lbApplyLocked(ctx, es, proj, cr, one); err != nil {
		t.Fatal(err)
	}
	has(logged.String(), `level=WARN msg="load balancer: push held" lb=abc vm=301 push=haproxy role=single class=package-manager`)
}

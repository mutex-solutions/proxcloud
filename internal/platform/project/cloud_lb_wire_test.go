package project

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// lbGuest is one appliance as its guest is: what dpkg says, what HAProxy runs next to what its file holds, and
// keepalived. The drill of v0.11.0-rc.1 found the product's view apart from this one in three ways, and the fake
// keeps them apart too: binaries that exist while dpkg still configures the package (packages "unpacked"), an
// HAProxy whose master has a worker and which runs the package's config (loaded != file), and a stopped service on a
// running VM. A stopped VM has no agent at all.
type lbGuest struct {
	stopped    bool   // the VM is stopped: its status says so, and the guest agent does not answer
	agentDown  bool   // the VM runs and its agent does not answer
	packages   string // "unpacked": the binaries exist, dpkg is not done; "installed"
	aptRunning bool   // a package manager runs in the guest
	haproxy    bool   // the unit is active: the master answers and has a worker
	file       string // haproxy.cfg
	pending    string // haproxy.cfg.new
	loaded     string // the config the running HAProxy read; not file after the package's own first start
	applyFails string // what an apply's proof says when it fails: the file is not taken
	keepalived bool
	role       string // MASTER or BACKUP while keepalived and HAProxy run
	holds      bool
}

// serving is the appliance's own answer to "does HAProxy listen on every bind of its config".
func (g *lbGuest) serving() bool { return g.haproxy && g.loaded == g.file }

// pairWire fakes the Proxmox API around two appliances, 301 on n1 and 302 on n2, as far as an apply reaches: their
// state, config and NICs, their firewall, and the guest agent in front of an lbGuest each. It logs every write in
// order. A guest command is answered from what the guest is, by what the command asks: a ready check that asks dpkg
// hears dpkg's answer, one that only asks whether the binaries exist hears that they do.
type pairWire struct {
	t      *testing.T
	tag    string // the appliances' balancer tag, pc-lb-abc when empty
	gone   bool   // no VM is listed any more
	mu     sync.Mutex
	log    []string
	rules  map[string][]pve.FirewallRule
	sets   map[string][]string
	guests map[string]*lbGuest
	runs   map[int]lbRun
	files  []string // the HAProxy configs written, in order
	reads  int      // state reads
}

type lbRun struct {
	exit     int
	out, err string
}

// newPairWire is a pair as a finished install leaves it: both appliances ready, 301 the standby, 302 holding the
// address.
func newPairWire(t *testing.T, tag string) *pairWire {
	return &pairWire{t: t, tag: tag, rules: map[string][]pve.FirewallRule{}, sets: map[string][]string{}, runs: map[int]lbRun{},
		guests: map[string]*lbGuest{
			"301": {packages: "installed", haproxy: true, role: "BACKUP"},
			"302": {packages: "installed", haproxy: true, role: "MASTER", holds: true},
		}}
}

// exec answers one guest command.
func (f *pairWire) exec(vm string, g *lbGuest, cmd string) lbRun {
	switch {
	case strings.Contains(cmd, "apt-get install"): // the install line, left running
		f.log = append(f.log, vm+" install")
		return lbRun{}
	case strings.Contains(cmd, "dpkg-query"): // the ready check
		switch {
		case g.aptRunning:
			return lbRun{exit: 3}
		case g.packages != "installed":
			return lbRun{exit: 1}
		}
		return lbRun{}
	case strings.Contains(cmd, "command -v haproxy"): // a ready check that only asks for the binaries (v0.11.0-rc.1)
		if g.packages == "" {
			return lbRun{exit: 1}
		}
		return lbRun{}
	case strings.Contains(cmd, "keepalived -t"):
		f.log = append(f.log, vm+" keepalived reload")
		g.keepalived = true
		if g.role == "" {
			g.role = "BACKUP"
		}
		return lbRun{}
	case strings.Contains(cmd, "haproxy -c"):
		f.log = append(f.log, vm+" haproxy reload")
		if g.applyFails != "" {
			return lbRun{exit: 1, err: g.applyFails}
		}
		g.file, g.loaded, g.haproxy = g.pending, g.pending, true
		return lbRun{}
	case strings.Contains(cmd, "proxcloud-lb.state"):
		f.reads++
		role, h, v, c := g.role, 0, 0, 0
		switch {
		case !g.keepalived:
			role = "STOPPED"
		case !g.haproxy:
			role = "FAULT" // the track script
		}
		if g.haproxy {
			h = 1
		}
		if g.holds && g.keepalived && g.haproxy {
			v = 1
		}
		if g.serving() {
			c = 1
		}
		return lbRun{out: fmt.Sprintf("%s %d %d %d", role, h, v, c)}
	case strings.Contains(cmd, "show stat"): // what the running HAProxy holds, not what the file says
		if !g.haproxy {
			return lbRun{exit: 1, err: "socat: /run/haproxy/admin.sock: Connection refused"}
		}
		out, px := "# pxname,svname,status,weight\n", ""
		for _, line := range strings.Split(g.loaded, "\n") {
			switch w := strings.Fields(line); {
			case len(w) < 2:
			case w[0] == "frontend":
				out += w[1] + ",FRONTEND,OPEN,\n"
			case w[0] == "backend":
				px = w[1]
				out += px + ",BACKEND,UP,1\n"
			case w[0] == "server":
				out += px + "," + w[1] + ",UP,1\n"
			}
		}
		return lbRun{out: out}
	case strings.Contains(cmd, "serving && echo ok"): // a single appliance's state
		f.reads++
		switch {
		case !g.haproxy:
			return lbRun{exit: 1, out: "HAProxy is not running"}
		case !g.serving():
			return lbRun{exit: 1, out: "HAProxy does not listen on 0.0.0.0:80"}
		}
		return lbRun{out: "ok"}
	}
	return lbRun{}
}

func (f *pairWire) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	write := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	tag := f.tag
	if tag == "" {
		tag = "pc-lb-abc"
	}
	if r.URL.Path == "/api2/json/cluster/resources" {
		if f.gone {
			write([]map[string]any{})
			return
		}
		var out []map[string]any
		for i, vm := range []string{"301", "302"} {
			if g := f.guests[vm]; g != nil {
				status := "running"
				if g.stopped {
					status = "stopped"
				}
				vmid, _ := strconv.Atoi(vm)
				out = append(out, map[string]any{"id": "qemu/" + vm, "type": "qemu", "vmid": vmid, "node": fmt.Sprintf("n%d", i+1), "pool": "pc-p1",
					"status": status, "tags": "proxcloud;" + tag})
			}
		}
		write(out)
		return
	}
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/api2/json/nodes/"), "/") // n, qemu, vmid, ...
	if len(p) >= 2 && p[1] == "tasks" {
		write(map[string]any{"status": "stopped", "exitstatus": "OK"})
		return
	}
	if len(p) < 4 || p[1] != "qemu" || f.guests[p[2]] == nil {
		f.t.Errorf("reached PVE: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	vm, rest := p[2], strings.Join(p[3:], "/")
	g := f.guests[vm]
	if strings.HasPrefix(rest, "agent/") && (g.stopped || g.agentDown) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"data":null,"message":"QEMU guest agent is not running\n"}`))
		return
	}
	mac := func(nic string) string { return fmt.Sprintf("bc:24:11:00:%s:%s", nic, vm[1:]) }
	switch r.Method + " " + rest {
	case "GET status/current":
		if g.stopped {
			write(map[string]any{"status": "stopped", "qmpstatus": "stopped", "tags": "proxcloud;" + tag})
			return
		}
		write(map[string]any{"status": "running", "qmpstatus": "running", "tags": "proxcloud;" + tag})
	case "POST status/start":
		g.stopped = false
		f.log = append(f.log, vm+" start")
		write("UPID:" + p[0] + ":00000001:00000002:00000003:qmstart:" + vm + ":t@pve:")
	case "GET config":
		write(map[string]any{"vga": "std", "net0": "virtio=" + mac("00") + ",bridge=v1", "net1": "virtio=" + mac("01") + ",bridge=vmbr1"})
	case "GET agent/network-get-interfaces":
		write(map[string]any{"result": []map[string]any{{"name": "lo"}, {"name": "ens18", "hardware-address": mac("00")}, {"name": "ens19", "hardware-address": mac("01")}}})
	case "GET firewall/rules":
		out := slices.Clone(f.rules[vm])
		for i := range out {
			out[i].Pos = i
		}
		write(out)
	case "POST firewall/rules":
		f.rules[vm] = append([]pve.FirewallRule{{Type: r.Form.Get("type"), Action: r.Form.Get("action"), Proto: r.Form.Get("proto"),
			Source: r.Form.Get("source"), Dest: r.Form.Get("dest"), DPort: r.Form.Get("dport"), Comment: r.Form.Get("comment")}}, f.rules[vm]...)
		f.log = append(f.log, vm+" rule "+r.Form.Get("proto"))
		write(nil)
	case "GET firewall/ipset":
		write([]map[string]any{{"name": "ipfilter-net1"}})
	case "GET firewall/ipset/ipfilter-net1":
		var out []map[string]any
		for _, c := range f.sets[vm] {
			out = append(out, map[string]any{"cidr": c})
		}
		write(out)
	case "POST firewall/ipset/ipfilter-net1":
		f.sets[vm] = append(f.sets[vm], r.Form.Get("cidr"))
		f.log = append(f.log, vm+" ipset "+r.Form.Get("cidr"))
		write(nil)
	case "POST agent/file-write":
		if strings.HasSuffix(r.Form.Get("file"), "haproxy.cfg.new") {
			f.files = append(f.files, r.Form.Get("content"))
			g.pending = r.Form.Get("content")
		}
		f.log = append(f.log, vm+" write "+r.Form.Get("file"))
		write(nil)
	case "POST agent/exec":
		pid := len(f.runs) + 1
		f.runs[pid] = f.exec(vm, g, strings.Join(r.Form["command"], " "))
		write(map[string]any{"pid": pid})
	case "GET agent/exec-status":
		pid, _ := strconv.Atoi(r.Form.Get("pid"))
		run := f.runs[pid]
		write(map[string]any{"exited": 1, "exitcode": run.exit, "out-data": run.out, "err-data": run.err})
	default:
		switch {
		case r.Method == http.MethodDelete && strings.HasPrefix(rest, "firewall/rules/"):
			pos, _ := strconv.Atoi(strings.TrimPrefix(rest, "firewall/rules/"))
			f.rules[vm] = slices.Delete(f.rules[vm], pos, pos+1)
			f.log = append(f.log, vm+" rule delete")
			write(nil)
		case r.Method == http.MethodDelete && strings.HasPrefix(rest, "firewall/ipset/ipfilter-net1/"):
			cidr := strings.TrimPrefix(rest, "firewall/ipset/ipfilter-net1/")
			f.sets[vm] = slices.DeleteFunc(f.sets[vm], func(c string) bool { return strings.TrimSuffix(c, "/32") == cidr })
			f.log = append(f.log, vm+" ipset delete "+cidr)
			write(nil)
		default:
			f.t.Errorf("reached PVE: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}
}

func (f *pairWire) first(entry string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Index(f.log, entry)
}

// count is how often entry was logged.
func (f *pairWire) count(entry string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, l := range f.log {
		if l == entry {
			n++
		}
	}
	return n
}

// set changes a guest under the fake's lock.
func (f *pairWire) set(vm string, change func(*lbGuest)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f.guests[vm])
}

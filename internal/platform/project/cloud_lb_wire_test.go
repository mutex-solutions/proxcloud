package project

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve/lbguesttest"
)

// lbGuest is one appliance as its guest is: what dpkg says, what HAProxy runs next to what its file holds, and
// keepalived. The drill of v0.11.0-rc.1 found the product's view apart from this one in three ways, and the fake
// keeps them apart too: binaries that exist while dpkg still configures the package (packages "unpacked"), an
// HAProxy whose master has a worker and which runs the package's config (loaded != file), and a stopped service on a
// running VM. A stopped VM has no agent at all, and neither has a paused one, which Proxmox lists as "paused".
//
// The drill of v0.11.0-rc.2 added the members (N3-8): HAProxy holds a state for each, a reload starts every member UP
// whatever its checks said before, unless the config reads the state file and the apply saved the state to it just
// before, and then only for a member the new config has under the same backend and server name.
type lbGuest struct {
	stopped    bool   // the VM is stopped: its status says so, and the guest agent does not answer
	paused     bool   // the VM is paused: Proxmox lists it as "paused", its qmpstatus says so, no agent answers
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
	dead       []string          // members no check can reach, by address:port
	members    map[string]string // what the running HAProxy holds of each member, "backend/server" -> UP or DOWN
	saved      map[string]string // the state file: what the last apply saved, nil when there is none
	// The UDP service (v0.11.1). disk holds every file the agent wrote, by path: the state read sums them as the
	// guest does, so a file changed behind the product shows. noNft: the image lacks the nftables package (the
	// install line of a pair with a UDP listener brings it). lvs: proxcloud-lvs.service runs and the IPVS table
	// holds the servers of its config. lvsFails: what keepalived says of a config it refuses.
	disk     map[string]string
	noNft    bool
	lvs      bool
	lvsFails string
	box      *lbguesttest.Guest // where the state read's script runs (stateRead)
}

// lbGuestKeepalived is the VRRP keepalived's config on a guest, and lbGuestBind finds a config's first bind of an
// address of its own.
const lbGuestKeepalived = "/etc/keepalived/keepalived.conf"

var lbGuestBind = regexp.MustCompile(`(?m)^\s*bind ([1-9][0-9.]*:[0-9]+)`)

// The UDP service's files, in the order the guest sums them (pve's lvssum).
var lbGuestLVSFiles = []string{"/etc/keepalived/lvs.conf", "/etc/proxcloud-lb/lvs.nft", "/etc/proxcloud-lb/lvs.sysctl", "/usr/local/sbin/proxcloud-lvs-net",
	"/etc/systemd/system/proxcloud-lvs.service"}

// keepalived's config and its support files (pve's lbPairFiles).
var lbGuestPairFiles = []string{"/etc/keepalived/keepalived.conf", "/usr/local/sbin/proxcloud-lb-check", "/usr/local/sbin/proxcloud-lb-notify",
	"/etc/sysctl.d/90-proxcloud-lb.conf", "/etc/systemd/system/keepalived.service.d/proxcloud.conf"}

// moveIn is an apply moving in the files the agent wrote beside their place, as <path>.new: every file of the
// product's reaches its place by a rename (drill Q1-o2 of v0.11.1).
func (g *lbGuest) moveIn(paths ...string) {
	for _, path := range paths {
		if content, ok := g.disk[path+".new"]; ok {
			g.disk[path] = content
			delete(g.disk, path+".new")
		}
	}
}

// sum is what sha256sum says of a file of the guest, "-" for one that is not there.
func (g *lbGuest) sum(path string) string {
	if content, ok := g.disk[path]; ok {
		return lbHash(content)
	}
	return "-"
}

// lvsSum is the guest's digest of the UDP service's files, "-" without a config.
func (g *lbGuest) lvsSum() string {
	if _, ok := g.disk[lbGuestLVSFiles[0]]; !ok {
		return "-"
	}
	all := ""
	for _, path := range lbGuestLVSFiles {
		all += g.disk[path]
	}
	return lbHash(all)
}

// ipvs is /proc/net/ip_vs: the servers of the config the UDP service runs, without the members no check reaches.
func (g *lbGuest) ipvs() string {
	out := "IP Virtual Server version 1.2.1 (size=4096)\nProt LocalAddress:Port Scheduler Flags\n  -> RemoteAddress:Port Forward Weight ActiveConn InActConn\n"
	if !g.lvs {
		return out
	}
	key := func(addr, port string) string {
		var a, b, c, d, p int
		_, _ = fmt.Sscanf(addr+" "+port, "%d.%d.%d.%d %d", &a, &b, &c, &d, &p)
		return fmt.Sprintf("%02X%02X%02X%02X:%04X", a, b, c, d, p)
	}
	for _, line := range strings.Split(g.disk[lbGuestLVSFiles[0]], "\n") {
		switch w := strings.Fields(line); {
		case len(w) >= 3 && w[0] == "virtual_server":
			out += "UDP  " + key(w[1], w[2]) + " wrr \n"
		case len(w) >= 3 && w[0] == "real_server" && !slices.Contains(g.dead, w[1]+":"+w[2]):
			out += "  -> " + key(w[1], w[2]) + "      Masq    1      0          0\n"
		}
	}
	return out
}

// lbConfigMembers lists a config's members as "backend/server" -> "address:port".
func lbConfigMembers(config string) map[string]string {
	out, px := map[string]string{}, ""
	for _, line := range strings.Split(config, "\n") {
		switch w := strings.Fields(line); {
		case len(w) >= 2 && w[0] == "backend":
			px = w[1]
		case len(w) >= 3 && w[0] == "server":
			out[px+"/"+w[1]] = w[2]
		}
	}
	return out
}

// load is HAProxy reading config, at a start or a reload: every member is UP, but one the state file calls DOWN when
// the config reads that file.
func (g *lbGuest) load(config string) {
	g.file, g.loaded, g.haproxy = config, config, true
	g.members = map[string]string{}
	for key := range lbConfigMembers(config) {
		g.members[key] = "UP"
		if strings.Contains(config, "load-server-state-from-file global") && g.saved[key] == "DOWN" {
			g.members[key] = "DOWN"
		}
	}
}

// check runs HAProxy's health checks to their end: a member is what its checks say.
func (g *lbGuest) check() {
	for key, addr := range lbConfigMembers(g.loaded) {
		g.members[key] = "UP"
		if slices.Contains(g.dead, addr) {
			g.members[key] = "DOWN"
		}
	}
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
	gone   bool   // no VM is listed any more, and their VMIDs are free
	unseen bool   // no VM is listed, and their VMIDs are held: the token lost sight of them
	mu     sync.Mutex
	log    []string
	rules  map[string][]pve.FirewallRule
	sets   map[string][]string
	guests map[string]*lbGuest
	runs   map[int]lbRun
	files  []string // the HAProxy configs written, in order
	reads  int      // state reads
	pages  int      // reads of the cluster's VM list: one per apply
	// gate, while set, holds every read of the VM list until it is closed: an apply in the middle of its work.
	gate chan struct{}
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
		if strings.Contains(cmd, "nftables") { // the install line of a pair with a UDP listener
			g.noNft = false
		}
		return lbRun{}
	case strings.Contains(cmd, "dpkg-query"): // the ready check
		switch {
		case g.aptRunning:
			return lbRun{exit: 3}
		case g.packages != "installed", g.noNft && strings.Contains(cmd, "nftables"):
			return lbRun{exit: 1}
		}
		return lbRun{}
	case strings.Contains(cmd, "systemctl enable -q proxcloud-lvs"): // the UDP service's apply: check, swap, (re)load, prove
		f.log = append(f.log, vm+" lvs reload")
		if g.lvsFails != "" {
			delete(g.disk, lbGuestLVSFiles[0]+".new")
			return lbRun{exit: 1, err: g.lvsFails}
		}
		g.moveIn(lbGuestLVSFiles...)
		g.lvs = true
		return lbRun{}
	case strings.Contains(cmd, "systemctl disable -q --now proxcloud-lvs"): // the last UDP listener went
		f.log = append(f.log, vm+" lvs remove")
		for _, path := range lbGuestLVSFiles {
			delete(g.disk, path)
		}
		g.lvs = false
		return lbRun{}
	case strings.Contains(cmd, "cat /proc/net/ip_vs"):
		return lbRun{out: g.ipvs()}
	case strings.Contains(cmd, "command -v haproxy"): // a ready check that only asks for the binaries (v0.11.0-rc.1)
		if g.packages == "" {
			return lbRun{exit: 1}
		}
		return lbRun{}
	case strings.Contains(cmd, "keepalived -t"):
		f.log = append(f.log, vm+" keepalived reload")
		g.keepalived = true
		g.moveIn(lbGuestPairFiles...)
		// The election, as far as the order of an apply shows in it (the lab's Q0b-1, and the real guests of
		// test/integration/lb_lone_test.go): a keepalived that starts takes the addresses unless a master's adverts
		// reach it, and a master sends them to the peer its own config names. One that runs keeps its state over
		// a reload.
		if g.role == "" {
			self := ""
			if _, rest, ok := strings.Cut(g.disk[lbGuestKeepalived], "unicast_src_ip "); ok {
				self, _, _ = strings.Cut(rest, "\n")
			}
			g.role, g.holds = "MASTER", true
			for id, o := range f.guests {
				if id != vm && o.keepalived && o.role == "MASTER" && strings.Contains(o.disk[lbGuestKeepalived], "    "+self+"\n") {
					g.role, g.holds = "BACKUP", false
				}
			}
		}
		return lbRun{}
	case strings.Contains(cmd, "haproxy -c"):
		f.log = append(f.log, vm+" haproxy reload")
		// The apply saves the members' state first, when it says so and HAProxy answers; no answer, no file.
		if strings.Contains(cmd, "show servers state") {
			g.saved = nil
			if g.haproxy {
				g.saved = maps.Clone(g.members)
			}
		}
		if g.applyFails != "" {
			return lbRun{exit: 1, err: g.applyFails}
		}
		// HAProxy binds an address the guest does not hold only where keepalived's sysctl file let it (the lab's
		// Q0b-1: the first appliance of a pair had no keepalived, and refused its config).
		if m := lbGuestBind.FindStringSubmatch(g.pending); m != nil && !strings.Contains(g.disk["/etc/sysctl.d/90-proxcloud-lb.conf"], "ip_nonlocal_bind = 1") {
			return lbRun{exit: 1, err: "[ALERT] (8464) : Binding [/etc/haproxy/haproxy.cfg:17] for frontend l" + m[1] + ": protocol tcpv4: cannot bind socket (Cannot assign requested address) for [" + m[1] + "]"}
		}
		g.load(g.pending)
		return lbRun{}
	case strings.Contains(cmd, "proxcloud-lb.state"):
		f.reads++
		return f.stateRead(g, cmd)
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
				out += px + "," + w[1] + "," + g.members[px+"/"+w[1]] + ",1\n"
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
		return lbRun{out: "ok " + lbHash(g.file)}
	}
	return lbRun{}
}

// lbHoldsRe finds, in the state read's script, the address it asks the guest for.
var lbHoldsRe = regexp.MustCompile(`grep -qF ' ([0-9.]+)/'`)

// stateRead answers the state read. The fake says what the guest is: keepalived's state, HAProxy and its listeners,
// the address, the files. What the product reads of it is the script's own answer, run as the text it is on this
// machine (lbguesttest), so a mistake in the script shows here: until v0.11.1-rc.1 the fake wrote the answer itself,
// and the script lost "holds the address" on every pair with a UDP listener while every test read ACTIVE (drill
// Q1-2). On a machine that cannot run the script the fake still writes it.
func (f *pairWire) stateRead(g *lbGuest, cmd string) lbRun {
	role := g.role
	switch {
	case !g.keepalived:
		role = "STOPPED"
	case !g.haproxy:
		role = "FAULT" // the track script
	}
	holds := g.holds && g.keepalived && g.haproxy
	udp := g.lvs && g.disk[lbGuestLVSFiles[0]] != ""
	if !lbguesttest.Available() {
		n := map[bool]int{true: 1}
		// The files as they are on the guest, which is not always what the product wrote last.
		return lbRun{out: fmt.Sprintf("%s %d %d %d %s %s %s %d", role, n[g.haproxy], n[holds], n[g.serving()], lbHash(g.file),
			g.sum("/etc/keepalived/keepalived.conf"), g.lvsSum(), n[udp])}
	}
	if g.box == nil {
		g.box = lbguesttest.New(f.t)
	}
	b := g.box
	b.Write("/run/proxcloud-lb.state", role+"\n")
	active := ""
	for unit, runs := range map[string]bool{"keepalived": g.keepalived, "haproxy": g.haproxy, "proxcloud-lvs": udp} {
		if runs {
			active += unit + "\n"
		}
	}
	b.Stub("active", active)
	b.Unstub("socat")
	if g.haproxy {
		b.Stub("socat", "7 master 0 [failed: 0] 0d00h01m00s 3.2.4\n# workers\n9 worker 0 0d00h01m00s 3.2.4\n")
	}
	addrs := "2: ens18    inet 10.0.0.31/24 brd 10.0.0.255 scope global ens18\n"
	if m := lbHoldsRe.FindStringSubmatch(cmd); m != nil && holds {
		addrs += "3: ens19    inet " + m[1] + "/24 scope global ens19\n"
	}
	b.Stub("ip", addrs)
	// HAProxy listens on the binds of the config it read; the fake's "serves" is that this is the file's.
	b.Write("/etc/haproxy/haproxy.cfg", g.file)
	var listens []string
	for _, line := range strings.Split(g.file, "\n") {
		if w := strings.Fields(line); g.serving() && len(w) >= 2 && w[0] == "bind" && !strings.HasPrefix(w[1], "unix@") {
			if strings.HasPrefix(w[1], ":") {
				w[1] = "0.0.0.0" + w[1]
			}
			listens = append(listens, w[1])
		}
	}
	b.Listen(listens...)
	// The files as they are on the guest, which is not always what the product wrote last.
	for _, path := range append([]string{"/etc/keepalived/keepalived.conf"}, lbGuestLVSFiles...) {
		if content, ok := g.disk[path]; ok {
			b.Write(path, content)
		} else {
			b.Remove(path)
		}
	}
	b.Write("/proc/net/ip_vs", g.ipvs())
	for _, key := range []string{"vs/conntrack", "vs/snat_reroute", "ip_forward"} {
		b.Write("/proc/sys/net/ipv4/"+key, "1\n")
	}
	b.Stub("nft", "table ip proxcloud-lb {\n\tchain lb-forward {\n\t\ttype filter hook forward priority filter; policy drop;\n\t}\n}\n")
	exit, out, stderr := b.Run("sh", strings.TrimPrefix(cmd, "sh -c "))
	return lbRun{exit: exit, out: out, err: stderr}
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
		f.pages++
		if gate := f.gate; gate != nil {
			f.mu.Unlock()
			<-gate
			f.mu.Lock()
		}
		if f.gone || f.unseen {
			write([]map[string]any{})
			return
		}
		var out []map[string]any
		for i, vm := range []string{"301", "302"} {
			if g := f.guests[vm]; g != nil {
				status := "running"
				switch {
				case g.stopped:
					status = "stopped"
				case g.paused: // what Proxmox lists (drill N3-5), not "running"
					status = "paused"
				}
				vmid, _ := strconv.Atoi(vm)
				out = append(out, map[string]any{"id": "qemu/" + vm, "type": "qemu", "vmid": vmid, "node": fmt.Sprintf("n%d", i+1), "pool": "pc-p1",
					"status": status, "tags": "proxcloud;" + tag})
			}
		}
		write(out)
		return
	}
	if r.URL.Path == "/api2/json/cluster/nextid" { // PVE answers from every guest of the cluster, seen or not
		if id := r.Form.Get("vmid"); f.guests[id] != nil && !f.gone {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": map[string]string{"vmid": "VM " + id + " already exists"}})
		} else {
			write(id)
		}
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
	if strings.HasPrefix(rest, "agent/") && (g.stopped || g.paused || g.agentDown) {
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
		qmp := "running"
		if g.paused {
			qmp = "paused"
		}
		write(map[string]any{"status": "running", "qmpstatus": qmp, "tags": "proxcloud;" + tag})
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
		if g.disk == nil {
			g.disk = map[string]string{}
		}
		// The agent truncates the file and fills it: no file of the product's is written onto its place, where
		// keepalived, systemd or HAProxy may be reading it (drill Q1-o2 of v0.11.1).
		if !strings.HasSuffix(r.Form.Get("file"), ".new") {
			f.t.Errorf("the agent wrote %s onto its place: a file reaches its place by a rename of <path>.new", r.Form.Get("file"))
		}
		g.disk[r.Form.Get("file")] = r.Form.Get("content")
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

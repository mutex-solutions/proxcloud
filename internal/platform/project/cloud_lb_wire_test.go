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
	// The certificate directory (v0.11.1): its files are in disk, written by the agent as <name>.pem.new and moved in
	// place by the install command. certMode holds what stat says of the directory or of a file that is not as a
	// push leaves it (a file or a directory somebody opened up); nothing recorded means root's alone.
	certMode map[string]string
}

// lbGuestCertDir is the certificate directory on a guest.
const lbGuestCertDir = "/etc/haproxy/certs"

// certFiles lists the certificate files in place, sorted.
func (g *lbGuest) certFiles() []string {
	var out []string
	for path := range g.disk {
		if strings.HasPrefix(path, lbGuestCertDir+"/") && strings.HasSuffix(path, ".pem") {
			out = append(out, path)
		}
	}
	slices.Sort(out)
	return out
}

// certState is what the guest's certstate prints: the directory, then each file's mode, then each file's digest. A
// guest that never had a certificate has no directory and prints nothing.
func (g *lbGuest) certState() string {
	if _, ok := g.disk[lbGuestCertDir]; !ok {
		return ""
	}
	mode := func(path, or string) string {
		if m := g.certMode[path]; m != "" {
			return m
		}
		return or
	}
	out := "crtdir " + mode(lbGuestCertDir, "700:root:root:directory") + "\n"
	for _, path := range g.certFiles() {
		out += "crt " + path + " " + mode(path, "600:root:root") + "\n"
	}
	for _, path := range g.certFiles() {
		out += lbHash(g.disk[path]) + "  " + path + "\n"
	}
	return out
}

// lbGuestKeepalived is the VRRP keepalived's config on a guest, and lbGuestBind finds a config's first bind of an
// address of its own.
const lbGuestKeepalived = "/etc/keepalived/keepalived.conf"

var lbGuestBind = regexp.MustCompile(`(?m)^\s*bind ([1-9][0-9.]*:[0-9]+)`)

// The UDP service's files, in the order the guest sums them (pve's lvssum).
var lbGuestLVSFiles = []string{"/etc/keepalived/lvs.conf", "/etc/proxcloud-lb/lvs.nft", "/etc/proxcloud-lb/lvs.sysctl", "/usr/local/sbin/proxcloud-lvs-net",
	"/etc/systemd/system/proxcloud-lvs.service"}

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
	// execs counts every command run in a guest, and certReads the state reads that asked for the certificate
	// files: a balancer without a TLS listener is never asked, and one with a TLS listener is asked in the state
	// read it already had.
	execs, certReads int
	pages            int // reads of the cluster's VM list: one per apply
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
	f.execs++
	// certs is what a state read that asks for the certificate files gets after its own line.
	certs := func() string {
		if !strings.Contains(cmd, "certstate") {
			return ""
		}
		f.certReads++
		return "\n" + g.certState()
	}
	switch {
	case strings.Contains(cmd, "is not a directory of root alone"): // the certificate directory, made private and proven
		if g.disk == nil {
			g.disk = map[string]string{}
		}
		g.disk[lbGuestCertDir] = ""
		delete(g.certMode, lbGuestCertDir)
		for path := range g.disk {
			if strings.HasPrefix(path, lbGuestCertDir+"/") && strings.HasSuffix(path, ".new") {
				delete(g.disk, path)
			}
		}
		return lbRun{out: g.certState()} // what is there: the push writes the files that are not
	case strings.Contains(cmd, "a certificate file is not root's alone"): // owner, mode and the move in place, per name
		words := strings.Fields(cmd)
		for i := len(words) - 1; i >= 0 && len(words[i]) == 64; i-- {
			path := lbGuestCertDir + "/" + words[i] + ".pem"
			g.disk[path] = g.disk[path+".new"]
			delete(g.disk, path+".new")
			delete(g.certMode, path)
		}
		return lbRun{}
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
		for _, path := range []string{lbGuestLVSFiles[0], lbGuestLVSFiles[4]} {
			g.disk[path] = g.disk[path+".new"]
			delete(g.disk, path+".new")
		}
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
		g.disk[lbGuestKeepalived] = g.disk[lbGuestKeepalived+".new"]
		delete(g.disk, lbGuestKeepalived+".new")
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
		for _, path := range g.certFiles() { // served: the certificate files the config does not name go
			if !strings.Contains(g.file, " "+path) {
				delete(g.disk, path)
			}
		}
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
		l := 0
		if g.lvs && g.disk[lbGuestLVSFiles[0]] != "" {
			l = 1
		}
		// The files as they are on the guest, which is not always what the product wrote last.
		return lbRun{out: fmt.Sprintf("%s %d %d %d %s %s %s %d", role, h, v, c, lbHash(g.file), g.sum(lbGuestKeepalived), g.lvsSum(), l) + certs()}
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
		return lbRun{out: "ok " + lbHash(g.file) + certs()}
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

package platformlife

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"log/slog"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
)

// fakeJobs is the job store: every Checkpoint from the loseAt-th on (and every Renew after it) reports a lost lease
// without running its function, as a commit that fails its lease check rolls back.
type fakeJobs struct {
	mu          sync.Mutex
	loseAt      int
	checkpoints int
	lost        bool
	renewErr    error // what Renew answers while the lease is not lost
	onLose      func()
	ends        []string // complete | defer:<code> | fail:<code> | retry:<code>
	defers      int      // calls of Defer, refused ones too
}

func (f *fakeJobs) ClaimKinds(context.Context, time.Duration, []string) (*durablejobs.Job, error) {
	return nil, nil
}
func (f *fakeJobs) Renew(context.Context, *durablejobs.Job, time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lost {
		return durablejobs.ErrLeaseLost
	}
	return f.renewErr
}
func (f *fakeJobs) Checkpoint(ctx context.Context, _ *durablejobs.Job, fn func(context.Context) error) error {
	f.mu.Lock()
	f.checkpoints++
	if f.loseAt > 0 && f.checkpoints >= f.loseAt && !f.lost {
		f.lost = true
		if f.onLose != nil {
			f.onLose()
		}
	}
	lost := f.lost
	f.mu.Unlock()
	if lost {
		return durablejobs.ErrLeaseLost
	}
	return fn(ctx)
}
func (f *fakeJobs) end(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lost {
		return durablejobs.ErrLeaseLost
	}
	f.ends = append(f.ends, s)
	return nil
}
func (f *fakeJobs) Complete(context.Context, *durablejobs.Job, func(context.Context) error) error {
	return f.end("complete")
}
func (f *fakeJobs) Fail(_ context.Context, _ *durablejobs.Job, _ time.Duration, permanent bool, code string) error {
	if !permanent {
		return f.end("retry:" + code)
	}
	return f.end("fail:" + code)
}
func (f *fakeJobs) Defer(_ context.Context, _ *durablejobs.Job, _ time.Duration, code string) error {
	f.mu.Lock()
	f.defers++
	f.mu.Unlock()
	return f.end("defer:" + code)
}
func (f *fakeJobs) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ends) == 0 {
		return ""
	}
	return f.ends[len(f.ends)-1]
}

// fakeState keeps the run, the installation and the secrets as JSON, so no write shares memory with the runner.
type fakeState struct {
	mu            sync.Mutex
	run, in, sec  []byte
	notFoundAfter bool
}

func (f *fakeState) put(run *Run, in *Installation, sec *Secrets) {
	f.run, _ = json.Marshal(run)
	f.in, _ = json.Marshal(in)
	f.sec, _ = json.Marshal(sec)
}

func (f *fakeState) get() (*Run, *Installation, *Secrets) {
	var run Run
	var in Installation
	var sec Secrets
	_ = json.Unmarshal(f.run, &run)
	_ = json.Unmarshal(f.in, &in)
	_ = json.Unmarshal(f.sec, &sec)
	return &run, &in, &sec
}

func (f *fakeState) Load(context.Context, string) (*Run, *Installation, *Secrets, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, in, sec := f.get()
	return run, in, sec, nil
}

func (f *fakeState) Mutate(_ context.Context, _ string, fn func(*Run, *Installation, *Secrets) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, in, sec := f.get()
	if err := fn(run, in, sec); err != nil {
		return err
	}
	f.put(run, in, sec)
	return nil
}

func (f *fakeState) snapshot() (*Run, *Installation, *Secrets) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.get()
}

// fakeImports is Template updates: receipts by node and digest.
type fakeImports struct {
	recs       []templateupdates.Receipt
	dead       map[string]bool
	enqueued   []templateupdates.VMRequest
	enqueueErr error
	markErr    error
}

func (f *fakeImports) ByKey(_ context.Context, svc, node, digest string) ([]templateupdates.Receipt, error) {
	var out []templateupdates.Receipt
	for _, r := range f.recs {
		if r.Payload.ServiceID == svc && r.Payload.Node == node && r.Payload.Digest == digest {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeImports) JobState(_ context.Context, id string) (string, error) {
	if f.dead[id] {
		return "dead", nil
	}
	return "pending", nil
}
func (f *fakeImports) EnqueueVM(_ context.Context, req templateupdates.VMRequest, actor string) (*templateupdates.Receipt, error) {
	if f.enqueueErr != nil {
		return nil, f.enqueueErr
	}
	f.enqueued = append(f.enqueued, req)
	rec := templateupdates.Receipt{ID: fmt.Sprintf("tu-%d", len(f.recs)+1), RequestedBy: actor, Step: templateupdates.StepQueued, CreatedAt: time.Now(),
		Payload: templateupdates.VMPayload{ServiceID: req.ServiceID, Node: req.Node, Digest: req.Digest, Package: req.Package}}
	f.recs = append(f.recs, rec)
	return &rec, nil
}
func (f *fakeImports) MarkTemplateGone(_ context.Context, _ templateupdates.PVEResolver, id string) error {
	if f.markErr != nil {
		return f.markErr
	}
	for i := range f.recs {
		if f.recs[i].ID == id {
			f.recs[i].TemplateGone = true
		}
	}
	return nil
}

// fakeVM is one VM of fakePVE.
type fakeVM struct {
	cfg     map[string]any
	pending map[string]any // config values Proxmox took but has not applied (fakePVE.pendCD)
	running bool
	pooled  bool
	system  bool // a member of proxcloud-system
}

// fakePVE is one node (pve1) with the seed pool, an ISO store "local" and the template 9100. It records every
// request that is not a GET, with the token it came with.
type fakePVE struct {
	mu        sync.Mutex
	t         *testing.T
	next      int
	vms       map[int]*fakeVM
	iso       []string // file names on local
	tasks     map[string]string
	rules     []pve.FirewallRule
	writes    []string
	failOnce  map[string]string // "METHOD path" → an error body answered once with 500
	keepISO   int               // deletes of an ISO that answer OK but leave the file
	agent     bool
	noMAC     bool           // a NIC set without a MAC address stays without one
	pendCD    bool           // a config write to a running VM ends OK but stays pending
	macs      int            // MAC addresses given so far
	cloneBusy bool           // a qmclone task is listed as running
	poolLag   int            // pool reads that answer without members: a clone that joins its pool after them
	onWrite   func(w string) // called with every recorded request
	cloneTags string
	uploads   [][]byte
	disks     []string               // volumes on local-lvm (the names after "local-lvm:")
	writers   []string               // the token id of each recorded request, parallel to writes
	templates []pve.ClusterResource  // /cluster/resources?type=vm
	noVMList  bool                   // /cluster/resources?type=vm answers 500
	tmplCfg   map[int]map[string]any // the config of node templates, on any node (none: Proxmox knows no such VM)
	tmplDeny  map[int]bool           // node templates whose config the token may not read
	// SDN: zones, vnets and each vnet's subnets. A created object is "new" until PUT /cluster/sdn applies it; as
	// Proxmox is understood to do, a list names that state only when asked with pending=1, and the node lists a vnet
	// among its bridges only once it is applied.
	zones   []pve.SDNZone
	vnets   []pve.SDNVnet
	subnets map[string][]pve.SDNSubnet
	probe   *probeEnv // the tenant path test's routes (tenantprobe_test.go)
	keepVM  bool      // a VM delete is refused
}

// sdn answers the /cluster/sdn routes and the node's network lists; false when path is not one.
func (f *fakePVE) sdn(r *http.Request, path string, data func(any)) bool {
	p := strings.Split(strings.TrimPrefix(path, "/cluster/sdn"), "/")[1:]
	pending := r.Form.Get("pending") == "1"
	switch {
	case path == "/cluster/status":
		data([]map[string]any{{"type": "node", "name": "pve1", "ip": "192.0.2.11"}})
		return true
	case path == "/nodes/pve1/network":
		ifaces := []map[string]any{{"iface": "vmbr0", "type": "bridge", "address": "192.0.2.11", "gateway": "192.0.2.1", "cidr": "192.0.2.11/24"}, {"iface": "vmbr1", "type": "bridge"}}
		if r.Form.Get("type") == "any_bridge" {
			for _, v := range f.vnets {
				if v.State == "" {
					ifaces = append(ifaces, map[string]any{"iface": v.Vnet, "type": "vnet"})
				}
			}
		} else {
			ifaces = append(ifaces, map[string]any{"iface": "eno1", "type": "eth"})
		}
		data(ifaces)
		return true
	case !strings.HasPrefix(path, "/cluster/sdn"):
		return false
	case len(p) == 0 && r.Method == http.MethodPut:
		for i := range f.zones {
			f.zones[i].State = ""
		}
		for i := range f.vnets {
			f.vnets[i].State = ""
		}
		data(f.upid("srvreload", 0))
	case len(p) == 1 && p[0] == "zones" && r.Method == http.MethodGet:
		zones := append([]pve.SDNZone{}, f.zones...)
		for i := range zones {
			if !pending {
				zones[i].State = ""
			}
		}
		data(zones)
	case len(p) == 1 && p[0] == "zones":
		f.zones = append(f.zones, pve.SDNZone{Zone: r.Form.Get("zone"), Type: r.Form.Get("type"), Nodes: r.Form.Get("nodes"), State: "new"})
		data(nil)
	case len(p) == 2 && p[0] == "zones":
		f.zones = slices.DeleteFunc(f.zones, func(z pve.SDNZone) bool { return z.Zone == p[1] })
		data(nil)
	case len(p) == 1 && p[0] == "vnets" && r.Method == http.MethodGet:
		vnets := append([]pve.SDNVnet{}, f.vnets...)
		for i := range vnets {
			if !pending {
				vnets[i].State = ""
			}
		}
		data(vnets)
	case len(p) == 1 && p[0] == "vnets":
		f.vnets = append(f.vnets, pve.SDNVnet{Vnet: r.Form.Get("vnet"), Zone: r.Form.Get("zone"), Alias: r.Form.Get("alias"), State: "new"})
		data(nil)
	case len(p) == 2 && p[0] == "vnets":
		f.vnets = slices.DeleteFunc(f.vnets, func(v pve.SDNVnet) bool { return v.Vnet == p[1] })
		data(nil)
	case len(p) == 3 && p[2] == "subnets" && r.Method == http.MethodGet:
		data(append([]pve.SDNSubnet{}, f.subnets[p[1]]...))
	case len(p) == 3 && p[2] == "subnets":
		snat, _ := strconv.Atoi(r.Form.Get("snat"))
		if f.subnets == nil {
			f.subnets = map[string][]pve.SDNSubnet{}
		}
		f.subnets[p[1]] = append(f.subnets[p[1]], pve.SDNSubnet{Subnet: "z-" + strings.ReplaceAll(r.Form.Get("subnet"), "/", "-"),
			CIDR: r.Form.Get("subnet"), Gateway: r.Form.Get("gateway"), SNAT: snat})
		data(nil)
	case len(p) == 4 && p[2] == "subnets":
		f.subnets[p[1]] = slices.DeleteFunc(f.subnets[p[1]], func(s pve.SDNSubnet) bool { return s.Subnet == p[3] })
		data(nil)
	default:
		return false
	}
	return true
}

func newFakePVE(t *testing.T) *fakePVE {
	return &fakePVE{t: t, next: 120, vms: map[int]*fakeVM{}, tasks: map[string]string{}, failOnce: map[string]string{}, cloneTags: "pc-template;ubuntu-2604"}
}

func (f *fakePVE) upid(typ string, id int) string {
	u := fmt.Sprintf("UPID:pve1:%08X:00000001:%08X:%s:%d:capmox@pve!capi:", len(f.tasks)+1, len(f.tasks)+1, typ, id)
	f.tasks[u] = "OK"
	return u
}

func (f *fakePVE) writeCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, w := range f.writes {
		if strings.HasPrefix(w, prefix) {
			n++
		}
	}
	return n
}

func (f *fakePVE) pooled() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int
	for id, vm := range f.vms {
		if vm.pooled {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func (f *fakePVE) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+path)
		id, _, _ := strings.Cut(strings.TrimPrefix(r.Header.Get("Authorization"), "PVEAPIToken="), "=")
		f.writers = append(f.writers, id)
		if f.onWrite != nil {
			f.onWrite(r.Method + " " + path)
		}
	}
	if body, ok := f.failOnce[r.Method+" "+path]; ok {
		delete(f.failOnce, r.Method+" "+path)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, body)
		return
	}
	_ = r.ParseForm()
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		_ = r.ParseMultipartForm(1 << 20)
	}
	data := func(v any) {
		b, _ := json.Marshal(map[string]any{"data": v})
		_, _ = w.Write(b)
	}
	vmid := func() (int, *fakeVM) {
		parts := strings.Split(path, "/")
		id, _ := strconv.Atoi(parts[4])
		return id, f.vms[id]
	}
	if p := strings.Split(path, "/"); len(p) == 6 && p[3] == "qemu" && p[5] == "config" && r.Method == http.MethodGet {
		if id, _ := strconv.Atoi(p[4]); f.vms[id] == nil { // a template: stage 7 reads it on the node it is on
			if f.tmplDeny[id] {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"data":null}`)
				return
			}
			if cfg := f.tmplCfg[id]; cfg != nil {
				data(cfg)
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"errors":{"vmid":"Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist"}}`, p[2], id)
			return
		}
	}
	if f.probeRoutes(w, r, path, data) || f.sdn(r, path, data) {
		return
	}
	switch {
	case path == "/pools/proxcloud-seed":
		var members []map[string]any
		f.poolLag--
		for id, vm := range f.vms {
			if vm.pooled && f.poolLag < 0 {
				members = append(members, map[string]any{"id": fmt.Sprintf("qemu/%d", id), "vmid": id, "node": "pve1", "type": "qemu"})
			}
		}
		data(map[string]any{"poolid": "proxcloud-seed", "members": members})
	case path == "/pools/"+pve.SystemPool:
		var members []map[string]any
		for id, vm := range f.vms {
			if vm.system {
				members = append(members, map[string]any{"id": fmt.Sprintf("qemu/%d", id), "vmid": id, "node": "pve1", "type": "qemu"})
			}
		}
		data(map[string]any{"poolid": pve.SystemPool, "members": members})
	case path == "/nodes/pve1/storage/local-lvm/content":
		var out []map[string]any
		for _, d := range f.disks {
			out = append(out, map[string]any{"volid": "local-lvm:" + d, "content": "images"})
		}
		data(out)
	case strings.HasPrefix(path, "/nodes/pve1/storage/local-lvm/content/"):
		name := strings.TrimPrefix(path, "/nodes/pve1/storage/local-lvm/content/")
		f.disks = slices.DeleteFunc(f.disks, func(n string) bool { return n == name })
		data(f.upid("imgdel", 0))
	case path == "/cluster/nextid":
		if v := r.Form.Get("vmid"); v != "" {
			id, _ := strconv.Atoi(v)
			if _, held := f.vms[id]; held {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"errors":{"vmid":"VM %d already exists"}}`, id)
				return
			}
			data(v)
			return
		}
		for f.vms[f.next] != nil {
			f.next++
		}
		data(strconv.Itoa(f.next))
		f.next++
	case path == "/storage":
		data([]map[string]any{{"storage": "local", "type": "dir", "content": "iso,import"}, {"storage": "local-lvm", "type": "lvmthin", "content": "images"},
			{"storage": "nas-templates", "type": "nfs", "content": "import", "shared": 1}})
	case path == "/cluster/resources" && r.Form.Get("type") == "storage":
		var rows []map[string]any
		for _, n := range []string{"pve1", "pve2", "pve3"} {
			for _, st := range []string{"local", "local-lvm", "nas-templates"} {
				rows = append(rows, map[string]any{"type": "storage", "storage": st, "node": n, "status": "available", "id": "storage/" + n + "/" + st})
			}
		}
		data(rows)
	case path == "/cluster/resources" && f.noVMList:
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"errors":{"type":"cluster not ready"}}`)
	case path == "/cluster/resources":
		data(f.templates)
	case path == "/nodes":
		data([]map[string]any{{"node": "pve1", "status": "online"}, {"node": "pve2", "status": "online"}, {"node": "pve3", "status": "online"}})
	case path == "/nodes/pve1/tasks":
		if f.cloneBusy {
			data([]map[string]any{{"upid": "UPID:pve1:1:1:1:qmclone:9100:x:", "type": "qmclone"}})
			return
		}
		data([]any{})
	case strings.HasPrefix(path, "/nodes/pve1/tasks/") && strings.HasSuffix(path, "/status"):
		up := strings.TrimSuffix(strings.TrimPrefix(path, "/nodes/pve1/tasks/"), "/status")
		data(map[string]any{"status": "stopped", "exitstatus": f.tasks[up]})
	case strings.HasPrefix(path, "/nodes/pve1/tasks/") && strings.HasSuffix(path, "/log"):
		data([]map[string]any{{"t": "starting"}, {"t": "clone failed: no space left"}})
	case path == "/nodes/pve1/qemu/9100/clone":
		id, _ := strconv.Atoi(r.Form.Get("newid"))
		if _, held := f.vms[id]; held {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"errors":{"newid":"VM %d already exists"}}`, id)
			return
		}
		// What a clone of a template made by Template updates has (vmjob.go): no NIC, a cloud-init drive on the boot store.
		f.vms[id] = &fakeVM{pooled: r.Form.Get("pool") == pve.SeedPool, cfg: map[string]any{
			"name": r.Form.Get("name"), "tags": f.cloneTags, "agent": "1", "boot": "order=scsi0", "scsihw": "virtio-scsi-single",
			"scsi0": fmt.Sprintf("local-lvm:vm-%d-disk-0,discard=on,size=6G", id), "ide2": fmt.Sprintf("local-lvm:vm-%d-cloudinit,media=cdrom", id)}}
		data(f.upid("qmclone", 9100))
	case strings.HasPrefix(path, "/nodes/pve1/qemu/"):
		id, vm := vmid()
		if vm == nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"errors":{"vmid":"Configuration file 'nodes/pve1/qemu-server/%d.conf' does not exist"}}`, id)
			return
		}
		rest := strings.Join(strings.Split(path, "/")[5:], "/")
		switch r.Method + " " + rest {
		case "GET config":
			cfg := maps.Clone(vm.cfg)
			if r.Form.Get("current") != "1" { // as Proxmox: the default answer has the pending values
				maps.Copy(cfg, vm.pending)
			}
			data(cfg)
		case "POST config":
			// As Proxmox: deletes are checked against the stored config (a protected VM keeps its drives), then the
			// values are set, and a NIC sent without a MAC address gets one.
			for _, d := range strings.Split(r.PostForm.Get("delete"), ",") {
				if _, has := vm.cfg[d]; has && vm.cfg["protection"] == "1" && d != "protection" {
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprintf(w, `{"message":"can't remove drive '%s' - protection mode enabled"}`, d)
					return
				}
				delete(vm.cfg, d)
			}
			for k := range r.PostForm {
				v := r.PostForm.Get(k)
				if model, rest, _ := strings.Cut(v, ","); k == "net0" && !strings.Contains(model, "=") && !f.noMAC {
					f.macs++ // a new address each time, as Proxmox
					v = fmt.Sprintf("%s=BC:24:11:00:00:%02X,%s", model, f.macs, rest)
				}
				if k == "delete" {
					continue
				}
				if f.pendCD && vm.running {
					if vm.pending == nil {
						vm.pending = map[string]any{}
					}
					vm.pending[k] = v
					continue
				}
				vm.cfg[k] = v
			}
			data(f.upid("qmconfig", id))
		case "PUT resize":
			vm.cfg["scsi0"] = fmt.Sprintf("local-lvm:vm-%d-disk-0,discard=on,size=%s", id, r.PostForm.Get("size"))
			data(f.upid("qmresize", id))
		case "GET status/current":
			st := "stopped"
			if vm.running {
				st = "running"
			}
			data(map[string]any{"status": st})
		case "POST status/start":
			vm.running = true
			data(f.upid("qmstart", id))
		case "POST status/stop":
			vm.running = false
			data(f.upid("qmstop", id))
		case "DELETE ":
			if vm.cfg["protection"] == "1" {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"errors":{"protection":"VM is protected"}}`)
				return
			}
			delete(f.vms, id)
			data(f.upid("qmdestroy", id))
		case "PUT firewall/options":
			data(nil)
		case "GET firewall/rules":
			for i := range f.rules {
				f.rules[i].Pos, f.rules[i].Digest = i, "d"
			}
			data(f.rules)
		case "POST firewall/rules":
			f.rules = append(f.rules, pve.FirewallRule{Type: r.PostForm.Get("type"), Action: r.PostForm.Get("action"), Proto: r.PostForm.Get("proto"),
				Source: r.PostForm.Get("source"), DPort: r.PostForm.Get("dport"), Comment: r.PostForm.Get("comment")})
			data(nil)
		case "POST agent/ping":
			if !f.agent {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"errors":{"agent":"QEMU guest agent is not running"}}`)
				return
			}
			data(nil)
		default:
			if r.Method == http.MethodDelete && strings.HasPrefix(rest, "firewall/rules/") {
				pos, _ := strconv.Atoi(strings.TrimPrefix(rest, "firewall/rules/"))
				f.rules = slices.Delete(f.rules, pos, pos+1)
				data(nil)
				return
			}
			f.t.Errorf("fake PVE: no route %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	case path == "/nodes/pve1/storage/local/upload":
		_, hdr, err := r.FormFile("filename")
		if err != nil {
			f.t.Errorf("upload without a file: %v", err)
			return
		}
		file, _ := hdr.Open()
		var b bytes.Buffer
		_, _ = b.ReadFrom(file)
		f.uploads = append(f.uploads, b.Bytes())
		f.iso = append(f.iso, hdr.Filename)
		u := fmt.Sprintf("UPID:pve1:%08X:00000001:%08X:imgcopy::capmox@pve!capi:", len(f.tasks)+1, len(f.tasks)+1)
		f.tasks[u] = "OK"
		data(u)
	case path == "/nodes/pve1/storage/local/content":
		var out []map[string]any
		for _, n := range f.iso {
			out = append(out, map[string]any{"volid": "local:iso/" + n, "content": "iso"})
		}
		data(out)
	case path == "/nodes/pve1/storage/nas-iso/content": // a second ISO store, empty
		data([]any{})
	case strings.HasPrefix(path, "/nodes/pve1/storage/local/content/iso/"):
		name := strings.TrimPrefix(path, "/nodes/pve1/storage/local/content/iso/")
		if f.keepISO > 0 {
			f.keepISO--
		} else {
			f.iso = slices.DeleteFunc(f.iso, func(n string) bool { return n == name })
		}
		data(f.upid("imgdel", 0))
	default:
		f.t.Errorf("fake PVE: no route %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// fakeSeed is the seed's API over TLS with a server certificate its CA signed (cert reads the CA when a client
// connects): readyz, the token Secret (bootstrap token only), the access review and the deletes (service account token
// only). calls records "METHOD path token-kind".
type fakeSeed struct {
	mu        sync.Mutex
	url       string
	ca        func() (certPEM, keyPEM string)
	boot      func() string
	sa        string
	ready     bool
	saAllowed bool
	forbid    int      // reads of the token Secret answered 403: the Role and RoleBinding are not applied yet
	deny      []denial // answered to the service account's next requests, one each, instead of serving them
	calls     []string
	// A small Kubernetes for the service account: objects by URL path, lists by collection path and label selector.
	objs     map[string]map[string]any
	extra    map[string]bool // more tokens that are the service account's (rotated ones)
	unsynced map[string]bool // Applications ArgoCD does not sync
	// resources is what ArgoCD's comparison lists per Application (status.resources). pruneOnly are Applications that
	// stay OutOfSync after a sync that succeeded (they never prune): "current" = the last sync was of the source the
	// Application has, anything else = it was of an earlier one. applied is every Application apply, <name>@<revision>.
	resources map[string][]any
	pruneOnly map[string]string
	applied   []string
	syncs     []string       // Applications a sync operation was started on
	lastOp    map[string]any // the operation of the last one
	onSync    func(name string)
	writes    []string // every request of the service account that is not a GET or an access review
	tokens    []string // TokenRequest tokens, with ttls the seconds each was asked for
	ttls      []float64
	grant     time.Duration // when set, the life every TokenRequest token gets instead of the one asked for
	uids      int
}

// denial is an error answer of the API server.
type denial struct {
	status  int
	message string
}

func newFakeSeed(t *testing.T, ca func() (string, string), boot func() string) *fakeSeed {
	f := &fakeSeed{ca: ca, boot: boot, sa: runSA, ready: true, saAllowed: true, objs: map[string]map[string]any{}, extra: map[string]bool{}, unsynced: map[string]bool{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(f.serve), ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = srv.Serve(tls.NewListener(ln, &tls.Config{GetCertificate: f.cert})) }()
	t.Cleanup(func() { _ = srv.Close() })
	f.url = "https://" + ln.Addr().String()
	return f
}

// staticCA is a seed CA made the way stage 3 makes one.
func staticCA(t *testing.T) func() (string, string) {
	s, err := NewSeedSecrets("pi-0a1b2c3d", 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return func() (string, string) { return s.CACert, s.CAKey }
}

func (f *fakeSeed) caPEM() string { c, _ := f.ca(); return c }

func (f *fakeSeed) cert(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	certPEM, keyPEM := f.ca()
	cb, _ := pem.Decode([]byte(certPEM))
	kb, _ := pem.Decode([]byte(keyPEM))
	if cb == nil || kb == nil {
		return nil, fmt.Errorf("fake seed: no CA yet")
	}
	ca, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	caKey, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func (f *fakeSeed) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	kind := ""
	switch {
	case tok == f.sa || f.extra[tok]:
		kind = "sa"
	case tok != "" && tok == f.boot():
		kind = "boot"
	}
	f.calls = append(f.calls, r.Method+" "+r.URL.Path+" "+kind)
	switch {
	case kind == "":
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"Unauthorized"}`)
	case kind == "sa" && len(f.deny) > 0:
		d := f.deny[0]
		f.deny = f.deny[1:]
		w.WriteHeader(d.status)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": d.message})
	case r.URL.Path == "/readyz":
		if !f.ready {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "ok")
	case r.URL.Path == "/api/v1/namespaces/proxcloud-system/secrets/proxcloud-portal-token-g1" && kind == "boot" && f.forbid > 0:
		f.forbid--
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"forbidden"}`)
	case r.URL.Path == "/api/v1/namespaces/proxcloud-system/secrets/proxcloud-portal-token-g1" && kind == "boot":
		fmt.Fprintf(w, `{"data":{"token":%q}}`, base64.StdEncoding.EncodeToString([]byte(f.sa)))
	case r.URL.Path == "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews" && kind == "sa":
		fmt.Fprintf(w, `{"status":{"allowed":%t}}`, f.saAllowed)
	case r.Method == http.MethodDelete && kind == "sa":
		f.writes = append(f.writes, "DELETE "+r.URL.Path)
		delete(f.objs, r.URL.Path)
		fmt.Fprint(w, `{}`)
	case kind == "sa":
		f.object(w, r)
	default:
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"forbidden"}`)
	}
}

func (f *fakeSeed) callsOf(kind string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasSuffix(c, " "+kind) {
			out = append(out, strings.TrimSuffix(c, " "+kind))
		}
	}
	return out
}

const (
	runCAPI = "5e1f4c7a-0d2b-4c3e-9a8b-c0ffee00c4p1"
	runCSI  = "5e1f4c7a-0d2b-4c3e-9a8b-c0ffee00c5i1"
	runBoot = "abcdef.0123456789abcdef"
	runSA   = "eyJhbGciOiJSUzI1NiJ9.planted-service-account-token"
	runK3s  = "PLANTEDK3STOKEN0123456789"
)

// env is one installation with a run, on fakes.
type env struct {
	t       *testing.T
	pve     *fakePVE
	jobs    *fakeJobs
	state   *fakeState
	imports *fakeImports
	r       *Runner
	logs    *bytes.Buffer
	job     *durablejobs.Job
}

func newEnv(t *testing.T, kind string) *env {
	t.Helper()
	e := &env{t: t, pve: newFakePVE(t), jobs: &fakeJobs{}, state: &fakeState{}, imports: &fakeImports{dead: map[string]bool{}}, logs: &bytes.Buffer{}}
	ts := httptest.NewServer(http.HandlerFunc(e.pve.serve))
	t.Cleanup(ts.Close)
	es := &externalservice.ExternalService{ID: "svc-pve", Type: externalservice.TypeCloud,
		Config: map[string]any{"provider": "proxmox", "apiUrl": ts.URL, "tokenId": "proxcloud@pve!platform"},
		Secret: map[string]any{"tokenSecret": "portal-secret-0000"}}
	e.r = &Runner{Jobs: e.jobs, State: e.state, Imports: e.imports, Log: slog.New(slog.NewTextHandler(e.logs, nil)), Lease: time.Minute,
		Provider: func(context.Context, string) (*externalservice.ExternalService, error) { return es, nil },
		Preflight: func(context.Context, *Installation) ([]Check, error) {
			return []Check{{ID: "flags", Status: CheckOK}}, nil
		}}
	cur, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	in := validInstallation(t)
	in.ID, in.Status.Phase = "pi-0a1b2c3d", PhaseApplying
	run := &Run{ID: "pr-1", InstallationID: in.ID, Kind: kind, State: RunQueued, BOM: BOMRef{Release: cur.Release, SHA256: cur.SHA256}}
	for _, id := range stageIDs(kind) {
		run.Stages = append(run.Stages, Stage{ID: id, State: StagePending})
	}
	sec := &Secrets{ID: in.ID, CAPI: &TokenCredential{TokenID: pve.CAPITokenID, Secret: runCAPI}, CSI: &TokenCredential{TokenID: pve.CSITokenID, Secret: runCSI}}
	e.state.put(run, in, sec)
	raw, _ := json.Marshal(Payload{Version: 1, RunID: run.ID, InstallationID: in.ID})
	e.job = &durablejobs.Job{ID: "job-1", Kind: durablejobs.PlatformRun, Payload: raw, Attempts: 1}
	return e
}

// upTo marks the stages before id done (stage 2 with the seed template 9100 on pve1).
func (e *env) upTo(id string) {
	e.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		for i := range run.Stages {
			if run.Stages[i].ID == id {
				return
			}
			run.Stages[i].State = StageDone
			if run.Stages[i].ID == StageImages {
				run.Stages[i].Receipt = &StageReceipt{Templates: map[string]int{"seed:pve1": 9100, "node:pve1": 9101, "node:pve2": 9102, "node:pve3": 9103}}
			}
		}
	})
}

func (e *env) mutate(fn func(*Run, *Installation, *Secrets)) {
	_ = e.state.Mutate(context.Background(), "", func(r *Run, i *Installation, s *Secrets) error { fn(r, i, s); return nil })
}

// once runs one acquisition and answers how it gave the job back.
func (e *env) once() string {
	e.t.Helper()
	e.r.defaults()
	e.r.perform(context.Background(), e.job)
	return e.jobs.last()
}

// until runs acquisitions until the job is given back other than by a Defer (at most n).
func (e *env) until(n int) string {
	e.t.Helper()
	for range n {
		if end := e.once(); !strings.HasPrefix(end, "defer:") {
			return end
		}
	}
	return e.jobs.last()
}

// item is the stage's item of that id (nil when it has none).
func item(s Stage, id string) *StageItem {
	if i := slices.IndexFunc(s.Items, func(it StageItem) bool { return it.ID == id }); i >= 0 {
		return &s.Items[i]
	}
	return nil
}

func (e *env) stage(id string) Stage {
	run, _, _ := e.state.snapshot()
	for _, s := range run.Stages {
		if s.ID == id {
			return s
		}
	}
	e.t.Fatalf("no stage %s", id)
	return Stage{}
}

// readySeedAt puts a seed generation that stage 3 built at vmid on fake PVE and in the state, with the seed API at seed.
func (e *env) seedAt(vmid, gen int, seed *fakeSeed, running bool) {
	e.pve.vms[vmid] = &fakeVM{pooled: true, running: running, cfg: map[string]any{"name": SeedVMName("pi-0a1b2c3d", gen),
		"tags": "proxcloud;pc-platform;pc-platform-pi-0a1b2c3d", "protection": "1", "net0": "virtio=BC:24:11:00:00:01,bridge=vmbr1,firewall=1",
		"ide2": "local:iso/" + SeedCDName("pi-0a1b2c3d", gen) + ",media=cdrom"}}
	e.pve.iso = append(e.pve.iso, SeedCDName("pi-0a1b2c3d", gen))
	server := ""
	ca := ""
	if seed != nil {
		server, ca = seed.url, seed.caPEM()
	}
	e.mutate(func(_ *Run, in *Installation, sec *Secrets) {
		in.Status.Seed = &SeedStatus{VMID: vmid, Node: "pve1", Generation: gen, APIServer: server, CACert: ca,
			CDVolume: "local:iso/" + SeedCDName("pi-0a1b2c3d", gen)}
		sec.BootstrapToken, sec.K3sToken = runBoot, runK3s
	})
}

// put stores obj at its URL path (tests seed what a controller would have made).
func (f *fakeSeed) put(path string, obj map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objs[path] = obj
}

func (f *fakeSeed) obj(path string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objs[path]
}

func (f *fakeSeed) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

// object serves the generic part (called with the lock held): apply and merge patches, gets, lists, token requests.
func (f *fakeSeed) object(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	answer := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+path)
	}
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/token"):
		token := fmt.Sprintf("requested-token-%d-0123456789", len(f.tokens)+1)
		secs, _ := dig(body, "spec", "expirationSeconds").(float64)
		f.tokens, f.ttls = append(f.tokens, token), append(f.ttls, secs)
		life := time.Duration(secs) * time.Second
		if f.grant > 0 {
			life = f.grant
		}
		answer(map[string]any{"status": map[string]any{"token": token, "expirationTimestamp": time.Now().Add(life).UTC().Format(time.RFC3339)}})
	case r.Method == http.MethodPatch && r.Header.Get("Content-Type") == "application/merge-patch+json":
		cur := f.objs[path]
		if cur == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if body["operation"] != nil { // ArgoCD takes the operation off the Application as it starts it
			name := path[strings.LastIndex(path, "/")+1:]
			f.syncs = append(f.syncs, name)
			f.lastOp, _ = body["operation"].(map[string]any)
			if f.onSync != nil {
				f.onSync(name)
			}
		}
		answer(cur)
	case r.Method == http.MethodPatch:
		cur := f.objs[path]
		if cur == nil {
			f.uids++
			cur = map[string]any{}
			body["metadata"].(map[string]any)["uid"] = fmt.Sprintf("uid-%d", f.uids)
		} else {
			body["metadata"].(map[string]any)["uid"] = dig(cur, "metadata", "uid")
		}
		for _, keep := range []string{"status", "data"} {
			if body[keep] == nil && cur[keep] != nil {
				body[keep] = cur[keep]
			}
		}
		name := str(body, "metadata", "name")
		if sd, ok := body["stringData"].(map[string]any); ok {
			data := map[string]any{}
			for k, v := range sd {
				data[k] = base64.StdEncoding.EncodeToString([]byte(v.(string)))
			}
			body["data"] = data
			delete(body, "stringData")
		}
		if body["type"] == "kubernetes.io/service-account-token" && body["data"] == nil { // the token controller
			token := "rotated-token-of-" + name
			f.extra[token] = true
			body["data"] = map[string]any{"token": base64.StdEncoding.EncodeToString([]byte(token))}
		}
		if body["kind"] == "Application" {
			f.applied = append(f.applied, name+"@"+str(body, "spec", "source", "targetRevision"))
		}
		if body["kind"] == "Application" && !f.unsynced[name] {
			source := dig(body, "spec", "source")
			status := map[string]any{"health": map[string]any{"status": "Healthy"}, "operationState": map[string]any{"phase": "Succeeded"},
				"sync": map[string]any{"status": "Synced", "comparedTo": map[string]any{"source": source}}}
			if res, ok := f.resources[name]; ok {
				status["resources"] = res
			}
			if last, ok := f.pruneOnly[name]; ok && dig(body, "spec", "syncPolicy", "automated") != nil {
				status["sync"].(map[string]any)["status"] = "OutOfSync"
				if last != "current" {
					source = map[string]any{"targetRevision": last}
				}
				status["operationState"] = map[string]any{"phase": "Succeeded", "syncResult": map[string]any{"source": source}}
			}
			body["status"] = status
		}
		f.objs[path] = body
		answer(body)
	case r.Method != http.MethodGet:
		w.WriteHeader(http.StatusMethodNotAllowed)
	case f.objs[path] != nil:
		answer(f.objs[path])
	case isCollection(path):
		var keys []string
		for k := range f.objs {
			if strings.HasPrefix(k, path+"/") && !strings.Contains(k[len(path)+1:], "/") && matches(f.objs[k], r.URL.Query().Get("labelSelector")) {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		items := []any{}
		for _, k := range keys {
			items = append(items, f.objs[k])
		}
		answer(map[string]any{"items": items})
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"not found"}`)
	}
}

// isCollection tells /api/v1/nodes and /apis/g/v/namespaces/ns/plural from the path of one object.
func isCollection(path string) bool {
	p := strings.Split(strings.Trim(path, "/"), "/")
	rest := p[min(len(p), 3):]
	if p[0] == "api" {
		rest = p[min(len(p), 2):]
	}
	return len(rest) == 1 || (len(rest) == 3 && rest[0] == "namespaces")
}

// matches is a label selector of "k=v" and "k" terms.
func matches(obj map[string]any, selector string) bool {
	for _, term := range strings.FieldsFunc(selector, func(r rune) bool { return r == ',' }) {
		k, v, eq := strings.Cut(term, "=")
		got, has := dig(obj, "metadata", "labels").(map[string]any)[k]
		if !has || (eq && got != v) {
			return false
		}
	}
	return true
}

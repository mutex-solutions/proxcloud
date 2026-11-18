package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// workerPVE is the part of Proxmox the worker firewall talks to, with one running worker (VM 201) of cluster pc-c-a.
type workerPVE struct {
	mu     sync.Mutex
	dcOn   bool
	groups map[string][]map[string]any // group id -> rules, newest first
	notes  map[string]string
	net0   string
	opts   map[string]any
	rules  []map[string]any
	sets   map[string][]map[string]any
	writes []string
}

func newWorkerPVE(t *testing.T) (*workerPVE, *pve.Service) {
	f := &workerPVE{dcOn: true, groups: map[string][]map[string]any{}, notes: map[string]string{},
		net0: "virtio=BC:24:11:00:00:01,bridge=vtenant", opts: map[string]any{}, sets: map[string][]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "proxcloud@pve!platform", Secret: "s"}, "pve", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	return f, ps
}

func (f *workerPVE) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	p := strings.TrimPrefix(r.URL.Path, "/api2/json")
	seg := strings.Split(strings.Trim(p, "/"), "/")
	get := r.Method == http.MethodGet
	if !get {
		f.writes = append(f.writes, r.Method+" "+p)
	}
	form := func() map[string]any {
		out := map[string]any{}
		for k := range r.Form {
			out[k] = r.Form.Get(k)
		}
		if en, err := strconv.Atoi(r.Form.Get("enable")); err == nil {
			out["enable"] = en
		}
		return out
	}
	listed := func(rs []map[string]any) []map[string]any {
		out := make([]map[string]any, 0, len(rs))
		for i, x := range rs {
			c := map[string]any{"pos": i, "digest": fmt.Sprint(len(rs))}
			for k, v := range x {
				c[k] = v
			}
			out = append(out, c)
		}
		return out
	}
	reply := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	const vm = "/nodes/pve1/qemu/201/"
	switch {
	case p == "/cluster/firewall/options":
		reply(map[string]any{"enable": map[bool]int{true: 1}[f.dcOn]})
	case p == "/cluster/resources" && r.Form.Get("type") == "node":
		reply([]map[string]any{{"type": "node", "node": "pve1", "status": "online"}})
	case p == "/cluster/resources":
		reply([]map[string]any{{"type": "qemu", "vmid": 201, "node": "pve1", "name": "pc-c-a-default-pve1-x", "pool": "pc-p1",
			"status": "running", "tags": "proxcloud;pc-k8s-pc-c-a"}})
	case p == "/cluster/firewall/groups" && get:
		out := []map[string]any{}
		for g := range f.groups {
			out = append(out, map[string]any{"group": g, "comment": f.notes[g]})
		}
		reply(out)
	case p == "/cluster/firewall/groups":
		f.groups[r.Form.Get("group")], f.notes[r.Form.Get("group")] = nil, r.Form.Get("comment")
		reply(nil)
	case strings.HasPrefix(p, "/cluster/firewall/groups/"):
		g := seg[3]
		switch {
		case get:
			reply(listed(f.groups[g]))
		case r.Method == http.MethodPost:
			f.groups[g] = append([]map[string]any{form()}, f.groups[g]...)
			reply(nil)
		case len(seg) == 5:
			pos, _ := strconv.Atoi(seg[4])
			f.groups[g] = slices.Delete(f.groups[g], pos, pos+1)
			reply(nil)
		default:
			delete(f.groups, g)
			reply(nil)
		}
	case strings.HasPrefix(p, "/nodes/pve1/tasks/"):
		reply(map[string]any{"status": "stopped", "exitstatus": "OK"})
	case p == vm+"config" && get:
		reply(map[string]any{"net0": f.net0})
	case p == vm+"config":
		f.net0 = r.Form.Get("net0")
		reply("UPID:pve1:1:1:1:qmconfig:201:x:")
	case p == vm+"firewall/options" && get:
		reply(f.opts)
	case p == vm+"firewall/options":
		for k, v := range form() {
			f.opts[k] = fmt.Sprint(v)
		}
		reply(nil)
	case p == vm+"firewall/rules" && get:
		reply(listed(f.rules))
	case p == vm+"firewall/rules":
		f.rules = append([]map[string]any{form()}, f.rules...)
		reply(nil)
	case strings.HasPrefix(p, vm+"firewall/rules/"):
		pos, _ := strconv.Atoi(seg[len(seg)-1])
		f.rules = slices.Delete(f.rules, pos, pos+1)
		reply(nil)
	case p == vm+"firewall/ipset" && get:
		out := []map[string]any{}
		for n := range f.sets {
			out = append(out, map[string]any{"name": n})
		}
		reply(out)
	case p == vm+"firewall/ipset":
		f.sets[r.Form.Get("name")] = []map[string]any{}
		reply(nil)
	case strings.HasPrefix(p, vm+"firewall/ipset/"):
		n := seg[len(seg)-1]
		switch r.Method {
		case http.MethodGet:
			reply(f.sets[n])
		case http.MethodPost:
			f.sets[n] = append(f.sets[n], map[string]any{"cidr": r.Form.Get("cidr")})
			reply(nil)
		default:
			delete(f.sets, n)
			reply(nil)
		}
	default:
		http.Error(w, "no fake for "+r.Method+" "+p, http.StatusNotImplemented)
	}
}

// tenantNodes is a tenant cluster with one node; its Lease renews on every read while alive.
type tenantNodes struct {
	address string
	alive   bool
	reads   int
}

func (n *tenantNodes) ListNodes(context.Context) ([]map[string]any, error) {
	return []map[string]any{{
		"metadata": map[string]any{"name": "pc-c-a-default-pve1-x"},
		"status": map[string]any{
			"addresses":  []any{map[string]any{"type": "Hostname", "address": "x"}, map[string]any{"type": "InternalIP", "address": n.address}},
			"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
		},
	}}, nil
}

func (n *tenantNodes) ListCRs(context.Context, string, string, string, string, string) ([]map[string]any, error) {
	if n.alive {
		n.reads++
	}
	return []map[string]any{{"metadata": map[string]any{"name": "pc-c-a-default-pve1-x"},
		"spec": map[string]any{"renewTime": fmt.Sprintf("2026-11-18T10:00:%02d.000000Z", n.reads)}}}, nil
}

func TestProtectClusterWorkers(t *testing.T) {
	workerFirewallSettle, workerFirewallWatch, workerFirewallPoll = time.Millisecond, 20*time.Millisecond, time.Millisecond
	ctx := context.Background()
	f := pve.WorkerFirewall{ProjectID: "p1", ClusterID: "pc-c-a", CIDRPool: "10.100.0.0/16",
		Workers: []string{"10.100.0.232", "10.100.0.233"}, LoadBalancers: []string{"10.100.0.248"}}
	addrs := []string{"10.100.0.232", "10.100.0.233", "10.100.0.248"}
	conflict := func(err error, want string) bool {
		var he *httpx.HTTPError
		return errors.As(err, &he) && he.Status == http.StatusConflict && strings.Contains(he.Msg, want)
	}

	// Refused before anything is written: Proxmox would enforce nothing, or the filter would cut a node off.
	fake, ps := newWorkerPVE(t)
	fake.dcOn = false
	if _, err := protectClusterWorkers(ctx, ps, &tenantNodes{address: "10.100.0.232", alive: true}, f, addrs); !conflict(err, "datacenter firewall") {
		t.Fatalf("datacenter firewall off: %v", err)
	}
	fake.dcOn = true
	if _, err := protectClusterWorkers(ctx, ps, &tenantNodes{address: "10.100.0.9", alive: true}, f, addrs); !conflict(err, "not one of the cluster's reserved worker addresses") {
		t.Fatalf("a node outside the reserved addresses: %v", err)
	}
	if _, err := protectClusterWorkers(ctx, ps, &tenantNodes{address: "10.100.0.232", alive: true}, f, nil); !conflict(err, "no reserved addresses") {
		t.Fatalf("no reserved addresses: %v", err)
	}
	if len(fake.writes) != 0 {
		t.Fatalf("a refused protect writes nothing: %v", fake.writes)
	}

	// The node stops renewing its Lease once the firewall is on: everything is taken off again.
	_, err := protectClusterWorkers(ctx, ps, &tenantNodes{address: "10.100.0.232"}, f, addrs)
	if !conflict(err, "did not renew the node Lease") || !conflict(err, "taken off again") {
		t.Fatalf("a node that goes silent: %v", err)
	}
	if fake.opts["enable"] != "0" || len(fake.rules) != 0 || len(fake.groups) != 0 {
		t.Fatalf("after the way back: options %v, rules %v, groups %v", fake.opts, fake.rules, fake.groups)
	}

	// The node keeps renewing: protected, verified, and from now on the sync's to keep.
	rep, err := protectClusterWorkers(ctx, ps, &tenantNodes{address: "10.100.0.232", alive: true}, f, addrs)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Protected || !rep.Verified || !rep.Enforced || rep.GroupName != "k8s-pc-c-a" || len(rep.Workers) != 1 || !rep.Workers[0].Secured {
		t.Fatalf("report %+v", rep)
	}
	if fake.opts["enable"] != "1" || fake.opts["policy_in"] != "DROP" || !strings.HasSuffix(fake.net0, ",firewall=1") || len(fake.rules) != 1 {
		t.Fatalf("worker after protect: %v %q %v", fake.opts, fake.net0, fake.rules)
	}

	// Again on a protected cluster: nothing to write, nothing to watch, and a silent node takes nothing off.
	fake.writes = nil
	rep, err = protectClusterWorkers(ctx, ps, &tenantNodes{address: "10.100.0.232"}, f, addrs)
	if err != nil || rep.Verified || !rep.Protected || len(fake.writes) != 0 {
		t.Fatalf("second protect: %+v %v, wrote %v", rep, err, fake.writes)
	}
}

// The addresses the chart gave MetalLB are load balancer addresses, the rest of the reservation is for workers: the
// group opens the first to anyone and the kubelet only to the second.
func TestSplitClusterAddresses(t *testing.T) {
	f := splitClusterAddresses([]string{"10.100.0.232", "10.100.0.233", "10.100.0.248", "10.100.0.249"}, []string{"10.100.0.248/32", "10.100.0.249/32"})
	if !slices.Equal(f.Workers, []string{"10.100.0.232", "10.100.0.233"}) || !slices.Equal(f.LoadBalancers, []string{"10.100.0.248", "10.100.0.249"}) {
		t.Fatalf("workers %v, load balancers %v", f.Workers, f.LoadBalancers)
	}
}

// sharedWorkers puts the workers in the operator's pool, on a network tenants share: no project owns a group for
// them, so the worker firewall refuses before it looks anything up.
func TestWorkerFirewallRefusesSharedWorkers(t *testing.T) {
	ks := kamaji.NewWithAPI(nil, kamaji.Config{Defaults: kamaji.ClusterDefaults{Shared: &kamaji.SharedWorkers{}}}, "k8s")
	_, _, _, err := (&Handler{}).workerFirewallTarget(context.Background(), ks, "pc-c-a")
	var he *httpx.HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusConflict || !strings.Contains(he.Msg, "sharedWorkers") {
		t.Fatalf("sharedWorkers: %v", err)
	}
}

// The provider sync's pass waits for a Protect or a Remove that is under way: it must not write DROP on a worker
// between a failed Protect's way back and the delete of the group.
func TestWorkerFirewallSyncWaitsForProtect(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()
	ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "proxcloud@pve!platform", Secret: "s"}, "pve", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	workerFirewallMu.Lock() // a Protect is watching the nodes
	done := make(chan error, 1)
	go func() { done <- (&Handler{}).ReconcileWorkerFirewalls(context.Background(), ps, nil) }()
	select {
	case <-done:
		t.Fatal("the pass ran while a Protect held the lock")
	case <-time.After(50 * time.Millisecond):
	}
	if calls.Load() != 0 {
		t.Fatal("the pass reached Proxmox while a Protect held the lock")
	}
	workerFirewallMu.Unlock()
	if err := <-done; err != nil || calls.Load() == 0 {
		t.Fatalf("the pass after the Protect: %v, %d calls", err, calls.Load())
	}
}

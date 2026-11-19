package pve

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func testWorkerFirewall() WorkerFirewall {
	return WorkerFirewall{
		ProjectID: "p1", ClusterID: "pc-c-0a1b2c3d", CIDRPool: "10.100.0.0/16",
		Workers:       []string{"10.100.0.232", "10.100.0.233", "10.100.0.234", "10.100.0.235", "10.100.0.240"},
		LoadBalancers: []string{"10.100.0.248/32", "10.100.0.249/32"},
	}
}

func workerAddrs(f WorkerFirewall) []string {
	out := slices.Clone(f.Workers)
	for _, lb := range f.LoadBalancers {
		out = append(out, strings.TrimSuffix(lb, "/32"))
	}
	return out
}

// What a worker accepts is the documented table (docs/managed-k8s.md, "Worker firewall"): nothing else, and the
// kubelet only from the cluster's own workers.
func TestWorkerGroupRules(t *testing.T) {
	rules, err := testWorkerFirewall().rules()
	if err != nil {
		t.Fatal(err)
	}
	const workers = "10.100.0.232/30,10.100.0.240"
	var got []string
	for _, r := range rules {
		if r.Type != "in" || r.Action != "ACCEPT" || r.Enable != 1 || r.Comment == "" {
			t.Errorf("every rule is an enabled inbound ACCEPT with a comment: %+v", r)
		}
		got = append(got, strings.Join([]string{r.Proto, r.Source, r.Dest, r.DPort}, "|"))
	}
	want := []string{
		"udp|" + workers + "||8472", "tcp|" + workers + "||4240", "icmp|" + workers + "||", "tcp|" + workers + "||10250",
		"tcp|" + workers + "||7946", "udp|" + workers + "||7946",
		"||10.100.0.248/31|",
		"tcp|10.100.0.0/16||30000:32767", "udp|10.100.0.0/16||30000:32767",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rules\n got %q\nwant %q", got, want)
	}
	f := testWorkerFirewall()
	f.Workers = nil
	if _, err := f.rules(); err == nil {
		t.Fatal("no worker addresses: want an error, not a group that accepts nothing between workers")
	}
	f.Workers = []string{"10.100.0.0/24"}
	if _, err := f.rules(); err == nil {
		t.Fatal("a network as a worker address: want an error")
	}
}

func TestWorkerFirewallSecure(t *testing.T) {
	ctx := context.Background()
	fake, s := newSandboxFake(t)
	f := testWorkerFirewall()
	addrs := workerAddrs(f)
	tag := "proxcloud;" + WorkerTagPrefix + f.ClusterID
	w1 := fake.addVM(201, f.ClusterID+"-default-pve1-abc", "pc-p1", "virtio=BC:24:11:00:00:01,bridge=vtenant")
	w1.tags = tag
	w2 := fake.addVM(202, f.ClusterID+"-default-pve1-def", "pc-p1", "virtio=BC:24:11:00:00:02,bridge=vtenant")
	w2.tags, w2.status = tag, "stopped" // CAPMOX has not started it yet
	server := fake.addVM(203, "web", "pc-p1", "virtio=BC:24:11:00:00:03,bridge=vtenant")
	server.tags = "proxcloud"
	other := fake.addVM(204, f.ClusterID+"-default-pve1-xyz", "pc-p2", "virtio=BC:24:11:00:00:04,bridge=vtenant")
	other.tags = tag // another project's pool: never this cluster's

	// No group: the cluster is not protected and a pass writes nothing.
	if err := s.ReconcileWorkerFirewalls(ctx, func(context.Context, string, string) ([]string, error) { return addrs, nil }); err != nil {
		t.Fatal(err)
	}
	if len(fake.writes) != 0 {
		t.Fatalf("a cluster without a group must stay untouched, wrote %v", fake.writes)
	}

	group, err := s.EnsureWorkerGroup(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if fake.grpNote[group] != OwnershipAlias("p1", "k8s-"+f.ClusterID) || len(fake.groups[group]) != 9 {
		t.Fatalf("group %q: %q, %d rules", group, fake.grpNote[group], len(fake.groups[group]))
	}
	if fake.groups[group][0].DPort != "8472" {
		t.Fatalf("the rules read top down in the documented order, first is %+v", fake.groups[group][0])
	}
	// The tenant edits the group; a second ensure leaves it alone.
	fake.groups[group] = fake.groups[group][:8]
	if again, err := s.EnsureWorkerGroup(ctx, f); err != nil || again != group || len(fake.groups[group]) != 8 {
		t.Fatalf("an existing group is not rewritten: %q %v, %d rules", again, err, len(fake.groups[group]))
	}

	fake.writes = nil
	states, err := s.SecureClusterWorkers(ctx, "p1", f.ClusterID, group, addrs)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || !states[0].Secured || !states[0].Changed || states[1].Secured {
		t.Fatalf("the running worker is secured, the stopped one waits: %+v", states)
	}
	if got := cidrsOf(w1.sets["ipfilter-net0"]); !slices.Equal(got, slices.Sorted(slices.Values(addrs))) {
		t.Fatalf("ip filter %v, want the cluster's worker and load balancer addresses %v", got, addrs)
	}
	if len(w1.rules) != 1 || w1.rules[0].Type != "group" || w1.rules[0].Action != group {
		t.Fatalf("worker rules %+v", w1.rules)
	}
	for k, v := range map[string]string{"enable": "1", "ipfilter": "1", "macfilter": "1", "dhcp": "0", "policy_in": "DROP", "policy_out": "ACCEPT"} {
		if w1.opts[k] != v {
			t.Errorf("option %s = %v, want %s", k, w1.opts[k], v)
		}
	}
	if w1.cfg.Str("net0") != "virtio=BC:24:11:00:00:01,bridge=vtenant,firewall=1" {
		t.Fatalf("net0 %q", w1.cfg.Str("net0"))
	}
	// The NIC flag is what makes PVE apply the rest, so it is the last write: no half-written firewall filters a
	// running worker.
	if last := fake.writes[len(fake.writes)-1]; last != "PUT /nodes/pve1/qemu/201/config" && last != "POST /nodes/pve1/qemu/201/config" {
		t.Fatalf("last write %q, want the NIC flag", last)
	}
	for _, vm := range []*fakeVM{w2, server, other} {
		if len(vm.rules) != 0 || len(vm.opts) != 0 || len(vm.sets) != 0 || strings.Contains(vm.cfg.Str("net0"), "firewall") {
			t.Fatalf("%s must be untouched: %+v %v %v", vm.name, vm.rules, vm.opts, vm.cfg)
		}
	}

	// In place: a second pass and the read-only view write nothing.
	fake.writes = nil
	states, err = s.SecureClusterWorkers(ctx, "p1", f.ClusterID, group, addrs)
	if err != nil || !states[0].Secured || states[0].Changed || len(fake.writes) != 0 {
		t.Fatalf("second pass: %+v %v, wrote %v", states, err, fake.writes)
	}
	if states, err = s.ClusterWorkers(ctx, "p1", f.ClusterID, group, addrs); err != nil || !states[0].Secured || len(fake.writes) != 0 {
		t.Fatalf("read-only view: %+v %v, wrote %v", states, err, fake.writes)
	}

	// A replacement worker starts: the provider sync covers it, by the group alone.
	w2.status = ""
	asked := ""
	err = s.ReconcileWorkerFirewalls(ctx, func(_ context.Context, projectID, clusterID string) ([]string, error) {
		asked = projectID + "/" + clusterID
		return addrs, nil
	})
	if err != nil || asked != "p1/"+f.ClusterID {
		t.Fatalf("reconcile: %v, asked %q", err, asked)
	}
	if len(w2.rules) != 1 || w2.opts["policy_in"] != "DROP" || !strings.HasSuffix(w2.cfg.Str("net0"), ",firewall=1") {
		t.Fatalf("the new worker is behind the firewall after the sync: %+v %v %q", w2.rules, w2.opts, w2.cfg.Str("net0"))
	}
	// No addresses (the cluster is being released, or belongs to another portal): nothing, never an empty filter.
	w2.opts = map[string]any{}
	if err := s.ReconcileWorkerFirewalls(ctx, func(context.Context, string, string) ([]string, error) { return nil, nil }); err != nil || len(w2.opts) != 0 {
		t.Fatalf("no addresses must skip the cluster: %v %v", err, w2.opts)
	}
	if _, err := s.SecureClusterWorkers(ctx, "p1", f.ClusterID, group, nil); err == nil {
		t.Fatal("an empty IP filter would cut every worker off: want an error")
	}

	// The way back: filtering stops, the group goes.
	if err := s.UnsecureClusterWorkers(ctx, "p1", f.ClusterID); err != nil {
		t.Fatal(err)
	}
	if w1.opts["enable"] != "0" || len(w1.rules) != 0 || len(fake.groups) != 0 {
		t.Fatalf("after unsecure: %v %+v, groups %v", w1.opts, w1.rules, fake.groups)
	}
	if id, err := s.WorkerGroup(ctx, "p1", f.ClusterID); err != nil || id != "" {
		t.Fatalf("group after unsecure: %q %v", id, err)
	}
}

// A cluster's group is deleted with the cluster and by "Remove the worker firewall", so it never goes on an ordinary
// server: not by attach, not at create. A group of that form already on a server (attached before this refusal) is
// not deleted from under it.
func TestWorkerGroupStaysOffServers(t *testing.T) {
	ctx := context.Background()
	fake, s := newSandboxFake(t)
	f := testWorkerFirewall()
	web := fake.addVM(203, "web", "pc-p1", "virtio=BC:24:11:00:00:03,bridge=vtenant")
	web.tags = "proxcloud"
	group, err := s.EnsureWorkerGroup(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	fake.writes = nil
	var ce *ConflictError
	if err := s.AttachSecurityGroup(ctx, "p1", "203", group); !errors.As(err, &ce) || !strings.Contains(ce.Msg, "Kubernetes cluster") {
		t.Fatalf("attach of a cluster group to a server: %v", err)
	}
	if _, err := s.CreateServer(ctx, CreateServerReq{ProjectID: "p1", Name: "web2", SecurityGroups: []string{group}}); !errors.As(err, &ce) {
		t.Fatalf("create of a server with a cluster group: %v", err)
	}
	if len(fake.writes) != 0 || len(web.rules) != 0 {
		t.Fatalf("a refused group writes nothing: %v", fake.writes)
	}
	// Another project's group of that name is not this project's business; a plain group still passes.
	if err := s.refuseWorkerGroups(ctx, "p2", []string{group}); err != nil {
		t.Fatal(err)
	}

	// The server references the group all the same.
	web.rules = []FirewallRule{{Type: "group", Action: group, Enable: 1}}
	if err := s.UnsecureClusterWorkers(ctx, "p1", f.ClusterID); !errors.As(err, &ce) || !strings.Contains(ce.Msg, "web") {
		t.Fatalf("remove while a server uses the group: %v", err)
	}
	if err := s.DeleteWorkerGroup(ctx, "p1", f.ClusterID); err != nil {
		t.Fatalf("the release leaves a referenced group and goes on: %v", err)
	}
	if len(fake.writes) != 0 || len(fake.groups) != 1 {
		t.Fatalf("a referenced group must stay: wrote %v, groups %v", fake.writes, fake.groups)
	}
	web.rules = nil
	if err := s.DeleteWorkerGroup(ctx, "p1", f.ClusterID); err != nil || len(fake.groups) != 0 {
		t.Fatalf("an unreferenced group goes with the cluster: %v, groups %v", err, fake.groups)
	}
}

// A worker on a node that is down is not written by the pass, and holds back Remove before anything is written: it
// would come back dropping everything with a reference to a group that is gone.
func TestWorkerFirewallNodeDown(t *testing.T) {
	ctx := context.Background()
	fake, s := newSandboxFake(t)
	f := testWorkerFirewall()
	addrs := workerAddrs(f)
	tag := "proxcloud;" + WorkerTagPrefix + f.ClusterID
	up := fake.addVM(201, f.ClusterID+"-default-pve1-abc", "pc-p1", "virtio=BC:24:11:00:00:01,bridge=vtenant")
	up.tags = tag
	down := fake.addVM(202, f.ClusterID+"-default-pve2-def", "pc-p1", "virtio=BC:24:11:00:00:02,bridge=vtenant")
	down.tags, down.node = tag, "pve2"
	group, err := s.EnsureWorkerGroup(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	states, err := s.SecureClusterWorkers(ctx, "p1", f.ClusterID, group, addrs)
	if err != nil || len(states) != 2 || !states[0].Secured || states[1].Secured || len(down.rules) != 0 || len(down.opts) != 0 {
		t.Fatalf("the pass skips the worker it cannot reach: %+v %v", states, err)
	}
	fake.writes = nil
	var ce *ConflictError
	if err := s.UnsecureClusterWorkers(ctx, "p1", f.ClusterID); !errors.As(err, &ce) || !strings.Contains(ce.Msg, "pve2") {
		t.Fatalf("remove with a worker on a down node: %v", err)
	}
	if len(fake.writes) != 0 || up.opts["enable"] != "1" {
		t.Fatalf("a refused remove writes nothing: %v", fake.writes)
	}
	down.node = ""
	if err := s.UnsecureClusterWorkers(ctx, "p1", f.ClusterID); err != nil || len(fake.groups) != 0 {
		t.Fatalf("remove once the node is back: %v", err)
	}
}

// A tenant cannot attach a group to a worker row, or take the cluster's group off it.
func TestWorkerRefusesSecurityGroupChanges(t *testing.T) {
	ctx := context.Background()
	fake, s := newSandboxFake(t)
	w := fake.addVM(201, "pc-c-0a1b2c3d-default-pve1-abc", "pc-p1", "virtio=BC:24:11:00:00:01,bridge=vtenant")
	w.tags = "proxcloud;pc-k8s-pc-c-0a1b2c3d"
	id, err := s.CreateSecurityGroup(ctx, "p1", "web")
	if err != nil {
		t.Fatal(err)
	}
	fake.writes = nil
	var ce *ConflictError
	if err := s.AttachSecurityGroup(ctx, "p1", "201", id); !errors.As(err, &ce) || !strings.Contains(ce.Msg, "worker node") {
		t.Fatalf("attach: %v", err)
	}
	own, err := s.EnsureWorkerGroup(ctx, WorkerFirewall{ProjectID: "p1", ClusterID: "pc-c-0a1b2c3d", Workers: []string{"10.100.0.10"}})
	if err != nil {
		t.Fatal(err)
	}
	w.rules = []FirewallRule{{Type: "group", Action: own, Enable: 1}, {Type: "group", Action: id, Enable: 1}}
	fake.writes = nil
	if err := s.DetachSecurityGroup(ctx, "p1", "201", own); !errors.As(err, &ce) {
		t.Fatalf("detach of the cluster's group: %v", err)
	}
	if len(fake.writes) != 0 {
		t.Fatalf("a refused change writes nothing: %v", fake.writes)
	}
	// A group attached before attaching was refused can still be taken off.
	if err := s.DetachSecurityGroup(ctx, "p1", "201", id); err != nil || len(w.rules) != 1 || w.rules[0].Action != own {
		t.Fatalf("detach of an older group from a worker: %v, rules %+v", err, w.rules)
	}
}

func TestWorkerNames(t *testing.T) {
	if !IsWorkerGroupName("k8s-pc-c-0a1b2c3d") || IsWorkerGroupName("web-k8s-") || IsWorkerGroupName("default") {
		t.Fatal("only names starting with k8s- are reserved for clusters")
	}
	if id, ok := WorkerGroupCluster("k8s-pc-c-0a1b2c3d"); !ok || id != "pc-c-0a1b2c3d" {
		t.Fatalf("cluster of a group name: %q %v", id, ok)
	}
	if !IsWorkerVM("proxcloud;fl-small;pc-k8s-pc-c-0a1b2c3d") || IsWorkerVM("proxcloud;fl-small") || IsWorkerVM("") {
		t.Fatal("a worker is a VM with a pc-k8s-<cluster> tag")
	}
}

// The sync sets onboot on a running worker of a cluster the portal has, once. A stopped worker (CAPMOX still writes
// it), a worker that has it, a server, and a VM that carries a worker tag in a pool or for a cluster that is not the
// portal's are not written.
func TestStartWorkersAtBoot(t *testing.T) {
	ctx := context.Background()
	fake, s := newSandboxFake(t)
	tag := "proxcloud;pc-flavor-s2-medium;" + WorkerTagPrefix + "c1"
	lacks := fake.addVM(201, "c1-default-pve1-abc", "pc-p1")
	lacks.tags = tag
	has := fake.addVM(202, "c1-default-pve1-def", "pc-p1")
	has.tags, has.cfg["onboot"] = tag, 1
	stopped := fake.addVM(203, "c1-default-pve1-ghi", "pc-p1")
	stopped.tags, stopped.status = tag, "stopped"
	fake.addVM(204, "web", "pc-p1").tags = "proxcloud"
	fake.addVM(205, "c1-default-pve1-xyz", "pc-p2").tags = tag                              // another project's pool
	fake.addVM(206, "c9-default-pve1-xyz", "pc-p1").tags = WorkerTagPrefix + "c9"           // a cluster the project does not have
	fake.addVM(207, "c1-default-pve1-own", "site").tags = tag                               // not a pool of this provider's projects
	fake.addVM(208, "pc-ghcr-ubuntu-2604-k8s", TemplatePool).tags = "pc-k8s;pc-k8s-1-35-10" // a node image: its tags read like a worker's
	asked := 0
	owns := func(_ context.Context, projectID, clusterID string) (bool, error) {
		asked++
		return projectID == "p1" && clusterID == "c1", nil
	}
	for pass := 1; pass <= 2; pass++ {
		fake.writes = nil
		if err := s.StartWorkersAtBoot(ctx, owns); err != nil {
			t.Fatal(err)
		}
		want := []string{"POST /nodes/pve1/qemu/201/config"}
		if pass == 2 {
			want = nil
		}
		if !slices.Equal(fake.writes, want) {
			t.Fatalf("pass %d wrote %v, want %v", pass, fake.writes, want)
		}
	}
	if lacks.cfg.Int("onboot") != 1 || stopped.cfg.Int("onboot") != 0 {
		t.Fatalf("onboot: running %v, stopped %v", lacks.cfg["onboot"], stopped.cfg["onboot"])
	}
	if asked != 2*4 { // p1/c1, p2/c1, p1/c9 and the template pool's version tag: once per pass, not once per VM
		t.Fatalf("owns asked %d times", asked)
	}
}

// The sync moves the VMs and templates of a project's pool from a serial display to a graphical one, once, and leaves
// the serial port. A VM that has a graphical display, a locked VM, a stopped worker (CAPMOX still writes it) and a VM
// in another pool (another project's, the site's own) are not written. Nothing is stopped or started.
func TestShowGraphicalDisplay(t *testing.T) {
	ctx := context.Background()
	fake, s := newSandboxFake(t)
	serial := func(vmid int, name, pool string) *fakeVM {
		vm := fake.addVM(vmid, name, pool)
		vm.cfg["vga"], vm.cfg["serial0"] = "serial0", "socket"
		return vm
	}
	web := serial(301, "web", "pc-p1")
	off := serial(302, "db", "pc-p1")
	off.status = "stopped"
	worker := serial(303, "c1-default-pve1-abc", "pc-p1")
	worker.tags = WorkerTagPrefix + "c1"
	fresh := serial(304, "c1-default-pve1-def", "pc-p1")
	fresh.tags, fresh.status = WorkerTagPrefix+"c1", "stopped"
	serial(305, "busy", "pc-p1").cfg["lock"] = "clone"
	fake.addVM(306, "new", "pc-p1").cfg["vga"] = "std"
	fake.addVM(307, "default", "pc-p1")
	serial(308, "web", "pc-p2")
	serial(309, "own", "site")
	for pass := 1; pass <= 2; pass++ {
		fake.writes = nil
		if err := s.ShowGraphicalDisplay(ctx, "p1"); err != nil {
			t.Fatal(err)
		}
		want := []string{"POST /nodes/pve1/qemu/301/config", "POST /nodes/pve1/qemu/302/config", "POST /nodes/pve1/qemu/303/config"}
		if pass == 2 {
			want = nil
		}
		if slices.Sort(fake.writes); !slices.Equal(fake.writes, want) {
			t.Fatalf("pass %d wrote %v, want %v", pass, fake.writes, want)
		}
	}
	for _, vm := range []*fakeVM{web, off, worker} {
		if vm.cfg.Str("vga") != "std" || vm.cfg.Str("serial0") != "socket" {
			t.Fatalf("%s: vga %v serial0 %v", vm.name, vm.cfg["vga"], vm.cfg["serial0"])
		}
	}
	if fresh.cfg.Str("vga") != "serial0" || fake.vms[305].cfg.Str("vga") != "serial0" || fake.vms[308].cfg.Str("vga") != "serial0" || fake.vms[309].cfg.Str("vga") != "serial0" {
		t.Fatal("a stopped worker, a locked VM or a VM of another pool was written")
	}
}

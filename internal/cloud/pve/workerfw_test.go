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
	if err := s.DetachSecurityGroup(ctx, "p1", "201", id); !errors.As(err, &ce) {
		t.Fatalf("detach: %v", err)
	}
	if len(fake.writes) != 0 {
		t.Fatalf("a refused change writes nothing: %v", fake.writes)
	}
}

func TestWorkerNames(t *testing.T) {
	if !IsWorkerGroupName("k8s-pc-c-0a1b2c3d") || IsWorkerGroupName("web-k8s-") || IsWorkerGroupName("default") {
		t.Fatal("only names starting with k8s- are reserved for clusters")
	}
	if !IsWorkerVM("proxcloud;fl-small;pc-k8s-pc-c-0a1b2c3d") || IsWorkerVM("proxcloud;fl-small") || IsWorkerVM("") {
		t.Fatal("a worker is a VM with a pc-k8s-<cluster> tag")
	}
}

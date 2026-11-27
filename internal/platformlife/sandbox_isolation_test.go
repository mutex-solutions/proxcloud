package platformlife

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// The run and the provider-wide isolation rules, on a site as the drill of 2026-11-26 found one (findings Z3-A and
// Z4-A): NO provider sync runs anywhere in these tests (the scheduler is off, or the next timed sync is minutes
// away), the cluster firewall holds rules of other owners, and Proxmox holds another stack's firewall group. The
// rules are Proxmox's as the fake keeps them: a list, a new rule on top, a delete by position. What a real
// proxmox-firewall does with them is the lab drill's to show.

const (
	isoOwner = "proxcloud:isolation"
	isoPool  = "10.100.0.0/16"
	isoRange = "10.200.200.64/28"
	isoPeers = "10.200.200.30/31,10.200.200.32/28,10.200.200.48/31"
	// The blocked networks as the rules name them: the provider's and the node's management network, sorted.
	isoBlocked = "10.200.200.0/26,192.0.2.0/24"
)

var (
	isoBase = []string{
		"forward DROP " + isoPool + " > " + isoPool,
		"in DROP " + isoPool + " > " + isoPool,
		"forward DROP " + isoPool + " > " + isoBlocked,
		"in DROP " + isoPool + " > " + isoBlocked,
	}
	isoShared = []string{
		"forward DROP " + isoPool + " > " + isoPool,
		"in DROP " + isoPool + " > " + isoPool,
		"forward DROP " + isoPool + " > " + isoBlocked + "," + isoRange,
		"in DROP " + isoPool + " > " + isoBlocked + "," + isoRange,
		"forward ACCEPT tcp " + isoRange + " > 10.200.200.20 6443",
		"forward ACCEPT tcp " + isoRange + " > " + isoPeers + " 6443",
		"forward ACCEPT udp " + isoRange + " > " + isoPeers + " 8472",
		"forward ACCEPT tcp " + isoRange + " > " + isoPeers + " 4240",
		"forward ACCEPT icmp " + isoRange + " > " + isoPeers,
		"forward DROP " + isoRange + " > " + isoPool + "," + isoBlocked,
		"in DROP " + isoRange + " > " + isoPool + "," + isoBlocked,
	}
	// separate: the range is fenced and nothing of the platform cluster is opened to it.
	isoSeparate = slices.Concat(isoShared[:4], isoShared[9:])
)

type isolationRig struct {
	*platform
	es  *externalservice.ExternalService
	svc *pve.Service
	// foreign are the rules of other owners, as they were put into the cluster firewall.
	foreign []pve.FirewallRule
}

// isolationPlatform is sandboxPlatform on a provider with a tenant pool, whose Check row isolation is the real one
// (Preflight.isolationRow against the fake Proxmox) and whose sandbox settings are written as the admin handler
// writes the provider row: the block and the range in one write, or both taken away.
func isolationPlatform(t *testing.T, sb *SandboxSpec) *isolationRig {
	t.Helper()
	ctx := context.Background()
	p := sandboxPlatform(t, RunApply, sb)
	es, _ := p.r.Provider(ctx, "svc-pve")
	es.Config["network"] = map[string]any{"cidrPool": isoPool, "blockedCidrs": []any{"10.200.200.0/26"}}
	client, err := pve.New(es.PVEConfig())
	if err != nil {
		t.Fatal(err)
	}
	r := &isolationRig{platform: p, es: es, svc: &pve.Service{C: client, PoolPrefix: es.PVEPoolPrefix()}}
	p.r.SandboxSite = func(_ context.Context, _ *Installation, s *SandboxSite) error {
		p.sites = append(p.sites, s)
		network := es.Config["network"].(map[string]any)
		if s.Config == nil {
			delete(es.Config, "sandbox")
			delete(network, "sandboxCidrs")
			return nil
		}
		es.Config["sandbox"], network["sandboxCidrs"] = s.Config, []any{s.Range}
		return nil
	}
	p.r.Preflight = func(ctx context.Context, in *Installation) ([]Check, error) { return []Check{r.row(ctx, in)}, nil }
	// Another stack on the same Proxmox cluster, and an operator: an isolation set under another pool prefix with a
	// sandbox network of its own, the platform's PVE API rule, an ssh rule, and a firewall group with its IP sets.
	const other = "proxcloud:isolation/sb8-"
	r.foreign = []pve.FirewallRule{
		{Type: "in", Action: "ACCEPT", Proto: "tcp", Source: "10.200.200.10,10.200.200.30/31", Dest: "192.0.2.0/24", DPort: "8006", Enable: 1, Comment: "proxcloud:pve-api:pi-0a1b2c3d"},
		{Type: "forward", Action: "DROP", Source: "10.120.0.0/16", Dest: "10.120.0.0/16", Enable: 1, Comment: other},
		{Type: "in", Action: "DROP", Source: "10.120.0.0/16", Dest: "10.120.0.0/16", Enable: 1, Comment: other},
		{Type: "forward", Action: "DROP", Source: "10.120.0.0/16", Dest: "192.0.2.0/24,10.110.0.0/24", Enable: 1, Comment: other},
		{Type: "forward", Action: "ACCEPT", Proto: "tcp", Source: "10.110.0.0/24", Dest: "192.0.2.41", DPort: "9000", Enable: 1, Comment: other},
		{Type: "forward", Action: "DROP", Source: "10.110.0.0/24", Dest: "10.120.0.0/16,192.0.2.0/24", Enable: 1, Comment: other},
		{Type: "in", Action: "ACCEPT", Proto: "tcp", Source: "198.51.100.0/24", DPort: "22", Enable: 1, Comment: "ops ssh"},
	}
	p.pve.clusterRules = slices.Clone(r.foreign)
	p.pve.fwGroups, p.pve.fwSets = []string{"sbx823effd548d9"}, []string{"sbx823effd548d9-n", "sbx823effd548d9-o"}
	return r
}

// sharedBlock is config.sandbox and the range as an earlier apply of this installation wrote them.
func (r *isolationRig) sharedBlock() {
	_, in, _ := r.state.snapshot()
	r.es.Config["sandbox"] = map[string]any{ProviderMarker: in.ID, "mode": SandboxShared, "pool": pve.SandboxPool, "nodes": []any{isoRange},
		"peers": []any{"10.200.200.30-10.200.200.49"}, "kubeletSources": []any{"10.200.200.30-10.200.200.49"}, "controlPlane": "10.200.200.20:6443",
		"bridge": "vmbr1", "deny": []any{"10.200.200.0/24"}}
	r.es.Config["network"].(map[string]any)["sandboxCidrs"] = []any{isoRange}
}

// synced leaves the provider-wide rules as a provider sync of some earlier day left them for the provider's config as
// it is now, on top of what the cluster firewall holds.
func (r *isolationRig) synced(t *testing.T) {
	t.Helper()
	n := r.es.PVENetworkConfig()
	if err := r.svc.WriteProviderIsolation(context.Background(), n.CIDRPool, n.BlockedCIDRs, n.SandboxCIDRs, r.es.PVESandboxEndpoints()...); err != nil {
		t.Fatal(err)
	}
}

// tenant gives the site one project with a network: its two ACCEPTs sit on top, as a network create leaves them.
func (r *isolationRig) tenant() {
	const c = "proxcloud:isolation:proj1"
	r.pve.clusterRules = append([]pve.FirewallRule{
		{Type: "forward", Action: "ACCEPT", Source: "10.100.1.0/24", Dest: "10.100.1.0/24", Enable: 1, Comment: c},
		{Type: "in", Action: "ACCEPT", Source: "10.100.1.0/24", Dest: "10.100.1.0/24", Enable: 1, Comment: c},
	}, r.pve.clusterRules...)
}

func (r *isolationRig) row(ctx context.Context, in *Installation) Check {
	mgmt, _, err := r.svc.C.ManagementCIDRs(ctx)
	if err != nil {
		return Check{ID: "isolation", Status: CheckFail, Message: err.Error()}
	}
	site := SiteOf(r.es)
	site.Mgmt = mgmt
	return Preflight{Installation: in, Site: site, Allow: r.es.PVESandboxEndpoints(), Portal: r.svc}.isolationRow(ctx, site, mgmt)
}

// check is the row as the Check reads it now.
func (r *isolationRig) check() Check {
	_, in, _ := r.state.snapshot()
	return r.row(context.Background(), in)
}

// rules are the cluster firewall's rules top down as lines: own the provider-wide set, project the tenants' ACCEPTs,
// and the rules of everyone else as they are.
func (r *isolationRig) rules() (own, project []string, foreign []pve.FirewallRule) {
	r.pve.mu.Lock()
	defer r.pve.mu.Unlock()
	for _, x := range r.pve.clusterRules {
		line := strings.Join(strings.Fields(x.Type+" "+x.Action+" "+x.Proto+" "+x.Source+" > "+x.Dest+" "+x.DPort), " ")
		switch {
		case x.Comment == isoOwner:
			if x.Enable != 1 {
				line += " (disabled)"
			}
			own = append(own, line)
		case strings.HasPrefix(x.Comment, isoOwner+":"):
			project = append(project, line)
		default:
			x.Pos, x.Digest = 0, ""
			foreign = append(foreign, x)
		}
	}
	return own, project, foreign
}

func (r *isolationRig) firewallWrites() []string {
	r.pve.mu.Lock()
	defer r.pve.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(r.pve.writes), func(w string) bool { return !strings.Contains(w, "/cluster/firewall/rules") })
}

// start makes the run a new one that begins at validate and then goes on at stage cluster (the stages between are
// the seed's, done).
func (r *isolationRig) start() {
	r.upTo(stageCluster)
	r.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		run.State, run.FinishedAt = RunRunning, nil
		after := false
		for i := range run.Stages {
			after = after || run.Stages[i].ID == stageCluster
			if after || run.Stages[i].ID == StageValidate {
				run.Stages[i] = Stage{ID: run.Stages[i].ID, State: StagePending}
			}
		}
	})
}

func (r *isolationRig) want(t *testing.T, what string, own []string, projects int) {
	t.Helper()
	got, project, foreign := r.rules()
	if !slices.Equal(got, own) {
		t.Fatalf("%s: the provider-wide rules\n%s\nwant\n%s", what, strings.Join(got, "\n"), strings.Join(own, "\n"))
	}
	if len(project) != projects {
		t.Fatalf("%s: %d project rules, want %d: %v", what, len(project), projects, project)
	}
	if !reflect.DeepEqual(foreign, r.foreign) {
		t.Fatalf("%s: the rules of other owners were changed or moved among themselves:\n%+v\nwant\n%+v", what, foreign, r.foreign)
	}
	if !slices.Equal(r.pve.fwGroups, []string{"sbx823effd548d9"}) || !slices.Equal(r.pve.fwSets, []string{"sbx823effd548d9-n", "sbx823effd548d9-o"}) {
		t.Fatalf("%s: another stack's firewall group or IP sets: %v %v", what, r.pve.fwGroups, r.pve.fwSets)
	}
}

// Apply, then a second apply, with no provider sync in between (Z4-A): the first one writes the range's rules itself
// in stage cluster, so the Check's row is ok at once and the second run passes its first stage. The second one finds
// the set in place and writes nothing to the cluster firewall.
func TestApplyTwiceWithNoSyncInBetween(t *testing.T) {
	for placement, want := range map[string][]string{SandboxShared: isoShared, SandboxSeparate: isoSeparate} {
		t.Run(placement, func(t *testing.T) {
			sb := sharedSandbox()
			if placement == SandboxSeparate {
				sb = separateSandbox()
				sb.System.Nodes = []string{"pve1"}
			}
			r := isolationPlatform(t, sb)
			if placement == SandboxSeparate {
				for _, name := range []string{"pc-sandbox-sandbox-pve1-x", "pc-sandbox-sandbox-system-pve1-x"} {
					m := meta(name, map[string]any{"cluster.x-k8s.io/cluster-name": SandboxClusterName})
					m["status"] = map[string]any{"phase": "Running"}
					r.wl.put("/apis/cluster.x-k8s.io/v1beta2/namespaces/pc-sandbox/machines/"+name, m)
				}
			}
			r.synced(t) // the provider's rules of before sandboxes: the four DROPs
			r.tenant()
			r.want(t, "before", isoBase, 2)
			if c := r.check(); c.Status != CheckOK {
				t.Fatalf("before: %+v", c)
			}

			r.start()
			if end := r.until(40); end != "complete" {
				t.Fatalf("first apply: %s: %+v %+v", end, r.stage(StageValidate), r.stage(stageCluster))
			}
			r.want(t, "after the first apply", want, 2)
			// The tenants' ACCEPTs stay above the DROPs, and the set is on top of everyone else's rules.
			if all := r.pve.clusterRules; !strings.HasPrefix(all[0].Comment, isoOwner+":") || !strings.HasPrefix(all[1].Comment, isoOwner+":") || all[2].Comment != isoOwner {
				t.Fatalf("order: %+v", all[:3])
			}
			if c := r.check(); c.Status != CheckOK || !strings.Contains(c.Message, "drops tenant traffic") {
				t.Fatalf("the Check right after the apply, with no provider sync: %+v", c)
			}

			before := len(r.firewallWrites())
			r.start()
			if end := r.until(40); end != "complete" {
				t.Fatalf("second apply with no sync in between: %s: %+v %+v", end, r.stage(StageValidate), r.stage(stageCluster))
			}
			if w := r.firewallWrites()[before:]; len(w) != 0 {
				t.Fatalf("a pass with nothing to change wrote to the cluster firewall: %v", w)
			}
			r.want(t, "after the second apply", want, 2)
		})
	}
}

// Off, then an apply, with no provider sync in between (Z3-A), on a site with a tenant project: the off takes the
// range's rules away in the step that takes the range off the provider, the Check is ok at once, and the next apply
// passes its first stage and puts them back.
func TestOffThenApplyWithNoSyncInBetween(t *testing.T) {
	r := isolationPlatform(t, &SandboxSpec{Placement: SandboxOff})
	r.mutate(func(_ *Run, in *Installation, _ *Secrets) {
		in.Status.Sandbox = &SandboxStatus{Placement: SandboxShared, NodeRange: isoRange, ProxyAddress: "10.200.200.210", Count: 1}
	})
	r.sharedBlock()
	r.synced(t)
	r.tenant()
	r.pve.pools = []string{pve.SandboxPool}
	r.want(t, "before", isoShared, 2)

	r.start()
	if end := r.until(40); end != "complete" {
		t.Fatalf("off: %s: %+v %+v", end, r.stage(StageValidate), r.stage(stageCluster))
	}
	r.want(t, "after the off", isoBase, 2)
	if c := r.check(); c.Status != CheckOK {
		t.Fatalf("the Check right after the off, with no provider sync: %+v", c)
	}
	if _, in, _ := r.state.snapshot(); in.Status.Sandbox != nil {
		t.Fatalf("the record stayed: %+v", in.Status.Sandbox)
	}

	r.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Spec.Sandbox = sharedSandbox() })
	r.sandboxMachine()
	r.sandboxIdentities(r.wl)
	r.start()
	if end := r.until(40); end != "complete" {
		t.Fatalf("the apply after the off, with no sync in between: %s: %+v %+v", end, r.stage(StageValidate), r.stage(stageCluster))
	}
	r.want(t, "after the apply", isoShared, 2)
	if c := r.check(); c.Status != CheckOK {
		t.Fatalf("the Check after the apply: %+v", c)
	}
}

// Off on a site with no project and no tenant network: no project's sync and no network create would ever rewrite the
// set, and the provider-level sync refuses to remove a DROP in place. The run leaves the row ok by itself.
func TestOffOnASiteWithNoProjectLeavesIsolationOK(t *testing.T) {
	r := isolationPlatform(t, &SandboxSpec{Placement: SandboxOff})
	r.mutate(func(_ *Run, in *Installation, _ *Secrets) {
		in.Status.Sandbox = &SandboxStatus{Placement: SandboxShared, NodeRange: isoRange, ProxyAddress: "10.200.200.210", Count: 1}
	})
	r.sharedBlock()
	r.synced(t)
	r.pve.pools = []string{pve.SandboxPool}
	r.want(t, "before", isoShared, 0)

	r.start()
	if end := r.until(40); end != "complete" {
		t.Fatalf("off: %s: %+v %+v", end, r.stage(StageValidate), r.stage(stageCluster))
	}
	r.want(t, "after the off", isoBase, 0)
	if c := r.check(); c.Status != CheckOK || !strings.Contains(c.Message, "drops tenant traffic") {
		t.Fatalf("the Check after an off on a site with no project: %+v", c)
	}
	// What the provider-level sync would have done with the rules the off found (rc.1 left them to it): refuse.
	r.sharedBlock()
	r.synced(t)
	delete(r.es.Config, "sandbox")
	delete(r.es.Config["network"].(map[string]any), "sandboxCidrs")
	n := r.es.PVENetworkConfig()
	if err := r.svc.ReconcileProviderIsolation(context.Background(), n.CIDRPool, n.BlockedCIDRs, n.SandboxCIDRs); err == nil || !strings.Contains(err.Error(), "removes no DROP in place") {
		t.Fatalf("the provider sync on the leftover rules: %v", err)
	}
}

// The first stage and the installation's own sandbox range. The rules in place are the ones of before the range (the
// run was cancelled between its two writes, or a release that left the rules to a provider sync built the range):
// the row is a warning that names the range, and the off passes its first stage and puts things right. Any other
// difference still stops the run there.
func TestValidateIsNotStoppedByTheOwnSandboxRange(t *testing.T) {
	built := func(r *isolationRig) {
		r.mutate(func(_ *Run, in *Installation, _ *Secrets) {
			in.Status.Sandbox = &SandboxStatus{Placement: SandboxShared, NodeRange: isoRange, ProxyAddress: "10.200.200.210", Count: 1}
		})
		r.synced(t) // before the block: the four DROPs
		r.tenant()
		r.sharedBlock()
		r.pve.pools = []string{pve.SandboxPool}
	}
	r := isolationPlatform(t, &SandboxSpec{Placement: SandboxOff})
	built(r)
	if c := r.check(); c.Status != CheckWarn || !strings.Contains(c.Message, isoRange) || !strings.Contains(c.Message, "The next apply writes them itself") || strings.Contains(c.Message, "by hand") {
		t.Fatalf("the row for rules that lack only the own range: %+v", c)
	}
	r.start()
	if end := r.until(40); end != "complete" {
		t.Fatalf("the off: %s: %+v %+v", end, r.stage(StageValidate), r.stage(stageCluster))
	}
	r.want(t, "after the off", isoBase, 2)
	if c := r.check(); c.Status != CheckOK {
		t.Fatalf("after the off: %+v", c)
	}

	// The mirror: the range is off the provider and its rules are still there (an off that was cancelled between its
	// two writes). The record still names the range, so it is the installation's own.
	r = isolationPlatform(t, &SandboxSpec{Placement: SandboxOff})
	r.mutate(func(_ *Run, in *Installation, _ *Secrets) {
		in.Status.Sandbox = &SandboxStatus{Placement: SandboxShared, NodeRange: isoRange, ProxyAddress: "10.200.200.210", Count: 1}
	})
	r.sharedBlock()
	r.synced(t)
	delete(r.es.Config, "sandbox")
	delete(r.es.Config["network"].(map[string]any), "sandboxCidrs")
	r.pve.pools = []string{pve.SandboxPool}
	if c := r.check(); c.Status != CheckWarn {
		t.Fatalf("the row for rules that hold only the own range too many: %+v", c)
	}
	r.start()
	if end := r.until(40); end != "complete" {
		t.Fatalf("the off again: %s: %+v", end, r.stage(StageValidate))
	}
	r.want(t, "after the off again", isoBase, 0)

	for name, breakIt := range map[string]func(r *isolationRig){
		"a rule of the set disabled by hand": func(r *isolationRig) {
			i := slices.IndexFunc(r.pve.clusterRules, func(x pve.FirewallRule) bool {
				return x.Comment == isoOwner && x.Type == "forward" && x.Dest == isoBlocked
			})
			r.pve.clusterRules[i].Enable = 0
		},
		"a blocked network the rules do not drop": func(r *isolationRig) {
			r.es.Config["network"].(map[string]any)["blockedCidrs"] = []any{"10.200.200.0/26", "10.77.0.0/24"}
		},
		"the datacenter firewall off": func(r *isolationRig) { r.pve.dcOff = true },
		"a sandbox range that is not this installation's": func(r *isolationRig) {
			r.es.Config["network"].(map[string]any)["sandboxCidrs"] = []any{isoRange, "10.200.201.0/28"}
		},
	} {
		r := isolationPlatform(t, &SandboxSpec{Placement: SandboxOff})
		built(r)
		breakIt(r)
		if c := r.check(); c.Status != CheckFail {
			t.Fatalf("%s: the row %+v", name, c)
		}
		before := len(r.firewallWrites())
		r.start()
		if end := r.until(5); end != "fail:"+CodeIsolationNotEnforced {
			t.Fatalf("%s: the off ended %s: %+v", name, end, r.stage(StageValidate))
		}
		if w := r.firewallWrites()[before:]; len(w) != 0 {
			t.Fatalf("%s: a run stopped at validate wrote to the cluster firewall: %v", name, w)
		}
	}
}

// The step waits, and records nothing, while the rules it wrote do not read back: here another network change holds
// the site's SDN lock, the lock a provider sync takes.
func TestSandboxIsolationWaitsForTheSDNLock(t *testing.T) {
	r := isolationPlatform(t, sharedSandbox())
	r.synced(t)
	held := true
	r.r.SDNLock = func(context.Context) (func(), bool, error) { return func() {}, !held, nil }
	r.start()
	r.until(3)
	if s := r.stage(stageCluster); s.Code != CodeIsolationNotEnforced || !strings.Contains(s.Message, "Another network change") {
		t.Fatalf("with the lock held: %+v", s)
	}
	r.want(t, "with the lock held", isoBase, 0)
	if r.seed.obj(appsPath+"pc-platform") != nil && slices.Contains(r.seed.applied, "pc-platform@"+ClusterChartVersion(in2bom(t))) {
		t.Fatalf("the cluster's Application was applied before the range's rules were in place: %v", r.seed.applied)
	}
	held = false
	if end := r.until(40); end != "complete" {
		t.Fatalf("with the lock free: %s: %+v", end, r.stage(stageCluster))
	}
	r.want(t, "after", isoShared, 0)
}

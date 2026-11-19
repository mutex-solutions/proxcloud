package platformlife

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// upgrading is a platform that release 0.8.1 installed (every Application synced at 0.8.1's charts, the record on
// 0.8.1's BOM) on a build whose BOM is release 0.9.0 with the product's own charts bumped, with the first run of the
// upgrade started as StartRun starts it. The applies of the installing run are forgotten: applied lists the upgrade's.
func upgrading(t *testing.T) (p *platform, from, to BOMRef) {
	t.Helper()
	old := nextRelease(t, ownCharts)
	cur, _ := Current()
	from, to = refOf(old), refOf(cur)
	p = newPlatform(t, RunApply)
	p.mutate(func(run *Run, in *Installation, _ *Secrets) { run.BOM, in.Status.BOM = from, &from })
	p.upTo(stageSeedPlatform)
	if end := p.until(30); end != "complete" {
		t.Fatalf("the installing run: %s", end)
	}
	p.start(t, to.SHA256, "upgrade lab to 0.9.0")
	return p, from, to
}

// start begins the installation's next apply as StartRun does, with every stage up to the seed's platform done.
func (p *platform) start(t *testing.T, target, confirm string) {
	t.Helper()
	var err error
	p.mutate(func(run *Run, in *Installation, _ *Secrets) {
		cur, _ := Current()
		*run = Run{ID: run.ID, InstallationID: in.ID, Kind: RunApply, State: RunQueued, BOM: refOf(cur)}
		for _, id := range stageIDs(RunApply, p.part) {
			run.Stages = append(run.Stages, Stage{ID: id, State: StagePending})
		}
		if err = startApply(run, in, target, confirm, p.part, SeedBackup{Taken: true}, RollRead{Read: true, DeleteFirst: p.first}); err == nil {
			in.Status.Phase = PhaseApplying
		}
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	p.jobs = &fakeJobs{}
	p.r.Jobs = p.jobs
	p.seed.mu.Lock()
	p.seed.applied = nil
	p.seed.mu.Unlock()
	p.upTo(stageSeedPlatform)
}

// receipt is the stage's receipt (empty when it has none).
func (p *platform) receipt(id string) StageReceipt {
	if r := p.stage(id).Receipt; r != nil {
		return *r
	}
	return StageReceipt{}
}

func (p *platform) appliedRevisions() []string {
	p.seed.mu.Lock()
	defer p.seed.mu.Unlock()
	return slices.Clone(p.seed.applied)
}

// version is the recorded version of a component.
func version(in *Installation, cluster, name string) string {
	for _, c := range in.Status.Components {
		if c.Cluster == cluster && c.Name == name {
			return c.Version
		}
	}
	return ""
}

// StartRun's rules for an apply with a target: the typed confirmation, the refusal with its reasons, and the record.
func TestStartApply(t *testing.T) {
	old := nextRelease(t, ownCharts)
	cur, _ := Current()
	ref := refOf(old)
	in, run := installed(&ref), &Run{}
	if err := startApply(run, in, "", "", "", SeedBackup{}, RollRead{}); err != nil || run.BOM != ref || run.FromBOM != nil || in.Status.TargetBOM != nil {
		t.Fatalf("an apply without a target stays on the installed BOM: %+v %v", run, err)
	}
	for _, confirm := range []string{"", "upgrade lab", "remove lab"} {
		if err := startApply(&Run{}, in, cur.SHA256, confirm, "", SeedBackup{}, RollRead{}); !errors.Is(err, ErrBadConfirm) || in.Status.TargetBOM != nil {
			t.Fatalf("confirm %q: %v", confirm, err)
		}
	}
	if err := startApply(&Run{}, in, "", "upgrade lab to 0.9.0", "", SeedBackup{}, RollRead{}); !errors.Is(err, ErrBadConfirm) {
		t.Fatalf("a confirmation without a target: %v", err)
	}
	// Lab round 2, finding R2-3: the installed BOM as the target is named as such, whatever the confirmation.
	for _, confirm := range []string{"", "upgrade lab to 0.9.0"} {
		if err := startApply(&Run{}, in, old.SHA256, confirm, "", SeedBackup{}, RollRead{}); !errors.Is(err, ErrSameBOM) || in.Status.TargetBOM != nil {
			t.Fatalf("the installed BOM as targetBom, confirm %q: %v", confirm, err)
		}
	}
	if err := startApply(run, in, cur.SHA256, "upgrade lab to 0.9.0", "", SeedBackup{}, RollRead{}); err != nil || run.BOM != refOf(cur) || run.FromBOM == nil || *run.FromBOM != ref ||
		in.Status.TargetBOM == nil || *in.Status.TargetBOM != refOf(cur) || *in.Status.BOM != ref {
		t.Fatalf("the upgrade's first run: %+v, status %+v, %v", run, in.Status, err)
	}
	// Retry: no target, no confirmation, the same target.
	retry := &Run{}
	if err := startApply(retry, in, "", "", "", SeedBackup{}, RollRead{}); err != nil || retry.BOM != refOf(cur) || *retry.FromBOM != ref {
		t.Fatalf("Retry: %+v %v", retry, err)
	}

	comp(t, cur.BOM, ClusterWorkload, "cilium").Chart.Version = "1.18.17"
	var refused *UpgradeRefused
	in = installed(&ref)
	if err := startApply(&Run{}, in, cur.SHA256, "upgrade lab to 0.9.0", "", SeedBackup{}, RollRead{}); !errors.As(err, &refused) || len(refused.Reasons) != 1 ||
		!strings.HasPrefix(err.Error(), CodeUpgradeRefused+": workload/cilium: "+later) || in.Status.TargetBOM != nil {
		t.Fatalf("a third-party chart next to the own ones: %v", err)
	}
}

// The upgrade on fakes: each own chart is applied at the target's version in the order of the stages and waves, every
// third-party chart stays where it is, a wave waits while ArgoCD's status is of the old source, the record of each
// component moves when its wave has passed, and status.bom moves only in the last stage.
func TestUpgradeMovesThePlatform(t *testing.T) {
	p, from, to := upgrading(t)
	cur, _ := Current()
	// The workload's wave 2 does not sync: ArgoCD still reports Synced and Healthy, of the 0.8.1 chart.
	p.seed.unsynced["pc-platform-capi-tenant"] = true
	if end := p.until(30); end != "defer:waiting_workload_platform" {
		t.Fatalf("end %s: %+v", end, p.stage(stageWorkloadPlatform))
	}
	_, in, _ := p.state.snapshot()
	if in.Status.BOM == nil || *in.Status.BOM != from || in.Status.TargetBOM == nil || *in.Status.TargetBOM != to {
		t.Fatalf("while the upgrade runs the record names the BOM it left and the target: %+v", in.Status)
	}
	if got := []string{version(in, ClusterSeed, "capi"), version(in, ClusterSeed, ClusterName), version(in, ClusterWorkload, "capi-tenant"), version(in, ClusterWorkload, "platform")}; !slices.Equal(got, []string{"0.1.3", "0.1.14", "0.1.2", "0.1.2"}) {
		t.Fatalf("components recorded as their waves pass: %v", got)
	}
	if app := p.seed.obj(appsPath + "pc-platform-platform"); str(app, "spec", "source", "targetRevision") != "0.1.2" {
		t.Fatalf("wave 3 was applied before wave 2 passed: %v", app["spec"])
	}
	if !p.receipt(stageCluster).RolloutChecked {
		t.Fatalf("the cluster chart was synced without the rollout check: %+v", p.stage(stageCluster))
	}
	delete(p.seed.unsynced, "pc-platform-capi-tenant")
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(stageWorkloadPlatform))
	}
	run, in, _ := p.state.snapshot()
	if run.State != RunSucceeded || in.Status.Phase != PhaseReady || *in.Status.BOM != to || in.Status.TargetBOM != nil || len(in.Status.Components) != len(cur.Components) {
		t.Fatalf("run %s, status %+v", run.State, in.Status)
	}
	for i, c := range cur.Components {
		if got := in.Status.Components[i]; got.Name != c.Name || got.Version != c.Chart.Version || got.Digest != c.Chart.Digest {
			t.Errorf("component %s recorded as %+v", c.Name, got)
		}
	}
	var own []string
	for _, a := range p.appliedRevisions() {
		name, rev, _ := strings.Cut(a, "@")
		switch name {
		case "capi", ClusterName, "pc-platform-capi-tenant", "pc-platform-platform":
			if !slices.Contains(own, a) {
				own = append(own, a)
			}
		default:
			if c := comp(t, cur.BOM, ClusterWorkload, strings.TrimPrefix(name, "pc-platform-")); rev != c.Chart.Version {
				t.Errorf("third-party %s applied at %s", name, rev)
			}
		}
	}
	if want := []string{"capi@0.1.3", "pc-platform@0.1.14", "pc-platform-capi-tenant@0.1.3", "pc-platform-platform@0.1.3"}; !slices.Equal(own, want) {
		t.Fatalf("own charts applied %v, want %v", own, want)
	}
	if len(p.registers) != 4 || str(p.registers[2], "config", "argocd", "chartVersion") != "0.1.14" {
		t.Fatalf("the Kubernetes provider's chart after the upgrade: %d registrations", len(p.registers))
	}
	if app := p.seed.obj(appsPath + "pc-platform-cilium"); str(app, "metadata", "labels", "proxcloud.io/bom-release") != "0.9.0" {
		t.Errorf("an unchanged component keeps the old release label: %v", app["metadata"])
	}
}

// An upgrade whose wave fails ends failed with the target kept, and Retry goes on to the target: no Application is
// written at the version of the BOM the platform left.
func TestUpgradeRetryGoesForward(t *testing.T) {
	p, from, to := upgrading(t)
	p.seed.unsynced["pc-platform-capi-tenant"] = true
	if end := p.until(30); end != "defer:waiting_workload_platform" {
		t.Fatalf("end %s", end)
	}
	p.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		for i := range run.Stages {
			if past := time.Now().Add(-time.Second); run.Stages[i].ID == stageWorkloadPlatform {
				run.Stages[i].Deadline = &past
			}
		}
	})
	if end := p.once(); end != "fail:"+CodeComponentUnhealthy {
		t.Fatalf("after the deadline: %s", end)
	}
	_, in, _ := p.state.snapshot()
	if in.Status.Phase != PhaseFailed || *in.Status.BOM != from || in.Status.TargetBOM == nil || *in.Status.TargetBOM != to || version(in, ClusterSeed, "capi") != "0.1.3" {
		t.Fatalf("after the failed run: %+v", in.Status)
	}
	delete(p.seed.unsynced, "pc-platform-capi-tenant")
	p.start(t, "", "") // Retry: no target, no confirmation
	if run, _, _ := p.state.snapshot(); run.BOM != to || run.FromBOM == nil || *run.FromBOM != from {
		t.Fatalf("Retry runs with %+v from %+v", run.BOM, run.FromBOM)
	}
	if end := p.until(30); end != "complete" {
		t.Fatalf("Retry: %s %+v", end, p.stage(stageWorkloadPlatform))
	}
	for _, a := range p.appliedRevisions() {
		if slices.Contains([]string{"capi@0.1.2", "pc-platform@0.1.13", "pc-platform-capi-tenant@0.1.2", "pc-platform-platform@0.1.2"}, a) {
			t.Fatalf("Retry wrote the old revision %s", a)
		}
	}
	if _, in, _ := p.state.snapshot(); *in.Status.BOM != to || in.Status.TargetBOM != nil {
		t.Fatalf("after Retry: %+v", in.Status)
	}
}

// An upgrade run that loses its lease at any write is resumed by the next acquisition and ends at the target.
func TestUpgradeResumesAfterALostLease(t *testing.T) {
	for n := 1; n <= 24; n += 3 {
		p, _, to := upgrading(t)
		p.jobs.loseAt = n
		for i := 0; i < 40 && !p.jobs.lost; i++ {
			p.once()
		}
		p.jobs = &fakeJobs{}
		p.r.Jobs = p.jobs
		if end := p.until(40); end != "complete" {
			t.Fatalf("lost at %d: %s", n, end)
		}
		if _, in, _ := p.state.snapshot(); *in.Status.BOM != to || in.Status.TargetBOM != nil || version(in, ClusterWorkload, "platform") != "0.1.3" {
			t.Fatalf("lost at %d: %+v", n, in.Status)
		}
		for _, a := range p.appliedRevisions() {
			if a == "pc-platform@0.1.13" || a == "capi@0.1.2" {
				t.Fatalf("lost at %d: the resumed run wrote %s", n, a)
			}
		}
	}
}

// The rollout check before the cluster chart's sync. A machine or bootstrap template ArgoCD would create, or a
// control plane it would change, fails the run with nothing of the cluster synced: the Application has the new source
// and no automated sync. A template ArgoCD would only prune is an old one and passes. Until ArgoCD has compared the
// new source the run waits. The switch turns the check off.
func TestUpgradeRefusesAMachineRollout(t *testing.T) {
	res := func(kind, name, status string, prune bool) map[string]any {
		return map[string]any{"kind": kind, "name": name, "status": status, "requiresPruning": prune}
	}
	kcp := res("KubeadmControlPlane", "pc-platform-cp", "Synced", false)
	for name, c := range map[string]struct {
		resources []any
		want      string
	}{
		"a new machine template":   {[]any{kcp, res("ProxmoxMachineTemplate", "pc-platform-cp-pve1-ffffffff", "OutOfSync", false)}, "ProxmoxMachineTemplate pc-platform-cp-pve1-ffffffff is new or would change"},
		"a new bootstrap template": {[]any{kcp, res("KubeadmConfigTemplate", "pc-platform-system-pve1-ffffffff", "OutOfSync", false)}, "KubeadmConfigTemplate pc-platform-system-pve1-ffffffff is new or would change"},
		"a changed control plane":  {[]any{res("KubeadmControlPlane", "pc-platform-cp", "OutOfSync", false)}, "KubeadmControlPlane pc-platform-cp would change"},
		"an old template to prune": {[]any{kcp, res("ProxmoxMachineTemplate", "pc-platform-cp-pve1-00000000", "OutOfSync", true), res("MachineDeployment", "pc-platform-system-pve1", "OutOfSync", false)}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			p, from, to := upgrading(t)
			p.seed.resources[ClusterName] = c.resources
			end := p.until(30)
			_, in, _ := p.state.snapshot()
			app := p.seed.obj(appsPath + ClusterName)
			if c.want == "" {
				if end != "complete" || *in.Status.BOM != to {
					t.Fatalf("end %s: %+v", end, p.stage(stageCluster))
				}
				return
			}
			if end != "fail:"+CodeUnexpectedRollout || !strings.Contains(p.stage(stageCluster).Message, c.want) {
				t.Fatalf("end %s: %+v", end, p.stage(stageCluster))
			}
			if str(app, "spec", "source", "targetRevision") != "0.1.14" || dig(app, "spec", "syncPolicy", "automated") != nil || dig(app, "spec", "syncPolicy", "retry") == nil {
				t.Fatalf("the Application after the refusal: %v", app["spec"])
			}
			if *in.Status.BOM != from || *in.Status.TargetBOM != to || version(in, ClusterSeed, ClusterName) != "0.1.13" || version(in, ClusterSeed, "capi") != "0.1.3" {
				t.Fatalf("the record after the refusal: %+v", in.Status)
			}
			// The same again at Retry; with the check switched off the run goes on.
			p.start(t, "", "")
			if end := p.until(30); end != "fail:"+CodeUnexpectedRollout {
				t.Fatalf("Retry: %s", end)
			}
			p.r.SkipRolloutGate = true
			p.start(t, "", "")
			if end := p.until(30); end != "complete" {
				t.Fatalf("with the check off: %s %+v", end, p.stage(stageCluster))
			}
		})
	}

	// ArgoCD has not compared the new source yet: its list is of the old chart and says nothing.
	p, _, _ := upgrading(t)
	p.seed.unsynced[ClusterName] = true
	if end := p.until(30); end != "defer:waiting_cluster" || !strings.Contains(p.stage(stageCluster).Message, "Waiting for ArgoCD to compare") {
		t.Fatalf("end %s: %+v", end, p.stage(stageCluster))
	}
	if app := p.seed.obj(appsPath + ClusterName); dig(app, "spec", "syncPolicy", "automated") != nil {
		t.Fatalf("the Application syncs by itself while the check waits: %v", app["spec"])
	}
	delete(p.seed.unsynced, ClusterName)
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s", end)
	}

	// A comparison of the new source that lists no control plane (empty, or not complete yet) says nothing either: the
	// run waits, and the Application stays without automated sync.
	p, _, _ = upgrading(t)
	p.seed.resources[ClusterName] = []any{}
	if end := p.until(30); end != "defer:waiting_cluster" || !strings.Contains(p.stage(stageCluster).Message, "Waiting for ArgoCD to compare") {
		t.Fatalf("an empty comparison: %s: %+v", end, p.stage(stageCluster))
	}
	if app := p.seed.obj(appsPath + ClusterName); dig(app, "spec", "syncPolicy", "automated") != nil {
		t.Fatalf("the Application syncs by itself after an empty comparison: %v", app["spec"])
	}

	// A run that stays on its BOM makes no such check.
	q := newPlatform(t, RunApply)
	q.seed.resources[ClusterName] = []any{res("KubeadmControlPlane", "pc-platform-cp", "OutOfSync", false)}
	q.upTo(stageSeedPlatform)
	if end := q.until(30); end != "complete" || q.receipt(stageCluster).RolloutChecked {
		t.Fatalf("an apply on its own BOM: %s", end)
	}
}

// Stage 7 of an upgrade waits for the sync of the new cluster chart before it records the component as moved. The
// Application never prunes, so it may stay OutOfSync for good: the last sync being of the current source is enough.
func TestUpgradeWaitsForTheClusterChartSync(t *testing.T) {
	p, _, to := upgrading(t)
	p.seed.resources[ClusterName] = []any{map[string]any{"kind": "KubeadmControlPlane", "name": "pc-platform-cp", "status": "Synced"},
		map[string]any{"kind": "MachineDeployment", "name": "pc-platform-system-pve1", "status": "OutOfSync"}}
	p.seed.pruneOnly = map[string]string{ClusterName: "0.1.13"} // OutOfSync, and the last sync was of the old chart
	if end := p.until(30); end != "defer:waiting_cluster" || !strings.Contains(p.stage(stageCluster).Message, "Waiting for ArgoCD to sync pc-platform at chart proxmox-kamaji-cluster 0.1.14") {
		t.Fatalf("end %s: %+v", end, p.stage(stageCluster))
	}
	if _, in, _ := p.state.snapshot(); version(in, ClusterSeed, ClusterName) != "0.1.13" {
		t.Fatalf("the cluster chart is recorded as moved before its sync")
	}
	p.seed.pruneOnly[ClusterName] = "current"
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(stageCluster))
	}
	if _, in, _ := p.state.snapshot(); *in.Status.BOM != to {
		t.Fatalf("status %+v", in.Status)
	}
}

// A site that once changed a machine size keeps the old machine templates: pc-platform never prunes, so it is
// OutOfSync for them alone. A cluster chart that renders every object unchanged then gives ArgoCD nothing to sync, and
// automated sync without prune starts none: the last sync stays the old chart's for good. Stage 7 must not wait for a
// sync that never comes. With anything else out of sync it does wait (TestUpgradeWaitsForTheClusterChartSync).
func TestUpgradePassesWhenOnlyPruningIsLeft(t *testing.T) {
	res := func(kind, name, status string, prune bool) map[string]any {
		return map[string]any{"kind": kind, "name": name, "status": status, "requiresPruning": prune}
	}
	left := []any{res("KubeadmControlPlane", "pc-platform-cp", "Synced", false), res("Cluster", ClusterName, "Synced", false),
		res("ProxmoxMachineTemplate", "pc-platform-cp-pve1-00000000", "OutOfSync", true)}
	p, _, to := upgrading(t)
	p.seed.resources[ClusterName] = left
	p.seed.pruneOnly = map[string]string{ClusterName: "0.1.13"} // OutOfSync, and no sync of the new chart ever starts
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(stageCluster))
	}
	if _, in, _ := p.state.snapshot(); *in.Status.BOM != to || version(in, ClusterSeed, ClusterName) != "0.1.14" || slices.Contains(p.seed.syncs, ClusterName) {
		t.Fatalf("status %+v, syncs started %v", in.Status, p.seed.syncs)
	}
}

// Between the workload's waves an upgrade waits for the tenants that were healthy before: a tenant that was down
// already does not hold it, one that goes down after a wave does, until it is healthy again.
func TestUpgradeWaitsForTenants(t *testing.T) {
	p, _, _ := upgrading(t)
	tenant := func(name, health string) {
		app := meta(name, map[string]any{"app.kubernetes.io/managed-by": "proxcloud"})
		app["status"] = map[string]any{"health": map[string]any{"status": health}}
		p.wl.put(appsPath+name, app)
	}
	tcp := func(status string) {
		o := map[string]any{"metadata": map[string]any{"name": "k1", "namespace": "t-1"}, "status": map[string]any{"kubernetesResources": map[string]any{"version": map[string]any{"status": status}}}}
		p.wl.put("/apis/kamaji.clastix.io/v1alpha1/tenantcontrolplanes/k1", o)
	}
	tenant("db-broken", "Degraded")
	tenant("db-1", "Healthy")
	tcp("Ready")
	p.seed.unsynced["pc-platform-capi-tenant"] = true // holds the run after the workload's wave 1 and 2
	if end := p.until(30); end != "defer:waiting_workload_platform" || p.stage(stageWorkloadPlatform).Code != CodeComponentUnhealthy {
		t.Fatalf("end %s: %+v", end, p.stage(stageWorkloadPlatform))
	}
	if _, in, _ := p.state.snapshot(); in.Status.TenantsDownBefore == nil || !slices.Equal(*in.Status.TenantsDownBefore, []string{"Application db-broken (Degraded)"}) {
		t.Fatalf("the tenants down before the waves: %+v", in.Status)
	}
	tenant("db-1", "Degraded")
	tcp("NotReady")
	if end := p.once(); end != "defer:waiting_workload_platform" || p.stage(stageWorkloadPlatform).Code != CodeTenantsUnhealthy ||
		!strings.Contains(p.stage(stageWorkloadPlatform).Message, "2 of 3 tenant objects") || !strings.Contains(p.stage(stageWorkloadPlatform).Message, "TenantControlPlane t-1/k1 (NotReady)") {
		t.Fatalf("end %s: %+v", end, p.stage(stageWorkloadPlatform))
	}
	// The wait runs out and the run fails. Retry still waits for the tenant the wave broke: the note of who was down
	// before is the upgrade's, and is not taken again with db-1 already Degraded.
	delete(p.seed.unsynced, "pc-platform-capi-tenant")
	p.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		for i := range run.Stages {
			if past := time.Now().Add(-time.Second); run.Stages[i].ID == stageWorkloadPlatform {
				run.Stages[i].Deadline = &past
			}
		}
	})
	if end := p.once(); end != "fail:"+CodeTenantsUnhealthy {
		t.Fatalf("after the deadline: %s", end)
	}
	p.start(t, "", "")
	if end := p.until(30); end != "defer:waiting_workload_platform" || p.stage(stageWorkloadPlatform).Code != CodeTenantsUnhealthy {
		t.Fatalf("Retry with the tenant still down: %s: %+v", end, p.stage(stageWorkloadPlatform))
	}
	if _, in, _ := p.state.snapshot(); in.Status.BOM == nil || in.Status.TargetBOM == nil || !slices.Equal(*in.Status.TenantsDownBefore, []string{"Application db-broken (Degraded)"}) {
		t.Fatalf("Retry took the note again: %+v", in.Status)
	}
	tenant("db-1", "Healthy")
	tcp("Ready")
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(stageWorkloadPlatform))
	}
	if _, in, _ := p.state.snapshot(); in.Status.TargetBOM != nil || in.Status.TenantsDownBefore != nil {
		t.Fatalf("the finished upgrade keeps its note: %+v", in.Status)
	}
}

package platformlife

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
)

// capiNode is a Node as Cluster API leaves it in the platform cluster: its annotations name the cluster and the
// Machine it was joined for.
func capiNode(name, machine, ready string) map[string]any {
	n := meta(name, map[string]any{})
	n["metadata"].(map[string]any)["uid"] = "uid-of-" + name
	n["metadata"].(map[string]any)["annotations"] = map[string]any{nodeClusterAnnotation: ClusterName, nodeMachineAnnotation: machine}
	n["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": ready}}}
	return n
}

func (p *platform) deletes() []string {
	p.wl.mu.Lock()
	defer p.wl.mu.Unlock()
	var out []string
	for _, w := range p.wl.writes {
		if strings.HasPrefix(w, "DELETE ") {
			out = append(out, w)
		}
	}
	return out
}

// The drill of 0.10.1-rc.1, on a platform with one control plane machine: Cluster API replaced the machine and left
// the old one's Node object (its 10 seconds for the delete ran out while the address moved), so five Nodes stood
// against four machines and every run ended in nodes_timeout. A site that stands there gets out with one Retry: the
// run deletes the Node no Machine owns, names it, and goes on to the next unit.
func TestRollDeletesTheNodeOfAReplacedControlPlaneMachine(t *testing.T) {
	p, _ := rolling(t)
	var audited []string
	p.r.OnNodeDeleted = func(in *Installation, node, why, actor string) {
		audited = append(audited, in.ID+" "+node+" "+actor+" "+why)
	}
	p.once()
	p.machine("pc-platform-cp", "1.35.11")
	const ghost = "/api/v1/nodes/pc-platform-cp-lhcn7"
	p.wl.put(ghost, capiNode("pc-platform-cp-lhcn7", "pc-platform-cp-lhcn7", "Unknown"))
	p.mutate(func(run *Run, _ *Installation, _ *Secrets) { run.CancelRequested = true }) // the lab's run was cancelled in this wait
	p.once()
	if run, in, _ := p.state.snapshot(); run.State != RunCancelled || rollOf(in).Step != 1 {
		t.Fatalf("the stuck site: %s, %+v", run.State, rollOf(in))
	}
	p.begin(t, "", "") // the one Retry
	end, st := p.once(), p.stage(stageCluster)
	if end != "defer:waiting_cluster" || p.wl.obj(ghost) != nil || !slices.Equal(p.deletes(), []string{"DELETE " + ghost}) || st.Code != CodeNodesTimeout ||
		!strings.Contains(st.Message, "pc-platform-cp-lhcn7") || !slices.ContainsFunc(st.Items, func(i StageItem) bool {
		return i.ID == "node/pc-platform-cp-lhcn7" && i.State == "deleted" && strings.Contains(i.Message, "no Machine owned it")
	}) {
		t.Fatalf("the look that finds the Node: %s, deletes %v, stage %+v", end, p.deletes(), st)
	}
	if end := p.once(); end != "defer:waiting_cluster" || p.rollStep() != 2 {
		t.Fatalf("after the Node is gone: %s, step %d: %+v", end, p.rollStep(), p.stage(stageCluster))
	}
	for _, owner := range []string{"pc-platform-system-pve1", "pc-platform-kamaji-pve2", "pc-platform-dbaas-pve3"} {
		p.machine(owner, "1.35.11")
	}
	// The item stays on the stage through every later write of its items (the drill's run had lost it at the next
	// look), and the deletion has its audit entry, by the run.
	named := func() bool {
		return slices.ContainsFunc(p.stage(stageCluster).Items, func(i StageItem) bool { return i.ID == "node/pc-platform-cp-lhcn7" && i.State == "deleted" })
	}
	if p.once(); !named() {
		t.Fatalf("after the next look: %+v", p.stage(stageCluster))
	}
	if end := p.until(30); end != "complete" || len(p.deletes()) != 1 || !named() {
		t.Fatalf("end %s, deletes %v: %+v", end, p.deletes(), p.stage(stageCluster))
	}
	run, in, _ := p.state.snapshot()
	if len(audited) != 1 || !strings.HasPrefix(audited[0], in.ID+" pc-platform-cp-lhcn7 "+templateupdates.PlatformRunActor+run.ID+" Node pc-platform-cp-lhcn7 was not Ready") {
		t.Fatalf("audit entries: %q", audited)
	}
}

// A Node that is not Ready and belongs to a live Machine is waited for, named, and never deleted: by the Machine its
// annotation names, by a Machine's nodeRef, by a Machine's name (it is still joining), or by the provider id of a VM.
// A Node replaced under the same name since it was read is left alone as well (the delete carries the uid).
func TestNodesKeepsANodeAMachineOwns(t *testing.T) {
	p, _ := rolling(t)
	p.once()
	p.machine("pc-platform-cp", "1.35.11")
	const node = "/api/v1/nodes/pc-platform-dbaas-pve3"
	p.wl.put(node, capiNode("pc-platform-dbaas-pve3", "pc-platform-dbaas-pve3-x", "False"))
	end, st := p.once(), p.stage(stageCluster)
	if end != "defer:waiting_cluster" || p.rollStep() != 1 || p.wl.obj(node) == nil || len(p.deletes()) != 0 || st.Code != CodeNodesTimeout ||
		!strings.Contains(st.Message, "pc-platform-dbaas-pve3 (Ready False)") {
		t.Fatalf("a node of a live machine: %s, step %d, deletes %v, stage %+v", end, p.rollStep(), p.deletes(), st)
	}

	machine := func(name, nodeRef, providerID string) map[string]any {
		m := meta(name, nil)
		m["spec"], m["status"] = map[string]any{"providerID": providerID}, map[string]any{"nodeRef": map[string]any{"name": nodeRef}}
		return m
	}
	gone := capiNode("n1", "m-gone", "Unknown")
	withID := capiNode("n1", "m-gone", "Unknown")
	withID["spec"] = map[string]any{"providerID": "proxmox://aaaa"}
	foreign := capiNode("n1", "m-gone", "Unknown")
	foreign["metadata"].(map[string]any)["annotations"].(map[string]any)[nodeClusterAnnotation] = "pc-c-other"
	for name, c := range map[string]struct {
		node     map[string]any
		machines []map[string]any
		infra    []map[string]any
		orphan   bool
	}{
		"its machine is gone":                 {gone, []map[string]any{machine("m-live", "n2", "")}, nil, true},
		"its machine lives":                   {capiNode("n1", "m-live", "False"), []map[string]any{machine("m-live", "", "")}, nil, false},
		"a machine's nodeRef names it":        {gone, []map[string]any{machine("m-live", "n1", "")}, nil, false},
		"a machine of its name is joining":    {gone, []map[string]any{machine("n1", "", "")}, nil, false},
		"a machine has its provider id":       {withID, []map[string]any{machine("m-live", "", "proxmox://aaaa")}, nil, false},
		"a ProxmoxMachine has its id":         {withID, []map[string]any{machine("m-live", "", "")}, []map[string]any{{"spec": map[string]any{"providerID": "proxmox://aaaa"}}}, false},
		"it is Ready":                         {capiNode("n1", "m-gone", "True"), []map[string]any{machine("m-live", "", "")}, nil, false},
		"no machine could be read":            {gone, nil, nil, false},
		"Cluster API did not join it":         {func() map[string]any { n := meta("n1", nil); n["status"] = gone["status"]; return n }(), []map[string]any{machine("m-live", "", "")}, nil, false},
		"it is another cluster's by its mark": {foreign, []map[string]any{machine("m-live", "", "")}, nil, false},
	} {
		if got := len(orphanNodes([]map[string]any{c.node}, c.machines, c.infra)) == 1; got != c.orphan {
			t.Errorf("%s: orphan %t, want %t", name, got, c.orphan)
		}
	}

	run, in, sec := p.state.snapshot()
	x := &acq{run: run, in: in, sec: sec}
	seed, _ := x.seed()
	wl, err := x.workload(context.Background(), seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := wl.DeleteNode(context.Background(), "pc-platform-dbaas-pve3", "uid-of-an-earlier-node"); err == nil || p.wl.obj(node) == nil {
		t.Fatalf("a delete with another uid: %v", err)
	}
}

// The Check row nodes: ok while every node is Ready; a warning that names a node that is not, and says of one no
// Machine owns what the next apply does. Never a failure, or the run that repairs it could not start.
func TestNodesCheck(t *testing.T) {
	p, _ := rolling(t)
	run, in, sec := p.state.snapshot()
	x := &acq{run: run, in: in, sec: sec}
	seed, _ := x.seed()
	wl, err := x.workload(context.Background(), seed)
	if err != nil {
		t.Fatal(err)
	}
	if c := NodesCheck(context.Background(), seed, wl); c == nil || c.Status != CheckOK || c.Message != "The platform cluster's 4 nodes are Ready." {
		t.Fatalf("every node Ready: %+v", c)
	}
	p.wl.put("/api/v1/nodes/pc-platform-cp-lhcn7", capiNode("pc-platform-cp-lhcn7", "pc-platform-cp-lhcn7", "Unknown"))
	p.wl.put("/api/v1/nodes/pc-platform-dbaas-pve3", capiNode("pc-platform-dbaas-pve3", "pc-platform-dbaas-pve3-x", "False"))
	c := NodesCheck(context.Background(), seed, wl)
	if c == nil || c.Status != CheckWarn || !strings.Contains(c.Message, "Node pc-platform-cp-lhcn7 is not Ready (Unknown) and no Machine owns it") ||
		!strings.Contains(c.Message, "The next Apply deletes the Node object") || !strings.Contains(c.Message, "Node pc-platform-dbaas-pve3 is not Ready (False).") {
		t.Fatalf("a leftover node and a node of a live machine: %+v", c)
	}
	if NodesCheck(context.Background(), nil, wl) != nil || NodesCheck(context.Background(), seed, nil) != nil {
		t.Fatal("a row without a cluster to read")
	}
}

// laterBuild makes the build a later one than the release that started the roll: the embedded 0.8.0 BOM stands for the
// unfinished target (the BOM this build had until now), and this build's BOM gets edit on top of it.
func laterBuild(t *testing.T, edit func(*BOM)) (mid Embedded) {
	t.Helper()
	cur, _ := Current()
	mid, _ = Lookup(previousSHA256["0.8.0"])
	saved := *mid.BOM
	t.Cleanup(func() { *mid.BOM = saved })
	*mid.BOM = *cur.BOM
	mid.Release, mid.Components = "0.9.0-rc.1", slices.Clone(cur.Components)
	cur.Components = slices.Clone(cur.Components)
	edit(cur.BOM)
	return mid
}

// correctedChart is what a release that corrects the cluster chart changes: the chart, which lists the versions of
// the installed BOM and of the unfinished target, and nothing a machine is made of.
func correctedChart(b *BOM) {
	for i := range b.Components {
		if c := &b.Components[i]; c.Chart.Name == "proxmox-kamaji-cluster" {
			c.Chart.Version, c.Chart.Digest, c.UpgradeFrom = "0.1.16", "sha256:"+strings.Repeat("f", 64), []string{"0.1.13", "0.1.15"}
		}
	}
	b.TenantCharts.Kubernetes.Version, b.TenantCharts.Kubernetes.Digest = "0.1.16", "sha256:"+strings.Repeat("f", 64)
}

// The rule for a roll that stands unfinished when a later build arrives. Its BOM names the same Kubernetes version,
// node image and k3s (own charts and the release differ): an apply without a target continues under it, nothing to
// confirm, and the plan shows one upgrade in progress. Before the roll has begun and after its last unit nothing is
// carried. A later BOM that replaces machines again is not taken: Retry goes on to the unfinished target, and naming
// the build's BOM is refused with the reason.
func TestMoveCarriesARollUnderALaterBOM(t *testing.T) {
	old := nextRelease(t, newerK8s)
	cur, _ := Current()
	mid := laterBuild(t, correctedChart)
	from, midRef, target := refOf(old), refOf(mid), refOf(cur)
	_, site, _ := newPlatform(t, RunApply).state.snapshot()
	rollingAt := func(step int) *Installation {
		in := installed(&from)
		in.Spec = site.Spec
		in.Spec.NodeImage.Mode = ImageBOM
		in.Status.TargetBOM = &midRef
		in.Status.Clusters = []ClusterStatus{{Name: ClusterName}}
		if step > 0 {
			in.Status.Clusters[0].Roll = &RollStatus{Step: step}
		}
		return in
	}
	units := len(rollUnits(rollingAt(1).Spec.Clusters[0]))
	for _, named := range []string{"", cur.SHA256} {
		in := rollingAt(1)
		to, up, err := move(in, named)
		if err != nil || to.SHA256 != cur.SHA256 || !up.Allowed || !up.InProgress || up.Confirm != "" || up.Carried == nil || *up.Carried != midRef || up.To != target {
			t.Fatalf("target %q in a roll: %+v %v", named, up, err)
		}
	}
	in := rollingAt(1)
	if p, err := PlanFor(in, ""); err != nil || p.Target != target || p.Upgrade == nil || p.Upgrade.Carried == nil || p.Replacement != nil || len(p.Blocking) != 0 {
		t.Fatalf("the plan in a roll under a later build: %+v %v", p, err)
	}
	run := &Run{}
	if err := startApply(run, in, "", "", "", SeedBackup{}, RollRead{}); err != nil || run.BOM != target || *in.Status.TargetBOM != target || *run.FromBOM != from || rollOf(in).Step != 1 {
		t.Fatalf("the Retry: %v, run %+v, target %+v, roll %+v", err, run, in.Status.TargetBOM, rollOf(in))
	}
	for _, step := range []int{0, units + 1} { // not begun; every unit replaced, the record left to write
		to, up, err := move(rollingAt(step), "")
		if err != nil || to.SHA256 != mid.SHA256 || !up.InProgress || up.Carried != nil {
			t.Fatalf("step %d: an apply without a target goes on to the unfinished target: to %s, %+v %v", step, to.Release, up, err)
		}
	}

	// A later build that names another Kubernetes version again.
	cur.Kubernetes.Version = "1.35.12"
	cur.Kubernetes.NodeImage.Tag, cur.Kubernetes.NodeImage.Digest = strings.Replace(newTag, "1.35.11", "1.35.12", 1), "sha256:"+strings.Repeat("a", 64)
	if to, up, err := move(rollingAt(2), ""); err != nil || to.SHA256 != mid.SHA256 || !up.Allowed || !up.InProgress || up.Carried != nil {
		t.Fatalf("Retry under a build that replaces machines again: to %s, %+v %v", to.Release, up, err)
	}
	_, up, err := move(rollingAt(2), cur.SHA256)
	if err != nil || up.Allowed || up.InProgress || up.Confirm != "" || !slices.ContainsFunc(up.Blocking, func(b string) bool {
		return strings.Contains(b, "has begun (step 2)") && strings.Contains(b, "kubernetes.version") && strings.Contains(b, "Finish the upgrade to 0.9.0-rc.1 first")
	}) {
		t.Fatalf("a later BOM that replaces machines again, named: %+v %v", up, err)
	}
}

// The same with the files this build carries: an installation on 0.10.0 that stands after the first unit of the
// replacement toward a kept BOM with this build's Kubernetes pins (0.10.1-rc.1, the lab's state after the first
// drill, 0.10.1-rc.2 after a Retry on that candidate, or 0.10.1 after a Retry on the release). A Retry is never
// refused. Under a release BOM that moves no third-party chart against 0.10.0 the kept one is carried to it; under the
// development BOM of main, which carries nothing, and under a release BOM that moves cert-manager against 0.10.0 (the
// move from 0.10.0 also replaces machines, so the pair gate refuses cert-manager's rows), the Retry goes on to the kept
// one itself, which it can only do while its file is kept. 0.10.2-rc.1 has these pins too and is taken the same way,
// although no build records it as the target of such a replacement: the move from 0.10.0 to it is refused by the pair
// gate. Under 0.10.2 it is not carried either, although the two BOMs differ in the release line alone: the move from
// 0.10.0 to 0.10.2 is refused by the same gate. 0.10.2 itself has these pins too and is taken the same way, for the
// same reason, and so are 0.11.0-rc.1, 0.11.0-rc.2, 0.11.0 and 0.11.1-rc.1.
func TestRollTowardAKeptCandidateGoesOn(t *testing.T) {
	cur, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	prev, err := Previous()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(prev, func(b Embedded) bool { return b.Release == "0.10.0" })
	if i < 0 {
		t.Fatal("no previous BOM 0.10.0")
	}
	from, seen := refOf(prev[i]), 0
	dev := strings.HasSuffix(cur.Release, "-dev")
	_, site, _ := newPlatform(t, RunApply).state.snapshot()
	for _, mid := range prev {
		if mid.Kubernetes != cur.Kubernetes || mid.Seed.K3s.Version != cur.Seed.K3s.Version {
			continue
		}
		seen++
		midRef := refOf(mid)
		in := installed(&from)
		in.Spec = site.Spec
		in.Spec.NodeImage.Mode = ImageBOM
		in.Status.TargetBOM = &midRef
		in.Status.Clusters = []ClusterStatus{{Name: ClusterName, Roll: &RollStatus{Step: 1}}}
		to, up, err := move(in, "")
		if err != nil || !up.Allowed || !up.InProgress || up.Confirm != "" || len(up.Blocking) != 0 {
			t.Fatalf("0.10.0 in a roll toward %s: to %s, allowed %v, in progress %v, blocking %v, %v", mid.Release, to.Release, up != nil && up.Allowed, up != nil && up.InProgress, up, err)
		}
		itself := dev || slices.ContainsFunc(Classify(prev[i].BOM, cur.BOM), func(c PlanChange) bool { return c.Name == "cert-manager" && c.Blocking != "" })
		if itself && (to.SHA256 != mid.SHA256 || up.Carried != nil) {
			t.Errorf("0.10.0 in a roll toward %s under %s: to %s, carried %v, want %s itself", mid.Release, cur.Release, to.Release, up.Carried, mid.Release)
		}
		if !itself && (to.SHA256 != cur.SHA256 || up.Carried == nil || *up.Carried != midRef) {
			t.Errorf("0.10.0 in a roll toward %s under %s: to %s, carried %v, want carried to this build's", mid.Release, cur.Release, to.Release, up.Carried)
		}
	}
	if cur.Kubernetes.Version == "1.36.6" && seen < 3 {
		t.Errorf("%d kept BOMs with this build's Kubernetes pins, want 0.10.1-rc.1, 0.10.1-rc.2 and 0.10.1", seen)
	}
}

// The same on a run: the roll stands after its first unit when the corrected build arrives. The Retry keeps the step,
// renders the units it has not reached with the new chart and the unit it has passed as it is, lets ArgoCD compare
// without the node deletion timeout the live Application does not have yet, applies with it, and ends on this
// build's BOM.
func TestRollContinuesUnderALaterBOM(t *testing.T) {
	p, _ := rolling(t)
	p.once()
	p.machine("pc-platform-cp", "1.35.11")
	if end := p.once(); end != "defer:waiting_cluster" || p.rollStep() != 2 {
		t.Fatalf("after the control plane: %s, step %d", end, p.rollStep())
	}
	p.mutate(func(run *Run, _ *Installation, _ *Secrets) { run.CancelRequested = true })
	p.once()
	cur, _ := Current()
	midRef := refOf(laterBuild(t, correctedChart))
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Status.TargetBOM = &midRef })
	p.begin(t, "", "")
	if end := p.once(); end != "defer:waiting_cluster" || p.rollStep() != 2 || !strings.HasPrefix(p.rendered(), "cp 1.35.11 pve1=9201; system 1.35.11 pve1=9201; kamaji 1.35.10") {
		t.Fatalf("the Retry under the later build: %s, step %d, rendered %s: %+v", end, p.rollStep(), p.rendered(), p.stage(stageCluster))
	}
	app := p.seed.obj(appsPath + ClusterName)
	if str(app, "spec", "source", "targetRevision") != "0.1.16" || dig(app, "spec", "syncPolicy", "automated") == nil ||
		dig(app, "spec", "source", "helm", "valuesObject", "nodeDeletionTimeoutSeconds") != nil {
		t.Fatalf("the Application after the step: %s", jsonOf(dig(app, "spec")))
	}
	if _, in, _ := p.state.snapshot(); in.Status.TargetBOM.SHA256 != cur.SHA256 {
		t.Fatalf("the target: %+v", in.Status.TargetBOM)
	}
	for _, owner := range []string{"pc-platform-system-pve1", "pc-platform-kamaji-pve2", "pc-platform-dbaas-pve3"} {
		p.machine(owner, "1.35.11")
	}
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(stageCluster))
	}
	if _, in, _ := p.state.snapshot(); in.Status.BOM.SHA256 != cur.SHA256 || in.Status.TargetBOM != nil || rollOf(in) != nil && rollOf(in).Step <= 4 && in.Status.Clusters[0].KubernetesVersion != "1.35.11" {
		t.Fatalf("after the roll: %+v, roll %+v", in.Status, rollOf(in))
	}
}

// What ArgoCD compares before a sync renders the node deletion timeout as the live Application has it: none while
// live is of a chart before 0.1.16 or sets none, the chart's own once live carries it. Compared otherwise, the
// control plane would read OutOfSync for the timeout alone and every upgrade would end in unexpected_rollout.
func TestGateRenderMirrorsTheLiveTimeout(t *testing.T) {
	app := func(version string, values map[string]any) map[string]any {
		return map[string]any{"spec": map[string]any{"syncPolicy": map[string]any{"automated": map[string]any{}},
			"source": map[string]any{"targetRevision": version, "helm": map[string]any{"valuesObject": values}}}}
	}
	of := func(version string) appOf {
		return appOf{c: Component{Chart: Chart{Version: version}}, obj: app(version, map[string]any{"kubernetesVersion": "1.36.6"})}
	}
	for name, c := range map[string]struct {
		a     appOf
		live  map[string]any
		unset bool
	}{
		"live before the chart with the timeout": {of("0.1.16"), app("0.1.15", map[string]any{}), true},
		"live left without the timeout":          {of("0.1.16"), app("0.1.16", map[string]any{"nodeDeletionTimeoutSeconds": float64(-1)}), true},
		"live carries the timeout":               {of("0.1.16"), app("0.1.16", map[string]any{}), false},
		"a later chart over one that carries it": {of("0.1.17"), app("0.1.16", map[string]any{}), false},
		"the target chart does not read it":      {of("0.1.15"), app("0.1.14", map[string]any{}), false},
	} {
		held, err := gateRender(c.a, c.live)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		v := dig(held, "spec", "source", "helm", "valuesObject", "nodeDeletionTimeoutSeconds")
		if (v != nil) != c.unset || (v != nil && jsonOf(v) != "-1") || dig(held, "spec", "syncPolicy", "automated") != nil || dig(c.a.obj, "spec", "syncPolicy", "automated") == nil ||
			dig(c.a.obj, "spec", "source", "helm", "valuesObject", "nodeDeletionTimeoutSeconds") != nil {
			t.Errorf("%s: held %s, the run's own %s", name, jsonOf(held), jsonOf(c.a.obj))
		}
	}
}

// An import that Proxmox answers with an error is tried again by its job: the stage's item says which try runs and
// what the one before ended with, where it used to show the step alone for as long as the tries took.
func TestImagesShowsTheImportTries(t *testing.T) {
	x := &acq{r: &Runner{Imports: &fakeImports{tries: map[string]int{"r1": 3}, lastCode: map[string]string{"r1": "retryable"},
		recs: []templateupdates.Receipt{{ID: "r1", Step: templateupdates.StepUploading, Payload: templateupdates.VMPayload{ServiceID: "svc", Node: "pve3", Digest: "sha256:d"}}}}},
		in: &Installation{ServiceID: "svc"}}
	note, ours := "", []string{}
	state, vmid, err := x.image(context.Background(), "pve3", VMImage{Digest: "sha256:d"}, nil, "actor", &ours, &note)
	if err != nil || state != string(templateupdates.StepUploading) || vmid != 0 || !strings.Contains(note, "Try 3 of the import") || !strings.Contains(note, "retryable") {
		t.Fatalf("%s %d %v: %q", state, vmid, err, note)
	}
}

package kamaji

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Argo holds the Application at Progressing for minutes after CAPI says Available, because the
// ClusterResourceSet (MetalLB address pool) waits out a retry backoff. The sync reads READY only
// while that CRS is the one part not done, and only within resourceSetGrace of the create.
func TestClusterReadyPastResourceSet(t *testing.T) {
	cond := func(typ, status string) map[string]any {
		return map[string]any{"status": map[string]any{"conditions": []any{map[string]any{"type": typ, "status": status}}}}
	}
	named := func(name string, obj map[string]any) map[string]any {
		obj["metadata"] = map[string]any{"name": name, "generation": float64(1)}
		return obj
	}
	type world struct {
		age     time.Duration
		sync    string
		ready   float64 // ready workers of 2
		hcp     string  // Ready condition of the MetalLB HelmChartProxy
		crs     string  // ResourcesApplied condition of the CRS
		readErr error
	}
	base := world{age: 10 * time.Minute, sync: "Synced", ready: 2, hcp: "True", crs: "False"}
	for _, tc := range []struct {
		name string
		edit func(*world)
		want string
	}{
		{"only the CRS left, inside the grace", func(*world) {}, "READY"},
		{"CRS still not applied past the grace", func(w *world) { w.age = 2 * time.Hour }, "PROGRESSING"},
		{"a chart not Ready", func(w *world) { w.hcp = "False" }, "PROGRESSING"},
		{"CRS applied, something else progressing", func(w *world) { w.crs = "True" }, "PROGRESSING"},
		{"one node short", func(w *world) { w.ready = 1 }, "PROGRESSING"},
		{"not synced", func(w *world) { w.sync = "OutOfSync" }, "PROGRESSING"},
		{"add-on read fails", func(w *world) { w.readErr = errors.New("boom") }, "PROGRESSING"},
	} {
		w := base
		tc.edit(&w)
		api := newFakeAPI()
		svc := NewWithAPI(api, testCfg(), "svc-1")
		const id = "pc-c-1"
		api.apps["argocd/"+id] = map[string]any{
			"metadata": map[string]any{"name": id, "creationTimestamp": time.Now().Add(-w.age).UTC().Format(time.RFC3339),
				"labels": map[string]any{LabelProject: "p1", LabelService: "svc-1", LabelManagedBy: ManagedByValue}},
			"spec": map[string]any{"source": map[string]any{"helm": map[string]any{"valuesObject": map[string]any{
				"nodeGroups": []any{map[string]any{"name": "workers", "machineCount": float64(2)}},
			}}}},
			"status": map[string]any{
				"health": map[string]any{"status": "Progressing"},
				"sync":   map[string]any{"status": w.sync},
				// The live shape on Argo 3.6: names and kinds, no health.
				"resources": []any{
					map[string]any{"kind": "Secret", "name": id + "-metallb-config"},
					map[string]any{"group": "addons.cluster.x-k8s.io", "kind": "ClusterResourceSet", "name": id + "-tenant-config"},
					map[string]any{"group": "addons.cluster.x-k8s.io", "kind": "HelmChartProxy", "name": id + "-metallb"},
				},
			},
		}
		api.tcps["pc-p1/"+ControlPlaneName(id)] = map[string]any{"status": map[string]any{
			"controlPlaneEndpoint": "10.0.0.5:6443",
			"kubernetesResources":  map[string]any{"version": map[string]any{"status": "Ready"}},
		}}
		api.mds = []map[string]any{{
			"metadata": map[string]any{"labels": map[string]any{labelNodeGroup: "workers"}},
			"spec":     map[string]any{"replicas": float64(2)},
			"status":   map[string]any{"replicas": float64(2), "readyReplicas": w.ready, "phase": "Running"},
		}}
		api.crs = map[string][]map[string]any{
			"helmchartproxies":    {named(id+"-metallb", cond("Ready", w.hcp))},
			"clusterresourcesets": {named(id+"-tenant-config", cond("ResourcesApplied", w.crs))},
		}
		api.crsErr = w.readErr
		rows, err := svc.SyncProvider("az1", "p1").List(context.Background())
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: List = %d rows, %v", tc.name, len(rows), err)
		}
		if got := rows[0].Data["cluster"].(map[string]any)["status"]; got != tc.want {
			t.Errorf("%s: status = %v, want %s", tc.name, got, tc.want)
		}
	}
}

// Fixtures of the progress tests: times are minutes before now.
var progressNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func ago(min int) string {
	return progressNow.Add(-time.Duration(min) * time.Minute).Format(time.RFC3339)
}

func machine(cluster, group, name, phase string, updatedAgo int, conds ...map[string]any) map[string]any {
	list := []any{}
	for _, c := range conds {
		list = append(list, c)
	}
	return map[string]any{
		"metadata": map[string]any{"name": name, "creationTimestamp": ago(updatedAgo + 1),
			"labels": map[string]any{labelClusterName: cluster, labelNodeGroup: group}},
		"spec":   map[string]any{"infrastructureRef": map[string]any{"name": name}},
		"status": map[string]any{"phase": phase, "lastUpdated": ago(updatedAgo), "conditions": list},
	}
}

func pmachine(name, status, reason, msg string) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name}, "status": map[string]any{"conditions": []any{
		map[string]any{"type": condVMProvisioned, "status": status, "reason": reason, "message": msg}}}}
}

func condition(typ, status, reason string, changedAgo int) map[string]any {
	return map[string]any{"type": typ, "status": status, "reason": reason, "lastTransitionTime": ago(changedAgo)}
}

func md(cluster, group string, want, have, ready float64) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": cluster + "-" + group, "creationTimestamp": ago(120),
			"labels": map[string]any{labelClusterName: cluster, labelNodeGroup: group}},
		"spec":   map[string]any{"replicas": want},
		"status": map[string]any{"replicas": have, "readyReplicas": ready, "upToDateReplicas": have, "phase": "ScalingUp"},
	}
}

func TestClassifyMachine(t *testing.T) {
	ready := condition("NodeReady", "True", "NodeReady", 5)
	for _, tc := range []struct {
		name      string
		m, pm     map[string]any
		cpReady   bool
		code      string
		immediate bool
	}{
		{"the lab case: the VM runs, no node ever joined", machine("c", "w", "m", "Provisioned", 40, condition("NodeHealthy", "Unknown", reasonNodeNotExist, 40)),
			pmachine("m", "True", "", ""), true, CodeNotJoined, false},
		{"clone failed, CAPMOX tries again", machine("c", "w", "m", "Provisioning", 5), pmachine("m", "False", "CloningFailed", "500 storage busy"), true, CodeVMCreateRetrying, false},
		{"CAPMOX gave up", machine("c", "w", "m", "Provisioning", 5), pmachine("m", "False", "VMProvisionFailed", "unable to create VM 105"), true, CodeVMCreateFailed, true},
		{"a task failed", machine("c", "w", "m", "Provisioning", 5), pmachine("m", "False", "TaskFailed", "qmstart failed"), true, CodeVMCreateFailed, true},
		{"no memory on the node", machine("c", "w", "m", "Provisioning", 5),
			pmachine("m", "False", "VMProvisionFailed", "cannot reserve 8589934592B of memory on node pve3: 1024B available memory left"), true, CodeNoCapacity, true},
		{"a retried clone that says insufficient is not the terminal refusal", machine("c", "w", "m", "Provisioning", 5),
			pmachine("m", "False", "CloningFailed", "500 insufficient space on storage local-lvm"), true, CodeVMCreateRetrying, false},
		{"refused token", machine("c", "w", "m", "Provisioning", 5), pmachine("m", "False", "CloningFailed", "401 authentication failure"), true, CodeProviderAccessRefused, true},
		{"waits for the control plane", machine("c", "w", "m", "Pending", 60), nil, false, CodeWaitingControlPlane, false},
		{"pending with a control plane", machine("c", "w", "m", "Pending", 5), nil, true, CodeCreatingVM, false},
		{"cloning", machine("c", "w", "m", "Provisioning", 5), pmachine("m", "False", "Cloning", ""), true, CodeCreatingVM, false},
		{"booting", machine("c", "w", "m", "Provisioning", 5), pmachine("m", "False", "WaitingForBootstrapReady", ""), true, CodeStartingVM, false},
		{"joined, not Ready", machine("c", "w", "m", "Running", 30, condition("NodeReady", "False", "NodeNotReady", 3)), pmachine("m", "True", "", ""), true, CodeNodeNotReady, false},
		{"ready", machine("c", "w", "m", "Running", 30, ready), pmachine("m", "True", "", ""), true, "", false},
	} {
		st := classifyMachine(tc.m, tc.pm, tc.cpReady)
		if st.Code != tc.code || st.Immediate != tc.immediate || st.ready() != (tc.code == "") {
			t.Errorf("%s: got %+v, want code %q immediate %v", tc.name, st, tc.code, tc.immediate)
		}
	}
	// A node that is not Ready counts from the condition's change, not from the phase's.
	st := classifyMachine(machine("c", "w", "m", "Running", 30, condition("NodeReady", "False", "NodeNotReady", 3)), nil, true)
	if got := progressNow.Sub(st.Since); got != 3*time.Minute {
		t.Errorf("node not ready: clock is %v old, want 3m", got)
	}
}

func TestGroupProgress(t *testing.T) {
	states := func(cpReady bool, ms ...map[string]any) []MachineState {
		var out []MachineState
		for _, m := range ms {
			var pm map[string]any
			if digStr(m, "status", "phase") != "Pending" {
				pm = pmachine("m", "True", "", "")
			}
			out = append(out, classifyMachine(m, pm, cpReady))
		}
		return out
	}
	ready := func(name string) map[string]any {
		return machine("c", "w", name, "Running", 300, condition("NodeReady", "True", "NodeReady", 300))
	}
	// A settled group has no entry.
	if p := groupProgress(states(true, ready("a"), ready("b")), []map[string]any{md("c", "w", 2, 2, 2)}, 2, time.Time{}, 0, progressNow); p != nil {
		t.Errorf("settled group: %v", p)
	}
	// The lab case: built 40 min ago and never joined.
	p := groupProgress(states(true, machine("c", "w", "a", "Provisioned", 40)), []map[string]any{md("c", "w", 1, 1, 0)}, 1, time.Time{}, 0, progressNow)
	if p["code"] != CodeNotJoined || p["attention"] != true || p["machines"] != 1 || p["since"] != ago(40) {
		t.Errorf("never joined: %v", p)
	}
	// The same 10 min in is progress, not attention.
	if p := groupProgress(states(true, machine("c", "w", "a", "Provisioned", 10)), nil, 1, time.Time{}, 0, progressNow); p["attention"] != false {
		t.Errorf("joining: %v", p)
	}
	// A rolling upgrade: both old replicas ready, the surge machine stuck. ready == desired, so only the Machine shows it.
	rolling := md("c", "w", 2, 3, 2)
	p = groupProgress(states(true, ready("a"), ready("b"), machine("c", "w", "surge", "Provisioned", 50)), []map[string]any{rolling}, 2, time.Time{}, 0, progressNow)
	if p == nil || p["code"] != CodeNotJoined || p["attention"] != true {
		t.Errorf("stuck surge machine: %v", p)
	}
	// A short group with no Machine at all: the clock is the MachineDeployment's creation and ArgoCD's last sync.
	short := md("c", "w", 2, 0, 0)
	p = groupProgress(nil, []map[string]any{short}, 2, progressNow.Add(-20*time.Minute), 0, progressNow)
	if p["code"] != CodeProvisioning || p["machines"] != 2 || p["attention"] != true || p["since"] != ago(20) {
		t.Errorf("no machines, synced 20 min ago: %v", p)
	}
	if p := groupProgress(nil, []map[string]any{short}, 2, progressNow.Add(-5*time.Minute), 0, progressNow); p["attention"] != false {
		t.Errorf("no machines, synced 5 min ago: %v", p)
	}
	// Twelve machines cloning at once on slow disks: 40 min is inside 30 + 11×5 (capped at 90).
	var twelve []map[string]any
	for i := 0; i < 12; i++ {
		twelve = append(twelve, machine("c", "w", "m", "Provisioning", 40))
	}
	if p := groupProgress(states(true, twelve...), nil, 12, time.Time{}, 12, progressNow); p["attention"] != false || p["machines"] != 12 {
		t.Errorf("twelve parallel clones at 40 min: %v", p)
	}
	if p := groupProgress(states(true, twelve[0]), nil, 1, time.Time{}, 1, progressNow); p["attention"] != true {
		t.Errorf("one clone at 40 min: %v", p)
	}
	for i := range twelve {
		twelve[i] = machine("c", "w", "m", "Provisioning", 95)
	}
	if p := groupProgress(states(true, twelve...), nil, 12, time.Time{}, 12, progressNow); p["attention"] != true {
		t.Errorf("twelve clones at 95 min: %v", p)
	}
	// Waiting for the control plane never counts, however long.
	if p := groupProgress(states(false, machine("c", "w", "a", "Pending", 600)), nil, 1, time.Time{}, 0, progressNow); p["code"] != CodeWaitingControlPlane || p["attention"] != false {
		t.Errorf("waiting for the control plane: %v", p)
	}
	// A machine CAPMOX gave up on speaks first, at once.
	gaveUp := classifyMachine(machine("c", "w", "x", "Provisioning", 1), pmachine("x", "False", "VMProvisionFailed", "boom"), true)
	p = groupProgress(append(states(true, machine("c", "w", "a", "Pending", 1)), gaveUp), nil, 2, time.Time{}, 1, progressNow)
	if p["code"] != CodeVMCreateFailed || p["attention"] != true || p["machines"] != 1 {
		t.Errorf("give-up: %v", p)
	}
}

// A scale-up or a new group on an old cluster: the values already ask for more, ArgoCD's last sync is the one before
// the edit, and every machine and MachineDeployment is old. The missing machine counts from the edit.
func TestScaleUpIsNotStalledAtOnce(t *testing.T) {
	old := 48 * 60
	run := func(editedAgo int) map[string]any {
		c := map[string]any{"id": "c", "endpoint": "10.0.0.5:6443", "cp_status": "Ready", "node_groups": []any{
			map[string]any{"name": "workers", "count": float64(3)}, map[string]any{"name": "new", "count": float64(2)}}}
		app := map[string]any{
			"metadata": map[string]any{"creationTimestamp": ago(old), "annotations": map[string]any{}},
			"status":   map[string]any{"operationState": map[string]any{"finishedAt": ago(old)}},
		}
		if editedAgo >= 0 {
			app["metadata"].(map[string]any)["annotations"] = map[string]any{AnnotationChangedAt: ago(editedAgo)}
		}
		mdOld := md("c", "workers", 2, 2, 2)
		mdOld["metadata"].(map[string]any)["creationTimestamp"] = ago(old)
		objs := progressObjs{ok: true, machines: []map[string]any{
			machine("c", "workers", "a", "Running", old, condition("NodeReady", "True", "NodeReady", old)),
			machine("c", "workers", "b", "Running", old, condition("NodeReady", "True", "NodeReady", old)),
		}}
		addProgress(c, app, []map[string]any{mdOld}, objs, progressNow)
		return c
	}
	progress := func(c map[string]any, i int) map[string]any {
		p, _ := c["node_groups"].([]any)[i].(map[string]any)["progress"].(map[string]any)
		return p
	}
	c := run(1)
	for i, machines := range []int{1, 2} {
		if p := progress(c, i); p["code"] != CodeProvisioning || p["machines"] != machines || p["attention"] != false || p["since"] != ago(1) {
			t.Errorf("group %d one minute after the edit: %v", i, p)
		}
	}
	// The edit is the clock, so a machine that is still not there 20 min later does need attention.
	if p := progress(run(20), 0); p["attention"] != true || p["since"] != ago(20) {
		t.Errorf("20 min after the edit: %v", p)
	}
	// An Application no patch has stamped keeps ArgoCD's last sync as its clock.
	if p := progress(run(-1), 0); p["attention"] != true {
		t.Errorf("no stamp: %v", p)
	}
}

func TestClusterAttention(t *testing.T) {
	c := func(endpoint any, cp string, age int) map[string]any {
		return map[string]any{"endpoint": endpoint, "cp_status": cp, "created_at": ago(age)}
	}
	pc := func(age int, conds ...any) map[string]any {
		return map[string]any{"metadata": map[string]any{"creationTimestamp": ago(age)}, "status": map[string]any{"conditions": conds}}
	}
	avail := map[string]any{"type": condProxmoxAvail, "status": "True"}
	for _, tc := range []struct {
		name  string
		c, pc map[string]any
		want  any
	}{
		{"healthy", c("10.0.0.5:6443", "Ready", 600), pc(600, avail), nil},
		{"new cluster", c(nil, "", 5), nil, nil},
		{"no endpoint after 20 min", c(nil, "", 20), pc(20, avail), CodeControlPlaneStalled},
		{"endpoint, control plane stuck in Provisioning", c("10.0.0.5:6443", "Provisioning", 20), pc(20, avail), CodeControlPlaneStalled},
		{"a later upgrade is left alone", c("10.0.0.5:6443", "Upgrading", 600), pc(600, avail), nil},
		{"refused token on the ProxmoxCluster", c(nil, "", 2), pc(2, map[string]any{"type": condProxmoxAvail, "status": "False",
			"reason": "ProxmoxUnreachable", "message": "401 authentication failure", "lastTransitionTime": ago(1)}), CodeProviderAccessRefused},
		{"CAPMOX silent for 12 min", c("10.0.0.5:6443", "Ready", 12), pc(12), CodeProviderAccessRefused},
		{"CAPMOX silent for 5 min", c("10.0.0.5:6443", "Ready", 5), pc(5), nil},
	} {
		got := clusterAttention(tc.c, tc.pc, progressNow)
		if (got == nil) != (tc.want == nil) || (got != nil && got["code"] != tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The sync end to end on the fake: two clusters share a namespace and must not see each other's machines; a forbidden
// read leaves the fields out; any other error fails the List so the cache keeps the last row.
func TestSyncProviderProgress(t *testing.T) {
	// The sync runs on the wall clock: shift the fixtures onto it.
	saved := progressNow
	progressNow = time.Now().UTC()
	defer func() { progressNow = saved }()

	setup := func() (*fakeAPI, *Service) {
		api := newFakeAPI()
		svc := NewWithAPI(api, testCfg(), "svc-1")
		for _, id := range []string{"pc-c-stuck", "pc-c-fine"} {
			api.apps["argocd/"+id] = map[string]any{
				"metadata": map[string]any{"name": id, "creationTimestamp": ago(120),
					"labels": map[string]any{LabelProject: "p1", LabelService: "svc-1", LabelManagedBy: ManagedByValue}},
				"spec": map[string]any{"source": map[string]any{"helm": map[string]any{"valuesObject": map[string]any{
					"nodeGroups": []any{map[string]any{"name": "workers", "machineCount": float64(1)}},
				}}}},
				"status": map[string]any{"health": map[string]any{"status": "Progressing"}, "sync": map[string]any{"status": "Synced"}},
			}
			api.tcps["pc-p1/"+ControlPlaneName(id)] = map[string]any{"status": map[string]any{
				"controlPlaneEndpoint": "10.0.0.5:6443", "kubernetesResources": map[string]any{"version": map[string]any{"status": "Ready"}},
			}}
		}
		api.mds = []map[string]any{md("pc-c-stuck", "workers", 1, 1, 0), md("pc-c-fine", "workers", 1, 1, 1)}
		api.crs = map[string][]map[string]any{
			"machines": {
				machine("pc-c-stuck", "workers", "stuck-1", "Provisioned", 40, condition("NodeHealthy", "Unknown", reasonNodeNotExist, 40)),
				machine("pc-c-fine", "workers", "fine-1", "Running", 60, condition("NodeReady", "True", "NodeReady", 60)),
			},
			"proxmoxmachines": {pmachine("stuck-1", "True", "", ""), pmachine("fine-1", "True", "", "")},
		}
		return api, svc
	}
	list := func(svc *Service) (map[string]map[string]any, error) {
		rows, err := svc.SyncProvider("az1", "p1").List(context.Background())
		out := map[string]map[string]any{}
		for _, r := range rows {
			out[r.ExternalID] = r.Data["cluster"].(map[string]any)
		}
		return out, err
	}
	group := func(c map[string]any) map[string]any { return c["node_groups"].([]any)[0].(map[string]any) }

	api, svc := setup()
	first, err := list(svc)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := group(first["pc-c-stuck"])["progress"].(map[string]any)
	if p["code"] != CodeNotJoined || p["attention"] != true {
		t.Errorf("stuck cluster: progress = %v", p)
	}
	if got := group(first["pc-c-fine"])["progress"]; got != nil {
		t.Errorf("the settled cluster took its neighbour's machine: %v", got)
	}
	if first["pc-c-stuck"]["status"] != "PROGRESSING" {
		t.Errorf("attention must not change cluster.status: %v", first["pc-c-stuck"]["status"])
	}
	// One read of each kind per List, however many clusters.
	if n := len(api.listed); n != 3 {
		t.Errorf("ListCRs calls = %v, want machines, proxmoxmachines, proxmoxclusters once", api.listed)
	}
	// A second read inside the bound stores the same data: no minute counter, so no cache churn.
	second, _ := list(svc)
	if a, b := fmt.Sprint(first), fmt.Sprint(second); a != b {
		t.Errorf("two reads differ:\n%s\n%s", a, b)
	}

	// The RBAC of an older install: the fields are left out, the cluster still syncs.
	api, svc = setup()
	api.crsErrFor = map[string]error{"machines": &kamajik8s.APIError{Status: 403, Message: "forbidden"}}
	rows, err := list(svc)
	if err != nil || group(rows["pc-c-stuck"])["progress"] != nil || rows["pc-c-stuck"]["attention"] != nil {
		t.Errorf("forbidden read: err %v, cluster %v", err, rows["pc-c-stuck"])
	}
	// A transient error fails the sync instead of clearing the flag.
	api, svc = setup()
	api.crsErrFor = map[string]error{"proxmoxmachines": &kamajik8s.APIError{Status: 500, Message: "etcd leader changed"}}
	if _, err := list(svc); err == nil {
		t.Error("a 500 on the Machine read must fail the List")
	}

	// A settled project reads no Machines at all.
	api, svc = setup()
	api.mds[0] = md("pc-c-stuck", "workers", 1, 1, 1)
	if _, err := list(svc); err != nil || len(api.listed) != 0 {
		t.Errorf("settled project: err %v, read %v", err, api.listed)
	}
}

// Production finding R2: two workers had not joined for hours; a third, new one joined, and "needs attention" went
// away for 15 minutes. Every machine is measured from its own last progress, so neither a ready sibling nor one that
// is still starting hides the stuck ones.
func TestStuckMachinesKeepAttentionBesideAHealthyOne(t *testing.T) {
	stuck := func(name string) MachineState {
		return classifyMachine(machine("c", "w", name, "Provisioned", 209), pmachine(name, "True", "", ""), true)
	}
	fresh := func(phase string, conds ...map[string]any) MachineState {
		return classifyMachine(machine("c", "w", "new", phase, 1, conds...), pmachine("new", "True", "", ""), true)
	}
	mds := []map[string]any{md("c", "w", 3, 3, 1)}
	for name, sibling := range map[string]MachineState{
		"ready":           fresh("Running", condition("NodeReady", "True", "NodeReady", 1)),
		"joined, not yet": fresh("Running", condition("NodeReady", "False", "NodeNotReady", 1)),
		"still starting":  fresh("Provisioning"),
	} {
		p := groupProgress([]MachineState{stuck("a"), stuck("b"), sibling}, mds, 3, time.Time{}, 0, progressNow)
		if p["code"] != CodeNotJoined || p["machines"] != 2 || p["attention"] != true || p["since"] != ago(209) {
			t.Errorf("sibling %s: %v", name, p)
		}
		// The names of the machines that need attention, and only those: what the tenant's Replace is offered on when
		// the instance list has no row for them (production: the workers that never joined were never in it).
		if fmt.Sprint(p["machineNames"]) != "[a b]" {
			t.Errorf("sibling %s: machineNames %v", name, p["machineNames"])
		}
	}
	many := []MachineState{}
	for i := 0; i < progressMachineNames+2; i++ {
		many = append(many, stuck(fmt.Sprint("m", i)))
	}
	if p := groupProgress(many, mds, len(many), time.Time{}, 0, progressNow); len(p["machineNames"].([]string)) != progressMachineNames {
		t.Errorf("machineNames is not capped: %v", p["machineNames"])
	}
	// A machine that is not made yet has no name to replace.
	if p := groupProgress(nil, mds, 2, progressNow.Add(-3*time.Hour), 0, progressNow); p["attention"] != true || p["machineNames"] != nil {
		t.Errorf("no machine made: %v", p)
	}
	// Nothing stalled: the least advanced machine speaks, as before.
	p := groupProgress([]MachineState{fresh("Provisioned"), fresh("Provisioning")}, mds, 2, time.Time{}, 1, progressNow)
	if p["code"] != CodeCreatingVM && p["code"] != CodeProvisioning || p["attention"] != false || p["machineNames"] != nil {
		t.Errorf("two machines on their way: %v", p)
	}
}

func replaceSetup(t *testing.T, machines ...map[string]any) (*fakeAPI, *Service) {
	t.Helper()
	api := newFakeAPI()
	svc := NewWithAPI(api, testCfg(), "svc-1")
	api.apps["argocd/pc-c-1"] = map[string]any{"metadata": map[string]any{"name": "pc-c-1",
		"labels": map[string]any{LabelProject: "p1", LabelService: "svc-1", LabelManagedBy: ManagedByValue}}}
	api.crs = map[string][]map[string]any{"machines": machines}
	return api, svc
}

// The replace action deletes exactly the named worker Machine of the cluster, and asks before it takes the last
// Ready node of a group.
func TestReplaceMachine(t *testing.T) {
	ctx := context.Background()
	readyM := func(cluster, group, name string) map[string]any {
		return machine(cluster, group, name, "Running", 60, condition("NodeReady", "True", "NodeReady", 60))
	}
	stuck := machine("pc-c-1", "workers", "stuck", "Provisioned", 200)
	// Ready in every field but on its way out: only its deletionTimestamp says it will not hold the group's pods.
	deleting := readyM("pc-c-1", "workers", "leaving")
	deleting["metadata"].(map[string]any)["deletionTimestamp"] = ago(1)
	controlPlane := machine("pc-c-1", "", "cp-0", "Running", 60)
	api, svc := replaceSetup(t, stuck, readyM("pc-c-1", "workers", "only-ready"), deleting, controlPlane,
		readyM("pc-c-other", "workers", "foreign"), readyM("pc-c-1", "gpu", "gpu-a"), readyM("pc-c-1", "gpu", "gpu-b"))

	// A machine without a Node needs no confirmation, whatever the group has left.
	if err := svc.ReplaceMachine(ctx, "pc-c-1", "stuck", false); err != nil {
		t.Fatalf("stuck machine: %v", err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != "machines:pc-p1/stuck" {
		t.Fatalf("deleted = %v", api.deleted)
	}
	// The last Ready node of its group: refused, then done with confirm. The deleting sibling does not count as Ready.
	if err := svc.ReplaceMachine(ctx, "pc-c-1", "only-ready", false); !errors.Is(err, ErrLastReadyNode) {
		t.Errorf("last ready node: %v", err)
	}
	if err := svc.ReplaceMachine(ctx, "pc-c-1", "only-ready", true); err != nil {
		t.Errorf("last ready node, confirmed: %v", err)
	}
	// One of two Ready nodes needs none.
	if err := svc.ReplaceMachine(ctx, "pc-c-1", "gpu-a", false); err != nil {
		t.Errorf("one of two ready: %v", err)
	}
	for name, want := range map[string]error{"leaving": ErrMachineDeleting, "foreign": ErrMachineNotFound, "cp-0": ErrMachineNotFound, "nope": ErrMachineNotFound} {
		if err := svc.ReplaceMachine(ctx, "pc-c-1", name, true); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", name, err, want)
		}
	}
	if err := svc.ReplaceMachine(ctx, "pc-c-none", "stuck", true); !errors.Is(err, ErrClusterNotFound) {
		t.Errorf("unknown cluster: %v", err)
	}
	// Another provider's cluster of the same name is not ours to touch.
	api.apps["argocd/pc-c-1"]["metadata"].(map[string]any)["labels"].(map[string]any)[LabelService] = "svc-2"
	if err := svc.ReplaceMachine(ctx, "pc-c-1", "gpu-b", true); !errors.Is(err, ErrClusterNotFound) {
		t.Errorf("another provider's cluster: %v", err)
	}
	if want := []string{"machines:pc-p1/stuck", "machines:pc-p1/only-ready", "machines:pc-p1/gpu-a"}; fmt.Sprint(api.deleted) != fmt.Sprint(want) {
		t.Errorf("deleted = %v, want %v", api.deleted, want)
	}
}

// The operator's machines view carries each node group's MachineHealthCheck and the provider that hosts the workers.
func TestClusterMachinesHealthChecks(t *testing.T) {
	api, svc := replaceSetup(t, machine("pc-c-1", "workers", "stuck", "Provisioned", 200))
	api.crs["proxmoxmachines"] = []map[string]any{pmachine("stuck", "True", "", "")}
	api.crs["machinehealthchecks"] = []map[string]any{{
		"metadata": map[string]any{"name": "pc-c-1-workers", "labels": map[string]any{labelClusterName: "pc-c-1", labelNodeGroup: "workers"}},
		"spec": map[string]any{"checks": map[string]any{"nodeStartupTimeoutSeconds": float64(1800)},
			"remediation": map[string]any{"triggerIf": map[string]any{"unhealthyLessThanOrEqualTo": float64(1)}}},
		"status": map[string]any{"expectedMachines": float64(2), "currentHealthy": float64(0), "remediationsAllowed": float64(0)},
	}}
	if err := api.ApplySecret(context.Background(), "pc-p1", CloudSecretName("pc-c-1"), nil, nil, map[string]string{annotationComputeService: "pve-1"}); err != nil {
		t.Fatal(err)
	}
	out, err := svc.ClusterMachines(context.Background(), "pc-c-1", progressNow)
	if err != nil {
		t.Fatal(err)
	}
	if want := []HealthCheck{{NodeGroup: "workers", NodeStartupTimeoutSeconds: 1800, MaxUnhealthy: 1, ExpectedMachines: 2}}; fmt.Sprint(out.HealthChecks) != fmt.Sprint(want) {
		t.Errorf("health checks = %+v", out.HealthChecks)
	}
	if out.ComputeServiceID != "pve-1" || len(out.Machines) != 1 || out.Machines[0].Code != CodeNotJoined || !out.Machines[0].Attention || out.Machines[0].Ready {
		t.Errorf("compute %q, machines %+v", out.ComputeServiceID, out.Machines)
	}
	// An install whose role cannot read them yet still gets its machines.
	api.crsErrFor = map[string]error{"machinehealthchecks": &kamajik8s.APIError{Status: 403, Message: "forbidden"}}
	if out, err := svc.ClusterMachines(context.Background(), "pc-c-1", progressNow); err != nil || len(out.HealthChecks) != 0 || len(out.Machines) != 1 {
		t.Errorf("forbidden health checks: %v %+v", err, out)
	}
}

// The machineHealthCheck block is written only for a chart that reads it, at create and when a cluster is moved onto
// such a chart; after that it follows the provider's setting at every change of the cluster.
func TestMachineHealthCheckValues(t *testing.T) {
	cfg := testCfg()
	cfg.Defaults.NodeStartupTimeoutSeconds = 1800
	cfg.ChartVersion = "0.1.13"
	if v := BuildValues(cfg, testSpec())["machineHealthCheck"]; v != nil {
		t.Errorf("chart 0.1.13 got %v", v)
	}
	cfg.ChartVersion = MachineHealthCheckChartVersion
	want := map[string]any{"nodeStartupTimeoutSeconds": 1800, "maxUnhealthy": 1}
	if v := BuildValues(cfg, testSpec())["machineHealthCheck"]; fmt.Sprint(v) != fmt.Sprint(want) {
		t.Errorf("chart %s got %v, want %v", cfg.ChartVersion, v, want)
	}
	off := cfg
	off.Defaults.NodeStartupTimeoutSeconds = 0
	if v := BuildValues(off, testSpec())["machineHealthCheck"]; v != nil {
		t.Errorf("turned off, got %v", v)
	}

	// A cluster made on 0.1.13 and bumped.
	old := cfg
	old.ChartVersion = "0.1.13"
	cfg.Defaults.RepairMaxUnhealthy = 2
	api := newFakeAPI()
	ctx := context.Background()
	spec := testSpec()
	if _, err := NewWithAPI(api, old, "svc-1").CreateCluster(ctx, spec, ClusterSecrets{Cloud: map[string]string{"token": "t"}}); err != nil {
		t.Fatal(err)
	}
	svc := NewWithAPI(api, cfg, "svc-1")
	values := func() any {
		return dig(api.apps["argocd/"+spec.ID], "spec", "source", "helm", "valuesObject", "machineHealthCheck")
	}
	if err := svc.SetChartVersion(ctx, spec.ID, "0.1.13"); err != nil || values() != nil {
		t.Fatalf("re-pin on 0.1.13: %v, %v", err, values())
	}
	if err := svc.SetChartVersion(ctx, spec.ID, "0.1.14"); err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"nodeStartupTimeoutSeconds": 1800, "maxUnhealthy": 2}; fmt.Sprint(values()) != fmt.Sprint(want) {
		t.Errorf("bumped: %v, want %v", values(), want)
	}
	// The provider's setting is the cluster's: a new timeout reaches a cluster that already has the block, at a bump
	// onto the version it is on and at any other change of the cluster, and 0 takes the block away.
	cfg.Defaults.NodeStartupTimeoutSeconds = 3600
	if err := NewWithAPI(api, cfg, "svc-1").SetChartVersion(ctx, spec.ID, "0.1.14"); err != nil || fmt.Sprint(values()) != fmt.Sprint(map[string]any{"nodeStartupTimeoutSeconds": 3600, "maxUnhealthy": 2}) {
		t.Errorf("same-version bump under a new timeout: %v, %v", err, values())
	}
	cfg.Defaults.NodeStartupTimeoutSeconds = 0
	if err := NewWithAPI(api, cfg, "svc-1").PatchClusterValues(ctx, spec.ID, func(map[string]any) error { return nil }); err != nil || values() != nil {
		t.Errorf("turned off, after a values patch: %v, %v", err, values())
	}
	// A chart that does not read the block: what the values carry is not touched.
	api.apps["argocd/"+spec.ID]["spec"].(map[string]any)["source"].(map[string]any)["helm"].(map[string]any)["valuesObject"].(map[string]any)["machineHealthCheck"] = "kept"
	if err := NewWithAPI(api, cfg, "svc-1").SetChartVersion(ctx, spec.ID, "0.1.13"); err != nil || values() != "kept" {
		t.Errorf("chart 0.1.13: %v, %v", err, values())
	}
}

// Both consoles get the same answers for what the replace action refuses; the last Ready node carries the reason a
// console asks its second question on, and a tenant never reads the RBAC file name.
func TestReplaceMachineError(t *testing.T) {
	status := func(err error) (int, any, string) {
		var he *httpx.HTTPError
		if !errors.As(ReplaceMachineError(err), &he) {
			return 0, nil, ""
		}
		return he.Status, he.Details, he.Msg
	}
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"machine": {ErrMachineNotFound, 404}, "cluster": {ErrClusterNotFound, 404}, "deleting": {ErrMachineDeleting, 409},
		"last ready": {ErrLastReadyNode, 409}, "rbac": {&kamajik8s.APIError{Status: 403, Message: "machines is forbidden"}, 502},
	} {
		if got, _, _ := status(tc.err); got != tc.want {
			t.Errorf("%s: status %d, want %d", name, got, tc.want)
		}
	}
	if _, details, _ := status(ErrLastReadyNode); fmt.Sprint(details) != "map[reason:last_ready_node]" {
		t.Errorf("last ready node details: %v", details)
	}
	if _, _, msg := status(&kamajik8s.APIError{Status: 403, Message: "machines is forbidden"}); strings.Contains(msg, "rbac.yaml") || strings.Contains(msg, "forbidden") {
		t.Errorf("the tenant's message names the operator's file: %s", msg)
	}
	plain := errors.New("dial tcp: connection refused")
	if ReplaceMachineError(plain) != plain || ReplaceMachineError(nil) != nil {
		t.Error("any other error, and nil, must come back as they are")
	}
}

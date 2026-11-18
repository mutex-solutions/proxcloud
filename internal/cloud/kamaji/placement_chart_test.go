package kamaji

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// machineDeploymentReplicas renders the chart and returns each MachineDeployment's replicas by name.
func machineDeploymentReplicas(t *testing.T, out string) map[string]int {
	t.Helper()
	got := map[string]int{}
	for _, doc := range strings.Split(out, "\n---") {
		var obj struct {
			Kind     string
			Metadata struct{ Name string }
			Spec     struct{ Replicas int }
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil || obj.Kind != "MachineDeployment" {
			continue
		}
		got[obj.Metadata.Name] = obj.Spec.Replicas
	}
	return got
}

func placementValues(groups string) string { return "nodeGroups:\n" + groups }

const placementGroup = `  - name: %s
    machineFlavor: small
    machineImageId: "%s"
    machineCores: 2
    machineMemoryMiB: 2048
    machineRootVolume: {diskSize: 30}
    machineTags: [proxcloud]
    machineCount: %d
`

// EvenSplit is the baseline of every group that has no stored placement: it must be the chart's own split, or the
// first edit of an existing cluster would move its workers. One render, a group per (count, number of nodes).
func TestEvenSplitMatchesChart(t *testing.T) {
	var groups strings.Builder
	for count := 0; count <= 15; count++ {
		for n := 1; n <= 6; n++ {
			var pins []string
			for i := 1; i <= n; i++ {
				pins = append(pins, fmt.Sprintf("pve%d=92%02d", i, i))
			}
			fmt.Fprintf(&groups, placementGroup, fmt.Sprintf("g%dn%d", count, n), strings.Join(pins, ","), count)
		}
	}
	got := machineDeploymentReplicas(t, helmTemplate(t, "c", "pc-p1", placementValues(groups.String())))
	if len(got) != 16*21 {
		t.Fatalf("%d MachineDeployments rendered, want %d", len(got), 16*21)
	}
	for count := 0; count <= 15; count++ {
		for n := 1; n <= 6; n++ {
			var nodes []string
			for i := 1; i <= n; i++ {
				nodes = append(nodes, fmt.Sprintf("pve%d", i))
			}
			for node, want := range EvenSplit(count, nodes) {
				if name := fmt.Sprintf("c-g%dn%d-%s", count, n, node); got[name] != want {
					t.Errorf("%s: chart %d, EvenSplit %d", name, got[name], want)
				}
			}
		}
	}
}

// A group with machinePlacement gets exactly those replicas per node, a node the map leaves out gets 0, and the
// machine templates keep their names: placement must never roll a node.
func TestChartPlacement(t *testing.T) {
	const image = "pve1=9201,pve2=9202,pve3=9203"
	even := helmTemplate(t, "c", "pc-p1", placementValues(fmt.Sprintf(placementGroup, "workers", image, 5)))
	placed := helmTemplate(t, "c", "pc-p1", placementValues(fmt.Sprintf(placementGroup, "workers", image, 5)+"    machinePlacement: {pve1: 1, pve3: 4}\n"))
	if got := machineDeploymentReplicas(t, even); got["c-workers-pve1"] != 2 || got["c-workers-pve2"] != 2 || got["c-workers-pve3"] != 1 {
		t.Errorf("even split: %v", got)
	}
	if got := machineDeploymentReplicas(t, placed); got["c-workers-pve1"] != 1 || got["c-workers-pve2"] != 0 || got["c-workers-pve3"] != 4 || len(got) != 3 {
		t.Errorf("placement: %v", got)
	}
	// Only the replicas lines differ.
	a, b := strings.Split(even, "\n"), strings.Split(placed, "\n")
	if len(a) != len(b) {
		t.Fatalf("placement changes the number of rendered lines: %d and %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] && !strings.HasPrefix(strings.TrimSpace(a[i]), "replicas:") {
			t.Errorf("placement changes more than replicas: %q became %q", a[i], b[i])
		}
	}
}

// ArgoCD prunes: a map that does not add up, or that names a node without an image, would delete workers. The render
// fails instead, and the Application keeps what it has.
func TestChartRefusesBadPlacement(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	for name, tc := range map[string]struct{ placement, want string }{
		"sum below the count": {"{pve1: 1, pve2: 1}", "machinePlacement places 2 machines, machineCount is 3"},
		"sum above the count": {"{pve1: 3, pve2: 1}", "machinePlacement places 4 machines, machineCount is 3"},
		"unknown node":        {"{pve1: 2, pve9: 1}", `names node "pve9"`},
		"negative":            {"{pve1: 4, pve2: -1}", "is negative"},
	} {
		f := filepath.Join(t.TempDir(), "values.yaml")
		values := placementValues(fmt.Sprintf(placementGroup, "workers", "pve1=9201,pve2=9202", 3) + "    machinePlacement: " + tc.placement + "\n")
		if err := os.WriteFile(f, []byte(values), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(helm, "template", "c", "../../../deploy/charts/proxmox-kamaji-cluster", "-f", f).CombinedOutput()
		if err == nil || !strings.Contains(string(out), tc.want) {
			t.Errorf("%s: err %v, want a failure with %q\n%s", name, err, tc.want, out)
		}
	}
}

func TestSetPlacement(t *testing.T) {
	group := func() map[string]any {
		return map[string]any{"name": "workers", "machineCount": 3, "machineImageId": "pve1=9201,pve2=9202"}
	}
	g := group()
	if err := SetPlacement(g, map[string]int{"pve1": 3, "pve2": 0}); err != nil {
		t.Fatal(err)
	}
	if p := g["machinePlacement"].(map[string]any); len(p) != 1 || p["pve1"] != 3 {
		t.Errorf("stored %v, want only pve1: 3", p)
	}
	for name, bad := range map[string]map[string]int{
		"empty": {}, "short": {"pve1": 2}, "long": {"pve1": 2, "pve2": 2}, "unknown node": {"pve1": 2, "pve3": 1}, "negative": {"pve1": 4, "pve2": -1},
	} {
		g := group()
		if err := SetPlacement(g, bad); err == nil || g["machinePlacement"] != nil {
			t.Errorf("%s: err %v, stored %v", name, err, g["machinePlacement"])
		}
	}
	// What is stored reads back, and the tenant-facing shape carries it.
	values := map[string]any{"nodeGroups": []any{g}}
	if got := GroupPlacements(values)[0]; got.Placement["pve1"] != 3 || got.Count != 3 || len(got.Nodes()) != 2 {
		t.Errorf("read back %+v", got)
	}
	if got := GroupPlacements(map[string]any{"nodeGroups": []any{group()}})[0].Baseline(); got["pve1"] != 2 || got["pve2"] != 1 {
		t.Errorf("baseline without a stored placement: %v, want the even split", got)
	}
}

func TestImageTemplates(t *testing.T) {
	got := ImageTemplates("pve1=9201, pve2=9202,pve3=,=9204,pve5=abc,bare")
	if len(got) != 2 || got["pve1"] != 9201 || got["pve2"] != 9202 {
		t.Errorf("ImageTemplates = %v, want pve1 and pve2 only", got)
	}
}

func TestChartHasPlacement(t *testing.T) {
	for v, want := range map[string]bool{"0.1.12": false, "0.1.13": true, "0.1.13-rc1": true, "0.2.0": true, "": false, "latest": false} {
		if ChartHasPlacement(v) != want {
			t.Errorf("ChartHasPlacement(%q) = %v", v, !want)
		}
	}
}

// What the placement call sites read off the management cluster.
func TestPlacementReads(t *testing.T) {
	api := newFakeAPI()
	svc := NewWithAPI(api, testCfg(), "svc-1")
	ctx := t.Context()
	spec := testSpec()
	spec.NodeGroups[0].Placement = map[string]int{"pve1": spec.NodeGroups[0].Count}
	if _, err := svc.CreateCluster(ctx, spec, ClusterSecrets{Cloud: map[string]string{"token": "t"}}); err != nil {
		t.Fatal(err)
	}
	// The values carry the decision, and the sync shows it.
	clusters, err := svc.ListClusterValues(ctx)
	if err != nil || len(clusters) != 1 || clusters[0].ID != spec.ID {
		t.Fatalf("ListClusterValues = %v, %v", clusters, err)
	}
	if got := GroupPlacements(clusters[0].Values)[0].Placement; got["pve1"] != spec.NodeGroups[0].Count {
		t.Errorf("stored placement %v", got)
	}
	if got := NodeGroupsFromValues(clusters[0].Values)[0]["placement"]; got == nil {
		t.Error("the synced node group lacks its placement")
	}
	// A patch sees the chart version the cluster itself is pinned to.
	var seen string
	if err := svc.PatchClusterChartValues(ctx, spec.ID, func(_ map[string]any, v string) error { seen = v; return nil }); err != nil || seen != testCfg().ChartVersion {
		t.Errorf("chart version in the patch = %q, %v; want %q", seen, err, testCfg().ChartVersion)
	}
	// A terminating cluster asks Proxmox for nothing more.
	api.apps["argocd/"+spec.ID]["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-17T00:00:00Z"
	if clusters, _ := svc.ListClusterValues(ctx); len(clusters) != 0 {
		t.Errorf("a deleting cluster is listed: %v", clusters)
	}

	api.mds = []map[string]any{
		{"metadata": map[string]any{"name": spec.ID + "-workers-pve1", "labels": map[string]any{labelNodeGroup: "workers"}},
			"spec": map[string]any{"replicas": float64(2)}, "status": map[string]any{"readyReplicas": float64(2)}},
		{"metadata": map[string]any{"name": spec.ID + "-workers-pve2", "labels": map[string]any{labelNodeGroup: "workers"}},
			"spec": map[string]any{"replicas": float64(2)}, "status": map[string]any{}},
	}
	unready, err := svc.UnreadyReplicas(ctx, spec.ProjectID, spec.ID)
	if err != nil || unready["workers"]["pve1"] != 0 || unready["workers"]["pve2"] != 2 {
		t.Errorf("UnreadyReplicas = %v, %v", unready, err)
	}
	// A group whose name is the start of another's, and node names with a dash: the group comes from the label and the
	// node is what follows "<cluster>-<group>-".
	unreadyMD := func(group, node string, want, ready float64) map[string]any {
		return map[string]any{"metadata": map[string]any{"name": spec.ID + "-" + group + "-" + node, "labels": map[string]any{labelNodeGroup: group}},
			"spec": map[string]any{"replicas": want}, "status": map[string]any{"readyReplicas": ready}}
	}
	api.mds = []map[string]any{
		unreadyMD("work", "pve1", 3, 2), unreadyMD("work", "gpu-pve1", 1, 1),
		unreadyMD("work-gpu", "pve1", 2, 0), unreadyMD("work-gpu", "gpu-pve1", 4, 1),
		// Not the chart's name for its group: left out, never guessed.
		{"metadata": map[string]any{"name": "other-work-pve9", "labels": map[string]any{labelNodeGroup: "work"}}, "spec": map[string]any{"replicas": float64(5)}},
	}
	unready, err = svc.UnreadyReplicas(ctx, spec.ProjectID, spec.ID)
	work, gpu := unready["work"], unready["work-gpu"]
	if err != nil || len(unready) != 2 || len(work) != 2 || work["pve1"] != 1 || work["gpu-pve1"] != 0 || len(gpu) != 2 || gpu["pve1"] != 2 || gpu["gpu-pve1"] != 3 {
		t.Errorf("UnreadyReplicas with prefix names = %v, %v", unready, err)
	}
}

// healthChecks renders the chart and returns each MachineHealthCheck's startup timeout and guard by name.
func healthChecks(t *testing.T, out string) map[string][2]int {
	t.Helper()
	got := map[string][2]int{}
	for _, doc := range strings.Split(out, "\n---") {
		var obj struct {
			Kind     string
			Metadata struct {
				Name        string
				Annotations map[string]string
			}
			Spec struct {
				ClusterName string `yaml:"clusterName"`
				Selector    struct {
					MatchLabels map[string]string `yaml:"matchLabels"`
				}
				Checks struct {
					NodeStartupTimeoutSeconds int `yaml:"nodeStartupTimeoutSeconds"`
				}
				Remediation struct {
					TriggerIf struct {
						UnhealthyLessThanOrEqualTo any `yaml:"unhealthyLessThanOrEqualTo"`
					} `yaml:"triggerIf"`
				}
			}
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil || obj.Kind != "MachineHealthCheck" {
			continue
		}
		guard, ok := obj.Spec.Remediation.TriggerIf.UnhealthyLessThanOrEqualTo.(int)
		if !ok {
			t.Errorf("%s: the guard %v is not a number", obj.Metadata.Name, obj.Spec.Remediation.TriggerIf.UnhealthyLessThanOrEqualTo)
		}
		group := strings.TrimPrefix(obj.Metadata.Name, "c-")
		if obj.Spec.ClusterName != "c" || obj.Spec.Selector.MatchLabels[labelNodeGroup] != group || obj.Spec.Selector.MatchLabels[labelClusterName] != "c" {
			t.Errorf("%s selects %v of cluster %q", obj.Metadata.Name, obj.Spec.Selector.MatchLabels, obj.Spec.ClusterName)
		}
		if obj.Metadata.Annotations["argocd.argoproj.io/ignore-healthcheck"] != "true" {
			t.Errorf("%s counts in the Application's health", obj.Metadata.Name)
		}
		got[obj.Metadata.Name] = [2]int{obj.Spec.Checks.NodeStartupTimeoutSeconds, guard}
	}
	return got
}

// A worker that never registers a Node is replaced by a MachineHealthCheck per node group. The guard is a number,
// capped below the group's size: a group whose machines all failed (the production case: 2 of 2) is left alone, and a
// group of one has no check at all. The chart's default renders none, as chart 0.1.13 does.
func TestChartMachineHealthCheck(t *testing.T) {
	groups := fmt.Sprintf(placementGroup, "one", "pve1=9201", 1) + fmt.Sprintf(placementGroup, "two", "pve1=9201,pve2=9202", 2) +
		fmt.Sprintf(placementGroup, "five", "pve1=9201", 5)
	if got := healthChecks(t, helmTemplate(t, "c", "pc-p1", placementValues(groups))); len(got) != 0 {
		t.Errorf("default values render %v", got)
	}
	mhc := func(body string) string { return placementValues(groups) + "machineHealthCheck:\n" + body }
	got := healthChecks(t, helmTemplate(t, "c", "pc-p1", mhc("  nodeStartupTimeoutSeconds: 1800\n")))
	if want := map[string][2]int{"c-two": {1800, 1}, "c-five": {1800, 1}}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("guard 1: %v, want %v", got, want)
	}
	got = healthChecks(t, helmTemplate(t, "c", "pc-p1", mhc("  nodeStartupTimeoutSeconds: 900\n  maxUnhealthy: 3\n")))
	if want := map[string][2]int{"c-two": {900, 1}, "c-five": {900, 3}}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("guard 3: %v, want %v", got, want)
	}
	for _, bad := range []string{"  nodeStartupTimeoutSeconds: 900\n  maxUnhealthy: \"40%\"\n", "  nodeStartupTimeoutSeconds: 900\n  maxUnhealthy: 0\n",
		"  nodeStartupTimeoutSeconds: -5\n", "  nodeStartupTimeoutSeconds: 15m\n"} {
		f := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(f, []byte(mhc(bad)), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("helm", "template", "c", "../../../deploy/charts/proxmox-kamaji-cluster", "-f", f).CombinedOutput(); err == nil || !strings.Contains(string(out), "machineHealthCheck.") {
			t.Errorf("%q rendered: %v\n%.300s", bad, err, out)
		}
	}
}

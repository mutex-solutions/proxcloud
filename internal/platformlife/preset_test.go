package platformlife

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// The control plane count: 1 or 3, a spec without it is 1 (Normalize writes it), the node range holds count + 1
// control plane addresses, machines on several nodes and a sandbox block are refused with the reason.
func TestValidateControlPlaneCount(t *testing.T) {
	in := validInstallation(t)
	if got := in.Spec.Clusters[0].ControlPlane.Replicas; got != 1 {
		t.Fatalf("Normalize left replicas at %d, want 1", got)
	}
	in.Spec.Clusters[0].ControlPlane.Nodes = []string{"pve1"}
	if in.Normalize(); in.Spec.Clusters[0].ControlPlane.Nodes != nil {
		t.Errorf("Normalize kept controlPlane.nodes %v, which says what node says", in.Spec.Clusters[0].ControlPlane.Nodes)
	}
	stored := validInstallation(t) // a record from before the field
	stored.Spec.Clusters[0].ControlPlane.Replicas = 0
	if err := stored.Validate(); err != nil || stored.Spec.Clusters[0].ControlPlane.Count() != 1 {
		t.Fatalf("a spec without replicas is one machine: %v", err)
	}
	for _, tc := range []struct {
		name, want string
		mut        func(*Installation)
	}{
		{"three", "", func(in *Installation) { in.Spec.Clusters[0].ControlPlane.Replicas = 3 }},
		{"the one node, named twice", "", func(in *Installation) { in.Spec.Clusters[0].ControlPlane.Nodes = []string{"pve1"} }},
		{"two", "controlPlane.replicas 2: 1 or 3", func(in *Installation) { in.Spec.Clusters[0].ControlPlane.Replicas = 2 }},
		{"five", "controlPlane.replicas 5: 1 or 3", func(in *Installation) { in.Spec.Clusters[0].ControlPlane.Replicas = 5 }},
		{"negative", "controlPlane.replicas -1: 1 or 3", func(in *Installation) { in.Spec.Clusters[0].ControlPlane.Replicas = -1 }},
		{"three on three nodes", "controlPlane.nodes: every control plane machine goes to one Proxmox node", func(in *Installation) {
			in.Spec.Clusters[0].ControlPlane.Replicas, in.Spec.Clusters[0].ControlPlane.Nodes = 3, []string{"pve1", "pve2", "pve3"}
		}},
		{"another node than controlPlane.node", "shared storage", func(in *Installation) { in.Spec.Clusters[0].ControlPlane.Nodes = []string{"pve2"} }},
		// 3 + 1 control plane, 3 group machines, 3 group surges: 10. The lab range of 20 holds them; 9 do not.
		{"three in a range for one", "nodeRange holds 9 addresses; 7 machines (3 of the control plane and its one more during an update) and 3 rolling surges need 10", func(in *Installation) {
			in.Spec.Clusters[0].ControlPlane.Replicas, in.Spec.Clusters[0].NodeRange = 3, "10.200.200.30-10.200.200.38"
		}},
		{"one in the same range", "", func(in *Installation) { in.Spec.Clusters[0].NodeRange = "10.200.200.30-10.200.200.38" }},
		{"sandbox elsewhere", `spec.sandbox.placement "own" is not shared, separate or off`, func(in *Installation) { in.Spec.Sandbox = &SandboxSpec{Placement: "own"} }},
	} {
		in := validInstallation(t)
		tc.mut(in)
		err := in.Validate()
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
}

// A control plane a run built with three machines does not shrink; what was only saved does. The rule reads what a
// run handed to the cluster, not the stored spec, and the count is not one of the fixed fields.
func TestControlPlaneShrink(t *testing.T) {
	with := func(n, built int, installed bool) *Installation {
		in := validInstallation(t)
		in.Spec.Clusters[0].ControlPlane.Replicas = n
		if installed {
			in.Status.BOM = &BOMRef{Release: "0.9.0", SHA256: "x"}
			in.Status.Clusters = []ClusterStatus{{Name: ClusterName}}
		}
		if built > 0 {
			in.Status.Clusters = []ClusterStatus{{Name: ClusterName, ControlPlaneReplicas: built}}
		}
		return in
	}
	for _, tc := range []struct {
		name      string
		old, next *Installation
		want      string
	}{
		{"1 to 3, installed", with(1, 0, true), with(3, 0, false), ""},
		{"stored without the field to 3", with(0, 0, true), with(3, 0, false), ""},
		{"3 to 3, built", with(3, 3, true), with(3, 0, false), ""},
		{"3 to 1, a draft", with(3, 0, false), with(1, 0, false), ""},
		{"3 saved on an installed platform and never applied, back to 1", with(3, 0, true), with(1, 0, false), ""},
		{"3 to 1, built", with(3, 3, true), with(1, 0, false), "does not go from 3 machines to 1"},
		{"3 to none, built", with(3, 3, true), with(0, 0, false), "etcd"},
		{"3 to 1, half built by a first install that failed", with(3, 3, false), with(1, 0, false), "a run built this control plane with 3 machines"},
	} {
		got := ControlPlaneShrink(tc.old, tc.next)
		if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
		if f := FixedChanges(tc.old, tc.next); slices.ContainsFunc(f, func(s string) bool { return strings.Contains(s, "controlPlane") }) {
			t.Errorf("%s: the count is a fixed field: %v", tc.name, f)
		}
	}
}

// A count above one is a problem of the save, not only of the Check, where the installation's BOM names a chart that
// builds one machine: a saved 3 would fail every apply and upgrade of that platform.
func TestControlPlaneCountProblems(t *testing.T) {
	// The BOM of 0.8.0, by name: the first file under previous/ is no longer the oldest release (0.10.0-rc.1 sorts
	// before 0.8.0), and its chart builds three.
	old080, ok := Lookup(previousSHA256["0.8.0"])
	if !ok {
		t.Fatal("this build does not embed the 0.8.0 BOM")
	}
	in := validInstallation(t)
	in.Status.BOM = &BOMRef{Release: old080.Release, SHA256: old080.SHA256}
	if got := in.ControlPlaneCountProblems(); got != nil || in.BuildsThree() {
		t.Errorf("one machine on a previous BOM: %v, builds three %t", got, in.BuildsThree())
	}
	in.Spec.Clusters[0].ControlPlane.Replicas = 3
	if got := in.ControlPlaneCountProblems(); len(got) != 1 || !strings.Contains(got[0], "which builds one") || !strings.Contains(got[0], "0.1.14") {
		t.Errorf("three on a previous BOM: %v", got)
	}
	in.Status.BOM = nil
	withClusterChart(t, "0.1.14")
	if got := in.ControlPlaneCountProblems(); got != nil || !in.BuildsThree() {
		t.Errorf("three on a BOM with chart 0.1.14: %v, builds three %t", got, in.BuildsThree())
	}
}

// withClusterChart sets the cluster chart version of this release's embedded BOM for the test.
func withClusterChart(t *testing.T, version string) {
	t.Helper()
	cur, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	saved := *cur.BOM
	t.Cleanup(func() { *cur.BOM = saved })
	cur.Components = slices.Clone(cur.Components)
	for i := range cur.Components {
		if cur.Components[i].Name == ClusterName {
			cur.Components[i].Chart.Version = version
		}
	}
}

// A preset fills what the spec leaves out and nothing else; the result is a spec Validate takes.
func TestApplyPreset(t *testing.T) {
	bare := func(mut func(*Spec)) *Installation {
		in := &Installation{Name: "lab", ServiceID: "svc-pve", Spec: Spec{
			PVEAPIURL: "https://10.200.200.2:8006",
			Storage:   Storage{Boot: "local-lvm", ISO: "local", Import: "nas-templates"},
			Network:   Network{Prefix: "10.200.200.0/24", Gateway: "10.200.200.1", DNS: []string{"10.200.200.1"}},
			Seed:      SeedSpec{Node: "pve1", Address: "10.200.200.10", APISources: []string{"192.168.33.0/24"}},
			Clusters:  []ClusterSpec{{Endpoint: "10.200.200.20", NodeRange: "10.200.200.30-10.200.200.49", MetalLBRange: "10.200.200.200-10.200.200.220"}},
		}}
		mut(&in.Spec)
		return in
	}
	apply := func(preset string, in *Installation) ClusterSpec {
		t.Helper()
		if err := ApplyPreset(preset, &in.Spec, true); err != nil {
			t.Fatalf("%s: %v", preset, err)
		}
		in.Normalize()
		if err := in.Validate(); err != nil {
			t.Fatalf("%s: the filled spec is refused: %v", preset, err)
		}
		return in.Spec.Clusters[0]
	}

	// Homelab on one node: one control plane machine, one small machine per group, a network the node routes.
	in := bare(func(*Spec) {})
	c := apply(PresetHomelab, in)
	if !reflect.DeepEqual(c.ControlPlane, Machine{Node: "pve1", Replicas: 1, Cores: 2, MemoryMiB: 4096, DiskGiB: 30}) {
		t.Errorf("homelab control plane: %+v", c.ControlPlane)
	}
	for i, name := range NodeGroupNames {
		if want := (NodeGroup{Name: name, Nodes: []string{"pve1"}, Count: 1, Cores: 2, MemoryMiB: 4096, DiskGiB: 40}); !reflect.DeepEqual(c.NodeGroups[i], want) {
			t.Errorf("homelab group: %+v, want %+v", c.NodeGroups[i], want)
		}
	}
	if in.Spec.Seed.Cores != 2 || in.Spec.Seed.MemoryMiB != 4096 || in.Spec.Seed.DiskGiB != 30 {
		t.Errorf("homelab seed: %+v", in.Spec.Seed)
	}
	if in.Spec.Network.Mode != NetworkRouted || in.Spec.Network.Bridge != "pcplat0" {
		t.Errorf("homelab network on one node: %+v", in.Spec.Network)
	}

	// A bridge the caller names stays a bridge; so does a site with machines on a second node.
	if in := bare(func(s *Spec) { s.Network.Bridge = "vmbr1" }); apply(PresetHomelab, in).Name == "" || in.Spec.Network.Mode != "" || in.Spec.Network.Bridge != "vmbr1" {
		t.Errorf("homelab with a bridge: %+v", in.Spec.Network)
	}
	in = bare(func(s *Spec) {
		s.Network.Bridge = "vmbr1"
		s.Clusters[0].NodeGroups = []NodeGroup{{Name: "dbaas", Nodes: []string{"pve1", "pve2"}}}
	})
	if c := apply(PresetHomelab, in); c.NodeGroups[0].Count != 2 || in.Spec.Network.Mode != "" || len(c.NodeGroups) != 3 {
		t.Errorf("homelab on two nodes: one machine per node, no routed network: %+v %+v", c.NodeGroups, in.Spec.Network)
	}
	// No bridge named and a group on another node: not every machine is on one node, so no network is chosen (the
	// spec is then not complete, so Validate is not asked).
	in = bare(func(s *Spec) { s.Clusters[0].NodeGroups = []NodeGroup{{Name: "dbaas", Nodes: []string{"pve2"}}} })
	if err := ApplyPreset(PresetHomelab, &in.Spec, true); err != nil || in.Spec.Network.Mode != "" || in.Spec.Network.Bridge != "" {
		t.Errorf("homelab with a group on another node chose a network: %+v %v", in.Spec.Network, err)
	}

	// Standard: three control plane machines, two machines per group, or one per node when a group has more nodes.
	in = bare(func(s *Spec) {
		s.Network.Bridge = "vmbr1"
		s.Clusters[0].NodeGroups = []NodeGroup{{Name: "kamaji", Nodes: []string{"pve1", "pve2", "pve3"}}, {Name: "system", Cores: 8}}
	})
	c = apply(PresetStandard, in)
	if c.ControlPlane.Replicas != 3 || c.ControlPlane.Node != "pve1" || in.Spec.Seed.MemoryMiB != 6144 || in.Spec.Network.Mode != "" {
		t.Errorf("standard: %+v %+v %+v", c.ControlPlane, in.Spec.Seed, in.Spec.Network)
	}
	counts := map[string][2]int{}
	for _, g := range c.NodeGroups {
		counts[g.Name] = [2]int{g.Count, g.Cores}
	}
	if want := map[string][2]int{"kamaji": {3, 4}, "system": {2, 8}, "dbaas": {2, 4}}; !reflect.DeepEqual(counts, want) {
		t.Errorf("standard groups (count, cores): %v, want %v", counts, want)
	}

	// Under a release whose chart builds one control plane machine, standard fills one; a count the caller gives stays.
	for given, want := range map[int]int{0: 1, 3: 3} {
		in = bare(func(s *Spec) { s.Network.Bridge, s.Clusters[0].ControlPlane.Replicas = "vmbr1", given })
		if err := ApplyPreset(PresetStandard, &in.Spec, false); err != nil || in.Spec.Clusters[0].ControlPlane.Replicas != want || in.Spec.Clusters[0].NodeGroups[0].Count != 2 {
			t.Errorf("standard where the release builds one, %d given: %+v %v", given, in.Spec.Clusters[0], err)
		}
	}

	// What the caller gives stays; custom and no preset fill nothing; an unknown preset is refused.
	in = bare(func(s *Spec) {
		s.Network.Bridge = "vmbr1"
		s.Clusters[0].ControlPlane = Machine{Node: "pve2", Replicas: 1, Cores: 8}
	})
	if c := apply(PresetStandard, in); c.ControlPlane.Replicas != 1 || c.ControlPlane.Node != "pve2" || c.ControlPlane.Cores != 8 || c.NodeGroups[0].Nodes[0] != "pve2" {
		t.Errorf("standard over given values: %+v", c)
	}
	for _, preset := range []string{"", PresetCustom} {
		in, before := bare(func(*Spec) {}), bare(func(*Spec) {})
		if err := ApplyPreset(preset, &in.Spec, true); err != nil || !reflect.DeepEqual(in, before) {
			t.Errorf("preset %q changed the spec: %v", preset, err)
		}
	}
	if err := ApplyPreset("production", &bare(func(*Spec) {}).Spec, true); err == nil || !strings.Contains(err.Error(), "homelab, standard or custom") {
		t.Errorf("an unknown preset: %v", err)
	}
}

// The cluster values name the count only above one and only for a chart that reads it: an older chart would build one
// machine and say nothing, so the render is refused. With one machine the values hold no such key under any BOM, which
// keeps the goldens of every BOM as they are.
func TestClusterValuesControlPlaneReplicas(t *testing.T) {
	kubeadm := func(b *BOM, n int) (map[string]any, error) {
		in := renderInput()
		in.Cluster.ControlPlaneReplicas = n
		v, err := ClusterValues(b, in)
		if err != nil {
			return nil, err
		}
		return v["controlPlane"].(map[string]any)["kubeadm"].(map[string]any), nil
	}
	withChart := func(version string) *BOM { return bomWithClusterChart(t, version) }
	for _, version := range []string{"0.1.13", "0.1.14"} {
		for _, n := range []int{0, 1} {
			k, err := kubeadm(withChart(version), n)
			if _, has := k["replicas"]; err != nil || has {
				t.Errorf("chart %s with %d: %v %v", version, n, k, err)
			}
		}
	}
	if k, err := kubeadm(withChart("0.1.14"), 3); err != nil || k["replicas"] != 3 || k["machineImageId"] != "pve1=9100" {
		t.Errorf("chart 0.1.14 with 3: %v %v", k, err)
	}
	if _, err := kubeadm(withChart("0.1.13"), 3); err == nil || !strings.Contains(err.Error(), "names cluster chart 0.1.13, which builds one control plane machine") {
		t.Errorf("chart 0.1.13 with 3 must be refused: %v", err)
	}
	prev, err := Previous()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range prev {
		// A previous BOM answers by its own cluster chart: the ones from before 0.1.14 build one machine.
		chart := ClusterChartVersion(p.BOM)
		_, err := kubeadm(p.BOM, 3)
		if builds := kamaji.ChartHasControlPlaneReplicas(chart); builds == (err != nil) {
			t.Errorf("the BOM of release %s (chart %s): builds three = %v, but kubeadm(3) answered %v", p.Release, chart, builds, err)
		}
	}
}

// bomWithClusterChart is this release's BOM with the cluster chart at version.
func bomWithClusterChart(t *testing.T, version string) *BOM {
	t.Helper()
	b := *embedded(t)
	b.Components = slices.Clone(b.Components)
	for i := range b.Components {
		if b.Components[i].Name == ClusterName {
			b.Components[i].Chart.Version = version
		}
	}
	return &b
}

// Three control plane machines under this release's BOM with the cluster chart that builds them (0.1.14, the chart in
// this tree and the one the BOM pins): the values as a golden, and through the chart one KubeadmControlPlane of
// three replicas on the one pinned template. No previous BOM has such a golden: three are refused there.
func TestRenderGoldenThreeControlPlanes(t *testing.T) {
	b := bomWithClusterChart(t, "0.1.14")
	in := renderInput()
	in.Cluster.ControlPlaneReplicas = 3
	values, err := ClusterValues(b, in)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "render-cluster-values-3cp.golden.yaml", yamlDoc(t, values))

	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	f := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(f, yamlDoc(t, values), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(helm, "template", ClusterName, "../../deploy/charts/proxmox-kamaji-cluster", "-n", ClusterName, "-f", f).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var replicas, allowed any
	for _, o := range yamlObjects(t, out) {
		spec, _ := o["spec"].(map[string]any)
		name, _ := o["metadata"].(map[string]any)["name"].(string)
		switch {
		case o["kind"] == "KubeadmControlPlane":
			replicas = spec["replicas"]
		case o["kind"] == "ProxmoxMachineTemplate" && strings.HasPrefix(name, ClusterName+"-cp-"):
			allowed = spec["template"].(map[string]any)["spec"].(map[string]any)["allowedNodes"]
		}
	}
	if replicas != 3 || !reflect.DeepEqual(allowed, []any{"pve1"}) {
		t.Errorf("control plane replicas %v on %v, want 3 on [pve1]", replicas, allowed)
	}
}

// chartBuildsReplicas: the embedded BOM's cluster chart reads controlPlane.kubeadm.replicas.
func chartBuildsReplicas(t *testing.T) bool {
	in := renderInput()
	in.Cluster.ControlPlaneReplicas = 3
	_, err := ClusterValues(embedded(t), in)
	return err == nil
}

// The memory the settings place on each node: the seed, count + 1 control plane machines and each group's share of
// its machines, the first nodes taking the remainder as the chart splits machineCount; and, apart, one more machine of
// each group on each of its nodes.
func TestPlannedMemory(t *testing.T) {
	in := validInstallation(t) // 4 GiB machines: seed and control plane on pve1, one group machine on each node
	need, spare := in.PlannedMemoryMiB()
	if want := map[string]int{"pve1": 4096 * 4, "pve2": 4096, "pve3": 4096}; !reflect.DeepEqual(need, want) {
		t.Errorf("one control plane: %v, want %v", need, want)
	}
	if want := map[string]int{"pve1": 4096, "pve2": 4096, "pve3": 4096}; !reflect.DeepEqual(spare, want) {
		t.Errorf("one control plane, spares: %v, want %v", spare, want)
	}
	c := &in.Spec.Clusters[0]
	c.ControlPlane.Replicas = 3
	c.NodeGroups[2] = NodeGroup{Name: "dbaas", Nodes: []string{"pve3", "pve2"}, Count: 3, Cores: 2, MemoryMiB: 8192, DiskGiB: 30}
	// pve1: seed 4, control plane (3+1)*4, system 4. pve2: kamaji 4, dbaas 8. pve3: dbaas 2*8.
	need, spare = in.PlannedMemoryMiB()
	if want := map[string]int{"pve1": 4096 * 6, "pve2": 4096 + 8192, "pve3": 8192 * 2}; !reflect.DeepEqual(need, want) {
		t.Errorf("three control planes: %v, want %v", need, want)
	}
	if want := map[string]int{"pve1": 4096, "pve2": 4096 + 8192, "pve3": 8192}; !reflect.DeepEqual(spare, want) {
		t.Errorf("three control planes, spares: %v, want %v", spare, want)
	}
}

// Lab round 2, G2F-2: on an installed platform a control plane count above the one a run built is a change of the
// plan, and where the node has no room for it the capacity row fails and the plan blocks, with the numbers. What a run
// already placed only warns and blocks nothing, however full the node is.
func TestControlPlaneGrowth(t *testing.T) {
	const gi = 1 << 30
	cur, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	// Seed 4, three groups of one = 16 GiB on pve1, and the control plane's machines and one more, 4 GiB each.
	run := func(built, asked, nodeGiB int) (Plan, Check, []string) {
		t.Helper()
		in := validInstallation(t)
		in.ID = "pi-0a1b2c3d"
		for i := range in.Spec.Clusters[0].NodeGroups {
			in.Spec.Clusters[0].NodeGroups[i].Nodes = []string{"pve1"}
		}
		in.Spec.Clusters[0].ControlPlane.Replicas = asked
		in.Status = Status{BOM: &BOMRef{Release: cur.Release, SHA256: cur.SHA256}, Clusters: []ClusterStatus{{Name: ClusterName}}}
		if built > 1 {
			in.Status.Clusters[0].ControlPlaneReplicas = built
		}
		f := &preflightPVE{extra: map[string]string{"/nodes": `[{"node":"pve1","status":"online","maxmem":` + strconv.Itoa(nodeGiB*gi) + `}]`, "/cluster/resources": `[]`}}
		srv := httptest.NewServer(http.HandlerFunc(f.serve))
		defer srv.Close()
		pf := Preflight{Installation: in, CAPI: f.client(t, srv.URL, pve.CAPITokenID)}
		plan, err := PlanFor(in, "")
		if err != nil {
			t.Fatal(err)
		}
		return plan, pf.capacity(context.Background()), pf.GrowthBlocking(context.Background())
	}

	// 1 -> 3 with room for four (32 GiB): listed, not blocked. The groups' spares do not fit, which stays a warning.
	plan, row, blocking := run(1, 3, 32)
	if len(plan.Changes) != 1 || len(plan.Blocking) != 0 || len(blocking) != 0 || row.Status != CheckWarn {
		t.Fatalf("1 -> 3 that fits: plan %+v, row %+v, blocking %v", plan, row, blocking)
	}
	if c := plan.Changes[0]; c.Cluster != ClusterName || c.Name != "controlPlane.replicas" || c.From != "1" || c.To != "3" || c.Stage != stageCluster ||
		c.Impact != "The apply adds 2 control plane machines on node pve1." || c.Blocking != "" {
		t.Fatalf("the change of 1 -> 3: %+v", c)
	}
	// 1 -> 3 with room for three and not four: listed, and blocked with the numbers.
	plan, row, blocking = run(1, 3, 31)
	if len(plan.Changes) != 1 || plan.Changes[0].To != "3" || row.Status != CheckFail || len(blocking) != 1 || blocking[0] != row.Message {
		t.Fatalf("1 -> 3 that does not fit: plan %+v, row %+v, blocking %v", plan, row, blocking)
	}
	for _, part := range []string{"Node pve1: the settings place 32 GiB of memory there, and 31 GiB can be reserved (31 GiB in the node, less 0 GiB configured",
		"The settings take the control plane of pc-platform from 1 to 3 machines there"} {
		if !strings.Contains(row.Message, part) {
			t.Errorf("no %q in the failed row: %s", part, row.Message)
		}
	}
	// Unchanged and over capacity (three run, or one runs, on a node that lost its room): a warning, no change, no block.
	for _, n := range []int{1, 3} {
		plan, row, blocking = run(n, n, 16)
		if len(plan.Changes) != 0 || len(plan.Blocking) != 0 || len(blocking) != 0 || row.Status != CheckWarn || strings.Contains(row.Message, "from 1 to 3") {
			t.Fatalf("%d of %d over capacity: plan %+v, row %+v, blocking %v", n, n, plan, row, blocking)
		}
	}
	// A 1 saved over a 3 that a run built is refused at the save (ControlPlaneShrink); a record that has it is no growth.
	if plan, _, blocking = run(3, 1, 16); len(plan.Changes) != 0 || len(blocking) != 0 {
		t.Fatalf("3 built, 1 asked: plan %+v, blocking %v", plan, blocking)
	}
	// A new installation lists its components, not a count, and no token means no read.
	in := validInstallation(t)
	in.Spec.Clusters[0].ControlPlane.Replicas = 3
	if plan, err := PlanFor(in, ""); err != nil || len(plan.Changes) != len(cur.Components) {
		t.Fatalf("a new installation with three: %+v %v", plan, err)
	}
	if b := (Preflight{Installation: in}).GrowthBlocking(context.Background()); b != nil {
		t.Fatalf("without the CAPI token: %v", b)
	}
}

// The capacity row counts as CAPMOX does before a clone: the node's memory less the configured memory of every VM and
// container on it that is not a template, running or not, and not this installation's own. Three control plane
// machines need room for four. Before the first install a node without room fails; after it, and when the nodes
// cannot be read, the row warns; and the spec row refuses three under a BOM whose chart builds one.
func TestPreflightCapacity(t *testing.T) {
	const gi = 1 << 30
	run := func(mut func(*Installation), nodes, vms string) map[string]Check {
		in := validInstallation(t)
		in.ID = "pi-0a1b2c3d"
		for i := range in.Spec.Clusters[0].NodeGroups {
			in.Spec.Clusters[0].NodeGroups[i].Nodes = []string{"pve1"}
		}
		mut(in)
		f := &preflightPVE{extra: map[string]string{"/nodes": nodes, "/cluster/resources": vms}}
		srv := httptest.NewServer(http.HandlerFunc(f.serve))
		defer srv.Close()
		pf := Preflight{Installation: in, Site: Site{CIDRPool: "10.100.0.0/16"},
			Portal: &pve.Service{C: f.client(t, srv.URL, pve.DefaultPlatformTokenID), ServiceID: "svc-pve"}, CAPI: f.client(t, srv.URL, pve.CAPITokenID)}
		rows := checkMap(pf.Run(context.Background()))
		if len(f.writes) != 0 {
			t.Errorf("the Check wrote to PVE: %v", f.writes)
		}
		return rows
	}
	want := func(name string, c Check, status string, parts ...string) {
		t.Helper()
		if c.Status != status {
			t.Errorf("%s: %s %q, want %s: %s", name, c.ID, c.Status, status, c.Message)
		}
		for _, p := range parts {
			if !strings.Contains(c.Message, p) {
				t.Errorf("%s: no %q in %s: %s", name, p, c.ID, c.Message)
			}
		}
	}
	three := func(in *Installation) { in.Spec.Clusters[0].ControlPlane.Replicas = 3 }
	node := func(gib int) string {
		return `[{"node":"pve1","status":"online","maxmem":` + strconv.Itoa(gib*gi) + `}]`
	}
	// On the node: a tenant VM of 8 GiB that is stopped, a container of 4, a template (not counted), and two of this
	// installation's own machines (part of the plan, not counted twice).
	others := `[{"type":"qemu","node":"pve1","vmid":200,"status":"stopped","maxmem":` + strconv.Itoa(8*gi) + `},
		{"type":"lxc","node":"pve1","vmid":201,"status":"running","maxmem":` + strconv.Itoa(4*gi) + `},
		{"type":"qemu","node":"pve1","vmid":9100,"template":1,"maxmem":` + strconv.Itoa(64*gi) + `},
		{"type":"qemu","node":"pve1","vmid":103,"status":"running","tags":"pc-platform;pc-platform-pi-0a1b2c3d;proxcloud","maxmem":` + strconv.Itoa(4*gi) + `},
		{"type":"qemu","node":"pve2","vmid":300,"status":"running","maxmem":` + strconv.Itoa(64*gi) + `}]`

	// One control plane: seed 4, control plane 2*4, three groups of one = 24 GiB, and 12 more for the groups' spares.
	// 48 in the node less 12 = 36: all of it fits.
	want("one, exactly", run(func(*Installation) {}, node(48), others)["capacity"], CheckOK, "pve1: 36 GiB of 36 GiB")
	// 36 in the node less 12 = 24: the machines and the control plane's spare fit, the groups' spares do not. The
	// smallest preset on a small node installs, with a warning.
	want("one, without room for the groups' spares", run(func(*Installation) {}, node(36), others)["capacity"], CheckWarn,
		"Node pve1 holds the machines (24 GiB of 24 GiB), and not the spare machine of every group on it as well (12 GiB more)")
	want("one, without room for the control plane's spare", run(func(*Installation) {}, node(35), others)["capacity"], CheckFail,
		"Node pve1: the settings place 24 GiB of memory there, and 23 GiB can be reserved")
	// Three: 24 + 2*4 = 32 GiB.
	rows := run(three, node(43), others)
	want("three on a node with room for three, not four", rows["capacity"], CheckFail,
		"Node pve1: the settings place 32 GiB of memory there, and 31 GiB can be reserved (43 GiB in the node, less 12 GiB configured for the other VMs and containers on it).",
		"refused and not tried again", "rolling update of the control plane")
	if chartBuildsReplicas(t) {
		want("three under this BOM", rows["spec"], CheckOK)
	}
	want("three with room for four", run(three, node(44), others)["capacity"], CheckWarn, "Node pve1 holds the machines (32 GiB of 32 GiB)")
	want("three with room for four and the groups' spares", run(three, node(56), others)["capacity"], CheckOK, "pve1: 44 GiB of 44 GiB")
	// An installed platform is not stopped by the row, and neither is a site whose nodes cannot be read.
	cur, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	installed := func(in *Installation) { three(in); in.Status.BOM = &BOMRef{Release: cur.Release, SHA256: cur.SHA256} }
	want("installed", run(installed, node(43), others)["capacity"], CheckWarn, "Node pve1: the settings place 32 GiB")
	want("no node list", run(three, "!", others)["capacity"], CheckWarn, "Could not read the nodes' memory with capmox@pve!capi")
	want("a node that reports nothing", run(three, `[{"node":"pve1","status":"offline"}]`, others)["capacity"], CheckWarn, "Node pve1 did not report its memory")

	// Under a BOM whose chart builds one machine, three are a failed spec row.
	// The BOM of 0.8.0, by name: the first file under previous/ is no longer the oldest release (0.10.0-rc.1 sorts
	// before 0.8.0), and its chart builds three.
	old080, ok := Lookup(previousSHA256["0.8.0"])
	if !ok {
		t.Fatal("this build does not embed the 0.8.0 BOM")
	}
	old := func(in *Installation) {
		three(in)
		in.Status.BOM = &BOMRef{Release: old080.Release, SHA256: old080.SHA256}
	}
	want("three on a previous BOM", run(old, node(56), others)["spec"], CheckFail, "3 control plane machines", "which builds one", "0.1.14")
}

// From 1 to 3 on a built platform is an apply that adds machines: the cluster stage writes the count into the cluster
// chart's values and waits until three control plane machines run, not one. Under a BOM whose cluster chart builds one
// (0.1.13 and older; this release's pins 0.1.14) the same apply stops with the reason, and nothing is written.
func TestClusterStageWaitsForThreeControlPlanes(t *testing.T) {
	three := func(_ *Run, in *Installation, _ *Secrets) { in.Spec.Clusters[0].ControlPlane.Replicas = 3 }
	if !chartBuildsReplicas(t) {
		p := newPlatform(t, RunApply)
		p.only(stageCluster)
		p.mutate(three)
		if end, s := p.once(), p.stage(stageCluster); !strings.HasPrefix(end, "fail:") || !strings.Contains(s.Message, "which builds one control plane machine: 3 need chart 0.1.14 or newer") {
			t.Fatalf("three under a chart that builds one: end %s, message %q", end, s.Message)
		}
		if _, in, _ := p.state.snapshot(); len(in.Status.Clusters) == 1 && in.Status.Clusters[0].ControlPlaneReplicas != 0 || ControlPlaneShrink(in, validInstallation(t)) != "" {
			t.Errorf("a run that built nothing recorded three, so the count could not go back to one: %+v", in.Status.Clusters)
		}
	}
	withClusterChart(t, "0.1.14")
	p := newPlatform(t, RunApply)
	p.only(stageCluster)
	p.mutate(three)
	if end, s := p.once(), p.stage(stageCluster); end != "defer:waiting_cluster" || !strings.Contains(s.Message, "4 of 6 machines are Running (4 exist)") {
		t.Fatalf("one control plane machine of three: end %s, message %q", end, s.Message)
	}
	// Recorded as soon as the count went to the cluster, while two machines are still missing: from here it stays three.
	if _, in, _ := p.state.snapshot(); len(in.Status.Clusters) != 1 || in.Status.Clusters[0].ControlPlaneReplicas != 3 || !strings.Contains(ControlPlaneShrink(in, validInstallation(t)), "does not go from 3 machines to 1") {
		t.Errorf("the status does not say three were handed to the cluster: %+v", in.Status.Clusters)
	}
	values := dig(p.seed.obj(appsPath+ClusterName), "spec", "source", "helm", "valuesObject", "controlPlane", "kubeadm", "replicas")
	if values != 3 && values != int64(3) && values != float64(3) {
		t.Errorf("the Application's values name %v control plane machines, want 3", values)
	}
	for _, name := range []string{"pc-platform-cp-y", "pc-platform-cp-z"} {
		m := meta(name, map[string]any{"cluster.x-k8s.io/cluster-name": ClusterName})
		m["spec"], m["status"] = map[string]any{"infrastructureRef": map[string]any{"name": name + "-infra"}}, map[string]any{"phase": "Running"}
		p.seed.put(capiNS+"machines/"+name, m)
	}
	if end := p.once(); end != "complete" {
		t.Fatalf("three control plane machines run: end %s %+v", end, p.stage(stageCluster))
	}
}

package platformlife

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// machineFacts renders the cluster chart with a values golden and answers what decides whether Cluster API replaces
// a machine, and whether the run's check before the sync (rolloutGate) lets the chart through: the whole
// KubeadmControlPlane and every machine and bootstrap template as rendered, metadata included (the check fails on any
// of them that ArgoCD does not report Synced, a label or an annotation too), and every MachineDeployment's machine
// template. It must stay at least as strict as rolloutGate: a pair that passes here and fails there leaves every
// upgraded platform with an unfinished upgrade.
func machineFacts(t *testing.T, helm, chart, values string) map[string]any {
	t.Helper()
	// Without nodeDeletionTimeoutSeconds (chart 0.1.16; an older chart does not read the value): the check before the
	// sync compares without it as long as the live Application renders none (gateRender), and
	// TestNodeDeletionTimeoutChangesNoMachine shows that the value changes nothing else.
	out, err := exec.Command(helm, "template", ClusterName, chart, "-n", ClusterName, "-f", values, "--set", "nodeDeletionTimeoutSeconds=-1").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %s -f %s: %v\n%s", chart, values, err, out)
	}
	facts := map[string]any{}
	var templates []string
	for _, o := range yamlObjects(t, out) {
		kind, _ := o["kind"].(string)
		name, _ := o["metadata"].(map[string]any)["name"].(string)
		spec, _ := o["spec"].(map[string]any)
		switch kind {
		case "KubeadmControlPlane":
			facts[kind+" "+name] = o
		case "MachineDeployment":
			facts[kind+" "+name] = spec["template"]
		case "ProxmoxMachineTemplate", "KubeadmConfigTemplate":
			templates = append(templates, kind+" "+name)
			facts[kind+" "+name] = o
		}
	}
	if len(templates) == 0 || len(facts) < len(templates)+2 {
		t.Fatalf("%s renders no control plane, machine deployment or template", chart)
	}
	return facts
}

// clusterChart is the cluster chart at the version a BOM pins: the chart in this tree when that is its version, else
// the published one, by the BOM's digest (pinnedChart: from the registry once, then from the test chart cache).
func clusterChart(t *testing.T, helm string, c Chart) string {
	t.Helper()
	const local = "../../deploy/charts/proxmox-kamaji-cluster"
	if out, err := exec.Command(helm, "show", "chart", local).Output(); err == nil {
		if objs := yamlObjects(t, out); len(objs) == 1 && objs[0]["version"] == c.Version {
			return local
		}
	}
	return pinnedChart(t, c)
}

// The check a release makes of its BOM pairs (docs/ci-release.md, "What a release checks about its BOM pairs"): for
// every previous BOM the classifier lets a platform move from, the cluster chart of this build's BOM, rendered with
// the values this build renders for it, replaces no machine of a platform that the previous release rendered with its
// chart and its values (the goldens of TestRenderGoldenOfPreviousBOMs). The values come from Go, not only from the
// BOM, so a renderer change is caught here as well as a chart change. A pair that fails must not be allowed: the new
// chart version must not list the old one in upgradeFrom, and the platform then stays on its BOM until a release
// rolls machines.
//
// A pair that replaces machines by design (class machines: another Kubernetes version or node image) is held to the
// same comparison at the point where it must hold: this build's chart with the values a run renders before the roll
// begins (step 0: the previous BOM's version, kube-vip and templates) renders exactly what the previous release
// rendered. The run applies the new chart version with step 1 and its gate lets only that step's unit differ, so a
// chart or renderer that changed any other machine object would fail every such upgrade at step 1 with
// unexpected_rollout. The steps after 0 are TestRollRendersOneUnitAtATime's.
func TestUpgradeRollsNoMachine(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	cur := embedded(t)
	prev, err := Previous()
	if err != nil {
		t.Fatal(err)
	}
	chart := clusterChart(t, helm, comp(t, cur, ClusterSeed, ClusterName).Chart)
	plain := machineFacts(t, helm, chart, "testdata/render-cluster-values.golden.yaml")
	checked, rolled := 0, 0
	for _, b := range prev {
		changes := Classify(b.BOM, cur)
		if slices.ContainsFunc(changes, func(c PlanChange) bool { return c.Blocking != "" }) {
			t.Logf("%s: no upgrade from it to %s, so nothing to compare", b.Release, cur.Release)
			continue
		}
		checked++
		want := plain
		if hasMachines(changes) {
			rolled++
			in := renderInput() // the input of both goldens
			in.Cluster.Roll = &Roll{FromVersion: b.Kubernetes.Version, FromKubeVip: b.Images.KubeVip, FromTemplates: in.Templates}
			v, err := ClusterValues(cur, in)
			if err != nil {
				t.Fatalf("%s: step 0 of the roll to %s: %v", b.Release, cur.Release, err)
			}
			values := filepath.Join(t.TempDir(), "step0.yaml")
			if err := os.WriteFile(values, yamlDoc(t, v), 0o600); err != nil {
				t.Fatal(err)
			}
			want = machineFacts(t, helm, chart, values)
			if reflect.DeepEqual(want, plain) {
				t.Errorf("%s to %s is class machines, and step 0 renders the machines of the target: the roll would order nothing", b.Release, cur.Release)
			}
		}
		got := machineFacts(t, helm, clusterChart(t, helm, comp(t, b.BOM, ClusterSeed, ClusterName).Chart), filepath.Join("testdata", "previous", b.Release, "render-cluster-values.golden.yaml"))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("an upgrade from %s to %s would replace machines, or fail the run's rollout check: the cluster chart renders another control plane, machine template or bootstrap template (metadata included).\n"+
				"%s: %v\n%s: %v", b.Release, cur.Release, b.Release, got, cur.Release, want)
		}
	}
	// Today every previous BOM differs from main's in the release line and the product's own charts. When a BOM change
	// refuses them all, the check has nothing left to say, which is correct; it must not go unnoticed while pairs are
	// allowed.
	t.Logf("%d of %d previous BOMs compared, %d of them at step 0 of a roll", checked, len(prev), rolled)
}

// The cluster chart in this tree, when it is a version ahead of the one this build's BOM pins (bumped in a pull
// request, tagged and pinned after it), renders the same machines as the pinned one from the same values: the pin
// that follows starts no rollout on a platform, and the run's rollout check lets it through. Every values fixture: this
// build's (one and three control plane machines) and those of every previous release.
func TestChartInTreeRollsNoMachine(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	const local = "../../deploy/charts/proxmox-kamaji-cluster"
	pinned := clusterChart(t, helm, comp(t, embedded(t), ClusterSeed, ClusterName).Chart)
	if pinned == local {
		t.Skip("the BOM pins the chart in this tree")
	}
	previous, err := filepath.Glob("testdata/previous/*/render-cluster-values.golden.yaml")
	if err != nil || len(previous) == 0 {
		t.Fatalf("previous values fixtures: %v", err)
	}
	for _, values := range append([]string{"testdata/render-cluster-values.golden.yaml", "testdata/render-cluster-values-3cp.golden.yaml"}, previous...) {
		if got, want := machineFacts(t, helm, local, values), machineFacts(t, helm, pinned, values); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the chart in this tree renders another control plane, machine template or bootstrap template than the pinned one.\nin tree: %v\npinned: %v", values, got, want)
		}
	}
}

// nodeDeletionTimeoutSeconds (chart 0.1.16) is the one thing the run's check before the sync does not compare while
// the live Application renders none (gateRender), so the chart is held to what that relies on: the value is
// machineTemplate.spec.deletion of the KubeadmControlPlane and template.spec.deletion of every MachineDeployment and
// nothing else. Taken out of the default render, every object is the one a negative value renders, which
// TestChartInTreeRollsNoMachine and TestUpgradeRollsNoMachine compare with the charts before it. Cluster API copies
// that field to the Machines in place and leaves it out of its rollout decision (the chart's templates cite v1.14.4).
func TestNodeDeletionTimeoutChangesNoMachine(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	render := func(set ...string) []map[string]any {
		args := append([]string{"template", ClusterName, "../../deploy/charts/proxmox-kamaji-cluster", "-n", ClusterName, "-f", "testdata/render-cluster-values.golden.yaml"}, set...)
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("helm %v: %v\n%s", args, err, out)
		}
		return yamlObjects(t, out)
	}
	with, without := render(), render("--set", "nodeDeletionTimeoutSeconds=-1")
	if len(with) != len(without) {
		t.Fatalf("%d objects with the timeout, %d without", len(with), len(without))
	}
	want := map[string]any{"nodeDeletionTimeoutSeconds": 1800}
	found := map[string]int{}
	for i, o := range with {
		var spec map[string]any
		switch o["kind"] {
		case "KubeadmControlPlane":
			spec, _ = dig(o, "spec", "machineTemplate", "spec").(map[string]any)
		case "MachineDeployment":
			spec, _ = dig(o, "spec", "template", "spec").(map[string]any)
		}
		if spec != nil {
			if !sameJSON(spec["deletion"], want) {
				t.Errorf("%v %s: deletion %v, want %v", o["kind"], str(o, "metadata", "name"), spec["deletion"], want)
			}
			delete(spec, "deletion")
			found[o["kind"].(string)]++
		}
		if !sameJSON(o, without[i]) {
			t.Errorf("%v %s differs in more than the node deletion timeout:\nwith:    %s\nwithout: %s", o["kind"], str(o, "metadata", "name"), jsonOf(o), jsonOf(without[i]))
		}
	}
	if found["KubeadmControlPlane"] != 1 || found["MachineDeployment"] < 3 {
		t.Fatalf("the render has %v", found)
	}
}

// certManagerReads are the cert-manager moves a release read: upstream's upgrade notes for the pair, and the render of
// TestCertManagerMoveRenders. removed and added are what the new chart stops and starts rendering with the run's values
// (ArgoCD prunes the removed: cert-manager is not stateful). A BOM pair whose cert-manager row is class chart must be
// one of them.
var certManagerReads = []certManagerRead{
	// website content/docs/releases/upgrading/upgrading-1.20-1.21.md lines 8-19: the chart no longer creates the
	// controller's tokenrequest Role and RoleBinding. Its other two items (Challenge and Order verbs of
	// cert-manager-edit, the removed ServiceMonitor and PodMonitor values) touch nothing the product sets or uses.
	{"v1.20.2", "v1.21.3", []string{"rbac.authorization.k8s.io/v1 Role cert-manager/cert-manager-tokenrequest",
		"rbac.authorization.k8s.io/v1 RoleBinding cert-manager/cert-manager-tokenrequest"}, nil},
}

type certManagerRead struct {
	from, to       string
	removed, added []string
}

// certManagerUpstream is the index digest (the sha256 of the archive) of a version no BOM pins yet: the test takes it
// from upstream, where the chart mirror takes it (deploy/platform/charts-mirror.yaml), until a BOM pins it from the mirror.
// Empty while every version of certManagerReads is pinned: both come from the mirror by the BOM's digest.
var certManagerUpstream = map[string]string{}

// The check a release makes of a cert-manager move, as TestUpgradeRollsNoMachine does of the cluster chart: every
// cert-manager row a BOM pair allows is a move the release read (certManagerReads), and each read move renders with the
// values a run gives the seed's and the platform cluster's Application at the BOM's Kubernetes version (the chart's
// values schema and kubeVersion pass), keeps every CRD at the one version v1, served and stored, and stops and starts
// rendering exactly the objects the release named. Render only: ArgoCD's server-side apply of the CRDs, the
// startupapicheck Job and the rolling update are what the lab drill shows.
func TestCertManagerMoveRenders(t *testing.T) {
	cur := embedded(t)
	prev, err := Previous()
	if err != nil {
		t.Fatal(err)
	}
	boms := []*BOM{cur}
	for _, b := range prev {
		boms = append(boms, b.BOM)
		for _, c := range Classify(b.BOM, cur) {
			if c.Name == "cert-manager" && c.Class == ClassChart && !slices.ContainsFunc(certManagerReads, func(m certManagerRead) bool { return m.from == c.From && m.to == c.To }) {
				t.Errorf("%s to %s moves %s/cert-manager from %s to %s, which no release read: add the move to certManagerReads", b.Release, cur.Release, c.Cluster, c.From, c.To)
			}
		}
	}
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	apps, err := Applications(cur.For(Spec{}), renderInput())
	if err != nil {
		t.Fatal(err)
	}
	var values []string // one file per cert-manager Application: the seed's and the platform cluster's
	for _, a := range apps {
		if str(a, "spec", "source", "chart") == "cert-manager" {
			f := filepath.Join(t.TempDir(), str(a, "metadata", "name")+".yaml")
			if err := os.WriteFile(f, yamlDoc(t, dig(a, "spec", "source", "helm", "valuesObject")), 0o600); err != nil {
				t.Fatal(err)
			}
			values = append(values, f)
		}
	}
	if len(values) != 2 {
		t.Fatalf("%d cert-manager Applications, want the seed's and the platform cluster's", len(values))
	}
	for _, m := range certManagerReads {
		from, to := certManagerChart(t, helm, boms, m.from), certManagerChart(t, helm, boms, m.to)
		for _, v := range values {
			old, now := certManagerRender(t, helm, from, v, cur.Kubernetes.Version), certManagerRender(t, helm, to, v, cur.Kubernetes.Version)
			var removed, added []string
			for k := range old {
				if now[k] == nil {
					removed = append(removed, k)
				}
			}
			for k := range now {
				if old[k] == nil {
					added = append(added, k)
				}
			}
			slices.Sort(removed)
			slices.Sort(added)
			if !slices.Equal(removed, m.removed) || !slices.Equal(added, m.added) {
				t.Errorf("%s to %s with %s: removed %q, added %q; the release read removed %q, added %q", m.from, m.to, filepath.Base(v), removed, added, m.removed, m.added)
			}
		}
	}
}

// certManagerChart is the archive of cert-manager at version: from the chart mirror by the digest a BOM pins, else
// from upstream with its index digest in certManagerUpstream. Either way once, then from the test chart cache.
func certManagerChart(t *testing.T, helm string, boms []*BOM, version string) string {
	t.Helper()
	for _, b := range boms {
		for _, c := range b.Components {
			if c.Name == "cert-manager" && c.Chart.Version == version {
				return pinnedChart(t, c.Chart)
			}
		}
	}
	digest, ok := certManagerUpstream[version]
	if !ok {
		t.Fatalf("no BOM pins cert-manager %s, and certManagerUpstream names no digest for it", version)
	}
	path, _, err := cachedBlob(chartCacheDir(), digest, func() ([]byte, error) {
		t.Logf("chart cache: pulling cert-manager %s (%s) from upstream", version, digest)
		dir := t.TempDir()
		if out, err := exec.Command(helm, "pull", "cert-manager", "--repo", "https://charts.jetstack.io", "--version", version, "-d", dir).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("%v\n%s", err, out)
		}
		return os.ReadFile(filepath.Join(dir, "cert-manager-"+version+".tgz"))
	})
	return chartOrSkip(t, "cert-manager "+version, path, err)
}

// certManagerRender renders chart with values at Kubernetes version kube, by object (objectKey), and fails on a CRD
// that is not at the one version v1, served and stored.
func certManagerRender(t *testing.T, helm, chart, values, kube string) map[string]map[string]any {
	t.Helper()
	out, err := exec.Command(helm, "template", "cert-manager", chart, "-n", "cert-manager", "-f", values, "--kube-version", kube).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %s -f %s --kube-version %s: %v\n%s", filepath.Base(chart), filepath.Base(values), kube, err, out)
	}
	objs, crds := map[string]map[string]any{}, 0
	for _, o := range yamlObjects(t, out) {
		objs[objectKey(o)] = o
		if o["kind"] != "CustomResourceDefinition" {
			continue
		}
		crds++
		versions, _ := dig(o, "spec", "versions").([]any)
		var v map[string]any
		if len(versions) == 1 {
			v, _ = versions[0].(map[string]any)
		}
		if v["name"] != "v1" || v["served"] != true || v["storage"] != true {
			t.Errorf("%s: CRD %s has versions %v, want v1 alone, served and stored", filepath.Base(chart), str(o, "metadata", "name"), versions)
		}
	}
	if crds == 0 {
		t.Fatalf("%s renders no CRD with %s: crds.enabled is gone from the values", filepath.Base(chart), filepath.Base(values))
	}
	return objs
}

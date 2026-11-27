package platformlife

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

const (
	crdsPath      = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/"
	sandboxPool   = "/apis/ipam.cluster.x-k8s.io/v1alpha2/namespaces/pc-platform/inclusterippools/pc-platform-sandbox"
	pmtPath       = "/apis/infrastructure.cluster.x-k8s.io/v1alpha2/namespaces/pc-platform/proxmoxmachinetemplates/"
	kctPath       = "/apis/bootstrap.cluster.x-k8s.io/v1beta2/namespaces/pc-platform/kubeadmconfigtemplates/"
	sandboxMD     = capiNS + "machinedeployments/pc-platform-sandbox-pve1"
	sandboxMS     = capiNS + "machinesets/pc-platform-sandbox-pve1-7c9d4"
	sandboxOneCRD = "/apis/agents.x-k8s.io/v1beta1/namespaces/sb-0a1b2c3d/sandboxes/left"
)

// helmBin is the helm binary: required in CI, a skip elsewhere.
func helmBin(t *testing.T) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	return helm
}

// helmChart renders a chart of the tree with values, as ArgoCD renders an Application's source. A render that fails
// is the test's failure and answers nothing (it is called from the fakes' handlers too).
func helmChart(t *testing.T, helm, release, chart, namespace string, values any) []map[string]any {
	f := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(f, yamlDoc(t, values), 0o600); err != nil {
		t.Error(err)
		return nil
	}
	out, err := exec.Command(helm, "template", release, "../../deploy/charts/"+chart, "-n", namespace, "-f", f).CombinedOutput()
	if err != nil {
		t.Errorf("helm template %s: %v\n%s", chart, err, out)
		return nil
	}
	return yamlObjects(t, out)
}

// realCharts gives the fakes' ArgoCD the two charts in the tree, rendered with the values of the Application the run
// applied: the cluster chart's objects go to the seed, where the fake syncs them without prune and compares them, and
// the sandbox chart's definitions to the platform cluster (the rest of that chart is nothing a run reads). The
// definitions carry the finalizer the API server gives each one, so a delete of one is not done at once.
func (p *platform) realCharts(t *testing.T) {
	t.Helper()
	helm := helmBin(t)
	p.seed.argo = func(app map[string]any) map[string]map[string]any {
		chart, ns := str(app, "spec", "source", "chart"), str(app, "spec", "destination", "namespace")
		release := str(app, "spec", "source", "helm", "releaseName")
		if release == "" {
			release = str(app, "metadata", "name")
		}
		switch chart {
		case "proxmox-kamaji-cluster":
			out := map[string]map[string]any{}
			for _, o := range helmChart(t, helm, release, chart, ns, dig(app, "spec", "source", "helm", "valuesObject")) {
				plural := strings.ToLower(str(o, "kind")) + "s"
				if strings.HasSuffix(plural, "ys") {
					plural = strings.TrimSuffix(plural, "ys") + "ies"
				}
				base := "/apis/" + str(o, "apiVersion")
				if !strings.Contains(str(o, "apiVersion"), "/") {
					base = "/api/" + str(o, "apiVersion")
				}
				out[base+"/namespaces/"+ns+"/"+plural+"/"+str(o, "metadata", "name")] = o
			}
			return out
		case "proxcloud-sandbox":
			p.wl.mu.Lock()
			defer p.wl.mu.Unlock()
			for _, o := range helmChart(t, helm, release, chart, ns, dig(app, "spec", "source", "helm", "valuesObject")) {
				if o["kind"] == "CustomResourceDefinition" {
					o["metadata"].(map[string]any)["finalizers"] = []any{"customresourcecleanup.apiextensions.k8s.io"}
					p.wl.objs[crdsPath+str(o, "metadata", "name")] = o
				}
			}
		}
		return nil
	}
}

// builtSharedSandbox is a platform whose run built the shared group with the charts in the tree, with what Cluster
// API and the address provider hold on a real seed while the group's machine lives: the Machine with its group label,
// a MachineSet that names the two templates, the VM in the pool, and the pool's finalizer.
func builtSharedSandbox(t *testing.T) *platform {
	t.Helper()
	p := sandboxPlatform(t, RunApply, sharedSandbox())
	p.realCharts(t)
	p.upTo(stageSeedPlatform)
	if end := p.until(30); end != "complete" {
		t.Fatalf("the build: %s: %+v", end, p.stage(stageCluster))
	}
	md := p.seed.obj(sandboxMD)
	pmt, kct := str(md, "spec", "template", "spec", "infrastructureRef", "name"), str(md, "spec", "template", "spec", "bootstrap", "configRef", "name")
	if p.seed.obj(sandboxPool) == nil || p.seed.obj(pmtPath+pmt) == nil || p.seed.obj(kctPath+kct) == nil || !strings.HasPrefix(pmt, "pc-platform-sandbox-pve1-") {
		t.Fatalf("the cluster chart renders no pool or no templates for the group: %q %q", pmt, kct)
	}
	var crds []string
	for _, path := range p.wl.paths(crdsPath) {
		crds = append(crds, strings.TrimPrefix(path, crdsPath))
	}
	if !slices.Equal(crds, sandboxCRDs) {
		t.Fatalf("the sandbox chart installs the definitions %v, and an off deletes %v", crds, sandboxCRDs)
	}
	if got := p.seed.appSync(ClusterName); got != "Synced" {
		t.Fatalf("after the build %s reads %s", ClusterName, got)
	}
	m := p.seed.obj(capiNS + "machines/pc-platform-sandbox-pve1-x")
	m["metadata"].(map[string]any)["labels"].(map[string]any)["proxcloud.io/node-group"] = SandboxGroup
	ms := meta("pc-platform-sandbox-pve1-7c9d4", map[string]any{"cluster.x-k8s.io/cluster-name": ClusterName, "proxcloud.io/node-group": SandboxGroup})
	ms["spec"] = dig(md, "spec")
	p.seed.put(sandboxMS, ms)
	p.seed.obj(sandboxPool)["metadata"].(map[string]any)["finalizers"] = []any{"ipam.cluster.x-k8s.io/ProtectPool"}
	p.pve.templates = []pve.ClusterResource{{Type: "qemu", VMID: 140, Node: "pve1", Pool: pve.SandboxPool}}
	return p
}

func (p *platform) off(t *testing.T) {
	t.Helper()
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Spec.Sandbox = &SandboxSpec{Placement: SandboxOff} })
	for _, id := range []string{stageCluster, stageWorkloadPlatform} {
		p.again(id)
	}
}

// machineGone is Cluster API done with the group's machine: the Machine, its ProxmoxMachine, its Node and its VM.
func (p *platform) machineGone() {
	p.seed.drop(capiNS + "machines/pc-platform-sandbox-pve1-x")
	p.seed.drop("/apis/infrastructure.cluster.x-k8s.io/v1alpha2/namespaces/pc-platform/proxmoxmachines/pc-platform-sandbox-pve1-infra")
	p.wl.drop("/api/v1/nodes/pc-platform-sandbox-pve1")
	p.pve.templates = nil
}

// waits runs acquisitions and wants the stage cluster to wait with a message that holds want.
func (p *platform) waits(t *testing.T, want string) {
	t.Helper()
	if end := p.until(4); !strings.HasPrefix(end, "defer:") || !strings.Contains(p.stage(stageCluster).Message, want) {
		t.Fatalf("the run does not wait with %q: %s: %+v\n%s", want, end, p.stage(stageCluster), p.logs)
	}
}

// An off of the shared group leaves nothing: the four definitions on the platform cluster, the group's two templates
// and its address pool on the seed are deleted in an order no machine is hurt by, each waited for, and the cluster's
// Application reads Synced afterwards. Then a second build works. The lab's off left all three kinds and the
// Application OutOfSync (v0120-reports/drill-D3.md, Z3-B).
//
// The charts are the real ones. ArgoCD, Cluster API, the address provider and the API server's finalizers are the
// fakes' model of them (realCharts, builtSharedSandbox, fakeSeed.argoCompare): what the real ones do is the lab's.
func TestSandboxOffLeavesNothingBehind(t *testing.T) {
	p := builtSharedSandbox(t)
	templates := append(p.seed.paths(pmtPath+"pc-platform-sandbox-"), p.seed.paths(kctPath+"pc-platform-sandbox-")...)
	others := len(p.seed.paths(pmtPath)) + len(p.seed.paths(kctPath)) - len(templates)
	if len(templates) != 2 || others == 0 {
		t.Fatalf("templates of the group %v, of the other groups %d", templates, others)
	}
	p.off(t)

	// The sandbox chart's Application goes first, then its definitions: the API server takes its time for those.
	p.waits(t, "definitions of agent-sandbox")
	if p.seed.obj(sandboxApp) != nil || p.seed.obj(sandboxPool) == nil {
		t.Fatal("the sandbox Application is left, or the pool went before the machines")
	}
	for _, path := range p.wl.paths(crdsPath) {
		if str(p.wl.obj(path), "metadata", "deletionTimestamp") == "" {
			t.Fatalf("%s is not being deleted", path)
		}
		p.wl.drop(path)
	}

	// The group leaves the cluster's Application and its MachineDeployment is deleted. The machine is still there:
	// nothing of the group's is touched under it, and the Application is OutOfSync by the pool alone, as on the lab.
	p.waits(t, "machines are Running (5 exist)")
	if p.seed.obj(sandboxMD) != nil || len(p.seed.paths(pmtPath+"pc-platform-sandbox-")) != 1 || p.seed.obj(sandboxPool) == nil {
		t.Fatal("the MachineDeployment is left, or a template or the pool went under a machine")
	}
	if got := p.seed.appSync(ClusterName); got != "OutOfSync" {
		t.Fatalf("with the pool left %s reads %s: the fake's ArgoCD does not show what the lab's did", ClusterName, got)
	}

	// Cluster API has deleted the machine and its VM; the MachineSet is a moment behind and still names the templates.
	p.machineGone()
	p.waits(t, "Cluster API still holds")
	if len(p.seed.paths(pmtPath+"pc-platform-sandbox-"))+len(p.seed.paths(kctPath+"pc-platform-sandbox-")) != 2 {
		t.Fatal("a template was deleted while a MachineSet names it")
	}

	// The MachineSet is gone: the templates go, then the pool. The address provider refuses the pool while an address
	// of it is claimed, and holds it with a finalizer after that: both are waited out, and the access on Proxmox is
	// there until the seed holds nothing of the group.
	p.seed.drop(sandboxMS)
	p.seed.refuse = map[string]denial{"DELETE " + sandboxPool: {400, `admission webhook "validation.inclusterippool.ipam.cluster.x-k8s.io" denied the request: Pool has IPAddresses allocated. Cannot delete Pool until all IPAddresses have been removed.`}}
	p.waits(t, "refuses to delete the address pool")
	if len(p.seed.paths(pmtPath+"pc-platform-sandbox-"))+len(p.seed.paths(kctPath+"pc-platform-sandbox-")) != 0 {
		t.Fatal("the group's templates are left")
	}
	p.seed.refuse = nil
	p.waits(t, "is deleting the address pool")
	if _, in, _ := p.state.snapshot(); in.Status.Sandbox == nil || len(p.pve.pools) != 1 || slices.ContainsFunc(p.pve.acl, func(a string) bool { return strings.HasSuffix(a, "delete=1") }) {
		t.Fatalf("the access on Proxmox went before the seed was clean: pools %v, acl %v", p.pve.pools, p.pve.acl)
	}
	p.seed.drop(sandboxPool)
	if end := p.until(30); end != "complete" {
		t.Fatalf("the off: %s: %+v\n%+v", end, p.stage(stageCluster), p.stage(stageWorkloadPlatform))
	}

	_, in, _ := p.state.snapshot()
	left := slices.Concat(p.wl.paths(crdsPath), p.seed.paths(pmtPath+"pc-platform-sandbox-"), p.seed.paths(kctPath+"pc-platform-sandbox-"), p.seed.paths(sandboxPool))
	if len(left) != 0 || in.Status.Sandbox != nil || len(p.pve.pools) != 0 {
		t.Fatalf("left after the off: %v; status %+v, pools %v", left, in.Status.Sandbox, p.pve.pools)
	}
	if got := len(p.seed.paths(pmtPath)) + len(p.seed.paths(kctPath)); got != others {
		t.Errorf("the other groups keep %d templates of %d", got, others)
	}
	if got := p.seed.appSync(ClusterName); got != "Synced" || str(p.seed.obj(appsPath+ClusterName), "status", "health", "status") != "Healthy" {
		t.Fatalf("after the off %s reads %s", ClusterName, got)
	}

	// A second build after the off: the same settings, a new machine.
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Spec.Sandbox = sharedSandbox() })
	p.sandboxMachine()
	for _, id := range []string{stageCluster, stageWorkloadPlatform, stageRegister} {
		p.again(id)
	}
	if end := p.until(30); end != "complete" {
		t.Fatalf("the second build: %s: %+v\n%+v", end, p.stage(stageCluster), p.stage(stageWorkloadPlatform))
	}
	_, in, _ = p.state.snapshot()
	if st := in.Status.Sandbox; st == nil || st.BuiltAt == nil || p.seed.obj(sandboxPool) == nil || len(p.wl.paths(crdsPath)) != len(sandboxCRDs) ||
		len(p.seed.paths(pmtPath+"pc-platform-sandbox-")) != 1 || p.seed.appSync(ClusterName) != "Synced" || p.seed.obj(sandboxApp) == nil {
		t.Fatalf("the second build: status %+v, pool %v, definitions %v, %s %s", st, p.seed.obj(sandboxPool) != nil, p.wl.paths(crdsPath), ClusterName, p.seed.appSync(ClusterName))
	}
}

// A definition goes only when no object of its kind is left in any namespace: deleting it would delete them. The
// other three go, the run says which one it kept, and the off ends.
func TestSandboxOffKeepsADefinitionWithObjects(t *testing.T) {
	p := builtSharedSandbox(t)
	p.wl.put(sandboxOneCRD, meta("left", nil))
	p.off(t)
	p.waits(t, "definitions of agent-sandbox")
	if msg := p.stage(stageCluster).Message; strings.Contains(msg, "sandboxes.agents.x-k8s.io") || !strings.Contains(msg, "sandboxclaims.extensions.agents.x-k8s.io") {
		t.Fatalf("the definitions being deleted: %s", msg)
	}
	for _, path := range p.wl.paths(crdsPath) {
		if strings.HasSuffix(path, "/sandboxes.agents.x-k8s.io") != (str(p.wl.obj(path), "metadata", "deletionTimestamp") == "") {
			t.Fatalf("%s: deleted with an object left, or kept without one", path)
		}
		if !strings.HasSuffix(path, "/sandboxes.agents.x-k8s.io") {
			p.wl.drop(path)
		}
	}
	p.machineGone()
	p.seed.drop(sandboxMS)
	p.waits(t, "is deleting the address pool")
	p.seed.drop(sandboxPool)
	if end := p.until(30); end != "complete" {
		t.Fatalf("the off: %s: %+v\n%+v", end, p.stage(stageCluster), p.stage(stageWorkloadPlatform))
	}
	if got := p.wl.paths(crdsPath); len(got) != 1 || p.wl.obj(sandboxOneCRD) == nil || !strings.Contains(p.logs.String(), "a sandbox definition is kept") {
		t.Fatalf("definitions left %v; the object %v\n%s", got, p.wl.obj(sandboxOneCRD), p.logs)
	}
}

// Why the proxmox-csi node plugin's pods are replaced on the other machines when the shared group comes or goes (the
// lab: v0120-reports/drill-D2.md Z2-a and drill-D3.md Z3-C, the Application's spec changed at 21:53:01 and back at
// 22:34:03 in v0120lab/z2/b/changes-and-ctd.txt and z3/c/watch-changes-head.txt): the group changes the values of the
// platform's storage component, by the node affinity alone, which is in the DaemonSet's pod template. Separate changes
// nothing of it. It cannot go without changing what an installation without sandboxes renders, so the plan rows say it
// where it happens, and only there.
func TestSharedGroupReplacesTheStoragePluginsPodsAndThePlanSaysSo(t *testing.T) {
	b := sandboxBOM(t)
	csi := func(sb *SandboxSpec) map[string]any {
		spec, in := validInstallation(t).Spec, renderInput()
		if sb != nil {
			spec.Sandbox, in = sb, sandboxRender(sb)
		}
		for _, a := range renderAll(t, b.For(spec), in).apps {
			if str(a, "metadata", "name") == "pc-platform-proxmox-csi" {
				return dig(a, "spec", "source", "helm", "valuesObject").(map[string]any)
			}
		}
		t.Fatal("no Application of proxmox-csi")
		return nil
	}
	none, shared, separate := csi(nil), csi(sharedSandbox()), csi(separateSandbox())
	if !sameJSON(none, separate) || sameJSON(none, shared) {
		t.Fatal("the storage component's values: separate must leave them, shared must change them")
	}
	delete(shared["node"].(map[string]any), "affinity")
	if !sameJSON(none, shared) {
		t.Fatal("the shared group changes more of the storage component than the node plugin's affinity")
	}

	in := validInstallation(t)
	in.Status.BOM, in.Status.Clusters = &BOMRef{Release: b.Release, SHA256: b.SHA256}, []ClusterStatus{{Name: ClusterName}}
	impact := func() string {
		p, err := PlanFor(in, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range p.Changes {
			if c.Name == "sandbox" {
				return c.Impact
			}
		}
		t.Fatal("no row sandbox")
		return ""
	}
	const says = "replaces the plugin's pod on every other machine"
	now := in.CreatedAt
	for placement, sb := range map[string]*SandboxSpec{SandboxShared: sharedSandbox(), SandboxSeparate: separateSandbox()} {
		in.Spec.Sandbox, in.Status.Sandbox = sb, nil
		on := impact()
		in.Spec.Sandbox, in.Status.Sandbox = &SandboxSpec{Placement: SandboxOff}, &SandboxStatus{Placement: placement, BuiltAt: &now}
		off := impact()
		if want := placement == SandboxShared; strings.Contains(on, says) != want || strings.Contains(off, says) != want {
			t.Errorf("%s: the plan must name the plugin's pods being replaced only where they are: on %q, off %q", placement, on, off)
		}
	}
}

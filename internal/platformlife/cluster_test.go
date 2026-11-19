package platformlife

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

const (
	wlAdmin       = "workload-admin-token-0123456789"
	providerToken = "provider-sa-token-0123456789abcdef"
	seedNS        = "/api/v1/namespaces/"
	appsPath      = "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/"
	capiNS        = "/apis/cluster.x-k8s.io/v1beta2/namespaces/pc-platform/"
	inventoryPath = "/apis/clusterctl.cluster.x-k8s.io/v1alpha3/providers/"
	managerSecret = seedNS + "argocd/secrets/cluster-pc-platform"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func meta(name string, labels map[string]any) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name, "labels": labels}}
}

// platform is an installation whose seed is ready, with the seed's and the workload's API on fakes and everything a
// controller would have made already there: ArgoCD up, the Cluster initialized, its Machines Running, the nodes Ready.
// The clusterctl inventories are empty until the run starts the hook's sync.
type platform struct {
	*env
	seed, wl  *fakeSeed
	registers []map[string]any
	disabled  int
	conflict  error
}

func newPlatform(t *testing.T, kind string) *platform {
	t.Helper()
	p := &platform{env: newEnv(t, kind)}
	p.seed = newFakeSeed(t, staticCA(t), func() string { return "" })
	p.wl = newFakeSeed(t, staticCA(t), func() string { return "" })
	p.wl.sa = wlAdmin
	p.seedAt(120, 1, p.seed, true)
	p.pve.iso = nil
	p.mutate(func(_ *Run, in *Installation, sec *Secrets) {
		in.Status.Seed.CDVolume, in.Status.Seed.TokenSecret = "", SeedPortalTokenSecret
		sec.BootstrapToken = ""
		sec.SeedKubeconfig = &SeedKubeconfig{Server: p.seed.url, CA: p.seed.caPEM(), Token: runSA}
	})
	p.r.Register = func(_ context.Context, body map[string]any) (string, error) {
		if p.conflict != nil {
			return "", p.conflict
		}
		p.registers = append(p.registers, body)
		return "svc-" + str(body, "config", "provider"), nil
	}
	p.r.Disable = func(context.Context, string) error { p.disabled++; return nil }

	cur, _ := Current()
	install := func(f *fakeSeed, want Providers) {
		for typ, ref := range map[string]string{"CoreProvider": want.Core, "BootstrapProvider": want.Bootstrap, "ControlPlaneProvider": want.ControlPlane,
			"InfrastructureProvider": want.Infrastructure, "IPAMProvider": want.IPAM, "AddonProvider": want.Addon} {
			if name, version, ok := strings.Cut(ref, ":"); ok {
				f.objs[inventoryPath+typ+"-"+name] = map[string]any{"type": typ, "providerName": name, "version": version}
			}
		}
	}
	p.seed.onSync = func(name string) { // the clusterctl hook runs when the Application is synced again
		if name == "capi" {
			install(p.seed, cur.CAPI.Seed)
		} else {
			p.wl.mu.Lock()
			install(p.wl, cur.CAPI.Workload)
			p.wl.mu.Unlock()
		}
	}
	// What ArgoCD's comparison lists of the cluster chart: nothing differs from the live cluster.
	p.seed.resources = map[string][]any{ClusterName: {
		map[string]any{"kind": "KubeadmControlPlane", "name": "pc-platform-cp", "status": "Synced"},
		map[string]any{"kind": "ProxmoxMachineTemplate", "name": "pc-platform-cp-pve1-0a1b2c3d", "status": "Synced"},
		map[string]any{"kind": "KubeadmConfigTemplate", "name": "pc-platform-system-pve1-0a1b2c3d", "status": "Synced"},
	}}
	p.seed.put("/apis/apps/v1/namespaces/argocd/deployments/argocd-repo-server", map[string]any{"status": map[string]any{"availableReplicas": 1}})
	kc, err := kamajik8s.BuildKubeconfig("wl", p.wl.url, []byte(p.wl.caPEM()), wlAdmin)
	if err != nil {
		t.Fatal(err)
	}
	p.seed.put(seedNS+"pc-platform/secrets/pc-platform-kubeconfig", map[string]any{"data": map[string]any{"value": b64(kc)}})
	p.seed.put(seedNS+"pc-platform/secrets/pc-platform-ca", map[string]any{"data": map[string]any{"tls.crt": b64(p.wl.caPEM())}})
	p.seed.put(capiNS+"clusters/pc-platform", map[string]any{"metadata": map[string]any{"name": ClusterName},
		"status": map[string]any{"initialization": map[string]any{"controlPlaneInitialized": true}}})
	cluster := map[string]any{"cluster.x-k8s.io/cluster-name": ClusterName}
	for i, name := range []string{"pc-platform-cp", "pc-platform-system-pve1", "pc-platform-kamaji-pve2", "pc-platform-dbaas-pve3"} {
		m := meta(name+"-x", cluster)
		m["spec"], m["status"] = map[string]any{"infrastructureRef": map[string]any{"name": name + "-infra"}}, map[string]any{"phase": "Running"}
		p.seed.put(capiNS+"machines/"+name+"-x", m)
		pm := meta(name+"-infra", nil)
		pm["spec"] = map[string]any{"virtualMachineID": 130 + i}
		p.seed.put("/apis/infrastructure.cluster.x-k8s.io/v1alpha2/namespaces/pc-platform/proxmoxmachines/"+name+"-infra", pm)
		labels := map[string]any{}
		if i > 0 {
			labels[GroupLabel] = strings.Split(name, "-")[2]
			p.seed.put(capiNS+"machinedeployments/"+name, meta(name, map[string]any{"cluster.x-k8s.io/cluster-name": ClusterName, "proxcloud.io/node-group": labels[GroupLabel]}))
		}
		n := meta(name, labels)
		n["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
		p.wl.put("/api/v1/nodes/"+name, n)
	}
	pv := func(name, ns, driver string) {
		p.wl.put("/api/v1/persistentvolumes/"+name, map[string]any{"spec": map[string]any{"claimRef": map[string]any{"namespace": ns, "name": "data-" + name},
			"csi": map[string]any{"driver": driver, "volumeHandle": "lab/pve1/local-lvm/vm-9999-" + name}}})
		p.pve.disks = append(p.pve.disks, "vm-9999-"+name)
	}
	pv("pvc-etcd", "kamaji-system", csiDriver)
	pv("pvc-tenant", "pc-db-tenant", csiDriver)
	p.wl.put(seedNS+"proxcloud-system/secrets/proxcloud-token", map[string]any{"data": map[string]any{"token": b64(providerToken), "ca.crt": b64(p.wl.caPEM())}})
	return p
}

// again makes a finished run run stage id once more.
func (p *platform) again(id string) {
	p.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		run.State, run.FinishedAt = RunRunning, nil
		for i := range run.Stages {
			if run.Stages[i].ID == id {
				run.Stages[i] = Stage{ID: id, State: StagePending}
			}
		}
	})
}

func (p *platform) k8sWrites() int { return p.seed.writeCount() + p.wl.writeCount() }

// leaks fails when a secret appears in the run, the installation, the job codes or the logs.
func (p *platform) leaks(t *testing.T, more ...string) {
	t.Helper()
	run, in, _ := p.state.snapshot()
	dump, _ := json.Marshal(map[string]any{"run": run, "installation": in, "ends": p.jobs.ends, "logs": p.logs.String()})
	for _, secret := range append([]string{runCAPI, runCSI, runSA, wlAdmin, providerToken}, append(more, p.wl.tokens...)...) {
		if strings.Contains(string(dump), secret) {
			t.Fatalf("%q leaked: %s", secret, dump)
		}
	}
}

// Stages 6 to 11 on fakes: the seed's ArgoCD and projects, the clusterctl hook started again until the inventory
// equals the BOM, the cluster's Application with the CAPI token's Secret, a MachineDeployment of a removed node
// deleted, the argocd-manager token only in the seed's cluster Secret, the workload's waves, the platform's volume
// recorded (not the tenant's), both providers registered, the components and the BOM written.
func TestApplyBuildsThePlatform(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.upTo(stageSeedPlatform)
	p.seed.put(capiNS+"machinedeployments/pc-platform-kamaji-pve9", meta("pc-platform-kamaji-pve9",
		map[string]any{"cluster.x-k8s.io/cluster-name": ClusterName, "proxcloud.io/node-group": "kamaji"}))
	p.seed.put(capiNS+"machinedeployments/tenant-md", meta("tenant-md", map[string]any{"cluster.x-k8s.io/cluster-name": "other"}))
	// The machines as CAPMOX leaves them, without onboot; one that has it already, and another installation's VM.
	tags := "proxcloud;pc-platform;pc-platform-" + inID
	p.pve.vms[201] = &fakeVM{system: true, running: true, cfg: map[string]any{"name": "pc-platform-cp-1", "tags": tags}}
	p.pve.vms[202] = &fakeVM{system: true, running: true, cfg: map[string]any{"name": "pc-platform-kamaji-pve2-1", "tags": tags, "onboot": 1}}
	p.pve.vms[203] = &fakeVM{system: true, running: true, cfg: map[string]any{"name": "pc-platform-cp-9", "tags": "proxcloud;pc-platform;pc-platform-other"}}
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v\n%s", end, p.stage(stageWorkloadPlatform), p.logs)
	}
	// Stage 7 ends by setting onboot on this installation's machines that lack it, and on no other VM: after a node
	// reboot Proxmox starts only those (v0.8.0 set it on the seed alone).
	if got := p.pve.vms[201].cfg["onboot"]; got != "1" {
		t.Errorf("machine 201 has onboot %v after stage 7", got)
	}
	if p.pve.writeCount("POST /nodes/pve1/qemu/202/config") != 0 || p.pve.writeCount("POST /nodes/pve1/qemu/203/config") != 0 {
		t.Errorf("a machine that has onboot, or another installation's VM, was written: %v", p.pve.writes)
	}
	run, in, _ := p.state.snapshot()
	cur, _ := Current()
	if run.State != RunSucceeded || in.Status.Phase != PhaseReady || in.Status.BOM == nil || in.Status.BOM.SHA256 != cur.SHA256 ||
		len(in.Status.Components) != len(cur.Components) || !in.Status.Components[0].Managed {
		t.Fatalf("run %s, status %+v", run.State, in.Status)
	}
	if !slices.Equal(p.seed.syncs, []string{"capi", "pc-platform-capi-tenant"}) {
		t.Fatalf("syncs started: %v", p.seed.syncs)
	}
	for _, kept := range []string{"helm.cattle.io/v1/namespaces/kube-system/helmcharts/argocd", "argoproj.io/v1alpha1/namespaces/argocd/appprojects/proxcloud-platform",
		"argoproj.io/v1alpha1/namespaces/argocd/applications/pc-platform", "argoproj.io/v1alpha1/namespaces/argocd/applications/pc-platform-kamaji"} {
		if p.seed.obj("/apis/"+kept) == nil {
			t.Errorf("the seed has no %s", kept)
		}
	}
	if got := str(p.seed.obj(seedNS+"pc-platform/secrets/pc-platform-pve"), "data", "secret"); got != b64(runCAPI) {
		t.Fatalf("CAPI Secret: %q", got)
	}
	if p.seed.obj(capiNS+"machinedeployments/pc-platform-kamaji-pve9") != nil || p.seed.obj(capiNS+"machinedeployments/pc-platform-kamaji-pve2") == nil ||
		p.seed.obj(capiNS+"machinedeployments/tenant-md") == nil || !slices.Equal(p.stage(stageCluster).Receipt.MachineDeploymentsDeleted, []string{"pc-platform-kamaji-pve9"}) {
		t.Fatalf("pruning: receipt %+v", p.stage(stageCluster).Receipt)
	}
	// One token, asked for a year, in the seed's cluster Secret and in no Secret of the workload.
	held := p.seed.obj(managerSecret)
	config, _ := base64.StdEncoding.DecodeString(str(held, "data", "config"))
	if len(p.wl.tokens) != 1 || p.wl.ttls[0] != managerTokenTTL.Seconds() || !strings.Contains(string(config), p.wl.tokens[0]) ||
		str(held, "metadata", "labels", "argocd.argoproj.io/secret-type") != "cluster" || in.Status.Clusters[0].ArgoCDTokenExpiresAt == nil {
		t.Fatalf("argocd-manager token: %v, Secret %v", p.wl.tokens, held)
	}
	p.wl.mu.Lock()
	for path, o := range p.wl.objs {
		if raw, _ := json.Marshal(o); strings.Contains(path, "/secrets/") && strings.Contains(string(raw), b64(p.wl.tokens[0])) {
			t.Errorf("the workload stores the argocd-manager token in %s", path)
		}
	}
	p.wl.mu.Unlock()
	if p.wl.obj(seedNS+"csi-proxmox/secrets/proxmox-csi-plugin") == nil {
		t.Error("no CSI config Secret on the workload")
	}
	if want := []Volume{{Handle: "lab/pve1/local-lvm/vm-9999-pvc-etcd", Component: "kamaji-etcd", PVC: "kamaji-system/data-pvc-etcd"}}; !slices.Equal(in.Status.Volumes, want) {
		t.Fatalf("volumes %+v", in.Status.Volumes)
	}
	if len(p.registers) != 2 || in.Status.Providers == nil || in.Status.Providers.Kubernetes != "svc-kamaji" || in.Status.Providers.Database != "svc-dbaas" ||
		!strings.Contains(str(p.registers[0], "secret", "kubeconfig"), providerToken) || str(p.registers[0], "config", ProviderMarker) != inID {
		t.Fatalf("providers %+v, %d bodies", in.Status.Providers, len(p.registers))
	}
	p.leaks(t)
}

// A wave is applied only when the one before is Synced and Healthy; the nodes are waited for after the first.
func TestWaveGating(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.only(stageWorkloadPlatform)
	p.seed.unsynced["pc-platform-cilium"] = true
	p.wl.objs["/api/v1/nodes/pc-platform-dbaas-pve3"]["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False"}}}
	if end := p.once(); end != "defer:waiting_workload_platform" {
		t.Fatalf("end %s", end)
	}
	// The platform's volumes are recorded at every look: a run that fails in a wave leaves a Remove their handles.
	if _, in, _ := p.state.snapshot(); len(in.Status.Volumes) != 1 || in.Status.Volumes[0].Handle != "lab/pve1/local-lvm/vm-9999-pvc-etcd" {
		t.Fatalf("volumes recorded while wave 1 is pending: %+v", in.Status.Volumes)
	}
	s := p.stage(stageWorkloadPlatform)
	if s.Code != CodeComponentUnhealthy || s.Progress != "step 1 of 4, wave 0" || len(s.Items) != 1 || s.Items[0].ID != "pc-platform-cilium" ||
		p.seed.obj(appsPath+"pc-platform-cilium") == nil || p.seed.obj(appsPath+"pc-platform-cert-manager") != nil {
		t.Fatalf("wave 1 not Synced: %+v", s)
	}
	// Synced and Healthy, but compared against another version: still not this wave's.
	old := map[string]any{"health": map[string]any{"status": "Healthy"},
		"sync": map[string]any{"status": "Synced", "comparedTo": map[string]any{"source": map[string]any{"targetRevision": "0.0.1"}}}}
	p.seed.objs[appsPath+"pc-platform-cilium"]["status"] = old
	if end := p.once(); end != "defer:waiting_workload_platform" || p.seed.obj(appsPath+"pc-platform-cert-manager") != nil {
		t.Fatalf("an older version passed: %s", end)
	}
	// The same version, compared against other values than the Application has now: not this wave's either.
	app := p.seed.objs[appsPath+"pc-platform-cilium"]
	old["sync"] = map[string]any{"status": "Synced", "comparedTo": map[string]any{"source": map[string]any{
		"repoURL": dig(app, "spec", "source", "repoURL"), "chart": dig(app, "spec", "source", "chart"), "targetRevision": dig(app, "spec", "source", "targetRevision"),
		"helm": map[string]any{"valuesObject": map[string]any{"before": "the edit"}}}}}
	if end := p.once(); end != "defer:waiting_workload_platform" || p.stage(stageWorkloadPlatform).Code != CodeComponentUnhealthy {
		t.Fatalf("a status of other values passed: %s %+v", end, p.stage(stageWorkloadPlatform))
	}
	delete(p.seed.unsynced, "pc-platform-cilium")
	if end := p.once(); end != "defer:waiting_workload_platform" || p.stage(stageWorkloadPlatform).Code != CodeNodesTimeout ||
		p.seed.obj(appsPath+"pc-platform-cert-manager") != nil {
		t.Fatalf("a node is not Ready: %s %+v", end, p.stage(stageWorkloadPlatform))
	}
	p.wl.objs["/api/v1/nodes/pc-platform-dbaas-pve3"]["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	first := *p.stage(stageWorkloadPlatform).Deadline
	if end := p.until(10); end != "complete" || !p.stage(stageWorkloadPlatform).Deadline.After(first) {
		t.Fatalf("end %s, deadline %v after %v", end, p.stage(stageWorkloadPlatform).Deadline, first)
	}
}

// A wave that stays stuck keeps its deadline over the acquisitions and fails at it: walking the waves before it again
// neither starts the deadline again nor writes the stage.
func TestWaveStallDeadline(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.only(stageWorkloadPlatform)
	p.seed.unsynced["pc-platform-cert-manager"] = true
	if end := p.once(); end != "defer:waiting_workload_platform" || p.stage(stageWorkloadPlatform).Progress != "step 2 of 4, wave 1" {
		t.Fatalf("end %s: %+v", end, p.stage(stageWorkloadPlatform))
	}
	first := p.stage(stageWorkloadPlatform)
	for range 3 {
		if end := p.once(); end != "defer:waiting_workload_platform" {
			t.Fatalf("end %s", end)
		}
	}
	if s := p.stage(stageWorkloadPlatform); !s.Deadline.Equal(*first.Deadline) || !s.LastProgressAt.Equal(*first.LastProgressAt) || s.Progress != "step 2 of 4, wave 1" {
		t.Fatalf("the deadline moved while wave 2 is stuck: %v -> %v (%s)", first.Deadline, s.Deadline, s.Progress)
	}
	// Wave 1 pending again shows in the items and leaves the progress and the deadline where they were.
	p.seed.unsynced["pc-platform-cilium"] = true
	p.seed.objs[appsPath+"pc-platform-cilium"]["status"] = map[string]any{"sync": map[string]any{"status": "OutOfSync"}, "health": map[string]any{"status": "Degraded"}}
	if end := p.once(); end != "defer:waiting_workload_platform" {
		t.Fatalf("end %s", end)
	}
	if s := p.stage(stageWorkloadPlatform); !s.Deadline.Equal(*first.Deadline) || s.Progress != "step 2 of 4, wave 1" || len(s.Items) != 1 || s.Items[0].State != "OutOfSync/Degraded" {
		t.Fatalf("wave 1 pending again: %+v", s)
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
}

// A platform v0.8.0 installed, applied again on a build whose own BOM differs in every pin: the images, the
// Applications, the providers and the recorded status all come from the 0.8.0 BOM, and the platform stays on it.
func TestApplyStaysOnTheInstalledBOM(t *testing.T) {
	old := newerBuild(t)
	ref := BOMRef{Release: old.Release, SHA256: old.SHA256}
	onOld := func(run *Run, in *Installation, _ *Secrets) { run.BOM, in.Status.BOM = ref, &ref }

	e := imageEnv(t)
	e.mutate(onOld)
	e.once()
	if len(e.imports.enqueued) != 4 {
		t.Fatalf("imports %+v", e.imports.enqueued)
	}
	for _, req := range e.imports.enqueued {
		if req.Digest != old.Seed.Image.Digest && req.Digest != old.Kubernetes.NodeImage.Digest {
			t.Fatalf("an image that is not of the installed BOM: %+v", req)
		}
	}

	p := newPlatform(t, RunApply)
	p.mutate(onOld)
	p.upTo(stageSeedPlatform)
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v\n%s", end, p.stage(stageWorkloadPlatform), p.logs)
	}
	run, in, _ := p.state.snapshot()
	if run.State != RunSucceeded || in.Status.BOM == nil || *in.Status.BOM != ref || len(in.Status.Components) != len(old.Components) {
		t.Fatalf("run %s, status %+v", run.State, in.Status)
	}
	for i, c := range old.Components {
		if got := in.Status.Components[i]; got.Version != c.Chart.Version || got.Digest != c.Chart.Digest {
			t.Errorf("component %s recorded as %+v, the installed BOM has %s", c.Name, got, c.Chart.Version)
		}
		app := p.seed.obj(appsPath + "pc-platform-" + c.Name)
		if c.Name == "proxcloud-platform" || app == nil {
			continue
		}
		if str(app, "metadata", "labels", "proxcloud.io/bom-release") != "0.8.0" || str(app, "spec", "source", "targetRevision") != c.Chart.Version {
			t.Errorf("Application of %s is not from the installed BOM: %v", c.Name, app["metadata"])
		}
	}
	cilium := comp(t, old.BOM, ClusterWorkload, "cilium")
	if app := p.seed.obj(appsPath + "pc-platform-cilium"); str(app, "spec", "source", "targetRevision") != cilium.Chart.Version ||
		str(app, "spec", "destination", "namespace") != cilium.Namespace {
		t.Fatalf("cilium: %v", app)
	}
	if len(p.registers) != 2 || str(p.registers[0], "config", "argocd", "chartVersion") != old.TenantCharts.Kubernetes.Version {
		t.Fatalf("providers registered: %d, the Kubernetes chart %s", len(p.registers), str(p.registers[0], "config", "argocd", "chartVersion"))
	}
}

// Remove of a platform v0.8.0 installed reads the platform's volumes by the namespaces of the installed BOM.
func TestRemoveReadsTheInstalledBOM(t *testing.T) {
	old := newerBuild(t)
	p := removal(t)
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) {
		in.Status.BOM = &BOMRef{Release: old.Release, SHA256: old.SHA256}
	})
	p.once()
	if _, in, _ := p.state.snapshot(); len(in.Status.Volumes) != 1 || in.Status.Volumes[0].Handle != "lab/pve1/local-lvm/vm-9999-pvc-etcd" {
		t.Fatalf("volumes %+v: %+v", in.Status.Volumes, p.stage(StageRemoveCluster))
	}
	p.seed.mu.Lock()
	for k := range p.seed.objs {
		if strings.HasPrefix(k, capiNS+"machines/") {
			delete(p.seed.objs, k)
		}
	}
	p.seed.mu.Unlock()
	if end := p.until(8); end != "complete" || !slices.Equal(p.pve.disks, []string{"vm-9999-pvc-tenant"}) {
		t.Fatalf("end %s, volumes left %v", end, p.pve.disks)
	}
}

// The nodes are waited for after the lowest wave, also when the BOM lists a component of a later wave first.
func TestNodeWaitFollowsTheLowestWave(t *testing.T) {
	cur, _ := Current()
	order := slices.Clone(cur.BOM.Components)
	t.Cleanup(func() { cur.BOM.Components = order })
	cur.BOM.Components = slices.Clone(order)
	slices.Reverse(cur.BOM.Components)
	p := newPlatform(t, RunApply)
	p.only(stageWorkloadPlatform)
	p.wl.objs["/api/v1/nodes/pc-platform-dbaas-pve3"]["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False"}}}
	if end := p.once(); end != "defer:waiting_workload_platform" || p.stage(stageWorkloadPlatform).Code != CodeNodesTimeout ||
		p.stage(stageWorkloadPlatform).Progress != "step 1 of 4, wave 0" || p.seed.obj(appsPath+"pc-platform-cert-manager") != nil {
		t.Fatalf("end %s: %+v", end, p.stage(stageWorkloadPlatform))
	}
}

// CAPMOX reports a token Proxmox refuses on the ProxmoxCluster or a ProxmoxMachine, not on a Machine: the stage fails
// capmox_auth_failed with that message instead of waiting for its deadline. A VMID that reads 401 is not a refusal.
func TestCAPMOXAuthFailure(t *testing.T) {
	infra := "/apis/infrastructure.cluster.x-k8s.io/v1alpha2/namespaces/pc-platform/"
	cond := func(msg string) map[string]any {
		return map[string]any{"conditions": []any{map[string]any{"type": "ProxmoxAvailable", "status": "False", "message": msg}}}
	}
	for _, path := range []string{infra + "proxmoxclusters/pc-platform", infra + "proxmoxmachines/pc-platform-cp-infra"} {
		p := newPlatform(t, RunApply)
		p.only(stageCluster)
		obj := p.seed.obj(path)
		if obj == nil {
			obj = meta("pc-platform", nil)
		}
		obj["status"] = cond("cannot clone VM 401: still locked")
		p.seed.put(path, obj)
		if end := p.once(); end != "complete" {
			t.Fatalf("%s, a VMID 401: end %s %+v", path, end, p.stage(stageCluster))
		}
		p.again(stageCluster)
		obj["status"] = cond("failed to list nodes: 401 authentication failure " + runCAPI)
		if end := p.once(); end != "fail:"+CodeCAPMOXAuthFailed || !strings.Contains(p.stage(stageCluster).Message, "401 authentication failure") {
			t.Fatalf("%s: end %s %+v", path, end, p.stage(stageCluster))
		}
		p.leaks(t)
	}
}

// Stage 6 deletes the portal's token Secrets a rotation left on the seed, and keeps the one whose token is stored.
func TestSeedPlatformSweepsTokenSecrets(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.only(stageSeedPlatform)
	ns := seedNS + "proxcloud-system/secrets/"
	for _, name := range []string{"proxcloud-portal-token-g1", "proxcloud-portal-token-g2", "unrelated"} {
		p.seed.put(ns+name, meta(name, nil))
	}
	if end := p.until(6); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(stageSeedPlatform))
	}
	if p.seed.obj(ns+"proxcloud-portal-token-g2") != nil || p.seed.obj(ns+"proxcloud-portal-token-g1") == nil || p.seed.obj(ns+"unrelated") == nil {
		t.Fatalf("Secrets after stage 6: %v", p.seed.writes)
	}
}

// A provider installed at another version than the BOM's is not upgraded: capi_init_failed, and no sync is started.
func TestInventoryVersionMismatch(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.only(stageSeedPlatform)
	p.seed.put(inventoryPath+"core", map[string]any{"type": "CoreProvider", "providerName": "cluster-api", "version": "v1.0.0"})
	if end := p.until(5); end != "fail:"+CodeCAPIInitFailed || len(p.seed.syncs) != 0 {
		t.Fatalf("end %s, syncs %v", end, p.seed.syncs)
	}
}

// ArgoCD does not sync a revision again once its retries are spent, self-heal included. The run starts the sync of
// an Application that is not Synced and Healthy and whose last operation ended Failed or Error, with the
// Application's own retry policy; it leaves one alone that has no operation yet or whose operation still runs.
func TestFailedSyncIsStartedAgain(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.only(stageWorkloadPlatform)
	const name = "pc-platform-cert-manager"
	p.seed.unsynced[name] = true
	status := func(phase string) {
		st := map[string]any{"sync": map[string]any{"status": "OutOfSync"}, "health": map[string]any{"status": "Missing"}}
		if phase != "" {
			st["operationState"] = map[string]any{"phase": phase, "message": "one or more synchronization tasks completed unsuccessfully (retried 10 times)"}
		}
		p.seed.mu.Lock()
		p.seed.objs[appsPath+name]["status"] = st
		p.seed.mu.Unlock()
	}
	for _, phase := range []string{"", "Running"} {
		if end := p.once(); end != "defer:waiting_workload_platform" || len(p.seed.syncs) != 0 {
			t.Fatalf("phase %q: end %s, syncs %v", phase, end, p.seed.syncs)
		}
		status("Running")
	}
	status("Failed")
	if end := p.once(); end != "defer:waiting_workload_platform" || !slices.Equal(p.seed.syncs, []string{name}) {
		t.Fatalf("end %s, syncs %v", end, p.seed.syncs)
	}
	app := p.seed.obj(appsPath + name)
	if !reflect.DeepEqual(p.seed.lastOp["retry"], dig(app, "spec", "syncPolicy", "retry")) || dig(p.seed.lastOp, "sync", "revision") != dig(app, "spec", "source", "targetRevision") {
		t.Fatalf("operation %v", p.seed.lastOp)
	}
	items := p.stage(stageWorkloadPlatform).Items
	if i := slices.IndexFunc(items, func(it StageItem) bool { return it.ID == name }); i < 0 || !strings.Contains(items[i].Message, "the run started it again") {
		t.Fatalf("items %+v", items)
	}
	// The sync the run started ends Failed too: its retries are spent a second time, so the stage fails with what the
	// operation failed on instead of starting a third one. The next run starts it once more.
	p.seed.mu.Lock()
	p.seed.objs[appsPath+name]["status"].(map[string]any)["operationState"] = map[string]any{"phase": "Failed", "message": "retried 10 times",
		"syncResult": map[string]any{"resources": []any{map[string]any{"kind": "Job", "namespace": "cert-manager", "name": "startupapicheck", "hookPhase": "Failed", "message": "deadline exceeded"}}}}
	p.seed.mu.Unlock()
	if end := p.once(); end != "fail:"+CodeComponentUnhealthy || len(p.seed.syncs) != 1 ||
		!strings.Contains(p.stage(stageWorkloadPlatform).Message, "Job cert-manager/startupapicheck: deadline exceeded") {
		t.Fatalf("a second failed sync: end %s, syncs %v, %+v", end, p.seed.syncs, p.stage(stageWorkloadPlatform))
	}
	p.again(stageWorkloadPlatform)
	if end := p.once(); end != "defer:waiting_workload_platform" || len(p.seed.syncs) != 2 {
		t.Fatalf("the next run: end %s, syncs %v", end, p.seed.syncs)
	}
	// The workload cluster's own Application, which stage cluster applies outside the waves.
	p = newPlatform(t, RunApply)
	p.seed.unsynced[ClusterName] = true
	p.only(stageCluster)
	p.once()
	p.seed.mu.Lock()
	p.seed.objs[appsPath+ClusterName]["status"] = map[string]any{"sync": map[string]any{"status": "OutOfSync"}, "operationState": map[string]any{"phase": "Error"}}
	p.seed.mu.Unlock()
	p.again(stageCluster)
	if end := p.once(); !slices.Contains(p.seed.syncs, ClusterName) {
		t.Fatalf("end %s, syncs %v: %+v", end, p.seed.syncs, p.stage(stageCluster))
	}
}

// A failed Machine fails the stage with an item naming the machine, its VMID and Cluster API's failure message; a
// Proxmox authentication failure has its own code.
func TestMachineFailure(t *testing.T) {
	for msg, code := range map[string]string{"cannot clone: storage local-lvm is full": CodeMachineFailed, "401 authentication failure": CodeCAPMOXAuthFailed} {
		p := newPlatform(t, RunApply)
		p.only(stageCluster)
		p.seed.objs[capiNS+"machines/pc-platform-kamaji-pve2-x"]["status"] = map[string]any{"phase": "Failed", "failureMessage": msg + " " + runCAPI}
		if end := p.once(); end != "fail:"+code {
			t.Fatalf("%s: end %s", msg, end)
		}
		s := p.stage(stageCluster)
		i := slices.IndexFunc(s.Items, func(it StageItem) bool { return it.ID == "pc-platform-kamaji-pve2-x" })
		if i < 0 || s.Items[i].State != "Failed" || !strings.Contains(s.Items[i].Message, "VM 132") || !strings.Contains(s.Items[i].Message, msg) ||
			!strings.Contains(s.Message, "pc-platform-kamaji-pve2-x (VM 132)") {
			t.Fatalf("%s: items %+v, message %q", msg, s.Items, s.Message)
		}
		p.leaks(t)
	}
}

// Cluster API v1.14 sets no Machine Failed and CAPMOX v0.9.2 has no failure fields: a machine CAPMOX gave up shows only
// as the ProxmoxMachine's condition VirtualMachineProvisioned, False with VMProvisionFailed or TaskFailed. The stage
// fails with that text and the command that makes Cluster API build the machine again. A step CAPMOX tries again is
// waited for, with its reason and error in the item and in the stage's message.
func TestCAPMOXMachineConditions(t *testing.T) {
	const pm = "/apis/infrastructure.cluster.x-k8s.io/v1alpha2/namespaces/pc-platform/proxmoxmachines/pc-platform-kamaji-pve2-infra"
	set := func(p *platform, reason, msg string) {
		p.seed.mu.Lock()
		defer p.seed.mu.Unlock()
		p.seed.objs[capiNS+"machines/pc-platform-kamaji-pve2-x"]["status"] = map[string]any{"phase": "Provisioning"}
		p.seed.objs[pm]["status"] = map[string]any{"conditions": []any{map[string]any{"type": "VirtualMachineProvisioned", "status": "False", "reason": reason, "message": msg}}}
	}
	item := func(p *platform) StageItem {
		s := p.stage(stageCluster)
		i := slices.IndexFunc(s.Items, func(it StageItem) bool { return it.ID == "pc-platform-kamaji-pve2-x" })
		if i < 0 {
			t.Fatalf("no item of the machine: %+v", s.Items)
		}
		return s.Items[i]
	}
	for reason, c := range map[string]struct{ msg, code string }{
		"VMProvisionFailed": {"cannot reserve 4294967296B of memory on node pve2: 1073741824B available memory left", CodeMachineFailed},
		"TaskFailed":        {"qmclone: zfs error: cannot create 'rpool/data/vm-132-disk-0': out of space " + runCAPI, CodeMachineFailed},
	} {
		p := newPlatform(t, RunApply)
		p.only(stageCluster)
		set(p, reason, c.msg)
		if end := p.once(); end != "fail:"+c.code {
			t.Fatalf("%s: end %s %+v", reason, end, p.stage(stageCluster))
		}
		s, it := p.stage(stageCluster), item(p)
		if it.State != "Failed" || !strings.Contains(it.Message, "VM 132: "+reason+": "+c.msg[:20]) ||
			!strings.Contains(s.Message, "kubectl -n pc-platform delete machine pc-platform-kamaji-pve2-x") {
			t.Fatalf("%s: item %+v, message %q", reason, it, s.Message)
		}
		p.leaks(t)
	}
	p := newPlatform(t, RunApply)
	p.only(stageCluster)
	set(p, "CloningFailed", "500 clone failed: can't lock file")
	if end := p.once(); end != "defer:waiting_cluster" || p.stage(stageCluster).Code != CodeControlPlaneTimeout {
		t.Fatalf("a step CAPMOX tries again: end %s %+v", end, p.stage(stageCluster))
	}
	if s, it := p.stage(stageCluster), item(p); it.State != "Provisioning" || !strings.Contains(it.Message, "CloningFailed: 500 clone failed") ||
		!strings.Contains(s.Message, "pc-platform-kamaji-pve2-x (VM 132): CloningFailed: 500 clone failed") {
		t.Fatalf("item %+v, message %q", it, s.Message)
	}
	set(p, "WaitingForBootstrapReady", "")
	if end := p.once(); end != "defer:waiting_cluster" || item(p).Message != "VM 132: WaitingForBootstrapReady" {
		t.Fatalf("a step without an error: end %s, item %+v", end, item(p))
	}
	// A VMID that holds another VM: deleting the Machine would make CAPMOX delete that VM, so the message has no
	// delete command and says not to.
	p = newPlatform(t, RunApply)
	p.only(stageCluster)
	set(p, "VMProvisionFailed", `expected VM name to match "pc-platform-kamaji-pve2-infra" but it was "pc-seed-other"`)
	if end, s := p.once(), p.stage(stageCluster); end != "fail:"+CodeMachineFailed || strings.Contains(s.Message, "kubectl") ||
		!strings.Contains(s.Message, "Do not delete the Machine") || !strings.Contains(s.Message, "ProxmoxMachine pc-platform-kamaji-pve2-infra") {
		t.Fatalf("a VMID of another VM: end %s, message %q", end, s.Message)
	}
}

// CAPMOX calls a VM provisioned as soon as it runs and has its addresses. A machine that then does not join (kubeadm,
// the API address, etcd) shows on Cluster API's own conditions: the Machine's Ready in its item and in the waiting
// message, the KubeadmControlPlane's Available in the message. The Application comes last and only while its sync is
// not done: it is Progressing for as long as the machines are built.
func TestClusterWaitingNote(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.only(stageCluster)
	p.seed.unsynced[ClusterName] = true
	app := func(sync string) {
		obj := meta(ClusterName, nil)
		obj["status"] = map[string]any{"sync": map[string]any{"status": sync}, "health": map[string]any{"status": "Progressing"},
			"operationState": map[string]any{"phase": "Succeeded", "message": "successfully synced (all tasks run)"}}
		p.seed.put(appsPath+ClusterName, obj)
	}
	app("Synced")
	p.seed.mu.Lock()
	p.seed.objs[capiNS+"machines/pc-platform-kamaji-pve2-x"]["status"] = map[string]any{"phase": "Provisioned", "conditions": []any{map[string]any{"type": "Ready",
		"status": "False", "reason": "NotReady", "message": "* NodeHealthy: Waiting for a Node with spec.providerID proxmox://a to exist\n* Control plane: not reachable"}}}
	p.seed.objs["/apis/infrastructure.cluster.x-k8s.io/v1alpha2/namespaces/pc-platform/proxmoxmachines/pc-platform-kamaji-pve2-infra"]["status"] = map[string]any{
		"conditions": []any{map[string]any{"type": "VirtualMachineProvisioned", "status": "True", "reason": "Provisioned"}}}
	p.seed.objs["/apis/controlplane.cluster.x-k8s.io/v1beta2/namespaces/pc-platform/kubeadmcontrolplanes/pc-platform-cp"] = map[string]any{"status": map[string]any{
		"conditions": []any{map[string]any{"type": "Available", "status": "False", "reason": "NotAvailable", "message": "etcd member pc-platform-cp-x is not healthy"}}}}
	p.seed.mu.Unlock()
	const ready = "Cluster API: NotReady: * NodeHealthy: Waiting for a Node with spec.providerID proxmox://a to exist * Control plane: not reachable"
	end, s := p.once(), p.stage(stageCluster)
	i := slices.IndexFunc(s.Items, func(it StageItem) bool { return it.ID == "pc-platform-kamaji-pve2-x" })
	if end != "defer:waiting_cluster" || i < 0 || s.Items[i].Message != "VM 132: "+ready || !strings.Contains(s.Message, "pc-platform-kamaji-pve2-x (VM 132): "+ready) ||
		!strings.Contains(s.Message, "Control plane: NotAvailable: etcd member pc-platform-cp-x is not healthy") || strings.Contains(s.Message, "Application") {
		t.Fatalf("end %s, items %+v, message %q", end, s.Items, s.Message)
	}
	app("OutOfSync")
	p.once()
	if msg := p.stage(stageCluster).Message; !strings.Contains(msg, "Application pc-platform is OutOfSync/Progressing") || strings.Index(msg, "Application") < strings.Index(msg, "Cluster API:") {
		t.Fatalf("an Application that is not synced: %q", msg)
	}
}

// A sync that keeps failing is not waited out to the stage's time limit: ArgoCD spends about 40 minutes on the ten
// attempts of one operation. After four failed attempts, the last of them in this run, the stage fails with what the
// attempt failed on. An attempt that failed before this run began does not count: a Retry gives the fix one attempt.
func TestFailingSyncIsNotWaitedOut(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.only(stageWorkloadPlatform)
	const name = "pc-platform-cert-manager"
	p.seed.unsynced[name] = true
	status := func(retries int, finished time.Time) {
		p.seed.mu.Lock()
		defer p.seed.mu.Unlock()
		p.seed.objs[appsPath+name]["status"] = map[string]any{"sync": map[string]any{"status": "OutOfSync"}, "health": map[string]any{"status": "Missing"},
			"operationState": map[string]any{"phase": "Running", "retryCount": retries, "finishedAt": finished.UTC().Format(time.RFC3339), "message": "one or more synchronization tasks completed unsuccessfully. Retrying attempt",
				"syncResult": map[string]any{"resources": []any{map[string]any{"kind": "Job", "namespace": "cert-manager", "name": "startupapicheck", "hookPhase": "Failed", "message": "deadline exceeded"}}}}}
	}
	p.once()
	for what, st := range map[string]struct {
		retries  int
		finished time.Time
	}{"three attempts": {3, time.Now().Add(time.Minute)}, "attempts of the run before": {6, time.Now().Add(-time.Hour)}} {
		status(st.retries, st.finished)
		if end := p.once(); end != "defer:waiting_workload_platform" {
			t.Fatalf("%s: end %s %+v", what, end, p.stage(stageWorkloadPlatform))
		}
	}
	status(4, time.Now().Add(time.Minute))
	if end, s := p.once(), p.stage(stageWorkloadPlatform); end != "fail:"+CodeComponentUnhealthy || len(p.seed.syncs) != 0 ||
		!strings.Contains(s.Message, "failed 4 times") || !strings.Contains(s.Message, "Job cert-manager/startupapicheck: deadline exceeded") {
		t.Fatalf("four attempts: end %s, syncs %v, %+v", end, p.seed.syncs, s)
	}
}

// CAPMOX opens the Proxmox API before it writes anything on the ProxmoxCluster: a URL the seed cannot reach or a token
// Proxmox refuses shows as no condition at all. The stage says so while it waits and fails after ten minutes.
func TestCAPMOXSilence(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.only(stageCluster)
	const pc = "/apis/infrastructure.cluster.x-k8s.io/v1alpha2/namespaces/pc-platform/proxmoxclusters/pc-platform"
	p.seed.put(pc, meta("pc-platform", nil))
	p.seed.mu.Lock()
	p.seed.objs[capiNS+"machines/pc-platform-cp-x"]["status"] = map[string]any{"phase": "Pending"}
	p.seed.mu.Unlock()
	if end := p.once(); end != "defer:waiting_cluster" || !strings.Contains(p.stage(stageCluster).Message, capmoxLog) ||
		!strings.Contains(p.stage(stageCluster).Message, "https://10.200.200.2:8006") {
		t.Fatalf("end %s: %+v", end, p.stage(stageCluster))
	}
	p.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		for i := range run.Stages {
			if past := time.Now().Add(-capmoxSilence - time.Minute); run.Stages[i].ID == stageCluster {
				run.Stages[i].StartedAt = &past
			}
		}
	})
	if end := p.once(); end != "fail:"+CodeCAPMOXAuthFailed || !strings.Contains(p.stage(stageCluster).Message, capmoxLog) {
		t.Fatalf("after ten minutes: end %s %+v", end, p.stage(stageCluster))
	}
	// A Retry finds the ProxmoxCluster of the run before, on which CAPMOX is in a backoff of up to 1000 s: the run
	// makes it look again, once, with an update of the object. A ProxmoxCluster made in this run is left alone.
	for age, want := range map[time.Duration]int{-time.Hour: 1, time.Minute: 0} {
		p = newPlatform(t, RunApply)
		p.only(stageCluster)
		obj := meta("pc-platform", nil)
		obj["metadata"].(map[string]any)["creationTimestamp"] = time.Now().Add(age).UTC().Format(time.RFC3339)
		p.seed.put(pc, obj)
		p.seed.mu.Lock()
		p.seed.objs[capiNS+"machines/pc-platform-cp-x"]["status"] = map[string]any{"phase": "Pending"}
		p.seed.mu.Unlock()
		for range 2 {
			if end := p.once(); end != "defer:waiting_cluster" {
				t.Fatalf("made %s ago: end %s %+v", -age, end, p.stage(stageCluster))
			}
		}
		p.seed.mu.Lock()
		got := len(slices.DeleteFunc(slices.Clone(p.seed.writes), func(w string) bool { return w != "PATCH "+pc }))
		p.seed.mu.Unlock()
		if got != want {
			t.Fatalf("made %s ago: %d updates of the ProxmoxCluster, want %d: %v", -age, got, want, p.seed.writes)
		}
	}
	// A condition, also a False one, is CAPMOX reporting: its text is shown and the stage waits.
	p = newPlatform(t, RunApply)
	p.only(stageCluster)
	obj := meta("pc-platform", nil)
	obj["status"] = map[string]any{"conditions": []any{map[string]any{"type": "ProxmoxAvailable", "status": "False", "reason": "ProxmoxUnreachable", "message": "secret not ready"}}}
	p.seed.put(pc, obj)
	p.seed.mu.Lock()
	p.seed.objs[capiNS+"machines/pc-platform-cp-x"]["status"] = map[string]any{"phase": "Pending"}
	p.seed.mu.Unlock()
	if end := p.once(); end != "defer:waiting_cluster" || !strings.Contains(p.stage(stageCluster).Message, "ProxmoxCluster: ProxmoxUnreachable: secret not ready") {
		t.Fatalf("a False condition: end %s %+v", end, p.stage(stageCluster))
	}
}

// A node template with a Proxmox cloud-init drive on another slot than ide0 gives CAPMOX's machines two cidata media:
// stage 7 refuses it before it writes anything, with the command that removes the drive. A drive on ide0 passes.
func TestNodeTemplateCloudInitDrive(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.only(stageCluster)
	p.pve.tmplCfg = map[int]map[string]any{
		9101: {"scsi0": "local-lvm:base-9101-disk-0", "ide0": "local-lvm:vm-9101-cloudinit,media=cdrom"},
		9102: {"scsi0": "local-lvm:base-9102-disk-0", "ide2": "local-lvm:vm-9102-cloudinit,media=cdrom"}}
	if end := p.once(); end != "fail:"+CodeMachineFailed || !strings.Contains(p.stage(stageCluster).Message, "qm set 9102 --delete ide2") ||
		!strings.Contains(p.stage(stageCluster).Message, "its smoke test in Template updates adds one") || p.seed.writeCount() != 0 {
		t.Fatalf("end %s: %+v, seed writes %v", end, p.stage(stageCluster), p.seed.writes)
	}
	p.pve.mu.Lock()
	p.pve.tmplCfg[9102] = map[string]any{"scsi0": "local-lvm:base-9102-disk-0", "ide0": "local-lvm:vm-9102-cloudinit,media=cdrom", "ide2": "none,media=cdrom"}
	p.pve.mu.Unlock()
	p.again(stageCluster)
	if end := p.once(); end != "complete" {
		t.Fatalf("the drive on ide0: end %s %+v", end, p.stage(stageCluster))
	}
	// A template that passed leaves an item, so the stage shows that it looked; one Proxmox no longer knows has none.
	if s := p.stage(stageCluster); item(s, "template-9102") == nil || item(s, "template-9102").State != CheckOK || !strings.Contains(item(s, "template-9102").Message, "pve2") ||
		item(s, "template-9101") == nil || item(s, "template-9103") != nil || item(s, "pc-platform-cp-x") == nil {
		t.Fatalf("items of checked templates: %+v", s.Items)
	}
	// A template the portal token may not read is not checked, and the stage goes on with an item that says so.
	p = newPlatform(t, RunApply)
	p.only(stageCluster)
	p.pve.tmplDeny = map[int]bool{9102: true}
	if end, s := p.once(), p.stage(stageCluster); end != "complete" || len(s.Items) == 0 || s.Items[0].ID != "template-9102" || s.Items[0].State != "not checked" {
		t.Fatalf("a template that may not be read: end %s %+v", end, s)
	}
}

// ArgoCD reports an Application Synced and Healthy from its resources alone, while a Sync hook of the same operation
// still runs or keeps failing. The gate also needs the operation Succeeded; the item and the stage's message carry the
// hook that failed, and the providers clusterctl has not installed are named with their type.
func TestGateNeedsTheSyncSucceeded(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.only(stageSeedPlatform)
	p.seed.unsynced["capi"] = true
	if end := p.until(3); end != "defer:waiting_seed_platform" || p.seed.obj(appsPath+"capi") == nil {
		t.Fatalf("end %s: %+v", end, p.stage(stageSeedPlatform))
	}
	p.seed.mu.Lock()
	app := p.seed.objs[appsPath+"capi"]
	app["status"] = map[string]any{"health": map[string]any{"status": "Healthy"}, "sync": map[string]any{"status": "Synced", "comparedTo": map[string]any{"source": dig(app, "spec", "source")}},
		"operationState": map[string]any{"phase": "Running", "message": "one or more synchronization tasks completed unsuccessfully. Retrying attempt #3",
			"syncResult": map[string]any{"resources": []any{
				map[string]any{"kind": "ServiceAccount", "namespace": "proxcloud-system", "name": "proxcloud-clusterctl", "status": "Synced", "message": "serviceaccount/proxcloud-clusterctl created"},
				map[string]any{"kind": "Job", "namespace": "proxcloud-system", "name": "proxcloud-clusterctl", "hookPhase": "Failed", "message": "Job has reached the specified backoff limit"}}}}}
	p.seed.mu.Unlock()
	if end := p.once(); end != "defer:waiting_seed_platform" || len(p.seed.syncs) != 0 {
		t.Fatalf("end %s, syncs %v", end, p.seed.syncs)
	}
	s := p.stage(stageSeedPlatform)
	i := slices.IndexFunc(s.Items, func(it StageItem) bool { return it.ID == "capi" })
	const hook = "Job proxcloud-system/proxcloud-clusterctl: Job has reached the specified backoff limit"
	if i < 0 || s.Code != CodeCAPIInitFailed || s.Items[i].State != "Synced/Healthy, sync Running" || !strings.Contains(s.Items[i].Message, hook) ||
		strings.Contains(s.Items[i].Message, "serviceaccount/") || !strings.Contains(s.Message, "capi (Synced/Healthy, sync Running)") || !strings.Contains(s.Message, hook) ||
		!strings.Contains(s.Message, "Bootstrap kubeadm:") || !strings.Contains(s.Message, "ControlPlane kubeadm:") {
		t.Fatalf("stage %+v", s)
	}
	// The providers are there (an earlier attempt's hook installed them) and the operation still runs: not done.
	p.seed.mu.Lock()
	p.seed.onSync("capi")
	p.seed.mu.Unlock()
	if end := p.once(); end != "defer:waiting_seed_platform" || p.stage(stageSeedPlatform).Code != CodeComponentUnhealthy {
		t.Fatalf("a running sync passed the gate: %s %+v", end, p.stage(stageSeedPlatform))
	}
	p.seed.mu.Lock()
	app["status"].(map[string]any)["operationState"] = map[string]any{"phase": "Succeeded"}
	p.seed.mu.Unlock()
	if end := p.once(); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(stageSeedPlatform))
	}
}

// The argocd-manager token is renewed only when under 90 days are left or the service account was recreated.
func TestManagerTokenRenewal(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.only(stageWorkloadAccess)
	run := func(want int, why string) {
		t.Helper()
		if end := p.once(); end != "complete" || len(p.wl.tokens) != want {
			t.Fatalf("%s: end %s, %d tokens", why, end, len(p.wl.tokens))
		}
		p.again(stageWorkloadAccess)
	}
	run(1, "first run")
	run(1, "a year left")
	annotations := p.seed.objs[managerSecret]["metadata"].(map[string]any)["annotations"].(map[string]any)
	annotations[tokenExpiryAnnotation] = time.Now().Add(91 * 24 * time.Hour).UTC().Format(time.RFC3339)
	run(1, "91 days left")
	annotations = p.seed.objs[managerSecret]["metadata"].(map[string]any)["annotations"].(map[string]any)
	annotations[tokenExpiryAnnotation] = time.Now().Add(89 * 24 * time.Hour).UTC().Format(time.RFC3339)
	run(2, "89 days left")
	p.wl.objs[seedNS+"kube-system/serviceaccounts/argocd-manager"]["metadata"].(map[string]any)["uid"] = "recreated"
	run(3, "a new service account uid")
	if got := str(p.seed.obj(managerSecret), "metadata", "annotations", managerUIDAnnotation); got != "recreated" {
		t.Fatalf("uid annotation %q", got)
	}
}

// Stage 10 fails provider_conflict when a row that is not the installation's is in the way.
func TestRegisterConflict(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.only(stageRegister)
	p.conflict = fmt.Errorf("%w: a provider named %q exists", ErrProviderConflict, "lab Kubernetes")
	if end := p.once(); end != "fail:"+CodeProviderConflict || !strings.Contains(p.stage(stageRegister).Message, "lab Kubernetes") {
		t.Fatalf("end %s %+v", end, p.stage(stageRegister))
	}
}

// A row is adopted by config.provider plus the marker, and only the owned keys change on it; another row under the
// same name, or two rows with the marker, are a conflict; no row means create.
func TestAdoptProvider(t *testing.T) {
	cur, _ := Current()
	body, _ := ProviderBodies(cur.BOM, RenderInput{ID: inID, Name: "lab", Region: "r1", PVEAPIURL: "https://pve:8006"}, "KUBECONFIG")
	if row, err := AdoptProvider(nil, body); row != nil || err != nil {
		t.Fatalf("no rows: %v %v", row, err)
	}
	// The replicas offered follow the dbaas group: a row adopted after the group dropped below three PVE nodes offers 1.
	_, dbBody := ProviderBodies(cur.BOM, RenderInput{ID: inID, Name: "lab", Region: "r1", Cluster: RenderCluster{NodeGroups: []RenderNodeGroup{{Name: "dbaas", Nodes: []string{"pve1", "pve2"}}}}}, "KUBECONFIG")
	wide := map[string]any{"_id": "svc-9", "name": "lab Databases", "config": map[string]any{"provider": "dbaas", ProviderMarker: inID,
		"database": map[string]any{"engines": map[string]any{"postgresql": map[string]any{"replicas": []any{1, 3}, "versions": "the operator's"}}}}}
	if row, err := AdoptProvider([]map[string]any{wide}, dbBody); err != nil || !reflect.DeepEqual(dig(row, "config", "database", "engines", "postgresql", "replicas"), []any{1}) ||
		dig(row, "config", "database", "engines", "postgresql", "versions") != "the operator's" {
		t.Fatalf("replicas on an adopted row: %v %v", row, err)
	}
	mine := map[string]any{"_id": "svc-1", "name": "renamed by the operator", "status": "DISABLED", "secret": map[string]any{"kubeconfig": "sealed"},
		"config": map[string]any{"provider": "kamaji", ProviderMarker: inID, "attachToNewProjects": true, "argocd": map[string]any{"chartVersion": "0.0.1"},
			"cluster": map[string]any{"versions": map[string]any{"1.34": "img"}, "pveApiUrl": "https://old:8006"}}}
	other := map[string]any{"_id": "svc-2", "name": "installer Kubernetes", "config": map[string]any{"provider": "kamaji"}}
	database := map[string]any{"_id": "svc-3", "name": "x", "config": map[string]any{"provider": "dbaas", ProviderMarker: inID}}
	row, err := AdoptProvider([]map[string]any{other, database, mine}, body)
	if err != nil || str(row, "_id") != "svc-1" {
		t.Fatalf("adopt: %v %v", row, err)
	}
	if str(row, "name") != "renamed by the operator" || str(row, "status") != "DISABLED" || dig(row, "config", "attachToNewProjects") != true ||
		str(row, "config", "cluster", "versions", "1.34") != "img" || str(row, "secret", "kubeconfig") != "sealed" {
		t.Fatalf("keys the stage does not own changed: %v", row)
	}
	if str(row, "config", "cluster", "pveApiUrl") != "https://pve:8006" || str(row, "config", "argocd", "chartVersion") != cur.TenantCharts.Kubernetes.Version ||
		dig(row, "config", "namespacePlacement") == nil {
		t.Fatalf("owned keys not rewritten: %v", row["config"])
	}
	// A Remove disables the row and notes the status it had; the next install's stage 10 gives that status back. The row
	// above, disabled by the operator, stayed disabled.
	removed := map[string]any{"_id": "svc-5", "name": "lab Kubernetes", "status": "ACTIVE", "config": map[string]any{"provider": "kamaji", ProviderMarker: inID}}
	if !DisableProvider(removed) || DisableProvider(removed) || str(removed, "status") != "DISABLED" {
		t.Fatalf("disable: %v", removed)
	}
	if row, err := AdoptProvider([]map[string]any{removed}, body); err != nil || str(row, "status") != "ACTIVE" || dig(row, "config", providerStatusBefore) != nil {
		t.Fatalf("a row disabled by a Remove: %v %v", row, err)
	}
	if DisableProvider(mine) {
		t.Fatal("a row that is disabled already was marked as disabled by the Remove")
	}
	other["name"] = "lab Kubernetes"
	if _, err := AdoptProvider([]map[string]any{other}, body); !errors.Is(err, ErrProviderConflict) {
		t.Fatalf("a foreign row under the name: %v", err)
	}
	twin := map[string]any{"_id": "svc-4", "name": "y", "config": map[string]any{"provider": "kamaji", ProviderMarker: inID}}
	if _, err := AdoptProvider([]map[string]any{mine, twin}, body); !errors.Is(err, ErrProviderConflict) {
		t.Fatalf("two marked rows: %v", err)
	}
}

// removal is a platform that an apply built, at the start of a remove.
func removal(t *testing.T) *platform {
	p := newPlatform(t, RunRemove)
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) {
		in.Status.Clusters = []ClusterStatus{{Name: ClusterName}}
		in.Status.Volumes = []Volume{{Handle: "lab/pve1/local-lvm/vm-9999-pvc-old", Component: "kamaji-etcd", PVC: "kamaji-system/old"}}
		in.Status.Providers = &ProviderIDs{Kubernetes: "svc-kamaji", Database: "svc-dbaas"}
	})
	p.seed.put(appsPath+"pc-platform", meta("pc-platform", nil))
	p.pve.vms[120].cfg["protection"] = "1"
	return p
}

// The cluster part of a remove takes the cluster and the volumes out of the status when it is done. A Remove started
// again after the seed VM was deleted then has nothing to ask of Cluster API and needs no confirmation.
func TestRemoveResumesAfterTheSeedIsGone(t *testing.T) {
	p := removal(t)
	p.only(StageRemoveCluster)
	p.seed.mu.Lock()
	for k := range p.seed.objs {
		if strings.HasPrefix(k, capiNS+"machines/") {
			delete(p.seed.objs, k)
		}
	}
	p.seed.mu.Unlock()
	if end := p.until(8); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(StageRemoveCluster))
	}
	if _, in, _ := p.state.snapshot(); len(in.Status.Clusters)+len(in.Status.Volumes) != 0 {
		t.Fatalf("status after the cluster part: %+v", in.Status)
	}
	delete(p.pve.vms, 120)
	p.again(StageRemoveCluster)
	disabled, k8s, pveWrites := p.disabled, p.k8sWrites(), len(p.pve.writes)
	if end := p.once(); end != "complete" || p.stage(StageRemoveCluster).State != StageDone {
		t.Fatalf("a Remove after the seed VM is gone: %s %+v", end, p.stage(StageRemoveCluster))
	}
	if p.disabled != disabled || p.k8sWrites() != k8s || len(p.pve.writes) != pveWrites {
		t.Fatalf("something was written: %v %v", p.seed.writes, p.pve.writes)
	}
}

// A remove is refused while tenant Applications exist on the workload: nothing is disabled, deleted or written.
func TestRemoveRefusedWithTenants(t *testing.T) {
	p := removal(t)
	p.wl.put(appsPath+"pc-1234-prod", meta("pc-1234-prod", map[string]any{"app.kubernetes.io/managed-by": "proxcloud"}))
	if end := p.once(); end != "fail:"+CodeTenantsExist || !strings.Contains(p.stage(StageRemoveCluster).Message, "pc-1234-prod") {
		t.Fatalf("end %s %+v", end, p.stage(StageRemoveCluster))
	}
	if p.disabled != 0 || p.k8sWrites() != 0 || len(p.pve.writes) != 0 || p.seed.obj(capiNS+"clusters/pc-platform") == nil {
		t.Fatalf("something changed: disabled %d, kubernetes %v %v, proxmox %v", p.disabled, p.seed.writes, p.wl.writes, p.pve.writes)
	}
	// A workload that does not answer cannot say there are none: retried, and passed only with the confirmation.
	p = removal(t)
	p.wl.sa = "another-token"
	if end := p.once(); end != "defer:retry_remove_cluster" || p.stage(StageRemoveCluster).Code != CodeWorkloadUnreachable || p.k8sWrites() != 0 {
		t.Fatalf("silent workload: %s %+v", end, p.stage(StageRemoveCluster))
	}
	p.mutate(func(run *Run, _ *Installation, _ *Secrets) { run.Confirmed = true })
	if end, s := p.once(), p.stage(StageRemoveCluster); end != "defer:waiting_remove_cluster" || p.seed.obj(capiNS+"clusters/pc-platform") != nil || item(s, "volumes") != nil ||
		item(s, "tenants") == nil || item(s, "tenants").State != "not checked" {
		t.Fatalf("confirmed: %s %+v", end, p.stage(StageRemoveCluster))
	}
	// Passed on the confirmation with no volume recorded: the stage says that it could not look for one.
	p = removal(t)
	p.wl.sa = "another-token"
	p.mutate(func(run *Run, in *Installation, _ *Secrets) { run.Confirmed, in.Status.Volumes = true, nil })
	if end, v := p.once(), item(p.stage(StageRemoveCluster), "volumes"); end != "defer:waiting_remove_cluster" || v == nil || v.State != "not checked" {
		t.Fatalf("confirmed without a recorded volume: %s %+v", end, p.stage(StageRemoveCluster))
	}
}

// A cluster whose control plane never came up never could hold a tenant: it is removed without the tenant check
// standing in the way, a silent API is neither retried nor needs the confirmation. That no provider is recorded says
// nothing: stage 10 records them only after both are registered, and the first row is public once it exists. Such a
// cluster's silence is retried, needs the confirmation, and is passed with the note on the volumes.
func TestRemoveOfAClusterWithoutTenants(t *testing.T) {
	p := removal(t)
	p.seed.put(capiNS+"clusters/pc-platform", map[string]any{"metadata": map[string]any{"name": ClusterName}})
	p.wl.sa = "another-token" // the kubeconfig is on the seed and the API does not answer with it
	// Its workers have no VM yet: the message and the items say so, not "VM 0".
	const worker = "/apis/infrastructure.cluster.x-k8s.io/v1alpha2/namespaces/pc-platform/proxmoxmachines/pc-platform-dbaas-pve3-infra"
	p.seed.mu.Lock()
	delete(p.seed.objs[worker], "spec")
	p.seed.mu.Unlock()
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Status.Volumes = nil })
	if end := p.once(); end != "defer:waiting_remove_cluster" || p.stage(StageRemoveCluster).Code != CodeMachineDeleteStuck ||
		p.seed.obj(capiNS+"clusters/pc-platform") != nil || p.disabled != 1 {
		t.Fatalf("control plane never initialized: end %s %+v", end, p.stage(StageRemoveCluster))
	}
	if s := p.stage(StageRemoveCluster); strings.Contains(s.Message, "VM 0") || !strings.Contains(s.Message, "pc-platform-dbaas-pve3-x (no VM)") ||
		item(s, "pc-platform-dbaas-pve3-x") == nil || item(s, "pc-platform-dbaas-pve3-x").Message != "no VM" {
		t.Fatalf("a machine without a VM: %+v", s)
	}
	// The finished stage says what was deleted and what it did not check.
	p.seed.mu.Lock()
	for k := range p.seed.objs {
		if strings.HasPrefix(k, capiNS+"machines/") {
			delete(p.seed.objs, k)
		}
	}
	p.seed.mu.Unlock()
	if end := p.until(8); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(StageRemoveCluster))
	}
	s := p.stage(StageRemoveCluster)
	if tn, m, w, v := item(s, "tenants"), item(s, "pc-platform-cp-x"), item(s, "pc-platform-dbaas-pve3-x"), item(s, "volumes"); tn == nil || tn.State != "not checked" ||
		!strings.Contains(tn.Message, "never came up") || m == nil || m.State != "deleted" || m.Message != "VM 130" || w == nil || w.State != "deleted" ||
		v == nil || v.State != "none recorded" || item(s, "cluster") == nil {
		t.Fatalf("items of the finished stage: %+v", s.Items)
	}
	p = removal(t)
	p.seed.put(capiNS+"clusters/pc-platform", map[string]any{"metadata": map[string]any{"name": ClusterName}})
	p.wl.sa = "another-token"
	p.once()
	p = removal(t)
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Status.Providers, in.Status.Volumes = nil, nil })
	p.wl.sa = "another-token"
	if end := p.once(); end != "defer:retry_remove_cluster" || p.stage(StageRemoveCluster).Code != CodeWorkloadUnreachable || p.k8sWrites() != 0 || p.disabled != 0 {
		t.Fatalf("no provider recorded: end %s %+v", end, p.stage(StageRemoveCluster))
	}
	p.mutate(func(run *Run, _ *Installation, _ *Secrets) { run.Confirmed = true })
	if end, s := p.once(), p.stage(StageRemoveCluster); end != "defer:waiting_remove_cluster" || item(s, "volumes") == nil || item(s, "volumes").State != "not checked" {
		t.Fatalf("no provider recorded, confirmed: end %s %+v", end, s)
	}
}

// The Cluster is read before the Application is deleted, and a sync that was running may create it in between: after
// deleting the Application of a seed that showed no Cluster, the stage looks once more before it calls the cluster gone.
func TestRemoveReadsTheClusterAgain(t *testing.T) {
	p := removal(t)
	cl := p.seed.obj(capiNS + "clusters/pc-platform")
	p.seed.mu.Lock()
	for k := range p.seed.objs {
		if strings.HasPrefix(k, capiNS+"machines/") || k == capiNS+"clusters/pc-platform" {
			delete(p.seed.objs, k)
		}
	}
	p.seed.mu.Unlock()
	if end := p.once(); end != "defer:waiting_remove_cluster" || p.seed.obj(appsPath+"pc-platform") != nil || len(p.pve.writes) != 0 {
		t.Fatalf("end %s: %+v, proxmox %v", end, p.stage(StageRemoveCluster), p.pve.writes)
	}
	if _, in, _ := p.state.snapshot(); len(in.Status.Clusters) != 1 {
		t.Fatalf("the cluster was called gone on the read before the delete: %+v", in.Status)
	}
	p.seed.put(capiNS+"clusters/pc-platform", cl) // what the running sync made meanwhile
	if end := p.once(); end != "defer:waiting_remove_cluster" || p.seed.obj(capiNS+"clusters/pc-platform") != nil {
		t.Fatalf("the late Cluster was not deleted: %s %+v", end, p.stage(StageRemoveCluster))
	}
	if end := p.until(8); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(StageRemoveCluster))
	}
}

// The cluster part of a remove: providers disabled, the volumes read again, the Application deleted and then the
// Cluster, the wait for the machines (the stuck ones named with their VMIDs), then exactly the recorded volumes deleted
// with the CSI token, and the seed part after it.
func TestRemoveCluster(t *testing.T) {
	p := removal(t)
	if end := p.once(); end != "defer:waiting_remove_cluster" {
		t.Fatalf("end %s: %+v", end, p.stage(StageRemoveCluster))
	}
	_, in, _ := p.state.snapshot()
	s := p.stage(StageRemoveCluster)
	if p.disabled != 1 || p.seed.obj(appsPath+"pc-platform") != nil || p.seed.obj(capiNS+"clusters/pc-platform") != nil ||
		s.Code != CodeMachineDeleteStuck || !strings.Contains(s.Message, "pc-platform-kamaji-pve2-x (VM 132)") ||
		len(in.Status.Volumes) != 1 || in.Status.Volumes[0].Handle != "lab/pve1/local-lvm/vm-9999-pvc-etcd" {
		t.Fatalf("stage %+v, volumes %+v, seed writes %v", s, in.Status.Volumes, p.seed.writes)
	}
	if i, j := slices.Index(p.seed.writes, "DELETE "+appsPath+"pc-platform"), slices.Index(p.seed.writes, "DELETE "+capiNS+"clusters/pc-platform"); i < 0 || j < i {
		t.Fatalf("the Application must go before the Cluster: %v", p.seed.writes)
	}
	if len(p.pve.writes) != 0 {
		t.Fatalf("Proxmox written while machines are left: %v", p.pve.writes)
	}
	// CAPMOX deletes a ProxmoxMachine before its Machine goes: the VMID seen at the first look stays (F45).
	p.seed.mu.Lock()
	for k := range p.seed.objs {
		if strings.Contains(k, "/proxmoxmachines/") {
			delete(p.seed.objs, k)
		}
	}
	p.seed.mu.Unlock()
	if end := p.once(); end != "defer:waiting_remove_cluster" {
		t.Fatalf("end %s", end)
	}
	if s := p.stage(StageRemoveCluster); !strings.Contains(s.Message, "pc-platform-kamaji-pve2-x (VM 132)") || item(s, "pc-platform-kamaji-pve2-x").Message != "VM 132" {
		t.Fatalf("the VMID was lost with the ProxmoxMachine: %+v", s)
	}
	p.seed.mu.Lock()
	for k := range p.seed.objs {
		if strings.HasPrefix(k, capiNS+"machines/") {
			delete(p.seed.objs, k)
		}
	}
	p.seed.mu.Unlock()
	if end := p.until(8); end != "complete" {
		t.Fatalf("end %s: %s", end, p.logs)
	}
	if !slices.Equal(p.pve.disks, []string{"vm-9999-pvc-tenant"}) {
		t.Fatalf("volumes left %v", p.pve.disks)
	}
	for i, w := range p.pve.writes {
		if csi := p.pve.writers[i] == pve.CSITokenID; csi != strings.Contains(w, "/storage/local-lvm/content/") {
			t.Fatalf("%s came with %s", w, p.pve.writers[i])
		}
	}
	run, in, sec := p.state.snapshot()
	if run.State != RunSucceeded || in.Status.Phase != PhaseRemoved || len(in.Status.Clusters)+len(in.Status.Volumes) != 0 || sec.CSI != nil || p.pve.vms[120] != nil {
		t.Fatalf("run %s, status %+v", run.State, in.Status)
	}
	// The finished run says what it deleted: every machine with its VM, the volume, the seed VM, the secrets; and
	// that the cluster was asked for tenants.
	s = p.stage(StageRemoveCluster)
	if tn, m, v := item(s, "tenants"), item(s, "pc-platform-kamaji-pve2-x"), item(s, "volume-vm-9999-pvc-etcd"); tn == nil || tn.State != "none" || m == nil ||
		m.State != "deleted" || m.Message != "VM 132" || v == nil || v.State != "deleted" || !strings.Contains(v.Message, "lab/pve1/local-lvm/vm-9999-pvc-etcd") ||
		item(s, "volumes") != nil || item(p.stage(StageRemoveSeed), "vm-120") == nil || item(p.stage(StageRemoveSeed), "vm-120").State != "gone" ||
		item(p.stage(StageRemoveSecrets), "secrets") == nil {
		t.Fatalf("items: %+v, %+v, %+v", s.Items, p.stage(StageRemoveSeed).Items, p.stage(StageRemoveSecrets).Items)
	}
	p.leaks(t)
}

// The seed VM is gone: the remove needs the typed confirmation, and then deletes only the VMs of proxcloud-system
// tagged for this installation and only the recorded volumes.
func TestRemoveSeedLost(t *testing.T) {
	build := func() *platform {
		p := removal(t)
		delete(p.pve.vms, 120)
		tagged := func(tags string, running bool) *fakeVM {
			return &fakeVM{system: true, running: running, cfg: map[string]any{"name": "vm", "tags": tags}}
		}
		p.pve.vms[201] = tagged("proxcloud;pc-platform;pc-platform-"+inID, true)
		p.pve.vms[202] = tagged("proxcloud;pc-platform;pc-platform-pi-ffffffff", true)
		p.pve.vms[203] = tagged("", false)
		p.pve.vms[204] = &fakeVM{cfg: map[string]any{"name": "tenant", "tags": "pc-platform-" + inID}} // not in the pool
		p.pve.vms[205] = &fakeVM{system: true, cfg: map[string]any{"name": "pc-platform-cp-7xk2p"}}    // CAPMOX had not tagged it yet
		p.pve.disks = append(p.pve.disks, "vm-9999-pvc-old")
		p.pve.iso = []string{"user-data-201.iso", "user-data-202.iso", "user-data-205.iso"}
		return p
	}
	p := build()
	if end := p.once(); end != "fail:"+CodeSeedLost || len(p.pve.writes) != 0 || p.disabled != 0 ||
		!strings.Contains(p.stage(StageRemoveCluster).Message, `"remove lab"`) {
		t.Fatalf("without the confirmation: %s %+v, writes %v", end, p.stage(StageRemoveCluster), p.pve.writes)
	}
	p = build()
	p.mutate(func(run *Run, _ *Installation, _ *Secrets) { run.Confirmed = true })
	if end := p.until(8); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(StageRemoveCluster))
	}
	left := []int{}
	for id := range p.pve.vms {
		left = append(left, id)
	}
	slices.Sort(left)
	if !slices.Equal(left, []int{202, 203, 204, 205}) || !slices.Equal(p.pve.disks, []string{"vm-9999-pvc-etcd", "vm-9999-pvc-tenant"}) ||
		p.disabled == 0 || p.k8sWrites() != 0 {
		t.Fatalf("VMs %v, volumes %v, disabled %d", left, p.pve.disks, p.disabled)
	}
	// The deleted machine's bootstrap ISO went before it, and no other; the machine CAPMOX had not tagged is named.
	// And the finished stage lists the VM and the volume it deleted, and that it could not ask for tenants.
	s := p.stage(StageRemoveCluster)
	if left, gone, vol, tn := item(s, "vm-205"), item(s, "vm-201"), item(s, "volume-vm-9999-pvc-old"), item(s, "tenants"); !slices.Equal(p.pve.iso, []string{"user-data-202.iso", "user-data-205.iso"}) ||
		left == nil || left.State != "left" || gone == nil || gone.State != "deleted" || vol == nil || vol.State != "deleted" || tn == nil || tn.State != "not checked" ||
		item(s, "vm-202") != nil || len(s.Items) != 4 {
		t.Fatalf("ISOs %v, items %+v", p.pve.iso, s.Items)
	}
	if i, j := slices.Index(p.pve.writes, "DELETE /nodes/pve1/storage/local/content/iso/user-data-201.iso"), slices.Index(p.pve.writes, "DELETE /nodes/pve1/qemu/201"); i < 0 || j < i {
		t.Fatalf("the ISO must go before its VM: %v", p.pve.writes)
	}
	p.leaks(t)

	// An apply that failed before stage 7 recorded a cluster, and a seed VM deleted by hand: the item does not say
	// that a Remove deleted a cluster.
	p = removal(t)
	delete(p.pve.vms, 120)
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Status.Clusters, in.Status.Volumes = nil, nil })
	p.only(StageRemoveCluster)
	if end, c := p.once(), item(p.stage(StageRemoveCluster), "cluster"); end != "complete" || c == nil || c.State != "gone" ||
		!strings.Contains(c.Message, "or none was built") || p.k8sWrites() != 0 {
		t.Fatalf("no cluster recorded and no seed VM: end %s, item %+v", end, c)
	}
}

// A lease lost at any checkpoint of the new stages stops every Kubernetes, Proxmox and provider write at once; the
// next acquisitions resume and the end is the same.
func TestLeaseLossInTheClusterStages(t *testing.T) {
	for _, kind := range []string{RunApply, RunRemove} {
		total := -1
		for n := 0; n <= total || total < 0; n++ {
			p := newPlatform(t, kind)
			if kind == RunRemove {
				p = removal(t)
				p.seed.mu.Lock()
				for k := range p.seed.objs {
					if strings.HasPrefix(k, capiNS+"machines/") {
						delete(p.seed.objs, k)
					}
				}
				p.seed.mu.Unlock()
			} else {
				p.upTo(stageSeedPlatform)
			}
			writes := func() int { return p.k8sWrites() + len(p.pve.writes) + len(p.registers) + p.disabled }
			if n > 0 {
				p.jobs.loseAt = n
				atLoss := -1
				p.jobs.onLose = func() { atLoss = writes() }
				for i := 0; i < 40 && atLoss < 0; i++ {
					p.once()
				}
				if atLoss < 0 || writes() != atLoss {
					t.Fatalf("%s, lost at %d: %d writes at the loss, %d after", kind, n, atLoss, writes())
				}
				p.jobs = &fakeJobs{}
				p.r.Jobs = p.jobs
			}
			if end := p.until(40); end != "complete" {
				t.Fatalf("%s, lost at %d: %s", kind, n, end)
			}
			run, in, _ := p.state.snapshot()
			if run.State != RunSucceeded || (kind == RunApply && (len(p.registers) < 2 || in.Status.Providers == nil)) {
				t.Fatalf("%s, lost at %d: run %s", kind, n, run.State)
			}
			if n == 0 {
				total = p.jobs.checkpoints
			}
		}
	}
}

// Rotation: the next token Secret is created, its token checked and stored, and only then every older token Secret
// deleted; a store that refuses (a run started) keeps the old Secret.
func TestRotateSeedCredential(t *testing.T) {
	p := newPlatform(t, RunApply)
	_, in, sec := p.state.snapshot()
	g1, g2 := seedNS+"proxcloud-system/secrets/proxcloud-portal-token-g1", seedNS+"proxcloud-system/secrets/proxcloud-portal-token-g2"
	p.seed.put(g1, meta("proxcloud-portal-token-g1", nil))
	p.seed.put(seedNS+"proxcloud-system/secrets/unrelated", meta("unrelated", nil))
	err := RotateSeedCredential(context.Background(), in, sec, func(context.Context, string, string, string) error { return ErrRunActive })
	if !errors.Is(err, ErrRunActive) || p.seed.obj(g1) == nil || p.seed.obj(g2) != nil {
		t.Fatalf("refused store: %v, old Secret %v, new Secret %v", err, p.seed.obj(g1), p.seed.obj(g2))
	}
	p.seed.saAllowed = false
	if err := RotateSeedCredential(context.Background(), in, sec, func(context.Context, string, string, string) error { t.Error("stored"); return nil }); err == nil ||
		p.seed.obj(g1) == nil || p.seed.obj(g2) != nil {
		t.Fatalf("a token that is not cluster-admin: %v, new Secret %v", err, p.seed.obj(g2))
	}
	p.seed.saAllowed = true
	// A store error that is not a refusal may have committed: the new Secret stays.
	if err := RotateSeedCredential(context.Background(), in, sec, func(context.Context, string, string, string) error { return errors.New("connection lost") }); err == nil ||
		p.seed.obj(g1) == nil || p.seed.obj(g2) == nil {
		t.Fatalf("an ambiguous store error: %v, Secrets %v %v", err, p.seed.obj(g1), p.seed.obj(g2))
	}
	var stored []string
	err = RotateSeedCredential(context.Background(), in, sec, func(_ context.Context, old, next, token string) error {
		if p.seed.obj(g1) == nil {
			t.Error("the old Secret was deleted before the new credential was stored")
		}
		stored = []string{old, next, token}
		return nil
	})
	if err != nil || !slices.Equal(stored, []string{"proxcloud-portal-token-g1", "proxcloud-portal-token-g2", "rotated-token-of-proxcloud-portal-token-g2"}) {
		t.Fatalf("rotation: %v, stored %v", err, stored)
	}
	if p.seed.obj(g1) != nil || p.seed.obj(g2) == nil || p.seed.obj(seedNS+"proxcloud-system/secrets/unrelated") == nil ||
		str(p.seed.obj(g2), "type") != "kubernetes.io/service-account-token" {
		t.Fatalf("Secrets after the rotation: %v", p.seed.writes)
	}
	if strings.Contains(fmt.Sprint(err), "rotated-token") {
		t.Fatal("token in an error")
	}
	if err := RotateSeedCredential(context.Background(), &Installation{}, nil, nil); !errors.Is(err, ErrNoSeed) {
		t.Fatalf("no seed: %v", err)
	}
}

// The break-glass kubeconfig carries a one-hour TokenRequest token of the portal's account, and nothing stores it.
func TestBreakGlassKubeconfig(t *testing.T) {
	p := newPlatform(t, RunApply)
	_, in, sec := p.state.snapshot()
	before := string(p.state.in) + string(p.state.sec)
	kc, expires, err := BreakGlassKubeconfig(context.Background(), in, sec)
	if err != nil || len(p.seed.tokens) != 1 || p.seed.ttls[0] != 3600 || !strings.Contains(kc, p.seed.tokens[0]) || !strings.Contains(kc, p.seed.url) ||
		time.Until(expires) > time.Hour || time.Until(expires) < 59*time.Minute {
		t.Fatalf("kubeconfig: %v, ttls %v, expires %v", err, p.seed.ttls, expires)
	}
	if !slices.Equal(p.seed.writes, []string{"POST /api/v1/namespaces/proxcloud-system/serviceaccounts/proxcloud-portal/token"}) ||
		string(p.state.in)+string(p.state.sec) != before {
		t.Fatalf("something was stored: %v", p.seed.writes)
	}
}

// The platform kubeconfig carries a one-hour TokenRequest token of the break-glass account on the platform cluster,
// its API address and its CA, and never the admin kubeconfig's credential. The account reads no Secret but ArgoCD's
// initial admin password and changes nothing but a port-forward. Nothing is stored, and nothing is written to the seed.
func TestPlatformKubeconfig(t *testing.T) {
	p := newPlatform(t, RunApply)
	_, in, sec := p.state.snapshot()
	before := string(p.state.in) + string(p.state.sec)
	kc, expires, err := PlatformKubeconfig(context.Background(), in, sec)
	if err != nil || len(p.wl.tokens) != 1 || p.wl.ttls[0] != 3600 || !strings.Contains(kc, "token: "+p.wl.tokens[0]) || !strings.Contains(kc, p.wl.url) ||
		strings.Contains(kc, wlAdmin) || strings.Contains(kc, p.seed.url) || time.Until(expires) > time.Hour || time.Until(expires) < 59*time.Minute {
		t.Fatalf("kubeconfig: %v, ttls %v, expires %v\n%s", err, p.wl.ttls, expires, kc)
	}
	if c, err := kamajik8s.New(kc); err != nil || c.Server() != p.wl.url {
		t.Fatalf("the kubeconfig does not parse: %v", err)
	}
	rbac := "/apis/rbac.authorization.k8s.io/v1/"
	if !slices.Equal(p.wl.writes, []string{
		"PATCH /api/v1/namespaces/kube-system/serviceaccounts/proxcloud-break-glass",
		"PATCH " + rbac + "clusterroles/proxcloud-break-glass", "PATCH " + rbac + "clusterrolebindings/proxcloud-break-glass",
		"PATCH " + rbac + "namespaces/argocd/roles/proxcloud-break-glass", "PATCH " + rbac + "namespaces/argocd/rolebindings/proxcloud-break-glass",
		"POST /api/v1/namespaces/kube-system/serviceaccounts/proxcloud-break-glass/token",
	}) || len(p.seed.writes) != 0 || len(p.seed.tokens) != 0 || string(p.state.in)+string(p.state.sec) != before {
		t.Fatalf("writes: platform cluster %v, seed %v", p.wl.writes, p.seed.writes)
	}
	// The rules: cluster-wide only reads, and no rule there names secrets or a wildcard that would hold them.
	for _, rule := range p.wl.obj(rbac + "clusterroles/proxcloud-break-glass")["rules"].([]any) {
		r := rule.(map[string]any)
		groups, resources := fmt.Sprint(r["apiGroups"]), fmt.Sprint(r["resources"])
		if fmt.Sprint(r["verbs"]) != "[get list watch]" || strings.Contains(groups, "*") || strings.Contains(groups, "bootstrap") ||
			strings.Contains(resources, "secrets") || (strings.Contains(resources, "*") && slices.Contains(r["apiGroups"].([]any), any(""))) {
			t.Fatalf("cluster-wide rule %v", r)
		}
	}
	argo, _ := json.Marshal(p.wl.obj(rbac + "namespaces/argocd/roles/proxcloud-break-glass")["rules"])
	if string(argo) != `[{"apiGroups":[""],"resources":["pods/portforward"],"verbs":["get","create"]},`+
		`{"apiGroups":[""],"resourceNames":["argocd-initial-admin-secret"],"resources":["secrets"],"verbs":["get"]}]` {
		t.Fatalf("the Role in argocd: %s", argo)
	}
	for _, b := range []string{rbac + "clusterrolebindings/proxcloud-break-glass", rbac + "namespaces/argocd/rolebindings/proxcloud-break-glass"} {
		if got := p.wl.obj(b); str(got, "roleRef", "name") != "proxcloud-break-glass" || fmt.Sprint(got["subjects"]) != "[map[kind:ServiceAccount name:proxcloud-break-glass namespace:kube-system]]" {
			t.Fatalf("%s: %v", b, got)
		}
	}

	// A cluster that grants more than the hour: no kubeconfig.
	p.wl.grant = 24 * time.Hour
	if kc, _, err := PlatformKubeconfig(context.Background(), in, sec); !errors.Is(err, ErrWorkloadAPI) || kc != "" {
		t.Fatalf("a 24 hour token was handed out: %v", err)
	}
	p.wl.grant = 0
	// The platform cluster refuses: its error, told from the seed's, and without the admin credential in it.
	p.wl.deny = []denial{{403, "forbidden"}}
	if _, _, err := PlatformKubeconfig(context.Background(), in, sec); !errors.Is(err, ErrWorkloadAPI) || strings.Contains(fmt.Sprint(err), wlAdmin) {
		t.Fatalf("a refusing platform cluster: %v", err)
	}
	// No cluster on the seed yet, and no seed.
	p.seed.put(seedNS+"pc-platform/secrets/pc-platform-kubeconfig", map[string]any{"data": map[string]any{}})
	if _, _, err := PlatformKubeconfig(context.Background(), in, sec); !errors.Is(err, ErrNoCluster) {
		t.Fatalf("no admin kubeconfig on the seed: %v", err)
	}
	if _, _, err := PlatformKubeconfig(context.Background(), &Installation{}, nil); !errors.Is(err, ErrNoSeed) {
		t.Fatalf("no seed: %v", err)
	}
}

// The live read derives degraded from ArgoCD and stores nothing.
func TestLive(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Status.Phase = PhaseReady })
	app := func(name, health, server string) {
		a := meta(name, map[string]any{"proxcloud.io/platform-installation": inID, "proxcloud.io/component": name})
		a["spec"] = map[string]any{"destination": map[string]any{"server": server}}
		a["status"] = map[string]any{"sync": map[string]any{"status": "Synced"}, "health": map[string]any{"status": health}}
		p.seed.put(appsPath+name, a)
	}
	_, in, sec := p.state.snapshot()
	if got := Live(context.Background(), in, sec); got.Phase != PhaseDegraded || len(got.Components) != 0 {
		t.Fatalf("no Applications: %+v", got)
	}
	app("capi", "Healthy", inCluster)
	app("cilium", "Healthy", "https://10.200.200.20:6443")
	got := Live(context.Background(), in, sec)
	if got.Phase != PhaseReady || len(got.Components) != 2 || got.Components[0] != (LiveComponent{Name: "capi", Cluster: ClusterSeed, Sync: "Synced", Health: "Healthy"}) ||
		got.Components[1].Cluster != ClusterWorkload {
		t.Fatalf("healthy: %+v", got)
	}
	app("cilium", "Degraded", "https://10.200.200.20:6443")
	if got := Live(context.Background(), in, sec); got.Phase != PhaseDegraded {
		t.Fatalf("degraded: %+v", got)
	}
	sec.SeedKubeconfig.Token = "revoked"
	if got := Live(context.Background(), in, sec); got.Phase != PhaseDegraded || got.Code != CodeSeedAPIUnreachable || strings.Contains(got.Error, "revoked") ||
		!strings.Contains(got.Error, "refuses the stored credential") {
		t.Fatalf("seed refuses: %+v", got)
	}
	// The seed VM runs and nothing listens on its API (a lab seed after a reboot): the message names the VM, its node
	// and the console, and says that the record still reads ready.
	sec.SeedKubeconfig.Token = runSA
	silent := *in
	st := *in.Status.Seed
	st.APIServer, st.VMID, st.Node = "https://127.0.0.1:1", 102, "pve1" // nothing listens on port 1
	silent.Status.Seed = &st
	if got := Live(context.Background(), &silent, sec); got.Phase != PhaseDegraded || got.Code != CodeSeedAPIUnreachable || len(got.Components) != 0 ||
		!strings.Contains(got.Error, "does not answer") || !strings.Contains(got.Error, "seed VM 102 runs on pve1") || !strings.Contains(got.Error, "qm terminal 102") ||
		!strings.Contains(got.Error, "'When the seed does not answer'") { // the section's real title in both guides
		t.Fatalf("seed silent: %+v", got)
	}
	if !strings.Contains(got0(Live(context.Background(), &silent, sec)), "still says ready") {
		t.Fatal("a ready record is named as such")
	}
	// A failed record is not called ready (F43).
	silent.Status.Phase = PhaseFailed
	if e := got0(Live(context.Background(), &silent, sec)); strings.Contains(e, "says ready") || !strings.Contains(e, "The record says failed.") {
		t.Fatalf("failed record: %s", e)
	}
	if _, stored, _ := p.state.snapshot(); stored.Status.Phase != PhaseReady || p.seed.writeCount() != 0 {
		t.Fatalf("stored phase %s, writes %v", stored.Status.Phase, p.seed.writes)
	}
}

func got0(l LiveState) string { return l.Error }

// An API server that has just started refuses a good credential for some seconds: a 401, a 403 with the RBAC
// authorizer's list of roles it has not read yet (the seed's k3s restarted under disk load in a lab run, and stage 7's
// next look ended the run seed_api_unreachable), a bare 403 while it has not read the bindings either, and the 403 of
// an admission plugin that is not ready. No wording tells these from a real refusal, so every Kubernetes 401 and 403
// is a passing error, on the seed and on the workload: the stage tries again and goes on. Not for long: the fifth
// look on end that is refused ends the run with the same code and the server's text.
func TestAnAPIServerThatStartsIsTriedAgain(t *testing.T) {
	const rbac = `namespaces "pc-platform" is forbidden: User "system:serviceaccount:proxcloud-system:proxcloud-portal" cannot patch resource "namespaces" in API group "" ` +
		`at the cluster scope: RBAC: [clusterrole.rbac.authorization.k8s.io "cluster-admin" not found, clusterrole.rbac.authorization.k8s.io "system:discovery" not found]`
	const plain = `namespaces "pc-platform" is forbidden: User "system:serviceaccount:proxcloud-system:proxcloud-portal" cannot patch resource "namespaces" in API group "" at the cluster scope`
	for name, d := range map[string]denial{
		"rbac roles":          {403, rbac},
		"rbac bindings":       {403, plain},
		"admission":           {403, `secrets "pc-platform-pve" is forbidden: not yet ready to handle request`},
		"token authenticator": {401, "Unauthorized"},
	} {
		p := newPlatform(t, RunApply)
		p.only(stageCluster)
		p.seed.deny = []denial{d}
		end, s := p.once(), p.stage(stageCluster)
		if end != "defer:retry_cluster" || s.State != StageWaiting || s.Code != CodeSeedAPIUnreachable || s.Retries != 1 || s.Refused != 1 ||
			!strings.HasPrefix(s.Message, startingUpNote) || !strings.Contains(s.Message, d.message[:12]) || strings.Contains(s.Message, "tries again") {
			t.Fatalf("%s on the seed: end %s %+v", name, end, s)
		}
		if end, s := p.once(), p.stage(stageCluster); end != "complete" || s.Refused != 0 {
			t.Fatalf("%s on the seed, the next look: end %s %+v", name, end, s)
		}
		p.leaks(t)

		p = newPlatform(t, RunApply)
		p.only(stageWorkloadAccess)
		p.wl.deny = []denial{d}
		if end, s := p.once(), p.stage(stageWorkloadAccess); end != "defer:retry_workload_access" || s.Code != CodeWorkloadUnreachable || s.Retries != 1 {
			t.Fatalf("%s on the workload: end %s %+v", name, end, s)
		}
		if end := p.once(); end != "complete" {
			t.Fatalf("%s on the workload, the next look: end %s %+v", name, end, p.stage(stageWorkloadAccess))
		}
	}
	// A refusal that stays ends the run at the fifth look on end, not at the stage's time limit (60 minutes here): a
	// revoked token, a role or a binding that was deleted. The failed stage does not say that an API server starts.
	for name, deny := range map[string]func(*platform){
		"a revoked token":   func(p *platform) { p.seed.sa = "a-token-the-seed-no-longer-knows" }, // every answer is a 401
		"a deleted role":    func(p *platform) { p.seed.deny = slices.Repeat([]denial{{403, rbac}}, refusedLooks) },
		"a deleted binding": func(p *platform) { p.seed.deny = slices.Repeat([]denial{{403, plain}}, refusedLooks) },
	} {
		p := newPlatform(t, RunApply)
		p.only(stageCluster)
		deny(p)
		for i := 1; i < refusedLooks; i++ {
			if end, s := p.once(), p.stage(stageCluster); end != "defer:retry_cluster" || s.Refused != i {
				t.Fatalf("%s, look %d: end %s %+v", name, i, end, s)
			}
		}
		end, s := p.once(), p.stage(stageCluster)
		if end != "fail:"+CodeSeedAPIUnreachable || s.State != StageFailed || s.Retries != refusedLooks-1 || !strings.Contains(s.Message, "5 looks on end") ||
			!(strings.Contains(s.Message, "401: Unauthorized") || strings.Contains(s.Message, "is forbidden")) || strings.Contains(s.Message, "after it starts") {
			t.Fatalf("%s at the fifth look: end %s %+v", name, end, s)
		}
	}
	// Only looks on end count: four refusals, an answer of another kind, and the count starts again.
	p := newPlatform(t, RunApply)
	p.only(stageCluster)
	p.seed.deny = append(slices.Repeat([]denial{{403, rbac}}, refusedLooks-1), denial{503, "etcd is slow"}, denial{403, rbac})
	for range refusedLooks - 1 {
		p.once()
	}
	if end, s := p.once(), p.stage(stageCluster); end != "defer:retry_cluster" || s.Refused != 0 {
		t.Fatalf("a 503 after four refusals: end %s %+v", end, s)
	}
	if end, s := p.once(), p.stage(stageCluster); end != "defer:retry_cluster" || s.Refused != 1 {
		t.Fatalf("a refusal after the 503: end %s %+v", end, s)
	}
	if end := p.once(); end != "complete" {
		t.Fatalf("end %s %+v", end, p.stage(stageCluster))
	}
	// The stage's forty still end a run whose refusals come between other errors.
	p = newPlatform(t, RunApply)
	p.only(stageCluster)
	p.seed.deny = []denial{{403, rbac}, {403, rbac}}
	p.once()
	p.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		run.Stages[slices.Index(stageIDs(RunApply), stageCluster)].Retries = stageRetries
	})
	if end, s := p.once(), p.stage(stageCluster); end != "fail:"+CodeSeedAPIUnreachable || s.State != StageFailed || !strings.Contains(s.Message, "Gave up after 40 retries") ||
		!strings.Contains(s.Message, "RBAC: ") {
		t.Fatalf("the fortieth: end %s %+v", end, s)
	}
	// Any other refusal is final at once: no start explains it.
	p = newPlatform(t, RunApply)
	p.only(stageCluster)
	p.seed.deny = []denial{{422, `Namespace "pc-platform" is invalid`}}
	if end, s := p.once(), p.stage(stageCluster); end != "fail:"+CodeSeedAPIUnreachable || s.State != StageFailed || s.Retries != 0 {
		t.Fatalf("a 422: end %s %+v", end, s)
	}
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&kamajik8s.APIError{Status: 401, Message: "Unauthorized"}, true},
		{&kamajik8s.APIError{Status: 403, Message: rbac}, true},
		{&kamajik8s.APIError{Status: 403, Message: "forbidden"}, true},
		{&kamajik8s.APIError{Status: 404, Message: `RBAC: role "x" not found`}, false},
		{&kamajik8s.APIError{Status: 422, Message: "invalid"}, false},
		{&pve.APIError{Status: 401}, false},
		{&pve.APIError{Status: 403}, false},
		{errors.New("connection refused"), false},
	} {
		if got := startingUp(c.err); got != c.want {
			t.Errorf("startingUp(%v) = %t", c.err, got)
		}
	}
	if err := classify(CodeSeedCloneFailed, &pve.APIError{Status: 401}); !strings.HasPrefix(err.Error(), CodeSeedCloneFailed) || err.(*stageErr).kind != kindFail {
		t.Errorf("a Proxmox 401 must stay a refusal: %v", err)
	}
}

// A machine Cluster API has made but CAPMOX has not cloned yet has no VMID: its item and the message say "no VM".
func TestMachineWithoutAVM(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.only(stageCluster)
	p.seed.mu.Lock()
	p.seed.objs[capiNS+"machines/pc-platform-kamaji-pve2-x"]["status"] = map[string]any{"phase": "Pending", "conditions": []any{map[string]any{"type": "Ready",
		"status": "False", "reason": "NotReady", "message": "* InfrastructureReady: WaitingForBootstrapData"}}}
	delete(p.seed.objs["/apis/infrastructure.cluster.x-k8s.io/v1alpha2/namespaces/pc-platform/proxmoxmachines/pc-platform-kamaji-pve2-infra"], "spec")
	p.seed.mu.Unlock()
	end, s := p.once(), p.stage(stageCluster)
	if it := item(s, "pc-platform-kamaji-pve2-x"); end != "defer:waiting_cluster" || it == nil || !strings.HasPrefix(it.Message, "no VM: Cluster API:") ||
		!strings.Contains(s.Message, "pc-platform-kamaji-pve2-x (no VM):") {
		t.Fatalf("end %s, items %+v, message %q", end, s.Items, s.Message)
	}
	if dump, _ := json.Marshal(s); strings.Contains(string(dump), "VM 0") {
		t.Fatalf("VM 0 is shown: %s", dump)
	}
}

// ArgoCD can still show what an earlier attempt failed on (a condition it has not cleared yet) when the Application
// is Synced and Healthy with its sync Succeeded: the item of a finished Application carries no such text.
func TestFinishedApplicationItemHasNoOldFailure(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.only(stageSeedPlatform)
	if end := p.until(5); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(stageSeedPlatform))
	}
	p.seed.mu.Lock()
	p.seed.unsynced["cert-manager"] = true // the status below stays as it is
	p.seed.objs[appsPath+"cert-manager"]["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "SyncError",
		"message": "Failed last sync attempt: namespace auto creation failed: dial tcp 10.90.0.10:6443: connect: no route to host"}}
	p.seed.mu.Unlock()
	p.again(stageSeedPlatform)
	if end, it := p.once(), item(p.stage(stageSeedPlatform), "cert-manager"); end != "complete" || it == nil || it.State != "Synced/Healthy" || it.Message != "" {
		t.Fatalf("end %s, item %+v", end, it)
	}
}

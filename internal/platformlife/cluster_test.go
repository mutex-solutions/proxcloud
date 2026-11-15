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
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v\n%s", end, p.stage(stageWorkloadPlatform), p.logs)
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
	s := p.stage(stageWorkloadPlatform)
	if s.Code != CodeComponentUnhealthy || s.Progress != "wave 1 of 4" || len(s.Items) != 1 || s.Items[0].ID != "pc-platform-cilium" ||
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
	if end := p.once(); end != "defer:waiting_workload_platform" || p.stage(stageWorkloadPlatform).Progress != "wave 2 of 4" {
		t.Fatalf("end %s: %+v", end, p.stage(stageWorkloadPlatform))
	}
	first := p.stage(stageWorkloadPlatform)
	for range 3 {
		if end := p.once(); end != "defer:waiting_workload_platform" {
			t.Fatalf("end %s", end)
		}
	}
	if s := p.stage(stageWorkloadPlatform); !s.Deadline.Equal(*first.Deadline) || !s.LastProgressAt.Equal(*first.LastProgressAt) || s.Progress != "wave 2 of 4" {
		t.Fatalf("the deadline moved while wave 2 is stuck: %v -> %v (%s)", first.Deadline, s.Deadline, s.Progress)
	}
	// Wave 1 pending again shows in the items and leaves the progress and the deadline where they were.
	p.seed.unsynced["pc-platform-cilium"] = true
	p.seed.objs[appsPath+"pc-platform-cilium"]["status"] = map[string]any{"sync": map[string]any{"status": "OutOfSync"}, "health": map[string]any{"status": "Degraded"}}
	if end := p.once(); end != "defer:waiting_workload_platform" {
		t.Fatalf("end %s", end)
	}
	if s := p.stage(stageWorkloadPlatform); !s.Deadline.Equal(*first.Deadline) || s.Progress != "wave 2 of 4" || len(s.Items) != 1 || s.Items[0].State != "OutOfSync/Degraded" {
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
		p.stage(stageWorkloadPlatform).Progress != "wave 1 of 4" || p.seed.obj(appsPath+"pc-platform-cert-manager") != nil {
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
	if end := p.once(); end != "defer:waiting_remove_cluster" || p.seed.obj(capiNS+"clusters/pc-platform") != nil {
		t.Fatalf("confirmed: %s", end)
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
		p.pve.disks = append(p.pve.disks, "vm-9999-pvc-old")
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
	if !slices.Equal(left, []int{202, 203, 204}) || !slices.Equal(p.pve.disks, []string{"vm-9999-pvc-etcd", "vm-9999-pvc-tenant"}) ||
		p.disabled == 0 || p.k8sWrites() != 0 {
		t.Fatalf("VMs %v, volumes %v, disabled %d", left, p.pve.disks, p.disabled)
	}
	p.leaks(t)
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
	if got := Live(context.Background(), in, sec); got.Phase != PhaseDegraded || got.Error == "" || strings.Contains(got.Error, "revoked") {
		t.Fatalf("seed refuses: %+v", got)
	}
	if _, stored, _ := p.state.snapshot(); stored.Status.Phase != PhaseReady || p.seed.writeCount() != 0 {
		t.Fatalf("stored phase %s, writes %v", stored.Status.Phase, p.seed.writes)
	}
}

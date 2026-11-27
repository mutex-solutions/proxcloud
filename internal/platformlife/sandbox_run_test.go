package platformlife

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

const sandboxApp = appsPath + "pc-platform-sandbox"

// sandboxPlatform is newPlatform under a BOM that offers sandboxes, with one more machine of group sandbox on the
// fakes when shared is asked for. The fakes are Kubernetes and Proxmox as the run talks to them, not as they behave:
// ArgoCD here syncs at once and deletes an Application at once, Cluster API never builds or removes a machine, and the
// fake Proxmox accepts every ACL write unless told otherwise. What a real ArgoCD, CAPMOX and Proxmox do with these
// requests is the lab drill's to show.
func sandboxPlatform(t *testing.T, kind string, sb *SandboxSpec) *platform {
	t.Helper()
	sandboxBOM(t)
	p := newPlatform(t, kind)
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Spec.Sandbox = sb })
	if sb != nil && sb.Placement == SandboxShared {
		p.sandboxMachine()
		p.sandboxIdentities(p.wl)
	}
	if sb != nil && sb.Placement == SandboxSeparate {
		// pc-sandbox answers at an API of its own; Kamaji keeps its admin kubeconfig beside the control plane.
		p.sbx = newFakeSeed(t, staticCA(t), func() string { return "" })
		p.sbx.sa = "sbx-admin-0123456789"
		kc, err := kamajik8s.BuildKubeconfig(SandboxClusterName, p.sbx.url, []byte(p.sbx.caPEM()), p.sbx.sa)
		if err != nil {
			t.Fatal(err)
		}
		p.wl.put(seedNS+"pc-sandbox/secrets/pc-sandbox-kamaji-cp-admin-kubeconfig", map[string]any{"data": map[string]any{"admin.conf": b64(kc)}})
		p.sandboxIdentities(p.sbx)
	}
	return p
}

const (
	sandboxPortalSecret   = "sbx-portal-token-0123456789"
	sandboxFirewallSecret = "sbx-firewall-token-0123456789"
)

// sandboxIdentities are the token Secrets the sandbox chart's two identities get from the cluster that runs it.
func (p *platform) sandboxIdentities(on *fakeSeed) {
	for name, token := range map[string]string{sandboxPortalToken: sandboxPortalSecret, sandboxFirewallToken: sandboxFirewallSecret} {
		on.put(seedNS+"sandbox-system/secrets/"+name, map[string]any{"data": map[string]any{"token": b64(token), "ca.crt": b64(on.caPEM())}})
	}
}

func (p *platform) sandboxMachine() {
	name := "pc-platform-sandbox-pve1"
	m := meta(name+"-x", map[string]any{"cluster.x-k8s.io/cluster-name": ClusterName})
	m["spec"], m["status"] = map[string]any{"infrastructureRef": map[string]any{"name": name + "-infra"}}, map[string]any{"phase": "Running"}
	p.seed.put(capiNS+"machines/"+name+"-x", m)
	pm := meta(name+"-infra", nil)
	pm["spec"] = map[string]any{"virtualMachineID": 140}
	p.seed.put("/apis/infrastructure.cluster.x-k8s.io/v1alpha2/namespaces/pc-platform/proxmoxmachines/"+name+"-infra", pm)
	p.seed.put(capiNS+"machinedeployments/"+name, meta(name, map[string]any{"cluster.x-k8s.io/cluster-name": ClusterName, "proxcloud.io/node-group": SandboxGroup}))
	n := meta(name, map[string]any{GroupLabel: SandboxGroup})
	n["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	p.wl.put("/api/v1/nodes/"+name, n)
}

// An apply with placement shared: the portal token makes the pool and writes the one ACL row before the cluster's
// Application names the pool, the group is in that Application's values, the proxy's certificate is on the platform
// cluster before the sandbox chart's Application, and the record says what was built.
func TestApplyBuildsTheSharedSandbox(t *testing.T) {
	p := sandboxPlatform(t, RunApply, sharedSandbox())
	p.upTo(stageSeedPlatform)
	var order []string // the first write of each kind, on Proxmox and on the seed
	p.pve.onWrite = func(w string) {
		if w == "POST /pools" || w == "PUT /access/acl" {
			order = append(order, w)
		}
	}
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v\n%+v\n%s", end, p.stage(stageCluster), p.stage(stageWorkloadPlatform), p.logs)
	}
	_, in, sec := p.state.snapshot()
	if !slices.Equal(p.pve.pools, []string{pve.SandboxPool}) {
		t.Errorf("pools %v", p.pve.pools)
	}
	if got := slices.Compact(slices.Clone(p.pve.acl)); !slices.Equal(got, []string{"/pool/proxcloud-sbx ProxCloudCAPI capmox@pve!capi delete=0"}) {
		t.Errorf("ACL writes %v: one pool, one token", p.pve.acl)
	}
	for i, w := range p.pve.writes {
		if (w == "POST /pools" || w == "PUT /access/acl") && p.pve.writers[i] != "proxcloud@pve!platform" {
			t.Errorf("%s was written by %s, not by the portal token", w, p.pve.writers[i])
		}
	}
	if len(p.pve.tokens) != 0 {
		t.Errorf("shared mints no token: %v", p.pve.tokens)
	}
	app := p.seed.obj(appsPath + "pc-platform")
	groups, _ := dig(app, "spec", "source", "helm", "valuesObject", "nodeGroups").([]any)
	last, _ := groups[len(groups)-1].(map[string]any)
	if len(groups) != 4 || last["name"] != SandboxGroup || last["pool"] != pve.SandboxPool || len(order) == 0 || order[0] != "POST /pools" {
		t.Fatalf("the cluster's node groups: %v; Proxmox writes in order %v", groups, order)
	}
	// The pool and the row are there before the Application that names the pool.
	if i := slices.Index(p.seed.applied, "pc-platform@"+ClusterChartVersion(in2bom(t))); i < 0 {
		t.Fatalf("applied %v", p.seed.applied)
	}
	if sbx := p.seed.obj(sandboxApp); sbx == nil || str(sbx, "spec", "source", "helm", "valuesObject", "mode") != SandboxShared ||
		str(sbx, "spec", "source", "helm", "valuesObject", "proxy", "address") != "10.200.200.210" {
		t.Fatalf("the sandbox chart's Application: %v", sbx)
	}
	tls := p.wl.obj(seedNS + "sandbox-system/secrets/sandbox-proxy-tls")
	if tls == nil || tls["type"] != "kubernetes.io/tls" || !certificateFor(sec.SandboxTLSCert, "sbx.example.org", time.Now().AddDate(4, 0, 0)) || sec.SandboxTLSKey == "" {
		t.Fatalf("the proxy's certificate: %v", tls)
	}
	st := in.Status.Sandbox
	if st == nil || st.Placement != SandboxShared || st.NodeRange != "10.200.200.64/28" || st.ProxyAddress != "10.200.200.210" || st.Count != 1 || st.BuiltAt == nil ||
		st.Certificate != sec.SandboxTLSCert || !slices.Equal(st.Nodes, []string{"pve1"}) {
		t.Fatalf("status %+v", st)
	}
	if !slices.ContainsFunc(in.Status.Components, func(c ComponentStatus) bool { return c.Name == "sandbox" && c.Cluster == ClusterWorkload }) {
		t.Errorf("the sandbox component is not recorded: %+v", in.Status.Components)
	}
	p.leaks(t, sec.SandboxTLSKey)

	// A second apply keeps the certificate (what it writes on Proxmox: TestSandboxAccessIsWrittenOnce).
	cert := sec.SandboxTLSCert
	for _, id := range []string{stageCluster, stageWorkloadPlatform} {
		p.again(id)
	}
	if end := p.until(30); end != "complete" {
		t.Fatalf("second apply: %s", end)
	}
	if _, _, sec := p.state.snapshot(); sec.SandboxTLSCert != cert {
		t.Error("second apply: a new certificate")
	}
	// The record holds what the last run handed to Cluster API, not the largest count ever: after an apply with fewer
	// machines, more machines are machines nothing has placed, and the capacity row fails for them again.
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Status.Sandbox.Count = 3 })
	p.again(stageCluster)
	if end := p.until(30); end != "complete" {
		t.Fatalf("third apply: %s", end)
	}
	_, in, _ = p.state.snapshot()
	if in.Status.Sandbox.Count != 1 {
		t.Fatalf("the record keeps a count no run asks for any more: %+v", in.Status.Sandbox)
	}
	in.Spec.Sandbox.Count = 3
	if got := in.sandboxGrowth(); !slices.Equal(got, []string{"pve1"}) {
		t.Errorf("growth after a smaller group: %v", got)
	}
}

// What a run writes on Proxmox for the access of the sandbox machines, counted. A run looks at its stage many times
// while Cluster API builds machines (22 times on the lab), and each look used to create the pool again, which Proxmox
// answers with a 500, and write the row again; a first mint deleted a token that did not exist, a 500 too
// (v0120-reports/drill-D2.md Z2-c, drill-D4.md Z4-d). Now the first run writes each thing once, with no failed call,
// an apply that changes nothing writes nothing, and a row deleted by hand is written again.
//
// The fake answers as Proxmox does for a pool that exists and a token that does not; what it lists is what was written
// through it, where a real token sees only the rows it may modify.
func TestSandboxAccessIsWrittenOnce(t *testing.T) {
	for placement, sb := range map[string]*SandboxSpec{SandboxShared: sharedSandbox(), SandboxSeparate: separateSandbox()} {
		t.Run(placement, func(t *testing.T) {
			if placement == SandboxSeparate {
				sb.System.Nodes = []string{"pve1"}
			}
			p := sandboxPlatform(t, RunApply, sb)
			if placement == SandboxSeparate { // as TestApplyBuildsTheSeparateSandbox: a routed network, the cluster's machines Running
				p.mutate(func(_ *Run, in *Installation, _ *Secrets) { routed(in) })
				for _, name := range []string{"pc-sandbox-sandbox-pve1-x", "pc-sandbox-sandbox-system-pve1-x"} {
					m := meta(name, map[string]any{"cluster.x-k8s.io/cluster-name": SandboxClusterName})
					m["status"] = map[string]any{"phase": "Running"}
					p.wl.put("/apis/cluster.x-k8s.io/v1beta2/namespaces/pc-sandbox/machines/"+name, m)
				}
			}
			p.upTo(stageSeedPlatform)
			// A machine of the platform cluster is still being built: the stage waits, look after look.
			slow := capiNS + "machines/pc-platform-system-pve1-x"
			p.seed.obj(slow)["status"] = map[string]any{"phase": "Provisioning"}
			for range 8 {
				if end := p.once(); !strings.HasPrefix(end, "defer:") {
					t.Fatalf("with a machine not Running: %s", end)
				}
			}
			if s := p.stage(stageCluster); s.State != StageWaiting {
				t.Fatalf("the stage cluster does not wait: %+v", s)
			}
			p.seed.obj(slow)["status"] = map[string]any{"phase": "Running"}
			if end := p.until(30); end != "complete" {
				t.Fatalf("first run: %s: %+v\n%+v", end, p.stage(stageCluster), p.stage(stageWorkloadPlatform))
			}
			count := func() [4]int {
				return [4]int{p.pve.writeCount("POST /pools"), p.pve.writeCount("PUT /access/acl"), p.pve.writeCount("POST /access/users/"), p.pve.writeCount("DELETE /access/users/")}
			}
			want := [4]int{1, 1, 0, 0} // the pool and the one row
			if placement == SandboxSeparate {
				want = [4]int{1, 7, 1, 0} // the pool, the token and its rows (the six of a mint and the platform's vnet)
			}
			if got := count(); got != want || len(p.pve.failed) != 0 {
				t.Fatalf("first run: pool creates, ACL writes, token mints, token deletes %v, want %v; answered with a 500: %v", got, want, p.pve.failed)
			}

			// An apply that changes nothing writes nothing on Proxmox at all.
			writes := p.pve.writeCount("")
			for _, id := range []string{stageCluster, stageWorkloadPlatform, stageRegister} {
				p.again(id)
			}
			if end := p.until(30); end != "complete" {
				t.Fatalf("second run: %s", end)
			}
			if got := p.pve.writeCount(""); got != writes || len(p.pve.failed) != 0 {
				t.Fatalf("an apply with no change wrote %d times on Proxmox: %v; answered with a 500: %v", got-writes, p.pve.writes[writes:], p.pve.failed)
			}
			if placement != SandboxShared {
				return
			}
			// The row deleted by hand in Proxmox comes back with the next apply, in one write.
			p.pve.acl = nil
			p.again(stageCluster)
			if end := p.until(30); end != "complete" {
				t.Fatalf("third run: %s", end)
			}
			if !slices.Equal(p.pve.acl, []string{"/pool/proxcloud-sbx ProxCloudCAPI capmox@pve!capi delete=0"}) {
				t.Fatalf("the row deleted by hand: ACL writes %v", p.pve.acl)
			}
		})
	}
}

// A platform that a release before the offer installed, whose record holds the block all the same (the save refuses it
// until the upgrade, so only a record written past the save can): an apply runs with the installed BOM, which has no
// sandbox chart, builds nothing and asks nothing of Proxmox.
func TestSandboxRunRefusedOnABOMWithoutTheChart(t *testing.T) {
	old, ok := Lookup(previousSHA256["0.11.1"])
	if !ok || old.Feature(FeatureSandbox) != nil {
		t.Fatal("this build embeds the 0.11.1 BOM, which names no sandbox chart")
	}
	from := refOf(old)
	p := newPlatform(t, RunApply)
	p.mutate(func(run *Run, in *Installation, _ *Secrets) {
		run.BOM, in.Status.BOM = from, &from
		in.Spec.Sandbox = sharedSandbox()
	})
	p.upTo(stageCluster)
	if end := p.until(5); end != "fail:"+CodeSandboxRefused || len(p.pve.pools)+len(p.pve.acl)+len(p.pve.tokens) != 0 || p.seed.obj(appsPath+"pc-platform") != nil {
		t.Fatalf("end %s: %+v; pools %v, acl %v", end, p.stage(stageCluster), p.pve.pools, p.pve.acl)
	}
	if s := p.stage(stageCluster); !strings.Contains(s.Message, "release 0.11.1 has no sandbox chart") {
		t.Errorf("stage %+v", s)
	}
}

// The upgrade the offer arrives with, on the real pair: a platform that release 0.11.1 installed moves to this build's
// BOM with no sandbox block. The plan has the cluster chart's row and the sandbox chart as added, and no machines row;
// the run passes the rollout check, moves the cluster's Application to the new chart and writes nothing of sandboxes:
// no pool, no ACL row, no token, no Application, no record, no third provider. The record lists every component but
// the sandbox chart. Only then is the block taken at the save, and its plan names the machines an apply would build.
func TestUpgradeFromTheLastReleaseBuildsNoSandbox(t *testing.T) {
	old, ok := Lookup(previousSHA256["0.11.1"])
	if !ok {
		t.Fatal("this build does not embed the 0.11.1 BOM")
	}
	cur := sandboxBOM(t)
	cur.Release = "0.12.0" // main's "-dev" BOM is no target of an upgrade; the pins are the ones under test
	from, to := refOf(old), refOf(cur)
	p := newPlatform(t, RunApply)
	p.mutate(func(run *Run, in *Installation, _ *Secrets) { run.BOM, in.Status.BOM = from, &from })
	p.upTo(stageSeedPlatform)
	if end := p.until(30); end != "complete" {
		t.Fatalf("the installing run: %s", end)
	}
	_, in, _ := p.state.snapshot()
	in.Spec.Sandbox = sharedSandbox()
	if probs := in.SandboxProblems(); len(probs) != 1 || !strings.Contains(probs[0], "Upgrade the platform to release "+cur.Release+" first") {
		t.Fatalf("the block before the upgrade: %v", probs)
	}
	in.Spec.Sandbox = nil
	plan, err := PlanFor(in, to.SHA256)
	if err != nil || plan.Upgrade == nil || len(plan.Upgrade.Blocking) != 0 || hasMachines(plan.Upgrade.Changes) {
		t.Fatalf("the plan of the move: %+v %v", plan.Upgrade, err)
	}
	classes := map[string]string{}
	for _, c := range plan.Upgrade.Changes {
		classes[c.Cluster+"/"+c.Name] = c.Class
	}
	if classes["seed/"+ClusterName] != ClassChart || classes["workload/sandbox"] != ClassAdded || len(classes) != 3 {
		t.Fatalf("the rows of the move: %v", classes)
	}

	p.start(t, to.SHA256, "upgrade lab to "+cur.Release)
	if end := p.until(30); end != "complete" {
		t.Fatalf("the upgrade: %s: %+v\n%+v", end, p.stage(stageCluster), p.stage(stageWorkloadPlatform))
	}
	_, in, _ = p.state.snapshot()
	if *in.Status.BOM != to || !p.receipt(stageCluster).RolloutChecked || str(p.seed.obj(appsPath+ClusterName), "spec", "source", "targetRevision") != ClusterChartVersion(cur.BOM) {
		t.Fatalf("status %+v, cluster stage %+v", in.Status.BOM, p.stage(stageCluster))
	}
	if len(p.pve.pools)+len(p.pve.acl)+len(p.pve.tokens) != 0 || p.seed.obj(sandboxApp) != nil || in.Status.Sandbox != nil || len(p.sites) != 0 ||
		in.Status.Providers == nil || in.Status.Providers.Sandbox != "" || len(in.Status.Components) != len(cur.Components)-1 {
		t.Fatalf("pools %v, acl %v, sandbox %+v, providers %+v, %d components", p.pve.pools, p.pve.acl, in.Status.Sandbox, in.Status.Providers, len(in.Status.Components))
	}
	for _, c := range in.Status.Components {
		if c.Name == "sandbox" {
			t.Errorf("the record names the sandbox chart: %+v", c)
		}
	}
	// Asking: the save takes the block now, and the plan of the apply names the group's machines.
	in.Spec.Sandbox = sharedSandbox()
	if probs := in.SandboxProblems(); probs != nil {
		t.Fatalf("the block after the upgrade: %v", probs)
	}
	plan, err = PlanFor(in, "")
	if err != nil || len(plan.Blocking) != 0 {
		t.Fatalf("the plan with the block: %+v %v", plan, err)
	}
	rows := map[string]PlanChange{}
	for _, c := range plan.Changes {
		rows[c.Name] = c
	}
	if c := rows["sandbox"]; !strings.HasPrefix(c.To, SandboxShared+" (sandbox: 1 x 2 cores") || c.Class != ClassMachines || rows["sandbox-pool"].To == "" || rows["sandbox-access"].To == "" || len(rows) != 3 {
		t.Errorf("the rows of the apply: %+v", plan.Changes)
	}
}

// in2bom is the BOM the run tests apply (this build's, under sandboxBOM).
func in2bom(t *testing.T) *BOM {
	t.Helper()
	cur, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	return cur.BOM
}

// Without the block nothing of a sandbox is asked of Proxmox or of the clusters, also under a BOM with the chart.
func TestApplyWithoutSandboxWritesNothingOfIt(t *testing.T) {
	p := sandboxPlatform(t, RunApply, nil)
	p.upTo(stageSeedPlatform)
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s", end)
	}
	_, in, _ := p.state.snapshot()
	if len(p.pve.pools)+len(p.pve.acl)+len(p.pve.tokens) != 0 || p.seed.obj(sandboxApp) != nil || in.Status.Sandbox != nil ||
		p.wl.obj(seedNS+"sandbox-system/secrets/sandbox-proxy-tls") != nil || len(in.Status.Components) != len(in2bom(t).Components)-1 {
		t.Fatalf("pools %v, acl %v, status %+v, %d components", p.pve.pools, p.pve.acl, in.Status.Sandbox, len(in.Status.Components))
	}
	// The provider side as well: no sandbox settings on the Proxmox provider, the two tenant providers and no third,
	// and no record of a sandbox provider or of an attach choice applied to one.
	if len(p.sites) != 0 || len(p.registers) != 2 || in.Status.Providers == nil || in.Status.Providers.Sandbox != "" || in.Status.SandboxAttach != "" {
		t.Fatalf("%d writes of sandbox settings, %d providers registered, providers %+v, sandbox attach %q", len(p.sites), len(p.registers), in.Status.Providers, in.Status.SandboxAttach)
	}
}

// Proxmox refuses the row (a site whose portal token lost its role on the pool): the run stops before the cluster's
// Application is touched and says which script to run again.
func TestSandboxAccessRefused(t *testing.T) {
	p := sandboxPlatform(t, RunApply, sharedSandbox())
	p.upTo(stageCluster)
	p.pve.refuseACL = true
	if end := p.until(5); end != "fail:"+CodeSandboxAccessFailed {
		t.Fatalf("end %s: %+v", end, p.stage(stageCluster))
	}
	if s := p.stage(stageCluster); !strings.Contains(s.Message, "Run the managed-services script again as root") || p.seed.obj(appsPath+"pc-platform") != nil {
		t.Fatalf("stage %+v", s)
	}
}

// Placement off on a platform with a built shared group: the sandbox chart's Application goes with a cascade, the
// group's MachineDeployment is deleted, and once the pool is empty the row, the pool and the record are gone.
func TestSandboxOff(t *testing.T) {
	p := sandboxPlatform(t, RunApply, &SandboxSpec{Placement: SandboxOff})
	now := time.Now().UTC()
	p.mutate(func(_ *Run, in *Installation, sec *Secrets) {
		in.Status.Sandbox = &SandboxStatus{Placement: SandboxShared, NodeRange: "10.200.200.64/28", ProxyAddress: "10.200.200.210", Count: 1, BuiltAt: &now}
		sec.SandboxTLSCert, sec.SandboxTLSKey = "cert", "key-0123456789"
	})
	p.pve.pools = []string{pve.SandboxPool}
	p.seed.put(sandboxApp, meta("pc-platform-sandbox", nil))
	p.seed.put(capiNS+"machinedeployments/pc-platform-sandbox-pve1", meta("pc-platform-sandbox-pve1", map[string]any{"cluster.x-k8s.io/cluster-name": ClusterName, "proxcloud.io/node-group": SandboxGroup}))
	p.wl.put("/api/v1/namespaces/sandbox-system", meta("sandbox-system", nil))
	p.upTo(stageCluster)
	// A sandbox VM is still in the pool: the row and the pool stay until it is gone.
	p.pve.templates = []pve.ClusterResource{{Type: "qemu", VMID: 140, Node: "pve1", Pool: pve.SandboxPool}}
	if end := p.until(6); !strings.HasPrefix(end, "defer:") {
		t.Fatalf("with a VM in the pool: %s %+v", end, p.stage(stageCluster))
	}
	if s := p.stage(stageCluster); s.Code != CodeMachineDeleteStuck || !strings.Contains(s.Message, "VM 140") || len(p.pve.acl) != 0 || len(p.pve.pools) != 1 {
		t.Fatalf("stage %+v, acl %v", s, p.pve.acl)
	}
	if i, j := slices.Index(p.seed.writes, "PATCH "+sandboxApp), slices.Index(p.seed.writes, "DELETE "+sandboxApp); i < 0 || j < i || p.seed.obj(sandboxApp) != nil {
		t.Fatalf("the Application is given the cascade's finalizer, then deleted: %v", p.seed.writes)
	}
	if p.seed.obj(capiNS+"machinedeployments/pc-platform-sandbox-pve1") != nil || p.wl.obj("/api/v1/namespaces/sandbox-system") != nil {
		t.Fatal("the group's MachineDeployment or the chart's namespace is left")
	}
	p.pve.templates = nil
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(stageCluster))
	}
	_, in, sec := p.state.snapshot()
	if in.Status.Sandbox != nil || sec.SandboxTLSKey != "" || len(p.pve.pools) != 0 || !slices.Equal(p.pve.acl, []string{"/pool/proxcloud-sbx ProxCloudCAPI capmox@pve!capi delete=1"}) {
		t.Fatalf("status %+v, pools %v, acl %v", in.Status.Sandbox, p.pve.pools, p.pve.acl)
	}
	app := p.seed.obj(appsPath + "pc-platform")
	if groups, _ := dig(app, "spec", "source", "helm", "valuesObject", "nodeGroups").([]any); len(groups) != 3 {
		t.Errorf("the cluster keeps %d groups", len(groups))
	}
}

// A run does not turn a built placement into the other one.
func TestSandboxSwitchRefused(t *testing.T) {
	p := sandboxPlatform(t, RunApply, separateSandbox())
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) {
		in.Status.Sandbox = &SandboxStatus{Placement: SandboxShared}
	})
	p.upTo(stageCluster)
	if end := p.until(5); end != "fail:"+CodeSandboxRefused || len(p.pve.writes) != 0 {
		t.Fatalf("end %s, Proxmox writes %v", end, p.pve.writes)
	}
}

// An apply with placement separate, on a routed platform network (one node): the portal mints the cluster's token
// with a scope that names the platform's vnet, the token reaches the platform cluster as one Secret that does not
// look like a tenant cluster's, and the cluster's Application carries the cascade's finalizer and the tenant chart.
func TestApplyBuildsTheSeparateSandbox(t *testing.T) {
	sb := separateSandbox()
	sb.System.Nodes = []string{"pve1"}
	p := sandboxPlatform(t, RunApply, sb)
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) { routed(in) })
	for _, name := range []string{"pc-sandbox-sandbox-pve1-x", "pc-sandbox-sandbox-system-pve1-x"} {
		m := meta(name, map[string]any{"cluster.x-k8s.io/cluster-name": SandboxClusterName})
		m["status"] = map[string]any{"phase": "Running"}
		p.wl.put("/apis/cluster.x-k8s.io/v1beta2/namespaces/pc-sandbox/machines/"+name, m)
	}
	p.upTo(stageSeedPlatform)
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v\n%+v\n%s", end, p.stage(stageCluster), p.stage(stageWorkloadPlatform), p.logs)
	}
	_, in, sec := p.state.snapshot()
	if !slices.Equal(p.pve.tokens, []string{"capmox@pve!pc-sandbox"}) || sec.Sandbox == nil || sec.Sandbox.TokenID != "capmox@pve!pc-sandbox" {
		t.Fatalf("tokens %v, stored %+v", p.pve.tokens, sec.Sandbox)
	}
	want := []string{
		"/pool/proxcloud-sbx ProxCloudCAPI capmox@pve!pc-sandbox delete=0",
		"/storage/local-lvm ProxCloudCAPI capmox@pve!pc-sandbox delete=0",
		"/pool/pc-templates PVETemplateUser capmox@pve!pc-sandbox delete=0",
		"/nodes ProxCloudCAPI capmox@pve!pc-sandbox delete=0",
		"/sdn/zones/localnetwork ProxCloudCAPI capmox@pve!pc-sandbox delete=0",
		"/storage/local PVEDatastoreAdmin capmox@pve!pc-sandbox delete=0",
		"/sdn/zones/pcplat/pcplat0 ProxCloudCAPI capmox@pve!pc-sandbox delete=0",
	}
	if !slices.Equal(p.pve.acl, want) {
		t.Fatalf("the token's rows:\n%s\nwant\n%s", strings.Join(p.pve.acl, "\n"), strings.Join(want, "\n"))
	}
	// The platform's own Cluster API gets no row on the pool: the sandbox cluster's token alone builds there.
	if slices.ContainsFunc(p.pve.acl, func(a string) bool { return strings.Contains(a, pve.CAPITokenID) }) {
		t.Errorf("a row for %s: %v", pve.CAPITokenID, p.pve.acl)
	}
	app := p.seed.obj(appsPath + SandboxClusterName)
	fin, _ := dig(app, "metadata", "finalizers").([]any)
	if app == nil || len(fin) != 1 || str(app, "spec", "source", "chart") != "proxmox-kamaji-cluster" || str(app, "spec", "destination", "namespace") != SandboxClusterName ||
		str(app, "spec", "source", "helm", "valuesObject", "proxmox", "network", "bridge") != "pcplat0" {
		t.Fatalf("the cluster's Application: %v", app)
	}
	secret := p.wl.obj(seedNS + "pc-sandbox/secrets/" + SandboxCredentialsSecret)
	if secret == nil || str(secret, "metadata", "labels", "app.kubernetes.io/managed-by") != ManagedBy || dig(secret, "metadata", "labels", "proxcloud.io/service") != nil {
		t.Fatalf("the token's Secret: %v", secret)
	}
	ns := p.wl.obj("/api/v1/namespaces/pc-sandbox")
	if ns == nil || !strings.Contains(str(ns, "metadata", "annotations", "scheduler.alpha.kubernetes.io/node-selector"), "kamaji") {
		t.Errorf("the namespace's placement: %v", ns)
	}
	st := in.Status.Sandbox
	if st == nil || st.Placement != SandboxSeparate || st.Count != 1 || st.SystemCount != 1 || st.ChartVersion != in2bom(t).TenantCharts.Kubernetes.Version || st.BuiltAt == nil {
		t.Fatalf("status %+v", st)
	}
	// No Application of the sandbox chart on the platform cluster, and no sandbox group in it.
	if p.seed.obj(sandboxApp) != nil || len(in.Status.Components) != len(in2bom(t).Components)-1 {
		t.Errorf("separate renders the sandbox chart on the platform cluster")
	}
	p.leaks(t, sec.Sandbox.Secret)
	// A second apply mints nothing again.
	for _, id := range []string{stageCluster, stageWorkloadPlatform} {
		p.again(id)
	}
	if end := p.until(30); end != "complete" || len(p.pve.acl) != len(want) || len(p.pve.tokens) != 1 {
		t.Fatalf("second apply: %s, %d rows, tokens %v", end, len(p.pve.acl), p.pve.tokens)
	}
}

// Remove on a platform with sandboxes. Separate: the sandbox cluster's Application is deleted before anything of the
// platform cluster is touched (its machines need the platform cluster's Cluster API and Kamaji). Both: once the
// platform cluster is gone, the token or the ACL row, the pool and the record go, with the portal token.
func TestRemoveWithSandboxes(t *testing.T) {
	for placement, row := range map[string]string{SandboxShared: "/pool/proxcloud-sbx ProxCloudCAPI capmox@pve!capi delete=1", SandboxSeparate: ""} {
		t.Run(placement, func(t *testing.T) {
			sandboxBOM(t)
			p := removal(t)
			p.mutate(func(_ *Run, in *Installation, sec *Secrets) {
				in.Status.Sandbox = &SandboxStatus{Placement: placement, Count: 1}
				if placement == SandboxSeparate {
					sec.Sandbox = &TokenCredential{TokenID: "capmox@pve!pc-sandbox", Secret: "minted-0123456789"}
				}
			})
			p.pve.pools = []string{pve.SandboxPool}
			if placement == SandboxSeparate {
				p.pve.tokens = []string{"capmox@pve!pc-sandbox"}
				app := meta(SandboxClusterName, nil)
				app["metadata"].(map[string]any)["finalizers"] = []any{resourcesFinalizer}
				p.seed.put(appsPath+SandboxClusterName, app)
				if end := p.once(); end != "defer:waiting_remove_cluster" {
					t.Fatalf("end %s: %+v", end, p.stage(StageRemoveCluster))
				}
				if s := p.stage(StageRemoveCluster); !strings.Contains(s.Message, "The sandbox cluster pc-sandbox is deleted first") || p.seed.obj(appsPath+"pc-platform") == nil ||
					slices.Contains(p.seed.writes, "DELETE "+capiNS+"clusters/pc-platform") || !slices.Contains(p.seed.writes, "DELETE "+appsPath+SandboxClusterName) {
					t.Fatalf("the platform cluster was touched before the sandbox cluster is gone: %+v %v", s, p.seed.writes)
				}
			}
			if end := p.once(); end != "defer:waiting_remove_cluster" {
				t.Fatalf("end %s: %+v", end, p.stage(StageRemoveCluster))
			}
			// Nothing of the sandbox access goes while the platform cluster's machines are left.
			if len(p.pve.pools) != 1 || len(p.pve.acl) != 0 {
				t.Fatalf("pools %v, acl %v", p.pve.pools, p.pve.acl)
			}
			p.seed.mu.Lock()
			for k := range p.seed.objs {
				if strings.HasPrefix(k, capiNS+"machines/") {
					delete(p.seed.objs, k)
				}
			}
			p.seed.mu.Unlock()
			if end := p.until(8); end != "complete" {
				t.Fatalf("end %s: %+v\n%s", end, p.stage(StageRemoveCluster), p.logs)
			}
			_, in, sec := p.state.snapshot()
			if in.Status.Sandbox != nil || sec.Sandbox != nil || len(p.pve.pools) != 0 || len(p.pve.tokens) != 0 {
				t.Fatalf("status %+v, pools %v, tokens %v", in.Status.Sandbox, p.pve.pools, p.pve.tokens)
			}
			if row != "" && !slices.Equal(p.pve.acl, []string{row}) {
				t.Errorf("acl %v", p.pve.acl)
			}
			for i, w := range p.pve.writes {
				if (strings.HasPrefix(w, "DELETE /pools") || w == "PUT /access/acl" || strings.Contains(w, "/token/")) && p.pve.writers[i] != "proxcloud@pve!platform" {
					t.Errorf("%s came with %s", w, p.pve.writers[i])
				}
			}
			p.leaks(t, "minted-0123456789")
		})
	}
}

// An upgrade to the first release whose BOM has the sandbox chart, with the settings asking for the shared group in
// the same apply: the added component is let through (class added), and the gate that refuses every new machine
// template on an upgrade lets through the templates of the group no run has built, and only those. Once the group
// stood, a new template of it is a machine replaced, as for every other group.
func TestUpgradeGateLetsTheNewSandboxGroupThrough(t *testing.T) {
	res := func(kind, name string) map[string]any {
		return map[string]any{"kind": kind, "name": name, "status": "OutOfSync", "requiresPruning": false}
	}
	kcp := map[string]any{"kind": "KubeadmControlPlane", "name": "pc-platform-cp", "status": "Synced"}
	group := []any{kcp, res("ProxmoxMachineTemplate", "pc-platform-sandbox-pve1-ffffffff"), res("KubeadmConfigTemplate", "pc-platform-sandbox-pve1-ffffffff")}
	now := time.Now().UTC()
	for name, c := range map[string]struct {
		resources []any
		built     *SandboxStatus
		want      string
	}{
		"the group is new":               {group, nil, ""},
		"recorded and not built yet":     {group, &SandboxStatus{Placement: SandboxShared, Count: 1}, ""},
		"the group stood already":        {group, &SandboxStatus{Placement: SandboxShared, Count: 1, BuiltAt: &now}, "ProxmoxMachineTemplate pc-platform-sandbox-pve1-ffffffff is new or would change"},
		"another group's template too":   {append(slices.Clone(group), res("KubeadmConfigTemplate", "pc-platform-system-pve1-ffffffff")), nil, "KubeadmConfigTemplate pc-platform-system-pve1-ffffffff is new or would change"},
		"a sandbox node not in settings": {append(slices.Clone(group), res("ProxmoxMachineTemplate", "pc-platform-sandbox-pve2-ffffffff")), nil, "pc-platform-sandbox-pve2-ffffffff is new or would change"},
		"the control plane would change": {[]any{map[string]any{"kind": "KubeadmControlPlane", "name": "pc-platform-cp", "status": "OutOfSync"}}, nil, "KubeadmControlPlane pc-platform-cp would change"},
	} {
		t.Run(name, func(t *testing.T) {
			old := nextRelease(t, func(b *BOM) { ownCharts(b); addSandbox(b) })
			cur, _ := Current()
			from, to := refOf(old), refOf(cur)
			p := newPlatform(t, RunApply)
			p.mutate(func(run *Run, in *Installation, _ *Secrets) { run.BOM, in.Status.BOM = from, &from })
			p.upTo(stageSeedPlatform)
			if end := p.until(30); end != "complete" {
				t.Fatalf("the installing run: %s", end)
			}
			p.mutate(func(_ *Run, in *Installation, _ *Secrets) {
				in.Spec.Sandbox, in.Status.Sandbox = sharedSandbox(), c.built
			})
			p.sandboxMachine()
			p.sandboxIdentities(p.wl)
			p.start(t, to.SHA256, "upgrade lab to 0.9.0")
			p.seed.resources[ClusterName] = c.resources
			end := p.until(30)
			_, in, _ := p.state.snapshot()
			if c.want != "" {
				if end != "fail:"+CodeUnexpectedRollout || !strings.Contains(p.stage(stageCluster).Message, c.want) {
					t.Fatalf("end %s: %+v", end, p.stage(stageCluster))
				}
				return
			}
			if end != "complete" || *in.Status.BOM != to || in.Status.Sandbox == nil || in.Status.Sandbox.BuiltAt == nil || p.seed.obj(sandboxApp) == nil {
				t.Fatalf("end %s: %+v\n%+v\nstatus %+v", end, p.stage(stageCluster), p.stage(stageWorkloadPlatform), in.Status.Sandbox)
			}
		})
	}
}

package platformlife

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
)

const (
	rollPhrase = "upgrade lab to 0.9.0 and replace its machines"
	kcpPath    = "/apis/controlplane.cluster.x-k8s.io/v1beta2/namespaces/pc-platform/kubeadmcontrolplanes/pc-platform-cp"
	newTag     = "ubuntu-2604-k8s-1.35.11-20270101000000-000000000000"
)

// newerK8s makes the target BOM a release that names the next Kubernetes patch with its node image, on the cluster
// chart that can order a rollout.
func newerK8s(b *BOM) {
	b.Kubernetes.Version = "1.35.11"
	b.Kubernetes.NodeImage.Tag, b.Kubernetes.NodeImage.Digest = newTag, "sha256:"+strings.Repeat("e", 64)
	for i := range b.Components {
		if c := &b.Components[i]; c.Chart.Name == "proxmox-kamaji-cluster" {
			c.Chart.Version, c.Chart.Digest, c.UpgradeFrom = "0.1.15", "sha256:"+strings.Repeat("d", 64), []string{"0.1.13"}
		}
	}
}

// The step rule of the Kubernetes version and what else decides class machines: the node image alone (a rebuilt
// image), kube-vip only together with a node image, and the cluster chart that can order a rollout.
func TestClassifyMachines(t *testing.T) {
	for _, c := range []struct{ from, to, want string }{
		{"1.35.10", "1.35.11", ""}, {"1.35.10", "1.36.0", ""}, {"1.35.10", "1.35.10", ""},
		{"1.35.10", "1.35.9", "Kubernetes never goes back"}, {"1.35.10", "1.34.12", "Kubernetes never goes back"},
		{"1.35.10", "1.37.0", "one minor at a time: go through 1.36 first"}, {"1.35.10", "2.0.0", "one minor at a time: go through 1.36 first"},
	} {
		if got := k8sStep(c.from, c.to); got != c.want {
			t.Errorf("%s to %s: %q, want %q", c.from, c.to, got, c.want)
		}
	}
	kubeVip := func(b *BOM) { b.Images.KubeVip = "ghcr.io/kube-vip/kube-vip:v0.9.0@sha256:" + strings.Repeat("e", 64) }
	for name, c := range map[string]struct {
		edit     func(*BOM)
		machines []string // the pins of class machines
		refused  string   // a part of the one refusal's reason
	}{
		"a patch":                {newerK8s, []string{"kubernetes.version", "kubernetes.nodeImage"}, ""},
		"a rebuilt image":        {func(b *BOM) { newerK8s(b); b.Kubernetes.Version = "1.35.10" }, []string{"kubernetes.nodeImage"}, ""},
		"kube-vip with an image": {func(b *BOM) { newerK8s(b); kubeVip(b) }, []string{"kubernetes.version", "kubernetes.nodeImage", "images.kubeVip"}, ""},
		"kube-vip alone": {func(b *BOM) { newerK8s(b); b.Kubernetes = embeddedOld(t).Kubernetes; kubeVip(b) }, nil,
			"kube-vip moves only together with a new node image"},
		"back": {func(b *BOM) { newerK8s(b); b.Kubernetes.Version = "1.35.9" }, []string{"kubernetes.nodeImage"}, "Kubernetes never goes back"},
		"an old chart": {func(b *BOM) { ownCharts(b); b.Kubernetes.NodeImage.Digest = "sha256:" + strings.Repeat("e", 64) }, nil,
			"the BOM names cluster chart 0.1.14, which cannot order a rollout"},
	} {
		t.Run(name, func(t *testing.T) {
			old := nextRelease(t, c.edit)
			cur, _ := Current()
			var machines, refused []string
			for _, ch := range Classify(old.BOM, cur.BOM) {
				switch ch.Class {
				case ClassMachines:
					machines = append(machines, ch.Name)
				case ClassRefused:
					refused = append(refused, ch.Blocking)
				}
			}
			if !slices.Equal(machines, c.machines) || (c.refused == "") != (len(refused) == 0) || (c.refused != "" && !strings.Contains(refused[0], c.refused)) {
				t.Fatalf("machines %v, refused %v", machines, refused)
			}
		})
	}
	// An installation that names its own node templates has no BOM image to roll to.
	old := nextRelease(t, newerK8s)
	cur, _ := Current()
	in := rollInstallation(t, &BOMRef{Release: old.Release, SHA256: old.SHA256})
	in.Spec.NodeImage = NodeImage{Mode: ImageTemplates, Templates: map[string]int{"pve1": 9001, "pve2": 9002, "pve3": 9003}}
	if _, up, err := move(in, cur.SHA256); err != nil || up.Allowed || !strings.Contains(strings.Join(up.Blocking, " "), "this installation names its own node templates") {
		t.Fatalf("mode templates: %+v %v", up, err)
	}
}

func embeddedOld(t *testing.T) *BOM {
	t.Helper()
	old, ok := Lookup(previousSHA256["0.8.1"])
	if !ok {
		t.Fatal("this build does not embed the 0.8.1 BOM")
	}
	return old.BOM
}

// rollInstallation is the lab installation as a run of ref built it.
func rollInstallation(t *testing.T, ref *BOMRef) *Installation {
	in := validInstallation(t)
	in.ID = "pi-0a1b2c3d"
	in.Status = Status{BOM: ref, Seed: &SeedStatus{VMID: 120, Node: "pve1", K3sVersion: "v1.35.10+k3s1"},
		Clusters: []ClusterStatus{{Name: ClusterName, KubernetesVersion: "1.35.10", Templates: map[string]int{"pve1": 9101, "pve2": 9102, "pve3": 9103}}}}
	return in
}

// The plan of a move that replaces machines, as the operator reads it before either button: the two parts with their
// phrases, the line that there is no way back, one row per unit with what stops, and what is made and deleted on the
// site. A homelab (one node, one control plane machine) and three nodes with one unit that has no room.
// `go test ./internal/platformlife -run TestRollPlanGolden -update` rewrites the files.
func TestRollPlanGolden(t *testing.T) {
	for name, c := range map[string]struct {
		shape func(*Installation)
		read  RollRead
	}{
		"roll-homelab": {func(in *Installation) {
			c := &in.Spec.Clusters[0]
			for i := range c.NodeGroups {
				c.NodeGroups[i].Nodes = []string{"pve1"}
			}
		}, RollRead{Read: true, Templates: map[string]int{"pve1": 9201}, Databases: 2}},
		"roll-three-nodes": {func(in *Installation) { in.Spec.Clusters[0].ControlPlane.Replicas = 3 },
			RollRead{Read: true, DeleteFirst: []string{"dbaas/pve3"}, Templates: map[string]int{"pve1": 9201, "pve2": 9202, "pve3": 9203}, Databases: 0}},
	} {
		t.Run(name, func(t *testing.T) {
			old := nextRelease(t, newerK8s)
			ref := refOf(old)
			in := rollInstallation(t, &ref)
			c.shape(in)
			p, err := PlanFor(in, "")
			if err != nil || p.Upgrade == nil || len(p.Upgrade.Parts) != 2 {
				t.Fatalf("plan %+v: %v", p.Upgrade, err)
			}
			cur, _ := Current()
			p.Upgrade.To.SHA256 = cur.SHA256
			p.NeedRoll(in, func(BOMRef) RollRead { return c.read })
			u := p.Upgrade
			if !u.Allowed || u.Confirm != rollPhrase || u.Parts[0].Confirm != "upgrade lab to 0.9.0" || u.Parts[0].Done || u.Changes[len(u.Changes)-1].Class != ClassSite {
				t.Fatalf("parts %+v, confirm %q", u.Parts, u.Confirm)
			}
			if u.Changes[0].Name != "roll" || u.Changes[0].Impact != noWayBack {
				t.Fatalf("the plan does not begin with the line that there is no way back: %+v", u.Changes[0])
			}
			u.To.SHA256 = "" // of this build's BOM file, which is not the pair's
			raw, err := json.MarshalIndent(u, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden(t, filepath.Join("upgrade", "0.8.1-to-0.9.0-"+name, "plan.golden.json"), append(raw, '\n'))
		})
	}
}

// The two parts and their phrases through an upgrade: either part starts it; the platform part waits for the seed's
// k3s and for the site's read; the run that begins the roll takes the longer phrase even when the upgrade has started;
// once the roll has begun nothing is asked and the seed part is over.
func TestRollPartsAndPhrases(t *testing.T) {
	old := nextRelease(t, func(b *BOM) { newerK8s(b); newerK3s("v1.35.11+k3s1")(b) })
	cur, _ := Current()
	ref := refOf(old)
	in := rollInstallation(t, &ref)
	ok := RollRead{Read: true}
	start := func(target, confirm, part string, read RollRead) error {
		return startApply(&Run{}, in, target, confirm, part, SeedBackup{Taken: true}, read)
	}
	var refused *UpgradeRefused
	if err := start(cur.SHA256, rollPhrase, "", ok); !errors.As(err, &refused) || !strings.HasPrefix(refused.Reasons[0], CodeSeedNotUpgraded+": ") {
		t.Fatalf("the platform part before the seed's k3s moved: %v", err)
	}
	if err := start(cur.SHA256, rollPhrase, PartSeed, ok); !errors.Is(err, ErrBadConfirm) {
		t.Fatalf("the seed part with the platform part's phrase: %v", err)
	}
	if err := start(cur.SHA256, "upgrade lab to 0.9.0", "platform", ok); !errors.Is(err, ErrBadPart) && err == nil {
		t.Fatalf("an unknown part: %v", err)
	}
	seed := &Run{}
	if err := startApply(seed, in, cur.SHA256, "upgrade lab to 0.9.0", PartSeed, SeedBackup{Taken: true}, RollRead{}); err != nil || seed.Part != PartSeed || in.Status.TargetBOM == nil {
		t.Fatalf("the seed part: %+v %v", seed, err)
	}
	in.Status.Seed.K3sVersion = "v1.35.11+k3s1" // stage seed-k3s
	// The upgrade is in progress and the roll has not begun: the phrase, and the site's read.
	var noRoll *RollRefused
	if err := start("", "", "", ok); !errors.Is(err, ErrBadConfirm) {
		t.Fatalf("the platform part without its phrase: %v", err)
	}
	if err := start("", rollPhrase, "", RollRead{}); !errors.As(err, &noRoll) || noRoll.Code != CodePlatformUnhealthy {
		t.Fatalf("the platform part without a read of the site: %v", err)
	}
	missing := CodeNodeImageMissing + `: node pve2 has no template of node image ` + newTag + `; run "Seed and node image" first`
	if err := start("", rollPhrase, "", RollRead{Read: true, Blocking: []string{missing}}); !errors.As(err, &noRoll) || noRoll.Code != CodeNodeImageMissing || err.Error() != missing {
		t.Fatalf("the platform part without the image: %v", err)
	}
	run := &Run{}
	if err := startApply(run, in, "", rollPhrase, "", SeedBackup{}, RollRead{Read: true, DeleteFirst: []string{"dbaas/pve3"}}); err != nil || !slices.Equal(run.DeleteFirst, []string{"dbaas/pve3"}) {
		t.Fatalf("the platform part: %+v %v", run, err)
	}
	// Stage 7 began the roll: Retry asks nothing, reads nothing, and the seed part is over.
	in.Status.Clusters[0].Roll = &RollStatus{Step: 2, DeleteFirst: []string{"dbaas/pve3"}}
	if err := start("", "", "", RollRead{}); err != nil {
		t.Fatalf("Retry of a begun roll: %v", err)
	}
	if err := start("", "", PartSeed, RollRead{}); !errors.As(err, &refused) {
		t.Fatalf("the seed part after the roll began: %v", err)
	}
	p, _ := PlanFor(in, "")
	p.NeedRoll(in, func(BOMRef) RollRead { t.Fatal("a begun roll reads the site"); return RollRead{} })
	if u := p.Upgrade; !u.Allowed || u.Confirm != "" || !u.Parts[0].Done || u.Parts[0].Allowed ||
		!slices.ContainsFunc(u.Changes, func(c PlanChange) bool {
			return c.Name == "roll/dbaas@pve3" && strings.Contains(c.Impact, "deleted BEFORE")
		}) {
		t.Fatalf("the plan of a begun roll: %+v", u)
	}
	// A move that replaces no machine has no parts.
	nextRelease(t, ownCharts)
	in = rollInstallation(t, &ref)
	if err := start(cur.SHA256, "upgrade lab to 0.9.0", PartSeed, ok); !errors.Is(err, ErrBadPart) {
		t.Fatalf("part seed on a move without machines: %v", err)
	}
}

// Each step of a roll through the chart: only the units up to the step carry the target's version and template, no
// template of a unit not reached changes its name, a delete-first unit gets the strategy, and without a roll the
// values are the ones every other run renders.
func TestRollRendersOneUnitAtATime(t *testing.T) {
	b := *embedded(t)
	b.Kubernetes.Version = "1.35.11"
	in := renderInput()
	in.Templates = map[string]int{"pve1": 9201, "pve2": 9202, "pve3": 9203}
	plain, err := ClusterValues(&b, in)
	if err != nil {
		t.Fatal(err)
	}
	old := map[string]int{"pve1": 9101, "pve2": 9102, "pve3": 9103}
	order := []string{"system/pve1", "system/pve2", "kamaji/pve2", "dbaas/pve1", "dbaas/pve2", "dbaas/pve3"}
	render := func(step int) (map[string]any, map[string]string) {
		in.Cluster.Roll = &Roll{Step: step, FromVersion: "1.35.10", FromKubeVip: "ghcr.io/kube-vip/kube-vip:v0.8.0", FromTemplates: old, DeleteFirst: []string{"dbaas/pve3"}}
		v, err := ClusterValues(&b, in)
		if err != nil {
			t.Fatal(err)
		}
		return v, chartFacts(t, v)
	}
	if _, facts := render(0); facts["cp"] != "v1.35.10 9101 ghcr.io/kube-vip/kube-vip:v0.8.0" || facts["system/pve1"] != "v1.35.10 9101" {
		t.Fatalf("step 0 renders what runs: %v", facts)
	}
	var before map[string]string
	for step := 1; step <= len(order)+2; step++ {
		v, facts := render(step)
		if !strings.HasPrefix(facts["cp"], "v1.35.11 9201 "+b.Images.KubeVip) {
			t.Fatalf("step %d: control plane %q", step, facts["cp"])
		}
		for i, unit := range order {
			node := unit[strings.Index(unit, "/")+1:]
			want := fmt.Sprintf("v1.35.10 %d", old[node])
			if i+2 <= step {
				want = fmt.Sprintf("v1.35.11 %d", in.Templates[node])
			}
			if facts[unit] != want {
				t.Fatalf("step %d: %s is %q, want %q", step, unit, facts[unit], want)
			}
			if before != nil && i+2 > step && facts["template "+unit] != before["template "+unit] {
				t.Fatalf("step %d: the template of %s, which is not reached, changed its name", step, unit)
			}
		}
		if facts["strategy dbaas/pve3"] != "0/1" || facts["strategy dbaas/pve1"] != "" {
			t.Fatalf("step %d: strategies %v", step, facts)
		}
		before = facts
		if step > len(order) { // every unit reached: the roll's keys are gone but the strategy
			for _, g := range v["nodeGroups"].([]any) {
				delete(g.(map[string]any), "deleteFirstOn")
			}
			if !sameJSON(v, plain) {
				t.Fatalf("step %d differs from a render without a roll", step)
			}
		}
	}
	b.Components = slices.Clone(b.Components)
	comp(t, &b, ClusterSeed, ClusterName).Chart.Version = "0.1.14"
	if _, err := ClusterValues(&b, in); err == nil || !strings.Contains(err.Error(), "cannot order a rollout") {
		t.Fatalf("a roll on chart 0.1.14: %v", err)
	}
}

// chartFacts renders values through the chart in the tree: "cp" and each unit as "<version> <template VMID>" (the
// control plane with its kube-vip image), each unit's machine template name and rollout strategy.
func chartFacts(t *testing.T, values map[string]any) map[string]string {
	t.Helper()
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
	objs := yamlObjects(t, out)
	templates := map[string]string{}
	for _, o := range objs {
		if o["kind"] == "ProxmoxMachineTemplate" {
			templates[str(o, "metadata", "name")] = fmt.Sprint(dig(o, "spec", "template", "spec", "templateID"))
		}
	}
	facts := map[string]string{}
	for _, o := range objs {
		switch o["kind"] {
		case "KubeadmControlPlane":
			vip := ""
			for _, line := range strings.Split(jsonOf(o), `\n`) {
				if i := strings.Index(line, "image: "); i >= 0 && strings.Contains(line, "kube-vip") {
					vip = strings.Trim(line[i+7:], `" `)
				}
			}
			facts["cp"] = str(o, "spec", "version") + " " + templates[str(o, "spec", "machineTemplate", "spec", "infrastructureRef", "name")] + " " + vip
		case "MachineDeployment":
			name := strings.TrimPrefix(str(o, "metadata", "name"), ClusterName+"-")
			unit := name[:strings.LastIndex(name, "-")] + "/" + name[strings.LastIndex(name, "-")+1:]
			ref := str(o, "spec", "template", "spec", "infrastructureRef", "name")
			facts[unit], facts["template "+unit] = str(o, "spec", "template", "spec", "version")+" "+templates[ref], ref
			if ru := dig(o, "spec", "rollout", "strategy", "rollingUpdate"); ru != nil {
				facts["strategy "+unit] = fmt.Sprintf("%v/%v", dig(ru, "maxSurge"), dig(ru, "maxUnavailable"))
			}
		}
	}
	return facts
}

// rolling is a platform that release 0.8.1 installed, with Cluster API's objects as its controllers report them, on a
// build whose BOM names Kubernetes 1.35.11, and the run that begins the roll started: the target image is a template
// on every node (9201 to 9203), every node has room.
func rolling(t *testing.T) (p *platform, to BOMRef) {
	t.Helper()
	old := nextRelease(t, newerK8s)
	cur, _ := Current()
	from := refOf(old)
	p = newPlatform(t, RunApply)
	p.mutate(func(run *Run, in *Installation, _ *Secrets) { run.BOM, in.Status.BOM = from, &from })
	p.upTo(stageSeedPlatform)
	if end := p.until(30); end != "complete" {
		t.Fatalf("the installing run: %s", end)
	}
	p.pve.mu.Lock()
	p.pve.maxMem = 64 << 30
	p.pve.mu.Unlock()
	settled := func(n int) map[string]any {
		return map[string]any{"observedGeneration": 1, "replicas": n, "upToDateReplicas": n, "readyReplicas": n, "availableReplicas": n}
	}
	p.seed.put(kcpPath, map[string]any{"metadata": map[string]any{"name": "pc-platform-cp", "generation": 1}, "spec": map[string]any{"replicas": 1}, "status": settled(1)})
	for _, name := range []string{"pc-platform-system-pve1", "pc-platform-kamaji-pve2", "pc-platform-dbaas-pve3"} {
		md := p.seed.obj(capiNS + "machinedeployments/" + name)
		md["metadata"].(map[string]any)["generation"] = 1
		md["spec"], md["status"] = map[string]any{"replicas": 1}, settled(1)
		p.seed.put(capiNS+"machinedeployments/"+name, md)
	}
	for _, owner := range []string{"pc-platform-cp", "pc-platform-system-pve1", "pc-platform-kamaji-pve2", "pc-platform-dbaas-pve3"} {
		p.machine(owner, "1.35.10")
	}
	p.begin(t, cur.SHA256, rollPhrase)
	return p, refOf(cur)
}

// machine sets the one Machine of owner at version, as Cluster API has it once the unit's replacement is over.
func (p *platform) machine(owner, version string) {
	path := capiNS + "machines/" + owner + "-x"
	m := p.seed.obj(path)
	labels := maps.Clone(m["metadata"].(map[string]any)["labels"].(map[string]any)) // newPlatform gives every machine one map
	m["metadata"].(map[string]any)["labels"] = labels
	if owner == "pc-platform-cp" {
		labels["cluster.x-k8s.io/control-plane"] = ""
	} else {
		labels["proxcloud.io/machine-deployment"] = owner
	}
	m["spec"].(map[string]any)["version"] = "v" + version
	p.seed.put(path, m)
}

// begin starts the next apply and gives its stage 2 the target image's templates.
func (p *platform) begin(t *testing.T, target, confirm string) {
	t.Helper()
	p.start(t, target, confirm)
	p.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		run.Stages[1].Receipt = &StageReceipt{Templates: map[string]int{"seed:pve1": 9100, "node:pve1": 9201, "node:pve2": 9202, "node:pve3": 9203}}
	})
}

// rendered is what the cluster's Application carries: the control plane's version and template, then each group's.
func (p *platform) rendered() string {
	v, _ := dig(p.seed.obj(appsPath+ClusterName), "spec", "source", "helm", "valuesObject").(map[string]any)
	out := []string{"cp " + str(v, "kubernetesVersion") + " " + str(v, "controlPlane", "kubeadm", "machineImageId")}
	groups, _ := v["nodeGroups"].([]any)
	for _, g := range groups {
		m := g.(map[string]any)
		version := str(v, "kubernetesVersion")
		if old, ok := m["kubernetesVersions"].(map[string]any); ok {
			for _, o := range old {
				version = o.(string)
			}
		}
		out = append(out, str(m, "name")+" "+version+" "+str(m, "machineImageId"))
	}
	return strings.Join(out, "; ")
}

func (p *platform) rollStep() int {
	_, in, _ := p.state.snapshot()
	if r := rollOf(in); r != nil {
		return r.Step
	}
	return 0
}

// The roll on fakes: the control plane first, then system, kamaji, dbaas, each step a render that moves one unit and
// nothing else; a step is not left before its machine runs the target, nor before the tenants that were healthy are
// healthy again; the record names the old version until the last unit is done and the new one after; stage 11 clears
// the roll.
func TestRollReplacesTheMachinesInOrder(t *testing.T) {
	p, to := rolling(t)
	steps := []struct{ owner, rendered string }{
		{"pc-platform-cp", "cp 1.35.11 pve1=9201; system 1.35.10 pve1=9101; kamaji 1.35.10 pve2=9102; dbaas 1.35.10 pve3=9103"},
		{"pc-platform-system-pve1", "cp 1.35.11 pve1=9201; system 1.35.11 pve1=9201; kamaji 1.35.10 pve2=9102; dbaas 1.35.10 pve3=9103"},
		{"pc-platform-kamaji-pve2", "cp 1.35.11 pve1=9201; system 1.35.11 pve1=9201; kamaji 1.35.11 pve2=9202; dbaas 1.35.10 pve3=9103"},
		{"pc-platform-dbaas-pve3", "cp 1.35.11 pve1=9201; system 1.35.11 pve1=9201; kamaji 1.35.11 pve2=9202; dbaas 1.35.11 pve3=9203"},
	}
	for i, s := range steps {
		// Two looks: the unit's machine is still the old one.
		for range 2 {
			if end, st := p.once(), p.stage(stageCluster); end != "defer:waiting_cluster" || st.Code != CodeRolloutStalled || p.rollStep() != i+1 || p.rendered() != s.rendered {
				t.Fatalf("step %d: end %s, step %d, %+v\nrendered %s", i+1, end, p.rollStep(), st, p.rendered())
			}
		}
		_, in, _ := p.state.snapshot()
		if c := in.Status.Clusters[0]; c.KubernetesVersion != "1.35.10" || c.Templates["pve1"] != 9101 || *in.Status.BOM == to {
			t.Fatalf("step %d: the record moved before the last unit: %+v", i+1, c)
		}
		if it := item(p.stage(stageCluster), s.owner+"-x"); it == nil || !strings.Contains(it.Message, "not replaced yet") || p.receipt(stageCluster).RollChecked != i+1 {
			t.Fatalf("step %d: item %+v, receipt %+v", i+1, it, p.receipt(stageCluster))
		}
		if i == 0 {
			// The owner has not seen the change yet: machines at the target alone do not end the step.
			kcp := p.seed.obj(kcpPath)
			kcp["metadata"].(map[string]any)["generation"] = 2
			p.seed.put(kcpPath, kcp)
			p.machine(s.owner, "1.35.11")
			if end := p.once(); end != "defer:waiting_cluster" || p.rollStep() != 1 {
				t.Fatalf("a control plane that has not observed its change: %s, step %d", end, p.rollStep())
			}
			kcp["status"].(map[string]any)["observedGeneration"] = 2
			p.seed.put(kcpPath, kcp)
			// A tenant that was healthy is not, after the control plane: the next unit waits for it.
			p.wl.put(appsPath+"t-1", map[string]any{"metadata": map[string]any{"name": "t-1", "labels": map[string]any{"app.kubernetes.io/managed-by": "proxcloud"}},
				"status": map[string]any{"health": map[string]any{"status": "Degraded"}}})
			if end, st := p.once(), p.stage(stageCluster); end != "defer:waiting_cluster" || st.Code != CodeTenantsUnhealthy || p.rollStep() != 1 {
				t.Fatalf("between two units with a tenant down: %s %+v", end, st)
			}
			p.wl.mu.Lock()
			delete(p.wl.objs, appsPath+"t-1")
			p.wl.mu.Unlock()
			continue
		}
		p.machine(s.owner, "1.35.11")
	}
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(stageCluster))
	}
	_, in, _ := p.state.snapshot()
	if c := in.Status.Clusters[0]; c.KubernetesVersion != "1.35.11" || c.Templates["pve3"] != 9203 || c.Roll != nil || *in.Status.BOM != to || in.Status.TargetBOM != nil {
		t.Fatalf("after the roll: %+v, %+v", c, in.Status)
	}
	p.leaks(t)
}

// A look before ArgoCD has compared or synced the step's source does not advance, whatever the machines say.
func TestRollWaitsForTheStepsSync(t *testing.T) {
	p, _ := rolling(t)
	p.seed.unsynced[ClusterName] = true
	p.machine("pc-platform-cp", "1.35.11")
	for range 3 {
		if end := p.once(); end != "defer:waiting_cluster" || p.rollStep() != 1 || !strings.Contains(p.stage(stageCluster).Message, "ArgoCD") {
			t.Fatalf("end %s, step %d: %+v", end, p.rollStep(), p.stage(stageCluster))
		}
	}
}

// A machine that fails at unit 2 ends the run with the old advice; Retry asks nothing, continues at unit 2 and never
// renders unit 1 at the old version. A Cancel between looks ends the run; the next apply goes on where it stood.
func TestRollRetryAndPause(t *testing.T) {
	p, _ := rolling(t)
	p.once()
	p.machine("pc-platform-cp", "1.35.11")
	if end := p.once(); end != "defer:waiting_cluster" || p.rollStep() != 2 {
		t.Fatalf("after the control plane: %s, step %d: %+v", end, p.rollStep(), p.stage(stageCluster))
	}
	path := capiNS + "machines/pc-platform-system-pve1-x"
	m := p.seed.obj(path)
	m["status"] = map[string]any{"phase": "Failed", "failureMessage": "clone failed: storage full"}
	p.seed.put(path, m)
	if end, st := p.once(), p.stage(stageCluster); end != "fail:"+CodeMachineFailed || !strings.Contains(st.Message, "The machine it replaces still runs.") {
		t.Fatalf("a failed machine: %s %+v", end, st)
	}
	m["status"] = map[string]any{"phase": "Running"}
	p.seed.put(path, m)
	p.begin(t, "", "") // Retry: no target, no phrase
	if end := p.once(); end != "defer:waiting_cluster" || p.rollStep() != 2 || !strings.HasPrefix(p.rendered(), "cp 1.35.11 pve1=9201; system 1.35.11 pve1=9201; kamaji 1.35.10") {
		t.Fatalf("Retry: %s, step %d, rendered %s", end, p.rollStep(), p.rendered())
	}
	// Pause: a Cancel is honoured at the next look, with the unit's machine left to Cluster API.
	p.mutate(func(run *Run, _ *Installation, _ *Secrets) { run.CancelRequested = true })
	p.once()
	if run, in, _ := p.state.snapshot(); run.State != RunCancelled || rollOf(in).Step != 2 {
		t.Fatalf("after the Cancel: %s, %+v", run.State, rollOf(in))
	}
	p.machine("pc-platform-system-pve1", "1.35.11")
	p.begin(t, "", "") // Resume
	if end := p.once(); end != "defer:waiting_cluster" || p.rollStep() != 3 {
		t.Fatalf("Resume: %s, step %d", end, p.rollStep())
	}
	p.machine("pc-platform-kamaji-pve2", "1.35.11")
	p.machine("pc-platform-dbaas-pve3", "1.35.11")
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(stageCluster))
	}
}

// The gate inside a roll: at step 1 the control plane and its template may differ, a template of another unit may
// not (a machine size edited together with the upgrade); nothing of the step is synced then.
func TestRollRefusesAChangeOutsideItsStep(t *testing.T) {
	res := func(kind, name string) map[string]any {
		return map[string]any{"kind": kind, "name": name, "status": "OutOfSync"}
	}
	own := []any{res("KubeadmControlPlane", "pc-platform-cp"), res("ProxmoxMachineTemplate", "pc-platform-cp-pve1-ffffffff")}
	p, _ := rolling(t)
	p.seed.resources[ClusterName] = own
	if end := p.once(); end != "defer:waiting_cluster" || p.receipt(stageCluster).RollChecked != 1 {
		t.Fatalf("the step's own changes: %s %+v", end, p.stage(stageCluster))
	}
	p, _ = rolling(t)
	p.seed.resources[ClusterName] = append(slices.Clone(own), res("ProxmoxMachineTemplate", "pc-platform-kamaji-pve2-ffffffff"))
	end, st := p.once(), p.stage(stageCluster)
	if end != "fail:"+CodeUnexpectedRollout || !strings.Contains(st.Message, "pc-platform-kamaji-pve2-ffffffff") || strings.Contains(st.Message, "pc-platform-cp-pve1") {
		t.Fatalf("a template of another unit: %s %+v", end, st)
	}
	if app := p.seed.obj(appsPath + ClusterName); dig(app, "spec", "syncPolicy", "automated") != nil || p.rollStep() != 1 {
		t.Fatalf("the Application after the refusal syncs by itself, or the step moved: %v", app["spec"])
	}
}

// Stage 1 of the run that begins a roll reads the site again: a node without the image's template, and a control plane
// node without room for one more machine, each end the run before anything is applied. A step whose node lost its room
// since the plan refuses with nothing of the step applied. A unit the plan gave delete-first is not counted.
func TestRollPreChecksInTheRun(t *testing.T) {
	validate := func(p *platform) {
		p.mutate(func(run *Run, _ *Installation, _ *Secrets) {
			run.Stages[0] = Stage{ID: StageValidate, State: StagePending}
		})
	}
	p, _ := rolling(t)
	validate(p)
	if end, st := p.once(), p.stage(StageValidate); end != "fail:"+CodeNodeImageMissing || !strings.Contains(st.Message, `node pve1 has no template of node image `+newTag+`; run "Seed and node image" first`) {
		t.Fatalf("without the image: %s %+v", end, st)
	}
	imported := func(p *platform) {
		for i, node := range []string{"pve1", "pve2", "pve3"} {
			p.imports.recs = append(p.imports.recs, templateupdates.Receipt{ID: "tu-n" + node, Step: templateupdates.StepSucceeded, VMID: 9201 + i,
				Payload: templateupdates.VMPayload{ServiceID: "svc-pve", Node: node, Digest: "sha256:" + strings.Repeat("e", 64)}})
			p.pve.templates = append(p.pve.templates, pve.ClusterResource{VMID: 9201 + i, Node: node, Template: 1, Pool: pve.TemplatePool, Type: "qemu"})
		}
	}
	p, _ = rolling(t)
	imported(p)
	validate(p)
	p.pve.maxMem = 2 << 30
	if end, st := p.once(), p.stage(StageValidate); end != "fail:"+CodeCapacityShort || !strings.Contains(st.Message, "one more machine of controlPlane needs 4 GiB") || len(p.appliedRevisions()) != 0 {
		t.Fatalf("a control plane node without room: %s %+v, applied %v", end, st, p.appliedRevisions())
	}
	p, _ = rolling(t)
	imported(p)
	validate(p)
	p.wl.put("/apis/policy/v1/poddisruptionbudgets/db-1", map[string]any{"metadata": map[string]any{"name": "db-1", "namespace": "pc-db-tenant", "labels": map[string]any{"cnpg.io/cluster": "db-1"}},
		"status": map[string]any{"disruptionsAllowed": 0, "expectedPods": 1}})
	if end, st := p.once(), p.stage(StageValidate); end != "fail:"+CodeDrainBlocked || !strings.Contains(st.Message, "PodDisruptionBudget pc-db-tenant/db-1") || !strings.Contains(st.Message, "0.5.2") {
		t.Fatalf("a budget that allows no disruption: %s %+v", end, st)
	}
	// The room is gone at the step.
	p, _ = rolling(t)
	p.pve.maxMem = 2 << 30
	if end, st := p.once(), p.stage(stageCluster); end != "fail:"+CodeCapacityShort || !strings.Contains(st.Message, "Nothing of this step was applied") ||
		slices.ContainsFunc(p.appliedRevisions(), func(a string) bool { return strings.HasPrefix(a, ClusterName+"@") }) {
		t.Fatalf("a step without room: %s %+v, applied %v", end, st, p.appliedRevisions())
	}
	// rollRoom: one spare at a time, per unit: 5 GiB hold one more machine of 4 on each node, never the sum.
	in := rollInstallation(t, nil)
	for i := range in.Spec.Clusters[0].NodeGroups {
		in.Spec.Clusters[0].NodeGroups[i].Nodes = []string{"pve1"}
	}
	p.pve.maxMem = 5 << 30
	if short, first, err := rollRoom(context.Background(), p.r2(t), in); err != nil || short != "" || len(first) != 0 {
		t.Fatalf("one spare at a time: %q %v %v", short, first, err)
	}
	in.Spec.Clusters[0].NodeGroups[2].MemoryMiB = 8192
	if short, first, err := rollRoom(context.Background(), p.r2(t), in); err != nil || short != "" || !slices.Equal(first, []string{"dbaas/pve1"}) {
		t.Fatalf("a group without room deletes first: %q %v %v", short, first, err)
	}
}

// r2 is a Proxmox client of the fake with Cluster API's token.
func (p *platform) r2(t *testing.T) *pve.Client {
	t.Helper()
	es, _ := p.r.Provider(context.Background(), "")
	c, err := TokenClient(es, &TokenCredential{TokenID: pve.CAPITokenID, Secret: runCAPI})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Part seed ends succeeded after the seed's own charts with the target still set and nothing of the platform cluster
// applied; the plan then offers the platform part, which the site's read blocks while a node lacks the image.
func TestRollSeedPart(t *testing.T) {
	old := nextRelease(t, newerK8s)
	cur, _ := Current()
	from := refOf(old)
	p := newPlatform(t, RunApply)
	p.mutate(func(run *Run, in *Installation, _ *Secrets) { run.BOM, in.Status.BOM = from, &from })
	p.upTo(stageSeedPlatform)
	if end := p.until(30); end != "complete" {
		t.Fatalf("the installing run: %s", end)
	}
	p.part = PartSeed
	p.start(t, cur.SHA256, "upgrade lab to 0.9.0")
	if end := p.until(30); end != "complete" {
		t.Fatalf("part seed: %s", end)
	}
	run, in, _ := p.state.snapshot()
	if run.State != RunSucceeded || run.Part != PartSeed || len(run.Stages) != 7 || in.Status.TargetBOM == nil || *in.Status.BOM != from || rollOf(in) != nil ||
		in.Status.Clusters[0].KubernetesVersion != "1.35.10" || slices.ContainsFunc(p.appliedRevisions(), func(a string) bool { return strings.HasPrefix(a, ClusterName+"@") }) {
		t.Fatalf("after part seed: run %+v, status %+v, applied %v", run, in.Status, p.appliedRevisions())
	}
	plan, err := PlanFor(in, "")
	if err != nil {
		t.Fatal(err)
	}
	if u := plan.Upgrade; !u.InProgress || !u.Parts[0].Done || u.Confirm != rollPhrase {
		t.Fatalf("the plan after part seed: %+v", u)
	}
	missing := CodeNodeImageMissing + ": node pve1 has no template"
	plan.NeedRoll(in, func(BOMRef) RollRead { return RollRead{Read: true, Blocking: []string{missing}} })
	if u := plan.Upgrade; u.Allowed || u.Confirm != "" || u.Parts[0].Done || !slices.Contains(u.Parts[1].Blocking, missing) {
		t.Fatalf("the plan while a node lacks the image: %+v", u)
	}
}

// A control plane of three counts three nodes: the wait between two units, and stage 9, would otherwise never end.
func TestNodesCountsEveryControlPlaneMachine(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Spec.Clusters[0].ControlPlane.Replicas = 3 })
	for _, name := range []string{"pc-platform-cp-b", "pc-platform-cp-c"} {
		n := meta(name, map[string]any{})
		n["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
		p.wl.put("/api/v1/nodes/"+name, n)
	}
	run, in, sec := p.state.snapshot()
	x := &acq{run: run, in: in, sec: sec}
	seed, err := x.seed()
	if err != nil {
		t.Fatal(err)
	}
	wl, err := x.workload(context.Background(), seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.nodes(context.Background(), wl); err != nil {
		t.Fatalf("six nodes of a control plane of three and three groups: %v", err)
	}
}

// An archive of the seed's VMID counts only when it is of this seed: made after the seed became ready, and not noted
// with another VM's name. A store may hold year-old archives of an earlier VM that had the same VMID.
func TestSeedBackupIsOfThisSeed(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	ready := now.Add(-30 * 24 * time.Hour)
	var rows []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any = rows
		if strings.HasSuffix(r.URL.Path, "/storage") {
			data = []map[string]any{{"storage": "nas", "content": "backup"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()
	c, err := pve.New(pve.Config{APIURL: srv.URL, TokenID: "proxcloud@pve!platform", Secret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	in := &Installation{ID: "pi-0a1b2c3d", Status: Status{Seed: &SeedStatus{VMID: 106, Node: "pve1", Generation: 1, ReadyAt: &ready}}}
	row := func(at time.Time, notes string) map[string]any {
		return map[string]any{"volid": "nas:backup/vzdump-qemu-106-" + at.Format("2006_01_02") + ".vma.zst", "vmid": 106, "ctime": at.Unix(), "notes": notes}
	}
	for name, c2 := range map[string]struct {
		rows []map[string]any
		want string
	}{
		"an earlier VM of the same VMID": {[]map[string]any{row(now.Add(-365*24*time.Hour), "old-vm"), row(now.Add(-366*24*time.Hour), "")}, CheckWarn},
		"another VM's name, made today":  {[]map[string]any{row(now.Add(-time.Hour), "some-other-vm")}, CheckWarn},
		"this seed, by its name":         {[]map[string]any{row(now.Add(-time.Hour), SeedVMName("pi-0a1b2c3d", 1))}, CheckOK},
		"this seed, without notes":       {[]map[string]any{row(now.Add(-365*24*time.Hour), "old-vm"), row(now.Add(-time.Hour), "")}, CheckOK},
	} {
		rows = c2.rows
		got := SeedBackupCheck(context.Background(), c, in, now)
		if got.Status != c2.want || (name == "an earlier VM of the same VMID" && !strings.Contains(got.Message, "2 archives of VMID 106 were not counted")) {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

// A run on a seed whose VM firewall is as wanted writes no option: an apply whose plan lists nothing for the site
// must not rewrite the VM's firewall file.
func TestSeedFirewallIsWrittenOnlyWhenItDiffers(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.upTo(StageSeedVM)
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s", end)
	}
	if n := p.pve.writeCount("PUT /nodes/pve1/qemu/120/firewall/options"); n != 1 {
		t.Fatalf("the first apply wrote the options %d times", n)
	}
	p.again(StageSeedVM)
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s", end)
	}
	if n := p.pve.writeCount("PUT /nodes/pve1/qemu/120/firewall/options"); n != 1 {
		t.Fatalf("an apply of a seed whose firewall is as wanted wrote the options again (%d writes)", n)
	}
}

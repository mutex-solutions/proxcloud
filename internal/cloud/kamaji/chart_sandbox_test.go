package kamaji

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The sandbox cluster's system node group (installer stage 55): its trusted label goes on the Machines, where CAPI
// copies it to the Node, never into the kubelet's own --node-labels; and MetalLB's speaker tolerates its taint, so
// sandbox-proxy's address is announced from there. MetalLB's controller gets the selector and the tolerations named
// for it (chart 0.1.20), under the keys MetalLB's chart reads. A tenant cluster (none of the three) renders as before.
func TestChartRendersMachineLabelsAndSpeakerTolerations(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	render := func(values string) string {
		f := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(f, []byte(values), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(helm, "template", "sbx", "../../../deploy/charts/proxmox-kamaji-cluster", "-f", f).CombinedOutput()
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, out)
		}
		return string(out)
	}
	out := render(`
sandboxBandwidth: true
proxmox:
  metallb:
    speakerTolerations: [{key: proxcloud.io/sandbox-system, operator: Exists, effect: NoSchedule}]
    controllerNodeSelector: {node-restriction.kubernetes.io/proxcloud-system: "true"}
    controllerTolerations: [{key: proxcloud.io/sandbox-system, operator: Exists, effect: NoSchedule}]
nodeGroups:
  - name: sandbox-system
    machineFlavor: s
    machineImageId: "pve1=9210"
    machineCores: 2
    machineMemoryMiB: 2048
    machineRootVolume: {diskSize: 30}
    machineCount: 1
    machineLabels: {node-restriction.kubernetes.io/proxcloud-system: "true"}
    taints: [{key: proxcloud.io/sandbox-system, value: "true", effect: NoSchedule}]
`)
	if !strings.Contains(out, `\"bandwidthManager\":{\"enabled\":true}`) || !strings.Contains(out, `\"customConf\":true`) {
		t.Fatalf("sandbox Cilium lacks bandwidth/chained CNI config: %s", out)
	}
	_, md, ok := strings.Cut(out, "kind: MachineDeployment")
	if !ok {
		t.Fatalf("no MachineDeployment\n%s", out)
	}
	md, _, _ = strings.Cut(md, "\n---")
	_, tpl, _ := strings.Cut(md, "  template:")
	if !strings.Contains(tpl, `node-restriction.kubernetes.io/proxcloud-system: "true"`) {
		t.Errorf("the Machines lack the system label\n%s", md)
	}
	_, kt, _ := strings.Cut(out, "kind: KubeadmConfigTemplate")
	kt, _, _ = strings.Cut(kt, "\n---")
	if strings.Contains(kt, "node-restriction") {
		t.Errorf("the kubelet must not be asked to set a node-restriction label (NodeRestriction refuses it)\n%s", kt)
	}
	if !strings.Contains(kt, "proxcloud.io/sandbox-system") {
		t.Errorf("the system group's taint is missing\n%s", kt)
	}
	const tolerations = `\"tolerations\":[{\"effect\":\"NoSchedule\",\"key\":\"proxcloud.io/sandbox-system\",\"operator\":\"Exists\"}]`
	if !strings.Contains(out, `\"speaker\":{\"frr\":{\"enabled\":false},`+tolerations+`}`) {
		t.Errorf("MetalLB's speaker does not tolerate the system taint\n%s", out)
	}
	if !strings.Contains(out, `valuesTemplate: "{\"controller\":{\"nodeSelector\":{\"node-restriction.kubernetes.io/proxcloud-system\":\"true\"},`+tolerations+`},\"frrk8s\"`) {
		t.Errorf("MetalLB's controller is not held to the labels named, with the tolerations named\n%s", out)
	}

	if tenant := render("{}\n"); !strings.Contains(tenant, `valuesTemplate: "{\"frrk8s\":{\"enabled\":false},\"speaker\":{\"frr\":{\"enabled\":false}}}"`) {
		t.Errorf("a tenant cluster's MetalLB values must stay as they were (no roll of every tenant's MetalLB)\n%s", tenant)
	}
}

// ciliumOperatorTolerations (chart 0.1.20): on a cluster whose every node joins with a taint, Cilium's operator gets
// its own list and the taints named, so it starts on a node that is tainted and not Ready; without it no CRD of Cilium
// exists, no agent starts and no node becomes Ready (the sandbox cluster of v0.12.0-rc.2, drill E4-A). A tenant
// cluster sets nothing and its operator's values stay byte for byte what they were: its Deployment does not roll.
func TestChartLetsCiliumsOperatorTolerateTheClustersTaints(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	render := func(values string) string {
		f := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(f, []byte(values), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(helm, "template", "sbx", "../../../deploy/charts/proxmox-kamaji-cluster", "-f", f).CombinedOutput()
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, out)
		}
		return string(out)
	}
	out := render(`
ciliumOperatorTolerations:
  - {key: proxcloud.io/unsecured, operator: Exists, effect: NoSchedule}
  - {key: proxcloud.io/sandbox-system, operator: Exists, effect: NoSchedule}
`)
	want := `\"operator\":{\"replicas\":1,\"tolerations\":[` +
		`{\"key\":\"node-role.kubernetes.io/control-plane\",\"operator\":\"Exists\"},{\"key\":\"node-role.kubernetes.io/master\",\"operator\":\"Exists\"},` +
		`{\"key\":\"node.kubernetes.io/not-ready\",\"operator\":\"Exists\"},{\"key\":\"node.cloudprovider.kubernetes.io/uninitialized\",\"operator\":\"Exists\"},` +
		`{\"effect\":\"NoSchedule\",\"key\":\"proxcloud.io/unsecured\",\"operator\":\"Exists\"},{\"effect\":\"NoSchedule\",\"key\":\"proxcloud.io/sandbox-system\",\"operator\":\"Exists\"}]}`
	if !strings.Contains(out, want) {
		t.Errorf("Cilium's operator does not tolerate the cluster's taints on top of its own list\n%s", out)
	}
	// Only Cilium's operator: MetalLB's values carry no toleration from it.
	if strings.Count(out, "proxcloud.io/unsecured") != 1 {
		t.Errorf("the taint is named %d times in the render, want once (Cilium's operator)", strings.Count(out, "proxcloud.io/unsecured"))
	}
	if strings.Contains(out, "nodeSelector") {
		t.Errorf("no ciliumOperatorNodeSelector was set, and the operator's values carry a selector\n%s", out)
	}
	if tenant := render("{}\n"); !strings.Contains(tenant, `\"operator\":{\"replicas\":1},\"routingMode\"`) {
		t.Errorf("a tenant cluster's Cilium values must stay as they were (no roll of every tenant's operator)\n%s", tenant)
	}
	// ciliumOperatorNodeSelector (chart 0.1.20): the labels named, and Cilium's own kubernetes.io/os: linux with them,
	// whatever the value says of that key. With the selector alone the operator keeps Cilium's own tolerations.
	pinned := render("ciliumOperatorNodeSelector: {node-restriction.kubernetes.io/proxcloud-system: \"true\", kubernetes.io/os: windows}\n")
	if want := `\"operator\":{\"nodeSelector\":{\"kubernetes.io/os\":\"linux\",\"node-restriction.kubernetes.io/proxcloud-system\":\"true\"},\"replicas\":1},\"routingMode\"`; !strings.Contains(pinned, want) {
		t.Errorf("Cilium's operator is not held to the labels named, with kubernetes.io/os: linux\n%s", pinned)
	}
}

// chart0119TenantRender is the sha256 of what the published chart 0.1.19 renders for a tenant cluster with the
// chart's default values (helm template pc-c-abcd1234 -n pc-p1 with Helm goldenHelm; taken from the archive the BOM of
// 0.12.0-rc.2 pins by digest and from the tree at tag proxmox-kamaji-cluster-v0.1.19, which agree).
const chart0119TenantRender = "1e999008762bb6fbff4c511ff70b9633e981a7f02509f261d660c2f72e565cfa"

// A tenant cluster, which sets none of the values chart 0.1.20 adds (ciliumOperatorTolerations,
// ciliumOperatorNodeSelector, proxmox.metallb.controllerNodeSelector and controllerTolerations), is rendered by this
// chart byte for byte as chart 0.1.19 rendered it: a cluster that moves to the new chart has no add-on value changed,
// so the add-on controller upgrades no release and no pod of Cilium or MetalLB is replaced. A chart change that is
// meant to move a tenant cluster's render moves this constant with it, and its notes then say what rolls.
func TestChartRendersATenantClusterAsChart0119(t *testing.T) {
	got := helmTemplate(t, "pc-c-abcd1234", "pc-p1", "{}\n")
	if sum := fmt.Sprintf("%x", sha256.Sum256([]byte(got))); sum != chart0119TenantRender {
		t.Errorf("with default values the chart renders sha256 %s, chart 0.1.19 rendered %s: a tenant cluster's objects or add-on values change\n%s", sum, chart0119TenantRender, got)
	}
}

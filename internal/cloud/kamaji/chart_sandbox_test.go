package kamaji

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The sandbox cluster's system node group (installer stage 55): its trusted label goes on the Machines, where CAPI
// copies it to the Node, never into the kubelet's own --node-labels; and MetalLB's speaker tolerates its taint, so
// sandbox-proxy's address is announced from there. A tenant cluster (no speakerTolerations) renders as before.
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
	if !strings.Contains(out, `\"tolerations\":[{\"effect\":\"NoSchedule\",\"key\":\"proxcloud.io/sandbox-system\",\"operator\":\"Exists\"}]`) {
		t.Errorf("MetalLB's speaker does not tolerate the system taint\n%s", out)
	}

	if tenant := render("{}\n"); !strings.Contains(tenant, `\"speaker\":{\"frr\":{\"enabled\":false}}`) {
		t.Errorf("a tenant cluster's speaker values must stay as they were (no roll of every tenant's MetalLB)\n%s", tenant)
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
	if tenant := render("{}\n"); !strings.Contains(tenant, `\"operator\":{\"replicas\":1},\"routingMode\"`) {
		t.Errorf("a tenant cluster's Cilium values must stay as they were (no roll of every tenant's operator)\n%s", tenant)
	}
}

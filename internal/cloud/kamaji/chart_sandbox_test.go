package kamaji

import (
	"encoding/json"
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

// Separate placement gets the function objects through this chart: addons.sandbox.values is passed to the sandbox
// chart's HelmChartProxy as it is, the functions block and a portal CA of several lines too, and the sandbox chart
// renders from exactly those values, in mode separate, the function objects that mode needs: the policy that holds
// the portal identity's cluster-wide Deployment and Secret rights to fn-* (no RoleBinding does there), the egress
// policy, and the proxy's function mode with the CA. CAAPH reads valuesTemplate as a Go template, so nothing in the
// block may open one.
func TestSandboxAddonCarriesTheFunctionValues(t *testing.T) {
	const ca = "-----BEGIN CERTIFICATE-----\nMIIBexample\nline2==\n-----END CERTIFICATE-----\n"
	out := helmTemplate(t, "pc-sandbox", "pc-sandbox", `addons:
  sandbox:
    enabled: true
    values:
      proxy: {address: 192.0.2.151, domain: sbx.example.org}
      functions:
        enabled: true
        wakeURL: https://192.0.2.10/api/v1/internal/functions
        wakeTokenVersion: "3"
        portalCA: |
          -----BEGIN CERTIFICATE-----
          MIIBexample
          line2==
          -----END CERTIFICATE-----
`)
	var tpl string
	for _, o := range renderedObjects(t, out) {
		if o["kind"] == "HelmChartProxy" && o["metadata"].(map[string]any)["name"] == "pc-sandbox-sandbox" {
			tpl = o["spec"].(map[string]any)["valuesTemplate"].(string)
		}
	}
	if tpl == "" {
		t.Fatalf("no sandbox HelmChartProxy:\n%s", out)
	}
	if strings.Contains(tpl, "{{") {
		t.Errorf("valuesTemplate opens a template: %s", tpl)
	}
	var values struct {
		Mode      string         `json:"mode"`
		Functions map[string]any `json:"functions"`
	}
	if err := json.Unmarshal([]byte(tpl), &values); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"enabled": true, "wakeURL": "https://192.0.2.10/api/v1/internal/functions", "wakeTokenVersion": "3", "portalCA": ca}
	if values.Mode != "separate" || len(values.Functions) != len(want) {
		t.Fatalf("values %s", tpl)
	}
	for k, w := range want {
		if values.Functions[k] != w {
			t.Errorf("functions.%s = %q, want %q", k, values.Functions[k], w)
		}
	}

	// What the sandbox cluster then installs: the sandbox chart with those values, as the add-on controller renders it.
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not installed") // helmTemplate above has already failed under CI
	}
	f := filepath.Join(t.TempDir(), "values.json")
	if err := os.WriteFile(f, []byte(tpl), 0o600); err != nil {
		t.Fatal(err)
	}
	rendered, err := exec.Command(helm, "template", "sandbox", "../../../deploy/charts/proxcloud-sandbox", "-n", "sandbox-system", "-f", f).CombinedOutput()
	if err != nil {
		t.Fatalf("the sandbox chart does not render from the add-on's values: %v\n%s", err, rendered)
	}
	have := map[string]bool{}
	var caData, tokenVersion any
	for _, o := range renderedObjects(t, string(rendered)) {
		name, _ := o["metadata"].(map[string]any)["name"].(string)
		have[o["kind"].(string)+"/"+name] = true
		switch o["kind"].(string) + "/" + name {
		case "ConfigMap/sandbox-proxy-portal-ca":
			caData = o["data"].(map[string]any)["ca.crt"]
		case "Deployment/sandbox-proxy":
			meta := o["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)
			if a, ok := meta["annotations"].(map[string]any); ok {
				tokenVersion = a["proxcloud.io/function-wake-token-version"]
			}
		}
	}
	for _, w := range []string{"ValidatingAdmissionPolicy/proxcloud-function-objects", "ValidatingAdmissionPolicyBinding/proxcloud-function-objects",
		"CiliumClusterwideNetworkPolicy/function-tenant-egress-internet", "CiliumNodeConfig/sandbox-socket-lb"} {
		if !have[w] {
			t.Errorf("the sandbox cluster gets no %s", w)
		}
	}
	if have["ValidatingAdmissionPolicy/proxcloud-sandbox-namespaces"] || have["ClusterRole/proxcloud-function-namespace"] {
		t.Error("the add-on rendered mode shared's objects")
	}
	if caData != ca {
		t.Errorf("the portal CA reached the proxy as %q, want %q", caData, ca)
	}
	if tokenVersion != "3" {
		t.Errorf("the token version on the proxy's pods is %v", tokenVersion)
	}
}

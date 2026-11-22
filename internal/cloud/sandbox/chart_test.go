package sandbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

var updateChart = flag.Bool("update", false, "rewrite the sandbox chart goldens and the policy fixture")

const (
	sandboxChart = "../../../deploy/charts/proxcloud-sandbox"
	scl          = "../../../deploy/sandbox-cluster/"
	// The pods the portal's BuildCR makes, which test/policy runs through the chart's pod policy.
	policyFixture = "../../../test/policy/testdata/buildcr-pods.json"
)

// renderSandboxChart renders the chart with one of its examples, in namespace sandbox-system, as ArgoCD does.
func renderSandboxChart(t *testing.T, mode string, set ...string) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	args := append([]string{"template", "sandbox", sandboxChart, "-n", "sandbox-system", "-f", sandboxChart + "/examples/values-" + mode + ".yaml"}, set...)
	var stderr bytes.Buffer
	cmd := exec.Command(helm, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm %v: %v\n%s", args, err, stderr.String())
	}
	return string(out)
}

func yamlDocs(t *testing.T, text string) []map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(text))
	var docs []map[string]any
	for {
		var d map[string]any
		if err := dec.Decode(&d); errors.Is(err, io.EOF) {
			return docs
		} else if err != nil {
			t.Fatal(err)
		}
		if d != nil {
			docs = append(docs, d)
		}
	}
}

func find(t *testing.T, docs []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, d := range docs {
		if d["kind"] == kind && d["metadata"].(map[string]any)["name"] == name {
			return d
		}
	}
	t.Fatalf("no %s %s", kind, name)
	return nil
}

func readSCL(t *testing.T, file string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(scl + file)
	if err != nil {
		t.Fatal(err)
	}
	return yamlDocs(t, string(b))
}

// elideVendored puts a digest line in place of each agent-sandbox document the chart passes through verbatim (430 kB,
// reviewed as the vendored file), so a golden keeps the chart's own bytes and still moves when the file does.
func elideVendored(out string) string {
	const src = "# Source: proxcloud-sandbox/templates/agent-sandbox.yaml\n"
	parts := strings.Split(out, "\n---\n")
	for i, p := range parts {
		if strings.HasPrefix(p, src) && !strings.Contains(p, "\nkind: Deployment\n") {
			parts[i] = fmt.Sprintf("%s# vendored document sha256:%x", src, sha256.Sum256([]byte(p)))
		}
	}
	return strings.Join(parts, "\n---\n")
}

// The chart's whole output in both modes. `go test ./internal/cloud/sandbox -run TestSandboxChartGolden -update`
// rewrites the goldens (with the Helm of internal/cloud/kamaji goldenHelm).
func TestSandboxChartGolden(t *testing.T) {
	for _, mode := range []string{"shared", "separate"} {
		t.Run(mode, func(t *testing.T) {
			got := elideVendored(renderSandboxChart(t, mode))
			golden := filepath.Join("testdata", "chart-"+mode+".golden.yaml")
			if *updateChart {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden (run with -update to create): %v", err)
			}
			if got != string(want) {
				t.Errorf("render differs from %s (review, then -update):\n%s", golden, got)
			}
		})
	}
}

// What the chart takes from deploy/sandbox-cluster stays equal to it: the node-setup script and its runsc pin, the
// guardrail (with no extraDeny), the warming deny-all, the RuntimeClass handler, the telemetry collector, the network
// guard's RBAC and the vendored agent-sandbox. The installer path and the platform path then install and fence the
// same way.
func TestSandboxChartEqualsTheInstallerFiles(t *testing.T) {
	inst := map[string][]map[string]any{}
	for _, f := range []string{"node-setup.yaml", "network-policy.yaml", "runtimeclass.yaml", "telemetry.yaml"} {
		inst[f] = readSCL(t, f)
	}
	for _, mode := range []string{"shared", "separate"} {
		docs := yamlDocs(t, renderSandboxChart(t, mode, "--set", "guardrail.extraDeny=null"))

		cm := find(t, docs, "ConfigMap", "node-setup")["data"].(map[string]any)["setup.sh"]
		if want := find(t, inst["node-setup.yaml"], "ConfigMap", "node-setup")["data"].(map[string]any)["setup.sh"]; cm != want {
			t.Errorf("%s: node-setup's setup.sh differs from deploy/sandbox-cluster/node-setup.yaml", mode)
		}
		container := func(ds map[string]any, name string) map[string]any {
			spec := ds["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			for _, list := range []string{"containers", "initContainers"} {
				for _, c := range spec[list].([]any) {
					if c.(map[string]any)["name"] == name {
						return c.(map[string]any)
					}
				}
			}
			t.Fatalf("no container %s", name)
			return nil
		}
		gotDS, wantDS := find(t, docs, "DaemonSet", "node-setup"), find(t, inst["node-setup.yaml"], "DaemonSet", "node-setup")
		for _, k := range []string{"env", "command", "args", "volumeMounts", "securityContext", "readinessProbe"} {
			if g, w := container(gotDS, "setup")[k], container(wantDS, "setup")[k]; !reflect.DeepEqual(g, w) {
				t.Errorf("%s: node-setup container %s\n got %v\nwant %v", mode, k, g, w)
			}
		}
		for _, k := range []string{"volumes", "hostPID", "hostNetwork", "serviceAccountName", "tolerations", "priorityClassName"} {
			g := gotDS["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)[k]
			w := wantDS["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)[k]
			if !reflect.DeepEqual(g, w) {
				t.Errorf("%s: node-setup pod %s\n got %v\nwant %v", mode, k, g, w)
			}
		}
		for _, n := range []string{"sandbox-tenant-egress", "sandbox-tenant-ingress"} {
			if g, w := find(t, docs, "CiliumClusterwideNetworkPolicy", n)["spec"], find(t, inst["network-policy.yaml"], "CiliumClusterwideNetworkPolicy", n)["spec"]; !reflect.DeepEqual(g, w) {
				t.Errorf("%s: %s differs from deploy/sandbox-cluster/network-policy.yaml\n got %v\nwant %v", mode, n, g, w)
			}
		}
		if g, w := find(t, docs, "CiliumNetworkPolicy", "sandbox-warming-deny-all")["spec"], find(t, inst["network-policy.yaml"], "CiliumNetworkPolicy", "sandbox-warming-deny-all")["spec"]; !reflect.DeepEqual(g, w) {
			t.Errorf("%s: sandbox-warming-deny-all differs", mode)
		}
		if g, w := find(t, docs, "RuntimeClass", "gvisor")["handler"], find(t, inst["runtimeclass.yaml"], "RuntimeClass", "gvisor")["handler"]; g != w {
			t.Errorf("%s: RuntimeClass handler %v, want %v", mode, g, w)
		}
		gotT := find(t, docs, "DaemonSet", "sandbox-telemetry")["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		wantT := find(t, inst["telemetry.yaml"], "DaemonSet", "sandbox-telemetry")["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		for _, k := range []string{"volumes", "hostNetwork", "serviceAccountName", "tolerations"} {
			if !reflect.DeepEqual(gotT[k], wantT[k]) {
				t.Errorf("%s: telemetry pod %s differs", mode, k)
			}
		}
		gc, wc := gotT["containers"].([]any)[0].(map[string]any), wantT["containers"].([]any)[0].(map[string]any)
		for _, k := range []string{"command", "env", "securityContext", "volumeMounts"} {
			if !reflect.DeepEqual(gc[k], wc[k]) {
				t.Errorf("%s: telemetry container %s differs", mode, k)
			}
		}
		// What the network guard may read (its Node, the CiliumNodeConfigs of kube-system) is what the installer grants.
		for _, r := range [][2]string{{"ClusterRole", "sandbox-netguard"}, {"Role", "sandbox-netguard-cilium-read"}} {
			if g, w := find(t, docs, r[0], r[1])["rules"], find(t, inst["telemetry.yaml"], r[0], r[1])["rules"]; !reflect.DeepEqual(g, w) {
				t.Errorf("%s: %s %s rules differ from deploy/sandbox-cluster/telemetry.yaml\n got %v\nwant %v", mode, r[0], r[1], g, w)
			}
		}
	}
	for _, f := range []string{"sandbox-with-extensions.yaml", "LICENSE"} {
		a, errA := os.ReadFile(scl + "agent-sandbox/" + f)
		b, errB := os.ReadFile(sandboxChart + "/files/agent-sandbox/" + f)
		if errA != nil || errB != nil || !bytes.Equal(a, b) {
			t.Errorf("the chart's files/agent-sandbox/%s is not a copy of deploy/sandbox-cluster/agent-sandbox/%s (%v, %v)", f, f, errA, errB)
		}
	}
	vendored := readSCL(t, "agent-sandbox/sandbox-with-extensions.yaml")
	docs := yamlDocs(t, renderSandboxChart(t, "shared"))
	for _, w := range vendored {
		meta := w["metadata"].(map[string]any)
		g := find(t, docs, w["kind"].(string), meta["name"].(string))
		if w["kind"] == "Deployment" {
			spec := g["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			delete(spec, "nodeSelector")
			delete(spec, "tolerations")
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("agent-sandbox %v %v differs from the vendored file", w["kind"], meta["name"])
		}
	}
}

// Placement and mode: in shared mode every node-side DaemonSet runs on the sandbox group alone (node-setup installs runsc
// and restarts containerd, which no platform node may get), the system side on the system group, the RuntimeClass
// places gVisor pods on the sandbox group with exactly one toleration (never proxcloud.io/unsecured), and the
// CiliumNodeConfig chains the guard on the sandbox nodes with write-cni-conf-when-ready present and empty. In separate
// mode the installer's shape: node-side on every node, no scheduling, the cluster-wide identities.
func TestSandboxChartPlacement(t *testing.T) {
	podSpec := func(d map[string]any) map[string]any {
		return d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	}
	shared := yamlDocs(t, renderSandboxChart(t, "shared"))
	sandboxSel := map[string]any{"node-restriction.kubernetes.io/proxcloud-node-group": "sandbox"}
	systemSel := map[string]any{"node-restriction.kubernetes.io/proxcloud-node-group": "system"}
	daemonsets := 0
	for _, d := range shared {
		switch d["kind"] {
		case "DaemonSet":
			daemonsets++
			if s := podSpec(d)["nodeSelector"]; !reflect.DeepEqual(s, sandboxSel) {
				t.Errorf("DaemonSet %v: nodeSelector %v, want the sandbox group", d["metadata"].(map[string]any)["name"], s)
			}
		case "Deployment":
			if s := podSpec(d)["nodeSelector"]; !reflect.DeepEqual(s, systemSel) {
				t.Errorf("Deployment %v: nodeSelector %v, want the system group", d["metadata"].(map[string]any)["name"], s)
			}
		case "Namespace":
			if n := d["metadata"].(map[string]any)["name"]; n == "sandbox-system" {
				t.Error("the chart must not create its release namespace (ArgoCD and CAAPH do), nor place it")
			}
			if a := d["metadata"].(map[string]any)["annotations"]; a != nil {
				t.Errorf("namespace %v carries placement annotations %v", d["metadata"].(map[string]any)["name"], a)
			}
		}
	}
	if daemonsets != 3 {
		t.Errorf("%d DaemonSets, want node-setup, telemetry and template-prepull", daemonsets)
	}
	rc := find(t, shared, "RuntimeClass", "gvisor")["scheduling"]
	want := map[string]any{"nodeSelector": sandboxSel, "tolerations": []any{map[string]any{"key": "proxcloud.io/sandbox", "operator": "Exists", "effect": "NoSchedule"}}}
	if !reflect.DeepEqual(rc, want) {
		t.Errorf("RuntimeClass scheduling %v, want %v", rc, want)
	}
	cnc := find(t, shared, "CiliumNodeConfig", "sandbox-nodes")["spec"].(map[string]any)
	if got := cnc["nodeSelector"]; !reflect.DeepEqual(got, map[string]any{"matchLabels": map[string]any{"proxcloud.io/node-group": "sandbox"}}) {
		t.Errorf("CiliumNodeConfig nodeSelector %v", got)
	}
	defaults := cnc["defaults"].(map[string]any)
	if v, ok := defaults["write-cni-conf-when-ready"]; !ok || v != "" {
		t.Errorf("write-cni-conf-when-ready must be present and empty, got %v (present %v)", v, ok)
	}
	for k, v := range map[string]string{"custom-cni-conf": "true", "enable-bandwidth-manager": "true", "bpf-lb-sock-hostns-only": "true"} {
		if defaults[k] != v {
			t.Errorf("CiliumNodeConfig %s = %v, want %q", k, defaults[k], v)
		}
	}
	for _, n := range []string{"proxcloud-sandbox-namespaces", "proxcloud-sandbox-rolebindings", "proxcloud-sandbox-firewall-nodes", "proxcloud-sandbox-tenant-pods"} {
		find(t, shared, "ValidatingAdmissionPolicy", n)
		find(t, shared, "ValidatingAdmissionPolicyBinding", n)
	}
	for _, r := range find(t, shared, "ClusterRole", "proxcloud-sandbox")["rules"].([]any) {
		for _, res := range r.(map[string]any)["resources"].([]any) {
			if res != "namespaces" && res != "rolebindings" && res != "clusterroles" && res != "nodes" {
				t.Errorf("shared: the portal's cluster-wide role grants %v; everything namespaced goes through the per-namespace RoleBinding", res)
			}
		}
	}
	fw := find(t, shared, "ClusterRole", "proxcloud-sandbox-firewall")["rules"].([]any)
	if last := fw[len(fw)-1].(map[string]any); !reflect.DeepEqual(last["resources"], []any{"runtimeclasses"}) || !reflect.DeepEqual(last["resourceNames"], []any{"gvisor"}) {
		t.Errorf("the firewall identity (the fence gate) must get RuntimeClass gvisor: %v", last)
	}

	separate := yamlDocs(t, renderSandboxChart(t, "separate"))
	for _, d := range separate {
		if d["kind"] == "DaemonSet" && podSpec(d)["nodeSelector"] != nil {
			t.Errorf("separate: DaemonSet %v has a nodeSelector; every node of the sandbox cluster runs it", d["metadata"].(map[string]any)["name"])
		}
		if d["kind"] == "ValidatingAdmissionPolicy" && d["metadata"].(map[string]any)["name"] != "proxcloud-sandbox-tenant-pods" && d["metadata"].(map[string]any)["name"] != "proxcloud-noderestriction-probe-dry-run-only" {
			t.Errorf("separate: policy %v is shared mode's", d["metadata"].(map[string]any)["name"])
		}
	}
	if s := find(t, separate, "RuntimeClass", "gvisor")["scheduling"]; s != nil {
		t.Errorf("separate: RuntimeClass scheduling %v", s)
	}
	find(t, separate, "CiliumNodeConfig", "sandbox-socket-lb")
	if d := podSpec(find(t, separate, "Deployment", "sandbox-proxy")); !reflect.DeepEqual(d["nodeSelector"], map[string]any{"node-restriction.kubernetes.io/proxcloud-system": "true"}) {
		t.Errorf("separate: proxy nodeSelector %v", d["nodeSelector"])
	}
}

// The pods BuildCR makes (envd, native, GPU), as the agent-sandbox controller creates them, written for test/policy,
// which runs them through the chart's pod policy: the policy must admit every one. -update rewrites the fixture.
func TestPolicyFixtureIsBuildCR(t *testing.T) {
	cfg := Config{Templates: map[string]Template{"base": {Image: "ghcr.io/mutex-solutions/sandbox-base@sha256:" + strings.Repeat("1", 64)}}}
	pods := map[string]any{}
	for _, c := range []struct {
		name string
		envd bool
		gpu  *GPURequest
	}{{"envd", true, nil}, {"native", false, nil}, {"gpu", true, &GPURequest{Model: "nvidia-a6000", Count: 1}}} {
		cfg := cfg
		cfg.Envd = c.envd
		cr := BuildCR(cfg, "svc", Spec{ID: "sbx-00000001", ProjectID: "p1", Template: "base", CPU: 1, MemoryGiB: 1, GPU: c.gpu}, time.Unix(1800000000, 0).UTC())
		tpl := cr["spec"].(map[string]any)["podTemplate"].(map[string]any)
		pods[c.name] = map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "sbx-00000001", "namespace": "sb-p1"}, "spec": tpl["spec"]}
	}
	got, err := json.MarshalIndent(pods, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *updateChart {
		if err := os.WriteFile(policyFixture, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(policyFixture)
	if err != nil {
		t.Fatalf("read %s (run with -update to create): %v", policyFixture, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is not what BuildCR makes now: run with -update, then test/policy", policyFixture)
	}
}

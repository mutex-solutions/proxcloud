package sandbox

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestRotationDefaultsOffAndExactScope(t *testing.T) {
	if (Config{}.WithDefaults().Rotation.Enabled) {
		t.Fatal("rotation default on")
	}
	for _, p := range []RotationPolicy{{Enabled: true}, {Enabled: true, IntervalHours: 1, Namespace: "pc-sandbox", Cluster: "sandbox", MachineDeployments: []string{"worker"}}, {Enabled: true, IntervalHours: 24, Namespace: "../other", Cluster: "sandbox", MachineDeployments: []string{"worker"}}} {
		if p.Validate() == nil {
			t.Fatal(p)
		}
	}
	s := NewWithAPI(&fakeAPI{}, Config{}, "svc")
	r, e := s.RotationInspect(context.Background())
	if e != nil || r.Ready || len(r.Reasons) == 0 {
		t.Fatal(r, e)
	}
}
func TestSandboxInstallerSafetyContracts(t *testing.T) {
	stage, e := os.ReadFile("../../../installer/playbooks/55-sandbox-cluster.yml")
	if e != nil {
		t.Fatal(e)
	}
	text := string(stage)
	if strings.Index(text, "kubectl apply -f node-setup.yaml") > strings.Index(text, "ready=$(kubectl get nodes") || strings.Contains(text, "- name: Wait for every worker to be Ready") || strings.Index(text, "Cilium customConf mode has not converged") > strings.Index(text, "kubectl apply -f node-setup.yaml") {
		t.Fatal("Ready wait precedes host CNI bootstrap")
	}
	for path, wants := range map[string][]string{
		"../../../deploy/sandbox-cluster/node-setup.yaml":               {"hostNetwork: true", "sandbox-netguard -install", "serviceAccountName: sandbox-netguard"},
		"../../../deploy/sandbox-cluster/telemetry.yaml":                {"resources: [nodes/stats]", "runAsUser: 65534", "/var/lib/kubelet/pki/kubelet.crt"},
		"../../../installer/playbooks/templates/sandbox-cluster.yml.j2": {"sandboxBandwidth: true", "node-restriction.kubernetes.io/proxcloud-sandbox-worker"},
		"../../../installer/Makefile":                                   {"playbooks/55-sandbox-cluster.yml"},
	} {
		b, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		for _, w := range wants {
			if !strings.Contains(string(b), w) {
				t.Fatalf("%s missing %s", path, w)
			}
		}
		if strings.Contains(string(b), "resources: [nodes/proxy]") {
			t.Fatal("kubelet write through proxy")
		}
	}
}

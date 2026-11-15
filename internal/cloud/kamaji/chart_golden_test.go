package kamaji

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite the chart goldens in testdata/")

// goldenHelm is the Helm the goldens are made with: run -update with it. The go-test job of .github/workflows/test.yml
// pins the same (test_release_policy.py holds them equal), because another Helm may format the same render otherwise.
const goldenHelm = "v3.21.4"

// helmTemplate renders the cluster chart with one values document, as ArgoCD does for an Application.
func helmTemplate(t *testing.T, release, namespace, values string) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	f := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(f, []byte(values), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(helm, "template", release, "../../../deploy/charts/proxmox-kamaji-cluster", "-n", namespace, "-f", f).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	return string(out)
}

// The chart's whole output for fixed values documents. A chart change that moves a render shows up as a diff of these
// files to review; `go test ./internal/cloud/kamaji -run TestChartGolden -update` rewrites them. The tenant cases are
// the chart's own defaults, alone and with the two tenant templates the defaults leave out (the services-side CSI
// controller, the Gateway API carriers of Gateway Fabric), so a change of the portal's values (BuildValues) does not
// touch them; the kubeadm case is the platform's own cluster (controlPlane.kind kubeadm, kubeadmTestValues).
func TestChartGolden(t *testing.T) {
	for _, c := range []struct{ name, release, namespace, values string }{
		{"tenant", "pc-c-abcd1234", "pc-p1", "{}\n"},
		{"tenant-csi-gateway", "pc-c-abcd1234", "pc-p1", "proxmox:\n  csi:\n    gateway:\n      secretName: " +
			CSIGatewaySecretName("pc-c-abcd1234") + "\n      kubeconfigSecretName: " + AdminKubeconfigSecretName("pc-c-abcd1234") + "\n"},
		{"tenant-gateway-fabric", "pc-c-abcd1234", "pc-p1", "addons:\n  nginxGatewayFabric: {enabled: true}\n"},
		{"kubeadm", "pc-platform", "pc-platform", kubeadmTestValues},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := elideGatewayAPIFiles(t, helmTemplate(t, c.release, c.namespace, c.values))
			golden := filepath.Join("testdata", "chart-"+c.name+".golden.yaml")
			if *updateGolden {
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
				t.Errorf("render differs from %s (review, then -update with helm %s):\n%s", golden, goldenHelm, got)
			}
		})
	}
}

// elideGatewayAPIFiles puts a digest line in place of each pinned upstream Gateway API file the chart embeds verbatim
// (1.2 MB, reviewed as files of their own), so a golden keeps the template's own bytes and still moves when a file does.
func elideGatewayAPIFiles(t *testing.T, out string) string {
	t.Helper()
	files, err := filepath.Glob("../../../deploy/charts/proxmox-kamaji-cluster/files/gateway-api/v1.6.3/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("gateway-api files: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// What `indent 4` and the trimming `{{- end }}` of templates/gateway-api.yaml make of the file.
		body := strings.TrimRight("    "+strings.ReplaceAll(string(b), "\n", "\n    "), " \n")
		out = strings.ReplaceAll(out, body, fmt.Sprintf("    # %s sha256:%x", filepath.Base(f), sha256.Sum256(b)))
	}
	return out
}

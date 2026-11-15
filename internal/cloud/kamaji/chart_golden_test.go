package kamaji

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite the chart goldens in testdata/")

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
// files to review; `go test ./internal/cloud/kamaji -run TestChartGolden -update` rewrites them. The tenant case is the
// chart's own defaults, so a change of the portal's values (BuildValues) does not touch it.
func TestChartGolden(t *testing.T) {
	for _, c := range []struct{ name, release, namespace, values string }{
		{"tenant", "pc-c-abcd1234", "pc-p1", "{}\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := helmTemplate(t, c.release, c.namespace, c.values)
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
				t.Errorf("render differs from %s (review, then -update):\n%s", golden, got)
			}
		})
	}
}

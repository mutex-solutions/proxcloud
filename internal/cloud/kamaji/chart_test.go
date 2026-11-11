package kamaji

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// renderChart runs the portal's own values through the real chart. The chart's default values name a proxmox-csi class, so
// what keeps the token out of a tenant cluster is this pair: the portal's empty list and the chart's reading of it.
func renderChart(t *testing.T, spec ClusterSpec) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	cfg := testCfg()
	cfg.Defaults.Versions[spec.Version] = "pve1=9210" // the chart reads a template per node
	raw, err := json.Marshal(BuildValues(cfg, spec))
	if err != nil {
		t.Fatal(err)
	}
	values := filepath.Join(t.TempDir(), "values.json")
	if err := os.WriteFile(values, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(helm, "template", spec.ID, "../../../deploy/charts/proxmox-kamaji-cluster", "-f", values).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	return string(out)
}

func TestChartRendersTheCSISwitch(t *testing.T) {
	spec := testSpec()
	spec.Infra = ClusterInfra{Region: "r", Pool: "pc-p1", Storage: "local-zfs", Bridge: "v1", Gateway: "192.0.2.1", Prefix: 24,
		Addresses: []string{"192.0.2.200"}, LBAddresses: []string{"192.0.2.216/32"}, RWXServer: "192.0.2.5", RWXShare: "/mnt/x"}
	const nfsDefault = `\"storageclass.kubernetes.io/is-default-class\":\"true\"`
	proxy := spec.ID + "-proxmoxcsi"
	secret := CSISecretName(spec.ID)

	off := renderChart(t, spec)
	for _, absent := range []string{proxy, secret, "csi-proxmox", "existingConfigSecret"} {
		if strings.Contains(off, absent) {
			t.Errorf("off renders %q: the tenant cluster would get the token\n%s", absent, off)
		}
	}
	if !strings.Contains(off, spec.ID+"-nfscsi") || !strings.Contains(off, nfsDefault) {
		t.Errorf("off: the dataset's class must be the default one\n%s", off)
	}

	spec.Infra.StorageClasses = []StorageClass{{Name: "std", Storage: "local-zfs"}}
	on := renderChart(t, spec)
	for _, present := range []string{proxy, secret} {
		if !strings.Contains(on, present) {
			t.Errorf("on does not render %q", present)
		}
	}
	if strings.Count(on, nfsDefault) != 1 {
		t.Errorf("on: exactly one default class, proxmox-csi's first\n%s", on)
	}
}

// With the gateway, the controller runs on the services cluster with the tenant's kubeconfig on every container, and the
// tenant's own controller is rendered at 0 replicas: nothing in the tenant reads a config.
func TestChartRendersTheGatewayController(t *testing.T) {
	spec := testSpec()
	spec.Infra = ClusterInfra{Region: "r", Pool: "pc-p1", Storage: "local-zfs", Bridge: "v1", Gateway: "192.0.2.1", Prefix: 24,
		Addresses: []string{"192.0.2.200"}, LBAddresses: []string{"192.0.2.216/32"},
		StorageClasses: []StorageClass{{Name: "std", Storage: "local-zfs"}}}
	tenant := renderChart(t, spec)
	if strings.Contains(tenant, "-csi-controller") || !strings.Contains(tenant, `\"replicaCount\":1`) {
		t.Fatalf("without the gateway the tenant runs its own controller\n%s", tenant)
	}

	spec.Infra.CSIGateway = true
	out := renderChart(t, spec)
	if !strings.Contains(out, `\"replicaCount\":0`) {
		t.Errorf("the tenant's controller must be at 0 replicas\n%s", out)
	}
	_, dep, ok := strings.Cut(out, "name: "+spec.ID+"-csi-controller")
	if !ok {
		t.Fatalf("no services-side controller\n%s", out)
	}
	dep, _, _ = strings.Cut(dep, "\n---")
	if n := strings.Count(dep, `"--kubeconfig=/etc/tenant/kubeconfig"`); n != 4 {
		t.Errorf("%d containers with the tenant kubeconfig, want 4\n%s", n, dep)
	}
	if n := strings.Count(dep, `"--leader-election-namespace=csi-proxmox"`); n != 3 {
		t.Errorf("%d sidecars electing in the tenant, want 3\n%s", n, dep)
	}
	for _, want := range []string{"secretName: " + CSIGatewaySecretName(spec.ID), "secretName: " + AdminKubeconfigSecretName(spec.ID),
		"key: admin.svc", "automountServiceAccountToken: false"} {
		if !strings.Contains(dep, want) {
			t.Errorf("controller lacks %q\n%s", want, dep)
		}
	}
}

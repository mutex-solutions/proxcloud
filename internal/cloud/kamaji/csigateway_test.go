package kamaji

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The gateway's controller Secret is written next to the cloud secret, named in the values (with the Kamaji kubeconfig),
// never put in them, and removed with the cluster's other secrets.
func TestCSIGatewaySecretAndValues(t *testing.T) {
	ctx := context.Background()
	api := newFakeAPI()
	svc := NewWithAPI(api, testCfg(), "svc-1")
	spec := testSpec()
	spec.Infra.StorageClasses = []StorageClass{{Name: "std", Storage: "local-zfs"}}
	spec.Infra.CSIGateway = true
	sec := ClusterSecrets{Cloud: map[string]string{"token": "t"}, CSIManifest: "ns",
		CSIGateway: map[string]string{"config.yaml": "token_secret: s3cr3t", "ca.crt": "CA"}}
	if _, err := svc.CreateCluster(ctx, spec, sec); err != nil {
		t.Fatal(err)
	}
	key := "pc-p1/" + CSIGatewaySecretName(spec.ID)
	if got := api.secrets[key]; got["config.yaml"] != "token_secret: s3cr3t" || got["ca.crt"] != "CA" {
		t.Fatalf("gateway secret = %v", got)
	}
	v := BuildValues(testCfg(), spec)
	if got := dig(v, "proxmox", "csi", "gateway", "secretName"); got != CSIGatewaySecretName(spec.ID) {
		t.Errorf("gateway.secretName = %v", got)
	}
	if got := dig(v, "proxmox", "csi", "gateway", "kubeconfigSecretName"); got != AdminKubeconfigSecretName(spec.ID) {
		t.Errorf("gateway.kubeconfigSecretName = %v", got)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "s3cr3t") {
		t.Fatalf("values carry the credential: %s", raw)
	}
	spec.Infra.CSIGateway = false
	if got := dig(BuildValues(testCfg(), spec), "proxmox", "csi", "gateway"); got != nil {
		t.Errorf("without the gateway: gateway = %v", got)
	}

	if err := svc.AbandonCluster(ctx, "p1", spec.ID, "svc-pve"); err != nil {
		t.Fatal(err)
	}
	if _, ok := api.secrets[key]; ok {
		t.Fatal("abandon left the gateway secret")
	}
}

// A cluster made with the tenant-side controller moves onto the gateway in one Application apply: the chart pin and the
// gateway values change, nothing else in the values does, and the resource set loses the token-carrying config.
func TestSwitchCSIGateway(t *testing.T) {
	ctx := context.Background()
	api := newFakeAPI()
	cfg := testCfg()
	cfg.ChartVersion = "0.1.6"
	svc := NewWithAPI(api, cfg, "svc-1")
	spec := testSpec()
	spec.Infra.StorageClasses = []StorageClass{{Name: "std", Storage: "local-zfs"}, {Name: "fast", Storage: "nas"}}
	if _, err := svc.CreateCluster(ctx, spec, ClusterSecrets{Cloud: map[string]string{"token": "t"}, CSIManifest: "old token_secret"}); err != nil {
		t.Fatal(err)
	}
	before, err := svc.ClusterCSI(ctx, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Gateway || before.ProjectID != "p1" || before.ChartVersion != "0.1.6" || len(before.Classes) != 2 || before.Classes[1].Storage != "nas" {
		t.Fatalf("before = %+v", before)
	}
	if err := svc.SwitchCSIGateway(ctx, "p1", spec.ID, "0.1.6", map[string]string{"config.yaml": "c"}, "ns only"); err == nil {
		t.Fatal("a chart without the services-side controller is refused")
	}
	appKey := cfg.ArgoNamespace + "/" + spec.ID
	oldValues, _ := json.Marshal(dig(api.apps[appKey], "spec", "source", "helm", "valuesObject"))
	if err := svc.SwitchCSIGateway(ctx, "p1", spec.ID, "0.1.7", map[string]string{"config.yaml": "c", "ca.crt": "CA"}, "ns only"); err != nil {
		t.Fatal(err)
	}
	after, err := svc.ClusterCSI(ctx, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Gateway || after.ChartVersion != "0.1.7" || len(after.Classes) != 2 {
		t.Fatalf("after = %+v", after)
	}
	if got := api.secrets["pc-p1/"+CSIGatewaySecretName(spec.ID)]; got["ca.crt"] != "CA" {
		t.Fatalf("controller secret = %v", got)
	}
	if got := api.secrets["pc-p1/"+CSISecretName(spec.ID)]; got["csi.yaml"] != "ns only" {
		t.Fatalf("resource set = %v", got)
	}
	values := dig(api.apps[appKey], "spec", "source", "helm", "valuesObject").(map[string]any)
	csi := dig(values, "proxmox", "csi").(map[string]any)
	if dig(csi, "gateway", "kubeconfigSecretName") != AdminKubeconfigSecretName(spec.ID) {
		t.Fatalf("gateway values = %v", csi["gateway"])
	}
	delete(csi, "gateway")
	if newValues, _ := json.Marshal(values); string(newValues) != string(oldValues) {
		t.Fatalf("other values changed:\n%s\n%s", oldValues, newValues)
	}
	if dig(api.apps[appKey], "spec", "destination") == nil {
		t.Fatal("the re-apply dropped the destination")
	}
	if _, err := svc.ClusterCSI(ctx, "pc-c-nothere"); err == nil {
		t.Fatal("an unknown cluster is an error")
	}
}

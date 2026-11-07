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

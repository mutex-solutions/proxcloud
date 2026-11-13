package sandbox

import (
	"context"
	"errors"
	"testing"
)

const clusterOnlyKubeconfig = `apiVersion: v1
kind: Config
current-context: c
clusters:
- name: k
  cluster: {server: "https://192.0.2.10:6443"}
contexts:
- name: c
  context: {cluster: k, user: u}
users:
- name: u
  user: {token: t}
`

// N4: the template mirror's cleanup needs the cluster only. A provider that lost its mirror account, its template
// registry and its catalog still gets a Service (NewClusterOnly: the kubeconfig only), and that Service lists and
// deletes the job's worker pod (its credential Secrets are owned by the pod: the garbage collector removes them with
// it) and rolls a partial prepull switch back.
func TestMirrorCleanupNeedsOnlyTheCluster(t *testing.T) {
	ctx := context.Background()
	full, f := mirrorFixture()
	req := MirrorRequest{JobShort: "0123456789abcdef", Epoch: 1, Source: "ghcr.io/mutex-solutions/sandbox-base@" + dNew,
		Destination: mirrorReg + "/library/sandbox-base:20261112-9d00563", SourceToken: "ghcr-registry-token"}
	uid, err := full.StartMirror(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	old := mirrorReg + "/library/sandbox-base:20261101-aaaaaaa@" + dOld
	ref := mirrorReg + "/library/sandbox-base:20261112-9d00563@" + dNew
	if _, _, err := full.SetPrepullImage(ctx, ref); err != nil {
		t.Fatal(err)
	}
	cfg := full.cfg
	cfg.RegistryMirror, cfg.TemplateRegistry, cfg.Templates = RegistryCredential{}, "", nil
	s := NewWithAPI(f, cfg, "svc-sbx")
	pods, err := s.ListMirrorPods(ctx, req.JobShort)
	if err != nil || len(pods) != 1 || pods[0].UID != uid {
		t.Fatalf("list %+v %v", pods, err)
	}
	if err := s.DeleteMirrorPod(ctx, pods[0].Name, pods[0].UID); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.ListMirrorPods(ctx, req.JobShort); len(left) != 0 {
		t.Fatalf("worker pod left: %+v", left)
	}
	for _, which := range []string{"source", "push"} {
		owners, _ := dig(f.core["secrets/"+BuildNamespace+"/"+pods[0].Name+"-"+which], "metadata", "ownerReferences").([]any)
		if len(owners) != 1 || owners[0].(map[string]any)["uid"] != uid {
			t.Fatalf("the %s Secret does not go with the deleted pod: %v", which, owners)
		}
	}
	if n, err := s.RestorePrepullImages(ctx, ref, []PrepullImage{{DaemonSet: PrepullDaemonSet, Container: "p1", Image: old}}); err != nil || n != 1 {
		t.Fatalf("restore %d %v", n, err)
	}
	if cur, err := s.PrepullImages(ctx, mirrorReg+"/library/sandbox-base"); err != nil || len(cur) != 1 || cur[0].Image != old {
		t.Fatalf("prepull %+v %v", cur, err)
	}
	// NewClusterOnly needs a usable kubeconfig and nothing else; such a Service cannot mirror.
	for name, c := range map[string]Config{"none": {}, "unparseable": {Kubeconfig: "current-context: [x"}} {
		if _, err := NewClusterOnly(c, "svc-sbx"); !errors.Is(err, ErrNoClusterAccess) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := New(Config{Kubeconfig: clusterOnlyKubeconfig}, "svc-sbx"); err == nil {
		t.Fatal("New accepted a provider without templates")
	}
	cs, err := NewClusterOnly(Config{Kubeconfig: clusterOnlyKubeconfig}, "svc-sbx")
	if err != nil || cs.ServiceID() != "svc-sbx" {
		t.Fatalf("cluster-only: %v", err)
	}
	if _, err := cs.MirrorPolicy(); !errors.Is(err, ErrMirrorUnavailable) {
		t.Fatalf("a cluster-only service passes the mirror prerequisites: %v", err)
	}
}

package kamaji

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The controller's Secret is written next to the cloud secret and named in the values, never put in them; the values
// carry the image, the gateway and the mode; the Secret goes with the cluster's other secrets.
func TestLBControllerSecretAndValues(t *testing.T) {
	ctx := context.Background()
	api := newFakeAPI()
	svc := NewWithAPI(api, testCfg(), "svc-1")
	spec := testSpec()
	spec.Infra.LoadBalancer = &LBController{Image: "ghcr.io/mutex-solutions/proxcloud-api:v0.11.0", GatewayURL: "https://192.0.2.7:8444", Default: true}
	sec := ClusterSecrets{Cloud: map[string]string{"token": "t"}, LBGateway: map[string]string{"token": "lb@proxcloud!pc-c-abcd1234=s3cr3t", "ca.crt": "CA"}}
	if _, err := svc.CreateCluster(ctx, spec, sec); err != nil {
		t.Fatal(err)
	}
	key := "pc-p1/" + LBGatewaySecretName(spec.ID)
	if got := api.secrets[key]; got["token"] != "lb@proxcloud!pc-c-abcd1234=s3cr3t" || got["ca.crt"] != "CA" {
		t.Fatalf("controller secret = %v", got)
	}
	v := BuildValues(testCfg(), spec)
	want := map[string]any{"secretName": "pc-c-abcd1234-lb-gateway", "kubeconfigSecretName": "pc-c-abcd1234-kamaji-cp-admin-kubeconfig",
		"image": "ghcr.io/mutex-solutions/proxcloud-api:v0.11.0", "gatewayURL": "https://192.0.2.7:8444", "default": true}
	if got, _ := json.Marshal(v["loadBalancer"]); string(got) != mustJSON(want) {
		t.Fatalf("loadBalancer = %s", got)
	}
	if raw, _ := json.Marshal(v); strings.Contains(string(raw), "s3cr3t") {
		t.Fatalf("values carry the credential: %s", raw)
	}
	spec.Infra.LoadBalancer = nil
	if _, ok := BuildValues(testCfg(), spec)["loadBalancer"]; ok {
		t.Fatal("without a controller the values carry a loadBalancer block: a 0.1.16 render must not move")
	}
	if err := svc.AbandonCluster(ctx, "p1", spec.ID, "svc-pve"); err != nil {
		t.Fatal(err)
	}
	if _, ok := api.secrets[key]; ok {
		t.Fatal("abandon left the controller secret")
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// "Enable load balancer controller" on an existing cluster: one apply, class-only, nothing else in the values moves;
// again (a rotation) keeps the mode a cluster has, default included; a chart without the controller is refused.
func TestEnableLBController(t *testing.T) {
	ctx := context.Background()
	api := newFakeAPI()
	cfg := testCfg()
	cfg.ChartVersion = "0.1.16"
	svc := NewWithAPI(api, cfg, "svc-1")
	spec := testSpec()
	if _, err := svc.CreateCluster(ctx, spec, ClusterSecrets{Cloud: map[string]string{"token": "t"}}); err != nil {
		t.Fatal(err)
	}
	appKey := cfg.ArgoNamespace + "/" + spec.ID
	values := func() map[string]any {
		return dig(api.apps[appKey], "spec", "source", "helm", "valuesObject").(map[string]any)
	}
	before := mustJSON(values())
	lb := LBController{Image: "img:v1", GatewayURL: "https://192.0.2.7:8444", Default: true}
	if err := svc.EnableLBController(ctx, "p1", spec.ID, "0.1.16", lb, map[string]string{"token": "a"}); err == nil {
		t.Fatal("a chart without the controller is refused")
	}
	if _, ok := api.secrets["pc-p1/"+LBGatewaySecretName(spec.ID)]; ok {
		t.Fatal("a refused enable wrote the secret")
	}
	if err := svc.EnableLBController(ctx, "p2", spec.ID, "0.1.17", lb, map[string]string{"token": "a"}); err == nil {
		t.Fatal("another project's cluster is not found")
	}
	lb.Default = false // what the operator's action passes
	if err := svc.EnableLBController(ctx, "p1", spec.ID, "0.1.17", lb, map[string]string{"token": "a", "ca.crt": "CA"}); err != nil {
		t.Fatal(err)
	}
	if got := api.secrets["pc-p1/"+LBGatewaySecretName(spec.ID)]; got["token"] != "a" || got["ca.crt"] != "CA" {
		t.Fatalf("secret = %v", got)
	}
	if rev := digStr(api.apps[appKey], "spec", "source", "targetRevision"); rev != "0.1.17" {
		t.Fatalf("targetRevision = %s", rev)
	}
	v := values()
	if got := mustJSON(v["loadBalancer"]); got != mustJSON(LBValues(spec.ID, LBController{Image: "img:v1", GatewayURL: "https://192.0.2.7:8444"})) {
		t.Fatalf("loadBalancer = %s", got)
	}
	delete(v, "loadBalancer")
	if after := mustJSON(v); after != before {
		t.Fatalf("other values changed:\n%s\n%s", before, after)
	}

	// A cluster made in default mode keeps it through a rotation: MetalLB's class never changes on a cluster.
	values()["loadBalancer"] = LBValues(spec.ID, LBController{Image: "img:v1", GatewayURL: "https://192.0.2.7:8444", Default: true})
	if err := svc.EnableLBController(ctx, "p1", spec.ID, "0.1.17", LBController{Image: "img:v2", GatewayURL: "https://192.0.2.7:8444"}, map[string]string{"token": "b"}); err != nil {
		t.Fatal(err)
	}
	if lbv := values()["loadBalancer"].(map[string]any); lbv["default"] != true || lbv["image"] != "img:v2" {
		t.Fatalf("rotation: %v", lbv)
	}
	if api.secrets["pc-p1/"+LBGatewaySecretName(spec.ID)]["token"] != "b" {
		t.Fatal("rotation kept the old token")
	}
}

// A chart bump re-pins a cluster's controller image to the portal's, and only then (no other path rewrites it); a
// cluster with a controller is never moved below the chart that renders it.
func TestSetChartVersionAndTheController(t *testing.T) {
	ctx := context.Background()
	api := newFakeAPI()
	cfg := testCfg()
	cfg.ChartVersion = "0.1.17"
	cfg.LBControllerImage = "img:v2"
	svc := NewWithAPI(api, cfg, "svc-1")
	with, without := testSpec(), testSpec()
	with.Infra.LoadBalancer = &LBController{Image: "img:v1", GatewayURL: "https://192.0.2.7:8444"}
	without.ID = "pc-c-other"
	for _, s := range []ClusterSpec{with, without} {
		if _, err := svc.CreateCluster(ctx, s, ClusterSecrets{Cloud: map[string]string{"token": "t"}}); err != nil {
			t.Fatal(err)
		}
	}
	lbOf := func(id string) any {
		return dig(api.apps[cfg.ArgoNamespace+"/"+id], "spec", "source", "helm", "valuesObject", "loadBalancer")
	}
	if err := svc.SetChartVersion(ctx, with.ID, "0.1.16"); err == nil || !strings.Contains(err.Error(), "load balancer controller") {
		t.Fatalf("a move below 0.1.17: %v", err)
	}
	if err := svc.SetChartVersion(ctx, with.ID, "0.1.18"); err != nil {
		t.Fatal(err)
	}
	if got := lbOf(with.ID).(map[string]any)["image"]; got != "img:v2" {
		t.Fatalf("image after a bump = %v", got)
	}
	if err := svc.SetChartVersion(ctx, without.ID, "0.1.16"); err != nil {
		t.Fatal(err)
	}
	if got := lbOf(without.ID); got != nil {
		t.Fatalf("a cluster without a controller got a block: %v", got)
	}
}

// Default mode gives MetalLB a class and never NGINX Gateway Fabric's Service: the class of a Service cannot change,
// so a class on NGF's would fail the Helm upgrade of every Gateway of a cluster (C4). Without a Secret no controller and
// no class, whatever default says.
func TestChartLBClasses(t *testing.T) {
	out := helmTemplate(t, "pc-c-abcd1234", "pc-p1", lbGoldenValues(true)+"addons:\n  nginxGatewayFabric: {enabled: true}\n")
	var metallb, ngf string
	for _, doc := range strings.Split(out, "\n---\n") {
		for _, line := range strings.Split(doc, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "valuesTemplate: "); ok {
				switch {
				case strings.Contains(doc, "name: pc-c-abcd1234-metallb"):
					metallb = v
				case strings.Contains(doc, "name: pc-c-abcd1234-nginxgatewayfabric"):
					ngf = v
				}
			}
		}
	}
	if !strings.Contains(metallb, `loadBalancerClass\":\"proxcloud.io/internal`) {
		t.Fatalf("metallb values = %s", metallb)
	}
	if ngf == "" || strings.Contains(ngf, "loadBalancerClass") {
		t.Fatalf("nginx gateway fabric values = %q", ngf)
	}
	off := helmTemplate(t, "pc-c-abcd1234", "pc-p1", "loadBalancer: {default: true}\n")
	if strings.Contains(off, "proxcloud.io/internal") || strings.Contains(off, "lb-controller") {
		t.Fatal("default without the controller's Secret gave MetalLB a class or rendered a controller")
	}
}

// The consoles read a cluster's mode back off its values: the tenant's cluster row (clusterData) and the operator's
// list of clusters (ListClusterPins) say default, class-only, or nothing without a controller.
func TestLBModeReadBack(t *testing.T) {
	for want, values := range map[string]map[string]any{
		"default":    {"loadBalancer": LBValues("c", LBController{Default: true})},
		"class-only": {"loadBalancer": LBValues("c", LBController{})},
		"":           {"loadBalancer": map[string]any{"default": true}},
	} {
		if got := LBMode(values); got != want {
			t.Errorf("LBMode(%v) = %q, want %q", values, got, want)
		}
	}
	ctx := context.Background()
	api := newFakeAPI()
	svc := NewWithAPI(api, testCfg(), "svc-1")
	spec := testSpec()
	spec.Infra.LoadBalancer = &LBController{Image: "img:v1", GatewayURL: "https://192.0.2.7:8444", Default: true}
	if _, err := svc.CreateCluster(ctx, spec, ClusterSecrets{Cloud: map[string]string{"token": "t"}}); err != nil {
		t.Fatal(err)
	}
	pins, err := svc.ListClusterPins(ctx)
	if err != nil || len(pins) != 1 || pins[0].LoadBalancer != "default" {
		t.Fatalf("pins = %+v, %v", pins, err)
	}
	app := api.apps[testCfg().ArgoNamespace+"/"+spec.ID]
	if c := clusterData(app, nil, nil)["cluster"].(map[string]any); c["load_balancer"] != "default" {
		t.Fatalf("cluster data load_balancer = %v", c["load_balancer"])
	}
	delete(dig(app, "spec", "source", "helm", "valuesObject").(map[string]any), "loadBalancer")
	if c := clusterData(app, nil, nil)["cluster"].(map[string]any); c["load_balancer"] != nil {
		t.Fatalf("a cluster without a controller reads %v", c["load_balancer"])
	}
}

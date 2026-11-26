package kamaji

import (
	"context"
	"encoding/json"
	"reflect"
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

// A release that changes only the load balancer controller reaches a cluster that is already on the provider's chart:
// the cluster reads as behind (what the tenant's page and the operator's "Bump all" go by), a bump changes its
// controller's image and nothing else of its Application, and a cluster that would not change is neither offered an
// update nor applied. What is compared is what the Application carries; no cluster is asked. Lab drill of
// v0.11.1-rc.3, step F: the controller stayed on the earlier candidate because the chart had not moved.
func TestLBControllerUpdateOffer(t *testing.T) {
	ctx := context.Background()
	api := newFakeAPI()
	cfg := testCfg()
	cfg.ChartVersion = "0.1.19"
	cfg.LBControllerImage = "img:v2"
	svc := NewWithAPI(api, cfg, "svc-1")
	app := func(id string) map[string]any { return api.apps[cfg.ArgoNamespace+"/"+id] }
	for id, image := range map[string]string{"pc-c-behind": "img:v1", "pc-c-current": "img:v2", "pc-c-plain": "", "pc-c-oldchart": "img:v1"} {
		spec := testSpec()
		spec.ID = id
		if image != "" {
			spec.Infra.LoadBalancer = &LBController{Image: image, GatewayURL: "https://192.0.2.7:8444", Default: true}
		}
		if _, err := svc.CreateCluster(ctx, spec, ClusterSecrets{Cloud: map[string]string{"token": "t"}}); err != nil {
			t.Fatal(err)
		}
	}
	dig(app("pc-c-oldchart"), "spec", "source").(map[string]any)["targetRevision"] = "0.1.18"

	// read is what each surface says of each cluster: the operator's row, and the tenant's cluster data.
	read := func(svc *Service) (behindPin, imageBehind, offered map[string]bool) {
		t.Helper()
		behindPin, imageBehind, offered = map[string]bool{}, map[string]bool{}, map[string]bool{}
		pins, err := svc.ListClusterPins(ctx)
		if err != nil || len(pins) != 4 {
			t.Fatalf("pins = %+v, %v", pins, err)
		}
		for _, p := range pins {
			behindPin[p.ID], imageBehind[p.ID] = p.Behind(cfg.ChartVersion), p.LBControllerBehind
		}
		rows, err := svc.SyncProvider("az1", "p1").List(ctx)
		if err != nil || len(rows) != 4 {
			t.Fatalf("rows = %+v, %v", rows, err)
		}
		for _, r := range rows {
			c := r.Data["cluster"].(map[string]any)
			if v, said := c["lb_controller_update"]; said {
				offered[r.ExternalID] = v.(bool)
			}
		}
		return
	}
	want := func(what string, got, want map[string]bool) {
		t.Helper()
		for id, w := range want {
			if got[id] != w {
				t.Errorf("%s: %s = %v, want %v (%v)", what, id, got[id], w, got)
			}
		}
	}
	behindPin, imageBehind, offered := read(svc)
	want("behind the pin", behindPin, map[string]bool{"pc-c-behind": true, "pc-c-current": false, "pc-c-plain": false, "pc-c-oldchart": true})
	want("controller image behind", imageBehind, map[string]bool{"pc-c-behind": true, "pc-c-current": false, "pc-c-plain": false, "pc-c-oldchart": true})
	// The tenant's data says it only where it is true: the row of a cluster that needs nothing is the row it had.
	if len(offered) != 2 || !offered["pc-c-behind"] || !offered["pc-c-oldchart"] {
		t.Errorf("lb_controller_update on %v, want pc-c-behind and pc-c-oldchart alone", offered)
	}
	// A portal without a controller image would write none: no cluster is behind by it.
	noImage := cfg
	noImage.LBControllerImage = ""
	behindPin, imageBehind, offered = read(NewWithAPI(api, noImage, "svc-1"))
	want("no portal image: behind the pin", behindPin, map[string]bool{"pc-c-behind": false, "pc-c-oldchart": true})
	if len(offered) != 0 || imageBehind["pc-c-behind"] || imageBehind["pc-c-oldchart"] {
		t.Errorf("no portal image: image behind %v, offered %v", imageBehind, offered)
	}

	// The bump of every cluster that is behind, as the operator's "Bump all" does it (the tenant's action is the same
	// call for one cluster). Applies are counted by the object: an apply puts a new one in the fake.
	before := map[string]string{}
	objs := map[string]map[string]any{}
	for _, id := range []string{"pc-c-behind", "pc-c-current", "pc-c-plain", "pc-c-oldchart"} {
		before[id], objs[id] = mustJSON(app(id)), app(id)
	}
	pins, _ := svc.ListClusterPins(ctx)
	for _, p := range pins {
		if p.Behind(cfg.ChartVersion) {
			if err := svc.SetChartVersion(ctx, p.ID, cfg.ChartVersion); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, id := range []string{"pc-c-current", "pc-c-plain"} {
		if mustJSON(app(id)) != before[id] || reflect.ValueOf(app(id)).Pointer() != reflect.ValueOf(objs[id]).Pointer() {
			t.Errorf("%s needed nothing and was applied", id)
		}
	}
	// pc-c-behind: its controller's image, and not one other byte of its Application (chart, node groups, versions).
	if got := strings.ReplaceAll(mustJSON(app("pc-c-behind")), `"image":"img:v2"`, `"image":"img:v1"`); got != before["pc-c-behind"] {
		t.Errorf("the bump of a cluster on the pin changed more than the controller's image:\nbefore %s\nafter  %s", before["pc-c-behind"], mustJSON(app("pc-c-behind")))
	}
	if digStr(app("pc-c-behind"), "spec", "source", "helm", "valuesObject", "loadBalancer", "image") != "img:v2" ||
		digStr(app("pc-c-oldchart"), "spec", "source", "targetRevision") != "0.1.19" ||
		digStr(app("pc-c-oldchart"), "spec", "source", "helm", "valuesObject", "loadBalancer", "image") != "img:v2" {
		t.Errorf("after the bump: %s\n%s", mustJSON(app("pc-c-behind")), mustJSON(app("pc-c-oldchart")))
	}
	behindPin, _, offered = read(svc)
	if len(offered) != 0 || behindPin["pc-c-behind"] || behindPin["pc-c-oldchart"] {
		t.Errorf("after the bump: behind %v, offered %v", behindPin, offered)
	}
}

// What that bump rolls: the chart rendered with one controller image and with another differs in one line, the image
// of the controller's Deployment (strategy Recreate: its one pod is replaced). No machine template, no control plane
// and no add-on moves, so no machine is replaced.
func TestChartControllerImageMovesTheControllerAlone(t *testing.T) {
	values := lbGoldenValues(true) + "addons:\n  nginxGatewayFabric: {enabled: true}\n"
	was := strings.Split(helmTemplate(t, "pc-c-abcd1234", "pc-p1", values), "\n")
	now := strings.Split(helmTemplate(t, "pc-c-abcd1234", "pc-p1", strings.ReplaceAll(values, "proxcloud-api:v0.11.0", "proxcloud-api:v0.11.1")), "\n")
	if len(was) != len(now) {
		t.Fatalf("the render has %d lines with one image and %d with the other", len(was), len(now))
	}
	var moved []int
	for i := range was {
		if was[i] != now[i] {
			moved = append(moved, i)
		}
	}
	if len(moved) != 1 || strings.TrimSpace(now[moved[0]]) != `image: "ghcr.io/mutex-solutions/proxcloud-api:v0.11.1"` {
		t.Fatalf("lines that moved: %v", moved)
	}
	// The line is the controller Deployment's: the last "kind:" and "name:" above it.
	kind, name := "", ""
	for i := moved[0]; i >= 0 && (kind == "" || name == ""); i-- {
		if line := strings.TrimSpace(was[i]); kind == "" && strings.HasPrefix(was[i], "kind: ") {
			kind = line
		} else if name == "" && strings.HasPrefix(was[i], "  name: ") {
			name = line
		}
	}
	if kind != "kind: Deployment" || name != "name: pc-c-abcd1234-lb-controller" {
		t.Fatalf("the image line belongs to %q %q", kind, name)
	}
}

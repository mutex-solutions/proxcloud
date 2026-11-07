package kamaji

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testCfg() Config {
	return Config{
		Kubeconfig:    "kc",
		Region:        "az1",
		ArgoNamespace: "argocd",
		ArgoProject:   "proxcloud-k8s",
		ChartRepo:     "ghcr.io/mutex-solutions/charts",
		ChartName:     "kamaji-cluster",
		ChartVersion:  "0.2.3",
		Defaults: ClusterDefaults{
			DataStoreName: "default",
			DNSZone:       "k8s.example.com",
			Versions:      map[string]string{"1.35.4": "img-1354", "1.34.2": "img-1342"},
			RootVolumeGiB: 120,
		},
	}
}

func testSpec() ClusterSpec {
	return ClusterSpec{
		ID: "stc-abcd1234", DisplayName: "prod cluster", ProjectID: "p1", Version: "1.35.4", HA: true,
		OIDC:         map[string]string{"issuerUrl": "https://idp.example.com", "clientId": "kube"},
		AllowedCIDRs: []string{"10.0.0.0/8", "1.2.3.4/32"},
		NodeGroups: []NodeGroup{
			{Name: "workers", FlavorID: "m5.large", Count: 3,
				Labels: map[string]string{"tier": "app"}, Taints: []string{"gpu=true:NoSchedule"}},
			{Name: "burst", FlavorID: "m5.xlarge", Count: 1, RootVolumeGiB: 200},
		},
	}
}

func TestSpecValidate(t *testing.T) {
	d := testCfg().Defaults
	if err := testSpec().Validate(d); err != nil {
		t.Fatalf("valid spec: %v", err)
	}
	bad := testSpec()
	bad.Version = "1.99.0"
	if err := bad.Validate(d); err == nil || !strings.Contains(err.Error(), "not offered") {
		t.Errorf("uncurated version: %v", err)
	}
	bad = testSpec()
	bad.NodeGroups = nil
	if err := bad.Validate(d); err == nil {
		t.Error("no node groups: want error")
	}
	bad = testSpec()
	bad.NodeGroups[1].Autoscale = true
	if err := bad.Validate(d); err == nil || !strings.Contains(err.Error(), "autoscaling") {
		t.Errorf("autoscale: want a refusal, got %v", err)
	}
	// Flavor allowlist: when configured, only listed flavors pass.
	allow := d
	allow.Flavors = []string{"m5.large"}
	if err := testSpec().Validate(allow); err == nil || !strings.Contains(err.Error(), "not offered") {
		t.Errorf("allowlist: m5.xlarge must be refused: %v", err)
	}
	allow.Flavors = []string{"m5.large", "m5.xlarge"}
	if err := testSpec().Validate(allow); err != nil {
		t.Errorf("allowlist covering both flavors: %v", err)
	}
	// The chart rejects a node-group name it cannot turn into an object name, with a raw template
	// failure — catch it here where the message can say something useful.
	bad = testSpec()
	bad.NodeGroups[0].Name = "Workers_1"
	if err := bad.Validate(d); err == nil {
		t.Error("invalid node group name: want error")
	}
	bad = testSpec()
	bad.NodeGroups[1].Name = "workers"
	if err := bad.Validate(d); err == nil {
		t.Error("duplicate node group name: want error")
	}
	bad = testSpec()
	bad.NodeGroups[0].Taints = []string{"gpu=true"} // no effect
	if err := bad.Validate(d); err == nil {
		t.Error("taint without an effect: want error")
	}
	// Every node gets a sized root disk; with no size anywhere the chart would build a machine the
	// hypervisor refuses.
	noDisk := d
	noDisk.RootVolumeGiB = 0
	if err := testSpec().Validate(noDisk); err == nil {
		t.Error("no root volume size anywhere: want error")
	}
}

func TestNames(t *testing.T) {
	// The chart names the KamajiControlPlane <release>-kamaji-cp and the Kamaji CAPI provider gives
	// the TenantControlPlane the same name; Kamaji derives the kubeconfig secret from that. Getting
	// this wrong is invisible until a kubeconfig fetch, so pin it.
	if got := ControlPlaneName("stc-abcd1234"); got != "stc-abcd1234-kamaji-cp" {
		t.Errorf("ControlPlaneName = %s", got)
	}
	if got := AdminKubeconfigSecretName("stc-abcd1234"); got != "stc-abcd1234-kamaji-cp-admin-kubeconfig" {
		t.Errorf("AdminKubeconfigSecretName = %s", got)
	}
}

func TestBuildValues(t *testing.T) {
	cfg := testCfg()
	v := BuildValues(cfg, testSpec())

	if v["kubernetesVersion"] != "1.35.4" {
		t.Errorf("kubernetesVersion = %v", v["kubernetesVersion"])
	}
	cp := v["kamajiControlPlane"].(map[string]any)
	if cp["replicas"] != 3 {
		t.Errorf("HA replicas = %v", cp["replicas"])
	}
	net := cp["network"].(map[string]any)
	if src := net["loadBalancerSourceRanges"].([]any); len(src) != 2 || src[0] != "10.0.0.0/8" || src[1] != "1.2.3.4/32" {
		t.Errorf("loadBalancerSourceRanges = %v", src)
	}
	ann := net["serviceAnnotations"].(map[string]any)
	if ann["external-dns.alpha.kubernetes.io/hostname"] != "stc-abcd1234.k8s.example.com" {
		t.Errorf("hostname = %v", ann["external-dns.alpha.kubernetes.io/hostname"])
	}
	oidc := v["oidc"].(map[string]any)
	if oidc["issuerUrl"] != "https://idp.example.com" || oidc["clientId"] != "kube" {
		t.Errorf("oidc = %v", oidc)
	}
	groups := v["nodeGroups"].([]any)
	if len(groups) != 2 {
		t.Fatalf("nodeGroups = %d", len(groups))
	}

	// The chart `fail`s the render on a missing/misspelled node-group key rather than defaulting, so
	// these names ARE the contract with the cluster chart; if the chart moves a key, it moves here
	// and nowhere else.
	g0 := groups[0].(map[string]any)
	for key, want := range map[string]any{
		"name":           "workers",
		"machineFlavor":  "m5.large",
		"machineImageId": "img-1354", // resolved from the version matrix
		"machineCount":   3,
	} {
		if g0[key] != want {
			t.Errorf("node group key %q = %v, want %v", key, g0[key], want)
		}
	}
	if rv := g0["machineRootVolume"].(map[string]any); rv["diskSize"] != 120 { // provider default
		t.Errorf("machineRootVolume = %v", rv)
	}
	// Taints reach the chart as objects, not the "k=v:Effect" string the API speaks.
	taints := g0["taints"].([]any)
	if len(taints) != 1 {
		t.Fatalf("taints = %v", taints)
	}
	tt := taints[0].(map[string]any)
	if tt["key"] != "gpu" || tt["value"] != "true" || tt["effect"] != "NoSchedule" {
		t.Errorf("taint = %v", tt)
	}

	g1 := groups[1].(map[string]any)
	if rv := g1["machineRootVolume"].(map[string]any); rv["diskSize"] != 200 { // per-group override
		t.Errorf("per-group machineRootVolume = %v", rv)
	}
	// The addons block is the CHART's business — proxcloud must not send one without customer
	// picks, and in particular must never push the cloud credential into the workload cluster.
	if _, has := v["addons"]; has {
		t.Error("BuildValues must not generate an addons block")
	}

	// Round-trip: what the sync reads back out of the Application must match what went in.
	back := NodeGroupsFromValues(v)
	if len(back) != 2 || back[0]["flavor_id"] != "m5.large" || back[0]["count"] != 3 {
		t.Fatalf("round-trip = %v", back)
	}
	if got := back[0]["taints"].([]any); len(got) != 1 || got[0] != "gpu=true:NoSchedule" {
		t.Errorf("round-trip taints = %v", got)
	}
	if labels := back[0]["labels"].(map[string]any); labels["tier"] != "app" {
		t.Errorf("round-trip labels = %v", labels)
	}

	// No OIDC issuer → no oidc block at all (chart default = disabled).
	spec := testSpec()
	spec.OIDC = nil
	spec.HA = false
	v = BuildValues(cfg, spec)
	if _, has := v["oidc"]; has {
		t.Error("oidc block must be absent when no issuer")
	}
	if v["kamajiControlPlane"].(map[string]any)["replicas"] != 1 {
		t.Error("non-HA replicas != 1")
	}
}

// TestImageVariants covers the curated variant matrix: resolution, values stamping, the sync
// round-trip, and spec validation — a GPU pool picks "nvidia" by name and upgrades stay on it.
func TestImageVariants(t *testing.T) {
	d := ClusterDefaults{
		RootVolumeGiB: 120,
		Versions:      map[string]string{"1.35.4": "img-plain-1354", "1.34.2": "img-plain-1342"},
		ImageVariants: map[string]map[string]string{"nvidia": {"1.35.4": "img-nv-1354"}},
	}
	if got := d.ImageFor("1.35.4", ""); got != "img-plain-1354" {
		t.Errorf("default image = %q", got)
	}
	if got := d.ImageFor("1.35.4", "nvidia"); got != "img-nv-1354" {
		t.Errorf("variant image = %q", got)
	}
	if got := d.ImageFor("1.34.2", "nvidia"); got != "" {
		t.Errorf("missing variant image = %q, want empty", got)
	}
	if got := d.VariantsForVersion("1.35.4"); len(got) != 1 || got[0] != "nvidia" {
		t.Errorf("VariantsForVersion(1.35.4) = %v", got)
	}
	if got := d.VariantsForVersion("1.34.2"); len(got) != 0 {
		t.Errorf("VariantsForVersion(1.34.2) = %v", got)
	}

	groups := NodeGroupValues(d, "1.35.4", []NodeGroup{
		{Name: "gpu", FlavorID: "g1", ImageVariant: "nvidia", Count: 1},
		{Name: "cpu", FlavorID: "m1", Count: 1},
	})
	g0 := groups[0].(map[string]any)
	if g0["machineImageId"] != "img-nv-1354" || g0["imageVariant"] != "nvidia" {
		t.Errorf("gpu group = %v", g0)
	}
	g1 := groups[1].(map[string]any)
	if g1["machineImageId"] != "img-plain-1354" {
		t.Errorf("cpu group image = %v", g1["machineImageId"])
	}
	if _, has := g1["imageVariant"]; has {
		t.Error("default group must not carry an imageVariant key")
	}
	// The variant survives the values→sync round-trip, so the UI can show and re-submit it.
	back := NodeGroupsFromValues(map[string]any{"nodeGroups": groups})
	if back[0]["image_variant"] != "nvidia" {
		t.Errorf("round-trip variant = %v", back[0]["image_variant"])
	}
	if _, has := back[1]["image_variant"]; has {
		t.Error("default group round-trip must not carry image_variant")
	}

	// Validation: a variant must resolve for the cluster's version…
	spec := testSpec()
	spec.NodeGroups[0].ImageVariant = "nvidia"
	if err := spec.Validate(d); err != nil {
		t.Errorf("offered variant: %v", err)
	}
	spec.Version = "1.34.2"
	if err := spec.Validate(d); err == nil || !strings.Contains(err.Error(), "variant") {
		t.Errorf("unoffered variant: err = %v", err)
	}
	// …but the SET_NODE_GROUPS shape check (empty defaults) leaves image resolution to the
	// values patch, which has the fallback-to-current-image logic.
	spec.NodeGroups[0].RootVolumeGiB = 100
	spec.NodeGroups[1].RootVolumeGiB = 100
	if err := spec.Validate(ClusterDefaults{}); err != nil {
		t.Errorf("shape check must skip the variant rule: %v", err)
	}
}

// TestClusterAddons: curated toggles render as addons.<name>.enabled, unknown names are
// rejected, and no picks means no addons block at all (the chart's defaults rule).
func TestClusterAddons(t *testing.T) {
	spec := testSpec()
	spec.Addons = map[string]bool{"certManager": true, "metricsServer": false}
	v := BuildValues(testCfg(), spec)
	addons := v["addons"].(map[string]any)
	if addons["certManager"].(map[string]any)["enabled"] != true ||
		addons["metricsServer"].(map[string]any)["enabled"] != false {
		t.Errorf("addons = %v", addons)
	}
	// An add-on outside the allowlist is rejected.
	if err := (func() error {
		s := testSpec()
		s.Addons = map[string]bool{"credentialPush": true}
		return s.Validate(testCfg().Defaults)
	})(); err == nil || !strings.Contains(err.Error(), "add-on") {
		t.Errorf("unknown addon: err = %v", err)
	}
	if err := spec.Validate(testCfg().Defaults); err != nil {
		t.Errorf("valid addons: %v", err)
	}
	plain := BuildValues(testCfg(), testSpec())
	if _, has := plain["addons"]; has {
		t.Error("no picks must render no addons block")
	}

	if av := AddonValues(nil); len(av) != 0 {
		t.Errorf("no picks: %v", av)
	}

	// The menu's defaults are a CONTRACT: they must mirror the chart's effective defaults (and
	// the client wizard's), or a wizard that always sends the full set silently diverges from
	// what an API user gets by omitting the block. Update all three together, deliberately.
	want := map[string]bool{
		"certManager": false, "ingress": false, "metricsServer": true,
	}
	if len(ClusterAddons) != len(want) {
		t.Errorf("ClusterAddons = %v, want %v", ClusterAddons, want)
	}
	for k, v := range want {
		if got, ok := ClusterAddons[k]; !ok || got != v {
			t.Errorf("ClusterAddons[%s] = %v/%v, want %v", k, got, ok, v)
		}
	}
}

// The chart reads these keys; the portal is what fills them.
func TestBuildValuesProxmoxBlock(t *testing.T) {
	cfg := testCfg()
	cfg.Defaults.NodeGroupMTU = 1450
	spec := testSpec()
	spec.Infra = ClusterInfra{
		Region: "r1", Pool: "pc-p1", Storage: "local-zfs", Bridge: "vnet1", Gateway: "192.0.2.1", Prefix: 24,
		Addresses: []string{"192.0.2.200"}, LBAddresses: []string{"192.0.2.190"},
		StorageClasses: []StorageClass{{Name: "std", Storage: "local-zfs"}},
	}
	spec.NodeGroups[0].Cores, spec.NodeGroups[0].MemoryMiB, spec.NodeGroups[0].Tags = 2, 2048, []string{"proxcloud", "fl-small"}
	v := BuildValues(cfg, spec)
	for keys, want := range map[string]any{
		"region": "r1", "pool": "pc-p1", "storage": "local-zfs",
	} {
		if got := dig(v, "proxmox", keys); got != want {
			t.Errorf("proxmox.%s = %v", keys, got)
		}
	}
	if got := dig(v, "proxmox", "network", "bridge"); got != "vnet1" {
		t.Errorf("bridge = %v", got)
	}
	if got := dig(v, "proxmox", "network", "mtu"); got != 1450 {
		t.Errorf("mtu = %v", got)
	}
	if got := dig(v, "proxmox", "network", "addresses").([]any); len(got) != 1 || got[0] != "192.0.2.200" {
		t.Errorf("addresses = %v", got)
	}
	if got := dig(v, "proxmox", "csi", "configSecretName"); got != "stc-abcd1234-csi-config" {
		t.Errorf("csi secret = %v", got)
	}
	if got := dig(v, "proxmox", "metallb", "addresses").([]any); got[0] != "192.0.2.190" {
		t.Errorf("metallb = %v", got)
	}
	g := v["nodeGroups"].([]any)[0].(map[string]any)
	if g["machineCores"] != 2 || g["machineMemoryMiB"] != 2048 {
		t.Errorf("size = %v", g)
	}
	if tags := g["machineTags"].([]any); len(tags) != 2 || tags[1] != "fl-small" {
		t.Errorf("tags = %v", tags)
	}
	// No credential may enter the values: they sit in an Application anyone with argocd read can see.
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "token") {
		t.Errorf("values mention a token: %s", raw)
	}
}

func TestInfraValuesRWX(t *testing.T) {
	if _, ok := infraValues(ClusterDefaults{}, ClusterSpec{})["rwx"]; ok {
		t.Fatal("no dataset: the chart must not get an rwx block")
	}
	v := infraValues(ClusterDefaults{}, ClusterSpec{Infra: ClusterInfra{RWXServer: "192.0.2.10", RWXShare: "/mnt/tank/shares/p-k8s-stc-1"}})
	rwx, _ := v["rwx"].(map[string]any)
	if rwx["server"] != "192.0.2.10" || rwx["share"] != "/mnt/tank/shares/p-k8s-stc-1" {
		t.Fatalf("rwx = %v", v["rwx"])
	}
}

func TestTaintRoundTrip(t *testing.T) {
	for s, want := range map[string]string{
		"gpu=true:NoSchedule":  "gpu=true:NoSchedule",
		"dedicated:NoExecute":  "dedicated:NoExecute",
		"a=b:PreferNoSchedule": "a=b:PreferNoSchedule",
	} {
		obj := taintToObject(s)
		if obj == nil {
			t.Fatalf("taintToObject(%q) = nil", s)
		}
		if got := taintToString(obj); got != want {
			t.Errorf("round trip %q = %q", s, got)
		}
	}
	for _, bad := range []string{"", "noeffect", ":NoSchedule", "key:"} {
		if taintToObject(bad) != nil {
			t.Errorf("taintToObject(%q) must be nil", bad)
		}
	}
}

func TestBuildApplication(t *testing.T) {
	cfg := testCfg()
	spec := testSpec()
	app := BuildApplication(cfg, spec, "svc-1", "", BuildValues(cfg, spec))

	meta := app["metadata"].(map[string]any)
	if meta["name"] != "stc-abcd1234" || meta["namespace"] != "argocd" {
		t.Errorf("metadata = %v", meta)
	}
	labels := meta["labels"].(map[string]any)
	if labels[LabelProject] != "p1" || labels[LabelService] != "svc-1" || labels[LabelManagedBy] != "proxcloud" {
		t.Errorf("labels = %v", labels)
	}
	fins := meta["finalizers"].([]any)
	if len(fins) != 1 || fins[0] != "resources-finalizer.argocd.argoproj.io" {
		t.Errorf("finalizers = %v", fins)
	}
	src := app["spec"].(map[string]any)["source"].(map[string]any)
	if src["targetRevision"] != "0.2.3" { // pinned default when none given
		t.Errorf("targetRevision = %v", src["targetRevision"])
	}
	dst := app["spec"].(map[string]any)["destination"].(map[string]any)
	if dst["namespace"] != "pc-p1" {
		t.Errorf("destination ns = %v", dst["namespace"])
	}
}

type fakeAPI struct {
	namespaces map[string]map[string]string
	secrets    map[string]map[string]string // ns/name → stringData
	secretObjs map[string]map[string]any    // ns/name → full object (metadata for ListSecrets)
	apps       map[string]map[string]any    // ns/name → object
	tcps       map[string]map[string]any
	mds        []map[string]any
	deleted    []string
	annotated  map[string]map[string]string // ns/name → annotations
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		namespaces: map[string]map[string]string{},
		secrets:    map[string]map[string]string{},
		secretObjs: map[string]map[string]any{},
		apps:       map[string]map[string]any{},
		tcps:       map[string]map[string]any{},
	}
}

func (f *fakeAPI) EnsureNamespace(_ context.Context, name string, labels map[string]string) error {
	f.namespaces[name] = labels
	return nil
}
func (f *fakeAPI) GetNamespace(_ context.Context, name string) (map[string]any, error) {
	labels, ok := f.namespaces[name]
	if !ok {
		return nil, nil
	}
	l := map[string]any{}
	for k, v := range labels {
		l[k] = v
	}
	return map[string]any{"metadata": map[string]any{"name": name, "labels": l}}, nil
}
func (f *fakeAPI) DeleteNamespace(_ context.Context, name string) error {
	delete(f.namespaces, name)
	f.deleted = append(f.deleted, "namespace:"+name)
	return nil
}
func (f *fakeAPI) ApplyResourceSetSecret(_ context.Context, ns, name string, sd, labels map[string]string) error {
	return f.ApplySecret(context.Background(), ns, name, sd, labels, nil)
}
func (f *fakeAPI) ApplySecret(_ context.Context, ns, name string, sd map[string]string, labels, annotations map[string]string) error {
	f.secrets[ns+"/"+name] = sd
	meta := map[string]any{"name": name, "namespace": ns}
	l := map[string]any{}
	for k, v := range labels {
		l[k] = v
	}
	meta["labels"] = l
	if len(annotations) > 0 {
		a := map[string]any{}
		for k, v := range annotations {
			a[k] = v
		}
		meta["annotations"] = a
	}
	f.secretObjs[ns+"/"+name] = map[string]any{"metadata": meta}
	return nil
}
func (f *fakeAPI) ListSecrets(_ context.Context, ns, labelSelector string) ([]map[string]any, error) {
	var out []map[string]any
	for k, v := range f.secretObjs {
		if strings.HasPrefix(k, ns+"/") && matchSelector(v, labelSelector) {
			out = append(out, v)
		}
	}
	return out, nil
}
func (f *fakeAPI) ListSecretsAllNamespaces(_ context.Context, labelSelector string) ([]map[string]any, error) {
	var out []map[string]any
	for _, v := range f.secretObjs {
		if matchSelector(v, labelSelector) {
			out = append(out, v)
		}
	}
	return out, nil
}
func (f *fakeAPI) GetSecretData(_ context.Context, ns, name string) (map[string][]byte, error) {
	sd, ok := f.secrets[ns+"/"+name]
	if !ok {
		return nil, nil
	}
	out := map[string][]byte{}
	for k, v := range sd {
		out[k] = []byte(v)
	}
	return out, nil
}
func (f *fakeAPI) AnnotateSecret(_ context.Context, ns, name string, annotations map[string]string) error {
	if f.annotated == nil {
		f.annotated = map[string]map[string]string{}
	}
	f.annotated[ns+"/"+name] = annotations
	return nil
}
func (f *fakeAPI) DeleteSecret(_ context.Context, ns, name string) error {
	delete(f.secrets, ns+"/"+name)
	delete(f.secretObjs, ns+"/"+name)
	f.deleted = append(f.deleted, "secret:"+ns+"/"+name)
	return nil
}
func (f *fakeAPI) ApplyApplication(_ context.Context, app map[string]any) error {
	meta := app["metadata"].(map[string]any)
	key := meta["namespace"].(string) + "/" + meta["name"].(string)
	// Same-field-manager SSA: the applied object REPLACES the manager's owned field set — a
	// field absent from the patch is an ownership retraction and gets REMOVED. The earlier
	// "merge only spec.source" approximation here hid exactly the prod 422 (a partial
	// PatchClusterValues apply stripped the required spec.destination/spec.project).
	f.apps[key] = app
	return nil
}
func (f *fakeAPI) GetApplication(_ context.Context, ns, name string) (map[string]any, error) {
	return f.apps[ns+"/"+name], nil
}

// matchSelector applies a "k=v[,k2=v2…]" label selector against metadata.labels.
func matchSelector(obj map[string]any, selector string) bool {
	if selector == "" {
		return true
	}
	labels, _ := dig(obj, "metadata", "labels").(map[string]any)
	for _, term := range strings.Split(selector, ",") {
		k, v, _ := strings.Cut(term, "=")
		if labels[k] != v {
			return false
		}
	}
	return true
}

func (f *fakeAPI) ListApplications(_ context.Context, ns, labelSelector string) ([]map[string]any, error) {
	var out []map[string]any
	for k, v := range f.apps {
		if strings.HasPrefix(k, ns+"/") && matchSelector(v, labelSelector) {
			out = append(out, v)
		}
	}
	return out, nil
}
func (f *fakeAPI) DeleteApplication(_ context.Context, ns, name string) error {
	delete(f.apps, ns+"/"+name)
	f.deleted = append(f.deleted, "app:"+ns+"/"+name)
	return nil
}
func (f *fakeAPI) GetTenantControlPlane(_ context.Context, ns, name string) (map[string]any, error) {
	return f.tcps[ns+"/"+name], nil
}
func (f *fakeAPI) ListTenantControlPlanes(_ context.Context, ns, labelSelector string) ([]map[string]any, error) {
	var out []map[string]any
	for k, v := range f.tcps {
		if strings.HasPrefix(k, ns+"/") && matchSelector(v, labelSelector) {
			out = append(out, v)
		}
	}
	return out, nil
}
func (f *fakeAPI) ListMachineDeployments(_ context.Context, _, _ string) ([]map[string]any, error) {
	return f.mds, nil
}

func TestServiceCreateDelete(t *testing.T) {
	api := newFakeAPI()
	svc := NewWithAPI(api, testCfg(), "svc-1")
	ctx := context.Background()

	data, err := svc.CreateCluster(ctx, testSpec(), ClusterSecrets{Cloud: map[string]string{"token": "t"}, CSIManifest: "m"})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if _, ok := api.namespaces["pc-p1"]; !ok {
		t.Error("namespace not ensured")
	}
	sd := api.secrets["pc-p1/stc-abcd1234-cloud-config"]
	if sd == nil || sd["token"] != "t" {
		t.Errorf("cloud-config secret = %v", sd)
	}
	if csi := api.secrets["pc-p1/stc-abcd1234-csi-config"]; csi["csi.yaml"] != "m" {
		t.Errorf("csi secret = %v", csi)
	}
	if api.apps["argocd/stc-abcd1234"] == nil {
		t.Error("application not applied")
	}
	c := data["cluster"].(map[string]any)
	if c["status"] != "PENDING" || c["id"] != "stc-abcd1234" || c["name"] != "prod cluster" {
		t.Errorf("initial data = %v", c)
	}

	if err := svc.DeleteCluster(ctx, "p1", "stc-abcd1234"); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if len(api.apps) != 0 {
		t.Error("delete left the application behind")
	}
	// The credentials secret must SURVIVE the cluster delete — the infrastructure provider needs
	// it while the ArgoCD cascade deletes the worker VMs. FinalizeOrphans reaps it afterwards.
	if len(api.secrets) != 3 {
		t.Error("credentials and marker secrets must outlive the application delete")
	}
	// Idempotent: deleting again is fine.
	if err := svc.DeleteCluster(ctx, "p1", "stc-abcd1234"); err != nil {
		t.Fatalf("DeleteCluster twice: %v", err)
	}
}

// CAPMOX owns the cloud secret, so it is gone with the ProxmoxCluster. The release must still run.
func TestFinalizeAfterCloudSecretCollected(t *testing.T) {
	api := newFakeAPI()
	svc := NewWithAPI(api, testCfg(), "svc-1")
	ctx := context.Background()
	spec := testSpec()
	var released []string
	svc.OnClusterGone(func(_ context.Context, compute, project, cluster string) error {
		released = append(released, compute+"/"+project+"/"+cluster)
		return nil
	})
	if _, err := svc.CreateCluster(ctx, spec, ClusterSecrets{Cloud: map[string]string{"token": "t"}, ComputeServiceID: "cs1"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteCluster(ctx, "p1", spec.ID); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{CloudSecretName(spec.ID), CSISecretName(spec.ID)} {
		delete(api.secrets, "pc-p1/"+n)
		delete(api.secretObjs, "pc-p1/"+n)
	}
	meta := api.secretObjs["pc-p1/"+MarkerSecretName(spec.ID)]["metadata"].(map[string]any)
	meta["creationTimestamp"] = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := svc.FinalizeAllOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0] != "cs1/p1/"+spec.ID {
		t.Fatalf("released = %v", released)
	}
	if len(api.secrets) != 0 {
		t.Fatalf("secrets left: %v", api.secrets)
	}
}

func TestFinalizeOrphans(t *testing.T) {
	api := newFakeAPI()
	svc := NewWithAPI(api, testCfg(), "svc-1")
	ctx := context.Background()
	spec := testSpec()
	if _, err := svc.CreateCluster(ctx, spec, ClusterSecrets{Cloud: map[string]string{"token": "t"}}); err != nil {
		t.Fatal(err)
	}

	// Cluster still alive → nothing happens.
	if _, err := svc.FinalizeOrphans(ctx, "p1"); err != nil {
		t.Fatalf("FinalizeOrphans (live): %v", err)
	}
	if len(api.secrets) != 3 {
		t.Fatal("live cluster must not be finalized")
	}

	// Application deleted, but the TCP (cascade in flight) still exists → still nothing, and the
	// leftover is reported as pending (teardown defers tenant deletion on this signal).
	if err := svc.DeleteCluster(ctx, "p1", spec.ID); err != nil {
		t.Fatal(err)
	}
	api.tcps["pc-p1/"+ControlPlaneName(spec.ID)] = map[string]any{"metadata": map[string]any{"name": ControlPlaneName(spec.ID)}}
	pending, err := svc.FinalizeOrphans(ctx, "p1")
	if err != nil {
		t.Fatalf("FinalizeOrphans (cascade): %v", err)
	}
	if pending != 1 || len(api.secrets) != 3 {
		t.Fatalf("in-flight cascade must stay pending (pending=%d)", pending)
	}

	// Grace window: a fresh secret (mid-create signature) must never be treated as an orphan
	// even with no Application/TCP — pending, untouched.
	delete(api.tcps, "pc-p1/"+ControlPlaneName(spec.ID))
	meta := api.secretObjs["pc-p1/"+MarkerSecretName(spec.ID)]["metadata"].(map[string]any)
	meta["creationTimestamp"] = time.Now().UTC().Format(time.RFC3339)
	pending, err = svc.FinalizeOrphans(ctx, "p1")
	if err != nil {
		t.Fatalf("FinalizeOrphans (grace): %v", err)
	}
	if pending != 1 || len(api.secrets) != 3 {
		t.Fatalf("fresh secret must ride out the grace window (pending=%d)", pending)
	}

	// Cascade done (TCP gone, secret old) → secret deleted, and the now-empty managed namespace
	// GC'd.
	meta["creationTimestamp"] = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := svc.FinalizeOrphans(ctx, "p1"); err != nil {
		t.Fatalf("FinalizeOrphans (done): %v", err)
	}
	if len(api.secrets) != 0 {
		t.Error("orphan secret not reaped")
	}
	if _, ok := api.namespaces["pc-p1"]; ok {
		t.Error("empty managed namespace not GC'd")
	}

	// The service-level sweep sees the same world (labels carry service+project) — a second
	// cluster's leftovers finalize through FinalizeAllOrphans without any project doc.
	spec2 := testSpec()
	spec2.ID = "stc-second01"
	if _, err := svc.CreateCluster(ctx, spec2, ClusterSecrets{Cloud: map[string]string{"token": "t"}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteCluster(ctx, "p1", spec2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FinalizeAllOrphans(ctx); err != nil {
		t.Fatalf("FinalizeAllOrphans: %v", err)
	}
	if len(api.secrets) != 0 {
		t.Error("sweep left the orphan secret")
	}
}

func TestValidateUpgradePath(t *testing.T) {
	ok := [][2]string{{"1.34.2", "1.34.5"}, {"1.34.2", "1.35.0"}, {"v1.34.2", "1.35.4"}}
	for _, c := range ok {
		if err := ValidateUpgradePath(c[0], c[1]); err != nil {
			t.Errorf("%s → %s: %v", c[0], c[1], err)
		}
	}
	bad := [][2]string{
		{"1.35.4", "1.35.4"}, // same version
		{"1.35.4", "1.34.2"}, // minor downgrade
		{"1.35.4", "1.35.1"}, // patch downgrade
		{"1.33.0", "1.35.0"}, // two-minor jump
		{"1.35.4", "2.0.0"},  // major change
		{"junk", "1.35.4"},   // unparseable
	}
	for _, c := range bad {
		if err := ValidateUpgradePath(c[0], c[1]); err == nil {
			t.Errorf("%s → %s: want error", c[0], c[1])
		}
	}
}

func TestOwnershipGuards(t *testing.T) {
	api := newFakeAPI()
	svc := NewWithAPI(api, testCfg(), "svc-1")
	ctx := context.Background()
	// An Application NOT created by proxcloud (no managed-by label) — delete and patch must refuse.
	api.apps["argocd/legacy"] = map[string]any{
		"metadata": map[string]any{"name": "legacy", "namespace": "argocd"},
		"spec":     map[string]any{"source": map[string]any{"helm": map[string]any{"valuesObject": map[string]any{}}}},
	}
	if err := svc.DeleteCluster(ctx, "p1", "legacy"); err == nil || !strings.Contains(err.Error(), "not managed by proxcloud") {
		t.Errorf("DeleteCluster unmanaged: %v", err)
	}
	if _, ok := api.apps["argocd/legacy"]; !ok {
		t.Fatal("unmanaged application was deleted")
	}
	if err := svc.PatchClusterValues(ctx, "legacy", func(map[string]any) error { return nil }); err == nil || !strings.Contains(err.Error(), "not managed by proxcloud") {
		t.Errorf("PatchClusterValues unmanaged: %v", err)
	}
}

func TestAdminKubeconfig(t *testing.T) {
	api := newFakeAPI()
	svc := NewWithAPI(api, testCfg(), "svc-1")
	ctx := context.Background()

	// The TCP is named after the KamajiControlPlane the chart renders, NOT after the cluster id.
	api.tcps["pc-p1/"+ControlPlaneName("stc-x")] = map[string]any{
		"metadata": map[string]any{"name": ControlPlaneName("stc-x")},
	}
	api.secrets["pc-p1/"+AdminKubeconfigSecretName("stc-x")] = map[string]string{"admin.conf": "KUBECONFIG"}

	kc, err := svc.AdminKubeconfig(ctx, "p1", "stc-x")
	if err != nil {
		t.Fatalf("AdminKubeconfig: %v", err)
	}
	if string(kc) != "KUBECONFIG" {
		t.Errorf("kubeconfig = %q", kc)
	}
	// Never persisted anywhere by the service — nothing to assert beyond the fetch itself (plan D5).

	if _, err := svc.AdminKubeconfig(ctx, "p1", "stc-none"); err == nil {
		t.Error("absent cluster: want error")
	}

	// A cluster whose TCP carries a name we do not derive (hand-migrated, or a future chart layout)
	// still resolves through the label the Kamaji CAPI provider stamps.
	api.tcps["pc-p1/legacy-tcp"] = map[string]any{
		"metadata": map[string]any{
			"name":   "legacy-tcp",
			"labels": map[string]any{"cluster.x-k8s.io/cluster-name": "stc-y"},
		},
	}
	api.secrets["pc-p1/legacy-tcp-admin-kubeconfig"] = map[string]string{"admin.conf": "LEGACY"}
	kc, err = svc.AdminKubeconfig(ctx, "p1", "stc-y")
	if err != nil || string(kc) != "LEGACY" {
		t.Errorf("label fallback: %q %v", kc, err)
	}
}

// TestPublicKubeconfig: the DOWNLOADED kubeconfig's server is rewritten onto the cluster's
// public FQDN (port + CA preserved); no DNS zone or unparseable input → returned verbatim.
func TestPublicKubeconfig(t *testing.T) {
	kc := []byte(`apiVersion: v1
kind: Config
clusters:
- name: stc-x
  cluster:
    server: https://10.0.0.75:6443
    certificate-authority-data: Q0FEQVRB
contexts:
- name: admin
  context: {cluster: stc-x, user: admin}
users:
- name: admin
  user: {token: tok}
current-context: admin
`)
	out := publicKubeconfig(kc, "stc-x.k8s.example.com")
	s := string(out)
	if !strings.Contains(s, "server: https://stc-x.k8s.example.com:6443") {
		t.Errorf("server not rewritten:\n%s", s)
	}
	if strings.Contains(s, "10.0.0.75") {
		t.Errorf("internal address leaked:\n%s", s)
	}
	if !strings.Contains(s, "Q0FEQVRB") || !strings.Contains(s, "token: tok") {
		t.Errorf("CA/user data lost:\n%s", s)
	}

	// No DNS zone → untouched. Unparseable → untouched.
	if got := publicKubeconfig(kc, ""); string(got) != string(kc) {
		t.Error("no-zone rewrite must be identity")
	}
	if got := publicKubeconfig([]byte(":\tnot yaml"), "f.q.dn"); string(got) != ":\tnot yaml" {
		t.Error("unparseable input must round-trip verbatim")
	}

	// End-to-end through AdminKubeconfig with the provider's DNS zone (testCfg: k8s.example.com).
	api := newFakeAPI()
	svc := NewWithAPI(api, testCfg(), "svc-1")
	api.tcps["pc-p9/"+ControlPlaneName("stc-z")] = map[string]any{
		"metadata": map[string]any{"name": ControlPlaneName("stc-z")},
	}
	api.secrets["pc-p9/"+AdminKubeconfigSecretName("stc-z")] = map[string]string{"admin.conf": string(kc)}
	got, err := svc.AdminKubeconfig(context.Background(), "p9", "stc-z")
	if err != nil || !strings.Contains(string(got), "server: https://stc-z.k8s.example.com:6443") {
		t.Errorf("AdminKubeconfig rewrite: err=%v\n%s", err, got)
	}
}

func TestRotateKubeconfig(t *testing.T) {
	api := newFakeAPI()
	svc := NewWithAPI(api, testCfg(), "svc-1")
	ctx := context.Background()

	if err := svc.RotateKubeconfig(ctx, "p1", "stc-x"); err == nil {
		t.Error("no control plane yet: want error")
	}
	api.tcps["pc-p1/"+ControlPlaneName("stc-x")] = map[string]any{
		"metadata": map[string]any{"name": ControlPlaneName("stc-x")},
	}
	if err := svc.RotateKubeconfig(ctx, "p1", "stc-x"); err != nil {
		t.Fatalf("RotateKubeconfig: %v", err)
	}
	// Kamaji watches for this annotation and regenerates the secret; anything else is a no-op that
	// would look like it worked.
	ann := api.annotated["pc-p1/"+AdminKubeconfigSecretName("stc-x")]
	if _, ok := ann["certs.kamaji.clastix.io/rotate"]; !ok {
		t.Errorf("rotate annotation = %v", ann)
	}
}

func TestPatchClusterValuesUpgrade(t *testing.T) {
	api := newFakeAPI()
	cfg := testCfg()
	svc := NewWithAPI(api, cfg, "svc-1")
	ctx := context.Background()
	spec := testSpec()
	if _, err := svc.CreateCluster(ctx, spec, ClusterSecrets{Cloud: map[string]string{"token": "t"}}); err != nil {
		t.Fatal(err)
	}

	err := svc.PatchClusterValues(ctx, spec.ID, func(values map[string]any) error {
		values["kubernetesVersion"] = "1.34.2"
		return nil
	})
	if err != nil {
		t.Fatalf("PatchClusterValues: %v", err)
	}
	app := api.apps["argocd/"+spec.ID]
	src := app["spec"].(map[string]any)["source"].(map[string]any)
	if src["targetRevision"] != "0.2.3" {
		t.Errorf("chart pin changed: %v", src["targetRevision"])
	}
	values := src["helm"].(map[string]any)["valuesObject"].(map[string]any)
	if values["kubernetesVersion"] != "1.34.2" {
		t.Errorf("version = %v", values["kubernetesVersion"])
	}
	// The patch must re-apply EVERYTHING this field manager owns: with SSA, omitting a field
	// retracts it — the api server 422s on the required spec fields (the prod upgrade failure)
	// and would silently drop the finalizer + ownership labels + display-name annotation.
	if dig(app, "spec", "destination") == nil || dig(app, "spec", "project") == nil {
		t.Errorf("patch dropped spec.destination/spec.project: %v", app["spec"])
	}
	if dig(app, "spec", "syncPolicy") == nil {
		t.Error("patch dropped spec.syncPolicy")
	}
	if labels, _ := dig(app, "metadata", "labels").(map[string]any); labels[LabelManagedBy] != ManagedByValue {
		t.Errorf("patch dropped the ownership labels: %v", dig(app, "metadata", "labels"))
	}
	if fins, _ := dig(app, "metadata", "finalizers").([]any); len(fins) == 0 {
		t.Error("patch dropped the resources-finalizer")
	}

	if err := svc.PatchClusterValues(ctx, "stc-none", func(map[string]any) error { return nil }); err == nil {
		t.Error("absent cluster: want error")
	}
}

// TestSetChartVersionAndPins: the platform-update leg — re-pin keeps everything else the
// manager owns (the SSA lesson), and the pin listing reflects it.
func TestSetChartVersionAndPins(t *testing.T) {
	api := newFakeAPI()
	svc := NewWithAPI(api, testCfg(), "svc-1")
	ctx := context.Background()
	spec := testSpec()
	if _, err := svc.CreateCluster(ctx, spec, ClusterSecrets{Cloud: map[string]string{"token": "t"}}); err != nil {
		t.Fatal(err)
	}
	pins, err := svc.ListClusterPins(ctx)
	if err != nil || len(pins) != 1 || pins[0].ChartVersion != "0.2.3" || pins[0].ProjectID != "p1" {
		t.Fatalf("pins = %+v err=%v", pins, err)
	}
	if err := svc.SetChartVersion(ctx, spec.ID, "0.9.9"); err != nil {
		t.Fatalf("SetChartVersion: %v", err)
	}
	app := api.apps["argocd/"+spec.ID]
	if rev := dig(app, "spec", "source", "targetRevision"); rev != "0.9.9" {
		t.Errorf("targetRevision = %v", rev)
	}
	if dig(app, "spec", "destination") == nil || dig(app, "spec", "project") == nil {
		t.Error("re-pin dropped required spec fields")
	}
	if values := dig(app, "spec", "source", "helm", "valuesObject"); values == nil {
		t.Error("re-pin dropped the helm values")
	}
	if err := svc.SetChartVersion(ctx, spec.ID, ""); err == nil {
		t.Error("blank version must error")
	}
}

func TestSyncProviderList(t *testing.T) {
	api := newFakeAPI()
	cfg := testCfg()
	svc := NewWithAPI(api, cfg, "svc-1")
	ctx := context.Background()
	spec := testSpec()
	if _, err := svc.CreateCluster(ctx, spec, ClusterSecrets{Cloud: map[string]string{"token": "t"}}); err != nil {
		t.Fatal(err)
	}
	// Enrich the fake with live status: argo health + TCP endpoint + one MD.
	app := api.apps["argocd/"+spec.ID]
	app["status"] = map[string]any{
		"health": map[string]any{"status": "Healthy"},
		"sync":   map[string]any{"status": "Synced"},
	}
	app["metadata"].(map[string]any)["creationTimestamp"] = "2026-07-12T00:00:00Z"
	api.tcps["pc-p1/"+ControlPlaneName(spec.ID)] = map[string]any{
		"metadata": map[string]any{"name": ControlPlaneName(spec.ID)},
		"status":   map[string]any{"controlPlaneEndpoint": "10.0.0.5:6443"},
	}
	api.mds = []map[string]any{{
		"metadata": map[string]any{"name": spec.ID + "-workers-pve1", "labels": map[string]any{labelNodeGroup: "workers"}},
		"status":   map[string]any{"replicas": float64(2), "readyReplicas": float64(1), "phase": "ScalingUp"},
	}, {
		"metadata": map[string]any{"name": spec.ID + "-workers-pve2", "labels": map[string]any{labelNodeGroup: "workers"}},
		"status":   map[string]any{"replicas": float64(1), "readyReplicas": float64(1), "phase": "Running"},
	}}

	// A PRE-PROXCLOUD cluster on the same management cluster (same project label but NO managed-by
	// marker — worst case) must never enter the cache (decision 2026-07-12).
	api.apps["argocd/legacy-cluster"] = map[string]any{
		"metadata": map[string]any{
			"name":   "legacy-cluster",
			"labels": map[string]any{LabelProject: "p1"},
		},
		"spec": map[string]any{"source": map[string]any{}},
	}

	list, err := svc.SyncProvider("az1", "p1").List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("resources = %d (unmanaged cluster must be invisible)", len(list))
	}
	cr := list[0]
	if cr.Type != "KUBERNETES_CLUSTER" || cr.ExternalID != spec.ID || cr.Region != "az1" || cr.ProjectID != "p1" {
		t.Errorf("resource identity = %+v", cr)
	}
	c := cr.Data["cluster"].(map[string]any)
	if c["status"] != "READY" || c["sync_status"] != "Synced" {
		t.Errorf("status = %v / %v", c["status"], c["sync_status"])
	}
	if c["endpoint"] != "10.0.0.5:6443" {
		t.Errorf("endpoint = %v", c["endpoint"])
	}
	if c["created_at"] != "2026-07-12T00:00:00Z" {
		t.Errorf("created_at = %v", c["created_at"])
	}
	groups := c["node_groups"].([]any)
	g0 := groups[0].(map[string]any)
	if g0["ready_replicas"] != float64(2) || g0["phase"] != "ScalingUp" {
		t.Errorf("live merge = %v", g0)
	}
}

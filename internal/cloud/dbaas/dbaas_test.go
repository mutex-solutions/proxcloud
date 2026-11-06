package dbaas

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
)

// fakeAPI implements K8sAPI in memory, recording op order (create-ordering assertions) and
// modelling the two SSA semantics the real client has: Secret.type is IMMUTABLE (ApplySecret
// hardcodes Opaque — a re-apply onto an operator-minted basic-auth secret must 422) and the
// label set is REPLACED per apply (same-field-manager retraction). ListSecrets* honor the
// label selector — a typo'd sweep selector must fail tests, not silently no-op in prod.
type fakeAPI struct {
	namespaces  map[string]map[string]any
	secrets     map[string]map[string]any // key ns/name → full object
	secretData  map[string]map[string][]byte
	secretTypes map[string]string // key ns/name → Secret.type
	netpols     map[string]map[string]any
	apps        map[string]map[string]any            // key ns/name
	services    map[string]map[string]any            // key ns/name
	vpas        map[string]map[string]any            // key ns/name
	crs         map[string]map[string]map[string]any // plural → ns/name → object
	pods        map[string]map[string]any            // key ns/name
	ops         []string
	failApply   bool
	failDelete  map[string]bool // secret names whose DeleteSecret fails
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		namespaces:  map[string]map[string]any{},
		secrets:     map[string]map[string]any{},
		secretData:  map[string]map[string][]byte{},
		secretTypes: map[string]string{},
		netpols:     map[string]map[string]any{},
		apps:        map[string]map[string]any{},
		services:    map[string]map[string]any{},
		vpas:        map[string]map[string]any{},
		crs:         map[string]map[string]map[string]any{},
		pods:        map[string]map[string]any{},
		failDelete:  map[string]bool{},
	}
}

func key(ns, name string) string { return ns + "/" + name }

func (f *fakeAPI) EnsureNamespace(_ context.Context, name string, labels map[string]string) error {
	f.ops = append(f.ops, "ns:"+name)
	l := map[string]any{}
	for k, v := range labels {
		l[k] = v
	}
	f.namespaces[name] = map[string]any{"metadata": map[string]any{"name": name, "labels": l}}
	return nil
}
func (f *fakeAPI) GetNamespace(_ context.Context, name string) (map[string]any, error) {
	return f.namespaces[name], nil
}
func (f *fakeAPI) DeleteNamespace(_ context.Context, name string) error {
	f.ops = append(f.ops, "delns:"+name)
	delete(f.namespaces, name)
	return nil
}
func (f *fakeAPI) ApplySecret(_ context.Context, ns, name string, stringData map[string]string, labels, annotations map[string]string) error {
	f.ops = append(f.ops, "secret:"+name)
	k := key(ns, name)
	// The real ApplySecret SSA-applies type Opaque; the apiserver rejects a type change.
	if existing, ok := f.secretTypes[k]; ok && existing != "Opaque" {
		return &kamajik8s.APIError{Status: 422, Message: "Secret.type: field is immutable"}
	}
	obj := f.secrets[k]
	if obj == nil {
		meta := map[string]any{"name": name, "namespace": ns,
			"creationTimestamp": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)}
		obj = map[string]any{"metadata": meta}
		f.secrets[k] = obj
		f.secretData[k] = map[string][]byte{}
	}
	f.secretTypes[k] = "Opaque"
	meta := obj["metadata"].(map[string]any)
	// SSA same-manager semantics: the label/annotation SETS are replaced, absent = retracted.
	l := map[string]any{}
	for lk, lv := range labels {
		l[lk] = lv
	}
	meta["labels"] = l
	a := map[string]any{}
	for ak, av := range annotations {
		a[ak] = av
	}
	meta["annotations"] = a
	for dk, dv := range stringData {
		f.secretData[k][dk] = []byte(dv)
	}
	return nil
}
func (f *fakeAPI) PatchSecretData(_ context.Context, ns, name string, stringData map[string]string) error {
	f.ops = append(f.ops, "patchsecret:"+name)
	k := key(ns, name)
	if _, ok := f.secrets[k]; !ok {
		return &kamajik8s.APIError{Status: 404, Message: "not found"}
	}
	for dk, dv := range stringData {
		f.secretData[k][dk] = []byte(dv)
	}
	return nil
}
func (f *fakeAPI) ApplyNetworkPolicy(_ context.Context, np map[string]any) error {
	name := digStr(np, "metadata", "name")
	ns := digStr(np, "metadata", "namespace")
	f.ops = append(f.ops, "np:"+name)
	f.netpols[key(ns, name)] = np
	return nil
}
func (f *fakeAPI) GetSecretData(_ context.Context, ns, name string) (map[string][]byte, error) {
	d, ok := f.secretData[key(ns, name)]
	if !ok {
		return nil, nil
	}
	return d, nil
}
func (f *fakeAPI) listSecrets(ns, sel string) []map[string]any {
	var out []map[string]any
	for k, obj := range f.secrets {
		if (ns == "" || strings.HasPrefix(k, ns+"/")) && matchesSelector(obj, sel) {
			out = append(out, obj)
		}
	}
	return out
}
func (f *fakeAPI) ListSecrets(_ context.Context, ns, labelSelector string) ([]map[string]any, error) {
	return f.listSecrets(ns, labelSelector), nil
}
func (f *fakeAPI) ListSecretsAllNamespaces(_ context.Context, labelSelector string) ([]map[string]any, error) {
	return f.listSecrets("", labelSelector), nil
}
func (f *fakeAPI) DeleteSecret(_ context.Context, ns, name string) error {
	f.ops = append(f.ops, "delsecret:"+name)
	if f.failDelete[name] {
		return errors.New("transient apiserver error")
	}
	delete(f.secrets, key(ns, name))
	delete(f.secretData, key(ns, name))
	delete(f.secretTypes, key(ns, name))
	return nil
}
func (f *fakeAPI) ApplyApplication(_ context.Context, app map[string]any) error {
	if f.failApply {
		return errors.New("apply failed")
	}
	meta := app["metadata"].(map[string]any)
	name := meta["name"].(string)
	ns := meta["namespace"].(string)
	f.ops = append(f.ops, "app:"+name)
	if meta["creationTimestamp"] == nil {
		meta["creationTimestamp"] = time.Now().UTC().Format(time.RFC3339)
	}
	f.apps[key(ns, name)] = app
	return nil
}
func (f *fakeAPI) GetApplication(_ context.Context, ns, name string) (map[string]any, error) {
	return f.apps[key(ns, name)], nil
}
func (f *fakeAPI) ListApplications(_ context.Context, ns, labelSelector string) ([]map[string]any, error) {
	var out []map[string]any
	for k, app := range f.apps {
		if !strings.HasPrefix(k, ns+"/") {
			continue
		}
		if !matchesSelector(app, labelSelector) {
			continue
		}
		out = append(out, app)
	}
	return out, nil
}
func (f *fakeAPI) DeleteApplication(_ context.Context, ns, name string) error {
	f.ops = append(f.ops, "delapp:"+name)
	delete(f.apps, key(ns, name))
	return nil
}
func (f *fakeAPI) GetService(_ context.Context, ns, name string) (map[string]any, error) {
	return f.services[key(ns, name)], nil
}

func (f *fakeAPI) GetVPA(_ context.Context, ns, name string) (map[string]any, error) {
	return f.vpas[key(ns, name)], nil
}

// ListCRs serves the generic CR reads (backup lists). Keyed by plural so a test can seed
// backups without modelling every operator's CRD.
func (f *fakeAPI) ListCRs(_ context.Context, _, _, plural, ns, sel string) ([]map[string]any, error) {
	var out []map[string]any
	for k, obj := range f.crs[plural] {
		if strings.HasPrefix(k, ns+"/") && matchesSelector(obj, sel) {
			out = append(out, obj)
		}
	}
	return out, nil
}

// ListPods / PodLogs back the log reads. Pods are seeded per test; a name in failDelete
// doubles as "this pod's log read fails", which is how the partial-failure path is covered.
func (f *fakeAPI) ListPods(_ context.Context, ns, sel string) ([]map[string]any, error) {
	var out []map[string]any
	for k, obj := range f.pods {
		if strings.HasPrefix(k, ns+"/") && matchesSelector(obj, sel) {
			out = append(out, obj)
		}
	}
	return out, nil
}

func (f *fakeAPI) PodLogs(_ context.Context, ns, pod, container string, tail int) (string, error) {
	if f.failDelete[pod] {
		return "", errors.New("container is not running")
	}
	return fmt.Sprintf("log of %s/%s container=%s tail=%d", ns, pod, container, tail), nil
}

// matchesSelector honours the label selectors the Service actually uses (k=v comma lists).
func matchesSelector(obj map[string]any, sel string) bool {
	if sel == "" {
		return true
	}
	labels, _ := dig(obj, "metadata", "labels").(map[string]any)
	for _, part := range strings.Split(sel, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		if v, _ := labels[kv[0]].(string); v != kv[1] {
			return false
		}
	}
	return true
}

func testConfig() Config {
	return Config{
		Kubeconfig:     "kc",
		Region:         "RegionOne",
		ArgoNamespace:  "argocd",
		ArgoProject:    "proxcloud-dbaas",
		ChartRepo:      "ghcr.io/mutex-solutions/charts",
		ChartName:      "database-cluster",
		ChartVersion:   "0.1.0",
		StorageClasses: []string{"nvme"},
		Limits:         Limits{MaxCPU: 32, MaxMemoryGiB: 128, MaxStorageGiB: 2048},
		Engines: map[string]EngineOffer{
			EnginePostgreSQL: {Versions: []string{"16", "17"}, Default: "17", Replicas: []int{1, 2, 3}},
			EngineMySQL:      {Versions: []string{"8.4"}, Default: "8.4", Replicas: []int{1, 3}},
			EngineMariaDB:    {Versions: []string{"11.8"}, Default: "11.8", Replicas: []int{1, 3}},
			EngineValkey:     {Versions: []string{"9"}, Default: "9", Replicas: []int{1, 3}, Beta: true},
			EngineFerretDB:   {Versions: []string{"2"}, Default: "2", Replicas: []int{1, 2, 3}},
			EngineOpenSearch: {Versions: []string{"3.3.0"}, Default: "3.3.0", Replicas: []int{1, 3}},
			EngineKafka:      {Versions: []string{"4.2.0", "4.3.0"}, Default: "4.3.0", Replicas: []int{1, 3}},
		},
	}
}

func testSpec(engine, version string) DatabaseSpec {
	return DatabaseSpec{
		ID: "std-abc12345", DisplayName: "my db", ProjectID: "p1",
		Engine: engine, Version: version, Replicas: 1,
		CPU: 2, MemoryGiB: 4, StorageGiB: 20,
		AllowedCIDRs: []string{"10.1.0.0/24"},
	}
}

func TestSpecValidate(t *testing.T) {
	cfg := testConfig()
	if err := testSpec(EnginePostgreSQL, "17").Validate(cfg); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	cases := map[string]DatabaseSpec{}
	s := testSpec(EnginePostgreSQL, "17")
	s.Engine = "oracle"
	cases["unknown engine"] = s
	s = testSpec(EnginePostgreSQL, "99")
	cases["version off catalog"] = s
	s = testSpec(EnginePostgreSQL, "17")
	s.Replicas = 5
	cases["replicas off catalog"] = s
	s = testSpec(EnginePostgreSQL, "17")
	s.CPU = 64
	cases["cpu over limit"] = s
	s = testSpec(EnginePostgreSQL, "17")
	s.StorageGiB = 0
	cases["zero storage"] = s
	s = testSpec(EnginePostgreSQL, "17")
	s.StorageClass = "hdd"
	cases["storage class off allowlist"] = s
	s = testSpec(EnginePostgreSQL, "17")
	s.AllowedCIDRs = []string{"nope"}
	cases["bad cidr"] = s
	s = testSpec(EngineValkey, "9")
	cases["beta without ack"] = s
	s = testSpec(EnginePostgreSQL, "17")
	s.DashboardsEnabled = true
	cases["dashboards on non-opensearch"] = s
	s = testSpec(EnginePostgreSQL, "17")
	s.ReadEndpoint = true // one instance: the "reader" would be the primary under another name
	cases["read endpoint on a single instance"] = s
	s = testSpec(EngineKafka, "4.3.0")
	s.Replicas = 3
	s.ReadEndpoint = true
	cases["read endpoint on an engine without one"] = s
	s = testSpec(EnginePostgreSQL, "17")
	s.RestoreFrom = &RestoreSource{SourceID: "std-0abc1234"}
	cases["restore without an object store"] = s
	for name, spec := range cases {
		if err := spec.Validate(cfg); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
	// Beta ack lets valkey through.
	vk := testSpec(EngineValkey, "9")
	vk.BetaAck = true
	if err := vk.Validate(cfg); err != nil {
		t.Errorf("beta ack rejected: %v", err)
	}
}

func TestNames(t *testing.T) {
	if NamespaceFor("p1") != "pc-p1" {
		t.Fatal("NamespaceFor")
	}
	if LBServiceName("std-x") != "std-x-lb" {
		t.Fatal("LBServiceName")
	}
	if AuthSecretName("std-x") != "std-x-auth" {
		t.Fatal("AuthSecretName")
	}
	if MarkerSecretName("std-x") != "std-x-marker" {
		t.Fatal("MarkerSecretName")
	}
}

// TestBuildValues pins EVERY chart-values key per engine — the chart-drift tripwire (the
// database-cluster chart's values.yaml is the other half of this contract).
func TestBuildValues(t *testing.T) {
	cfg := testConfig()
	for _, engine := range []string{EnginePostgreSQL, EngineMySQL, EngineMariaDB, EngineValkey, EngineFerretDB} {
		spec := testSpec(engine, cfg.Engines[engine].Default)
		spec.StorageClass = "nvme"
		v := BuildValues(cfg, spec)
		want := map[string]any{
			"engine":        engine,
			"engineVersion": cfg.Engines[engine].Default,
			"instances":     1,
		}
		for k, w := range want {
			if v[k] != w {
				t.Errorf("%s: values[%q] = %v, want %v", engine, k, v[k], w)
			}
		}
		res := v["resources"].(map[string]any)
		if res["cpu"] != 2 || res["memoryGi"] != 4 {
			t.Errorf("%s: resources = %v", engine, res)
		}
		st := v["storage"].(map[string]any)
		if st["sizeGi"] != 20 || st["storageClassName"] != "nvme" {
			t.Errorf("%s: storage = %v", engine, st)
		}
		net := v["network"].(map[string]any)
		if len(net) != 1 {
			t.Errorf("%s: network carries only allowedCidrs, got %v", engine, net)
		}
		if cidrs := net["allowedCidrs"].([]any); len(cidrs) != 1 || cidrs[0] != "10.1.0.0/24" {
			t.Errorf("%s: allowedCidrs = %v", engine, net["allowedCidrs"])
		}
		strat := v["proxcloud"].(map[string]any)
		if strat["projectId"] != "p1" || strat["resourceId"] != spec.ID || strat["displayName"] != "my db" {
			t.Errorf("%s: proxcloud = %v", engine, strat)
		}
		// The complete top-level key set — a new key must be added HERE and in the chart.
		for k := range v {
			switch k {
			case "engine", "engineVersion", "instances", "resources", "storage", "network", "proxcloud":
			default:
				t.Errorf("%s: unexpected top-level values key %q", engine, k)
			}
		}
		// No secret-looking material may ever enter values (Application CRs are argocd-readable).
		if strings.Contains(strings.ToLower(fmt.Sprint(v)), "password") {
			t.Errorf("%s: values leak a password-like key", engine)
		}
	}
	// Default storage class stays absent (chart falls back to the cluster default).
	v := BuildValues(cfg, testSpec(EnginePostgreSQL, "17"))
	if _, has := v["storage"].(map[string]any)["storageClassName"]; has {
		t.Error("empty storage class must be omitted")
	}
}

func TestBuildApplication(t *testing.T) {
	cfg := testConfig()
	spec := testSpec(EnginePostgreSQL, "17")
	app := BuildApplication(cfg, spec, "svc-dbaas", "", BuildValues(cfg, spec))
	if digStr(app, "metadata", "name") != spec.ID || digStr(app, "metadata", "namespace") != "argocd" {
		t.Fatal("metadata name/namespace")
	}
	fins, _ := dig(app, "metadata", "finalizers").([]any)
	if len(fins) != 1 || fins[0] != "resources-finalizer.argocd.argoproj.io" {
		t.Fatal("resources-finalizer missing — delete would not cascade")
	}
	if digStr(app, "spec", "project") != "proxcloud-dbaas" {
		t.Fatal("AppProject")
	}
	if digStr(app, "spec", "source", "targetRevision") != "0.1.0" {
		t.Fatal("chart pin fallback")
	}
	if digStr(app, "spec", "destination", "namespace") != "pc-p1" {
		t.Fatal("destination namespace")
	}
	labels, _ := dig(app, "metadata", "labels").(map[string]any)
	if labels[LabelProject] != "p1" || labels[LabelService] != "svc-dbaas" || labels[LabelManagedBy] != ManagedByValue {
		t.Fatal("labels")
	}
	// Empty ArgoProject falls back to the guardrail project, never ArgoCD's "default".
	cfg2 := cfg
	cfg2.ArgoProject = ""
	app2 := BuildApplication(cfg2, spec, "svc", "", nil)
	if digStr(app2, "spec", "project") != "proxcloud-dbaas" {
		t.Fatal("ArgoProject fallback")
	}
}

func TestServiceCreateDelete(t *testing.T) {
	api := newFakeAPI()
	s := NewWithAPI(api, testConfig(), "svc-dbaas")
	ctx := context.Background()

	// mariadb needs the proxcloud-owned auth secret; ordering = ns+default-deny → marker →
	// auth → Application (marker FIRST: a crash mid-create converges via the sweep).
	spec := testSpec(EngineMariaDB, "11.8")
	data, err := s.CreateDatabase(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	wantOps := []string{"ns:pc-p1", "np:proxcloud-default-deny", "secret:" + MarkerSecretName(spec.ID), "secret:" + AuthSecretName(spec.ID), "app:" + spec.ID}
	if got := strings.Join(api.ops, ","); got != strings.Join(wantOps, ",") {
		t.Fatalf("create op order = %s, want %s", got, strings.Join(wantOps, ","))
	}
	if _, has := api.netpols[key("pc-p1", "proxcloud-default-deny")]; !has {
		t.Fatal("namespace default-deny must be stamped at ensure time")
	}
	db, _ := data["database"].(map[string]any)
	if db["status"] != "PENDING" || db["engine"] != EngineMariaDB {
		t.Fatalf("initial data = %v", db)
	}

	// postgres does NOT get an auth secret (CNPG mints its own).
	api2 := newFakeAPI()
	s2 := NewWithAPI(api2, testConfig(), "svc-dbaas")
	if _, err := s2.CreateDatabase(ctx, testSpec(EnginePostgreSQL, "17")); err != nil {
		t.Fatal(err)
	}
	if _, has := api2.secrets[key("pc-p1", AuthSecretName("std-abc12345"))]; has {
		t.Fatal("postgresql must not get a proxcloud auth secret")
	}

	// Delete removes only the Application; the marker stays (the sweep owns it).
	if err := s.DeleteDatabase(ctx, "p1", spec.ID); err != nil {
		t.Fatal(err)
	}
	if _, alive := api.apps[key("argocd", spec.ID)]; alive {
		t.Fatal("application not deleted")
	}
	if _, has := api.secrets[key("pc-p1", MarkerSecretName(spec.ID))]; !has {
		t.Fatal("marker must survive the delete (the sweep owns it)")
	}
}

func TestOwnershipGuards(t *testing.T) {
	api := newFakeAPI()
	s := NewWithAPI(api, testConfig(), "svc-dbaas")
	ctx := context.Background()
	// A foreign Application (no managed-by label) must refuse delete AND patch.
	api.apps[key("argocd", "std-foreign")] = map[string]any{
		"metadata": map[string]any{"name": "std-foreign", "namespace": "argocd"},
		"spec":     map[string]any{},
	}
	if err := s.DeleteDatabase(ctx, "p1", "std-foreign"); err == nil {
		t.Fatal("delete of unmanaged application must refuse")
	}
	if err := s.PatchDatabaseApp(ctx, "std-foreign", func(map[string]any) error { return nil }); err == nil {
		t.Fatal("patch of unmanaged application must refuse")
	}
}

func TestFinalizeOrphans(t *testing.T) {
	ctx := context.Background()
	mkMarker := func(api *fakeAPI, dbID string, age time.Duration) {
		k := key("pc-p1", MarkerSecretName(dbID))
		api.secrets[k] = map[string]any{"metadata": map[string]any{
			"name": MarkerSecretName(dbID), "namespace": "pc-p1",
			"creationTimestamp": time.Now().UTC().Add(-age).Format(time.RFC3339),
			"labels":            map[string]any{LabelProject: "p1", LabelService: "svc-dbaas", LabelManagedBy: ManagedByValue},
		}}
	}

	// Fresh marker (inside grace) → pending, untouched.
	api := newFakeAPI()
	s := NewWithAPI(api, testConfig(), "svc-dbaas")
	mkMarker(api, "std-new", time.Minute)
	pending, err := s.FinalizeOrphans(ctx, "p1")
	if err != nil || pending != 1 {
		t.Fatalf("grace window: pending=%d err=%v", pending, err)
	}

	// Live Application → skipped, not pending.
	api = newFakeAPI()
	s = NewWithAPI(api, testConfig(), "svc-dbaas")
	mkMarker(api, "std-live", 2*time.Hour)
	api.apps[key("argocd", "std-live")] = map[string]any{"metadata": map[string]any{
		"name": "std-live", "namespace": "argocd",
		"labels": map[string]any{LabelProject: "p1", LabelManagedBy: ManagedByValue},
	}}
	if pending, err = s.FinalizeOrphans(ctx, "p1"); err != nil || pending != 0 {
		t.Fatalf("live app: pending=%d err=%v", pending, err)
	}
	if _, has := api.secrets[key("pc-p1", MarkerSecretName("std-live"))]; !has {
		t.Fatal("live database's marker must stay")
	}

	// LB Service still present → cascade in flight → pending.
	api = newFakeAPI()
	s = NewWithAPI(api, testConfig(), "svc-dbaas")
	mkMarker(api, "std-lb", 2*time.Hour)
	api.services[key("pc-p1", LBServiceName("std-lb"))] = map[string]any{"metadata": map[string]any{"name": LBServiceName("std-lb")}}
	if pending, err = s.FinalizeOrphans(ctx, "p1"); err != nil || pending != 1 {
		t.Fatalf("lb present: pending=%d err=%v", pending, err)
	}

	// Auth-secret delete failure → marker STAYS (it is the only retry driver), pending.
	api = newFakeAPI()
	s = NewWithAPI(api, testConfig(), "svc-dbaas")
	mkMarker(api, "std-x", 2*time.Hour)
	api.secrets[key("pc-p1", AuthSecretName("std-x"))] = map[string]any{"metadata": map[string]any{
		"name": AuthSecretName("std-x"), "namespace": "pc-p1"}}
	api.failDelete[AuthSecretName("std-x")] = true
	pending, err = s.FinalizeOrphans(ctx, "p1")
	if pending != 1 || err == nil {
		t.Fatalf("auth delete failure: pending=%d err=%v", pending, err)
	}
	if _, has := api.secrets[key("pc-p1", MarkerSecretName("std-x"))]; !has {
		t.Fatal("marker must survive an auth-secret delete failure (retry driver)")
	}

	// Clean orphan → marker + auth reaped, ns GC'd.
	api = newFakeAPI()
	s = NewWithAPI(api, testConfig(), "svc-dbaas")
	api.namespaces["pc-p1"] = map[string]any{"metadata": map[string]any{
		"name": "pc-p1", "labels": map[string]any{LabelManagedBy: ManagedByValue}}}
	mkMarker(api, "std-x", 2*time.Hour)
	api.secrets[key("pc-p1", AuthSecretName("std-x"))] = map[string]any{"metadata": map[string]any{
		"name": AuthSecretName("std-x"), "namespace": "pc-p1"}}
	pending, err = s.FinalizeAllOrphans(ctx)
	if err != nil || pending != 0 {
		t.Fatalf("clean orphan: pending=%d err=%v", pending, err)
	}
	if _, has := api.secrets[key("pc-p1", MarkerSecretName("std-x"))]; has {
		t.Fatal("finished-cascade marker must be reaped")
	}
	if _, has := api.secrets[key("pc-p1", AuthSecretName("std-x"))]; has {
		t.Fatal("auth secret must be reaped with the marker")
	}
	if _, has := api.namespaces["pc-p1"]; has {
		t.Fatal("emptied managed namespace must be GC'd")
	}
}

func TestSyncProviderList(t *testing.T) {
	api := newFakeAPI()
	cfg := testConfig()
	s := NewWithAPI(api, cfg, "svc-dbaas")
	ctx := context.Background()
	spec := testSpec(EnginePostgreSQL, "17")
	if _, err := s.CreateDatabase(ctx, spec); err != nil {
		t.Fatal(err)
	}
	app := api.apps[key("argocd", spec.ID)]
	app["status"] = map[string]any{
		"health": map[string]any{"status": "Healthy"},
		"sync":   map[string]any{"status": "Synced"},
	}
	api.services[key("pc-p1", LBServiceName(spec.ID))] = map[string]any{
		"status": map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"ip": "10.1.0.5"}}}},
	}
	// A terminating sibling must NOT re-enter the cache.
	api.apps[key("argocd", "std-dying")] = map[string]any{
		"metadata": map[string]any{"name": "std-dying", "namespace": "argocd",
			"deletionTimestamp": "2026-01-01T00:00:00Z",
			"labels":            map[string]any{LabelProject: "p1", LabelManagedBy: ManagedByValue}},
		"spec": map[string]any{},
	}
	rows, err := s.SyncProvider("RegionOne", "p1").List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d (terminating app must be skipped)", len(rows))
	}
	db, _ := rows[0].Data["database"].(map[string]any)
	if db["status"] != "READY" || db["endpoint"] != "10.1.0.5" || db["port"] != 5432 {
		t.Fatalf("data = %v", db)
	}
	if db["engine"] != EnginePostgreSQL || db["version"] != "17" || db["replicas"] != 1 {
		t.Fatalf("spec read-back = %v", db)
	}
	if rows[0].Type != "DATABASE_CLUSTER" || rows[0].ExternalID != spec.ID || rows[0].Region != "RegionOne" {
		t.Fatalf("row = %+v", rows[0])
	}
	// Degraded maps through; missing health = PENDING.
	app["status"] = map[string]any{"health": map[string]any{"status": "Degraded"}}
	rows, _ = s.SyncProvider("RegionOne", "p1").List(ctx)
	if db, _ := rows[0].Data["database"].(map[string]any); db["status"] != "DEGRADED" {
		t.Fatal("Degraded map")
	}
	delete(app, "status")
	rows, _ = s.SyncProvider("RegionOne", "p1").List(ctx)
	if db, _ := rows[0].Data["database"].(map[string]any); db["status"] != "PENDING" {
		t.Fatal("no-health map")
	}
}

// TestConnectionSecretContract pins the per-engine secret tuple — the chart templates are the
// other half of this contract; a rename on either side must fail here.
func TestConnectionSecretContract(t *testing.T) {
	cases := []struct {
		engine, name, userKey, passKey, dbKey string
		port                                  int
		defUser, defDB                        string
	}{
		// The exposed postgres account is the SUPERUSER (chart sets enableSuperuserAccess); the
		// database name is fixed, not read off that secret.
		{EnginePostgreSQL, "std-1-superuser", "username", "password", "", 5432, "", "app"},
		{EngineFerretDB, "std-1-pg-app", "username", "password", "dbname", 27017, "", ""},
		{EngineMySQL, "std-1-secrets", "", "root", "", 3306, "root", ""},
		{EngineMariaDB, "std-1-auth", "", "password", "", 3306, "app", "app"},
		// valkey's exposed account is the `default` ACL user the chart wires to <id>-auth.
		{EngineValkey, "std-1-auth", "", "password", "", 6379, "default", ""},
		{EngineOpenSearch, "std-1-admin-password", "username", "password", "", 9200, "", ""},
		{EngineKafka, "std-1-auth", "", "password", "", 9094, "std-1-app", ""},
	}
	for _, c := range cases {
		name, uk, pk, dk := ConnectionSecret(c.engine, "std-1")
		if name != c.name || uk != c.userKey || pk != c.passKey || dk != c.dbKey {
			t.Errorf("%s: got (%s,%s,%s,%s)", c.engine, name, uk, pk, dk)
		}
		if Port(c.engine) != c.port {
			t.Errorf("%s: port %d", c.engine, Port(c.engine))
		}
		if DefaultUser(c.engine, "std-1") != c.defUser || DefaultDB(c.engine) != c.defDB {
			t.Errorf("%s: defaults (%s,%s)", c.engine, DefaultUser(c.engine, "std-1"), DefaultDB(c.engine))
		}
	}
	// Databases predating the superuser switch keep working off the scoped app role.
	if n, uk, pk, dk := PriorConnectionSecret(EnginePostgreSQL, "std-1"); n != "std-1-app" || uk != "username" || pk != "password" || dk != "dbname" {
		t.Errorf("postgres fallback = (%s,%s,%s,%s)", n, uk, pk, dk)
	}
	for _, e := range []string{EngineMySQL, EngineMariaDB, EngineValkey, EngineOpenSearch, EngineKafka, EngineFerretDB} {
		if n, _, _, _ := PriorConnectionSecret(e, "std-1"); n != "" {
			t.Errorf("%s must have no fallback, got %s", e, n)
		}
	}
	if !NeedsAuthSecret(EngineMariaDB) || !NeedsAuthSecret(EngineValkey) || !NeedsAuthSecret(EngineKafka) ||
		NeedsAuthSecret(EnginePostgreSQL) || NeedsAuthSecret(EngineMySQL) || NeedsAuthSecret(EngineOpenSearch) {
		t.Fatal("NeedsAuthSecret set")
	}
	if LBServiceNameFor(EngineKafka, "std-1") != "std-1-kafka-external-bootstrap" ||
		LBServiceNameFor(EnginePostgreSQL, "std-1") != "std-1-lb" {
		t.Fatal("LBServiceNameFor")
	}
}

func TestConnectionInfo(t *testing.T) {
	api := newFakeAPI()
	s := NewWithAPI(api, testConfig(), "svc-dbaas")
	ctx := context.Background()
	spec := testSpec(EnginePostgreSQL, "17")
	if _, err := s.CreateDatabase(ctx, spec); err != nil {
		t.Fatal(err)
	}
	// Not ready: no secret yet.
	if _, err := s.ConnectionInfo(ctx, "p1", spec.ID); err == nil {
		t.Fatal("expected not-ready before the operator mints the secret")
	}
	// CNPG mints the superuser secret; LB gets its address.
	api.secretData[key("pc-p1", spec.ID+"-superuser")] = map[string][]byte{
		"username": []byte("postgres"), "password": []byte("s3cr3t"), "dbname": []byte("postgres"),
	}
	api.secrets[key("pc-p1", spec.ID+"-superuser")] = map[string]any{"metadata": map[string]any{"name": spec.ID + "-superuser"}}
	api.services[key("pc-p1", LBServiceName(spec.ID))] = map[string]any{
		"status": map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"ip": "10.1.0.9"}}}},
	}
	info, err := s.ConnectionInfo(ctx, "p1", spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Host != "10.1.0.9" || info.Port != 5432 || info.Username != "postgres" || info.Password != "s3cr3t" || info.DBName != "app" {
		t.Fatalf("info = %+v", info)
	}
	if info.URI != "postgresql://postgres:s3cr3t@10.1.0.9:5432/app" {
		t.Fatalf("uri = %s", info.URI)
	}
}

func TestResetPassword(t *testing.T) {
	api := newFakeAPI()
	s := NewWithAPI(api, testConfig(), "svc-dbaas")
	ctx := context.Background()
	spec := testSpec(EngineMariaDB, "11.8")
	if _, err := s.CreateDatabase(ctx, spec); err != nil {
		t.Fatal(err)
	}
	before := string(api.secretData[key("pc-p1", AuthSecretName(spec.ID))]["password"])
	pw, err := s.ResetPassword(ctx, "p1", spec.ID, EngineMariaDB)
	if err != nil {
		t.Fatal(err)
	}
	after := string(api.secretData[key("pc-p1", AuthSecretName(spec.ID))]["password"])
	if pw == "" || pw == before || after != pw {
		t.Fatalf("reset: before=%q pw=%q after=%q", before, pw, after)
	}
	// The rotation must be a DATA-ONLY patch: the proxcloud ownership labels applied at create
	// survive (an SSA re-apply under the same field manager would retract them).
	auth := api.secrets[key("pc-p1", AuthSecretName(spec.ID))]
	if digStr(auth, "metadata", "labels", LabelManagedBy) != ManagedByValue {
		t.Fatal("reset stripped the ownership labels off the proxcloud-owned auth secret")
	}
	if _, err := s.ResetPassword(ctx, "p1", spec.ID, "oracle"); err == nil {
		t.Fatal("unknown engine must refuse")
	}
}

// TestResetPasswordOperatorOwned pins the CNPG case that killed the SSA approach: the app
// secret is OPERATOR-minted with the immutable type kubernetes.io/basic-auth — the rotation
// must merge-patch only the password key (an ApplySecret would 422 on the type), and a
// not-yet-minted secret must read as "not ready", never be SSA-created under the operator.
func TestResetPasswordOperatorOwned(t *testing.T) {
	api := newFakeAPI()
	s := NewWithAPI(api, testConfig(), "svc-dbaas")
	ctx := context.Background()
	spec := testSpec(EnginePostgreSQL, "17")
	if _, err := s.CreateDatabase(ctx, spec); err != nil {
		t.Fatal(err)
	}
	// Not minted yet → not-ready, and NO secret may appear as a side effect.
	if _, err := s.ResetPassword(ctx, "p1", spec.ID, EnginePostgreSQL); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("pre-mint reset must be not-ready, got %v", err)
	}
	if _, has := api.secrets[key("pc-p1", spec.ID+"-app")]; has {
		t.Fatal("reset must never create the operator's secret")
	}
	// CNPG mints the basic-auth secret.
	k := key("pc-p1", spec.ID+"-app")
	api.secrets[k] = map[string]any{"metadata": map[string]any{
		"name": spec.ID + "-app", "namespace": "pc-p1",
		"labels": map[string]any{"cnpg.io/cluster": spec.ID},
	}}
	api.secretTypes[k] = "kubernetes.io/basic-auth"
	api.secretData[k] = map[string][]byte{"username": []byte("app"), "password": []byte("old"), "dbname": []byte("app")}
	pw, err := s.ResetPassword(ctx, "p1", spec.ID, EnginePostgreSQL)
	if err != nil {
		t.Fatalf("rotation on a basic-auth secret must merge-patch, not SSA: %v", err)
	}
	if string(api.secretData[k]["password"]) != pw || string(api.secretData[k]["username"]) != "app" {
		t.Fatalf("only the password key may change: %v", api.secretData[k])
	}
	if api.secretTypes[k] != "kubernetes.io/basic-auth" {
		t.Fatal("secret type must be untouched")
	}
	if digStr(api.secrets[k], "metadata", "labels", "cnpg.io/cluster") != spec.ID {
		t.Fatal("operator labels must be untouched")
	}
}

func TestValidateUpgradePath(t *testing.T) {
	ok := [][2]string{{"16", "17"}, {"10.11", "11.4"}, {"8.4", "9.0"}, {"9.6", "10"}, {"2.5", "2.6"}}
	for _, c := range ok {
		if err := ValidateUpgradePath(c[0], c[1]); err != nil {
			t.Errorf("%s→%s must be allowed: %v", c[0], c[1], err)
		}
	}
	bad := [][2]string{{"17", "16"}, {"11.4", "10.11"}, {"17", "17"}, {"10", "9.6"}, {"x", "17"}, {"17", ""}}
	for _, c := range bad {
		if err := ValidateUpgradePath(c[0], c[1]); err == nil {
			t.Errorf("%s→%s must be refused", c[0], c[1])
		}
	}
}

func TestSetChartVersionAndPins(t *testing.T) {
	api := newFakeAPI()
	s := NewWithAPI(api, testConfig(), "svc-dbaas")
	ctx := context.Background()
	spec := testSpec(EnginePostgreSQL, "17")
	if _, err := s.CreateDatabase(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChartVersion(ctx, spec.ID, "0.2.0"); err != nil {
		t.Fatal(err)
	}
	pins, err := s.ListDatabasePins(ctx)
	if err != nil || len(pins) != 1 {
		t.Fatalf("pins=%v err=%v", pins, err)
	}
	if pins[0].ChartVersion != "0.2.0" || pins[0].Engine != EnginePostgreSQL || pins[0].ProjectID != "p1" {
		t.Fatalf("pin = %+v", pins[0])
	}
	if err := s.SetChartVersion(ctx, spec.ID, ""); err == nil {
		t.Fatal("empty version must refuse")
	}
	// The patch re-applies FULL metadata (finalizer survives).
	app := api.apps[key("argocd", spec.ID)]
	fins, _ := dig(app, "metadata", "finalizers").([]any)
	if len(fins) != 1 {
		t.Fatal("patch dropped the resources-finalizer")
	}
}

// TestStorageClassNeverCapacity pins the storage rule: the capacity tier (HDD NFS) is never
// offered to a database — a spec naming it is rejected even when the provider config lists it.
func TestStorageClassNeverCapacity(t *testing.T) {
	for name, want := range map[string]bool{
		"capacity": true, "capacity-nfs": true, "capacity-": true,
		"nvme": false, "capacitor": false, "my-capacity": false, "": false,
	} {
		if got := CapacityClass(name); got != want {
			t.Errorf("CapacityClass(%q) = %v, want %v", name, got, want)
		}
	}
	cfg := testConfig()
	cfg.StorageClasses = []string{"nvme", "capacity", "capacity-nfs"}
	for _, class := range []string{"capacity", "capacity-nfs"} {
		spec := testSpec(EnginePostgreSQL, "17")
		spec.StorageClass = class
		if err := spec.Validate(cfg); err == nil {
			t.Errorf("storage class %q must be rejected even when allow-listed", class)
		}
	}
	spec := testSpec(EnginePostgreSQL, "17")
	spec.StorageClass = "nvme"
	if err := spec.Validate(cfg); err != nil {
		t.Errorf("nvme rejected: %v", err)
	}
}

// TestBuildValuesNetworkAndScheduling pins the MetalLB address pool and the zone spread key:
// addressPool rides network.*, zoneKey rides scheduling.* — both only when configured, so the
// chart defaults (any auto-assign pool, topology.kubernetes.io/zone) stand otherwise.
func TestBuildValuesNetworkAndScheduling(t *testing.T) {
	cfg := testConfig()
	spec := testSpec(EnginePostgreSQL, "17")
	v := BuildValues(cfg, spec)
	if _, has := v["scheduling"]; has {
		t.Errorf("no scheduling config → no scheduling block, got %v", v["scheduling"])
	}
	if _, has := v["network"].(map[string]any)["addressPool"]; has {
		t.Error("no pool configured → no addressPool key")
	}

	cfg.LBAddressPool = "dbaas-services"
	cfg.ZoneKey = "kubernetes.io/hostname"
	cfg.NodeSelector = map[string]string{"node-role": "database"}
	cfg.Tolerations = []map[string]any{{"key": "dedicated", "operator": "Equal", "value": "database", "effect": "NoSchedule"}}
	spec.ReadEndpoint = true
	spec.Replicas = 3
	v = BuildValues(cfg, spec)
	net := v["network"].(map[string]any)
	if net["addressPool"] != "dbaas-services" || net["readEndpoint"] != true {
		t.Errorf("network = %v", net)
	}
	for _, banned := range []string{"networkId", "subnetId", "memberSubnetId"} {
		if _, has := net[banned]; has {
			t.Errorf("network.%s must not exist: nothing is placed on a tenant network", banned)
		}
	}
	sch := v["scheduling"].(map[string]any)
	if sch["zoneKey"] != "kubernetes.io/hostname" {
		t.Errorf("scheduling.zoneKey = %v", sch["zoneKey"])
	}
	if sel := sch["nodeSelector"].(map[string]any); sel["node-role"] != "database" {
		t.Errorf("nodeSelector = %v", sel)
	}
	if tol := sch["tolerations"].([]any); len(tol) != 1 {
		t.Errorf("tolerations = %v", tol)
	}
}

// TestCreateDatabaseNeedsNoTenantNetwork drives create end to end with a pool configured: no
// network leg, the Application carries the pool, marker first.
func TestCreateDatabaseNeedsNoTenantNetwork(t *testing.T) {
	api := newFakeAPI()
	cfg := testConfig()
	cfg.LBAddressPool = "dbaas-services"
	s := NewWithAPI(api, cfg, "svc-dbaas")
	spec := testSpec(EnginePostgreSQL, "17")
	if _, err := s.CreateDatabase(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	app := api.apps[key("argocd", spec.ID)]
	if got := digStr(app, "spec", "source", "helm", "valuesObject", "network", "addressPool"); got != "dbaas-services" {
		t.Fatalf("addressPool on the Application = %q", got)
	}
}

// TestFinalizeReapsDatabaseSecrets: a deleted database leaves nothing behind — its backup
// credentials, customer-user passwords and BYO certificate go with the marker, while a sibling
// database's secrets (which share the namespace) are untouched.
func TestFinalizeReapsDatabaseSecrets(t *testing.T) {
	ctx := context.Background()
	api := newFakeAPI()
	cfg := testConfig()
	cfg.Backup = BackupConfig{Endpoint: "http://s3.example.com:9000", Bucket: "b", AccessKey: "a", SecretKey: "s"}
	s := NewWithAPI(api, cfg, "svc-dbaas")
	s.finalizeGrace = 0
	gone, kept := testSpec(EnginePostgreSQL, "17"), testSpec(EnginePostgreSQL, "17")
	kept.ID = "std-keep0001"
	for _, spec := range []DatabaseSpec{gone, kept} {
		if _, err := s.CreateDatabase(ctx, spec); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{UserSecretName(spec.ID, "alice_b"), CustomTLSSecretName(spec.ID)} {
			if err := api.ApplySecret(ctx, "pc-p1", name, map[string]string{"k": "v"},
				map[string]string{LabelProject: "p1", LabelService: "svc-dbaas", LabelManagedBy: ManagedByValue}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, has := api.secrets[key("pc-p1", BackupSecretName(gone.ID))]; !has {
		t.Fatal("create must write the backup credential Secret")
	}
	delete(api.apps, key("argocd", gone.ID)) // the delete cascade finished
	if _, err := s.FinalizeOrphans(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{MarkerSecretName(gone.ID), BackupSecretName(gone.ID), UserSecretName(gone.ID, "alice_b"), CustomTLSSecretName(gone.ID)} {
		if _, has := api.secrets[key("pc-p1", name)]; has {
			t.Errorf("secret %s outlived its database", name)
		}
	}
	for _, name := range []string{MarkerSecretName(kept.ID), BackupSecretName(kept.ID), UserSecretName(kept.ID, "alice_b"), CustomTLSSecretName(kept.ID)} {
		if _, has := api.secrets[key("pc-p1", name)]; !has {
			t.Errorf("sibling secret %s must stay", name)
		}
	}
}

func TestValidateParameters(t *testing.T) {
	if err := ValidateParameters(EnginePostgreSQL, map[string]string{"max_connections": "200", "shared_buffers": "1GB"}); err != nil {
		t.Fatalf("valid set rejected: %v", err)
	}
	for name, params := range map[string]map[string]string{
		"not tunable (operator-owned)": {"wal_level": "logical"},
		"out of range":                 {"max_connections": "1"},
		"injection":                    {"work_mem": "4MB\n[mysqld]\nlog_bin=OFF"},
		"not a size":                   {"shared_buffers": "lots"},
	} {
		if err := ValidateParameters(EnginePostgreSQL, params); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
	if err := ValidateParameters(EngineFerretDB, map[string]string{"max_connections": "200"}); err == nil {
		t.Error("ferretdb has no tunables")
	}
	if !ParametersNeedRestart(EnginePostgreSQL, map[string]string{"max_connections": "200"}) ||
		ParametersNeedRestart(EnginePostgreSQL, map[string]string{"work_mem": "8MB"}) {
		t.Error("restart flag")
	}
}

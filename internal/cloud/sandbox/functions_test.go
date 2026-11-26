package sandbox

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The functions block: unset limits get their defaults, a project without limits of its own is never unlimited, and
// a bad block makes functions unavailable without touching the provider's sandboxes (Config.Validate does not read
// it).
func TestFunctionsConfig(t *testing.T) {
	got := Config{}.WithDefaults().Functions
	want := FunctionsConfig{IdleSeconds: 300, MinIdleSeconds: 60, MaxIdleSeconds: 3600, MaxCPUMillis: 2000, MaxMemoryMi: 4096, MaxFunctions: 20, MaxInstances: 5,
		ProjectQuota: FunctionQuota{Functions: 20, VCPUs: 8, RAMMb: 16384}}
	if got != want {
		t.Fatalf("defaults: %+v", got)
	}
	set := FunctionsConfig{Enabled: true, MinInstances: 1, MaxMinInstances: 1, IdleSeconds: 120, MaxInstances: 3, ProjectQuota: FunctionQuota{Functions: 2}}.WithDefaults()
	if set.IdleSeconds != 120 || set.MaxInstances != 3 || set.ProjectQuota != (FunctionQuota{Functions: 2, VCPUs: 8, RAMMb: 16384}) || !set.Enabled || set.MinInstances != 1 {
		t.Fatalf("set values: %+v", set)
	}
	// The block as the provider document carries it.
	var doc FunctionsConfig
	if err := json.Unmarshal([]byte(`{"enabled":true,"idleSeconds":90,"minIdleSeconds":60,"maxIdleSeconds":600,"maxCpuMillis":1000,"maxMemoryMi":1024,"maxFunctions":5,"maxInstances":2,"projectQuota":{"functions":3,"vcpus":2,"ramMb":2048}}`), &doc); err != nil {
		t.Fatal(err)
	}
	if doc != (FunctionsConfig{Enabled: true, IdleSeconds: 90, MinIdleSeconds: 60, MaxIdleSeconds: 600, MaxCPUMillis: 1000, MaxMemoryMi: 1024, MaxFunctions: 5, MaxInstances: 2, ProjectQuota: FunctionQuota{Functions: 3, VCPUs: 2, RAMMb: 2048}}) {
		t.Fatalf("decoded: %+v", doc)
	}

	on := FunctionsConfig{Enabled: true}
	if err := on.Validate(); err != nil {
		t.Fatalf("an enabled block of defaults: %v", err)
	}
	for want, mut := range map[string]func(*FunctionsConfig){
		"minInstances must be 0 or 1":      func(f *FunctionsConfig) { f.MinInstances = 2 },
		"maxMinInstances must be 0 or 1":   func(f *FunctionsConfig) { f.MaxMinInstances = 2 },
		"minIdleSeconds must be at least":  func(f *FunctionsConfig) { f.MinIdleSeconds = 5 },
		"idleSeconds must be between":      func(f *FunctionsConfig) { f.IdleSeconds = 7200 },
		"maxCpuMillis must be at least":    func(f *FunctionsConfig) { f.MaxCPUMillis = 100 },
		"maxMemoryMi must be at least":     func(f *FunctionsConfig) { f.MaxMemoryMi = 64 },
		"maxInstances must be between 1 a": func(f *FunctionsConfig) { f.MaxInstances = MaxFunctionInstances + 1 },
	} {
		f := on
		mut(&f)
		if err := f.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v", want, err)
		}
		// Off, the same block is not checked, and the provider's sandboxes never see it.
		f.Enabled = false
		cfg := testConfig()
		cfg.Functions = f
		if f.Validate() != nil || cfg.ValidateFunctions() != nil {
			t.Errorf("%q: a disabled block was checked", want)
		}
		cfg.Functions.Enabled = true
		if err := cfg.Validate(); err != nil {
			t.Errorf("%q: a bad functions block failed the provider: %v", want, err)
		}
	}
	// Functions are served under the provider's sandbox domain: without one there is no host to give a function.
	cfg := testConfig()
	cfg.Functions = on
	for domain, ok := range map[string]bool{"": false, "sbx.example.com": true, "sbx_example.com": false, "sbx.example.com.": false, "sbx..example.com": false} {
		cfg.Domain = domain
		if err := cfg.ValidateFunctions(); (err == nil) != ok {
			t.Errorf("domain %q: %v", domain, err)
		}
	}
}

// A function namespace carries the tenant label every tenant namespace carries, and a function's pods the owner
// labels a sandbox's pods carry. No list of sandboxes may take either for its own: on a shared cluster the identity
// is bound to another role there, and anywhere a function is not a sandbox to pause, sample, count or delete.
func TestSandboxListsNeverReadAFunctionNamespace(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	functionPod := func(ns string) map[string]any {
		return map[string]any{
			"metadata": map[string]any{"name": "fn-0123456789abcdef-7d9f-abcde", "namespace": ns, "uid": "fn-pod-uid",
				"labels": map[string]any{LabelProject: "p1", LabelService: "svc", LabelManagedBy: ManagedByValue, LabelFunctionID: "fn-0123456789abcdef"}},
			"spec": map[string]any{"nodeName": "n1"}, "status": map[string]any{"phase": "Running"},
		}
	}
	functionDeployment := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "fn-0123456789abcdef", "namespace": "fn-p1", "uid": "fn-dep-uid",
			"labels":      map[string]any{LabelProject: "p1", LabelService: "svc", LabelManagedBy: ManagedByValue, LabelFunctionID: "fn-0123456789abcdef"},
			"annotations": map[string]any{AnnotationAutoPause: "true", AnnotationEndAt: now.Add(-time.Hour).UTC().Format(time.RFC3339)}},
	}

	// Shared: the project has a sandbox namespace and a function namespace, both labelled as the tenant's and as
	// this service's. The sandbox lists go through sb-p1 alone; fn-p1, where the identity holds no sandbox role, is
	// never asked (the fake refuses a list there, as the API server would).
	api := newSharedAPI()
	cfg := testConfig()
	cfg.Shared = true
	s := NewWithAPI(api, cfg, "svc")
	if err := s.EnsureProjectNamespace(ctx, "p1", nil); err != nil {
		t.Fatal(err)
	}
	if err := EnsureTenantNamespace(ctx, api.fakeAPI, false, "svc", "p1", FunctionNamespace("p1"), FunctionRole, map[string]string{LabelFunctionTenant: "true"}); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{LabelProject: "p1", LabelService: "svc", LabelManagedBy: ManagedByValue, LabelTenant: "true", LabelFunctionTenant: "true"}; !reflect.DeepEqual(api.namespaces["fn-p1"], want) {
		t.Fatalf("the function namespace is labelled %v", api.namespaces["fn-p1"])
	}
	api.crs["fn-p1/fn-0123456789abcdef"] = functionDeployment
	api.core["pods/fn-p1/fn-0123456789abcdef-7d9f-abcde"] = functionPod("fn-p1")
	if nss, err := s.tenantNamespaces(ctx); err != nil || !reflect.DeepEqual(nss, []string{"sb-p1"}) {
		t.Fatalf("the sandbox namespaces are %v (%v), want sb-p1 alone", nss, err)
	}
	if nss, err := TenantNamespaces(ctx, api, "svc", "fn-"); err != nil || !reflect.DeepEqual(nss, []string{"fn-p1"}) {
		t.Fatalf("the function namespaces are %v (%v), want fn-p1 alone", nss, err)
	}
	if due, err := s.DueForAutoPause(ctx); err != nil || len(due) != 0 {
		t.Errorf("due for pause: %v %v", due, err)
	}
	inFunctionNS := func(items []map[string]any) bool {
		for _, it := range items {
			if strings.HasPrefix(digStr(it, "metadata", "namespace"), "fn-") {
				return true
			}
		}
		return false
	}
	if crs, err := s.listCRsAll(ctx, CRGroup, CRVersion, CRPlural, ""); err != nil || inFunctionNS(crs) {
		t.Errorf("sandboxes of every project: %v %v", crs, err)
	}
	if pods, err := s.listPodsAll(ctx, ""); err != nil || inFunctionNS(pods) {
		t.Errorf("pods of every project: %v %v", pods, err)
	}
	if xs, err := s.AbuseSamples(ctx); err != nil || len(xs) != 0 {
		t.Errorf("samples: %v %v", xs, err)
	}
	if live, err := s.live(ctx, "p1"); err != nil || len(live) != 0 {
		t.Errorf("the project's sandboxes: %v %v", live, err)
	}
	if len(api.refused) != 0 {
		t.Fatalf("a sandbox list went where the identity holds no sandbox role: %v", api.refused)
	}

	// A sandbox cluster of its own: one list at the cluster scope returns a function's pod beside the sandboxes'
	// (it carries the owner labels). It is no sample: it has no sandbox id and no Sandbox object behind it.
	plain := NewWithAPI(&sampleAPI{fakeAPI: newFake(), pods: []map[string]any{functionPod("fn-p1")}}, testConfig(), "svc")
	if xs, err := plain.AbuseSamples(ctx); err != nil || len(xs) != 0 {
		t.Errorf("samples on a cluster of its own: %v %v", xs, err)
	}
	if live, err := plain.live(ctx, "p1"); err != nil || len(live) != 0 {
		t.Errorf("the project's sandboxes on a cluster of its own: %v %v", live, err)
	}
}

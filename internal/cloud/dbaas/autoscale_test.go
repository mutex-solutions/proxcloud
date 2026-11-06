package dbaas

import (
	"context"
	"testing"
)

func TestParseQuantities(t *testing.T) {
	cpu := map[string]int{"1500m": 2, "2": 2, "2500m": 3, "0.5": 1, "4000m": 4}
	for in, want := range cpu {
		if got, err := parseCPUQty(in); err != nil || got != want {
			t.Errorf("cpu %q → %d (err %v), want %d", in, got, err, want)
		}
	}
	mem := map[string]int{"1500Mi": 2, "2Gi": 2, "3221225472": 3, "1073741824": 1, "512Mi": 1}
	for in, want := range mem {
		if got, err := parseMemGiQty(in); err != nil || got != want {
			t.Errorf("mem %q → %d (err %v), want %d", in, got, err, want)
		}
	}
	if _, err := parseCPUQty("x"); err == nil {
		t.Error("garbage cpu must error")
	}
	if _, err := parseMemGiQty(""); err == nil {
		t.Error("empty mem must error")
	}
}

func TestAutoscaleTick(t *testing.T) {
	ctx := context.Background()
	mkDB := func(api *fakeAPI, s *Service, id string, autoscale map[string]any) {
		spec := testSpec(EnginePostgreSQL, "17")
		spec.ID = id
		spec.StorageGiB = 20
		if _, err := s.CreateDatabase(ctx, spec); err != nil {
			t.Fatal(err)
		}
		if autoscale != nil {
			values := dig(api.apps[key("argocd", id)], "spec", "source", "helm", "valuesObject").(map[string]any)
			values["autoscale"] = autoscale
		}
	}
	vpaRec := func(cpu, mem string) map[string]any {
		return map[string]any{"status": map[string]any{"recommendation": map[string]any{
			"containerRecommendations": []any{map[string]any{"target": map[string]any{"cpu": cpu, "memory": mem}}},
		}}}
	}
	resources := func(api *fakeAPI, id string) map[string]any {
		return dig(api.apps[key("argocd", id)], "spec", "source", "helm", "valuesObject", "resources").(map[string]any)
	}

	// VPA recommends above current → bump, clamped to the ceiling.
	api := newFakeAPI()
	s := NewWithAPI(api, testConfig(), "svc-dbaas")
	mkDB(api, s, "std-up", map[string]any{"enabled": true, "maxCpu": 3, "maxMemoryGi": 8, "maxStorageGi": 0})
	api.vpas[key("pc-db-p1", AutoscaleVPAName("std-up"))] = vpaRec("3500m", "6Gi") // cpu ceil 4 → clamp 3
	bumped, err := s.AutoscaleTick(ctx, "p1", nil)
	if err != nil || bumped != 1 {
		t.Fatalf("bumped=%d err=%v", bumped, err)
	}
	if res := resources(api, "std-up"); res["cpu"] != 3 || res["memoryGi"] != 6 {
		t.Fatalf("resources = %v (want cpu 3 clamped, mem 6)", res)
	}

	// Recommendation below current → scale-up-only, untouched.
	api = newFakeAPI()
	s = NewWithAPI(api, testConfig(), "svc-dbaas")
	mkDB(api, s, "std-down", map[string]any{"enabled": true, "maxCpu": 8, "maxMemoryGi": 16, "maxStorageGi": 0})
	api.vpas[key("pc-db-p1", AutoscaleVPAName("std-down"))] = vpaRec("500m", "1Gi")
	if bumped, err = s.AutoscaleTick(ctx, "p1", nil); err != nil || bumped != 0 {
		t.Fatalf("scale-down must be a no-op: bumped=%d err=%v", bumped, err)
	}
	if res := resources(api, "std-down"); res["cpu"] != 2 || res["memoryGi"] != 4 {
		t.Fatalf("resources changed on a downward rec: %v", res)
	}

	// Disk ≥80% full → +20% ceil, clamped to maxStorageGi.
	api = newFakeAPI()
	s = NewWithAPI(api, testConfig(), "svc-dbaas")
	mkDB(api, s, "std-disk", map[string]any{"enabled": true, "maxCpu": 2, "maxMemoryGi": 4, "maxStorageGi": 23})
	bumped, err = s.AutoscaleTick(ctx, "p1", func(_ context.Context, ns string) (map[string]float64, error) {
		if ns != "pc-db-p1" {
			t.Fatalf("ns = %s", ns)
		}
		return map[string]float64{"data-std-disk-1": 0.91, "data-std-other-1": 0.99}, nil
	})
	if err != nil || bumped != 1 {
		t.Fatalf("disk: bumped=%d err=%v", bumped, err)
	}
	storage := dig(api.apps[key("argocd", "std-disk")], "spec", "source", "helm", "valuesObject", "storage").(map[string]any)
	if storage["sizeGi"] != 23 { // 20*1.2=24 → clamp 23
		t.Fatalf("sizeGi = %v, want clamp 23", storage["sizeGi"])
	}

	// Disabled database: VPA present but never consulted for a patch.
	api = newFakeAPI()
	s = NewWithAPI(api, testConfig(), "svc-dbaas")
	mkDB(api, s, "std-off", nil)
	api.vpas[key("pc-db-p1", AutoscaleVPAName("std-off"))] = vpaRec("8", "16Gi")
	if bumped, err = s.AutoscaleTick(ctx, "p1", nil); err != nil || bumped != 0 {
		t.Fatalf("disabled: bumped=%d err=%v", bumped, err)
	}

	// No VPA object at all (CRD absent) → clean no-op, no error.
	api = newFakeAPI()
	s = NewWithAPI(api, testConfig(), "svc-dbaas")
	mkDB(api, s, "std-novpa", map[string]any{"enabled": true, "maxCpu": 8, "maxMemoryGi": 16, "maxStorageGi": 0})
	if bumped, err = s.AutoscaleTick(ctx, "p1", nil); err != nil || bumped != 0 {
		t.Fatalf("no vpa: bumped=%d err=%v", bumped, err)
	}
}

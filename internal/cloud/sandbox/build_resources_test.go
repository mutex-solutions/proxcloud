package sandbox

import (
	"context"
	"testing"
)

func TestBuildPendingPullNotBillable(t *testing.T) {
	pod := map[string]any{"status": map[string]any{"phase": "Pending", "startTime": "2026-11-09T00:00:00Z"}}
	if d := buildData(pod); d["status"] != StatusPending || d["runningSince"] != nil {
		t.Fatal(d)
	}
}

func TestIndependentBuildResources(t *testing.T) {
	cfg := testConfig()
	cfg.Build = BuildConfig{CPU: 3, MemoryGiB: 12, DiskGiB: 45, MaxCPU: 7, MaxMemoryGiB: 24, MaxDiskGiB: 60}
	r, err := cfg.Build.Resources(BuildResources{})
	if err != nil || r.CPU != 3 || r.MemoryGiB != 12 || r.DiskGiB != 45 {
		t.Fatal(r, err)
	}
	res, _ := RenderDockerfile("python:3.12", nil)
	_, p := BuildObjects(cfg, BuildRequest{BuildID: "b1", Result: res})
	k := firstInitContainer(p, "kaniko")
	if digStr(k, "resources", "requests", "cpu") != "3" || digStr(k, "resources", "requests", "memory") != "12Gi" || digStr(k, "resources", "limits", "ephemeral-storage") != "45Gi" {
		t.Fatal(k)
	}
	d := buildData(p)
	if d["cpu"] != 3 || d["memoryGiB"] != 12 {
		t.Fatal("billing must reflect real builder", d)
	}
	for _, bad := range []BuildResources{{CPU: 8}, {MemoryGiB: 25}, {DiskGiB: 61}, {CPU: -1}} {
		if _, err := cfg.Build.Resources(bad); err == nil {
			t.Fatal("bad size", bad)
		}
	}
	f := newFake()
	s := NewWithAPI(f, cfg, "svc")
	if err := s.StartBuild(context.Background(), BuildRequest{Result: res, CPU: 8}); err == nil || len(f.core) != 0 {
		t.Fatal("must reject before pod", err)
	}
}

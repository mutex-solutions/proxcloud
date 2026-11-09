package sandbox

import (
	"context"
	"errors"
	"testing"
)

func gpuPod(node, model string, gpus int, phase string) map[string]any {
	return map[string]any{
		"spec": map[string]any{"nodeName": node, "nodeSelector": map[string]any{LabelGPU: model},
			"containers": []any{map[string]any{"resources": map[string]any{"limits": map[string]any{ResourceGPU: gpus}}}}},
		"status": map[string]any{"phase": phase},
	}
}

func TestPoolGPUCapacity(t *testing.T) {
	cordoned := gpuNode("c1", "nvidia-a6000", 8, true)
	cordoned["spec"].(map[string]any)["unschedulable"] = true
	foreignTaint := gpuNode("t1", "nvidia-a6000", 8, true)
	foreignTaint["spec"].(map[string]any)["taints"] = []any{map[string]any{"key": "dedicated", "effect": "NoSchedule"}}
	nodes := []map[string]any{
		gpuNode("big", "nvidia-a100", 4, true),
		gpuNode("small", "nvidia-a100", 2, true),
		gpuNode("one", "nvidia-3090", 1, true),
		gpuNode("down", "nvidia-l4", 4, false),
		cordoned, foreignTaint,
		{"metadata": map[string]any{"name": "cpu", "labels": map[string]any{}}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}},
	}
	initPod := gpuPod("big", "nvidia-a100", 0, "Running")
	initPod["spec"].(map[string]any)["initContainers"] = []any{map[string]any{"resources": map[string]any{"requests": map[string]any{ResourceGPU: "1"}}}}
	pods := []map[string]any{
		gpuPod("big", "nvidia-a100", 1, "Running"),
		initPod,
		gpuPod("one", "nvidia-3090", 1, "Succeeded"), // finished: frees its GPU
		gpuPod("", "nvidia-3090", 0, "Pending"),      // no GPU
	}
	got := map[string]GPUPool{}
	for _, p := range poolGPUCapacity([]string{"nvidia-a100", "nvidia-3090", "nvidia-l4", "nvidia-a6000", "nvidia-h100"}, nodes, pods) {
		got[p.Model] = p
	}
	if p := got["nvidia-a100"]; p.Total != 6 || p.Free != 4 || p.MaxPerSandbox != 2 {
		t.Errorf("a100 = %+v, want total 6, free 4, max per sandbox 2 (2 free on each node)", p)
	}
	if p := got["nvidia-3090"]; p.MaxPerSandbox != 1 || p.Free != 1 {
		t.Errorf("3090 = %+v", p)
	}
	for _, m := range []string{"nvidia-l4", "nvidia-a6000", "nvidia-h100"} {
		if p := got[m]; p.MaxPerSandbox != 0 || p.Total != 0 {
			t.Errorf("%s must offer nothing (not ready / cordoned / untolerated taint / no node): %+v", m, p)
		}
	}
	// Terminating and unschedulable pods hold nothing.
	terminating := gpuPod("one", "nvidia-3090", 1, "Running")
	terminating["metadata"] = map[string]any{"deletionTimestamp": "2026-11-06T10:00:00Z"}
	stuck := gpuPod("", "nvidia-3090", 1, "Pending")
	stuck["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "PodScheduled", "status": "False", "reason": "Unschedulable"}}
	for _, p := range poolGPUCapacity([]string{"nvidia-3090"}, nodes, append(append([]map[string]any{}, pods...), terminating, stuck)) {
		if p.MaxPerSandbox != 1 {
			t.Errorf("terminating/unschedulable pods must not hold the 3090: %+v", p)
		}
	}
	// A pending GPU pod waiting for the 3090 takes the only free one.
	pods = append(pods, gpuPod("", "nvidia-3090", 1, "Pending"))
	for _, p := range poolGPUCapacity([]string{"nvidia-3090"}, nodes, pods) {
		if p.MaxPerSandbox != 0 {
			t.Errorf("3090 with a pending claim = %+v", p)
		}
	}
}

func TestGPUCapacityClampsAndChecks(t *testing.T) {
	f := newFake()
	f.nodes = []map[string]any{gpuNode("big", "nvidia-3090", 4, true)}
	s := newTestService(f) // MaxGPUs and MaxConcurrentGPUs default to 1
	pools, limit, err := s.GPUCapacity(context.Background(), "p1")
	if err != nil || limit || len(pools) == 0 || pools[0].MaxPerSandbox != 1 || pools[0].Free != 4 {
		t.Fatalf("clamped to MaxGPUs=1: %+v limit=%v err=%v", pools, limit, err)
	}
	if err := s.checkGPUCapacity(context.Background(), &GPURequest{Model: "nvidia-3090", Count: 5}); !errors.Is(err, ErrNoGPUCapacity) {
		t.Errorf("5 GPUs on a 4-GPU node: %v", err)
	}
	if err := s.checkGPUCapacity(context.Background(), &GPURequest{Model: "nvidia-3090", Count: 4}); err != nil {
		t.Errorf("4 GPUs fit: %v", err)
	}
	f.nodes = append(f.nodes, gpuNode("old", "nvidia-k80", 1, true)) // not offered any more
	if err := s.checkGPUCapacity(context.Background(), &GPURequest{Model: "nvidia-k80", Count: 1}); err != nil {
		t.Errorf("a resumed sandbox on a model no longer offered: %v", err)
	}
	f.extraPods = []map[string]any{gpuPod("big", "nvidia-3090", 4, "Running")}
	gpuPoolCache.Delete(s.serviceID)
	if err := s.checkGPUCapacity(context.Background(), &GPURequest{Model: "nvidia-3090", Count: 1}); !errors.Is(err, ErrNoGPUCapacity) {
		t.Errorf("all GPUs busy: %v", err)
	}
}

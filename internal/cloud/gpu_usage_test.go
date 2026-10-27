package cloud

import (
	"errors"
	"reflect"
	"testing"
)

func gpuTestResource(resourceType, status string, extraSpecs any) CloudResource {
	flavor := map[string]any{"extra_specs": extraSpecs}
	return CloudResource{Type: resourceType, Data: map[string]any{"server": map[string]any{
		"status": status,
		"flavor": flavor,
	}}}
}

func TestGPUUsageFromResources(t *testing.T) {
	resources := []CloudResource{
		gpuTestResource(TypeServer, "ACTIVE", map[string]any{"pci_passthrough:alias": "NVIDIA_A100:1"}),
		gpuTestResource(TypeServer, "ERROR", map[string]string{"pci_passthrough:alias": "nvidia-a100:2"}),
		gpuTestResource(TypeBaremetalServer, "SHUTOFF", map[string]any{"resources:VGPU": "2"}),
		gpuTestResource(TypeServer, "DELETED", map[string]any{"pci_passthrough:alias": "nvidia-a100:8"}),
		gpuTestResource(TypeServer, "deleted", nil),             // deleted rows never poison availability
		gpuTestResource(TypeServer, "ACTIVE", map[string]any{}), // complete CPU-only flavor
		{Type: TypeVolume, Data: map[string]any{}},
	}

	got, available := GPUUsageFromResources(resources)
	want := map[string]int{"nvidia-a100": 3, "vgpu": 2}
	if !available {
		t.Fatal("GPU usage should be available for complete flavor snapshots")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GPUUsageFromResources() = %#v, want %#v", got, want)
	}
}

func TestGPUUsageFromResourcesMarksIncompleteCacheUnavailable(t *testing.T) {
	resources := []CloudResource{
		gpuTestResource(TypeServer, "ACTIVE", map[string]any{"pci_passthrough:alias": "a6000:1"}),
		gpuTestResource(TypeServer, "ACTIVE", map[string]string(nil)),
		gpuTestResource(TypeServer, "ACTIVE", map[string]any(nil)),
		{Type: TypeServer, Data: map[string]any{"server": map[string]any{
			"status": "BUILD",
			"flavor": map[string]any{"id": "unresolved-flavor"},
		}}},
	}

	got, available := GPUUsageFromResources(resources)
	if available {
		t.Fatal("GPU usage must not present an incomplete cache as trustworthy")
	}
	if got["a6000"] != 1 {
		t.Fatalf("partial known usage = %#v, want a6000=1", got)
	}
}

func TestCollectGPUUsageKeepsPartialUsageWhenLaterReadFails(t *testing.T) {
	wantErr := errors.New("baremetal cache unavailable")
	got, err := collectGPUUsage([]string{TypeServer, TypeBaremetalServer}, func(resourceType string) ([]CloudResource, error) {
		if resourceType == TypeBaremetalServer {
			return nil, wantErr
		}
		return []CloudResource{
			gpuTestResource(TypeServer, "ACTIVE", map[string]any{"pci_passthrough:alias": "a6000:2"}),
		}, nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("collectGPUUsage() error = %v, want %v", err, wantErr)
	}
	if got["a6000"] != 2 {
		t.Fatalf("partial usage = %#v, want a6000=2", got)
	}
}

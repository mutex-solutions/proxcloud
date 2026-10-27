package project

import "testing"

func TestGPUCapacityEntryShape(t *testing.T) {
	e := gpuCapacityEntry{Model: "nvidia-4090", Available: 1, Total: 2}
	if e.Available > e.Total {
		t.Fatal("impossible capacity")
	}
}

package project

import (
	"reflect"
	"testing"
)

func TestGPUQuotaLimits(t *testing.T) {
	got := gpuQuotaLimits(map[string]any{
		"gpu": map[string]any{
			"nvidia-a6000": float64(4),
			"h100":         int64(2),
			"*":            8,
			"fractional":   1.5,
			"negative":     -1,
			"invalid":      "3",
		},
	})
	want := map[string]int{"nvidia-a6000": 4, "h100": 2, "*": 8}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gpuQuotaLimits() = %#v, want %#v", got, want)
	}
}

func TestGPUQuotaLimitsAlwaysReturnsObject(t *testing.T) {
	got := gpuQuotaLimits(nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("gpuQuotaLimits(nil) = %#v, want non-nil empty map", got)
	}
}

func TestGPUQuotaLimitsCanonicalizesLegacyAliases(t *testing.T) {
	got := gpuQuotaLimits(map[string]any{"gpu": map[string]any{
		"NVIDIA_A6000": float64(4),
		"nvidia-a6000": float64(2),
	}})
	want := map[string]int{"nvidia-a6000": 2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gpuQuotaLimits() = %#v, want canonical key to win: %#v", got, want)
	}
}

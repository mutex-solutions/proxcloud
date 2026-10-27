package project

import "testing"

func TestGpuLimitFor(t *testing.T) {
	cases := []struct {
		name    string
		quota   map[string]any
		model   string
		limit   int
		limited bool
	}{
		{"no gpu key", map[string]any{}, "a100-80gb", 0, false},
		{"exact model", map[string]any{"gpu": map[string]any{"a100-80gb": float64(4)}}, "a100-80gb", 4, true},
		{"wildcard fallback", map[string]any{"gpu": map[string]any{"*": float64(8)}}, "h100", 8, true},
		{"exact wins over wildcard", map[string]any{"gpu": map[string]any{"a100-80gb": float64(2), "*": float64(8)}}, "a100-80gb", 2, true},
		{"legacy alias is normalized", map[string]any{"gpu": map[string]any{"NVIDIA_A100_80GB": float64(3)}}, "nvidia-a100-80gb", 3, true},
		{"other model unlimited", map[string]any{"gpu": map[string]any{"a100-80gb": float64(2)}}, "h100", 0, false},
		{"zero limit is limited", map[string]any{"gpu": map[string]any{"h100": float64(0)}}, "h100", 0, true},
		{"gpu not an object", map[string]any{"gpu": "nope"}, "h100", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			limit, limited := gpuLimitFor(c.quota, c.model)
			if limit != c.limit || limited != c.limited {
				t.Fatalf("gpuLimitFor(%v,%q) = (%d,%v), want (%d,%v)", c.quota, c.model, limit, limited, c.limit, c.limited)
			}
		})
	}
}

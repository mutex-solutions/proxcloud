package admin

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

func TestValidateProjectQuota(t *testing.T) {
	cases := []struct {
		name string
		body pgdoc.M
		ok   bool
	}{
		{"empty", pgdoc.M{}, true},
		{"no gpu key", pgdoc.M{"cores": float64(10)}, true},
		{"valid gpu", pgdoc.M{"gpu": map[string]any{"a100-80gb": float64(4), "*": float64(8)}}, true},
		{"gpu not object", pgdoc.M{"gpu": "x"}, false},
		{"negative", pgdoc.M{"gpu": map[string]any{"h100": float64(-1)}}, false},
		{"non integer", pgdoc.M{"gpu": map[string]any{"h100": 1.5}}, false},
		{"non numeric", pgdoc.M{"gpu": map[string]any{"h100": "two"}}, false},
		{"empty model", pgdoc.M{"gpu": map[string]any{" ": float64(1)}}, false},
		{"canonical collision", pgdoc.M{"gpu": map[string]any{"NVIDIA_A100": float64(1), "nvidia-a100": float64(2)}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateProjectQuota(c.body)
			if (err == nil) != c.ok {
				t.Fatalf("validateProjectQuota(%v) err=%v, want ok=%v", c.body, err, c.ok)
			}
		})
	}
}

func TestNormalizeProjectQuotaCanonicalizesGPUModels(t *testing.T) {
	got, err := normalizeProjectQuota(pgdoc.M{
		"gpu": map[string]any{" NVIDIA_A100_80GB ": float64(4), "*": float64(8)},
	})
	if err != nil {
		t.Fatalf("normalizeProjectQuota() error = %v", err)
	}
	gpu := got["gpu"].(map[string]any)
	if gpu["nvidia-a100-80gb"] != float64(4) || gpu["*"] != float64(8) || len(gpu) != 2 {
		t.Fatalf("normalized GPU quota = %#v", gpu)
	}
}

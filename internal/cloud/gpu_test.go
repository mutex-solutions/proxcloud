package cloud

import "testing"

func TestGPUFromFlavor(t *testing.T) {
	cases := []struct {
		name  string
		es    any
		model string
		count int
	}{
		{"typed string map (live client)", map[string]string{"pci_passthrough:alias": "nvidia-a6000:2"}, "nvidia-a6000", 2},
		{"non-map input", "garbage", "", 0},
		{"nil specs", nil, "", 0},
		{"no gpu keys", map[string]any{"hw:cpu_policy": "dedicated"}, "", 0},
		{"single alias", map[string]any{"pci_passthrough:alias": "a100-80gb:2"}, "a100-80gb", 2},
		{"alias normalized", map[string]any{"pci_passthrough:alias": "A100_80GB:1"}, "a100-80gb", 1},
		{"alias with spaces", map[string]any{"pci_passthrough:alias": " h100 : 4 "}, "h100", 4},
		{"multi alias sums", map[string]any{"pci_passthrough:alias": "a100-80gb:2,a100-80gb:1"}, "a100-80gb", 3},
		{"malformed count ignored", map[string]any{"pci_passthrough:alias": "a100:x"}, "", 0},
		{"zero count ignored", map[string]any{"pci_passthrough:alias": "a100:0"}, "", 0},
		{"vgpu fallback", map[string]any{"resources:VGPU": "1"}, "vgpu", 1},
		{"alias wins over vgpu", map[string]any{"pci_passthrough:alias": "l40s:1", "resources:VGPU": "2"}, "l40s", 1},
		{"non-string value ignored", map[string]any{"pci_passthrough:alias": 3}, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			model, count := GPUFromFlavor(c.es)
			if model != c.model || count != c.count {
				t.Fatalf("GPUFromFlavor(%v) = (%q,%d), want (%q,%d)", c.es, model, count, c.model, c.count)
			}
		})
	}
}

func TestNormalizeGPUAlias(t *testing.T) {
	if got := NormalizeGPUAlias("A100_80GB"); got != "a100-80gb" {
		t.Fatalf("got %q", got)
	}
}

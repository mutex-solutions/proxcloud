package cloud

import (
	"strconv"
	"strings"
)

// GPUFromFlavor derives the GPU model alias + device count from a nova flavor's
// extra specs. Accepts either map shape — map[string]string straight off the cloud
// client, or map[string]any after the cloudResource cache round-trip — so no call
// site can silently miss GPUs on a type assertion.
//
// Primary source: `pci_passthrough:alias` = "<alias>:<count>[,<alias>:<count>…]" —
// model = first alias, count = total across entries.
// Fallback: `resources:VGPU` = "<count>" → model "vgpu".
//
// The Nova alias is normalized (lowercase, "_"→"-") so pricing, usage and project quota
// use one stable key. Placement trait suffixes are normalized the same way for suggestions,
// but operators must still configure the actual Nova alias when the two names differ.
func GPUFromFlavor(specs any) (model string, count int) {
	var extraSpecs map[string]any
	switch m := specs.(type) {
	case map[string]any:
		extraSpecs = m
	case map[string]string:
		extraSpecs = make(map[string]any, len(m))
		for k, v := range m {
			extraSpecs[k] = v
		}
	default:
		return "", 0
	}
	if alias, ok := extraSpecs["pci_passthrough:alias"].(string); ok && strings.TrimSpace(alias) != "" {
		for _, part := range strings.Split(alias, ",") {
			name, n, ok := strings.Cut(strings.TrimSpace(part), ":")
			if !ok {
				continue
			}
			c, err := strconv.Atoi(strings.TrimSpace(n))
			if err != nil || c <= 0 {
				continue
			}
			if model == "" {
				model = NormalizeGPUAlias(name)
			}
			count += c
		}
		if count > 0 {
			return model, count
		}
	}
	if vgpu, ok := extraSpecs["resources:VGPU"].(string); ok {
		if c, err := strconv.Atoi(strings.TrimSpace(vgpu)); err == nil && c > 0 {
			return "vgpu", c
		}
	}
	return "", 0
}

// NormalizeGPUAlias lowercases and dash-normalizes a GPU alias / trait suffix so
// "NVIDIA_A6000" (from trait CUSTOM_PCI_NVIDIA_A6000) and "nvidia-a6000" (pci alias)
// compare equal.
func NormalizeGPUAlias(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "_", "-")
}

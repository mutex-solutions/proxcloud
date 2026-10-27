package cloud

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrGPUUsageIncomplete means at least one cached, non-deleted compute resource
// did not carry flavor extra specs. Returning an explicit error prevents quota
// surfaces from presenting a partial GPU count as a trustworthy zero.
var ErrGPUUsageIncomplete = errors.New("GPU usage cache is incomplete")

// GPUUsageFromResources sums GPU devices by canonical model alias. The boolean
// is false when any relevant cached server lacks the flavor data needed to tell
// whether it consumes a GPU.
func GPUUsageFromResources(resources []CloudResource) (map[string]int, bool) {
	usage := map[string]int{}
	available := true

	for i := range resources {
		resource := resources[i]
		if resource.Type != TypeServer && resource.Type != TypeBaremetalServer {
			continue
		}
		server, ok := resource.Data["server"].(map[string]any)
		if !ok || server == nil {
			available = false
			continue
		}
		if status, _ := server["status"].(string); strings.EqualFold(status, "DELETED") {
			continue
		}

		flavor, ok := server["flavor"].(map[string]any)
		if !ok || flavor == nil {
			available = false
			continue
		}
		extraSpecs, present := flavor["extra_specs"]
		if !present || extraSpecs == nil {
			available = false
			continue
		}
		switch specs := extraSpecs.(type) {
		case map[string]any:
			if specs == nil {
				available = false
				continue
			}
		case map[string]string:
			if specs == nil {
				available = false
				continue
			}
		default:
			available = false
			continue
		}

		if model, count := GPUFromFlavor(extraSpecs); count > 0 {
			usage[model] += count
		}
	}

	return usage, available
}

// GPUUsageByProject reads the same cache used by quota enforcement and returns
// project-global usage across server types, services and regions.
func (r *Repo) GPUUsageByProject(ctx context.Context, projectID string) (map[string]int, error) {
	if r == nil {
		return map[string]int{}, fmt.Errorf("GPU usage repository is unavailable")
	}
	return collectGPUUsage([]string{TypeServer, TypeBaremetalServer}, func(resourceType string) ([]CloudResource, error) {
		return r.FindByProjectAndType(ctx, projectID, resourceType)
	})
}

func collectGPUUsage(
	resourceTypes []string,
	find func(resourceType string) ([]CloudResource, error),
) (map[string]int, error) {
	resources := []CloudResource{}
	for _, resourceType := range resourceTypes {
		rows, err := find(resourceType)
		if err != nil {
			usage, available := GPUUsageFromResources(resources)
			if !available {
				err = errors.Join(err, ErrGPUUsageIncomplete)
			}
			return usage, err
		}
		resources = append(resources, rows...)
	}
	usage, available := GPUUsageFromResources(resources)
	if !available {
		return usage, ErrGPUUsageIncomplete
	}
	return usage, nil
}

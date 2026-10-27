package project

// gpuquota.go — the tier-1 GPU quota gate. Per-project limits live on the project doc as
// quota.gpu = {"<model>": n, "*": n} (admin-managed via PUT /admin/project/{id}/quota;
// model names use the shared alias vocabulary, e.g. "a100-80gb"). Enforcement happens HERE,
// at the ProxCloud create/resize gate — nothing is pushed to OpenStack (nova legacy quotas
// have no GPU class). Horizon-direct usage on imported projects bypasses this gate (tier 2,
// operator-managed).

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// gpuLimitFor resolves the project limit for a GPU model: the exact model key, else the
// "*" wildcard, else unlimited (limited=false).
func gpuLimitFor(quota map[string]any, model string) (limit int, limited bool) {
	limits := gpuQuotaLimits(quota)
	if value, ok := limits[cloud.NormalizeGPUAlias(model)]; ok {
		return value, true
	}
	if value, ok := limits["*"]; ok {
		return value, true
	}
	return 0, false
}

// gpuUsageDetail sums the project's cached servers' GPU devices per model (DELETED excluded —
// everything else, including ERROR, still holds its devices until the server is gone). Keeping
// the repository error lets read surfaces warn instead of presenting a misleading zero; the
// create/resize gate intentionally retains its existing fail-open behavior through gpuUsage.
func (h *Handler) gpuUsageDetail(ctx context.Context, projectID string) (map[string]int, error) {
	if h.cloud == nil {
		return map[string]int{}, fmt.Errorf("GPU usage repository is unavailable")
	}
	return h.cloud.GPUUsageByProject(ctx, projectID)
}

func (h *Handler) gpuUsage(ctx context.Context, projectID string) map[string]int {
	usage, _ := h.gpuUsageDetail(ctx, projectID)
	return usage
}

// enforceGPUQuotaForNodeGroups is the same tier-1 gate for kamaji node groups: without it, a
// user blocked from creating a GPU server could still request GPU worker VMs through a node
// group and have CAPO create them in the tenant, bypassing the quota entirely. Demand per GPU
// model = Σ nodes × flavor devices, where nodes = count, or MAX for an autoscale group (the
// autoscaler can reach max with no further gate). prevGroups (the cluster's current groups, on
// a node-group edit) subtract out — their workers are already counted in gpuUsage once the
// server sync picks them up. osSvcID is the project's OPENSTACK binding (where the worker VMs
// and flavors live), not the kamaji service. Fail-open like enforceGPUQuota.
func (h *Handler) enforceGPUQuotaForNodeGroups(ctx context.Context, proj *Project, svcID string, groups, prevGroups []kamaji.NodeGroup) error {
	if len(proj.Quota) == 0 || len(groups) == 0 || h.pveFlavors == nil {
		return nil
	}
	gpuOf := func(flavorID string) (string, int) {
		fl, err := h.pveFlavors.Get(ctx, flavorID)
		if err != nil || fl == nil || fl.GPUCount == 0 {
			return "", 0
		}
		return cloud.NormalizeGPUAlias(fl.GPUModel), fl.GPUCount
	}
	nodes := func(ng kamaji.NodeGroup) int {
		if ng.Autoscale {
			return ng.Max
		}
		return ng.Count
	}
	demand := map[string]int{}
	for _, ng := range groups {
		if model, n := gpuOf(ng.FlavorID); n > 0 {
			demand[model] += n * nodes(ng)
		}
	}
	for _, ng := range prevGroups {
		if model, n := gpuOf(ng.FlavorID); n > 0 {
			demand[model] -= n * nodes(ng)
		}
	}
	usage := h.gpuUsage(ctx, proj.ID)
	for model, want := range demand {
		if want <= 0 {
			continue
		}
		limit, limited := gpuLimitFor(proj.Quota, model)
		if !limited {
			continue
		}
		if usage[model]+want > limit {
			return httpx.NewError(http.StatusConflict, http.StatusConflict, fmt.Sprintf(
				"GPU quota exceeded for %s: %d in use + %d requested exceeds the project limit of %d",
				model, usage[model], want, limit))
		}
	}
	return nil
}

func gpuQuotaLimits(quota map[string]any) map[string]int {
	out := map[string]int{}
	sources := map[string]string{}
	gpu, ok := quota["gpu"].(map[string]any)
	if !ok {
		return out
	}
	for model, raw := range gpu {
		trimmed := strings.TrimSpace(model)
		canonical := cloud.NormalizeGPUAlias(trimmed)
		if trimmed == "*" {
			canonical = "*"
		}
		if canonical == "" {
			continue
		}
		value, ok := toDecAny(raw)
		if !ok || value.IsNegative() || !value.Equal(value.Truncate(0)) {
			continue
		}
		previous, exists := sources[canonical]
		canonicalSource := trimmed == canonical
		previousCanonical := previous == canonical
		if exists && !canonicalSource && (previousCanonical || trimmed >= previous) {
			continue
		}
		out[canonical] = int(value.IntPart())
		sources[canonical] = trimmed
	}
	return out
}

package project

// quota_usage.go — GET /project/{id}/quota-usage. The live OpenStack compute/
// storage quota legs died with that backend; on this fork every quota is
// platform-managed (Project.Quota), so the endpoint reports the ProxCloud limits +
// cached-row usage. The response keeps the old shape (compute/storage stay
// absent — the FE already treats them as optional legs).

import (
	"net/http"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

type gpuQuotaUsage struct {
	Limits         map[string]int `json:"limits"`
	Usage          map[string]int `json:"usage"`
	UsageAvailable bool           `json:"usageAvailable"`
}

type projectQuotaUsage struct {
	ServiceID string         `json:"serviceId"`
	Region    string         `json:"region"`
	GPU       gpuQuotaUsage  `json:"gpu"`
	Limits    map[string]any `json:"limits,omitempty"` // platform quota keys (vcpus/ramMb/servers/networks/diskGb)
	Warnings  []string       `json:"warnings"`
}

// quotaUsage reports the ProxCloud-managed quota (GPU + resource keys) with usage
// from the cached rows.
func (h *Handler) quotaUsage(w http.ResponseWriter, r *http.Request) {
	_, proj, ok := h.resolveForMember(w, r)
	if !ok {
		return
	}
	svcID := h.resolveServiceID(r, proj)
	out := projectQuotaUsage{
		ServiceID: svcID,
		Region:    proj.ServiceRegion(svcID),
		GPU: gpuQuotaUsage{
			Limits: gpuQuotaLimits(proj.Quota),
			Usage:  map[string]int{},
		},
		Warnings: []string{},
	}
	if len(proj.Quota) > 0 {
		out.Limits = proj.Quota
	}
	if usage, err := h.cloud.GPUUsageByProject(r.Context(), proj.ID); err == nil {
		out.GPU.Usage = usage
		out.GPU.UsageAvailable = true
	} else {
		out.Warnings = append(out.Warnings, "GPU usage is temporarily unavailable")
	}
	httpx.OK(w, out)
}

// serverGPUDemand re-exports the cached-row GPU accounting for the create-path
// pre-checks (kept — the FE wizard calls quota-usage before submit).
var _ = cloud.GPUFromFlavor

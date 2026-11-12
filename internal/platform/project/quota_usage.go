package project

// quota_usage.go — GET /project/{id}/quota-usage. Every quota is
// platform-managed (Project.Quota), so the endpoint reports the ProxCloud limits +
// cached-row usage. A provider without compute (ceph-s3, kamaji, dbaas) leaves the
// compute and storage legs absent.

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
	Limits    map[string]any `json:"limits,omitempty"` // platform quota keys (vcpus/ramMb/servers/networks/diskGb/floatingIps)
	Compute   *computeQuota  `json:"compute,omitempty"`
	Storage   *storageQuota  `json:"storage,omitempty"`
	Warnings  []string       `json:"warnings"`
}

// quotaMetric is one limit with its usage; a negative limit means unlimited.
type quotaMetric struct {
	Used     int `json:"used"`
	Reserved int `json:"reserved"`
	Limit    int `json:"limit"`
}

type computeQuota struct {
	Instances quotaMetric `json:"instances"`
	Cores     quotaMetric `json:"cores"`
	RAMMb     quotaMetric `json:"ramMb"`
}

type storageQuota struct {
	Gigabytes quotaMetric `json:"gigabytes"`
}

// pveQuotaMetrics lays Proxmox limits and usage out in the console's quota shape.
func pveQuotaMetrics(limits map[string]any, u pveQuotaDemand) (*computeQuota, *storageQuota) {
	metric := func(key string, used int) quotaMetric {
		limit := -1
		if raw, ok := limits[key]; ok {
			limit = intAny(raw)
		}
		return quotaMetric{Used: used, Limit: limit}
	}
	return &computeQuota{
		Instances: metric("servers", u.Servers),
		Cores:     metric("vcpus", u.VCPUs),
		RAMMb:     metric("ramMb", u.RAMMb),
	}, &storageQuota{
		Gigabytes: metric("diskGb", u.DiskGb),
	}
}

// quotaUsage reports the ProxCloud-managed quota (GPU + resource keys) with usage
// from the cached rows.
func (h *Handler) quotaUsage(w http.ResponseWriter, r *http.Request) {
	_, proj, ok := h.resolveForMember(w, r)
	if !ok {
		return
	}
	// An explicit service the project is not attached to is refused (#215), not answered with the
	// default provider's quota.
	svcID, err := h.resolveServiceID(r, proj)
	if err != nil {
		h.fail(w, err)
		return
	}
	// The limits the gate enforces: a Proxmox project without its own quota falls
	// back to the provider default (pveQuotaLimits).
	limits := proj.Quota
	isProxmox := false
	if svcID != "" && h.esSvc != nil {
		if es, err := h.esSvc.Get(r.Context(), svcID); err == nil && es.IsProxmox() {
			isProxmox = true
			if len(limits) == 0 {
				limits = pveQuotaLimits(proj, es)
			}
		}
	}
	out := projectQuotaUsage{
		ServiceID: svcID,
		Region:    proj.ServiceRegion(svcID),
		GPU: gpuQuotaUsage{
			Limits: gpuQuotaLimits(limits),
			Usage:  map[string]int{},
		},
		Warnings: []string{},
	}
	if len(limits) > 0 {
		out.Limits = limits
	}
	if isProxmox {
		if u, err := h.pveQuotaUsage(r.Context(), proj.ID, svcID); err == nil {
			out.Compute, out.Storage = pveQuotaMetrics(limits, u)
		} else {
			out.Warnings = append(out.Warnings, "Quota usage is temporarily unavailable")
		}
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

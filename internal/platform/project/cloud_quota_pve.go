package project

// cloud_quota_pve.go — platform-side resource quota for Proxmox projects (PVE has
// no quota machinery at all). Project.Quota JSONB keys: vcpus, ramMb, servers,
// networks, diskGb (+ the shared "gpu" map). Absent key = unlimited — the
// fail-open posture of the GPU gate; the gate only ever 409s on a hard exceed.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// pveQuotaDemand is what one create/resize adds.
type pveQuotaDemand struct {
	VCPUs    int
	RAMMb    int
	Servers  int
	Networks int
	DiskGb   int
}

// enforcePVEQuota gates a demand against Project.Quota using the cached rows as
// usage (DELETED rows are already excluded from the cache).
func (h *Handler) enforcePVEQuota(ctx context.Context, proj *Project, svcID string, d pveQuotaDemand) *httpx.HTTPError {
	limits := proj.Quota
	if len(limits) == 0 {
		return nil
	}
	limit := func(key string) (int, bool) {
		v, ok := limits[key]
		if !ok {
			return 0, false
		}
		return intAny(v), true
	}
	needsUsage := false
	for _, k := range []string{"vcpus", "ramMb", "servers", "networks", "diskGb"} {
		if _, ok := limit(k); ok {
			needsUsage = true
			break
		}
	}
	if !needsUsage {
		return nil
	}
	rows, err := h.cloud.FindAllByProjectID(ctx, proj.ID)
	if err != nil {
		return nil // fail-open: a DB hiccup never blocks (GPU-gate posture)
	}
	var usedVCPUs, usedRAM, usedServers, usedNetworks, usedDisk int
	for i := range rows {
		if rows[i].ServiceID != svcID {
			continue
		}
		switch rows[i].Type {
		case cloud.TypeServer:
			usedServers++
			if srv, ok := rows[i].Data["server"].(map[string]any); ok {
				if fl, ok := srv["flavor"].(map[string]any); ok {
					usedVCPUs += intAny(fl["vcpus"])
					usedRAM += intAny(fl["ram"])
				}
			}
		case cloud.TypeNetwork:
			usedNetworks++
		case cloud.TypeVolume:
			if vol, ok := rows[i].Data["volume"].(map[string]any); ok {
				usedDisk += intAny(vol["size"])
			}
		}
	}
	check := func(key string, used, want int) *httpx.HTTPError {
		lim, ok := limit(key)
		if !ok || want == 0 {
			return nil
		}
		if used+want > lim {
			return httpx.NewError(http.StatusConflict, http.StatusConflict,
				fmt.Sprintf("Quota exceeded for %s: %d in use, %d requested, limit %d", key, used, want, lim))
		}
		return nil
	}
	if e := check("vcpus", usedVCPUs, d.VCPUs); e != nil {
		return e
	}
	if e := check("ramMb", usedRAM, d.RAMMb); e != nil {
		return e
	}
	if e := check("servers", usedServers, d.Servers); e != nil {
		return e
	}
	if e := check("networks", usedNetworks, d.Networks); e != nil {
		return e
	}
	return check("diskGb", usedDisk, d.DiskGb)
}

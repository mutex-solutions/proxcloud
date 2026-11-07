package project

// cloud_quota_pve.go — platform-side resource quota for Proxmox projects (PVE has
// no quota machinery at all). Keys: externalservice.PVEQuotaKeys (servers, vcpus,
// ramMb, diskGb, networks, floatingIps) + the shared "gpu" map.
//
// Which limits apply (pveQuotaLimits): the project's own quota when it has one, else
// the provider default (config.provisioning.quota, also seeded onto the project at
// bootstrap). Unlimited only when neither is set; inside a non-empty quota an absent
// key is unlimited. A usage read that fails blocks the create (503) — never a silent
// pass.

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// pveQuotaDemand is what one create/resize adds (or, as usage, what is in use).
type pveQuotaDemand struct {
	VCPUs    int
	RAMMb    int
	Servers  int
	Networks int
	DiskGb   int
	// FloatingIPs counts public-NIC addresses (pvePublicIp rows, not cache rows).
	FloatingIPs int
}

func errQuotaUsageUnavailable() *httpx.HTTPError {
	return httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable,
		"Quota usage is temporarily unavailable, please try again")
}

// pveQuotaLimits is the quota that applies to proj on es (see the file comment).
func pveQuotaLimits(proj *Project, es *externalservice.ExternalService) map[string]any {
	if len(proj.Quota) > 0 {
		return proj.Quota
	}
	return es.PVEDefaultQuota()
}

// enforcePVEQuota gates a demand against the project's limits using the cached rows
// as usage (DELETED rows are already excluded from the cache).
func (h *Handler) enforcePVEQuota(ctx context.Context, proj *Project, es *externalservice.ExternalService, d pveQuotaDemand) *httpx.HTTPError {
	return checkPVEQuota(pveQuotaLimits(proj, es), d,
		func() (pveQuotaDemand, error) { return h.pveQuotaUsage(ctx, proj.ID, es.ID) },
		func() (int, error) {
			if h.publicIPs == nil {
				return 0, nil
			}
			n, err := h.publicIPs.CountForProject(ctx, es.ID, proj.ID)
			return int(n), err
		})
}

// byKey maps the demand onto the quota keys.
func (d pveQuotaDemand) byKey() map[string]int {
	return map[string]int{"servers": d.Servers, "vcpus": d.VCPUs, "ramMb": d.RAMMb,
		"diskGb": d.DiskGb, "networks": d.Networks, "floatingIps": d.FloatingIPs}
}

// checkPVEQuota is the gate itself. usage (cached rows) and floatingUsed (pvePublicIp
// rows) are only read when a limited key is actually requested; a read error fails
// closed.
func checkPVEQuota(limits map[string]any, d pveQuotaDemand, usage func() (pveQuotaDemand, error), floatingUsed func() (int, error)) *httpx.HTTPError {
	want := d.byKey()
	var used map[string]int
	for _, key := range externalservice.PVEQuotaKeys {
		raw, limited := limits[key]
		if !limited || want[key] <= 0 {
			continue
		}
		var n int
		if key == "floatingIps" {
			var err error
			if n, err = floatingUsed(); err != nil {
				return errQuotaUsageUnavailable()
			}
		} else {
			if used == nil {
				u, err := usage()
				if err != nil {
					return errQuotaUsageUnavailable()
				}
				used = u.byKey()
			}
			n = used[key]
		}
		if lim := intAny(raw); n+want[key] > lim {
			return httpx.NewError(http.StatusConflict, http.StatusConflict,
				fmt.Sprintf("Quota exceeded for %s: %d in use, %d requested, limit %d", key, n, want[key], lim))
		}
	}
	return nil
}

// pveQuotaUsage sums the project's cached rows on one provider.
func (h *Handler) pveQuotaUsage(ctx context.Context, projectID, svcID string) (pveQuotaDemand, error) {
	rows, err := h.cloud.FindAllByProjectID(ctx, projectID)
	if err != nil {
		return pveQuotaDemand{}, err
	}
	return pveUsageOf(rows, svcID), nil
}

func pveUsageOf(rows []cloud.CloudResource, svcID string) pveQuotaDemand {
	var u pveQuotaDemand
	for i := range rows {
		if rows[i].ServiceID != svcID {
			continue
		}
		switch rows[i].Type {
		case cloud.TypeServer:
			u.Servers++
			if srv, ok := rows[i].Data["server"].(map[string]any); ok {
				if fl, ok := srv["flavor"].(map[string]any); ok {
					u.VCPUs += intAny(fl["vcpus"])
					u.RAMMb += intAny(fl["ram"])
				}
			}
		case cloud.TypeNetwork:
			u.Networks++
		case cloud.TypeVolume:
			if vol, ok := rows[i].Data["volume"].(map[string]any); ok {
				u.DiskGb += intAny(vol["size"])
			}
		case cloud.TypeImage:
			// A project template is a disk on the provider's storage. Its size is in bytes.
			if img, ok := rows[i].Data["image"].(map[string]any); ok {
				u.DiskGb += gibUp(int64(intAny(img["size"])))
			}
		}
	}
	return u
}

// CSIDiskQuota gates disk the CSI gateway is about to create or grow for a tenant cluster's volume, against the
// project's diskGb. The gateway's registry is not in the cache, so csiUsed adds its volumes to the cached usage.
func (h *Handler) CSIDiskQuota(ctx context.Context, projectID, serviceID string, addGiB int, csiUsed func() (int, error)) error {
	proj, err := h.svc.GetProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	es, err := h.esSvc.Get(ctx, serviceID)
	if err != nil {
		return err
	}
	usage := func() (pveQuotaDemand, error) {
		rows, err := h.cloud.FindAllByProjectID(ctx, proj.ID)
		if err != nil {
			return pveQuotaDemand{}, err
		}
		// The registry counts the cluster disks itself, creating and just-grown ones too: not their cached rows again.
		rows = slices.DeleteFunc(rows, func(r cloud.CloudResource) bool { return pve.CSIClusterOf(r.Data) != "" })
		u := pveUsageOf(rows, es.ID)
		n, err := csiUsed()
		u.DiskGb += n
		return u, err
	}
	if herr := checkPVEQuota(pveQuotaLimits(proj, es), pveQuotaDemand{DiskGb: addGiB}, usage, func() (int, error) { return 0, nil }); herr != nil {
		return herr
	}
	return nil
}

// csiVolumeGuard refuses a portal write on a tenant cluster's disk: the cluster owns it, through its PersistentVolume.
func csiVolumeGuard(cr *cloud.CloudResource) *httpx.HTTPError {
	if id := pve.CSIClusterOf(cr.Data); id != "" {
		return httpx.NewError(http.StatusConflict, http.StatusConflict,
			"This volume belongs to Kubernetes cluster "+id+". Manage it through its PersistentVolumeClaim")
	}
	return nil
}

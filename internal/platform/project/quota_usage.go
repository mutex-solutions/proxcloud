package project

// quota_usage.go — GET /project/{id}/quota-usage. Every quota is
// platform-managed (Project.Quota), so the endpoint reports the ProxCloud limits +
// cached-row usage. A provider without compute (ceph-s3, kamaji, dbaas) leaves the
// compute and storage legs absent.

import (
	"context"
	"net/http"
	"net/netip"
	"slices"
	"time"

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
	Network   *networkQuota  `json:"network,omitempty"`
	// Functions is the project's function quota on a sandbox location and what its functions reserve of it
	// (functionQuotaUsage); absent on any other service.
	Functions map[string]any `json:"functions,omitempty"`
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

// networkQuota is the network leg: the networks, and the public addresses with what holds each one. A tenant at the
// floatingIps limit was told "5 in use" and had no list of the five (drill Q0b-2 of v0.11.1): a balancer's address,
// each Service address of a cluster and each address a cluster keeps count as a server's public NIC does, and none of
// them is a server's.
type networkQuota struct {
	Networks    quotaMetric    `json:"networks"`
	FloatingIPs quotaMetric    `json:"floatingIps"`
	Addresses   []quotaAddress `json:"addresses"`
}

// quotaAddress is one public address the floatingIps quota counts.
type quotaAddress struct {
	IP        string `json:"ip"`
	NetworkID string `json:"networkId"`
	// Kind: "server" (a server's public NIC), "loadBalancer" (a balancer's own address), "service" (a Service of a
	// Kubernetes cluster), "kept" (an address a cluster keeps after its Service went), "releasing" (a Service's
	// address on its way back to the pool: it counts until the appliances have let go of it).
	Kind string `json:"kind"`
	// Name: the server's or the balancer's name, a Service's namespace/name.
	Name string `json:"name,omitempty"`
	// ResourceID: the server's or the balancer's id in this project's lists.
	ResourceID  string     `json:"resourceId,omitempty"`
	Cluster     string     `json:"cluster,omitempty"` // of a Service's, a kept or a releasing address: the cluster's id
	ClusterName string     `json:"clusterName,omitempty"`
	Since       *time.Time `json:"since,omitempty"`
}

// quotaAddresses names what holds each of allocs, from the project's cached rows. allocs are the rows the quota
// gate counts (PublicIPRepo.PublicForProject), so the list is as long as the count.
func quotaAddresses(rows []cloud.CloudResource, svcID string, allocs []PublicIP) []quotaAddress {
	type balancer struct {
		lb  *loadBalancer
		ext string
	}
	var lbs []balancer
	servers, clusters := map[string]string{}, map[string]string{}
	for i := range rows {
		cr := &rows[i]
		switch {
		case cr.Type == cloud.TypeKubernetesCluster: // a row of the Kubernetes provider, named by its balancer
			cl, _ := cr.Data["cluster"].(map[string]any)
			clusters[cr.ExternalID] = strAny(cl["name"])
		case cr.ServiceID != svcID:
		case cr.Type == cloud.TypeLoadBalancer:
			if lb, err := lbOfRow(cr); err == nil {
				lbs = append(lbs, balancer{lb, cr.ExternalID})
			}
		case cr.Type == cloud.TypeServer:
			srv, _ := cr.Data["server"].(map[string]any)
			servers[cr.ExternalID] = strAny(srv["name"])
		}
	}
	out := make([]quotaAddress, 0, len(allocs))
	for _, a := range allocs {
		q := quotaAddress{IP: a.IP, NetworkID: a.NetworkID, Kind: "server", Name: servers[a.ServerID], ResourceID: a.ServerID, Since: a.CreatedAt}
		if i := slices.IndexFunc(lbs, func(b balancer) bool { return b.lb.owns(a.ServerID) }); i >= 0 {
			lb := lbs[i].lb
			q.Kind, q.Name, q.ResourceID = "loadBalancer", lb.Name, lbs[i].ext
			if c := lb.Cluster; c != nil {
				q.Cluster, q.ClusterName = c.ID, clusters[c.ID]
				if j := slices.IndexFunc(c.Services, func(s lbClusterSvc) bool { return s.VIP == a.IP }); j >= 0 {
					q.Kind, q.Name = "service", c.Services[j].Namespace+"/"+c.Services[j].Name
				} else if slices.Contains(c.Kept, a.IP) {
					q.Kind, q.Name = "kept", ""
				} else if slices.Contains(c.Releasing, a.IP) {
					q.Kind, q.Name = "releasing", ""
				} // else an address allocated a moment ago, before the row names its Service: the balancer's
			}
		}
		out = append(out, q)
	}
	slices.SortFunc(out, func(x, y quotaAddress) int {
		ax, _ := netip.ParseAddr(x.IP)
		ay, _ := netip.ParseAddr(y.IP)
		return ax.Compare(ay)
	})
	return out
}

// quotaMetricOf is one key's usage against its limit; a key without a limit is unlimited.
func quotaMetricOf(limits map[string]any, key string, used int) quotaMetric {
	limit := -1
	if raw, ok := limits[key]; ok {
		limit = intAny(raw)
	}
	return quotaMetric{Used: used, Limit: limit}
}

// pveNetworkQuota is the network leg of a Proxmox project: the same count of public addresses the quota gate
// uses (enforcePVEQuota), and the addresses themselves.
func (h *Handler) pveNetworkQuota(ctx context.Context, projectID, svcID string, limits map[string]any, rows []cloud.CloudResource) (*networkQuota, error) {
	var allocs []PublicIP
	if h.publicIPs != nil { // without the store no public NIC can attach: none is in use
		var err error
		if allocs, err = h.publicIPs.PublicForProject(ctx, svcID, projectID); err != nil {
			return nil, err
		}
	}
	return &networkQuota{
		Networks:    quotaMetricOf(limits, "networks", pveUsageOf(rows, svcID).Networks),
		FloatingIPs: quotaMetricOf(limits, "floatingIps", len(allocs)),
		Addresses:   quotaAddresses(rows, svcID, allocs),
	}, nil
}

// pveQuotaMetrics lays Proxmox limits and usage out in the console's quota shape.
func pveQuotaMetrics(limits map[string]any, u pveQuotaDemand) (*computeQuota, *storageQuota) {
	metric := func(key string, used int) quotaMetric { return quotaMetricOf(limits, key, used) }
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
		if rows, err := h.cloud.FindAllByProjectID(r.Context(), proj.ID); err == nil {
			out.Compute, out.Storage = pveQuotaMetrics(limits, pveUsageOf(rows, svcID))
			if out.Network, err = h.pveNetworkQuota(r.Context(), proj.ID, svcID, limits, rows); err != nil {
				out.Warnings = append(out.Warnings, "Public address usage is temporarily unavailable")
			}
		} else {
			out.Warnings = append(out.Warnings, "Quota usage is temporarily unavailable")
		}
	}
	out.Functions = h.functionQuotaUsage(r.Context(), proj, svcID)
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

package project

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// resolveForMember loads the member-scoped project (404 otherwise) + the caller, for the
// cloud read endpoints (resolves project + user + membership).
func (h *Handler) resolveForMember(w http.ResponseWriter, r *http.Request) (*user.User, *Project, bool) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		h.fail(w, err)
		return nil, nil, false
	}
	p, err := h.svc.GetProject(r.Context(), u.Sub, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return nil, nil, false
	}
	return u, p, true
}

// resourceCounts returns per-type cloud-resource counts + TOTAL.
// Empty project → {"TOTAL":0}. Reads the cloudResource cache (not the live cloud).
func (h *Handler) resourceCounts(w http.ResponseWriter, r *http.Request) {
	_, p, ok := h.resolveForMember(w, r)
	if !ok {
		return
	}
	counts, err := h.cloud.CountByType(r.Context(), p.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.OK(w, counts)
}

// ProjectStats has 30 long fields (all emitted, 0 for a fresh project).
// Cloud-typed counts from the project's resources, identity counts from the
// user's, unpaidBills from SENT bills. edges/hostingAccounts have no backing type → 0.
type ProjectStats struct {
	Servers                int64 `json:"servers"`
	Volumes                int64 `json:"volumes"`
	Zones                  int64 `json:"zones"`
	SSHKeys                int64 `json:"sshKeys"`
	Edges                  int64 `json:"edges"`
	Images                 int64 `json:"images"`
	HostingAccounts        int64 `json:"hostingAccounts"`
	Networks               int64 `json:"networks"`
	FloatingIPs            int64 `json:"floatingIps"`
	Routers                int64 `json:"routers"`
	Clusters               int64 `json:"clusters"`
	Ports                  int64 `json:"ports"`
	SecurityGroups         int64 `json:"securityGroups"`
	Subnets                int64 `json:"subnets"`
	LoadBalancers          int64 `json:"loadBalancers"`
	ObjectStorages         int64 `json:"objectStorages"`
	UnpaidBills            int64 `json:"unpaidBills"`
	ApplicationCredentials int64 `json:"applicationCredentials"`
	Credentials            int64 `json:"credentials"`
	Users                  int64 `json:"users"`
	Stacks                 int64 `json:"stacks"`
	ShareGroups            int64 `json:"shareGroups"`
	ShareGroupSnapshots    int64 `json:"shareGroupSnapshots"`
	Shares                 int64 `json:"shares"`
	ShareSnapshots         int64 `json:"shareSnapshots"`
	SecurityServices       int64 `json:"securityServices"`
	ShareNetworks          int64 `json:"shareNetworks"`
	VolumeSnapshots        int64 `json:"volumeSnapshots"`
	VolumeBackups          int64 `json:"volumeBackups"`
	BareMetalServers       int64 `json:"bareMetalServers"`
}

// resourceStats returns the project's ProjectStats.
func (h *Handler) resourceStats(w http.ResponseWriter, r *http.Request) {
	u, p, ok := h.resolveForMember(w, r)
	if !ok {
		return
	}
	proj, err := h.cloud.CountsForProject(r.Context(), p.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	idn, err := h.cloud.CountsForUser(r.Context(), u.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	bpID := p.BillingProfileID
	if bpID == "" {
		o, err := h.orgSvc.FindOrganization(r.Context(), p.OrganizationID)
		if err != nil {
			h.fail(w, err)
			return
		}
		if o != nil {
			bpID = o.BillingProfileID
		}
	}
	var unpaid int64
	if bpID != "" {
		if unpaid, err = h.billing.CountSentBills(r.Context(), bpID); err != nil {
			h.fail(w, err)
			return
		}
	}
	httpx.OK(w, ProjectStats{
		Servers:                proj[cloud.TypeServer],
		Volumes:                proj[cloud.TypeVolume],
		Zones:                  proj[cloud.TypeDNSZone],
		Images:                 proj[cloud.TypeImage],
		Clusters:               proj[cloud.TypeKubernetesCluster],
		Networks:               proj[cloud.TypeNetwork],
		LoadBalancers:          proj[cloud.TypeLoadBalancer],
		FloatingIPs:            proj[cloud.TypeFloatingIP],
		Routers:                proj[cloud.TypeRouter],
		Ports:                  proj[cloud.TypePort],
		SecurityGroups:         proj[cloud.TypeSecurityGroup],
		Subnets:                proj[cloud.TypeSubnet],
		ObjectStorages:         proj[cloud.TypeBucket],
		Stacks:                 proj[cloud.TypeStack],
		Shares:                 proj[cloud.TypeShare],
		ShareGroups:            proj[cloud.TypeShareGroup],
		ShareNetworks:          proj[cloud.TypeShareNetwork],
		ShareGroupSnapshots:    proj[cloud.TypeShareSnapshotGroup],
		ShareSnapshots:         proj[cloud.TypeShareSnapshot],
		SecurityServices:       proj[cloud.TypeShareSecurityService],
		VolumeBackups:          proj[cloud.TypeVolumeBackup],
		VolumeSnapshots:        proj[cloud.TypeVolumeSnapshot],
		BareMetalServers:       proj[cloud.TypeBaremetalServer],
		SSHKeys:                idn[cloud.TypeKeypair],
		ApplicationCredentials: idn[cloud.TypeApplicationCred],
		Users:                  idn[cloud.TypeUser],
		Credentials:            idn[cloud.TypeCredential],
		UnpaidBills:            unpaid,
	})
}

// serviceResourceTypes maps a service name (externalService.config.services key) to the
// CloudResourceTypes its provider serves — the basis for a location's resourceTypes list.
var serviceResourceTypes = map[string][]string{
	"compute":      {cloud.TypeServer, cloud.TypeBaremetalServer, cloud.TypeKeypair, cloud.TypeServerGroup},
	"network":      {cloud.TypeNetwork, cloud.TypeSubnet, cloud.TypeRouter, cloud.TypePort, cloud.TypeFloatingIP, cloud.TypeSecurityGroup},
	"volumev3":     {cloud.TypeVolume, cloud.TypeVolumeSnapshot, cloud.TypeVolumeBackup},
	"image":        {cloud.TypeImage},
	"kubernetes":   {cloud.TypeKubernetesCluster}, // Kamaji managed k8s (the kamaji provider's only slug)
	"database":     {cloud.TypeDatabaseCluster},   // Managed databases (the dbaas provider's only slug)
	"object-store": {cloud.TypeBucket},
}

// resourceTypes lists the resource types available per location for the project:
// one LocationResourceType per (attached CLOUD service × region) carrying the CloudResourceTypes
// whose provider is enabled for that service+region. The create-resource wizard's Location dropdown
// reads this and filters to the locations whose resourceTypes include the type being created (the FE
// shows "no available locations" when the filtered list is empty). Only for an ACTIVE project.
func (h *Handler) resourceTypes(w http.ResponseWriter, r *http.Request) {
	_, p, ok := h.resolveForMember(w, r)
	if !ok {
		return
	}
	if p.Status != "ENABLED" || h.esSvc == nil {
		httpx.List(w, []any{})
		return
	}
	out := []map[string]any{}
	for _, svcID := range p.ServiceIDs() {
		es, err := h.esSvc.Get(r.Context(), svcID)
		if err != nil || es == nil || es.IsDisabled() {
			continue
		}
		services, _ := es.Config["services"].(map[string]any)
		regions, _ := es.Config["regions"].(map[string]any)
		// A ceph-s3 provider carries a single config.region (its RGW zonegroup) instead of a
		// regions map. Without this synthetic entry it contributes NO location, and the client's cloud
		// scope (x-service-id / x-region-id) is never resolved — the whole Object storage page stays empty.
		if len(regions) == 0 && es.IsCephS3() {
			if r := es.CephRegion(); r != "" {
				regions = map[string]any{r: map[string]any{}}
			}
		}
		for regionName, rcfgAny := range regions {
			rcfg, _ := rcfgAny.(map[string]any)
			seen := map[string]bool{}
			types := []string{}
			for svcName, regMapAny := range services {
				regMap, _ := regMapAny.(map[string]any)
				if en, _ := regMap[regionName].(bool); !en {
					continue
				}
				for _, t := range serviceResourceTypes[svcName] {
					if !seen[t] {
						seen[t] = true
						types = append(types, t)
					}
				}
			}
			if len(types) == 0 {
				continue
			}
			name, _ := rcfg["name"].(string)
			if name == "" {
				name = regionName
			}
			country, _ := rcfg["country"].(string)
			displayName, _ := rcfg["displayName"].(string)
			out = append(out, map[string]any{
				"name": name, "country": country, "displayName": displayName,
				"serviceName": es.Name, "serviceId": es.ID, "region": regionName,
				// provider ("proxmox" | "ceph-s3" | …) lets the create form label which backend a
				// location belongs to.
				"provider":      es.Provider(),
				"resourceTypes": types,
			})
		}
	}
	httpx.List(w, out)
}

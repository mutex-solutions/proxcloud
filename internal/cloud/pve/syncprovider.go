package pve

// syncprovider.go — the periodic-sync providers for one (project, PVE service):
// SERVER (qemu VMs in the project pool), VOLUME (their disks), IMAGE (project
// templates). Each satisfies providers.Provider + ProjectScoped implicitly, with
// no import of the providers package (dbaas precedent — dependency arrow pve→cloud).
//
// Tenancy: every List filters by the project's pool. Pool membership is the ONLY
// scoping PVE gives us, and it is authoritative here — a VM outside the pool never
// enters the cache, and a cached VM that left the pool is delete-of-vanished'd by
// Reconcile.

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// poolVMs lists the project pool's qemu rows off /cluster/resources, split into
// live VMs and templates.
func (s *Service) poolVMs(ctx context.Context, projectID string) (vms, templates []ClusterResource, err error) {
	pool := s.PoolFor(projectID)
	rows, err := s.C.ClusterResources(ctx, "vm")
	if err != nil {
		return nil, nil, err
	}
	for _, r := range rows {
		if r.Type != "qemu" || r.Pool != pool {
			continue
		}
		if r.Template == 1 {
			templates = append(templates, r)
		} else {
			vms = append(vms, r)
		}
	}
	return vms, templates, nil
}

// --- SERVER ---

type ServerSyncProvider struct {
	svc       *Service
	region    string
	projectID string
}

func (s *Service) ServerSyncProvider(region, projectID string) *ServerSyncProvider {
	return &ServerSyncProvider{svc: s, region: region, projectID: projectID}
}

func (p *ServerSyncProvider) Type() string      { return cloud.TypeServer }
func (p *ServerSyncProvider) ProjectID() string { return p.projectID }

// CompareKeys: whole-map compare of the server payload; ignores ephemeral churn keys.
func (p *ServerSyncProvider) CompareKeys() []string { return []string{} }

func (p *ServerSyncProvider) List(ctx context.Context) ([]cloud.CloudResource, error) {
	vms, _, err := p.svc.poolVMs(ctx, p.projectID)
	if err != nil {
		return nil, err
	}
	now := nowUTC()
	out := make([]cloud.CloudResource, 0, len(vms))
	for _, r := range vms {
		// Guest-agent IPs, best-effort: only a running VM can answer, and a guest
		// without the agent just yields no addresses (cloud-init installs it).
		var addrs map[string]any
		if r.Status == "running" {
			if ifaces, err := p.svc.C.AgentNetworkInterfaces(ctx, r.Node, r.VMID); err == nil {
				addrs = AgentAddresses(ifaces)
			}
		}
		out = append(out, cloud.CloudResource{
			ExternalID: itoa0(r.VMID),
			ProjectID:  p.projectID,
			Region:     p.region,
			Type:       cloud.TypeServer,
			Data:       ServerData(r, addrs, nil),
			Info:       &cloud.Info{UpdatedAt: &now},
		})
	}
	return out, nil
}

// --- VOLUME ---

type VolumeSyncProvider struct {
	svc       *Service
	region    string
	projectID string
}

func (s *Service) VolumeSyncProvider(region, projectID string) *VolumeSyncProvider {
	return &VolumeSyncProvider{svc: s, region: region, projectID: projectID}
}

func (p *VolumeSyncProvider) Type() string          { return cloud.TypeVolume }
func (p *VolumeSyncProvider) ProjectID() string     { return p.projectID }
func (p *VolumeSyncProvider) CompareKeys() []string { return []string{} }

// List derives VOLUME rows from every pool VM's disk entries. There is no
// standalone volume in the PVE model — a disk belongs to a VM; a detached disk is
// its VM's unusedN entry (still billed). ExternalID = volid (cluster-unique).
func (p *VolumeSyncProvider) List(ctx context.Context) ([]cloud.CloudResource, error) {
	vms, _, err := p.svc.poolVMs(ctx, p.projectID)
	if err != nil {
		return nil, err
	}
	now := nowUTC()
	var out []cloud.CloudResource
	for _, r := range vms {
		cfg, err := p.svc.C.GetVMConfig(ctx, r.Node, r.VMID)
		if err != nil {
			// A vanished/locked VM must not wipe the whole project's volume cache —
			// fail the sync pass; Reconcile skips this round.
			return nil, err
		}
		profile := TagValue(cfg.Str("tags"), tagProfilePref)
		for _, d := range ParseDisks(cfg) {
			status := "in-use"
			if len(d.Slot) >= 6 && d.Slot[:6] == "unused" {
				status = "available"
			}
			name := r.Name + "-" + d.Slot
			out = append(out, cloud.CloudResource{
				ExternalID: d.VolID,
				ProjectID:  p.projectID,
				Region:     p.region,
				Type:       cloud.TypeVolume,
				Data: map[string]any{
					"volume": map[string]any{
						"name":              name,
						"size":              d.SizeGiB,
						"bootable":          d.Boot,
						"volume_type":       volumeTypeOf(profile, d.Storage),
						"status":            status,
						"availability_zone": r.Node,
						"storage":           d.Storage,
						"vmid":              r.VMID,
						"slot":              d.Slot,
					},
				},
				Info: &cloud.Info{UpdatedAt: &now},
			})
		}
	}
	return out, nil
}

// volumeTypeOf is the billing `type` attribute: the storage-profile slug when the
// VM carries one, else the raw storage id (honest fallback — price rules can still
// match on it).
func volumeTypeOf(profile, storage string) string {
	if profile != "" {
		return profile
	}
	return storage
}

// --- IMAGE (project-owned templates) ---

type ImageSyncProvider struct {
	svc       *Service
	region    string
	projectID string
}

func (s *Service) ImageSyncProvider(region, projectID string) *ImageSyncProvider {
	return &ImageSyncProvider{svc: s, region: region, projectID: projectID}
}

func (p *ImageSyncProvider) Type() string          { return cloud.TypeImage }
func (p *ImageSyncProvider) ProjectID() string     { return p.projectID }
func (p *ImageSyncProvider) CompareKeys() []string { return []string{} }

func (p *ImageSyncProvider) List(ctx context.Context) ([]cloud.CloudResource, error) {
	_, templates, err := p.svc.poolVMs(ctx, p.projectID)
	if err != nil {
		return nil, err
	}
	now := nowUTC()
	out := make([]cloud.CloudResource, 0, len(templates))
	for _, r := range templates {
		out = append(out, cloud.CloudResource{
			ExternalID: itoa0(r.VMID),
			ProjectID:  p.projectID,
			Region:     p.region,
			Type:       cloud.TypeImage,
			Data: map[string]any{
				"image": map[string]any{
					"id":         itoa0(r.VMID),
					"name":       r.Name,
					"status":     "active",
					"visibility": "private",
					"size":       r.MaxDisk,
					"min_disk":   r.MaxDisk / (1 << 30),
				},
			},
			Info: &cloud.Info{UpdatedAt: &now},
		})
	}
	return out, nil
}

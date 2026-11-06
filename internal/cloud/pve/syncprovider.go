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
		if st, err := p.svc.C.GetVMStatus(ctx, r.Node, r.VMID); err == nil && st.QMPStatus == "paused" {
			r.Status = "paused"
		}
		addrs := p.svc.ServerAddresses(ctx, r)
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

// VolumeProfileFunc resolves the storage profile and storage class of one disk: the storage it
// sits on, the node, and the VM's own profile tag (preferred when several profiles map the same
// storage). Either result may be "" (unknown).
type VolumeProfileFunc func(node, storage, vmProfile string) (profile, class string)

type VolumeSyncProvider struct {
	svc       *Service
	region    string
	projectID string
	profileOf VolumeProfileFunc
}

// VolumeSyncProvider lists the project's disks; profileOf (nil = unknown) stamps each disk's
// profile and storage class, which the volume billing resource exposes to price rules.
func (s *Service) VolumeSyncProvider(region, projectID string, profileOf VolumeProfileFunc) *VolumeSyncProvider {
	return &VolumeSyncProvider{svc: s, region: region, projectID: projectID, profileOf: profileOf}
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
		for _, d := range p.svc.withUnusedSizes(ctx, r.Node, r.VMID, ParseDisks(cfg)) {
			status := "in-use"
			attachments := []any{map[string]any{"server_id": itoa0(r.VMID), "device": d.Slot}}
			if len(d.Slot) >= 6 && d.Slot[:6] == "unused" {
				status = "available"
				attachments = []any{}
			}
			name := r.Name + "-" + d.Slot
			diskProfile, class := "", ""
			if p.profileOf != nil {
				diskProfile, class = p.profileOf(r.Node, d.Storage, profile)
			}
			volType := volumeTypeOf(profile, d.Storage)
			if diskProfile != "" {
				volType = diskProfile
			}
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
						"volume_type":       volType,
						"profile":           diskProfile,
						"storage_class":     class,
						"status":            status,
						"attachments":       attachments,
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

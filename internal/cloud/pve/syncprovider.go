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
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// vanishGrace is how long a row survives being absent from /cluster/resources: a just-created VM
// may not be listed yet (pmxcfs lag), and a just-deleted one may still be.
const vanishGrace = 2 * time.Minute

// splitVMs splits /cluster/resources qemu rows of one pool into live VMs and templates. A clone on
// its way to becoming an image is neither: as a server it would bill a phantom hour, disk included.
func splitVMs(rows []ClusterResource, pool string) (vms, templates []ClusterResource) {
	for _, r := range rows {
		if r.Type != "qemu" || r.Pool != pool {
			continue
		}
		if r.Template == 1 {
			templates = append(templates, r)
		} else if !strings.HasPrefix(r.Name, imageBuildPrefix) && !isHolder(r.Tags) {
			vms = append(vms, r)
		}
	}
	return vms, templates
}

// diskVMs lists the pool VMs whose disks are VOLUME rows: the live VMs and the holders.
func (s *Service) diskVMs(ctx context.Context, projectID string) ([]ClusterResource, error) {
	rows, err := s.C.ClusterResources(ctx, "vm")
	if err != nil {
		return nil, err
	}
	pool := s.PoolFor(projectID)
	vms, _ := splitVMs(rows, pool)
	for _, r := range rows {
		if r.Type == "qemu" && r.Pool == pool && isHolder(r.Tags) {
			vms = append(vms, r)
		}
	}
	return vms, nil
}

// poolVMs lists the project pool's qemu rows off /cluster/resources, split into
// live VMs and templates.
func (s *Service) poolVMs(ctx context.Context, projectID string) (vms, templates []ClusterResource, err error) {
	rows, err := s.C.ClusterResources(ctx, "vm")
	if err != nil {
		return nil, nil, err
	}
	vms, templates = splitVMs(rows, s.PoolFor(projectID))
	return vms, templates, nil
}

// supersededByIdentity: the row now names another object when its type changed (a template's vmid
// reused by a VM) or both sides have an identity and they differ. An unknown identity never
// replaces anything: rows from before the stamp just pick one up. Neither does a stamp arriving on
// a row that had only a ctime: that is the portal tagging the VM it just cloned.
func supersededByIdentity(cached, fresh *cloud.CloudResource, identityOf func(map[string]any) string) bool {
	if cached.Type != fresh.Type {
		return true
	}
	if identityOf == nil {
		return false
	}
	old, cur := identityOf(cached.Data), identityOf(fresh.Data)
	if old == "" || cur == "" || old == cur {
		return false
	}
	return !(strings.HasPrefix(old, identityMeta) && strings.HasPrefix(cur, identityStamp))
}

func serverIdentity(data map[string]any) string {
	id, _ := data[dataIdentity].(string)
	return id
}

// --- SERVER ---

// GPUAliasFunc names a PCI mapping's GPU model ("" = not a GPU).
type GPUAliasFunc func(mappingID string) string

// ServerSyncProvider lists a project's VMs. The full variant reads each VM's config and guest
// agent; the sampled variant is fed a pre-fetched /cluster/resources page and makes no per-VM call
// except for a VM without an identity stamp, keeping what only the full pass knows (agent IPs, GPU
// count from hostpci) from the cached row.
type ServerSyncProvider struct {
	svc       *Service
	region    string
	projectID string
	gpuAlias  GPUAliasFunc
	sampled   []ClusterResource
	isSampled bool
	cached    map[string]*cloud.CloudResource
}

func (s *Service) ServerSyncProvider(region, projectID string, gpuAlias GPUAliasFunc) *ServerSyncProvider {
	return &ServerSyncProvider{svc: s, region: region, projectID: projectID, gpuAlias: gpuAlias}
}

// SampledServerProvider is the per-minute variant over one /cluster/resources page. cached is the
// project's SERVER rows by externalId.
func (s *Service) SampledServerProvider(region, projectID string, page []ClusterResource, cached map[string]*cloud.CloudResource) *ServerSyncProvider {
	vms, _ := splitVMs(page, s.PoolFor(projectID))
	return &ServerSyncProvider{svc: s, region: region, projectID: projectID, sampled: vms, isSampled: true, cached: cached}
}

func (p *ServerSyncProvider) Type() string      { return cloud.TypeServer }
func (p *ServerSyncProvider) ProjectID() string { return p.projectID }

// CompareKeys: whole-map compare of the server payload; ignores ephemeral churn keys.
func (p *ServerSyncProvider) CompareKeys() []string { return []string{} }

func (p *ServerSyncProvider) VanishGrace() time.Duration { return vanishGrace }

func (p *ServerSyncProvider) Superseded(cached, fresh *cloud.CloudResource) bool {
	return supersededByIdentity(cached, fresh, serverIdentity)
}

func (p *ServerSyncProvider) List(ctx context.Context) ([]cloud.CloudResource, error) {
	vms := p.sampled
	if !p.isSampled {
		var err error
		if vms, _, err = p.svc.poolVMs(ctx, p.projectID); err != nil {
			return nil, err
		}
	}
	now := nowUTC()
	out := make([]cloud.CloudResource, 0, len(vms))
	for _, r := range vms {
		var data map[string]any
		var cfg VMConfig
		if !p.isSampled || TagValue(r.Tags, tagCreatedPref) == "" {
			// ponytail: a VM without a stamp (made outside the portal) costs one config read per
			// sample, because only its ctime can tell it from a reuse of its vmid. Stamp it on first
			// sight if pools ever hold many of them; that would also tell apart clones of one
			// template, which share a ctime.
			if c, err := p.svc.C.GetVMConfig(ctx, r.Node, r.VMID); err == nil {
				cfg = c
			}
		}
		if p.isSampled {
			data = p.sampledData(r)
		} else {
			if st, err := p.svc.C.GetVMStatus(ctx, r.Node, r.VMID); err == nil && st.QMPStatus == "paused" {
				r.Status = "paused"
			}
			data = ServerData(r, p.svc.ServerAddresses(ctx, r), GPUSpecsFromConfig(cfg, p.gpuAlias))
		}
		if _, tagged := data[dataIdentity]; !tagged && cfg != nil {
			if id := metaIdentity(cfg); id != "" {
				data[dataIdentity] = id
			}
		}
		out = append(out, cloud.CloudResource{
			ExternalID: itoa0(r.VMID),
			ProjectID:  p.projectID,
			Region:     p.region,
			Type:       cloud.TypeServer,
			Data:       data,
			Info:       &cloud.Info{UpdatedAt: &now},
		})
	}
	return out, nil
}

// sampledData builds a sampled VM's payload, carrying the agent IPs, GPU specs and paused state of
// the cached row so a sample never undoes what the last full pass learned. /cluster/resources calls
// a paused VM running; a resume is a task, and the task poller re-syncs on it.
func (p *ServerSyncProvider) sampledData(r ClusterResource) map[string]any {
	var addrs, specs map[string]any
	if c := p.cached[itoa0(r.VMID)]; c != nil {
		if srv, ok := c.Data["server"].(map[string]any); ok {
			addrs, _ = srv["addresses"].(map[string]any)
			if srv["status"] == "PAUSED" && r.Status == "running" {
				r.Status = "paused"
			}
			if flv, ok := srv["flavor"].(map[string]any); ok {
				if m, ok := flv["extra_specs"].(map[string]any); ok && len(m) > 0 {
					specs = m
				}
			}
		}
	}
	return ServerData(r, addrs, specs)
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

// Superseded: a volid carries the vmid, so a reused vmid reuses the volids of its disks too.
func (p *VolumeSyncProvider) Superseded(cached, fresh *cloud.CloudResource) bool {
	return supersededByIdentity(cached, fresh, func(data map[string]any) string {
		vol, _ := data["volume"].(map[string]any)
		id, _ := vol[dataIdentity].(string)
		return id
	})
}

// List derives VOLUME rows from every pool VM's disk entries. There is no
// standalone volume in the PVE model — a disk belongs to a VM; a detached disk is
// its VM's unusedN entry (still billed). ExternalID = volid (cluster-unique).
func (p *VolumeSyncProvider) List(ctx context.Context) ([]cloud.CloudResource, error) {
	vms, err := p.svc.diskVMs(ctx, p.projectID)
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
		identity := taggedIdentity(cfg.Str("tags"), cfg.Str("lock"))
		if identity == "" {
			identity = metaIdentity(cfg)
		}
		for _, d := range p.svc.withUnusedSizes(ctx, r.Node, r.VMID, ParseDisks(cfg)) {
			status := "in-use"
			attachments := []any{map[string]any{"server_id": itoa0(r.VMID), "device": d.Slot}}
			if len(d.Slot) >= 6 && d.Slot[:6] == "unused" {
				status = "available"
				attachments = []any{}
			}
			name := r.Name + "-" + d.Slot
			if isHolder(r.Tags) { // no server to name it after
				name = d.VolID[strings.IndexByte(d.VolID, ':')+1:]
			}
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
						dataIdentity:        identity,
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

// ImageSyncProvider lists a project's templates. page, when set, is a pre-fetched /cluster/resources
// page (the sampler's), so a sample costs no call of its own.
type ImageSyncProvider struct {
	svc       *Service
	region    string
	projectID string
	page      []ClusterResource
}

func (s *Service) ImageSyncProvider(region, projectID string) *ImageSyncProvider {
	return &ImageSyncProvider{svc: s, region: region, projectID: projectID}
}

// SampledImageProvider is the per-minute variant over one /cluster/resources page.
func (s *Service) SampledImageProvider(region, projectID string, page []ClusterResource) *ImageSyncProvider {
	return &ImageSyncProvider{svc: s, region: region, projectID: projectID, page: page}
}

func (p *ImageSyncProvider) Type() string               { return cloud.TypeImage }
func (p *ImageSyncProvider) ProjectID() string          { return p.projectID }
func (p *ImageSyncProvider) CompareKeys() []string      { return []string{} }
func (p *ImageSyncProvider) VanishGrace() time.Duration { return vanishGrace }

func (p *ImageSyncProvider) Superseded(cached, fresh *cloud.CloudResource) bool {
	return supersededByIdentity(cached, fresh, nil)
}

func (p *ImageSyncProvider) List(ctx context.Context) ([]cloud.CloudResource, error) {
	var templates []ClusterResource
	if p.page != nil {
		_, templates = splitVMs(p.page, p.svc.PoolFor(p.projectID))
	} else {
		var err error
		if _, templates, err = p.svc.poolVMs(ctx, p.projectID); err != nil {
			return nil, err
		}
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

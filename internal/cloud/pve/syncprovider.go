package pve

// syncprovider.go — the periodic-sync providers for one (project, PVE service):
// SERVER (qemu VMs in the project pool), VOLUME (their disks), IMAGE (project
// templates). Each satisfies providers.Provider + ProjectScoped implicitly, with
// no import of the providers package (dbaas precedent — dependency arrow pve→cloud).
//
// Tenancy: every List filters by the project's pool. Pool membership is the ONLY
// scoping PVE gives us. Lists may be filtered: vm_inventory.go verifies positive
// membership/configuration and proves omissions before Reconcile can write.

import (
	"context"
	"encoding/json"
	"maps"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// vanishGrace is how long a row survives being absent from /cluster/resources: a just-created VM
// may not be listed yet (pmxcfs lag), and a just-deleted one may still be.
const vanishGrace = 2 * time.Minute

// splitVMs splits /cluster/resources qemu rows of one pool into live VMs and templates. A clone on
// its way to becoming an image, or the donor of a rebuild's disk, is neither: as a server it would
// bill a phantom hour, disk included. Nor is a row PVE knows nothing about (rowUnread).
func splitVMs(rows []ClusterResource, pool string) (vms, templates []ClusterResource) {
	for _, r := range rows {
		if r.Type != "qemu" || r.Pool != pool || rowUnread(r) {
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

// rowUnread: PVE lists the vmid but says nothing usable about it. Its node is down, or the row has no name yet:
// /cluster/resources lags a create by seconds (lab, PVE 9.2), and every VM the portal makes has one.
func rowUnread(r ClusterResource) bool {
	return r.NodeDown || (r.Name == "" && r.Template != 1)
}

// unreadVMs is rowUnread over one pool's rows: vmid -> its node is down. A listed vmid exists, so Reconcile must not
// take the row of an unread one for a VM that is gone (KeepUnread).
func unreadVMs(rows []ClusterResource, pool string) map[int]bool {
	out := map[int]bool{}
	for _, r := range rows {
		if r.Type == "qemu" && r.Pool == pool && rowUnread(r) {
			out[r.VMID] = r.NodeDown
		}
	}
	return out
}

// diskVMs lists the pool VMs whose disks are VOLUME rows: the live VMs and the holders, plus the unread ones.
func (s *Service) diskVMs(ctx context.Context, projectID string) ([]ClusterResource, map[int]bool, error) {
	rows, err := s.C.ClusterResources(ctx, "vm")
	if err != nil {
		return nil, nil, err
	}
	pool := s.PoolFor(projectID)
	vms, _ := splitVMs(rows, pool)
	for _, r := range rows {
		if r.Type == "qemu" && r.Pool == pool && isHolder(r.Tags) && !rowUnread(r) {
			vms = append(vms, r)
		}
	}
	return vms, unreadVMs(rows, pool), nil
}

// poolVMs lists the project pool's qemu rows off /cluster/resources, split into
// live VMs and templates, plus the unread ones.
func (s *Service) poolVMs(ctx context.Context, projectID string) (vms, templates []ClusterResource, unread map[int]bool, err error) {
	rows, err := s.C.ClusterResources(ctx, "vm")
	if err != nil {
		return nil, nil, nil, err
	}
	pool := s.PoolFor(projectID)
	vms, templates = splitVMs(rows, pool)
	return vms, templates, unreadVMs(rows, pool), nil
}

// errUnreadVMs refuses a delete guard that cannot see every VM of the project: the object may be in use by one of them.
func errUnreadVMs() error {
	return conflictf("A host of this project's servers is down, so its servers cannot be checked. Try again when it is back")
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
	unread    map[int]bool
	inventory *vmInventory
}

func (s *Service) ServerSyncProvider(region, projectID string, gpuAlias GPUAliasFunc) *ServerSyncProvider {
	return &ServerSyncProvider{svc: s, region: region, projectID: projectID, gpuAlias: gpuAlias}
}

// SampledServerProvider is the per-minute variant over one /cluster/resources page. cached is the
// project's SERVER rows by externalId.
func (s *Service) SampledServerProvider(region, projectID string, page []ClusterResource, cached map[string]*cloud.CloudResource) *ServerSyncProvider {
	pool := s.PoolFor(projectID)
	return &ServerSyncProvider{svc: s, region: region, projectID: projectID, sampled: page, isSampled: true, cached: cached, unread: unreadVMs(page, pool)}
}

func (p *ServerSyncProvider) Type() string      { return cloud.TypeServer }
func (p *ServerSyncProvider) ProjectID() string { return p.projectID }

// CompareKeys: whole-map compare of the server payload; ignores ephemeral churn keys.
func (p *ServerSyncProvider) CompareKeys() []string { return []string{} }

func (p *ServerSyncProvider) VanishGrace() time.Duration { return vanishGrace }

func (p *ServerSyncProvider) Superseded(cached, fresh *cloud.CloudResource) bool {
	return vmUUIDChanged(cached, fresh) || supersededByIdentity(cached, fresh, serverIdentity)
}

func (p *ServerSyncProvider) List(ctx context.Context) ([]cloud.CloudResource, error) {
	var err error
	p.inventory, err = p.svc.readVMInventory(ctx, p.projectID, p.Type(), p.sampled, p.isSampled)
	if err != nil {
		return nil, err
	}
	vms, _ := splitVMs(p.inventory.page, p.svc.PoolFor(p.projectID))
	p.unread = map[int]bool{}
	now := nowUTC()
	out := make([]cloud.CloudResource, 0, len(vms))
	for _, r := range vms {
		var data map[string]any
		cfg := p.inventory.configs[r.VMID]
		if blankRow(r) {
			if cfg != nil && blankConfig(cfg) {
				continue // a create that never finished: no server, and RemoveBlankVMs retries removing it
			}
			if cfg == nil { // it could not be asked: keep what is known
				p.unread[r.VMID] = false
				continue
			}
		}
		cached := p.cached[itoa0(r.VMID)]
		if p.isSampled && (cached == nil || vmRowUUID(cached) != "") {
			data = p.sampledData(r)
		} else {
			st, err := p.svc.C.GetVMStatus(ctx, r.Node, r.VMID)
			if err != nil || st.QMPStatus == "" {
				return nil, vmInventoryError(p.Type(), itoa0(r.VMID), "VM runtime status unknown", err)
			}
			if st.QMPStatus == "paused" {
				r.Status = "paused"
			}
			addrs, agentOK := p.inventory.addresses(ctx, r)
			p.inventory.addressUnknown[r.VMID] = !agentOK
			data = ServerData(r, addrs, GPUSpecsFromConfig(cfg, p.gpuAlias))
			protection, err := p.svc.inventoryProtection(ctx, r, cfg)
			if err != nil {
				return nil, vmInventoryError(p.Type(), itoa0(r.VMID), "protection read unknown", err)
			}
			if protection != nil {
				data["server"].(map[string]any)["protection"] = protection
			}
			snaps, err := p.inventory.snapshots(ctx, r)
			if err != nil {
				return nil, vmInventoryError(p.Type(), itoa0(r.VMID), "snapshot read unknown", err)
			}
			if snaps != nil {
				data["server"].(map[string]any)["snapshots"] = snaps
			}
		}
		data[vmUUIDKey] = vmConfigUUID(cfg)
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

// KeepUnread: a VM on a dead node is still the tenant's. Its row stays, with what it knew; only the status changes,
// which also stops the server's bill for as long as nobody can use it.
func (p *ServerSyncProvider) KeepUnread(cached *cloud.CloudResource) (keep, changed bool) {
	return false, false
}

// sampledData builds a sampled VM's payload, carrying the agent IPs, GPU specs and paused state of
// the cached row so a sample never undoes what the last full pass learned. /cluster/resources calls
// a paused VM running; a resume is a task, and the task poller re-syncs on it.
func (p *ServerSyncProvider) sampledData(r ClusterResource) map[string]any {
	var addrs, specs map[string]any
	carried := map[string]any{} // what only a full pass reads, and billing rates
	if c := p.cached[itoa0(r.VMID)]; c != nil && vmRowUUID(c) != "" && vmRowUUID(c) == vmConfigUUID(p.inventory.configs[r.VMID]) {
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
			for _, k := range []string{"protection", "snapshots"} {
				if v, ok := srv[k]; ok {
					carried[k] = v
				}
			}
		}
	}
	data := ServerData(r, addrs, specs)
	maps.Copy(data["server"].(map[string]any), carried)
	return data
}

// diskGbOf sums a VM's disks: what a replica takes on the target, and what protection bills.
func diskGbOf(cfg VMConfig) int {
	n := 0
	for _, d := range ParseDisks(cfg) {
		n += d.SizeGiB
	}
	return n
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
	unread    map[int]bool
	inventory *vmInventory
}

// VolumeSyncProvider lists the project's disks; profileOf (nil = unknown) stamps each disk's
// profile and storage class, which the volume billing resource exposes to price rules.
func (s *Service) VolumeSyncProvider(region, projectID string, profileOf VolumeProfileFunc) *VolumeSyncProvider {
	return &VolumeSyncProvider{svc: s, region: region, projectID: projectID, profileOf: profileOf}
}

// VolumeLabel is the data.volume key of the name a tenant gave a volume. PVE has no name for a disk, so the name lives
// on the row only: a sync carries it over (MergeCached), and a re-read without it would show the derived name again.
const VolumeLabel = "label"

// CarryVolumeLabel keeps a volume row's label, and so its name, over a fresh read of the disk.
func CarryVolumeLabel(cached, fresh map[string]any) {
	c, _ := cached["volume"].(map[string]any)
	f, _ := fresh["volume"].(map[string]any)
	if l, _ := c[VolumeLabel].(string); l != "" && f != nil {
		f[VolumeLabel], f["name"] = l, l
	}
}

func (p *VolumeSyncProvider) MergeCached(cached, fresh map[string]any) {
	CarryVolumeLabel(cached, fresh)
}

func (p *VolumeSyncProvider) Type() string          { return cloud.TypeVolume }
func (p *VolumeSyncProvider) ProjectID() string     { return p.projectID }
func (p *VolumeSyncProvider) CompareKeys() []string { return []string{} }

// Superseded: a volid carries the vmid, so a reused vmid reuses the volids of its disks too.
func (p *VolumeSyncProvider) Superseded(cached, fresh *cloud.CloudResource) bool {
	return vmUUIDChanged(cached, fresh) || supersededByIdentity(cached, fresh, func(data map[string]any) string {
		vol, _ := data["volume"].(map[string]any)
		id, _ := vol[dataIdentity].(string)
		return id
	})
}

// List derives VOLUME rows from every pool VM's disk entries. There is no
// standalone volume in the PVE model — a disk belongs to a VM; a detached disk is
// its VM's unusedN entry (still billed). ExternalID = volid (cluster-unique).
// Reconcile archives what this leaves out, so the rows of a VM that cannot be read stay (KeepUnread). Only a holder gone
// from every node is not kept: /cluster/resources still lists one just dropped, and every cross-node attach drops one.
func (p *VolumeSyncProvider) List(ctx context.Context) ([]cloud.CloudResource, error) {
	return p.list(ctx, func(vm ClusterResource, err error) bool { return isHolder(vm.Tags) && IsNotFound(err) })
}

// ListReadable lists the rows of every VM that can be read. It is for callers that only add and update rows, where a
// VM just deleted or a node that is down must not hold up the rest of the project.
func (p *VolumeSyncProvider) ListReadable(ctx context.Context) ([]cloud.CloudResource, error) {
	return p.list(ctx, func(ClusterResource, error) bool { return true })
}

func (p *VolumeSyncProvider) list(ctx context.Context, skip func(ClusterResource, error) bool) ([]cloud.CloudResource, error) {
	var err error
	p.inventory, err = p.svc.readVMInventory(ctx, p.projectID, p.Type(), nil, false)
	if err != nil {
		return nil, err
	}
	pool := p.svc.PoolFor(p.projectID)
	vms, _ := splitVMs(p.inventory.page, pool)
	for _, r := range p.inventory.page {
		if r.Type == "qemu" && r.Pool == pool && isHolder(r.Tags) {
			vms = append(vms, r)
		}
	}
	p.unread = map[int]bool{}
	now := nowUTC()
	var out []cloud.CloudResource
	for _, r := range vms {
		cfg := p.inventory.configs[r.VMID]
		profile := TagValue(cfg.Str("tags"), tagProfilePref)
		identity := taggedIdentity(cfg.Str("tags"), cfg.Str("lock"))
		if identity == "" {
			identity = metaIdentity(cfg)
		}
		disks, err := p.inventory.disks(ctx, r)
		if err != nil {
			return nil, vmInventoryError(p.Type(), itoa0(r.VMID), "disk inventory incomplete", err)
		}
		for _, d := range disks {
			// A PVC's disk belongs to the tenant cluster, not to this VM: it moves with the pod and
			// outlives the worker. It is no volume of the project's (plan: hidden everywhere).
			if isCSIDisk(d.VolID) {
				continue
			}
			status := "in-use"
			attachments := []any{map[string]any{"server_id": itoa0(r.VMID), "device": d.Slot}}
			if len(d.Slot) >= 6 && d.Slot[:6] == "unused" {
				status = "available"
				attachments = []any{}
			}
			name := r.Name + "-" + d.Slot
			if isHolder(r.Tags) { // no server to name it after: the disk's file name, not its path on the store
				name = path.Base(d.VolID[strings.IndexByte(d.VolID, ':')+1:])
				name = strings.TrimSuffix(name, path.Ext(name))
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
						vmUUIDKey:           vmConfigUUID(cfg),
					},
				},
				Info: &cloud.Info{UpdatedAt: &now},
			})
		}
	}
	return out, nil
}

// KeepUnread: the disks of a VM nobody can read right now are still there. A tenant cluster's disk is no VM's: its row
// comes from the CSI gateway's registry (csigw.VolumeRows), and this sync leaves it alone.
func (p *VolumeSyncProvider) KeepUnread(cached *cloud.CloudResource) (keep, changed bool) {
	if CSIClusterOf(cached.Data) != "" {
		return true, false
	}
	vol, _ := cached.Data["volume"].(map[string]any)
	_, unread := p.unread[intOf(vol["vmid"])]
	return unread, false
}

// intOf reads a number off a row, fresh (int) or from the database (json.Number).
func intOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// holderElsewhere finds a holder its listed node does not have: mid-migrate, /cluster/resources still names the old node
// while the disk is billed and in use on the new one. notFound comes back when no node has it. A node that cannot say is
// an error of its own, so the holder counts as unread instead of its volume being archived while only out of sight.
func (s *Service) holderElsewhere(ctx context.Context, vm ClusterResource, notFound error) (string, VMConfig, error) {
	nodes, err := s.C.Nodes(ctx)
	if err != nil {
		return "", nil, err
	}
	for _, n := range nodes {
		if n.Node == vm.Node {
			continue
		}
		cfg, err := s.C.GetVMConfig(ctx, n.Node, vm.VMID)
		if err == nil {
			return n.Node, cfg, nil
		}
		if !IsNotFound(err) {
			return "", nil, err
		}
	}
	return "", nil, notFound
}

// DataK8sCluster marks a VOLUME row as a tenant cluster's disk (data.volume.k8sCluster = cluster id).
const DataK8sCluster = "k8sCluster"

// CSIClusterOf is the tenant cluster a VOLUME row's disk belongs to, "" for any other row.
func CSIClusterOf(data map[string]any) string {
	vol, _ := data["volume"].(map[string]any)
	id, _ := vol[DataK8sCluster].(string)
	return id
}

// isCSIDisk recognises proxmox-csi's volume names, <store>:vm-9999-pvc-<uuid> (with a file store's
// subdirectory and extension in between).
func isCSIDisk(volID string) bool { return strings.Contains(volID, "-pvc-") }

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
	unread    map[int]bool
	inventory *vmInventory
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
	return vmUUIDChanged(cached, fresh) || supersededByIdentity(cached, fresh, nil)
}

// KeepUnread: PVE does not even say a dead node's row is a template; the image is still there.
func (p *ImageSyncProvider) KeepUnread(cached *cloud.CloudResource) (keep, changed bool) {
	id, err := strconv.Atoi(cached.ExternalID)
	if err != nil {
		return false, false
	}
	_, unread := p.unread[id]
	return unread, false
}

func (p *ImageSyncProvider) List(ctx context.Context) ([]cloud.CloudResource, error) {
	var err error
	p.inventory, err = p.svc.readVMInventory(ctx, p.projectID, p.Type(), p.page, p.page != nil)
	if err != nil {
		return nil, err
	}
	_, templates := splitVMs(p.inventory.page, p.svc.PoolFor(p.projectID))
	p.unread = map[int]bool{}
	now := nowUTC()
	out := make([]cloud.CloudResource, 0, len(templates))
	for _, r := range templates {
		out = append(out, cloud.CloudResource{
			ExternalID: itoa0(r.VMID),
			ProjectID:  p.projectID,
			Region:     p.region,
			Type:       cloud.TypeImage,
			Data: map[string]any{
				vmUUIDKey: vmConfigUUID(p.inventory.configs[r.VMID]),
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

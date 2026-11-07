package csigw

// billing.go turns the registry into the project's VOLUME rows, so tenant cluster disks bill like any volume, and
// settles rows PVE disagrees with. The PVE volume sync skips these disks (they belong to no VM of the project) and
// keeps these rows; this provider keeps the others. Both reconcile the same (provider, project, VOLUME) set.

import (
	"context"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// creatingGrace is how long a creating row may exist with no disk before the reconciler drops it: a create or copy
// whose answer was lost writes its disk within that, or never.
const creatingGrace = 15 * time.Minute

// VolumeRows is the sync provider of one project's tenant cluster disks on one compute provider.
type VolumeRows struct {
	Reg       *Store
	ServiceID string
	Project   string
	Region    string
	// GW builds the gateway's client of the compute provider; the reconcile reads the stores with it.
	GW func(ctx context.Context, serviceID string) (*pve.Client, error)
	// ProfileOf names the storage profile and class of a store (externalservice.PVEVolumeProfileFunc). nil = the store.
	ProfileOf pve.VolumeProfileFunc
	Now       func() time.Time
}

func (p *VolumeRows) Type() string          { return cloud.TypeVolume }
func (p *VolumeRows) ProjectID() string     { return p.Project }
func (p *VolumeRows) CompareKeys() []string { return []string{} }

// KeepUnread keeps every row that is not a cluster disk: those are the PVE volume sync's.
func (p *VolumeRows) KeepUnread(cached *cloud.CloudResource) (keep, changed bool) {
	return pve.CSIClusterOf(cached.Data) == "", false
}

func (p *VolumeRows) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

// List settles the project's rows against PVE, then maps what is left alive (ready, deleting) to VOLUME rows. A store
// that cannot be read leaves its rows as they are: the registry, not a failed read, says what exists.
func (p *VolumeRows) List(ctx context.Context) ([]cloud.CloudResource, error) {
	var rows []Volume
	if err := p.Reg.vols.Find(ctx, pgdoc.M{"projectId": p.Project, "computeServiceId": p.ServiceID,
		"state": pgdoc.M{"$ne": StateDeleted}}, &rows); err != nil {
		return nil, err
	}
	if len(rows) > 0 && p.GW != nil {
		if cl, err := p.GW(ctx, p.ServiceID); err == nil {
			rows = p.settle(ctx, cl, rows)
		}
	}
	out := make([]cloud.CloudResource, 0, len(rows))
	for i := range rows {
		if rows[i].State == StateReady || rows[i].State == StateDeleting {
			out = append(out, p.row(&rows[i]))
		}
	}
	return out, nil
}

// settle applies what PVE shows. A creating row is left to its create or copy for the grace; past it, the row of a
// disk PVE lists turns ready (a create whose answer and confirm were both lost: the name was unused, so the disk is
// this row's), and the row of none goes. A live row whose disk is gone is deleted; a ready row takes the disk's real
// size and volid. Disks match by name (diskBase), since a file store lists them under "<vmid>/". It returns the rows as
// they are now.
func (p *VolumeRows) settle(ctx context.Context, cl *pve.Client, rows []Volume) []Volume {
	listed := map[string]map[string]listedDisk{}
	now := p.now()
	out := rows[:0]
	for _, v := range rows {
		key := v.Node + "/" + v.Storage
		disks, ok := listed[key]
		if !ok {
			var err error
			if disks, err = listDisks(ctx, cl, v.Node, v.Storage); err != nil {
				disks = nil
			}
			listed[key] = disks
		}
		if disks == nil {
			out = append(out, v)
			continue
		}
		d, there := disks[diskBase(v.VolID)]
		old := v.State == StateCreating && v.CreatedAt != nil && now.Sub(*v.CreatedAt) > creatingGrace
		switch {
		case v.State == StateCreating && !old:
		case v.State == StateCreating && !there:
			if p.Reg.DeleteVolume(ctx, v.ID) == nil {
				continue
			}
		case !there:
			if p.Reg.markDeleted(ctx, v.ID) == nil {
				continue
			}
		case v.State == StateCreating || v.State == StateReady:
			w := v
			w.State, w.VolID = StateReady, d.VolID
			if d.Size > 0 {
				w.SizeBytes = d.Size
			}
			if w != v && p.Reg.UpdateVolume(ctx, v.ID, pgdoc.M{"state": w.State, "volid": w.VolID, "sizeBytes": w.SizeBytes}) == nil {
				v = w
			}
		}
		out = append(out, v)
	}
	return out
}

// row is a disk as a VOLUME row of the project. The name is the PersistentVolume's (pvc-<uid>).
func (p *VolumeRows) row(v *Volume) cloud.CloudResource {
	_, disk, _ := strings.Cut(v.VolID, ":")
	name := disk[strings.LastIndex(disk, "/")+1:]
	if i := strings.Index(name, "-pvc-"); i >= 0 {
		name = strings.TrimSuffix(strings.TrimSuffix(name[i+1:], ".raw"), ".qcow2")
	}
	profile, class := "", ""
	if p.ProfileOf != nil {
		profile, class = p.ProfileOf(v.Node, v.Storage, "")
	}
	volType := v.Storage
	if profile != "" {
		volType = profile
	}
	status := "in-use"
	if v.State == StateDeleting {
		status = "deleting"
	}
	return cloud.CloudResource{
		ExternalID: v.VolID,
		ProjectID:  p.Project,
		Region:     p.Region,
		Type:       cloud.TypeVolume,
		Data: map[string]any{
			"volume": map[string]any{
				"name":              name,
				"size":              gibUp(v.SizeBytes),
				"bootable":          false,
				"volume_type":       volType,
				"profile":           profile,
				"storage_class":     class,
				"status":            status,
				"attachments":       []any{},
				"availability_zone": v.Node,
				"storage":           v.Storage,
				pve.DataK8sCluster:  v.ClusterID,
			},
		},
		// The disk's age, not the first sync's: billing accrues from it.
		CreatedAt: v.CreatedAt,
		Info:      &cloud.Info{CreatedAt: v.CreatedAt},
	}
}

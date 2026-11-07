package pve

// snapshot.go — the server snapshot product (S13), on PVE's own VM snapshots. Disk only (vmstate
// off keeps it storage-cheap; the guest agent freezes the filesystems while it runs). Storage decides
// what is honest to offer: a storage without snapshots refuses up front (capacity NFS by design),
// and ZFS rolls back to the newest snapshot only, so an older one is refused rather than attempted.

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ServerSnapshot is one snapshot as the API shows it.
type ServerSnapshot struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	CreatedAt   int64  `json:"createdAt"`
	Latest      bool   `json:"latest"`
}

var snapshotNameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{1,39}$`)

// ValidSnapshotName reports whether PVE accepts name, and it is not a replication snapshot's.
func ValidSnapshotName(name string) bool {
	return snapshotNameRe.MatchString(name) && !isPlatformSnapshot(name)
}

func isPlatformSnapshot(name string) bool {
	return name == "current" || strings.HasPrefix(name, "__") || strings.HasPrefix(name, "vzdump")
}

// ListServerSnapshots lists a pool VM's snapshots, newest first, without PVE's "current" row and the
// replication machinery's.
func (s *Service) ListServerSnapshots(ctx context.Context, projectID, externalID string) ([]ServerSnapshot, error) {
	vm, err := s.resolveOwned(ctx, projectID, externalID)
	if err != nil {
		return nil, err
	}
	rows, err := s.C.ListSnapshots(ctx, vm.Node, vm.VMID)
	if err != nil {
		return nil, err
	}
	latest := ""
	for _, r := range rows {
		if r.Name == "current" {
			latest = r.Parent
		}
	}
	out := []ServerSnapshot{}
	for _, r := range rows {
		if isPlatformSnapshot(r.Name) {
			continue
		}
		out = append(out, ServerSnapshot{Name: r.Name, Description: r.Description, CreatedAt: r.SnapTime, Latest: r.Name == latest})
	}
	slices.SortFunc(out, func(a, b ServerSnapshot) int { return int(b.CreatedAt - a.CreatedAt) })
	return out, nil
}

// CreateServerSnapshot snapshots a pool VM. snapshotable says whether a storage can (the provider's
// capability table). A VM with a disk on a storage that cannot is refused: PVE would fail half way
// or, on qcow2 over NFS, block the guest for the length of the copy.
func (s *Service) CreateServerSnapshot(ctx context.Context, projectID, externalID, name, description string, snapshotable func(storage string) bool) error {
	vm, err := s.resolveOwned(ctx, projectID, externalID)
	if err != nil {
		return err
	}
	cfg, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	if err != nil {
		return err
	}
	for _, d := range ParseDisks(cfg) {
		if d.Storage != "" && !strings.HasPrefix(d.Slot, "unused") && !snapshotable(d.Storage) {
			return conflictf("Disk %s of server %s is on storage %s, which has no snapshots. Move it to another volume type first", d.Slot, vm.Name, d.Storage)
		}
	}
	rows, err := s.C.ListSnapshots(ctx, vm.Node, vm.VMID)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Name == name {
			return conflictf("Server %s already has a snapshot named %s", vm.Name, name)
		}
	}
	up, err := s.C.CreateSnapshot(ctx, vm.Node, vm.VMID, name, description)
	if err != nil {
		return err
	}
	return s.C.WaitTask(ctx, up)
}

// RollbackServerSnapshot returns a pool VM to a snapshot; a running VM comes back running. onlyLatest
// reports whether a storage rolls back to its newest snapshot only (ZFS). The guest's disk state after
// the snapshot is gone, so the caller confirms with the tenant first. A rollback brings back the snapshot's cores, memory
// and devices too (lab, PVE 9.2), so guard gets the VM's config and the snapshot's and can refuse.
func (s *Service) RollbackServerSnapshot(ctx context.Context, projectID, externalID, name string, onlyLatest func(storage string) bool, guard func(ctx context.Context, cur, target VMConfig) error) error {
	vm, err := s.resolveOwned(ctx, projectID, externalID)
	if err != nil {
		return err
	}
	snaps, err := s.ListServerSnapshots(ctx, projectID, externalID)
	if err != nil {
		return err
	}
	idx := slices.IndexFunc(snaps, func(x ServerSnapshot) bool { return x.Name == name })
	if idx < 0 {
		return conflictf("Server %s has no snapshot named %s", vm.Name, name)
	}
	cfg, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	if err != nil {
		return err
	}
	target, err := s.C.SnapshotConfig(ctx, vm.Node, vm.VMID, name)
	if err != nil {
		return err
	}
	if err := guard(ctx, cfg, target); err != nil {
		return err
	}
	if !snaps[idx].Latest {
		for _, d := range ParseDisks(cfg) {
			if !strings.HasPrefix(d.Slot, "unused") && onlyLatest(d.Storage) {
				return conflictf("Disk %s is on storage %s, which only rolls back to the newest snapshot. Delete the newer snapshots first", d.Slot, d.Storage)
			}
		}
	}
	up, err := s.C.RollbackSnapshot(ctx, vm.Node, vm.VMID, name)
	if err == nil {
		err = s.C.WaitTask(ctx, up)
	}
	if err != nil {
		return err
	}
	tagErr := s.keepProtectionTags(ctx, vm, cfg.Str("tags"))
	// Rollback leaves a snapshot without vmstate stopped.
	if vm.Status == "running" {
		up, err := s.C.StartVM(ctx, vm.Node, vm.VMID)
		if err == nil {
			err = s.C.WaitTask(ctx, up)
		}
		if err != nil {
			return fmt.Errorf("pve rollback: restart: %w", err)
		}
	}
	if tagErr != nil {
		return fmt.Errorf("pve rollback: protection tags: %w", tagErr)
	}
	return nil
}

// DeleteServerSnapshot removes a snapshot of a pool VM.
func (s *Service) DeleteServerSnapshot(ctx context.Context, projectID, externalID, name string) error {
	vm, err := s.resolveOwned(ctx, projectID, externalID)
	if err != nil {
		return err
	}
	if isPlatformSnapshot(name) {
		return conflictf("Snapshot %s belongs to the platform", name)
	}
	up, err := s.C.DeleteSnapshot(ctx, vm.Node, vm.VMID, name)
	if err != nil {
		return err
	}
	return s.C.WaitTask(ctx, up)
}

// StampSnapshots adds a VM's tenant snapshots to its row for billing, each with its own start time so a new one
// is not charged back to when the VM was created. PVE reports no per-snapshot usage on any storage (a volume's
// "used" excludes its snapshots), so the metered size is the VM's provisioned disks.
// ponytail: provisioned GiB per snapshot is a proxy for the blocks each one pins; real usage needs a node-side read
// (zfs list -t snapshot, rbd du), add it if the price is ever to follow the actual delta.
// Best effort: a failed read leaves the row without them until the next pass.
func (s *Service) StampSnapshots(ctx context.Context, r ClusterResource, data map[string]any) {
	rows, err := s.C.ListSnapshots(ctx, r.Node, r.VMID)
	if err != nil {
		return
	}
	rows = slices.DeleteFunc(rows, func(x Snapshot) bool { return isPlatformSnapshot(x.Name) })
	if len(rows) == 0 {
		return
	}
	cfg, err := s.C.GetVMConfig(ctx, r.Node, r.VMID)
	if err != nil {
		return
	}
	slices.SortFunc(rows, func(a, b Snapshot) int { return strings.Compare(a.Name, b.Name) })
	items := make([]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, map[string]any{"name": row.Name, "createdAt": time.Unix(row.SnapTime, 0).UTC().Format(time.RFC3339)})
	}
	if srv, ok := data["server"].(map[string]any); ok {
		srv["snapshots"] = map[string]any{"items": items, "diskGb": diskGbOf(cfg)}
	}
}

func (s *Service) resolveOwned(ctx context.Context, projectID, externalID string) (*ClusterResource, error) {
	vm, err := s.ResolveVM(ctx, projectID, externalID)
	if err != nil {
		return nil, err
	}
	if vm == nil {
		return nil, fmt.Errorf("pve: vm %s not found", externalID)
	}
	return vm, nil
}

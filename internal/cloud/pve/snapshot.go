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
// the snapshot is gone, so the caller confirms with the tenant first.
func (s *Service) RollbackServerSnapshot(ctx context.Context, projectID, externalID, name string, onlyLatest func(storage string) bool) error {
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
	if !snaps[idx].Latest {
		cfg, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
		if err != nil {
			return err
		}
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

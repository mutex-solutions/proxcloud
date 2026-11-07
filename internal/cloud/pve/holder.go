package pve

// holder.go — volumes that outlive a server. A Proxmox disk must belong to a VM, so a detached volume with no
// server sits as unusedN on a holder VM: one per (project, node), never started, tagged pc-holder, kept in the
// project pool. Holders are not servers: no SERVER row, so no compute bill, no quota and no attach target. Their
// disks are ordinary VOLUME rows, billed like any other. A holder is removed with its last disk.
//
// A carrier is a holder made for one cross-node attach (carryToNode). It is hidden the same way and goes the same way,
// but no other operation may put a disk on it: it is about to change node, and its creator is about to remove it.
//
// What empties a holder and what fills one must not overlap, or the delete takes a disk moved in after its check. So
// every operation below that picks, fills or drops a holder runs under the project's holder lock.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	tagHolder  = "pc-holder"
	tagCarrier = "pc-carrier"

	// holderLockMaxFor bounds a crashed lock holder. A live one renames disks and migrates an empty VM: seconds.
	holderLockMaxFor = 10 * time.Minute
	// holderLockWait covers a queue of those before a caller gets a 409.
	holderLockWait = time.Minute
)

// lostTaskWait is how long a failure waits to learn what a task it lost track of did, before assuming the worst.
var lostTaskWait = time.Minute

func isHolder(tags string) bool { return hasTag(tags, tagHolder) }

// taskRefused: PVE answered no before starting the task. Any other failure could be a lost answer to a started task.
// PVE checks a VM's lock before it forks, and reports that as a 500.
func taskRefused(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Status < 500 || strings.Contains(ae.Error(), "is locked"))
}

// lockHolders takes the project's holder lock and returns its release. Busy = ConflictError.
func (s *Service) lockHolders(ctx context.Context, projectID string) (func(), error) {
	if s.HolderLock == nil {
		return func() {}, nil
	}
	release, acquired, err := s.HolderLock(ctx, "pve-holders-"+projectID, holderLockMaxFor, holderLockWait)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, conflictf("Another volume change is running in this project. Try again in a moment")
	}
	return release, nil
}

// holderOn returns the project's holder VM on node, creating it when there is none. The caller holds the holder lock.
func (s *Service) holderOn(ctx context.Context, projectID, node string) (*ClusterResource, error) {
	pool := s.PoolFor(projectID)
	rows, err := s.C.ClusterResources(ctx, "vm")
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Type == "qemu" && rows[i].Pool == pool && rows[i].Node == node && isHolder(rows[i].Tags) && !hasTag(rows[i].Tags, tagCarrier) {
			return &rows[i], nil
		}
	}
	return s.newHolder(ctx, projectID, node, JoinTags(tagHolder, TagManaged))
}

// newHolder creates a holder VM with these tags on node, whether or not the project has one there.
func (s *Service) newHolder(ctx context.Context, projectID, node, tags string) (*ClusterResource, error) {
	pool := s.PoolFor(projectID)
	vmid, err := s.C.NextID(ctx)
	if err != nil {
		return nil, err
	}
	// The token holds VM.Config.* only on VMs in the pool, so: created empty, configured once it is in there.
	up, err := s.C.CreateVM(ctx, node, map[string]string{"vmid": strconv.Itoa(vmid), "pool": pool})
	if err != nil {
		if !taskRefused(err) && !strings.Contains(err.Error(), "already exist") {
			s.dropLostCreate(ctx, projectID, vmid)
		}
		return nil, err
	}
	// From here the VM may exist. It holds no disk yet, so a failure removes it, and if that fails too the sync keeps it
	// out of the servers and retries (RemoveBlankVMs).
	ready := false
	defer func() {
		if !ready {
			s.destroyBestEffort(context.WithoutCancel(ctx), node, vmid)
		}
	}()
	if err := s.C.WaitTask(ctx, up); err != nil {
		return nil, err
	}
	up, err = s.C.SetVMConfig(ctx, node, vmid, map[string]string{
		"name": "pc-holder-" + node, "memory": "16", "cores": "1", "onboot": "0", "tags": tags,
	})
	if err != nil {
		return nil, err
	}
	if err := s.C.WaitTask(ctx, up); err != nil {
		return nil, err
	}
	// cluster/resources lags a finished task; callers resolve the VM through it.
	for range 15 {
		vm, err := s.ResolveVM(ctx, projectID, strconv.Itoa(vmid))
		if err == nil && vm != nil {
			ready = true
			return vm, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, fmt.Errorf("pve: holder VM %d not listed yet", vmid)
}

// dropLostCreate removes the VM a create POST with no answer may have made. NextID is racy, so the vmid alone proves nothing:
// only a VM in this pool and as blank as a create leaves it goes, by the same test the sync uses. PVE unreachable for the whole
// wait leaves the VM to the sync's RemoveBlankVMs.
func (s *Service) dropLostCreate(ctx context.Context, projectID string, vmid int) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lostTaskWait)
	defer cancel()
	for {
		// cluster/resources lags, and the task may still be running.
		if vm, err := s.ResolveVM(ctx, projectID, strconv.Itoa(vmid)); err == nil && vm != nil {
			if cfg, err := s.C.GetVMConfig(ctx, vm.Node, vmid); err == nil {
				if blankConfig(cfg) {
					s.destroyBestEffort(ctx, vm.Node, vmid)
				}
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// PlaceVolume picks where a standalone volume goes: the node with most free space on the profile's storage, or
// wantNode. storageFor maps a node to the profile's storage there ("" = not available on it).
func (s *Service) PlaceVolume(ctx context.Context, storageFor func(node string) string, wantNode string, bytes int64, headroomPct float64) (node, storage string, err error) {
	nodes, err := s.C.Nodes(ctx)
	if err != nil {
		return "", "", err
	}
	rows, err := s.C.ClusterResources(ctx, "storage")
	if err != nil {
		return "", "", err
	}
	var best int64 = -1
	for _, n := range nodes {
		if n.Status != "online" || (wantNode != "" && n.Node != wantNode) {
			continue
		}
		st := storageFor(n.Node)
		if st == "" {
			continue
		}
		for _, r := range rows {
			if r.Type != "storage" || r.Storage != st || r.Node != n.Node {
				continue
			}
			state := rowState(r)
			if free := state.Avail; StorageHasRoom(state, bytes, headroomPct) && free > best {
				node, storage, best = n.Node, st, free
			}
		}
	}
	if node == "" {
		return "", "", conflictf("No node has room for a %d GB volume on this storage type", bytes>>30)
	}
	return node, storage, nil
}

// CreateDetachedVolume allocates a volume with no server: a new disk on the node's holder, detached at once so it
// is the same unusedN shape as any other detached volume.
func (s *Service) CreateDetachedVolume(ctx context.Context, projectID, node, storage string, sizeGiB int, headroomPct float64) error {
	release, err := s.lockHolders(ctx, projectID)
	if err != nil {
		return err
	}
	defer release()
	holder, err := s.holderOn(ctx, projectID, node)
	if err != nil {
		return err
	}
	slot, err := s.AddVolume(ctx, projectID, strconv.Itoa(holder.VMID), sizeGiB, storage, headroomPct)
	if err != nil {
		return err
	}
	up, err := s.C.SetVMConfig(ctx, node, holder.VMID, nil, slot)
	if err != nil {
		return err
	}
	return s.C.WaitTask(ctx, up)
}

// KeepServerVolumes moves every data volume of a server to its node's holder, attached and detached alike, so the
// server can be deleted without them. Only the boot disk stays behind. Any failure aborts: the caller must not
// delete the server then. PVE reassigns a disk only within a node, which is why the holder is per node.
//
// A move that may still be running keeps the holder lock until it expires (holderLockMaxFor, far past any rename): the next
// operation in this project could see the holder empty and destroy it as the disk lands. That blocks the project's volume
// changes for those minutes, and only a lost answer from PVE gets there.
func (s *Service) KeepServerVolumes(ctx context.Context, projectID, externalID string) error {
	vm, err := s.ResolveVM(ctx, projectID, externalID)
	if err != nil || vm == nil {
		return err
	}
	cfg, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	if err != nil {
		return err
	}
	var data []VMDisk
	for _, d := range ParseDisks(cfg) {
		if !d.Boot && !strings.Contains(d.VolID, "cloudinit") {
			data = append(data, d)
		}
	}
	if len(data) == 0 {
		return nil
	}
	release, err := s.lockHolders(ctx, projectID)
	if err != nil {
		return err
	}
	moveInDoubt := false
	defer func() {
		if !moveInDoubt {
			release()
		}
	}()
	if vm.Status == "running" {
		up, err := s.C.StopVM(ctx, vm.Node, vm.VMID)
		if err != nil {
			return err
		}
		if err := s.C.WaitTask(ctx, up); err != nil {
			return err
		}
	}
	holder, err := s.holderOn(ctx, projectID, vm.Node)
	if err != nil {
		return err
	}
	for _, d := range data {
		slot := d.Slot
		if !strings.HasPrefix(slot, "unused") {
			up, err := s.C.SetVMConfig(ctx, vm.Node, vm.VMID, nil, slot)
			if err != nil {
				return err
			}
			if err := s.C.WaitTask(ctx, up); err != nil {
				return err
			}
			if cfg, err = s.C.GetVMConfig(ctx, vm.Node, vm.VMID); err != nil {
				return err
			}
			slot = ""
			for _, x := range ParseDisks(cfg) {
				if x.VolID == d.VolID {
					slot = x.Slot
				}
			}
			if slot == "" {
				return fmt.Errorf("pve: disk %s vanished while detaching", d.VolID)
			}
		}
		hcfg, err := s.C.GetVMConfig(ctx, vm.Node, holder.VMID)
		if err != nil {
			return err
		}
		free := ""
		for i := 0; i < 256 && free == ""; i++ {
			if k := "unused" + strconv.Itoa(i); hcfg.Str(k) == "" {
				free = k
			}
		}
		up, err := s.C.MoveDisk(ctx, vm.Node, vm.VMID, MoveDiskOpts{Disk: slot, TargetVMID: holder.VMID, TargetDisk: free})
		if err != nil {
			moveInDoubt = !taskRefused(err)
			return err
		}
		if err := s.C.WaitTask(ctx, up); err != nil {
			moveInDoubt = !s.C.TaskStopped(ctx, up, lostTaskWait)
			return err
		}
	}
	return nil
}

// dropEmptyHolder removes a holder with no disk left. Best effort: an empty holder costs nothing but a vmid, and
// the next delete or the project teardown gets it. The destroy takes every disk on the VM at that moment, so the caller
// holds the holder lock and knows no task is moving a disk onto this holder.
func (s *Service) dropEmptyHolder(ctx context.Context, vm *ClusterResource) {
	cfg, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	if err != nil || !isHolder(cfg.Str("tags")) || len(ParseDisks(cfg)) > 0 {
		return
	}
	if up, err := s.C.DeleteVM(ctx, vm.Node, vm.VMID); err == nil {
		_ = s.C.WaitTask(ctx, up)
	}
}

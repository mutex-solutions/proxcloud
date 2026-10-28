package pve

// volume.go — disk lifecycle. The honest PVE model: a disk always belongs to a
// VM; "detached" = the VM's unusedN entry (still billed); attach-to-another-VM =
// MoveDisk with target-vmid (same node unless shared storage). No standalone
// volume object exists and none is invented.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrStorageFull: the target storage lacks free space for the disk plus the headroom.
var ErrStorageFull = errors.New("pve: not enough free space on storage")

// ConflictError is a volume operation refused up front because PVE could not do it in the
// volume's current state or placement; Msg says what to change and is safe to show (409).
type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }

func conflictf(format string, a ...any) error { return &ConflictError{Msg: fmt.Sprintf(format, a...)} }

// AddVolume allocates a new data disk on a pool VM (SetVMConfig scsiN =
// "<storage>:<sizeGiB>") after checking the storage on the VM's node can take it while
// keeping headroomPct % free (ErrStorageFull otherwise). Returns the new slot; the volid
// lands in the next sync (PVE names it server-side).
func (s *Service) AddVolume(ctx context.Context, projectID, serverExtID string, sizeGiB int, storage string, headroomPct float64) (string, error) {
	vm, err := s.ResolveVM(ctx, projectID, serverExtID)
	if err != nil {
		return "", err
	}
	if vm == nil {
		return "", fmt.Errorf("pve: server %s not found", serverExtID)
	}
	rows, err := s.C.ClusterResources(ctx, "storage")
	if err != nil {
		return "", err
	}
	var st StorageNodeState
	for _, r := range rows {
		if r.Type == "storage" && r.Storage == storage && r.Node == vm.Node {
			st = rowState(r)
		}
	}
	if !StorageHasRoom(st, int64(sizeGiB)<<30, headroomPct) {
		return "", fmt.Errorf("%w: %s on node %s", ErrStorageFull, storage, vm.Node)
	}
	cfg, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	if err != nil {
		return "", err
	}
	slot := ""
	for i := 1; i < 30; i++ {
		k := "scsi" + strconv.Itoa(i)
		if cfg.Str(k) == "" {
			slot = k
			break
		}
	}
	if slot == "" {
		return "", fmt.Errorf("pve: no free disk slot on server %s", serverExtID)
	}
	up, err := s.C.SetVMConfig(ctx, vm.Node, vm.VMID, map[string]string{
		slot: fmt.Sprintf("%s:%d", storage, sizeGiB),
	})
	if err != nil {
		return "", err
	}
	return slot, s.C.WaitTask(ctx, up)
}

// volumeSlot re-resolves a cached volume row's (vmid, slot) against the live VM
// config by volid — the slot may have drifted (detach renumbers to unusedN).
func (s *Service) volumeSlot(ctx context.Context, projectID string, vmid int, volID string) (*ClusterResource, string, error) {
	vm, d, err := s.volumeDisk(ctx, projectID, vmid, volID)
	return vm, d.Slot, err
}

// volumeDisk is volumeSlot returning the whole parsed disk (slot, size, storage).
func (s *Service) volumeDisk(ctx context.Context, projectID string, vmid int, volID string) (*ClusterResource, VMDisk, error) {
	vm, err := s.ResolveVM(ctx, projectID, strconv.Itoa(vmid))
	if err != nil {
		return nil, VMDisk{}, err
	}
	if vm == nil {
		return nil, VMDisk{}, fmt.Errorf("pve: server %d not found", vmid)
	}
	cfg, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	if err != nil {
		return nil, VMDisk{}, err
	}
	for _, d := range ParseDisks(cfg) {
		if d.VolID == volID {
			return vm, d, nil
		}
	}
	return nil, VMDisk{}, fmt.Errorf("pve: disk %s not found on server %d", volID, vmid)
}

// ExtendVolume grows a disk to sizeGiB (grow-only — PVE cannot shrink).
func (s *Service) ExtendVolume(ctx context.Context, projectID string, vmid int, volID string, sizeGiB int) error {
	vm, slot, err := s.volumeSlot(ctx, projectID, vmid, volID)
	if err != nil {
		return err
	}
	if strings.HasPrefix(slot, "unused") {
		return fmt.Errorf("pve: attach the disk before extending it")
	}
	up, err := s.C.ResizeDisk(ctx, vm.Node, vm.VMID, slot, fmt.Sprintf("%dG", sizeGiB))
	if err != nil {
		return err
	}
	return s.C.WaitTask(ctx, up)
}

// DetachVolume unlinks a disk from its slot — PVE moves it to unusedN (kept +
// still billed; delete goes through the server's destroy or a future purge op).
func (s *Service) DetachVolume(ctx context.Context, projectID string, vmid int, volID string) error {
	vm, slot, err := s.volumeSlot(ctx, projectID, vmid, volID)
	if err != nil {
		return err
	}
	if strings.HasPrefix(slot, "unused") {
		return nil // already detached
	}
	if strings.HasSuffix(slot, "0") && (strings.HasPrefix(slot, "scsi") || strings.HasPrefix(slot, "virtio")) {
		return fmt.Errorf("pve: cannot detach the boot disk")
	}
	up, err := s.C.SetVMConfig(ctx, vm.Node, vm.VMID, nil, slot)
	if err != nil {
		return err
	}
	return s.C.WaitTask(ctx, up)
}

// AttachVolume re-links an unused disk into the next free scsi slot of the SAME
// VM, or reassigns it to another pool VM (MoveDisk target-vmid). PVE's disk reassign needs
// both VMs on the same node, even on shared storage (qemu-server move_vm_disk: "Both VMs need
// to be on the same node"), so a target on another node is refused up front (ConflictError).
func (s *Service) AttachVolume(ctx context.Context, projectID string, vmid int, volID string, targetServerExtID string) error {
	vm, slot, err := s.volumeSlot(ctx, projectID, vmid, volID)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(slot, "unused") {
		return fmt.Errorf("pve: disk is already attached")
	}
	// Same VM: re-link by volid.
	if targetServerExtID == "" || targetServerExtID == strconv.Itoa(vmid) {
		cfg, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
		if err != nil {
			return err
		}
		free := ""
		for i := 1; i < 30; i++ {
			k := "scsi" + strconv.Itoa(i)
			if cfg.Str(k) == "" {
				free = k
				break
			}
		}
		if free == "" {
			return fmt.Errorf("pve: no free disk slot")
		}
		up, err := s.C.SetVMConfig(ctx, vm.Node, vm.VMID, map[string]string{free: volID})
		if err != nil {
			return err
		}
		return s.C.WaitTask(ctx, up)
	}
	// Another VM: disk reassign. Ownership of the target is re-verified too.
	target, err := s.ResolveVM(ctx, projectID, targetServerExtID)
	if err != nil {
		return err
	}
	if target == nil {
		return fmt.Errorf("pve: target server %s not found", targetServerExtID)
	}
	if target.Node != vm.Node {
		return conflictf("Server %s runs on node %s and this volume on node %s; a volume can only move to a server on the same node. "+
			"Attach it to a server on %s", target.Name, target.Node, vm.Node, vm.Node)
	}
	tcfg, err := s.C.GetVMConfig(ctx, target.Node, target.VMID)
	if err != nil {
		return err
	}
	free := ""
	for i := 1; i < 30; i++ {
		k := "scsi" + strconv.Itoa(i)
		if tcfg.Str(k) == "" {
			free = k
			break
		}
	}
	if free == "" {
		return conflictf("Server %s has no free disk slot", target.Name)
	}
	up, err := s.C.MoveDisk(ctx, vm.Node, vm.VMID, MoveDiskOpts{
		Disk: slot, TargetVMID: target.VMID, TargetDisk: free,
	})
	if err != nil {
		return err
	}
	return s.C.WaitTask(ctx, up)
}

// AttachTargets lists the pool VMs a volume can be attached to now: none while it is
// attached (detach first); otherwise every pool VM on its VM's node (its own included).
func (s *Service) AttachTargets(ctx context.Context, projectID string, vmid int, volID string) ([]ClusterResource, error) {
	vm, slot, err := s.volumeSlot(ctx, projectID, vmid, volID)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(slot, "unused") {
		return []ClusterResource{}, nil
	}
	vms, _, err := s.poolVMs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := []ClusterResource{}
	for _, t := range vms {
		if t.Node == vm.Node {
			out = append(out, t)
		}
	}
	return out, nil
}

// DeleteVolume destroys a detached disk: dropping its unusedN entry from the VM config
// frees the volume on its storage. An attached disk must be detached first (ConflictError).
// TODO(verify-lab): PVE docs say unlinking unused[n] always removes the volume physically;
// confirm POST /config delete=unusedN does the same (and what happens when a VM snapshot
// still references the volume).
func (s *Service) DeleteVolume(ctx context.Context, projectID string, vmid int, volID string) error {
	vm, slot, err := s.volumeSlot(ctx, projectID, vmid, volID)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(slot, "unused") {
		return conflictf("Detach the volume before deleting it")
	}
	up, err := s.C.SetVMConfig(ctx, vm.Node, vm.VMID, nil, slot)
	if err != nil {
		return err
	}
	return s.C.WaitTask(ctx, up)
}

// RetypeVolume moves a disk to another storage of its VM's node (move_disk storage=,
// delete=1, so the source copy is removed once the copy succeeds). The target must hold VM
// disks and be active on that node with room for the disk plus headroomPct (ConflictError
// otherwise). The volid changes; the next volume sync picks up the new one.
// A detached unusedN disk is copied offline even while the VM runs (qemu-server clone_disk
// skips drive-mirror for unused drives). PVE refuses delete=1 while a VM snapshot still
// references the disk; that error surfaces as is.
// ponytail: waits for the copy inside the request; a large disk needs async task tracking.
func (s *Service) RetypeVolume(ctx context.Context, projectID string, vmid int, volID, storage string, headroomPct float64) error {
	vm, d, err := s.volumeDisk(ctx, projectID, vmid, volID)
	if err != nil {
		return err
	}
	if d.Storage == storage {
		return conflictf("The volume is already on storage %s", storage)
	}
	caps, err := s.C.DetectStorage(ctx)
	if err != nil {
		return err
	}
	c, ok := caps[storage]
	st := c.Nodes[vm.Node]
	if !ok || !c.HasContent("images") || !st.Active {
		return conflictf("Storage %s cannot hold disks on node %s", storage, vm.Node)
	}
	if !StorageHasRoom(st, int64(d.SizeGiB)<<30, headroomPct) {
		return conflictf("Not enough free space for a %d GB volume on storage %s", d.SizeGiB, storage)
	}
	up, err := s.C.MoveDisk(ctx, vm.Node, vm.VMID, MoveDiskOpts{Disk: d.Slot, Storage: storage, Delete: true})
	if err != nil {
		return err
	}
	return s.C.WaitTask(ctx, up)
}

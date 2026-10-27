package pve

// volume.go — disk lifecycle. The honest PVE model: a disk always belongs to a
// VM; "detached" = the VM's unusedN entry (still billed); attach-to-another-VM =
// MoveDisk with target-vmid (same node unless shared storage). No standalone
// volume object exists and none is invented.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// AddVolume allocates a new data disk on a pool VM (SetVMConfig scsiN =
// "<storage>:<sizeGiB>"). Returns the new slot; the volid lands in the next sync
// (PVE names it server-side).
func (s *Service) AddVolume(ctx context.Context, projectID, serverExtID string, sizeGiB int, storage string) (string, error) {
	vm, err := s.ResolveVM(ctx, projectID, serverExtID)
	if err != nil {
		return "", err
	}
	if vm == nil {
		return "", fmt.Errorf("pve: server %s not found", serverExtID)
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
	vm, err := s.ResolveVM(ctx, projectID, strconv.Itoa(vmid))
	if err != nil {
		return nil, "", err
	}
	if vm == nil {
		return nil, "", fmt.Errorf("pve: server %d not found", vmid)
	}
	cfg, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	if err != nil {
		return nil, "", err
	}
	for _, d := range ParseDisks(cfg) {
		if d.VolID == volID {
			return vm, d.Slot, nil
		}
	}
	return nil, "", fmt.Errorf("pve: disk %s not found on server %d", volID, vmid)
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
// VM, or reassigns it to another pool VM (MoveDisk target-vmid; PVE enforces the
// same-node/shared-storage constraint and its error surfaces honestly).
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
		return fmt.Errorf("pve: no free disk slot on target server")
	}
	up, err := s.C.MoveDisk(ctx, vm.Node, vm.VMID, MoveDiskOpts{
		Disk: slot, TargetVMID: target.VMID, TargetDisk: free,
	})
	if err != nil {
		return err
	}
	return s.C.WaitTask(ctx, up)
}

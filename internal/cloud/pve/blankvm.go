package pve

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// A holder VM is created empty and configured once it is in the pool. If the portal never gets to the second step (PVE
// stayed away, the pod restarted, the destroy failed too) the VM stays: no name, no tags, no disk. It is nobody's server.
// Everything here errs towards leaving a VM alone: a customer's VM must never count as blank.

// blankVMGrace outlives the holder lock's ceiling, so a create still under way has lost its lock long before its VM can
// be removed. The worst it then meets is a failed request, since a blank VM holds nothing.
var blankVMGrace = holderLockMaxFor + 5*time.Minute

// blankRow: the row of a VM nobody has configured. "VM <vmid>" is what PVE shows for no name, and no name can be set to
// it, since a VM's name is a DNS name.
func blankRow(r ClusterResource) bool {
	return r.Type == "qemu" && r.Template != 1 && r.Name == "VM "+strconv.Itoa(r.VMID) &&
		r.Tags == "" && r.MaxDisk == 0 && r.Status == "stopped"
}

// blankConfig: the config holds what PVE writes for a create with only vmid and pool (read on 9.2), and nothing else. Any
// other key means someone configured it, or something is at work on it (a lock, a snapshot, a disk, a NIC).
func blankConfig(cfg VMConfig) bool {
	for k := range cfg {
		switch k {
		case "digest", "meta", "smbios1", "vmgenid":
		case "boot":
			if strings.TrimSpace(cfg.Str(k)) != "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// configCreated reads when PVE created the VM off its meta key.
func configCreated(cfg VMConfig) (time.Time, bool) {
	for _, kv := range splitCommaKV(cfg.Str("meta")) {
		if kv[0] == "ctime" {
			if sec, err := strconv.ParseInt(kv[1], 10, 64); err == nil && sec > 0 {
				return time.Unix(sec, 0), true
			}
		}
	}
	return time.Time{}, false
}

// RemoveBlankVMs retries what a failed create left in the project's pool, once it is past any create still under way. It
// holds the holder lock, as only a holder create makes these. A failure is logged and left for the next sync.
func (s *Service) RemoveBlankVMs(ctx context.Context, projectID string) (int, error) {
	if s.HolderOperations != nil {
		// A blank VM + elapsed grace is not evidence that a lost create stopped.
		// New holders carry a preselected UUID; legacy blanks are operator-only.
		_, release, err := s.beginHolders(ctx, projectID)
		if err != nil {
			return 0, err
		}
		defer release()
		return 0, nil
	}
	rows, err := s.C.ClusterResources(ctx, "vm")
	if err != nil {
		return 0, err
	}
	pool := s.PoolFor(projectID)
	var candidates []ClusterResource
	for _, r := range rows {
		if r.Pool != pool || !blankRow(r) || r.NodeDown {
			continue
		}
		if s.blankPastGrace(ctx, r) {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	ctx, release, err := s.beginHolders(ctx, projectID)
	if err != nil {
		return 0, err
	}
	defer release()
	// The sync goes through projects one at a time, so a destroy that hangs must not hold it, or this project's lock. What is
	// left goes on the next sync.
	ctx, cancel := context.WithTimeout(ctx, lostTaskWait)
	defer cancel()
	removed := 0
	for _, r := range candidates {
		if !s.blankPastGrace(ctx, r) { // it may have been configured while this waited for the lock
			continue
		}
		up, err := s.C.DeleteVM(ctx, r.Node, r.VMID)
		if err == nil {
			err = s.C.WaitTask(ctx, up)
		}
		if err != nil {
			slog.Warn("pve: remove blank VM, retrying on the next sync", "vmid", r.VMID, "node", r.Node, "pool", pool, "err", err)
			continue
		}
		slog.Info("pve: removed a blank VM a failed create left", "vmid", r.VMID, "node", r.Node, "pool", pool)
		removed++
	}
	return removed, nil
}

func (s *Service) blankPastGrace(ctx context.Context, r ClusterResource) bool {
	cfg, err := s.C.GetVMConfig(ctx, r.Node, r.VMID)
	if err != nil || !blankConfig(cfg) {
		return false
	}
	created, ok := configCreated(cfg)
	return ok && time.Since(created) >= blankVMGrace
}

package pve

// backup.go — server backups on PVE's vzdump, to one backup-capable store of the provider (NFS, CephFS, PBS).
// Tenants have no PVE access, so the notes stamp vzdump writes is the ownership proof: every action lists
// the store and only goes on with an archive carrying this project's stamp, never with a volid as given.
//
// A backup belongs to its server and goes when the server goes: PVE lets the token see and delete an archive only
// through VM.Backup on its VM, which is held through the VM's pool. An archive of a VM destroyed outside the portal
// therefore vanishes from the token's listing, and no error reports it (lab, PVE 9.2).
// ponytail: such an archive stays on the store until an operator removes it (its notes name the project); a periodic
// root-side sweep or a narrow /vms ACL would close that, add one if VMs get destroyed behind the portal in practice.

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// backupTimeout bounds one backup or restore. vzdump's own wait for the node's backup lock stays below it,
	// so a queue fails as a task instead of as a timeout on a task still running.
	backupTimeout       = 2 * time.Hour
	backupLockWaitMin   = 60
	maxBackupsPerServer = 7 // ponytail: a fixed cap keeps tenants from filling the shared store, make it a quota once backups are billed
)

// ServerBackup is one archive as the API shows it. ID is the volid the other actions take.
type ServerBackup struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"createdAt"`
	SizeBytes int64  `json:"sizeBytes"`
}

// backupStamp is the prefix of an archive's notes. The trailing space ends the project id.
func backupStamp(projectID string) string { return "proxcloud project=" + projectID + " " }

// backupDenied names the privilege when PVE refused one: the operator can act on that, a bare 403 tells nobody anything.
func backupDenied(err error, store string) error {
	if IsForbidden(err) {
		return conflictf("The provider's API token cannot use backups (it needs VM.Backup on the server and Datastore.AllocateSpace on storage %s)", store)
	}
	return err
}

// Vzdump backs one VM up to storage, keeping every archive there: prune-backups needs Datastore.Allocate on the
// store and remove would apply the store's retention to tenants' archives behind their back.
func (c *Client) Vzdump(ctx context.Context, node string, vmid int, storage, notes string) (UPID, error) {
	return c.postTask(ctx, "/nodes/"+node+"/vzdump", formVals{
		"vmid": strconv.Itoa(vmid), "storage": storage, "mode": "snapshot", "compress": "zstd", "remove": "0",
		"lockwait": strconv.Itoa(backupLockWaitMin), "notes-template": notes,
	}.values())
}

// BackupConfig reads the VM config inside an archive. Needs VM.Backup on the VM, like the restore itself,
// which makes it the check that refuses before anything was stopped.
func (c *Client) BackupConfig(ctx context.Context, node, volid string) (VMConfig, error) {
	var raw string
	if err := c.get(ctx, "/nodes/"+node+"/vzdump/extractconfig", formVals{"volume": volid}.values(), &raw); err != nil {
		return nil, err
	}
	return parseVMConf(raw), nil
}

// parseVMConf reads a qemu-server config file's current section; snapshots follow in [name] sections.
func parseVMConf(raw string) VMConfig {
	cfg := VMConfig{}
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "[") {
			break
		}
		if k, v, ok := strings.Cut(line, ": "); ok && !strings.HasPrefix(k, "#") {
			cfg[k] = v
		}
	}
	return cfg
}

// ownedBackups keeps the archives stamped for projectID, newest first.
func ownedBackups(rows []StorageContent, projectID string) []ServerBackup {
	stamp := backupStamp(projectID)
	out := []ServerBackup{}
	for _, r := range rows {
		if strings.HasPrefix(r.Notes, stamp) {
			out = append(out, ServerBackup{ID: r.VolID, CreatedAt: r.CTime, SizeBytes: r.Size})
		}
	}
	slices.SortFunc(out, func(a, b ServerBackup) int { return int(b.CreatedAt - a.CreatedAt) })
	return out
}

// ListServerBackups lists a pool VM's archives on store.
func (s *Service) ListServerBackups(ctx context.Context, projectID, externalID, store string) ([]ServerBackup, error) {
	vm, err := s.resolveOwned(ctx, projectID, externalID)
	if err != nil {
		return nil, err
	}
	rows, err := s.C.StorageContents(ctx, vm.Node, store, "backup", vm.VMID)
	if err != nil {
		return nil, err
	}
	return ownedBackups(rows, projectID), nil
}

// CreateServerBackup archives a pool VM to the store and waits for it. A store that is not shared would
// leave the archives behind when the VM migrates, so it is refused rather than half working.
func (s *Service) CreateServerBackup(ctx context.Context, projectID, externalID, store string, capability StorageCapability, headroomPct float64) error {
	ctx, cancel := context.WithTimeout(ctx, backupTimeout)
	defer cancel()
	vm, err := s.resolveOwned(ctx, projectID, externalID)
	if err != nil {
		return err
	}
	if !capability.HasContent("backup") || !capability.Shared {
		return conflictf("Storage %s cannot hold server backups: it must be shared and allow backup content", store)
	}
	existing, err := s.ListServerBackups(ctx, projectID, externalID, store)
	if err != nil {
		return err
	}
	if len(existing) >= maxBackupsPerServer {
		return conflictf("Server %s has %d backups, the most it can keep. Delete one first", vm.Name, len(existing))
	}
	cfg, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	if err != nil {
		return err
	}
	if !StorageHasRoom(capability.Nodes[vm.Node], int64(diskGbOf(cfg))<<30, headroomPct) {
		return conflictf("Backup storage %s has no room for server %s right now. Try again later", store, vm.Name)
	}
	up, err := s.C.Vzdump(ctx, vm.Node, vm.VMID, store, backupStamp(projectID)+"server={{guestname}}")
	if err != nil {
		return backupDenied(err, store)
	}
	return s.C.WaitTask(ctx, up)
}

// restoreConflict says what a restore over cur would lose beyond the tenant's own rollback: PVE destroys every disk
// of the VM first, a detached one too, and only brings back what the archive holds. NICs must match as well, or a
// public address the portal holds would lose its NIC (or a restored NIC would keep a released address).
func restoreConflict(cur, backup VMConfig) string {
	for _, d := range ParseDisks(cur) {
		if strings.HasPrefix(d.Slot, "unused") || backup[d.Slot] == nil {
			return fmt.Sprintf("Disk %s is not in this backup and would be lost. Delete it or move it to another server first", d.Slot)
		}
	}
	if !maps.Equal(nicMACs(cur), nicMACs(backup)) {
		return "The server's network interfaces changed since this backup. Put them back first, or take a new backup"
	}
	return ""
}

var nicKeyRe = regexp.MustCompile(`^net\d+$`)

// nicMACs maps each NIC slot to its model=MAC part, the NIC's identity.
func nicMACs(cfg VMConfig) map[string]string {
	out := map[string]string{}
	for k := range cfg {
		if nicKeyRe.MatchString(k) {
			out[k], _, _ = strings.Cut(cfg.Str(k), ",")
		}
	}
	return out
}

// RestoreServerBackup replaces a pool VM's disks and config with an archive of it; a running VM comes back running.
// Past the POST there is no way back: PVE has destroyed the disks, so a failed task leaves the VM without them
// and only another restore helps.
func (s *Service) RestoreServerBackup(ctx context.Context, projectID, externalID, store, volid string) error {
	ctx, cancel := context.WithTimeout(ctx, backupTimeout)
	defer cancel()
	vm, err := s.resolveOwned(ctx, projectID, externalID)
	if err != nil {
		return err
	}
	if HasTag(vm.Tags, tagProtected) {
		return conflictf("Server %s is protected, and its replica would not follow a restore. Remove the protection first", vm.Name)
	}
	if err := s.ownBackup(ctx, projectID, externalID, store, volid); err != nil {
		return err
	}
	backup, err := s.C.BackupConfig(ctx, vm.Node, volid)
	if err != nil {
		return backupDenied(err, store)
	}
	cur, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	if err != nil {
		return err
	}
	if msg := restoreConflict(cur, backup); msg != "" {
		return conflictf("%s", msg)
	}
	running := vm.Status == "running"
	if vm.PoweredOn() {
		up, err := s.C.StopVM(ctx, vm.Node, vm.VMID)
		if err == nil {
			err = s.C.WaitTask(ctx, up)
		}
		if err != nil {
			return fmt.Errorf("pve restore: stop: %w", err)
		}
	}
	up, err := s.C.CreateVM(ctx, vm.Node, map[string]string{"vmid": strconv.Itoa(vm.VMID), "archive": volid, "force": "1"})
	if err != nil {
		// Refused before it began: the disks are intact, so the server goes back to how it was.
		if running {
			if su, serr := s.C.StartVM(ctx, vm.Node, vm.VMID); serr == nil {
				_ = s.C.WaitTask(ctx, su)
			}
		}
		return backupDenied(err, store)
	}
	if err := s.C.WaitTask(ctx, up); err != nil {
		return err
	}
	if running {
		su, err := s.C.StartVM(ctx, vm.Node, vm.VMID)
		if err == nil {
			err = s.C.WaitTask(ctx, su)
		}
		if err != nil {
			return fmt.Errorf("pve restore: restart: %w", err)
		}
	}
	return nil
}

// ownBackup is the ownership check of the actions that take a volid.
func (s *Service) ownBackup(ctx context.Context, projectID, externalID, store, volid string) error {
	backups, err := s.ListServerBackups(ctx, projectID, externalID, store)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(backups, func(b ServerBackup) bool { return b.ID == volid }) {
		return conflictf("This server has no backup %s", volid)
	}
	return nil
}

// DeleteServerBackup removes one archive of a pool VM.
func (s *Service) DeleteServerBackup(ctx context.Context, projectID, externalID, store, volid string) error {
	vm, err := s.resolveOwned(ctx, projectID, externalID)
	if err != nil {
		return err
	}
	if err := s.ownBackup(ctx, projectID, externalID, store, volid); err != nil {
		return err
	}
	return s.deleteArchive(ctx, vm.Node, store, volid)
}

func (s *Service) deleteArchive(ctx context.Context, node, store, volid string) error {
	up, err := s.C.deleteTask(ctx, "/nodes/"+node+"/storage/"+url.PathEscape(store)+"/content/"+url.PathEscape(volid), nil)
	if err == nil {
		err = s.C.WaitTask(ctx, up)
	}
	return backupDenied(err, store)
}

// DeleteServerBackups removes every archive of a pool VM, before the VM goes (see the note on top). A VM already
// gone has nothing that can be resolved, so nothing to do here.
func (s *Service) DeleteServerBackups(ctx context.Context, projectID, externalID, store string) error {
	vm, err := s.ResolveVM(ctx, projectID, externalID)
	if err != nil || vm == nil {
		return err
	}
	backups, err := s.ListServerBackups(ctx, projectID, externalID, store)
	if err != nil {
		return err
	}
	for _, b := range backups {
		if err := s.deleteArchive(ctx, vm.Node, store, b.ID); err != nil {
			return err
		}
	}
	return nil
}

// DeleteProjectBackups removes every archive of the project the token can see, while its VMs still exist. Fail-closed
// like the rest of teardown: one that lists but cannot go stops it, and the error names it.
func (s *Service) DeleteProjectBackups(ctx context.Context, projectID, store string) error {
	nodes, err := s.C.Nodes(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(nodes, func(n Node) bool { return n.Status == "online" })
	if i < 0 {
		return fmt.Errorf("pve teardown: no node online to list storage %s", store)
	}
	rows, err := s.C.StorageContents(ctx, nodes[i].Node, store, "backup", 0)
	if err != nil {
		return err
	}
	for _, b := range ownedBackups(rows, projectID) {
		if err := s.deleteArchive(ctx, nodes[i].Node, store, b.ID); err != nil {
			return fmt.Errorf("pve teardown: backup %s: %w", b.ID, err)
		}
	}
	return nil
}

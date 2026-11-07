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

// RestoreVolume is what the portal knows about one disk of a server: the name a tenant knows it by, and when its data
// came to this server. Since comes from the VOLUME row: a disk from elsewhere is a new row, one that stays (extended,
// detached and attached again, recreated by a restore) keeps its row, and a disk only a restore brought back dates from
// that backup.
type RestoreVolume struct {
	Name  string
	Since time.Time
}

// RestoreGuard runs right before a restore goes ahead, on what the server has now and what the archive would make of
// it. It answers with the server's volumes by volid, or refuses with an error.
type RestoreGuard func(ctx context.Context, cur, archive VMConfig) (map[string]RestoreVolume, error)

// archivedDisks is what the archive holds and brings back: PVE leaves out detached disks and any with backup=0.
func archivedDisks(archive VMConfig) []VMDisk {
	var out []VMDisk
	for _, d := range ParseDisks(archive) {
		if !strings.HasPrefix(d.Slot, "unused") && !slices.Contains(strings.Split(archive.Str(d.Slot), ","), "backup=0") {
			out = append(out, d)
		}
	}
	return out
}

// restoreConflict says what a restore over cur would lose beyond the tenant's own rollback: PVE destroys every disk
// of the VM first, a detached one too, and only brings back what the archive holds. NICs must match as well, or a
// public address the portal holds would lose its NIC (or a restored NIC would keep a released address).
//
// A disk counts as held only if the archive names its volid and the disk was on this server when the backup started.
// A slot says nothing, since the next disk takes the free one. A volid alone says nothing either: PVE names a disk with
// the lowest free number, so a disk added after the first one left gets that one's name (lab, PVE 9.2).
// The boot disk is the exception. It cannot leave its server, so rolling it back is what a restore is, a rebuild included.
// ponytail: this also refuses a disk that did leave and came back, or was retyped, though the archive has an older copy.
// Moving it away or taking a new backup gets past that; a per-volume id on the row would tell them apart.
func restoreConflict(cur, archive VMConfig, archivedAt int64, vols map[string]RestoreVolume) string {
	held := map[string]bool{}
	for _, d := range archivedDisks(archive) {
		held[d.VolID] = true
	}
	disks := ParseDisks(cur)
	slices.SortFunc(disks, func(a, b VMDisk) int { return strings.Compare(a.Slot, b.Slot) })
	for _, d := range disks {
		if d.Boot && archive[d.Slot] != nil {
			continue
		}
		v := vols[d.VolID]
		if strings.HasPrefix(d.Slot, "unused") || !held[d.VolID] || v.Since.IsZero() || v.Since.Unix() > archivedAt {
			name := v.Name
			if name == "" {
				name = d.Slot
			}
			return fmt.Sprintf("Volume %s is not in this backup and would be lost. Delete it, move it to another server, or take a new backup first", name)
		}
	}
	if !maps.Equal(nicMACs(cur), nicMACs(archive)) {
		return "The server's network interfaces changed since this backup. Put them back first, or take a new backup"
	}
	return ""
}

// RestoreDemand is what a restore adds to the server as it is now, per quota key. Negative when it would free some.
func RestoreDemand(cur, archive VMConfig) (vcpus, ramMb, diskGb int) {
	disk := 0
	for _, d := range archivedDisks(archive) {
		disk += d.SizeGiB
	}
	return configVCPUs(archive) - configVCPUs(cur), configRAMMb(archive) - configRAMMb(cur), disk - diskGbOf(cur)
}

// configVCPUs is the vCPU count of a config: PVE's defaults are one socket and one core.
func configVCPUs(cfg VMConfig) int { return max(cfg.Int("sockets"), 1) * max(cfg.Int("cores"), 1) }

// configRAMMb is the memory of a config, "2048" or "current=2048,...". PVE's default is 512.
func configRAMMb(cfg VMConfig) int {
	first, _, _ := strings.Cut(cfg.Str("memory"), ",")
	mb, err := strconv.Atoi(strings.TrimPrefix(first, "current="))
	if err != nil {
		return 512
	}
	return mb
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
// and only another restore helps. guard runs before the server stops and again once it has, since a stop can take
// minutes and a disk added meanwhile would go too. It answers with when the archive was taken.
func (s *Service) RestoreServerBackup(ctx context.Context, projectID, externalID, store, volid string, guard RestoreGuard) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, backupTimeout)
	defer cancel()
	vm, err := s.resolveOwned(ctx, projectID, externalID)
	if err != nil {
		return time.Time{}, err
	}
	if HasTag(vm.Tags, tagProtected) {
		return time.Time{}, conflictf("Server %s is protected, and its replica would not follow a restore. Remove the protection first", vm.Name)
	}
	owned, err := s.ownBackup(ctx, projectID, externalID, store, volid)
	if err != nil {
		return time.Time{}, err
	}
	archivedAt := time.Unix(owned.CreatedAt, 0)
	backup, err := s.C.BackupConfig(ctx, vm.Node, volid)
	if err != nil {
		return archivedAt, backupDenied(err, store)
	}
	check := func() error {
		cur, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
		if err != nil {
			return err
		}
		vols, err := guard(ctx, cur, backup)
		if err != nil {
			return err
		}
		if msg := restoreConflict(cur, backup, owned.CreatedAt, vols); msg != "" {
			return conflictf("%s", msg)
		}
		return nil
	}
	if err := check(); err != nil {
		return archivedAt, err
	}
	running := vm.Status == "running"
	restart := func() {
		if running {
			if su, serr := s.C.StartVM(ctx, vm.Node, vm.VMID); serr == nil {
				_ = s.C.WaitTask(ctx, su)
			}
		}
	}
	if running {
		up, err := s.C.StopVM(ctx, vm.Node, vm.VMID)
		if err == nil {
			err = s.C.WaitTask(ctx, up)
		}
		if err != nil {
			return archivedAt, fmt.Errorf("pve restore: stop: %w", err)
		}
		if err := check(); err != nil {
			restart()
			return archivedAt, err
		}
	}
	up, err := s.C.CreateVM(ctx, vm.Node, map[string]string{"vmid": strconv.Itoa(vm.VMID), "archive": volid, "force": "1"})
	if err != nil {
		// Refused before it began: the disks are intact, so the server goes back to how it was.
		restart()
		return archivedAt, backupDenied(err, store)
	}
	if err := s.C.WaitTask(ctx, up); err != nil {
		return archivedAt, err
	}
	if running {
		su, err := s.C.StartVM(ctx, vm.Node, vm.VMID)
		if err == nil {
			err = s.C.WaitTask(ctx, su)
		}
		if err != nil {
			return archivedAt, fmt.Errorf("pve restore: restart: %w", err)
		}
	}
	return archivedAt, nil
}

// ownBackup is the ownership check of the actions that take a volid.
func (s *Service) ownBackup(ctx context.Context, projectID, externalID, store, volid string) (ServerBackup, error) {
	backups, err := s.ListServerBackups(ctx, projectID, externalID, store)
	if err != nil {
		return ServerBackup{}, err
	}
	i := slices.IndexFunc(backups, func(b ServerBackup) bool { return b.ID == volid })
	if i < 0 {
		return ServerBackup{}, conflictf("This server has no backup %s", volid)
	}
	return backups[i], nil
}

// DeleteServerBackup removes one archive of a pool VM.
func (s *Service) DeleteServerBackup(ctx context.Context, projectID, externalID, store, volid string) error {
	vm, err := s.resolveOwned(ctx, projectID, externalID)
	if err != nil {
		return err
	}
	if _, err := s.ownBackup(ctx, projectID, externalID, store, volid); err != nil {
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

// DeleteServerBackups removes every archive of a pool VM, as the last step before the VM goes: nothing brings an archive
// back, and PVE only lets the token delete one while the VM exists (see the note on top). So a locked VM, whose destroy
// PVE would refuse, keeps them. A VM already gone has nothing that can be resolved, so nothing to do here.
func (s *Service) DeleteServerBackups(ctx context.Context, projectID, externalID, store string) error {
	vm, err := s.ResolveVM(ctx, projectID, externalID)
	if err != nil || vm == nil {
		return err
	}
	cfg, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	if err != nil {
		return err
	}
	if cfg.Str("lock") != "" {
		return conflictf("Server %s is busy with another operation. Try again in a moment", vm.Name)
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

// RetireServer deletes a server for good, in the one order that loses nothing on a refusal. Protection first: HA would
// restart the VM, and a replicated disk cannot leave it. Then the volumes that outlive it; if one cannot move the server
// stays, and its data with it. Its backups next, since nothing brings one back, and they cannot wait for the destroy
// (see DeleteServerBackups). backupStore "" = the provider keeps no backups.
func (s *Service) RetireServer(ctx context.Context, projectID, externalID, backupStore string, keepVolumes bool) error {
	if err := s.DisableProtection(ctx, projectID, externalID); err != nil {
		return fmt.Errorf("disable protection: %w", err)
	}
	if keepVolumes {
		if err := s.KeepServerVolumes(ctx, projectID, externalID); err != nil {
			return fmt.Errorf("keep the server's volumes: %w", err)
		}
	}
	if backupStore != "" {
		if err := s.DeleteServerBackups(ctx, projectID, externalID, backupStore); err != nil {
			return fmt.Errorf("delete the server's backups: %w", err)
		}
	}
	return s.DeleteServer(ctx, projectID, externalID)
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

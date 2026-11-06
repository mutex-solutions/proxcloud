package pve

// protect.go — the "Protected" add-on: ZFS replication to a second node plus an HA resource with a strict
// node-affinity rule over those two nodes. RPO = the replication schedule plus the sync time; a failover loses what
// was written after the last sync.
//
// Lifecycle: enable creates the job and syncs. HA is added only once a first sync has landed, because HA recovery
// before that would have nothing to start from. A VM carries tag pc-protected from enable on; ReconcileProtection
// (run on read and on every sync) moves it forward and reports its state.

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	tagProtected = "pc-protected"
	// DefaultReplicationSchedule is a pvesr calendar event: every 15 minutes.
	DefaultReplicationSchedule = "*/15"
	replicationScheduleMinutes = 15
)

// Protection states.
const (
	ProtectionSyncing  = "syncing"  // job exists, no first sync yet: billed nothing, no failover
	ProtectionActive   = "active"   // synced and HA managed
	ProtectionDegraded = "degraded" // HA managed, but the last sync failed or is older than twice the schedule
)

// ReplicationJob is one row of /cluster/replication.
type ReplicationJob struct {
	ID       string `json:"id"`
	Guest    int    `json:"guest"`
	Target   string `json:"target"`
	Source   string `json:"source"`
	Schedule string `json:"schedule"`
}

// ReplicationStatus is GET /nodes/{node}/replication/{id}/status.
type ReplicationStatus struct {
	LastSync  int64  `json:"last_sync"`
	Duration  any    `json:"duration"`
	FailCount int    `json:"fail_count"`
	Error     string `json:"error"`
	NextSync  int64  `json:"next_sync"`
}

func (c *Client) ListReplicationJobs(ctx context.Context) ([]ReplicationJob, error) {
	var out []ReplicationJob
	if err := c.get(ctx, "/cluster/replication", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateReplicationJob adds job <guest>-0.
func (c *Client) CreateReplicationJob(ctx context.Context, guest int, target, schedule string) (string, error) {
	id := strconv.Itoa(guest) + "-0"
	err := c.post(ctx, "/cluster/replication", url.Values{
		"id": {id}, "type": {"local"}, "target": {target}, "schedule": {schedule},
	}, nil)
	return id, err
}

// DeleteReplicationJob removes the job and, through pvesr, the replica. force drops the job at once without
// touching the target, for when that node is down; the replica volume then stays on it.
func (c *Client) DeleteReplicationJob(ctx context.Context, id string, force bool) error {
	form := url.Values{}
	if force {
		form.Set("force", "1")
	}
	return c.delete(ctx, "/cluster/replication/"+id, form, nil)
}

func (c *Client) GetReplicationStatus(ctx context.Context, node, id string) (*ReplicationStatus, error) {
	var st ReplicationStatus
	if err := c.get(ctx, "/nodes/"+node+"/replication/"+id+"/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (c *Client) ScheduleReplicationNow(ctx context.Context, node, id string) error {
	return c.post(ctx, "/nodes/"+node+"/replication/"+id+"/schedule_now", url.Values{}, nil)
}

// AddHAResource puts vm:<id> under HA, started. One restart on its node, then relocation.
func (c *Client) AddHAResource(ctx context.Context, vmid int) error {
	return c.post(ctx, "/cluster/ha/resources", url.Values{
		"sid": {"vm:" + strconv.Itoa(vmid)}, "state": {"started"}, "max_restart": {"1"}, "max_relocate": {"1"},
	}, nil)
}

func (c *Client) RemoveHAResource(ctx context.Context, vmid int) error {
	return c.delete(ctx, "/cluster/ha/resources/vm:"+strconv.Itoa(vmid), nil, nil)
}

func haRuleID(vmid int) string { return "pc-vm-" + strconv.Itoa(vmid) }

// AddNodeAffinityRule binds the VM to its two nodes, strictly. Equal priority on purpose: a recovered node must not
// pull the VM back by itself, which would be a second interruption nobody asked for.
func (c *Client) AddNodeAffinityRule(ctx context.Context, vmid int, nodes []string) error {
	return c.post(ctx, "/cluster/ha/rules", url.Values{
		"rule": {haRuleID(vmid)}, "type": {"node-affinity"}, "resources": {"vm:" + strconv.Itoa(vmid)},
		"nodes": {strings.Join(nodes, ",")}, "strict": {"1"},
	}, nil)
}

func (c *Client) DeleteHARule(ctx context.Context, vmid int) error {
	return c.delete(ctx, "/cluster/ha/rules/"+haRuleID(vmid), nil, nil)
}

// HARule is one row of GET /cluster/ha/rules.
type HARule struct {
	Rule string `json:"rule"`
}

func (c *Client) ListHARules(ctx context.Context) ([]HARule, error) {
	var out []HARule
	if err := c.get(ctx, "/cluster/ha/rules", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// HAResource is one row of GET /cluster/ha/resources.
type HAResource struct {
	SID   string `json:"sid"`
	State string `json:"state"`
}

func (c *Client) ListHAResources(ctx context.Context) ([]HAResource, error) {
	var out []HAResource
	if err := c.get(ctx, "/cluster/ha/resources", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// clusterQuorum reads what HA needs from /cluster/status: quorum and the node count.
func (c *Client) clusterQuorum(ctx context.Context) (quorate bool, nodes int, err error) {
	var rows []struct {
		Type    string `json:"type"`
		Quorate int    `json:"quorate"`
		Nodes   int    `json:"nodes"`
	}
	if err = c.get(ctx, "/cluster/status", nil, &rows); err != nil {
		return false, 0, err
	}
	for _, r := range rows {
		if r.Type == "cluster" {
			return r.Quorate == 1, r.Nodes, nil
		}
	}
	return false, 1, nil // a standalone node has no cluster row
}

// hasQDevice: a 2-node cluster has HA quorum only with one.
func (c *Client) hasQDevice(ctx context.Context) bool {
	var out map[string]any
	if err := c.get(ctx, "/cluster/config/qdevice", nil, &out); err != nil {
		return false
	}
	return len(out) > 0
}

// ProtectionReady says whether this cluster can run the add-on at all. reason is safe to show when it cannot.
func (s *Service) ProtectionReady(ctx context.Context) (ready bool, reason string, err error) {
	quorate, nodes, err := s.C.clusterQuorum(ctx)
	if err != nil {
		return false, "", err
	}
	switch {
	case nodes < 2:
		return false, "Protection needs at least two nodes", nil
	case !quorate:
		return false, "The cluster has no quorum", nil
	case nodes == 2 && !s.C.hasQDevice(ctx):
		return false, "A two-node cluster needs a QDevice for HA", nil
	}
	return true, "", nil
}

// ProtectableDisks checks every disk of a VM can replicate: zfspool, and the same storage active on some other
// node. It returns the storages involved. PVE fails the whole job on one disk it cannot replicate, and a disk left out
// of replication would be missing after a failover, so a mix is refused.
func (s *Service) ProtectableDisks(ctx context.Context, vm *ClusterResource, cfg VMConfig) (map[string]bool, error) {
	caps, err := s.C.DetectStorage(ctx)
	if err != nil {
		return nil, err
	}
	if hasPassthrough(cfg) {
		return nil, conflictf("A server with a passed-through device cannot be protected")
	}
	storages := map[string]bool{}
	for _, d := range ParseDisks(cfg) {
		if strings.Contains(d.VolID, "cloudinit") {
			continue // regenerated from the VM config on the node it starts on
		}
		c, ok := caps[d.Storage]
		if !ok || c.Type != "zfspool" {
			return nil, conflictf("Volume %s is on storage %s, which cannot be replicated. Only local ZFS can", d.VolID, d.Storage)
		}
		storages[d.Storage] = true
	}
	if len(storages) == 0 {
		return nil, conflictf("This server has no disk to replicate")
	}
	return storages, nil
}

// refuseUnreplicable stops a disk from joining a protected VM on a storage that cannot replicate: the job would fail
// on it, and leaving it out of the job would lose it on a failover.
func (s *Service) refuseUnreplicable(ctx context.Context, vm *ClusterResource, storage string) error {
	if !HasTag(vm.Tags, tagProtected) {
		return nil
	}
	caps, err := s.C.DetectStorage(ctx)
	if err != nil {
		return err
	}
	if c, ok := caps[storage]; !ok || c.Type != "zfspool" {
		return conflictf("Server %s is protected, and storage %s cannot be replicated. Remove the protection first", vm.Name, storage)
	}
	return nil
}

// slotStorage is the storage part of a volid.
func slotStorage(volID string) string {
	st, _, _ := strings.Cut(volID, ":")
	return st
}

func hasPassthrough(cfg VMConfig) bool {
	for k := range cfg {
		if strings.HasPrefix(k, "hostpci") {
			return true
		}
	}
	return false
}

// ReserveMem is the memory each node must keep free to take its protected VMs if their primary dies. Without it a
// failover can find no room to start in. Feed it to PickNode.
func (s *Service) ReserveMem(ctx context.Context, vms []ClusterResource) (map[string]int64, error) {
	jobs, err := s.C.ListReplicationJobs(ctx)
	if err != nil {
		return nil, err
	}
	mem := make(map[int]int64, len(vms))
	for _, r := range vms {
		mem[r.VMID] = r.MaxMem
	}
	out := map[string]int64{}
	for _, j := range jobs {
		out[j.Target] += mem[j.Guest]
	}
	return out, nil
}

// PickProtectionTarget picks the replica node: PickNode over the nodes holding the VM's storages, minus its own.
func (s *Service) PickProtectionTarget(ctx context.Context, vm *ClusterResource, storages map[string]bool, diskBytes int64, headroomPct, overcommit float64) (string, error) {
	nodes, err := s.C.Nodes(ctx)
	if err != nil {
		return "", err
	}
	vms, err := s.C.ClusterResources(ctx, "vm")
	if err != nil {
		return "", err
	}
	rows, err := s.C.ClusterResources(ctx, "storage")
	if err != nil {
		return "", err
	}
	reserve, err := s.ReserveMem(ctx, vms)
	if err != nil {
		return "", err
	}
	// A node qualifies when every storage of the VM is active on it and fits the disks.
	hits := map[string]int{}
	state := map[string]StorageNodeState{}
	for _, r := range rows {
		if r.Type == "storage" && storages[r.Storage] && r.Node != vm.Node {
			st := rowState(r)
			if st.Active && StorageHasRoom(st, diskBytes, headroomPct) {
				hits[r.Node]++
				state[r.Node] = st
			}
		}
	}
	allowed := map[string]bool{}
	for n, c := range hits {
		if c == len(storages) {
			allowed[n] = true
		}
	}
	if len(allowed) == 0 {
		return "", conflictf("No other node has room for this server's disks")
	}
	target, err := PickNode(nodes, vms, SchedReq{
		VCPUs: int(vm.MaxCPU), RAMMb: vm.MaxMem >> 20, OvercommitCPU: overcommit, AllowedNodes: allowed, ReserveMem: reserve,
	})
	if err != nil {
		return "", conflictf("No other node has capacity to take this server over")
	}
	return target, nil
}

// EnableProtection starts protection on a VM: replication job, first sync, tag. HA follows in ReconcileProtection.
func (s *Service) EnableProtection(ctx context.Context, projectID, externalID string, headroomPct, overcommit float64) error {
	ready, reason, err := s.ProtectionReady(ctx)
	if err != nil {
		return err
	}
	if !ready {
		return conflictf("%s", reason)
	}
	vm, err := s.ResolveVM(ctx, projectID, externalID)
	if err != nil {
		return err
	}
	if vm == nil {
		return fmt.Errorf("pve: server %s not found", externalID)
	}
	cfg, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	if err != nil {
		return err
	}
	if HasTag(cfg.Str("tags"), tagProtected) {
		return conflictf("This server is already protected")
	}
	storages, err := s.ProtectableDisks(ctx, vm, cfg)
	if err != nil {
		return err
	}
	var diskBytes int64
	for _, d := range ParseDisks(cfg) {
		diskBytes += int64(d.SizeGiB) << 30
	}
	target, err := s.PickProtectionTarget(ctx, vm, storages, diskBytes, headroomPct, overcommit)
	if err != nil {
		return err
	}
	id, err := s.C.CreateReplicationJob(ctx, vm.VMID, target, DefaultReplicationSchedule)
	if err != nil {
		return err
	}
	up, err := s.C.SetVMConfig(ctx, vm.Node, vm.VMID, map[string]string{"tags": addTag(cfg.Str("tags"), tagProtected)})
	if err == nil {
		err = s.C.WaitTask(ctx, up)
	}
	if err != nil {
		_ = s.C.DeleteReplicationJob(ctx, id, false)
		return err
	}
	return s.C.ScheduleReplicationNow(ctx, vm.Node, id)
}

// DisableProtection undoes it in reverse: rule, HA, job, tag. A target that is down is no reason to stay protected:
// the job goes with force and its replica stays on that node until cleaned by hand.
func (s *Service) DisableProtection(ctx context.Context, projectID, externalID string) error {
	vm, err := s.ResolveVM(ctx, projectID, externalID)
	if err != nil {
		return err
	}
	if vm == nil {
		return nil
	}
	// An unprotected VM touches none of this, so a server delete keeps working on a token without HA rights.
	cfg, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	if err != nil {
		return err
	}
	if !HasTag(cfg.Str("tags"), tagProtected) {
		return nil
	}
	if err := s.C.DeleteHARule(ctx, vm.VMID); err != nil && !IsNotFound(err) {
		return err
	}
	if err := s.C.RemoveHAResource(ctx, vm.VMID); err != nil && !IsNotFound(err) {
		return err
	}
	jobs, err := s.C.ListReplicationJobs(ctx)
	if err != nil {
		return err
	}
	nodes, err := s.C.Nodes(ctx)
	if err != nil {
		return err
	}
	online := map[string]bool{}
	for _, n := range nodes {
		online[n.Node] = n.Status == "online"
	}
	for _, j := range jobs {
		if j.Guest == vm.VMID {
			if err := s.C.DeleteReplicationJob(ctx, j.ID, !online[j.Target]); err != nil && !IsNotFound(err) {
				return err
			}
		}
	}
	up, err := s.C.SetVMConfig(ctx, vm.Node, vm.VMID, map[string]string{"tags": removeTag(cfg.Str("tags"), tagProtected)})
	if err != nil {
		return err
	}
	return s.C.WaitTask(ctx, up)
}

// ReconcileProtection reads a protected VM's state and adds HA once the first sync has landed. nil = not protected.
func (s *Service) ReconcileProtection(ctx context.Context, r ClusterResource) (map[string]any, error) {
	jobs, err := s.C.ListReplicationJobs(ctx)
	if err != nil {
		return nil, err
	}
	var job *ReplicationJob
	for i := range jobs {
		if jobs[i].Guest == r.VMID {
			job = &jobs[i]
		}
	}
	if job == nil {
		return nil, nil
	}
	// After a failover the row's node is the one the VM moved to, and the job has already turned around.
	st, err := s.C.GetReplicationStatus(ctx, r.Node, job.ID)
	if err != nil {
		// The node holding the state is down, which is the moment this page is looked at: report, do not fail.
		st = &ReplicationStatus{Error: "Replication state is unavailable: node " + r.Node + " does not answer"}
	}
	ha, err := s.C.ListHAResources(ctx)
	if err != nil {
		return nil, err
	}
	sid := "vm:" + strconv.Itoa(r.VMID)
	managed := false
	for _, h := range ha {
		managed = managed || h.SID == sid
	}
	if st.LastSync > 0 && !managed {
		if err := s.C.AddHAResource(ctx, r.VMID); err != nil {
			return nil, err
		}
		managed = true
	}
	if managed {
		// After the resource: PVE refuses a rule over an unmanaged resource. Retried on the next read if it fails.
		rules, err := s.C.ListHARules(ctx)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(rules, func(rl HARule) bool { return rl.Rule == haRuleID(r.VMID) }) {
			if err := s.C.AddNodeAffinityRule(ctx, r.VMID, []string{r.Node, job.Target}); err != nil {
				return nil, err
			}
		}
	}
	state := ProtectionSyncing
	if managed {
		state = ProtectionActive
		late := time.Now().Unix()-st.LastSync > 2*replicationScheduleMinutes*60
		if st.FailCount > 0 || late {
			state = ProtectionDegraded
		}
	}
	return map[string]any{
		"state": state, "target": job.Target, "schedule": job.Schedule, "rpoTargetMinutes": replicationScheduleMinutes,
		"lastSync": st.LastSync, "failCount": st.FailCount, "error": st.Error, "ha": managed,
	}, nil
}

// StampProtection adds a protected VM's state to its row, including the replicated size billing rates. It moves the
// VM's protection forward too. Best effort: a failed read leaves the row without it until the next pass.
func (s *Service) StampProtection(ctx context.Context, r ClusterResource, data map[string]any) {
	if !HasTag(r.Tags, tagProtected) {
		return
	}
	pr, err := s.ReconcileProtection(ctx, r)
	if err != nil || pr == nil {
		return
	}
	if cfg, err := s.C.GetVMConfig(ctx, r.Node, r.VMID); err == nil {
		pr["diskGb"] = diskGbOf(cfg)
	}
	if srv, ok := data["server"].(map[string]any); ok {
		srv["protection"] = pr
	}
}

// waitHAPower: for an HA-managed VM the power task only asks the HA stack and returns at once (lab: 2 s, the VM
// changed state up to 20 s later), so a row refreshed right after would say the opposite of what the tenant just did.
// Best effort: after a minute the row just follows at the next sample.
func (s *Service) waitHAPower(ctx context.Context, vm *ClusterResource, verb string) {
	want := "stopped"
	switch verb {
	case "START":
		want = "running"
	case "STOP", "SHUTOFF", "SHUTDOWN":
	default:
		return // a reboot stays running
	}
	for range 30 {
		if st, err := s.C.GetVMStatus(ctx, vm.Node, vm.VMID); err == nil && st.Status == want {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// checkStaleCloudInit refuses a migrate that PVE would abort. PVE regenerates a VM's cloud-init volume wherever the VM
// starts and does not replicate it, so a node left in a failover keeps its copy, and a later migrate back fails on
// "volume already exists" after copying the disks (seen on the lab, PVE 9.2). Removing it takes Datastore.Allocate on the
// storage, more than the platform token should hold, so the operator is told what to run.
func (s *Service) checkStaleCloudInit(ctx context.Context, vmid int, cfg VMConfig, target string) error {
	for k := range cfg {
		raw := cfg.Str(k)
		if !strings.Contains(raw, "cloudinit") || !strings.Contains(raw, "media=cdrom") {
			continue
		}
		vol, _, _ := strings.Cut(raw, ",")
		rows, err := s.C.StorageContents(ctx, target, slotStorage(vol), "images", vmid)
		if err != nil {
			return nil // cannot tell: let PVE decide
		}
		for _, r := range rows {
			if r.VolID == vol {
				return conflictf("Node %s still holds %s from before a failover. Run `pvesm free %s` on it, then migrate", target, vol, vol)
			}
		}
	}
	return nil
}

// MigrateServer moves a VM to another node (admin only). PVE sends only the delta when a replica exists there, and
// turns the migration into an HA request for an HA-managed VM.
func (s *Service) MigrateServer(ctx context.Context, vmid int, target string) error {
	rows, err := s.C.ClusterResources(ctx, "vm")
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Type == "qemu" && r.VMID == vmid {
			if r.Node == target {
				return conflictf("The server is already on node %s", target)
			}
			cfg, err := s.C.GetVMConfig(ctx, r.Node, vmid)
			if err != nil {
				return err
			}
			if err := s.checkStaleCloudInit(ctx, vmid, cfg, target); err != nil {
				return err
			}
			up, err := s.C.MigrateVM(ctx, r.Node, vmid, target, r.Status == "running")
			if err != nil {
				return err
			}
			if err := s.C.WaitTask(ctx, up); err != nil {
				return err
			}
			if !HasTag(cfg.Str("tags"), tagProtected) {
				return nil
			}
			// HA runs the real migration behind a task that returns at once: report what actually happened.
			for range 60 {
				rows, err := s.C.ClusterResources(ctx, "vm")
				if err != nil {
					return err
				}
				for _, x := range rows {
					if x.Type == "qemu" && x.VMID == vmid && x.Node == target {
						return nil
					}
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(5 * time.Second):
				}
			}
			return conflictf("The server did not reach node %s in five minutes. Check the migration task on the cluster", target)
		}
	}
	return conflictf("Server %d not found", vmid)
}

// HasTag reports a tag in PVE's semicolon-separated list.
func HasTag(tags, tag string) bool {
	for _, t := range strings.Split(tags, ";") {
		if t == tag {
			return true
		}
	}
	return false
}

func addTag(tags, tag string) string {
	if HasTag(tags, tag) {
		return tags
	}
	if tags == "" {
		return tag
	}
	return tags + ";" + tag
}

func removeTag(tags, tag string) string {
	kept := []string{}
	for _, t := range strings.Split(tags, ";") {
		if t != "" && t != tag {
			kept = append(kept, t)
		}
	}
	return strings.Join(kept, ";")
}

package syncjob

// pve_meter.go — the metering side of the Proxmox sync: a per-minute state sampler (one
// /cluster/resources call per provider, however many projects and VMs it holds), the task-log
// poller that reacts to out-of-portal changes in seconds, and the staleness alert.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
)

const (
	// taskPageLimit bounds one node's task read. /nodes/{n}/tasks lists newest first, so a node
	// running more VM tasks than this between two polls loses the oldest (the sampler still sees
	// their effect).
	taskPageLimit = 500
	// taskFirstLookback is how far a node with no cursor reads back.
	taskFirstLookback = time.Hour
	taskRetention     = 90 * 24 * time.Hour
	// orphanIPGrace mirrors the sync providers' vanish grace: a VM may be missing from a fresh page.
	orphanIPGrace    = 2 * time.Minute
	taskEventsAudit  = "VM_TASK"
	resourceProvider = "EXTERNAL_SERVICE"
)

// PublicIPLister lists a project's public-IP allocations on a service.
type PublicIPLister func(ctx context.Context, serviceID, projectID string) ([]project.PublicIP, error)

// PublicIPReleaser frees the address a VM holds on one NIC.
type PublicIPReleaser func(ctx context.Context, serviceID, serverID, nic string) error

// WithPublicIPs wires the allocations behind the FLOATING_IP billing rows (nil = no such rows).
func (j *Job) WithPublicIPs(list PublicIPLister, release PublicIPReleaser) *Job {
	j.publicIPs, j.releaseIP = list, release
	return j
}

// WithAudit wires the system audit trail of the sampler and the poller (nil = none).
func (j *Job) WithAudit(a *audit.Service) *Job { j.audit = a; return j }

// proxmoxTargets lists the enabled proxmox providers with their enabled projects.
func (j *Job) proxmoxTargets(ctx context.Context) ([]externalservice.ExternalService, []project.Project, error) {
	services, err := j.services.ListByType(ctx, externalservice.TypeCloud)
	if err != nil {
		return nil, nil, err
	}
	out := services[:0]
	for i := range services {
		es := &services[i]
		if !es.IsDisabled() && es.IsProxmox() && es.ServiceEnabledInRegion("compute", es.PVERegion()) {
			out = append(out, *es)
		}
	}
	projects, err := j.projects.AllEnabled(ctx)
	return out, projects, err
}

// RunPVEState is the fleet-wide sampler: SERVER, IMAGE and FLOATING_IP rows of every project, off
// one /cluster/resources page per provider. Returns the rows changed.
func (j *Job) RunPVEState(ctx context.Context) (int, error) {
	services, projects, err := j.proxmoxTargets(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for i := range services {
		total += j.samplePVE(ctx, &services[i], projects)
	}
	return total, nil
}

func (j *Job) samplePVE(ctx context.Context, es *externalservice.ExternalService, projects []project.Project) int {
	// Read the clock first: Reconcile treats it as the snapshot time, so a portal write that lands
	// while the page is in flight wins over the older page.
	now := j.now()
	ps, err := j.pveFor(es)
	if err != nil {
		j.log.Error("syncjob: build pve client for state sample", "serviceId", es.ID, "err", err)
		return 0
	}
	page, err := ps.C.ClusterResources(ctx, "vm")
	if err == nil && len(page) == 0 {
		// Never trust an empty page: it would archive every VM of every project. The full sync still
		// archives them if the cluster really is empty.
		err = errors.New("cluster reported no VMs")
	}
	if err != nil {
		j.meterFailed(ctx, es, now, err)
		return 0
	}
	j.meterOK(ctx, es, now)
	region := es.PVERegion()
	count := 0
	for i := range projects {
		p := &projects[i]
		if !p.HasService(es.ID) {
			continue
		}
		cached, err := j.cloud.FindByServiceProjectAndType(ctx, es.ID, p.ID, cloud.TypeServer)
		if err != nil {
			j.log.Error("syncjob: read cached servers", "project", p.ID, "serviceId", es.ID, "err", err)
			continue
		}
		byExt := make(map[string]*cloud.CloudResource, len(cached))
		for k := range cached {
			byExt[cached[k].ExternalID] = &cached[k]
		}
		j.releaseOrphanIPs(ctx, es.ID, p.ID, page, byExt, now)
		for _, prov := range []providers.Provider{
			ps.SampledServerProvider(region, p.ID, page, byExt),
			ps.SampledImageProvider(region, p.ID, page),
			j.floatingIPProvider(region, p.ID, es.ID),
		} {
			st, err := providers.Reconcile(ctx, prov, j.cloud, es.ID, now)
			if err != nil {
				j.log.Error("syncjob: state sample", "project", p.ID, "serviceId", es.ID, "type", prov.Type(), "err", err)
			}
			count += st.Created + st.Updated + st.Deleted
		}
	}
	return count
}

// releaseOrphanIPs frees the addresses of VMs destroyed outside the portal, which would otherwise
// stay allocated and billed forever.
func (j *Job) releaseOrphanIPs(ctx context.Context, serviceID, projectID string, page []pve.ClusterResource, rows map[string]*cloud.CloudResource, now time.Time) {
	if j.publicIPs == nil || j.releaseIP == nil {
		return
	}
	allocs, err := j.publicIPs(ctx, serviceID, projectID)
	if err != nil {
		j.log.Error("syncjob: list public IPs", "project", projectID, "serviceId", serviceID, "err", err)
		return
	}
	listed := make(map[string]bool, len(page))
	for i := range page {
		listed[strconv.Itoa(page[i].VMID)] = true
	}
	for _, o := range orphanIPs(allocs, listed, rows, now) {
		j.log.Info("syncjob: releasing the address of a vanished VM", "project", projectID, "serviceId", serviceID, "server", o.ServerID, "nic", o.NIC)
		if err := j.releaseIP(ctx, serviceID, o.ServerID, o.NIC); err != nil {
			j.log.Error("syncjob: release address", "project", projectID, "server", o.ServerID, "err", err)
		}
	}
}

// orphanIPs picks the allocations that outlived their VM: the vmid is not listed anymore (past the
// pmxcfs grace), or, for a public NIC, its row is younger than the allocation, which means the vmid
// was reused, since a public NIC is normally attached to a VM that already has a row. A net0 address,
// and a public NIC plugged during the VM's build (BeforeRow), do not get that second check: they are
// allocated before the row exists.
func orphanIPs(allocs []project.PublicIP, listed map[string]bool, rows map[string]*cloud.CloudResource, now time.Time) []project.PublicIP {
	var out []project.PublicIP
	for _, a := range allocs {
		if a.CreatedAt == nil || a.NIC == project.ReservedNIC {
			continue
		}
		row := rows[a.ServerID]
		gone := !listed[a.ServerID] && now.Sub(*a.CreatedAt) > orphanIPGrace
		reused := a.IsPublic() && !a.BeforeRow && row != nil && row.CreatedAt != nil && row.CreatedAt.After(*a.CreatedAt)
		if gone || reused {
			out = append(out, a)
		}
	}
	return out
}

// meterOK records a good sample and closes an open alert.
func (j *Job) meterOK(ctx context.Context, es *externalservice.ExternalService, now time.Time) {
	state, err := j.cloud.MeterState(ctx, es.ID)
	if err != nil {
		j.log.Error("syncjob: read meter state", "serviceId", es.ID, "err", err)
		return
	}
	if state != nil && state.AlertedAt != nil {
		j.log.Info("proxmox metering recovered: billing resumes", "serviceId", es.ID, "service", es.Name, "staleSince", state.LastOKAt)
		j.providerAudit(es, "METERING_RECOVERED", audit.OutcomeSuccess, "", now)
	}
	if err := j.cloud.SetMeterFields(ctx, es.ID, pgdoc.M{"lastOkAt": now}, "alertedAt"); err != nil {
		j.log.Error("syncjob: save meter state", "serviceId", es.ID, "err", err)
	}
}

// meterFailed keeps the last-known state, and past cloud.MeterStaleAfter raises the alert once per
// outage (the charge job reads the same heartbeat and stops billing the provider's resources).
func (j *Job) meterFailed(ctx context.Context, es *externalservice.ExternalService, now time.Time, cause error) {
	j.log.Warn("syncjob: proxmox state sample failed, keeping last-known state", "serviceId", es.ID, "err", cause)
	state, err := j.cloud.MeterState(ctx, es.ID)
	if err != nil {
		j.log.Error("syncjob: read meter state", "serviceId", es.ID, "err", err)
		return
	}
	if state == nil || state.LastOKAt == nil {
		// A provider that has never answered still starts the clock.
		if err := j.cloud.SetMeterFields(ctx, es.ID, pgdoc.M{"lastOkAt": now}); err != nil {
			j.log.Error("syncjob: save meter state", "serviceId", es.ID, "err", err)
		}
		return
	}
	if !state.Stale(now) || state.AlertedAt != nil {
		return
	}
	j.log.Error("proxmox metering stale: billing of this provider's resources is stopped until it answers again",
		"alert", "pve_metering_stale", "serviceId", es.ID, "service", es.Name, "lastOkAt", state.LastOKAt, "err", cause)
	j.providerAudit(es, "METERING_STALE", audit.OutcomeFailure, cause.Error(), now)
	if err := j.cloud.SetMeterFields(ctx, es.ID, pgdoc.M{"alertedAt": now}); err != nil {
		j.log.Error("syncjob: save meter state", "serviceId", es.ID, "err", err)
	}
}

func (j *Job) providerAudit(es *externalservice.ExternalService, action, outcome, errMsg string, now time.Time) {
	if j.audit == nil {
		return
	}
	ev := audit.SystemEvent()
	ev.Action = action
	ev.ResourceType = resourceProvider
	ev.ResourceID = es.ID
	ev.ResourceDisplayName = es.Name
	ev.Outcome = outcome
	ev.ErrorMessage = errMsg
	ev.Timestamp = &now
	j.audit.LogAsync(ev)
}

// floatingIPProvider turns a project's public-IP allocations into FLOATING_IP rows.
func (j *Job) floatingIPProvider(region, projectID, serviceID string) *floatingIPProvider {
	return &floatingIPProvider{list: j.publicIPs, region: region, projectID: projectID, serviceID: serviceID}
}

// floatingIPProvider: the allocation table is the truth, the row is derived from it
// (platform_derived). The allocation time is part of the row's identity, so an address released
// and handed to someone else gets a new row and bill item even if no sample saw it free.
type floatingIPProvider struct {
	list                         PublicIPLister
	region, projectID, serviceID string
}

func (p *floatingIPProvider) Type() string          { return cloud.TypeFloatingIP }
func (p *floatingIPProvider) ProjectID() string     { return p.projectID }
func (p *floatingIPProvider) CompareKeys() []string { return []string{} }

func (p *floatingIPProvider) List(ctx context.Context) ([]cloud.CloudResource, error) {
	if p.list == nil {
		return nil, nil
	}
	allocs, err := p.list(ctx, p.serviceID, p.projectID)
	if err != nil {
		return nil, err
	}
	out := make([]cloud.CloudResource, 0, len(allocs))
	for _, a := range allocs {
		if !a.IsPublic() {
			continue
		}
		var since int64
		if a.CreatedAt != nil {
			since = a.CreatedAt.Unix()
		}
		out = append(out, cloud.CloudResource{
			ExternalID: fmt.Sprintf("%s/%s/%d", a.NetworkID, a.IP, since),
			ProjectID:  p.projectID,
			Region:     p.region,
			Type:       cloud.TypeFloatingIP,
			CreatedAt:  a.CreatedAt, // bill from the allocation, not from the first sample after it
			Data: map[string]any{"floatingIp": map[string]any{
				"floating_ip_address": a.IP,
				"floating_network_id": a.NetworkID,
				"status":              "ACTIVE",
				"server_id":           a.ServerID,
				"nic":                 a.NIC,
				"platform_derived":    true,
			}},
		})
	}
	return out, nil
}

// RunPVETasks polls every online node's task log past its cursor: a finished VM task is recorded
// once (LIST_EVENTS), audited when someone other than the platform ran it, and its project re-synced
// at once, so an out-of-portal change reaches the cache in seconds. Returns the tasks recorded.
func (j *Job) RunPVETasks(ctx context.Context) (int, error) {
	services, projects, err := j.proxmoxTargets(ctx)
	if err != nil {
		return 0, err
	}
	now := j.now()
	if err := j.cloud.PruneTaskEvents(ctx, now.Add(-taskRetention)); err != nil {
		j.log.Warn("syncjob: prune task events", "err", err)
	}
	total := 0
	for i := range services {
		total += j.pollPVETasks(ctx, &services[i], projects, now)
	}
	return total, nil
}

func (j *Job) pollPVETasks(ctx context.Context, es *externalservice.ExternalService, projects []project.Project, now time.Time) int {
	ps, err := j.pveFor(es)
	if err != nil {
		j.log.Error("syncjob: build pve client for task poll", "serviceId", es.ID, "err", err)
		return 0
	}
	nodes, err := ps.C.Nodes(ctx)
	if err != nil {
		j.log.Warn("syncjob: list nodes for task poll", "serviceId", es.ID, "err", err)
		return 0
	}
	state, err := j.cloud.MeterState(ctx, es.ID)
	if err != nil {
		j.log.Error("syncjob: read meter state", "serviceId", es.ID, "err", err)
		return 0
	}
	cursors := map[string]int64{}
	if state != nil {
		for k, v := range state.TaskCursors {
			cursors[k] = v
		}
	}
	owner := j.vmOwner(ctx, es, ps, projects)
	// A task's user is the token's user, without the token name.
	platformUser, _, _ := strings.Cut(es.PVEConfig().TokenID, "!")
	dirty := map[string]bool{}
	recorded := 0
	for _, node := range nodes {
		if node.Status != "online" {
			continue
		}
		since, known := cursors[node.Node]
		if !known {
			since = now.Add(-taskFirstLookback).Unix()
		}
		tasks, err := ps.C.NodeTasks(ctx, node.Node, since, taskPageLimit)
		if err != nil {
			j.log.Warn("syncjob: read node tasks", "serviceId", es.ID, "node", node.Node, "err", err)
			continue
		}
		next := nextTaskCursor(tasks, since)
		for i := range tasks {
			t := &tasks[i]
			if t.EndTime == 0 || !isVMTask(t) {
				continue
			}
			projectID := owner(t.ID)
			if projectID == "" {
				continue
			}
			fresh, err := j.cloud.InsertTaskEvent(ctx, taskEvent(es.ID, projectID, t))
			if err != nil {
				j.log.Error("syncjob: record task event", "upid", t.UPID, "err", err)
				next = since // retried on the next poll; the UPID key keeps it from doubling
				continue
			}
			if !fresh {
				continue
			}
			recorded++
			dirty[projectID] = true
			if t.User != platformUser {
				j.taskAudit(t, projectID, now)
			}
		}
		cursors[node.Node] = next
	}
	if err := j.cloud.SetMeterFields(ctx, es.ID, pgdoc.M{"taskCursors": cursors}); err != nil {
		j.log.Error("syncjob: save task cursors", "serviceId", es.ID, "err", err)
	}
	for projectID := range dirty {
		if err := j.SyncOne(ctx, projectID, es.ID); err != nil {
			j.log.Warn("syncjob: sync after task", "project", projectID, "serviceId", es.ID, "err", err)
		}
	}
	return recorded
}

// nextTaskCursor is the newest start seen, held back to the oldest task still running so its end
// is not missed. The start second is re-read on the next poll on purpose: a task started later in
// that second must not slip past (the UPID key absorbs the repeats).
func nextTaskCursor(tasks []pve.NodeTask, since int64) int64 {
	next := since
	var oldestRunning int64
	for i := range tasks {
		t := &tasks[i]
		next = max(next, t.StartTime)
		if t.EndTime == 0 && isVMTask(t) && (oldestRunning == 0 || t.StartTime < oldestRunning) {
			oldestRunning = t.StartTime
		}
	}
	if oldestRunning != 0 {
		next = min(next, oldestRunning)
	}
	return next
}

// vmOwner resolves a vmid to the enabled project holding it: the live pool when the VM is listed
// (a cached row may be a stale one from before the vmid was reused, maybe by another tenant), the
// cached row for a VM already gone. The page is read once, on the first lookup.
func (j *Job) vmOwner(ctx context.Context, es *externalservice.ExternalService, ps *pve.Service, projects []project.Project) func(vmid string) string {
	attached := map[string]bool{}
	for i := range projects {
		if projects[i].HasService(es.ID) {
			attached[projects[i].ID] = true
		}
	}
	var pools map[string]string
	return func(vmid string) string {
		if pools == nil {
			pools = map[string]string{}
			page, err := ps.C.ClusterResources(ctx, "vm")
			if err != nil {
				j.log.Warn("syncjob: list VMs for task poll", "serviceId", es.ID, "err", err)
			}
			for _, r := range page {
				id, _ := strings.CutPrefix(r.Pool, ps.PoolPrefix)
				pools[strconv.Itoa(r.VMID)] = id
			}
		}
		if id, listed := pools[vmid]; listed {
			if attached[id] {
				return id
			}
			return ""
		}
		if row, err := j.cloud.FindByServiceIDAndExternalID(ctx, es.ID, vmid); err == nil && row != nil && attached[row.ProjectID] {
			return row.ProjectID
		}
		return ""
	}
}

// isVMTask: a task that can change what a VM is or does. A console session also carries a vmid,
// so the check is on the type.
func isVMTask(t *pve.NodeTask) bool {
	if _, err := strconv.Atoi(t.ID); err != nil {
		return false
	}
	return strings.HasPrefix(t.Type, "qm") && t.Type != "qmtermproxy" ||
		strings.HasPrefix(t.Type, "ha") ||
		slices.Contains([]string{"resize", "vzdump"}, t.Type)
}

func taskEvent(serviceID, projectID string, t *pve.NodeTask) *cloud.TaskEvent {
	started := time.Unix(t.StartTime, 0).UTC()
	ended := time.Unix(t.EndTime, 0).UTC()
	return &cloud.TaskEvent{
		ID: t.UPID, ServiceID: serviceID, ProjectID: projectID, VMID: t.ID, Node: t.Node,
		Type: t.Type, User: t.User, Status: t.Status, StartedAt: &started, EndedAt: &ended,
	}
}

func (j *Job) taskAudit(t *pve.NodeTask, projectID string, now time.Time) {
	if j.audit == nil {
		return
	}
	ev := audit.SystemEvent()
	ev.EventContext = audit.ContextProject
	ev.ProjectID = projectID
	ev.Action = taskEventsAudit
	ev.ResourceType = cloud.TypeServer
	ev.ResourceID = t.ID
	// The project's members read this event: no PVE account or UPID (it names one), only what ran where. Only tasks
	// someone other than the platform ran are audited, so the actor is an operator.
	ev.ResourceMetadata = map[string]any{"type": t.Type, "by": "operator", "node": t.Node, "status": t.Status}
	ev.Outcome = audit.OutcomeSuccess
	if t.Status != "OK" {
		ev.Outcome = audit.OutcomeFailure
		ev.ErrorMessage = t.Status
	}
	ev.Timestamp = &now
	j.audit.LogAsync(ev)
}

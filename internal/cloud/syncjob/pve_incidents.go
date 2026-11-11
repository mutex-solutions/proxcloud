package syncjob

import (
	"context"
	"errors"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveincident"
)

// PVEIncidentRecorder writes only new incident/audit DB records; nil disables it.
// It cannot mutate PVE, billing, the resource cache, allocations, or send notifications.
type PVEIncidentRecorder interface {
	BeginCapture(context.Context, string, string, string) (pveincident.Capture, error)
	Observe(context.Context, pveincident.SnapshotResult) (pveincident.ObserveResult, error)
}

func (j *Job) WithPVEIncidents(r PVEIncidentRecorder) *Job { j.incidents = r; return j }
func incidentGeneration(es *externalservice.ExternalService) string {
	// Stamped BEFORE decrypt from the stored encrypted credential revision.
	// No fallback from decrypted secret/public identity: absence fails admission.
	return es.ObservationGeneration
}
func (j *Job) beginIncidents(ctx context.Context, es *externalservice.ExternalService) *pveincident.SnapshotResult {
	if j.incidents == nil {
		return nil
	}
	c, err := j.incidents.BeginCapture(ctx, es.ID, incidentGeneration(es), es.PVERegion())
	if err != nil {
		if !errors.Is(err, pveincident.ErrThrottled) {
			j.log.Warn("incident capture unavailable", "serviceId", es.ID)
		}
		return nil
	}
	return &pveincident.SnapshotResult{Capture: c, Trust: pveincident.SourceTrust{Provider: pveincident.Untrusted, Nodes: pveincident.Untrusted, VMs: pveincident.Verified, Protection: pveincident.Verified}}
}
func (j *Job) finishIncidents(ctx context.Context, x *pveincident.SnapshotResult) {
	if x == nil {
		return
	}
	if _, err := j.incidents.Observe(ctx, *x); err != nil {
		j.log.Warn("incident observation not committed", "serviceId", x.Capture.ProviderID)
	}
}
func incidentNodes(x *pveincident.SnapshotResult, ps *pve.Service, ctx context.Context) {
	if x == nil {
		return
	}
	o := ps.ObserveNodes(ctx)
	x.Trust.Provider, x.Trust.Nodes = pveincident.Trust(o.ProviderTrust), pveincident.Trust(o.Trust)
	for _, n := range o.Nodes {
		x.Nodes = append(x.Nodes, pveincident.NodeFact{NodeID: n.NodeID, Status: pveincident.NodeStatus(n.Status)})
	}
}
func incidentUnknown(x *pveincident.SnapshotResult) {
	if x != nil {
		x.Trust.VMs, x.Trust.Protection = pveincident.Untrusted, pveincident.Untrusted
		x.LinksComplete = false
	}
}
func incidentServers(x *pveincident.SnapshotResult, ps *pve.ServerSyncProvider, ctx context.Context, refs []cloud.CloudResource) {
	if x == nil {
		return
	}
	o := ps.ObserveVerified(ctx, refs)
	if o.VMTrust != pve.ObservationVerified || o.ProtectionTrust != pve.ObservationVerified {
		incidentUnknown(x)
		return
	}
	// Only enabled project scopes are sampled, never assert provider-wide complete
	// ownership or remove last-verified links for an unobserved/disabled project.
	for _, s := range o.Servers {
		x.Links = append(x.Links, pveincident.Link{ServerID: s.ServerID, ProjectID: s.ProjectID, InstanceID: s.InstanceID, NodeID: s.NodeID})
		p := pveincident.ProtectionFact{ServerID: s.ServerID, InstanceID: s.InstanceID, Mode: s.Mode, Enabled: s.Enabled, HA: s.HA, FailCount: s.FailCount}
		if s.LastSync > 0 {
			p.LastSync = time.Unix(s.LastSync, 0).UTC()
		}
		x.Protection = append(x.Protection, p)
	}
}

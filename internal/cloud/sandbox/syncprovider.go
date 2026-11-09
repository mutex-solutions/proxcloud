package sandbox

import (
	"context"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// SyncProvider lists a project's sandboxes → SANDBOX cloud resources. It satisfies
// providers.Provider + ProjectScoped implicitly (dbaas precedent: no import, so the dependency
// arrow points sandbox→cloud only). A sandbox the controller deleted at its shutdownTime simply
// vanishes from the list, and Reconcile archives its row.
type SyncProvider struct {
	svc       *Service
	region    string
	projectID string
}

// SyncProvider builds the read-sync provider for one project.
func (s *Service) SyncProvider(region, projectID string) *SyncProvider {
	return &SyncProvider{svc: s, region: region, projectID: projectID}
}

func (p *SyncProvider) Type() string      { return cloud.TypeSandbox }
func (p *SyncProvider) ProjectID() string { return p.projectID }

func (p *SyncProvider) List(ctx context.Context) ([]cloud.CloudResource, error) {
	items, err := p.svc.ListSandboxes(ctx, p.projectID)
	if err != nil {
		return nil, err
	}
	builds, err := p.svc.ListBuilds(ctx, p.projectID)
	if err != nil {
		return nil, err
	}
	for id, data := range builds {
		items[id] = data
	}
	out := make([]cloud.CloudResource, 0, len(items))
	for id, data := range items {
		out = append(out, cloud.CloudResource{
			Type:       cloud.TypeSandbox,
			ExternalID: id,
			Region:     p.region,
			ProjectID:  p.projectID,
			Data:       data,
		})
	}
	return out, nil
}

// KeepUnread is where a sandbox gone from the cluster (its timeout, or a delete) gets its last row: Reconcile archives the
// row as this leaves it, and the charge rates archived rows to their end. It never keeps the row.
func (p *SyncProvider) KeepUnread(cr *cloud.CloudResource) (keep, changed bool) {
	MarkVanished(cr, p.svc.now())
	return false, false
}

// MarkVanished ends the row of a sandbox that is no longer on the cluster: DELETED at its shutdownTime if that has
// passed, else now. A row never seen running may still have run between two reads (a sandbox can live for seconds),
// so it is billed from the last time it was seen not running (seenPendingAt, else its create): at most one read
// interval more than it ran, never less. A row already DELETED keeps its end.
func MarkVanished(cr *cloud.CloudResource, now time.Time) {
	d := cr.Data
	if str(d["status"]) == StatusDeleted {
		return
	}
	end := now
	if sd, err := time.Parse(time.RFC3339, str(d["shutdownTime"])); err == nil && sd.Before(now) {
		end = sd
	}
	out := MarkDeleted(d, end)
	// A build pod records its own start and end; one that failed before it started ran nothing.
	if str(out["runningSince"]) == "" && str(d["kind"]) != "build" {
		since := str(d["seenPendingAt"])
		if since == "" {
			since = str(d["startedAt"])
		}
		if since == "" && cr.CreatedAt != nil {
			since = cr.CreatedAt.UTC().Format(time.RFC3339)
		}
		if since != "" {
			out["runningSince"], out["runningSinceAssumed"] = since, true
		}
	}
	cr.Data = out
}

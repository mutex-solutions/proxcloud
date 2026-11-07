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

// lingerFor keeps an ended sandbox's row past at least two minutely charge ticks.
const lingerFor = 3 * time.Minute

// Linger keeps a vanished sandbox's row for lingerFor after it ended, so the minutely charge rates
// it up to its end (the billing job only sees cached rows). A sandbox the controller removed at its
// shutdownTime is marked ended then; one deleted through proxcloud is already marked. Sandboxes that
// never ran have nothing to bill and are archived at once.
func (p *SyncProvider) Linger(cr *cloud.CloudResource, now time.Time) bool {
	d := cr.Data
	if str(d["runningSince"]) == "" {
		return false
	}
	ended, err := time.Parse(time.RFC3339, str(d["endedAt"]))
	if err != nil || str(d["status"]) != StatusDeleted {
		ended = now
		if sd, err := time.Parse(time.RFC3339, str(d["shutdownTime"])); err == nil && sd.Before(now) {
			ended = sd
		}
		cr.Data = MarkDeleted(d, ended)
	}
	return now.Sub(ended) < lingerFor
}

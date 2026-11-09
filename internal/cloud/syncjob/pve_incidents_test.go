package syncjob

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveincident"
)

type incidentRecorder struct {
	begin, observe int
	result         pveincident.SnapshotResult
}

func (r *incidentRecorder) BeginCapture(_ context.Context, p, g, region string) (pveincident.Capture, error) {
	r.begin++
	return pveincident.Capture{ProviderID: p, Generation: g, Region: region, Epoch: int64(r.begin), StartedAt: time.Now()}, nil
}
func (r *incidentRecorder) Observe(_ context.Context, x pveincident.SnapshotResult) (pveincident.ObserveResult, error) {
	r.observe++
	r.result = x
	return pveincident.ObserveResult{Accepted: true}, nil
}
func TestIncidentSamplerRecordsBuildFailureOncePerProvider(t *testing.T) {
	r := &incidentRecorder{}
	j := &Job{incidents: r, now: time.Now, log: slog.New(slog.NewTextHandler(io.Discard, nil)), pveFor: func(*externalservice.ExternalService) (*pve.Service, error) {
		return nil, errors.New("private raw error")
	}}
	es := &externalservice.ExternalService{ID: "provider", Config: map[string]any{"provider": "proxmox", "region": "region", "tokenId": "operator@pve!portal"}, Secret: map[string]any{"tokenSecret": "DO-NOT-PERSIST"}}
	j.samplePVE(context.Background(), es, []project.Project{{ID: "project1"}, {ID: "project2"}})
	if r.begin != 1 || r.observe != 1 || r.result.Trust.VMs != pveincident.Untrusted || r.result.Trust.Protection != pveincident.Untrusted || len(r.result.Nodes) != 0 || len(r.result.Links) != 0 {
		t.Fatal(r)
	}
	first := incidentGeneration(es)
	es.Secret = map[string]any{"tokenSecret": "rotated"}
	if incidentGeneration(es) != first {
		t.Fatal("secret entered generation hash")
	}
	es.Config["tokenId"] = "changed@pve!portal"
	if incidentGeneration(es) == first {
		t.Fatal("public token identity not fenced")
	}
}
func TestIncidentAggregationNeverPromotesUnknownScopes(t *testing.T) {
	x := pveincident.SnapshotResult{Trust: pveincident.SourceTrust{Provider: pveincident.Verified, Nodes: pveincident.Verified, VMs: pveincident.Verified, Protection: pveincident.Verified}, LinksComplete: true, Nodes: []pveincident.NodeFact{{NodeID: "node", Status: pveincident.Offline}}}
	incidentUnknown(&x)
	if x.Trust.Nodes != pveincident.Verified || x.Nodes[0].Status != pveincident.Offline || x.Trust.VMs != pveincident.Untrusted || x.LinksComplete {
		t.Fatal(x)
	}
	incidentUnknown(&x)
	if len(x.Protection) != 0 || len(x.Links) != 0 {
		t.Fatal("unknown created affected IDs", x)
	}
}

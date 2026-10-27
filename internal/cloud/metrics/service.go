package metrics

import (
	"context"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// MeasureFetcher is the gnocchi read interface (the concrete *Gnocchi implements it), so the
// aggregation is unit/integration-testable without a live cloud.
type MeasureFetcher interface {
	SearchInstanceInterfaces(ctx context.Context, instanceID string) ([]Resource, error)
	MeasuresMBForCurrentMonth(ctx context.Context, metricID string, granularity int, start time.Time) (decimal.Decimal, error)
}

// Service: for a SERVER, sum
// each network interface's incoming/outgoing traffic (MB, from gnocchi) into public vs
// private buckets and upsert the month's GnocchiMetrics doc.
type Service struct {
	repo *Repo
}

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

// FetchAndSaveGnocchiMetrics aggregates + persists a server's monthly traffic. `ports` and
// `publicNets` are the project's PORT and public NETWORK cloud resources (used to classify
// each interface). No-op for non-SERVER resources.
func (s *Service) FetchAndSaveGnocchiMetrics(ctx context.Context, f MeasureFetcher, server *cloud.CloudResource, ports, publicNets []cloud.CloudResource, granularity int, startAt, endAt time.Time) error {
	if server.Type != cloud.TypeServer {
		return nil
	}
	ifaces, err := f.SearchInstanceInterfaces(ctx, server.ExternalID)
	if err != nil {
		return err
	}
	m, err := s.repo.GetMetric(ctx, server, startAt, endAt)
	if err != nil {
		return err
	}
	inPub, outPub, inPriv, outPriv := decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero
	for _, iface := range ifaces {
		inc, err := f.MeasuresMBForCurrentMonth(ctx, iface.Metrics["network.incoming.bytes"], granularity, startAt)
		if err != nil {
			return err
		}
		out, err := f.MeasuresMBForCurrentMonth(ctx, iface.Metrics["network.outgoing.bytes"], granularity, startAt)
		if err != nil {
			return err
		}
		if isPublicTraffic(iface, ports, publicNets) {
			inPub, outPub = inPub.Add(inc), outPub.Add(out)
		} else {
			inPriv, outPriv = inPriv.Add(inc), outPriv.Add(out)
		}
	}
	m.Details.IncomingPrivateTrafficMb = inPriv
	m.Details.OutgoingPrivateTrafficMb = outPriv
	m.Details.IncomingPublicTrafficMb = inPub
	m.Details.OutgoingPublicTrafficMb = outPub
	m.Details.TotalPublicTrafficMb = inPub.Add(outPub)
	m.Details.TotalPrivateTrafficMb = inPriv.Add(outPriv)
	m.Details.TotalTrafficMb = inPub.Add(outPub).Add(inPriv).Add(outPriv)
	_, err = s.repo.Save(ctx, m)
	return err
}

// isPublicTraffic strips "tap" from the
// interface name → port-id prefix → find the matching PORT resource → its port.networkId →
// true iff that network is in the public-networks list.
func isPublicTraffic(iface Resource, ports, publicNets []cloud.CloudResource) bool {
	portIDPrefix := strings.ReplaceAll(iface.Name, "tap", "")
	for i := range ports {
		if !strings.HasPrefix(ports[i].ExternalID, portIDPrefix) {
			continue
		}
		netID := portNetworkID(ports[i].Data)
		if netID == "" {
			return false
		}
		for j := range publicNets {
			if publicNets[j].ExternalID == netID {
				return true
			}
		}
		return false
	}
	return false
}

// portNetworkID reads data.port.networkId from a PORT cloud resource's free-form data.
func portNetworkID(data map[string]any) string {
	port, ok := data["port"].(map[string]any)
	if !ok {
		return ""
	}
	if id, ok := port["networkId"].(string); ok {
		return id
	}
	return ""
}

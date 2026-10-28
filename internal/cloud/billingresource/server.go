package billingresource

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/metrics"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// ServerProvider yields up
// to two BillingResources for a SERVER — "instance" (compute attributes from the cloud data) and
// "instance_traffic" (the month's network traffic from UsageMetrics). notEligibleForBilling
// gating + the admin-configured per-attribute types are deferred.
type ServerProvider struct {
	usage *metrics.Repo
	now   func() time.Time
}

func NewServerProvider(usage *metrics.Repo) *ServerProvider {
	return &ServerProvider{usage: usage, now: func() time.Time { return time.Now().UTC() }}
}

func (p *ServerProvider) Type() string { return cloud.TypeServer }

// serverNotEligibleStatuses: a server in one of these
// statuses (the API's status vocabulary, see pve.MapVMStatus) is excluded from billing (DELETED/ERROR/UNKNOWN/BUILD).
var serverNotEligibleStatuses = map[string]bool{"DELETED": true, "ERROR": true, "UNKNOWN": true, "BUILD": true}

func (p *ServerProvider) GetBillingInformation(ctx context.Context, _ billingapi.BillingContext, cr *cloud.CloudResource) ([]*billingapi.BillingResource, error) {
	out := []*billingapi.BillingResource{}
	notEligible := false
	if server, ok := mapAt(cr.Data, "server"); ok {
		notEligible = serverNotEligibleStatuses[str(server["status"])]
	}
	if inst := p.instanceBR(cr, notEligible); inst != nil {
		out = append(out, inst)
	}
	// Only create the traffic resource when the server is eligible.
	if !notEligible {
		tr, err := p.trafficBR(ctx, cr)
		if err != nil {
			return nil, err
		}
		if tr != nil {
			out = append(out, tr)
		}
	}
	return out, nil
}

// instanceBR builds the "instance" resource from the server's flavor/data.
// nil when the cloud data has no server.
func (p *ServerProvider) instanceBR(cr *cloud.CloudResource, notEligible bool) *billingapi.BillingResource {
	server, ok := mapAt(cr.Data, "server")
	if !ok {
		return nil
	}
	flavor, _ := mapAt(server, "flavor")
	rootDisk := flavor["disk"]
	if cloud.ServerIsVolumeBacked(cr.Data) {
		// A volume-backed server does not consume the flavor's local root disk.
		// Its root disk is synced and rated as a VOLUME resource instead.
		rootDisk = 0
	}
	values := map[string]any{
		"instance_type":     str(cr.Data["flavorName"]),
		"ram_mb":            flavor["ram"],
		"vcpus":             flavor["vcpus"],
		"root_disk_gb":      rootDisk,
		"display_name":      server["name"],
		"host":              server["host"],
		"status":            server["status"],
		"is_bareMetal":      false,
		"availability_zone": server["availabilityZone"],
	}
	// GPU pricing dimension: model alias + device count from the flavor's extra specs
	// (rules filter on gpu_model and price gpu_count). Zero/"" for non-GPU flavors.
	gpuModel, gpuCount := cloud.GPUFromFlavor(flavor["extra_specs"])
	values["gpu_model"] = gpuModel
	values["gpu_count"] = gpuCount
	if img, ok := mapAt(server, "image"); ok {
		values["image"] = img["id"]
	}
	if ramMB, ok := toDec(flavor["ram"]); ok {
		values["ram_gb"] = ramMB.DivRound(decimal.NewFromInt(1024), 2)
	}
	return &billingapi.BillingResource{
		ResourceID:            cr.ID,
		ProjectID:             cr.ProjectID,
		ResourceType:          "instance",
		Values:                values,
		BillingResourceType:   instanceType(),
		NotEligibleForBilling: notEligible,
	}
}

// trafficBR builds the "instance_traffic" resource from the month's UsageMetrics.
// nil when no metrics exist for the current cycle.
func (p *ServerProvider) trafficBR(ctx context.Context, cr *cloud.CloudResource) (*billingapi.BillingResource, error) {
	cycleStart := firstDayOfCurrentMonth(p.now())
	m, err := p.usage.FindForCurrentMonth(ctx, cr.ID, cycleStart)
	if err != nil {
		return nil, err
	}
	if m == nil || m.Details == nil {
		return nil, nil
	}
	d := m.Details
	dn := str(cr.ExternalID)
	if server, ok := mapAt(cr.Data, "server"); ok {
		if n := str(server["name"]); n != "" {
			dn = n
		}
	}
	values := map[string]any{
		"incoming_private_traffic_mb": d.IncomingPrivateTrafficMb,
		"outgoing_private_traffic_mb": d.OutgoingPrivateTrafficMb,
		"incoming_public_traffic_mb":  d.IncomingPublicTrafficMb,
		"outgoing_public_traffic_mb":  d.OutgoingPublicTrafficMb,
		"total_public_traffic_mb":     d.TotalPublicTrafficMb,
		"total_private_traffic_mb":    d.TotalPrivateTrafficMb,
		"total_traffic_mb":            d.TotalTrafficMb,
		"display_name":                dn,
	}
	return &billingapi.BillingResource{
		ResourceID:          fmt.Sprintf("instance_traffic-%s", cr.ID),
		ProjectID:           cr.ProjectID,
		ResourceType:        "instance_traffic",
		Values:              values,
		BillingResourceType: instanceTrafficType(),
	}, nil
}

func instanceType() *billingapi.BillingResourceType {
	num := func(n string) billingapi.ResourceAttribute {
		return billingapi.ResourceAttribute{Name: n, Type: "number"}
	}
	s := func(n string) billingapi.ResourceAttribute {
		return billingapi.ResourceAttribute{Name: n, Type: "string"}
	}
	return &billingapi.BillingResourceType{ResourceType: "instance", Attributes: []billingapi.ResourceAttribute{
		s("instance_type"), num("ram_mb"), num("ram_gb"), num("vcpus"), num("root_disk_gb"),
		num("gpu_count"), s("gpu_model"),
		s("display_name"), s("host"), s("status"), s("image"),
		{Name: "is_bareMetal", Type: "boolean"}, s("availability_zone"),
	}}
}

func instanceTrafficType() *billingapi.BillingResourceType {
	yes := true
	usage := func(n string) billingapi.ResourceAttribute {
		return billingapi.ResourceAttribute{Name: n, Type: "number", IsUsage: &yes}
	}
	return &billingapi.BillingResourceType{ResourceType: "instance_traffic", Attributes: []billingapi.ResourceAttribute{
		usage("incoming_private_traffic_mb"), usage("outgoing_private_traffic_mb"),
		usage("incoming_public_traffic_mb"), usage("outgoing_public_traffic_mb"),
		usage("total_public_traffic_mb"), usage("total_private_traffic_mb"), usage("total_traffic_mb"),
		{Name: "display_name", Type: "string"},
	}}
}

func firstDayOfCurrentMonth(now time.Time) time.Time {
	y, m, _ := now.UTC().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

func mapAt(m map[string]any, key string) (map[string]any, bool) {
	if m == nil {
		return nil, false
	}
	sub, ok := m[key].(map[string]any)
	return sub, ok
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toDec(v any) (decimal.Decimal, bool) {
	switch n := v.(type) {
	case int:
		return decimal.NewFromInt(int64(n)), true
	case int32:
		return decimal.NewFromInt(int64(n)), true
	case int64:
		return decimal.NewFromInt(n), true
	case float64:
		return decimal.NewFromFloat(n), true
	case decimal.Decimal:
		return n, true
	default:
		return decimal.Zero, false
	}
}

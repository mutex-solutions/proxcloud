package billingresource

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// DatabaseClusterProvider maps a DATABASE_CLUSTER → a "database_cluster" BillingResource. UNLIKE
// kamaji, this type carries the FULL price: the database's compute and storage run on OPS-owned
// nodes (nothing in the customer tenant bills them), so the totals — pre-multiplied by replicas,
// because the pricing engine rates attributes independently — are the whole product.
type DatabaseClusterProvider struct{}

func NewDatabaseClusterProvider() *DatabaseClusterProvider { return &DatabaseClusterProvider{} }

func (p *DatabaseClusterProvider) Type() string { return cloud.TypeDatabaseCluster }

func (p *DatabaseClusterProvider) GetBillingInformation(_ context.Context, _ billingapi.BillingContext, cr *cloud.CloudResource) ([]*billingapi.BillingResource, error) {
	values := map[string]any{}
	notEligible := true
	if d, ok := mapAt(cr.Data, "database"); ok {
		values["display_name"] = d["name"]
		values["engine"] = d["engine"]
		values["version"] = d["version"]
		values["status"] = d["status"]
		values["replicas"] = d["replicas"]
		replicas := num(d["replicas"])
		if replicas < 1 {
			replicas = 1
		}
		values["vcpus_total"] = num(d["cpu"]) * replicas
		values["ram_gb_total"] = num(d["memory_gib"]) * replicas
		values["storage_gb_total"] = num(d["storage_gib"]) * replicas
		// Autoscale surcharge flag (1/0): a flat per-database fee while enabled. The scaled-up
		// capacity itself bills through the totals above — the tick raises the declared size.
		values["autoscale_enabled"] = num(d["autoscale_enabled"])
		// The read-only endpoint bills only where it actually costs something: postgresql
		// needs a SECOND LoadBalancer for it (its readers are different pods), while mysql and
		// mariadb answer reads on another port of the proxy they already run. The sync decides
		// which case a database is in; charging for a free port would not be defensible.
		readLB := float64(0)
		if b, _ := d["read_endpoint_lb"].(bool); b {
			readLB = 1
		}
		values["read_endpoint_lb"] = readLB
		// Gate on the tenant-side endpoint (the LB ingress IP): while it is absent the charge is
		// DEFERRED, not waived — the pricing engine freezes the bill-item watermark at
		// createdAt and, once the endpoint appears, back-bills the elapsed hours of the current
		// cycle at the then-current attribute values (SaveChargingToBill/updateBillItems). A
		// database degraded AFTER coming up keeps billing (it still holds capacity) — the
		// kubernetes_cluster stance.
		endpoint := str(d["endpoint"])
		values["endpoint"] = endpoint
		notEligible = endpoint == ""
	}
	return []*billingapi.BillingResource{{
		ResourceID:            cr.ID,
		ProjectID:             cr.ProjectID,
		ResourceType:          "database_cluster",
		Values:                values,
		BillingResourceType:   databaseClusterType(),
		NotEligibleForBilling: notEligible,
	}}, nil
}

func databaseClusterType() *billingapi.BillingResourceType {
	s := func(n string) billingapi.ResourceAttribute {
		return billingapi.ResourceAttribute{Name: n, Type: "string"}
	}
	n := func(nm string) billingapi.ResourceAttribute {
		return billingapi.ResourceAttribute{Name: nm, Type: "number"}
	}
	return &billingapi.BillingResourceType{ResourceType: "database_cluster", Attributes: []billingapi.ResourceAttribute{
		s("display_name"), s("engine"), s("version"), s("status"), s("endpoint"),
		n("replicas"), n("vcpus_total"), n("ram_gb_total"), n("storage_gb_total"), n("autoscale_enabled"),
		n("read_endpoint_lb"),
	}}
}

// num coerces a cached JSON number (float64 post-round-trip, int fresh from the create path).
func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}

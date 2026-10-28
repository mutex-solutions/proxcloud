package billingresource

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// KubernetesClusterProvider maps a KUBERNETES_CLUSTER → a "kubernetes_cluster" BillingResource:
// the CONTROL-PLANE fee only. Worker VMs live in the customer's own project and are
// billed by the existing instance/volume/LB providers (plan D4) — pricing them here would
// double-charge.
type KubernetesClusterProvider struct{}

func NewKubernetesClusterProvider() *KubernetesClusterProvider { return &KubernetesClusterProvider{} }

func (p *KubernetesClusterProvider) Type() string { return cloud.TypeKubernetesCluster }

func (p *KubernetesClusterProvider) GetBillingInformation(_ context.Context, _ billingapi.BillingContext, cr *cloud.CloudResource) ([]*billingapi.BillingResource, error) {
	values := map[string]any{}
	notEligible := true
	if c, ok := mapAt(cr.Data, "cluster"); ok {
		values["display_name"] = c["name"]
		values["version"] = c["version"]
		values["status"] = c["status"]
		values["cp_replicas"] = c["cp_replicas"]
		if groups, ok := c["node_groups"].([]any); ok {
			values["node_groups"] = len(groups)
		}
		// Charge only once the control plane is actually up (the endpoint exists): provisioning
		// time is free, and a cluster degraded AFTER coming up keeps billing (it still holds
		// control-plane capacity) — same stance as the instance provider's status handling.
		endpoint := str(c["endpoint"])
		values["endpoint"] = endpoint
		notEligible = endpoint == ""
	}
	return []*billingapi.BillingResource{{
		ResourceID:            cr.ID,
		ProjectID:             cr.ProjectID,
		ResourceType:          "kubernetes_cluster",
		Values:                values,
		BillingResourceType:   kubernetesClusterType(),
		NotEligibleForBilling: notEligible,
	}}, nil
}

func kubernetesClusterType() *billingapi.BillingResourceType {
	s := func(n string) billingapi.ResourceAttribute {
		return billingapi.ResourceAttribute{Name: n, Type: "string"}
	}
	n := func(nm string) billingapi.ResourceAttribute {
		return billingapi.ResourceAttribute{Name: nm, Type: "number"}
	}
	return &billingapi.BillingResourceType{ResourceType: "kubernetes_cluster", Attributes: []billingapi.ResourceAttribute{
		s("display_name"), s("version"), s("status"), s("endpoint"), n("cp_replicas"), n("node_groups"),
	}}
}

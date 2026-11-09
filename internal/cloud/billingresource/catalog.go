package billingresource

import "github.com/mutex-solutions/proxcloud/pkg/billingapi"

// Catalog returns the BillingResourceType catalog the cloud provider bills — the same per-type
// attribute schemas the charge engine maps resources to. Backs the admin PricePlanRule form
// (GET /admin/price-plan[/{id}]/resource-types returns these types): the form picks a
// resourceType then filters/prices on its attributes. Order is stable for the UI.
func Catalog() []*billingapi.BillingResourceType {
	return []*billingapi.BillingResourceType{
		instanceType(),
		instanceTrafficType(),
		volumeType(),
		floatingIPType(),
		imageType(),
		kubernetesClusterType(),
		databaseClusterType(),
		bucketType(),
		shareType(),
		serverProtectionType(),
		serverSnapshotType(),
		sandboxType(),
		sandboxSnapshotType(),
	}
}

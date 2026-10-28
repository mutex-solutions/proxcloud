package project

// cloud_kamaji_create.go — the Proxmox provisioning seam of Managed Kubernetes. Until it lands,
// kamaji cluster CREATE fails honestly; read/sync/delete paths are unaffected.

import (
	"context"
	"fmt"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
)

var errKamajiProxmoxPending = fmt.Errorf("Managed Kubernetes on Proxmox is not available yet (Phase K)")

// kamajiProvisionCluster prepares the cluster's infrastructure side before the Application is
// applied: the per-cluster credential and the node/LB address ranges.
//
// TODO(P7): CAPMOX credentialsRef (per-cluster PVE token) + IPAM range carving from the tenant
// vnet subnet replace this seam.
func (h *Handler) kamajiProvisionCluster(ctx context.Context, p *Project, d kamaji.ClusterDefaults, spec *kamaji.ClusterSpec) error {
	return errKamajiProxmoxPending
}

package project

// cloud_dbaas_create.go — the Proxmox provisioning seam of Managed Databases. On Proxmox the DB
// cluster exposes databases on a routable services subnet, tenants reach it over their normal
// egress path, and isolation is loadBalancerSourceRanges. Until that lands, dbaas CREATE fails
// honestly; read/sync/delete paths are unaffected.

import (
	"context"
	"fmt"
	"net"

	"github.com/mutex-solutions/proxcloud/internal/cloud/dbaas"
)

var errDbaasProxmoxPending = fmt.Errorf("Managed Databases on Proxmox are not available yet (Phase D)")

// dbaasProvisionDatabase prepares the database's network exposure before the Application is
// applied.
//
// TODO(P7): MetalLB services subnet (address pool for the LoadBalancer Service) replaces this
// seam.
func (h *Handler) dbaasProvisionDatabase(ctx context.Context, p *Project, cfg dbaas.Config, spec dbaas.DatabaseSpec) error {
	return errDbaasProxmoxPending
}

// dbaasValidateCIDRs is pure input validation for loadBalancerSourceRanges.
func dbaasValidateCIDRs(cidrs []string) error {
	for _, c := range cidrs {
		if _, _, err := net.ParseCIDR(c); err != nil {
			return fmt.Errorf("invalid CIDR %q", c)
		}
	}
	return nil
}

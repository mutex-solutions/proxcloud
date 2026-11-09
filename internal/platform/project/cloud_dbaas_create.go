package project

// cloud_dbaas_create.go — the create-time pieces of Managed Databases. On Proxmox the DB cluster
// exposes a database through a MetalLB-assigned address in a routable services subnet that
// tenants reach over their normal egress, and isolation is loadBalancerSourceRanges — so create
// has no tenant-network leg: nothing is resolved, shared or placed before the Application is
// applied.

import (
	"context"
	"fmt"
	"net"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/dbaas"
)

// dbaasSourceForRestore proves the restore source belongs to THIS project and matches the
// engine being created. Cross-project recovery would be a data breach with a friendly UI, and
// a cross-engine one is a corrupt datadir.
func (h *Handler) dbaasSourceForRestore(ctx context.Context, proj *Project, spec dbaas.DatabaseSpec) (*cloud.CloudResource, error) {
	rows, err := h.cloud.FindByProjectAndType(ctx, proj.ID, cloud.TypeDatabaseCluster)
	if err != nil {
		return nil, fmt.Errorf("could not verify the source database")
	}
	for i := range rows {
		cr := &rows[i]
		if cr.ExternalID != spec.RestoreFrom.SourceID {
			continue
		}
		d, _ := cr.Data["database"].(map[string]any)
		if engine, _ := d["engine"].(string); engine != spec.Engine {
			return nil, fmt.Errorf("the source database runs %s, not %s", engine, spec.Engine)
		}
		if backup, _ := d["backup"].(map[string]any); backup == nil || backup["enabled"] != true {
			return nil, fmt.Errorf("the source database has no backups to restore from")
		}
		return cr, nil
	}
	return nil, fmt.Errorf("source database %q is not in this project", spec.RestoreFrom.SourceID)
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

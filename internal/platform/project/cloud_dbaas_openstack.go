package project

// cloud_dbaas_openstack.go — STUB. The neutron-RBAC network-share machinery
// (share the tenant network with the dbaas keystone project so the Octavia LB
// could land its VIP there) is deleted with the OpenStack backend and replaced
// by NOTHING per the plan: on Proxmox the DB cluster exposes databases on a
// routable "dbaas services subnet" via MetalLB, tenants reach it over the normal
// egress path, and isolation is loadBalancerSourceRanges (enforced for real by
// kube-proxy/Cilium). Phase D rewires the create path; until then dbaas CREATE
// on this fork fails honestly and the teardown/sweep legs are inert no-ops.

import (
	"context"
	"fmt"
	"net"

	"github.com/mutex-solutions/proxcloud/internal/cloud/dbaas"
)

var errDbaasProxmoxPending = fmt.Errorf("Managed Databases on Proxmox are not available yet (Phase D)")

// dbaasResolveTenantNetwork proved the customer network ids belonged to the
// project's OpenStack tenant. Phase D validates the vnet ownership row instead.
func (h *Handler) dbaasResolveTenantNetwork(ctx context.Context, p *Project, cfg dbaas.Config, spec dbaas.DatabaseSpec) (string, error) {
	return "", errDbaasProxmoxPending
}

// dbaasEnsureNetShare created the neutron RBAC share. Gone — no share exists in
// the Proxmox model.
func (h *Handler) dbaasEnsureNetShare(ctx context.Context, p *Project, cfg dbaas.Config, networkID string) (bool, error) {
	return false, errDbaasProxmoxPending
}

// dbaasRevokeNetShare revoked it. No-op: nothing to revoke.
func (h *Handler) dbaasRevokeNetShare(ctx context.Context, p *Project, osServiceID, osProjectID, osRegion, networkID string) error {
	return nil
}

// dbaasNetShareRevoker feeds FinalizeOrphans. No shares exist on this fork —
// resolve every marker as already-revoked so orphan GC converges.
func (h *Handler) dbaasNetShareRevoker(p *Project) dbaas.RevokeNetShareResolver {
	return func(ctx context.Context, osServiceID, osProjectID, osRegion, networkID string) error {
		return nil
	}
}

// dbaasValidateCIDRs is pure input validation (kept — Phase D reuses it for
// loadBalancerSourceRanges).
func dbaasValidateCIDRs(cidrs []string) error {
	for _, c := range cidrs {
		if _, _, err := net.ParseCIDR(c); err != nil {
			return fmt.Errorf("invalid CIDR %q", c)
		}
	}
	return nil
}

package project

// cloud_kamaji_openstack.go — STUB. The OpenStack provisioning legs of Managed
// Kubernetes (CAPO clouds.yaml appcred mint, neutron network resolve, nova
// server-group anti-affinity) were removed with the OpenStack backend; the
// Proxmox replacements (CAPMOX credentialsRef token mint, IPAM range carving,
// allowedNodes anti-affinity) land in Phase K as cloud_kamaji_proxmox.go.
// Until then, kamaji cluster CREATE fails honestly; sync/read/delete paths that
// touched server-groups are no-ops (the whole server-group machinery is deleted
// без replacement per the plan — vnets have no port security to manage either).

import (
	"context"
	"fmt"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
)

var errKamajiProxmoxPending = fmt.Errorf("Managed Kubernetes on Proxmox is not available yet (Phase K)")

// kamajiProvisionCloud minted the per-cluster OpenStack appcred. Phase K mints a
// PVE token (proxcloud-capi@pve!<clusterID>) instead.
func (h *Handler) kamajiProvisionCloud(ctx context.Context, p *Project, d kamaji.ClusterDefaults, spec *kamaji.ClusterSpec) error {
	return errKamajiProxmoxPending
}

// kamajiResolveClusterNetwork resolved the cluster's neutron network/subnet.
// Phase K carves nodeRange/lbRange from the tenant vnet subnet instead.
func (h *Handler) kamajiResolveClusterNetwork(ctx context.Context, p *Project, osSvcID string, d kamaji.ClusterDefaults, spec *kamaji.ClusterSpec) error {
	return errKamajiProxmoxPending
}

// kamajiSyncServerGroups managed nova anti-affinity server groups per node group.
// Proxmox anti-affinity is best-effort via CAPMOX allowedNodes — nothing to sync.
func (h *Handler) kamajiSyncServerGroups(ctx context.Context, p *Project, clusterID string, groups []kamaji.NodeGroup) error {
	return nil
}

// kamajiReleaseServerGroups reaped the cluster's nova server groups on delete.
// No server groups exist on Proxmox — the delete path proceeds clean.
func (h *Handler) kamajiReleaseServerGroups(ctx context.Context, p *Project, clusterID string) error {
	return nil
}

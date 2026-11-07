package project

// cloud_kamaji_create.go — the Proxmox side of a Managed Kubernetes cluster: what its workers are made
// of, where they sit, what they run under. The chart turns the result into CAPMOX objects.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/csigw"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/cloud/truenas"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// kamajiWorkerAddrs + kamajiLBAddrs are the addresses a cluster reserves on its vnet. The workers' range
// caps a cluster: a rolling replacement needs one spare, hence kamajiMaxWorkers.
const (
	kamajiWorkerAddrs = 16
	kamajiLBAddrs     = 8
	kamajiMaxWorkers  = kamajiWorkerAddrs - 1
)

// kamajiReservation names a cluster's addresses in the allocation store.
func kamajiReservation(clusterID string) string { return "k8s:" + clusterID }

// kamajiCompute resolves the proxmox provider hosting a project's workers.
func (h *Handler) kamajiCompute(ctx context.Context, p *Project, d kamaji.ClusterDefaults) (*externalservice.ExternalService, error) {
	for _, id := range p.ServiceIDs() {
		if d.ProxmoxServiceID != "" && id != d.ProxmoxServiceID {
			continue
		}
		es, err := h.esSvc.Get(ctx, id)
		if err == nil && es != nil && es.IsProxmox() {
			return es, nil
		}
	}
	return nil, httpx.BadRequest("This project has no Proxmox service for the worker nodes")
}

// kamajiGroupFlavor is the flavor a group may use: an active one, or the retired one it already runs, so that retiring a
// flavor does not freeze the clusters on it. Like the allowlist in SET_NODE_GROUPS, this holds only for that group.
func (h *Handler) kamajiGroupFlavor(ctx context.Context, ng *kamaji.NodeGroup, prev []kamaji.NodeGroup) (*pveflavor.Flavor, error) {
	running := slices.ContainsFunc(prev, func(p kamaji.NodeGroup) bool { return p.Name == ng.Name && p.FlavorID == ng.FlavorID })
	if running && h.pveFlavors != nil {
		if fl, err := h.pveFlavors.Get(ctx, ng.FlavorID); err != nil {
			return nil, err
		} else if fl != nil {
			return fl, nil
		}
	}
	fl, herr := h.pveFlavorOf(ctx, ng.FlavorID)
	if herr != nil {
		return nil, herr
	}
	return fl, nil
}

// kamajiHydrateGroups fills what the client does not choose: VM size and tags from the flavor. Run on
// create and on every node group change, because the values carry them. prev is what the cluster runs now.
func (h *Handler) kamajiHydrateGroups(ctx context.Context, compute *externalservice.ExternalService, d kamaji.ClusterDefaults, clusterID string, groups, prev []kamaji.NodeGroup) error {
	total := 0
	profile := compute.PVEDefaultStorageProfile()
	for i := range groups {
		ng := &groups[i]
		fl, err := h.kamajiGroupFlavor(ctx, ng, prev)
		if err != nil {
			return err
		}
		// ponytail: GPU workers need hostpci in the machine template, which CAPMOX has no field for.
		if fl.GPUCount > 0 {
			return httpx.BadRequest(fmt.Sprintf("node group %q: GPU flavors are not available for node groups yet", ng.Name))
		}
		ng.Cores, ng.MemoryMiB = fl.VCPUs, fl.RAMMb
		if ng.RootVolumeGiB == 0 {
			ng.RootVolumeGiB = max(d.RootVolumeGiB, fl.DiskGb)
		}
		tags := []string{pve.TagManaged, pve.FlavorTag(fl.Slug), "pc-k8s-" + clusterID}
		p := fl.StorageProfile
		if p == "" {
			p = profile
		}
		if p != "" {
			tags = append(tags, pve.ProfileTag(p))
		}
		ng.Tags = tags
		total += ng.Count
	}
	if total > kamajiMaxWorkers {
		return httpx.BadRequest(fmt.Sprintf("A cluster holds at most %d workers", kamajiMaxWorkers))
	}
	return nil
}

// kamajiEnforceQuota gates workers like servers: they land in the project's pool and count once synced. prev is what the
// cluster already runs, so an edit asks only for its difference. Workers on a shared pool are not the project's.
func (h *Handler) kamajiEnforceQuota(ctx context.Context, proj *Project, compute *externalservice.ExternalService, d kamaji.ClusterDefaults, groups, prev []kamaji.NodeGroup) error {
	if d.Shared != nil {
		return nil
	}
	// The catalog as it is: a group on a flavor retired since still holds that size, and hydrating has already vetted groups.
	flavor := func(id string) (*pveflavor.Flavor, error) { return h.pveFlavors.Get(ctx, id) }
	want, err := kamajiWorkerDemand(groups, prev, d.RootVolumeGiB, flavor)
	if err != nil {
		return err
	}
	if herr := h.enforcePVEQuota(ctx, proj, compute, want); herr != nil {
		return herr
	}
	return nil
}

// KamajiCheckNodeGroups vets a node group edit against what the cluster's row says it runs: flavors, sizes and quota.
func (h *Handler) KamajiCheckNodeGroups(ctx context.Context, proj *Project, compute *externalservice.ExternalService, d kamaji.ClusterDefaults, cr *cloud.CloudResource, groups []kamaji.NodeGroup) error {
	prev := nodeGroupsFromCache(cr)
	if err := h.kamajiHydrateGroups(ctx, compute, d, cr.ExternalID, groups, prev); err != nil {
		return err
	}
	return h.kamajiEnforceQuota(ctx, proj, compute, d, groups, prev)
}

// kamajiWorkerDemand is what groups add on top of prev. A flavor deleted from the catalog (nil) has no known size, so its
// workers count as nothing: moving off one asks for the whole new size while the old workers still hold theirs.
func kamajiWorkerDemand(groups, prev []kamaji.NodeGroup, rootGiB int, flavor func(string) (*pveflavor.Flavor, error)) (pveQuotaDemand, error) {
	var want pveQuotaDemand
	add := func(gs []kamaji.NodeGroup, sign int) error {
		for _, ng := range gs {
			fl, err := flavor(ng.FlavorID)
			if err != nil {
				return err
			}
			if fl == nil {
				continue
			}
			disk := ng.RootVolumeGiB
			if disk == 0 {
				disk = max(rootGiB, fl.DiskGb)
			}
			n := sign * ng.Count
			want.Servers += n
			want.VCPUs += n * fl.VCPUs
			want.RAMMb += n * fl.RAMMb
			want.DiskGb += n * disk
		}
		return nil
	}
	if err := add(groups, 1); err != nil {
		return want, err
	}
	return want, add(prev, -1)
}

// kamajiNetwork resolves the customer's pick to the vnet row of this project.
func (h *Handler) kamajiNetwork(ctx context.Context, proj *Project, compute *externalservice.ExternalService, netID string) (vnet, zone string, err error) {
	if netID == "" {
		return "", "", httpx.BadRequest("Select one of this project's networks for the worker nodes")
	}
	cr, err := h.cloud.FindByServiceIDAndExternalID(ctx, compute.ID, netID)
	if err != nil || cr == nil {
		cr, err = h.cloud.FindByID(ctx, netID)
	}
	if err != nil || cr == nil || cr.ProjectID != proj.ID || cr.Type != cloud.TypeNetwork {
		return "", "", httpx.BadRequest("Select one of this project's networks for the worker nodes")
	}
	n, _ := cr.Data["network"].(map[string]any)
	return strAny(n["vnet"]), strAny(n["zone"]), nil
}

// kamajiProvisionCluster resolves the cluster's infrastructure and the secrets it runs under. What it took
// (addresses, tokens, dataset) is released again when it fails, and by KamajiReleaseCluster after a delete.
func (h *Handler) kamajiProvisionCluster(ctx context.Context, proj *Project, ks *kamaji.Service, spec *kamaji.ClusterSpec) (sec kamaji.ClusterSecrets, err error) {
	d := ks.Config().Defaults
	compute, err := h.kamajiCompute(ctx, proj, d)
	if err != nil {
		return sec, err
	}
	sec.ComputeServiceID = compute.ID
	ps, err := h.pveFor(compute)
	if err != nil {
		return sec, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "proxmox client not ready")
	}
	if err := h.kamajiHydrateGroups(ctx, compute, d, spec.ID, spec.NodeGroups, nil); err != nil {
		return sec, err
	}
	if err := h.kamajiEnforceQuota(ctx, proj, compute, d, spec.NodeGroups, nil); err != nil {
		return sec, err
	}
	if h.publicIPs == nil {
		return sec, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "IP allocation service not configured")
	}

	storage, classes := kamajiStorage(compute, d, spec)
	if storage == "" {
		return sec, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "The Proxmox provider has no storage profile for boot disks")
	}
	gatewayCSI, err := h.kamajiGatewayCSI(ks.Config(), compute)
	if err != nil {
		return sec, err
	}
	classes = kamajiTenantClasses(gatewayCSI, d, classes)
	in := kamaji.ClusterInfra{Region: compute.PVERegion(), Storage: storage, StorageClasses: classes, CSIGateway: gatewayCSI}

	var pool externalservice.PVEPublicNetwork
	zone := ""
	if sw := d.Shared; sw != nil {
		in.Pool, in.Bridge, in.Gateway, in.Prefix = sw.Pool, sw.Bridge, sw.Gateway, sw.Prefix
		pool = externalservice.PVEPublicNetwork{ID: sw.Bridge, CIDR: fmt.Sprintf("%s/%d", sw.Gateway, sw.Prefix), Gateway: sw.Gateway, Ranges: []string{sw.First + "-" + sw.Last}}
	} else {
		in.Pool = compute.PoolFor(proj.ID)
		var vnet string
		if vnet, zone, err = h.kamajiNetwork(ctx, proj, compute, spec.NetworkID); err != nil {
			return sec, err
		}
		cidr, gateway, first, last, static, serr := ps.VnetStaticPool(ctx, vnet)
		if serr != nil {
			return sec, upstreamErr(computeBackend, serr)
		}
		if !static {
			return sec, httpx.BadRequest("Worker nodes need a network without DHCP (an EVPN or VLAN zone)")
		}
		_, ipnet, perr := net.ParseCIDR(cidr)
		if perr != nil {
			return sec, upstreamErr(computeBackend, perr)
		}
		ones, _ := ipnet.Mask.Size()
		in.Bridge, in.Gateway, in.Prefix = vnet, gateway, ones
		pool = externalservice.PVEPublicNetwork{ID: vnet, CIDR: cidr, Gateway: gateway, Ranges: []string{first + "-" + last}}
		if err = kamajiCheckBootISO(ctx, ps, compute.PVEBootISOStorage(), storage); err != nil {
			return sec, err
		}
	}

	// The marker goes first: nothing else records this cluster until CreateCluster, and a rollback can fail too.
	if err = ks.RecordCluster(ctx, proj.ID, spec.ID, compute.ID); err != nil {
		return sec, upstreamErr("Kubernetes backend", err)
	}
	defer func() {
		if err != nil {
			if rerr := ks.AbandonCluster(context.WithoutCancel(ctx), proj.ID, spec.ID, compute.ID); rerr != nil {
				slog.Warn("kamaji: release after a failed create, the sweep retries", "cluster", spec.ID, "err", rerr)
			}
		}
	}()
	addrs, err := h.publicIPs.Reserve(ctx, compute.ID, proj.ID, kamajiReservation(spec.ID), pool, kamajiWorkerAddrs+kamajiLBAddrs)
	if err != nil {
		return sec, err
	}
	in.Addresses = addrs[:kamajiWorkerAddrs]
	for _, ip := range addrs[kamajiWorkerAddrs:] {
		in.LBAddresses = append(in.LBAddresses, ip+"/32") // MetalLB takes no bare address
	}
	spec.Infra = in

	apiURL := d.PVEAPIURL
	if apiURL == "" {
		apiURL = strAny(compute.Config["apiUrl"])
	}
	var capiID, capiSecret string
	if sw := d.Shared; sw != nil {
		capiID, capiSecret = sw.CAPITokenID, sw.CAPISecret
	} else {
		toks, terr := ps.C.MintClusterTokens(ctx, spec.ID, pve.ClusterTokenScope{
			Pool: in.Pool, TemplatePool: pve.TemplatePool, BootStorage: storage,
			ISOStorage: compute.PVEBootISOStorage(), Zone: zone, Vnet: in.Bridge,
		})
		if terr != nil {
			err = upstreamErr(computeBackend, terr)
			return sec, err
		}
		capiID, capiSecret = toks.CAPIID, toks.CAPISecret
	}
	sec.Cloud = map[string]string{"url": apiURL, "token": capiID, "secret": capiSecret}
	switch {
	case gatewayCSI:
		cred := csigw.Credential{ID: spec.ID, ProjectID: proj.ID, ComputeServiceID: compute.ID, Region: in.Region,
			Pool: in.Pool, Storages: classStorages(classes)}
		if sec.CSIGateway, err = h.csiGW.Reg.ControllerSecret(ctx, cred, h.csiGW.Endpoint); err != nil {
			return sec, err
		}
		sec.CSIManifest = kamajiCSINamespace
	case len(classes) > 0: // sharedWorkers: the operator's token, on the API URL CAPMOX uses
		sec.CSIManifest = kamajiCSIManifest(apiURL, d.Shared.CSITokenID, d.Shared.CSISecret, in.Region)
	}
	if d.RWXGiB > 0 {
		if err = h.kamajiRWX(ctx, proj, spec.ID, d.RWXGiB, &in); err != nil {
			return sec, err
		}
		spec.Infra = in
	}
	return sec, nil
}

// kamajiRWX gives a cluster its dataset: one ordinary share of the project, so it bills, counts against the share
// quota and follows the project's addresses like any other. No TrueNAS provider on the project: no RWX class.
func (h *Handler) kamajiRWX(ctx context.Context, proj *Project, clusterID string, sizeGiB int, in *kamaji.ClusterInfra) error {
	services, err := h.trueNASServices(ctx, proj)
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return nil
	}
	es := services[0]
	if err := h.shareQuotaCheck(ctx, proj, es, "", sizeGiB); err != nil {
		return err
	}
	unlock, err := h.lockShareAccess(ctx, proj.ID)
	if err != nil {
		return err
	}
	defer unlock()
	networks, err := h.shareNetworks(ctx, proj)
	if err != nil {
		return err
	}
	ts, err := es.TrueNASService()
	if err != nil {
		return err
	}
	defer ts.C.Close()
	sh, err := ts.Create(ctx, proj.ID, kamajiRWXShare(clusterID), truenas.ProtocolNFS, sizeGiB, networks)
	if err != nil {
		return shareErr(err)
	}
	now := time.Now().UTC()
	if _, err := h.cloud.Insert(ctx, &cloud.CloudResource{
		ServiceID: es.ID, ProjectID: proj.ID, Region: es.CephRegion(), Type: cloud.TypeShare,
		ExternalID: sh.Dataset, Data: ts.ShareData(proj.ID, sh), CreatedAt: &now, UpdatedAt: &now,
	}); err != nil {
		// The release finds a dataset by its row, and this one has none. Should the delete fail as well, the sync adds
		// the row and the tenant can delete it as a share.
		if derr := ts.Delete(context.WithoutCancel(ctx), proj.ID, sh.Dataset); derr != nil {
			slog.Warn("kamaji: dataset left after a failed create", "dataset", sh.Dataset, "err", derr)
		}
		return err
	}
	in.RWXServer, in.RWXShare = ts.Host, truenas.MountPath(sh.Dataset)
	return nil
}

func kamajiRWXShare(clusterID string) string { return "k8s-" + clusterID }

// KamajiReleaseCluster runs after a cluster's delete cascade has finished, and after a failed create
// (kamaji.OnClusterGone). An error keeps the marker, so the sweep retries: only a provider that is really gone
// counts as nothing to release on. csi, when set, goes first: it revokes the cluster's gateway credential and deletes the
// disks of its volumes (csigw.Store.ReleaseCluster).
func KamajiReleaseCluster(services serviceGetter, shares shareRowFinder, pveFor func(*externalservice.ExternalService) (*pve.Service, error), ips *PublicIPRepo, shared bool, csi func(ctx context.Context, clusterID string) error) func(ctx context.Context, computeID, projectID, clusterID string) error {
	rest := kamajiReleaseInfra(services, shares, pveFor, ips, shared)
	return func(ctx context.Context, computeID, projectID, clusterID string) error {
		// A CSI step that fails (a disk in use, a provider that lost its gateway token) keeps the marker, but does not
		// hold back the tokens, addresses and dataset: each part is safe to run again.
		var csiErr error
		if csi != nil {
			csiErr = csi(ctx, clusterID)
		}
		return errors.Join(csiErr, rest(ctx, computeID, projectID, clusterID))
	}
}

func kamajiReleaseInfra(services serviceGetter, shares shareRowFinder, pveFor func(*externalservice.ExternalService) (*pve.Service, error), ips *PublicIPRepo, shared bool) func(ctx context.Context, computeID, projectID, clusterID string) error {
	return func(ctx context.Context, computeID, projectID, clusterID string) error {
		es, err := services.Get(ctx, computeID)
		if err != nil && !errors.Is(err, externalservice.ErrNotFound) {
			return err
		}
		// sharedWorkers mints nothing, and its portal token may not touch tokens at all.
		if err == nil && pveFor != nil && !shared {
			ps, err := pveFor(es)
			if err != nil {
				return err
			}
			if err := ps.C.RevokeClusterTokens(ctx, clusterID); err != nil {
				return err
			}
		}
		if err := kamajiDeleteDataset(ctx, services, shares, projectID, clusterID); err != nil {
			return err
		}
		if ips != nil {
			return ips.ReleaseServer(ctx, computeID, kamajiReservation(clusterID))
		}
		return nil
	}
}

type serviceGetter interface {
	Get(ctx context.Context, id string) (*externalservice.ExternalService, error)
}

type shareRowFinder interface {
	FindByProjectAndType(ctx context.Context, projectID, resourceType string) ([]cloud.CloudResource, error)
}

// kamajiDeleteDataset removes a cluster's dataset, like its proxmox-csi disks, from the NAS its share row names: a
// broken NAS holding nothing of this cluster must not block its release. No row: no dataset, or the sync drops it.
func kamajiDeleteDataset(ctx context.Context, services serviceGetter, shares shareRowFinder, projectID, clusterID string) error {
	rows, err := shares.FindByProjectAndType(ctx, projectID, cloud.TypeShare)
	if err != nil {
		return err
	}
	for i := range rows {
		if !strings.HasSuffix(rows[i].ExternalID, "/"+projectID+"-"+kamajiRWXShare(clusterID)) {
			continue
		}
		nas, err := services.Get(ctx, rows[i].ServiceID)
		if errors.Is(err, externalservice.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		ts, err := nas.TrueNASService()
		if err != nil {
			return err
		}
		err = ts.Delete(ctx, projectID, rows[i].ExternalID)
		ts.C.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// KamajiFactory builds the kamaji service with its infrastructure release wired in; the API and the sync
// job's orphan sweep use the same one.
func KamajiFactory(esSvc *externalservice.Service, shares shareRowFinder, pveFor func(*externalservice.ExternalService) (*pve.Service, error), ips *PublicIPRepo, csi *CSIGateway) func(*externalservice.ExternalService) (*kamaji.Service, error) {
	var release func(ctx context.Context, clusterID string) error
	if csi != nil {
		release = func(ctx context.Context, clusterID string) error {
			return csi.Reg.ReleaseCluster(ctx, clusterID, csi.GW)
		}
	}
	return func(es *externalservice.ExternalService) (*kamaji.Service, error) {
		cfg := es.KamajiConfig()
		ks, err := kamaji.New(cfg, es.ID)
		if err != nil {
			return nil, err
		}
		ks.OnClusterGone(KamajiReleaseCluster(esSvc, shares, pveFor, ips, cfg.Defaults.Shared != nil, release))
		if cfg.Defaults.Shared == nil && pveFor != nil {
			ks.OnClusterLive(KamajiPinBootISO(esSvc, pveFor))
		}
		return ks, nil
	}
}

// KamajiPinBootISO moves a cluster minted before bootstrap ISOs left shared storage onto the provider's ISO store, on
// the sweep's pass after the portal upgrade (docs/managed-k8s.md, "Bootstrap ISOs").
func KamajiPinBootISO(services serviceGetter, pveFor func(*externalservice.ExternalService) (*pve.Service, error)) func(ctx context.Context, computeID, projectID, clusterID string) error {
	return func(ctx context.Context, computeID, _, clusterID string) error {
		es, err := services.Get(ctx, computeID)
		if err != nil {
			return err
		}
		ps, err := pveFor(es)
		if err != nil {
			return err
		}
		waiting, err := ps.C.PinClusterISO(ctx, clusterID, es.PVEBootISOStorage())
		if waiting != "" {
			slog.Info("kamaji: bootstrap ISO move waits for a node to join", "cluster", clusterID, "iso", waiting)
		}
		return err
	}
}

// kamajiTenantClasses is what a new cluster is offered through proxmox-csi: every class through the CSI gateway, none
// without it, since no PVE token goes to a tenant cluster. sharedWorkers keeps its classes on the operator's token:
// its tenants already share a pool and a token by the operator's choice.
func kamajiTenantClasses(gateway bool, d kamaji.ClusterDefaults, classes []kamaji.StorageClass) []kamaji.StorageClass {
	if gateway || d.Shared != nil {
		return classes
	}
	return nil
}

// kamajiStorage picks the boot store and the StorageClasses: every profile that can hold a boot disk,
// the default one first, which the chart makes the default class.
func kamajiStorage(compute *externalservice.ExternalService, d kamaji.ClusterDefaults, spec *kamaji.ClusterSpec) (string, []kamaji.StorageClass) {
	node := kamajiFirstNode(d, spec)
	profiles := compute.PVEStorageProfiles()
	def := compute.PVEDefaultStorageProfile()
	var classes []kamaji.StorageClass
	for slug, p := range profiles {
		st := p.StorageFor(node)
		if st == "" || !p.BootDisk() {
			continue
		}
		c := kamaji.StorageClass{Name: slug, Storage: st}
		if slug == def {
			classes = append([]kamaji.StorageClass{c}, classes...)
		} else {
			classes = append(classes, c)
		}
	}
	if len(classes) == 0 {
		return "", nil
	}
	return classes[0].Storage, classes
}

// kamajiFirstNode is the first PVE node pinned by the version's image.
func kamajiFirstNode(d kamaji.ClusterDefaults, spec *kamaji.ClusterSpec) string {
	first, _, _ := strings.Cut(d.ImageFor(spec.Version, ""), "=")
	return first
}

func classStorages(classes []kamaji.StorageClass) []string {
	out := make([]string, 0, len(classes))
	for _, c := range classes {
		out = append(out, c.Storage)
	}
	return out
}

// kamajiCheckBootISO refuses a create whose workers' join tokens would sit on a shared store, or on a store the
// token could not use: CAPMOX would then wait on the first upload for ever.
func kamajiCheckBootISO(ctx context.Context, ps *pve.Service, iso, boot string) error {
	caps, err := ps.C.DetectStorage(ctx)
	if err != nil {
		return upstreamErr(computeBackend, err)
	}
	if p := pve.BootISOProblem(caps, iso, boot); p != "" {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, "Bootstrap ISO storage: "+p+" (Proxmox provider setting isoStorage)")
	}
	if err := ps.C.CheckBootISOGrant(ctx, iso); err != nil {
		if errors.Is(err, pve.ErrBootISOGrant) {
			return httpx.NewError(http.StatusConflict, http.StatusConflict, err.Error())
		}
		return upstreamErr(computeBackend, err)
	}
	return nil
}

// kamajiCSINamespace is what the resource set gives a cluster whose proxmox-csi controller runs on the services cluster:
// the node plugin's namespace, privileged, and no config. The node plugin reads none and never calls PVE.
const kamajiCSINamespace = `apiVersion: v1
kind: Namespace
metadata:
  name: csi-proxmox
  labels:
    pod-security.kubernetes.io/enforce: privileged
`

// kamajiGatewayCSI reports whether a new cluster's block volumes go through the CSI gateway: the portal serves one,
// the workers sit in the project's pool (sharedWorkers keeps its operator-run token), and the compute provider holds
// the gateway's PVE token. Every storage profile is then offered. A pinned chart older than
// kamaji.GatewayChartVersion is refused: it has no services-side controller, so the cluster's PVCs would never bind.
func (h *Handler) kamajiGatewayCSI(cfg kamaji.Config, compute *externalservice.ExternalService) (bool, error) {
	if h.csiGW == nil || h.csiGW.Endpoint.URL == "" || cfg.Defaults.Shared != nil {
		return false, nil
	}
	if _, ok := compute.PVECSIGatewayConfig(); !ok {
		return false, nil
	}
	if !kamaji.ChartHasGatewayCSI(cfg.ChartVersion) {
		return false, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, fmt.Sprintf(
			"The Kubernetes provider pins cluster chart %s; the CSI gateway needs %s or later", cfg.ChartVersion, kamaji.GatewayChartVersion))
	}
	return true, nil
}

// CSIGateway is what a cluster's block volumes need from the portal: the gateway's registry, and where the controllers
// on the services cluster reach the gateway. With no Endpoint.URL new clusters get no block volumes, and the release
// still removes the disks of clusters made while it was set.
type CSIGateway struct {
	Reg      *csigw.Store
	Endpoint csigw.Endpoint
	// GW builds the gateway's PVE client of a compute provider (csigw.ProviderClients), for the release.
	GW func(ctx context.Context, serviceID string) (*pve.Client, error)
}

// SetCSIGateway turns on gateway CSI for new clusters (nil = no block volumes, except on sharedWorkers).
func (h *Handler) SetCSIGateway(g *CSIGateway) { h.csiGW = g }

// kamajiCSIManifest is the tenant-side config of proxmox-csi, for sharedWorkers alone: a tenant with cluster-admin can
// read the operator's token, and its ACLs do not stop it attaching other tenants' disks.
func kamajiCSIManifest(url, tokenID, tokenSecret, region string) string {
	cfg, _ := json.Marshal(map[string]any{
		"features": map[string]any{"provider": "capmox"},
		"clusters": []any{map[string]any{
			"url": strings.TrimRight(url, "/") + "/api2/json",
			// ponytail: PVE's self-signed certificate. Needs a real or pinned CA in this config.
			"insecure":     true,
			"token_id":     tokenID,
			"token_secret": tokenSecret,
			"region":       region,
		}},
	})
	// JSON is YAML; quoted, it survives both manifest layers untouched.
	body, _ := json.Marshal(string(cfg))
	return kamajiCSINamespace + `---
apiVersion: v1
kind: Secret
metadata:
  name: proxmox-csi-plugin
  namespace: csi-proxmox
stringData:
  config.yaml: ` + string(body) + "\n"
}

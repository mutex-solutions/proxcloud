package project

// cloud_kamaji_create.go — the Proxmox side of a Managed Kubernetes cluster: what its workers are made
// of, where they sit, what they run under. The chart turns the result into CAPMOX objects.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/cloud/truenas"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
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

// kamajiHydrateGroups fills what the client does not choose: VM size and tags from the flavor. Run on
// create and on every node group change, because the values carry them.
func (h *Handler) kamajiHydrateGroups(ctx context.Context, compute *externalservice.ExternalService, d kamaji.ClusterDefaults, clusterID string, groups []kamaji.NodeGroup) error {
	total := 0
	profile := compute.PVEDefaultStorageProfile()
	for i := range groups {
		ng := &groups[i]
		fl, herr := h.pveFlavorOf(ctx, ng.FlavorID)
		if herr != nil {
			return herr
		}
		if fl == nil || !fl.Active {
			return httpx.BadRequest(fmt.Sprintf("node group %q: unknown flavor", ng.Name))
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

// kamajiProvisionCluster resolves the cluster's infrastructure and the secrets it runs under. Addresses
// and tokens it took are released again when it fails, and by kamajiReleaseCluster after a delete.
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
	if err := h.kamajiHydrateGroups(ctx, compute, d, spec.ID, spec.NodeGroups); err != nil {
		return sec, err
	}
	if h.publicIPs == nil {
		return sec, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "IP allocation service not configured")
	}

	storage, classes := kamajiStorage(compute, d, spec)
	if storage == "" {
		return sec, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "The Proxmox provider has no storage profile for boot disks")
	}
	in := kamaji.ClusterInfra{Region: compute.PVERegion(), Storage: storage, StorageClasses: classes}

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
			return sec, httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, serr.Error())
		}
		if !static {
			return sec, httpx.BadRequest("Worker nodes need a network without DHCP (an EVPN or VLAN zone)")
		}
		_, ipnet, perr := net.ParseCIDR(cidr)
		if perr != nil {
			return sec, httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, perr.Error())
		}
		ones, _ := ipnet.Mask.Size()
		in.Bridge, in.Gateway, in.Prefix = vnet, gateway, ones
		pool = externalservice.PVEPublicNetwork{ID: vnet, CIDR: cidr, Gateway: gateway, Ranges: []string{first + "-" + last}}
	}

	owner := kamajiReservation(spec.ID)
	addrs, err := h.publicIPs.Reserve(ctx, compute.ID, proj.ID, owner, pool, kamajiWorkerAddrs+kamajiLBAddrs)
	if err != nil {
		return sec, err
	}
	defer func() {
		if err != nil {
			_ = h.publicIPs.ReleaseServer(context.WithoutCancel(ctx), compute.ID, owner)
		}
	}()
	in.Addresses = addrs[:kamajiWorkerAddrs]
	for _, ip := range addrs[kamajiWorkerAddrs:] {
		in.LBAddresses = append(in.LBAddresses, ip+"/32") // MetalLB takes no bare address
	}
	spec.Infra = in

	apiURL := d.PVEAPIURL
	if apiURL == "" {
		apiURL = strAny(compute.Config["apiUrl"])
	}
	csiURL := d.PVECSIURL
	if csiURL == "" {
		csiURL = apiURL
	}
	var capiID, capiSecret, csiID, csiSecret string
	if sw := d.Shared; sw != nil {
		capiID, capiSecret, csiID, csiSecret = sw.CAPITokenID, sw.CAPISecret, sw.CSITokenID, sw.CSISecret
	} else {
		toks, terr := ps.C.MintClusterTokens(ctx, spec.ID, pve.ClusterTokenScope{
			Pool: in.Pool, TemplatePool: pve.TemplatePool, Storages: classStorages(classes),
			ISOStorage: pveISOStorage(compute), Zone: zone, Vnet: in.Bridge,
		})
		if terr != nil {
			err = httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, "Could not mint the cluster's Proxmox tokens: "+terr.Error())
			return sec, err
		}
		capiID, capiSecret, csiID, csiSecret = toks.CAPIID, toks.CAPISecret, toks.CSIID, toks.CSISecret
	}
	sec.Cloud = map[string]string{"url": apiURL, "token": capiID, "secret": capiSecret}
	sec.CSIManifest = kamajiCSIManifest(csiURL, csiID, csiSecret, in.Region)
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
	if _, err := h.cloud.Insert(ctx, &cloud.CloudResource{
		ServiceID: es.ID, ProjectID: proj.ID, Region: es.CephRegion(), Type: cloud.TypeShare,
		ExternalID: sh.Dataset, Data: ts.ShareData(proj.ID, sh),
	}); err != nil {
		return err // the sync adds the row; the cluster's release removes the dataset
	}
	in.RWXServer, in.RWXShare = ts.Host, truenas.MountPath(sh.Dataset)
	return nil
}

func kamajiRWXShare(clusterID string) string { return "k8s-" + clusterID }

// kamajiReleaseCluster runs after a cluster's delete cascade has finished (kamaji.OnClusterGone).
func kamajiReleaseCluster(esSvc *externalservice.Service, pveFor func(*externalservice.ExternalService) (*pve.Service, error), ips *PublicIPRepo, shared bool) func(ctx context.Context, computeID, projectID, clusterID string) error {
	return func(ctx context.Context, computeID, projectID, clusterID string) error {
		es, err := esSvc.Get(ctx, computeID)
		if err != nil || es == nil {
			return nil // provider gone, nothing to release on
		}
		// sharedWorkers mints nothing, and its portal token may not touch tokens at all.
		if pveFor != nil && !shared {
			ps, err := pveFor(es)
			if err != nil {
				return err
			}
			if err := ps.C.RevokeClusterTokens(ctx, clusterID); err != nil {
				return err
			}
		}
		// A cluster's dataset goes with it, like its proxmox-csi disks. Missing is fine: the sync drops the row.
		nas, err := esSvc.ListByType(ctx, "CLOUD")
		if err != nil {
			return err
		}
		for i := range nas {
			if !nas[i].IsTrueNAS() {
				continue
			}
			ts, err := nas[i].TrueNASService()
			if err != nil {
				return err
			}
			err = ts.DeleteByName(ctx, projectID, kamajiRWXShare(clusterID))
			ts.C.Close()
			if err != nil {
				return err
			}
		}
		if ips != nil {
			return ips.ReleaseServer(ctx, computeID, kamajiReservation(clusterID))
		}
		return nil
	}
}

// KamajiFactory builds the kamaji service with its infrastructure release wired in; the API and the sync
// job's orphan sweep use the same one.
func KamajiFactory(esSvc *externalservice.Service, pveFor func(*externalservice.ExternalService) (*pve.Service, error), ips *PublicIPRepo) func(*externalservice.ExternalService) (*kamaji.Service, error) {
	return func(es *externalservice.ExternalService) (*kamaji.Service, error) {
		cfg := es.KamajiConfig()
		ks, err := kamaji.New(cfg, es.ID)
		if err != nil {
			return nil, err
		}
		ks.OnClusterGone(kamajiReleaseCluster(esSvc, pveFor, ips, cfg.Defaults.Shared != nil))
		return ks, nil
	}
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

// pveISOStorage is the store CAPMOX uploads its bootstrap ISO to: the one holding templates and ISOs.
func pveISOStorage(compute *externalservice.ExternalService) string {
	if s := strAny(compute.Config["isoStorage"]); s != "" {
		return s
	}
	return "nas-templates"
}

// kamajiCSIManifest is the tenant-side config of proxmox-csi. The token is scoped to this cluster's
// project pool and stores, because a tenant with cluster-admin can read it.
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
	return `apiVersion: v1
kind: Namespace
metadata:
  name: csi-proxmox
  labels:
    pod-security.kubernetes.io/enforce: privileged
---
apiVersion: v1
kind: Secret
metadata:
  name: proxmox-csi-plugin
  namespace: csi-proxmox
stringData:
  config.yaml: ` + string(body) + "\n"
}

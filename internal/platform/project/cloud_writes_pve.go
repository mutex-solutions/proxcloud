package project

// cloud_writes_pve.go — the Proxmox VE write surface: create/delete/action/bulk
// forks off the generic cloud handlers (the kamaji/dbaas precedent — a proxmox
// provider never reaches the bucket WriteService). Everything cloud-touching
// lives in internal/cloud/pve; this file is the HTTP face + the cache bookkeeping.
//
// Tenancy: every op re-verifies pool membership live (pve.Service.ResolveVM)
// against the project's derived pool — the platform is the PDP; the cached row
// alone is never trusted for a mutation.

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// SetPVEFlavors wires the Proxmox flavor catalog (enables the SERVER create path
// + LIST_FLAVORS for proxmox providers).
func (h *Handler) SetPVEFlavors(r *pveflavor.Repo) { h.pveFlavors = r }

// pveService builds the pve.Service for a provider, failing the request when the
// seam is unwired.
func (h *Handler) pveService(w http.ResponseWriter, es *externalservice.ExternalService) (*pve.Service, bool) {
	if h.pveFor == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "proxmox service not configured")
		return nil, false
	}
	ps, err := h.pveFor(es)
	if err != nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "proxmox client not ready")
		return nil, false
	}
	return ps, true
}

// pveRegion resolves the region stamped on created resources.
func (h *Handler) pveRegion(p *Project, es *externalservice.ExternalService) string {
	if r := p.ServiceRegion(es.ID); r != "" {
		return r
	}
	if r := es.PVERegion(); r != "" {
		return r
	}
	return h.defaultRegion
}

// --- create ---

// pveCreate handles POST /project/{id}/cloud for a proxmox provider.
// Types: SERVER (clone-from-template) and KEYPAIR (platform-only — no cloud call;
// the public key is injected into VMs via cloud-init at create time).
func (h *Handler) pveCreate(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, reqType string, data map[string]any) {
	// A client that goes away must not cancel a change half way: it would leave a half-built VM.
	r = r.WithContext(context.WithoutCancel(r.Context()))
	switch reqType {
	case cloud.TypeServer:
		h.pveCreateServer(w, r, u, proj, es, data)
	case cloud.TypeKeypair:
		h.pveCreateKeypair(w, r, u, proj, es, data)
	case cloud.TypeNetwork:
		h.pveCreateNetwork(w, r, u, proj, es, data)
	case cloud.TypeVolume:
		h.pveCreateVolume(w, r, u, proj, es, data)
	case cloud.TypeSecurityGroup:
		h.pveCreateSecurityGroup(w, r, u, proj, es, data)
	case cloud.TypeImage:
		h.pveCreateImage(w, r, u, proj, es, data)
	default:
		h.fail(w, httpx.BadRequest(fmt.Sprintf("resource type %q is not supported on a Proxmox provider", reqType)))
	}
}

func (h *Handler) pveCreateServer(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, data map[string]any) {
	ctx := r.Context()
	if h.pveFlavors == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "flavor catalog not configured")
		return
	}
	name := strAny(data["name"])
	if herr := vmNameErr(name); herr != nil {
		h.fail(w, herr)
		return
	}
	fl, err := h.pveFlavors.Get(ctx, strAny(data["flavorId"]))
	if err != nil || fl == nil || !fl.Active {
		h.fail(w, httpx.BadRequest("Unknown flavor"))
		return
	}
	ps, ok := h.pveService(w, es)
	if !ok {
		return
	}
	tpl, herr := h.pveResolveTemplate(ctx, es, proj, strAny(data["imageId"]))
	if herr != nil {
		h.fail(w, herr)
		return
	}
	// A server must sit on one of the project's vnets: the template's own NIC is an
	// admin bridge with no anti-spoof set, so ipfilter would leave it dead (and
	// without ipfilter it would be an open L2 foothold).
	bridge := h.pveResolveBridge(ctx, es, proj, data)
	if bridge == "" {
		h.fail(w, httpx.BadRequest("Select one of this project's networks for the server"))
		return
	}
	rootGiB, rootType := 0, ""
	if rv, ok := data["rootVolume"].(map[string]any); ok {
		rootGiB, rootType = intAny(rv["sizeGiB"]), strAny(rv["type"])
	}
	if rootGiB > 0 && rootGiB < tpl.MinDiskGb {
		h.fail(w, httpx.BadRequest(fmt.Sprintf("Root disk must be at least %d GB for this image", tpl.MinDiskGb)))
		return
	}
	// GPU quota gate (platform-side; PVE has no quota concept at all).
	if fl.GPUCount > 0 {
		if err := h.enforcePVEGPUQuota(ctx, proj, es, fl, nil); err != nil {
			h.fail(w, err)
			return
		}
	}
	// Resource quota gate (vcpus/ram/servers/disk — platform-side). The disk is
	// the root size the VM really gets: a user-picked rootVolume overrides the flavor,
	// and a clone never gets smaller than its image.
	diskGb := fl.DiskGb
	if rootGiB > 0 {
		diskGb = rootGiB
	}
	diskGb = max(diskGb, tpl.MinDiskGb)
	if err := h.enforcePVEQuota(ctx, proj, es, pveQuotaDemand{
		VCPUs: fl.VCPUs, RAMMb: fl.RAMMb, Servers: 1, DiskGb: diskGb,
	}); err != nil {
		h.fail(w, err)
		return
	}
	// GPU: build the alias's device inventory (every mapping carrying it) and its
	// free slots per node — both feed the scheduler and, after placement, the exact
	// mappings this VM reserves. An unmapped GPU flavor fails honestly.
	var gpuInv *pve.GPUInventory
	var gpuFree map[string]int
	if fl.GPUCount > 0 {
		alias := cloud.NormalizeGPUAlias(fl.GPUModel)
		mappings := es.PVEGPUMappingsFor(alias)
		if len(mappings) == 0 {
			h.fail(w, httpx.BadRequest(fmt.Sprintf("GPU model %q has no PCI mapping on this provider", fl.GPUModel)))
			return
		}
		inv, ierr := ps.GPUInventoryFor(ctx, alias, mappings)
		if ierr != nil {
			h.fail(w, upstreamErr(computeBackend, ierr))
			return
		}
		gpuInv, gpuFree = inv, inv.FreeCountByNode()
	}
	// Placement inputs: live storage capabilities give the profile's storage per node (node
	// restriction + free space) and tell whether the template can be cloned off its node.
	caps, derr := ps.C.DetectStorage(ctx)
	if derr != nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "proxmox cluster unavailable")
		return
	}
	tplCfg, terr := ps.C.GetVMConfig(ctx, tpl.Node, tpl.VMID)
	if terr != nil {
		h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, fmt.Sprintf("Image %q is not available right now", tpl.Name)))
		return
	}
	// The boot volume type the user picked, else the flavor's profile, else the provider default.
	profiles := es.PVEStorageProfiles()
	profileSlug := rootType
	if profileSlug == "" {
		profileSlug = fl.StorageProfile
	}
	if profileSlug == "" {
		profileSlug = es.PVEDefaultStorageProfile()
	}
	if _, known := profiles[profileSlug]; !known && rootType != "" {
		h.fail(w, httpx.BadRequest("Unknown volume type"))
		return
	}
	pl, herr := pvePlacement(tpl, pve.TemplateShared(tplCfg, caps), profileSlug, profiles, caps)
	if herr != nil {
		h.fail(w, herr)
		return
	}
	// Scheduler: capacity-ranked node pick over the live cluster snapshot.
	nodes, nerr := ps.C.Nodes(ctx)
	vms, verr := ps.C.ClusterResources(ctx, "vm")
	if nerr != nil || verr != nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "proxmox cluster unavailable")
		return
	}
	reserve, rerr := ps.ReserveMem(ctx, vms)
	if rerr != nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "proxmox cluster unavailable")
		return
	}
	spreadGroup, strictSpread := pve.SpreadGroupSlug(strAny(data["spreadGroup"])), false
	if strAny(data["spreadGroup"]) != "" && spreadGroup == "" {
		h.fail(w, httpx.BadRequest("Spread group names use letters, digits and dashes"))
		return
	}
	if v, ok := data["spreadStrict"].(bool); ok {
		strictSpread = v && spreadGroup != ""
	}
	target, serr := pve.PickNode(nodes, vms, pve.SchedReq{
		VCPUs: fl.VCPUs, RAMMb: int64(fl.RAMMb), OvercommitCPU: es.PVEOvercommitCPU(),
		AllowedNodes: pl.allowed, GPUs: fl.GPUCount, GPUFree: gpuFree,
		Storage: pl.storage, DiskBytes: int64(diskGb) << 30, HeadroomPct: es.PVEStorageHeadroomPct(),
		ReserveMem: reserve,
		SpreadPool: ps.PoolFor(proj.ID), SpreadGroup: spreadGroup, SpreadStrict: strictSpread,
	})
	if serr != nil {
		if strictSpread {
			h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, fmt.Sprintf("Every node that can host this server already runs a member of spread group %q", spreadGroup)))
			return
		}
		msg := "No capacity available for this flavor (CPU, RAM, GPU or free disk space)"
		if len(pl.allowed) == 1 && pl.allowed[tpl.Node] {
			msg += fmt.Sprintf(" on node %s, the only node image %q can be created on", tpl.Node, tpl.Name)
		}
		h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, msg))
		return
	}
	// A per-node copy on the target clones locally; otherwise the primary clones across (shared).
	tplVMID, tplNode := tpl.VMID, tpl.Node
	if id := tpl.OnNode(target); id != 0 {
		tplVMID, tplNode = id, target
	}
	// "" (no profile) = full clone onto the template's storage on the target.
	cloneStorage := profiles[profileSlug].StorageFor(target)
	// Reserve the concrete mappings on the chosen node (distinct ids — two hostpci
	// entries on one mapping resolve to the same card and the VM refuses to start).
	var gpuMappings []string
	if gpuInv != nil {
		picked, perr := gpuInv.PickGPUSlots(target, fl.GPUCount)
		if perr != nil {
			h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, perr.Error()))
			return
		}
		gpuMappings = picked
	}
	// SSH key (cloud-init): resolve the user's stored KEYPAIR by name.
	sshKeys := ""
	if kn := strAny(data["keyName"]); kn != "" {
		if pub := h.pveKeypairPublicKey(ctx, es.ID, u, kn); pub != "" {
			sshKeys = pub
		} else {
			h.fail(w, httpx.BadRequest("Keypair not found"))
			return
		}
	}
	// Security groups: the user's pick (names or ids), else the project default.
	groups, gerr := ps.ServerSecurityGroups(ctx, proj.ID, strSlice(data["securityGroupNames"]), es.PVENetworkConfig().CIDRPool)
	var notFound *pve.SecurityGroupNotFoundError
	if errors.As(gerr, &notFound) {
		h.fail(w, httpx.BadRequest("Security group not found: "+notFound.Ref))
		return
	}
	if gerr != nil {
		h.fail(w, upstreamErr(computeBackend, gerr))
		return
	}
	// A vnet whose zone has no DHCP (EVPN, VLAN): the platform allocates net0's address.
	var assignIP func(context.Context, int) (string, int, string, error)
	allocatedVMID := ""
	cidr, gateway, first, last, static, serr := ps.VnetStaticPoolFor(ctx, bridge, pveWantedSubnet(data))
	if serr != nil {
		h.fail(w, upstreamErr(computeBackend, serr))
		return
	}
	if static {
		if h.publicIPs == nil {
			httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "IP allocation service not configured")
			return
		}
		pool := externalservice.PVEPublicNetwork{ID: bridge, CIDR: cidr, Gateway: gateway, Ranges: []string{first + "-" + last}}
		assignIP = func(ctx context.Context, vmid int) (string, int, string, error) {
			allocatedVMID = strconv.Itoa(vmid)
			a, err := h.publicIPs.Allocate(ctx, es.ID, proj.ID, allocatedVMID, "net0", pool)
			if err != nil {
				return "", 0, "", err
			}
			return a.IP, a.Prefix, gateway, nil
		}
	}
	// User-data, vendor-data or instance metadata need the platform's own seed.
	userData := strAny(data["userData"])
	if len(userData) > maxUserData {
		h.fail(w, httpx.BadRequest(fmt.Sprintf("User data must be at most %d bytes", maxUserData)))
		return
	}
	metadata, herr := h.pveSeedMetadata(ctx, es.ID, h.pveRegion(proj, es), data)
	if herr != nil {
		h.fail(w, herr)
		return
	}
	var seedStep func(context.Context, string, int) error
	seededVMID := 0
	loginUser := strAny(data["adminUser"])
	if loginUser != "" && (strAny(data["adminPass"]) == "" || !seedLoginUserRe.MatchString(loginUser)) {
		h.fail(w, httpx.BadRequest("A login user needs a password and a name of lowercase letters, digits, - and _"))
		return
	}
	seedIn := pveSeedInput{Name: name, UserData: userData, Password: strAny(data["adminPass"]), LoginUser: loginUser, SSHKeys: sshKeys, Metadata: metadata}
	if pveNeedsSeed(es, seedIn) {
		if h.seeds == nil {
			httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "Server seed store not configured")
			return
		}
		write := h.pveSeedStep(ps, es, proj, seedIn, static)
		seedStep = func(ctx context.Context, node string, vmid int) error {
			seededVMID = vmid
			return write(ctx, node, vmid)
		}
	}
	started := time.Now().UTC()
	cr, cerr := ps.CreateServer(ctx, pve.CreateServerReq{
		Seed:        seedStep,
		AssignIP:    assignIP,
		SpreadGroup: spreadGroup,
		ProjectID:   proj.ID,
		Region:      h.pveRegion(proj, es),
		Name:        name,
		Spec: pve.ServerSpec{
			Slug: fl.Slug, VCPUs: fl.VCPUs, RAMMb: fl.RAMMb, DiskGb: fl.DiskGb,
			StorageProfile: profileSlug, GPUModel: fl.GPUModel, GPUCount: fl.GPUCount,
		},
		TemplateVMID:   tplVMID,
		TemplateNode:   tplNode,
		ImageSlug:      tpl.Slug,
		TargetNode:     target,
		Storage:        cloneStorage,
		Bridge:         bridge,
		GPUMappingIDs:  gpuMappings,
		SSHKeys:        sshKeys,
		Password:       strAny(data["adminPass"]),
		RootDiskGiB:    rootGiB,
		SecurityGroups: groups,
	})
	if cerr != nil {
		if allocatedVMID != "" {
			_ = h.publicIPs.ReleaseNIC(ctx, es.ID, allocatedVMID, "net0")
		}
		if seededVMID != 0 {
			h.pveReleaseSeed(ctx, ps, es.ID, strconv.Itoa(seededVMID))
		}
		var he *httpx.HTTPError
		if errors.As(cerr, &he) { // the pool ran out
			h.fail(w, he)
			return
		}
		h.fail(w, pveVolumeErr(cerr))
		return
	}
	cr.ServiceID = es.ID
	h.pveArchiveStaleRow(ctx, cr, started)
	saved, ierr := h.cloud.Insert(ctx, cr)
	if ierr != nil {
		// The VM exists cloud-side; a 500 would invite a duplicate-create retry.
		// Accept and let the sync reconcile (the WriteService server precedent).
		saved = cr
	}
	// The new root disk is a VOLUME row: show (and bill) it now, not after the next sync.
	h.pveSyncVolumes(ctx, es, proj)
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_CREATE", "", saved)
	httpx.OK(w, *saved)
}

// pveImageCatalog is the provider's public images: tagged templates read live from the cluster plus
// config.templates. A cluster that cannot be read leaves only the configured pins.
func (h *Handler) pveImageCatalog(ctx context.Context, es *externalservice.ExternalService) []externalservice.PVETemplate {
	var found []pve.DiscoveredTemplate
	if ps, err := h.pveFor(es); err == nil {
		if found, err = ps.C.Templates(ctx); err != nil {
			slog.Warn("template discovery failed", "service", es.ID, "err", err)
		}
	}
	return es.PVEImageCatalog(found)
}

// pveArchiveStaleRow archives a row still sitting on a vmid PVE just handed out to a new object (the
// old one went away less than a sample ago): the upsert would otherwise give the new object the old
// row and bill item, possibly another project's. The vmid was free when the create started, so only
// a row older than that is stale; a younger one is the sampler having seen this very VM.
func (h *Handler) pveArchiveStaleRow(ctx context.Context, cr *cloud.CloudResource, started time.Time) {
	old, err := h.cloud.FindByServiceIDAndExternalID(ctx, cr.ServiceID, cr.ExternalID)
	if err != nil || old == nil || (old.CreatedAt != nil && !old.CreatedAt.Before(started)) {
		return
	}
	if err := h.cloud.DeleteAndArchive(ctx, old, time.Now().UTC()); err != nil {
		slog.Error("pve create: archive stale row", "externalId", cr.ExternalID, "err", err)
	}
}

// pveResolveTemplate maps imageId → a clone source: an admin catalog slug, or a
// project-owned template (numeric vmid whose cached IMAGE row proves ownership).
func (h *Handler) pveResolveTemplate(ctx context.Context, es *externalservice.ExternalService, proj *Project, imageID string) (*externalservice.PVETemplate, *httpx.HTTPError) {
	if imageID == "" {
		return nil, httpx.BadRequest("imageId is required")
	}
	for _, t := range h.pveImageCatalog(ctx, es) {
		if t.Slug == imageID {
			tpl := t
			return &tpl, nil
		}
	}
	// Project template: the cached IMAGE row is the ownership proof.
	if vmid, err := strconv.Atoi(imageID); err == nil {
		cr, err := h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, imageID)
		if err == nil && cr != nil && cr.Type == cloud.TypeImage && cr.ProjectID == proj.ID {
			img, _ := cr.Data["image"].(map[string]any)
			node, minDisk := strAny(img["node"]), intAny(img["min_disk"])
			if node == "" {
				// The sync stamps availabilityZone-less images; fall back to a live pool scan.
				if ps, err := h.pveFor(es); err == nil {
					if vm, rerr := ps.ResolveVM(ctx, proj.ID, imageID); rerr == nil && vm != nil {
						node = vm.Node
						minDisk = max(minDisk, int(vm.MaxDisk>>30))
					}
				}
			}
			if node != "" {
				return &externalservice.PVETemplate{Slug: imageID, Name: strAny(img["name"]), VMID: vmid, Node: node, MinDiskGb: minDisk}, nil
			}
		}
	}
	return nil, httpx.BadRequest("Unknown image")
}

// pvePlan is a create's placement input for pve.PickNode.
type pvePlan struct {
	allowed map[string]bool                 // nil = any node
	storage map[string]pve.StorageNodeState // nil = no profile, no free-space filter
}

// pvePlacement derives where a server may be created. The profile (when configured) restricts
// placement to the nodes where its storage is active and feeds the free-space filter. PVE clones
// to another node (target=<node>) only when every template volume is on shared storage AND the
// clone's target storage is shared too ("can't clone to non-shared storage"; with no profile the
// clone lands on the template's own, shared, storages). Any other node gets nothing, so a local
// template or a local profile pins the clone to the template's node, and a profile not active
// there fails with 409 instead of a clone-time 502.
func pvePlacement(tpl *externalservice.PVETemplate, tplShared bool, profileSlug string, profiles map[string]externalservice.PVEStorageProfile, caps map[string]pve.StorageCapability) (pvePlan, *httpx.HTTPError) {
	var pl pvePlan
	profile, hasProfile := profiles[profileSlug]
	if hasProfile {
		if !profile.BootDisk() {
			return pl, httpx.NewError(http.StatusConflict, http.StatusConflict, pveNoBootDiskMsg(profile)+"; choose a flavor on another storage profile")
		}
		pl.storage = profile.Placement(caps)
		if len(pl.storage) == 0 {
			return pl, httpx.NewError(http.StatusConflict, http.StatusConflict, fmt.Sprintf("Storage profile %q has no active storage on any node", profileSlug))
		}
	}
	if !hasProfile {
		if !tplShared {
			pl.allowed = map[string]bool{tpl.Node: true}
			for node := range tpl.Copies {
				pl.allowed[node] = true
			}
		}
		return pl, nil
	}
	pl.allowed = map[string]bool{}
	for node := range pl.storage {
		if tpl.OnNode(node) != 0 || (tplShared && caps[profile.StorageFor(node)].Shared) {
			pl.allowed[node] = true
		}
	}
	if len(pl.allowed) == 0 {
		return pl, httpx.NewError(http.StatusConflict, http.StatusConflict, fmt.Sprintf(
			"Image %q can only be cloned on node %s (Proxmox clones to another node only from shared to shared storage), where storage profile %q is not available; the operator must enable the profile on %s or use shared storage",
			tpl.Name, tpl.Node, profileSlug, tpl.Node))
	}
	return pl, nil
}

// pveNoBootDiskMsg explains why a profile refuses boot disks (plan S18).
func pveNoBootDiskMsg(p externalservice.PVEStorageProfile) string {
	if p.Capacity() {
		return fmt.Sprintf("Storage profile %q (%s) is HDD capacity storage and cannot hold a boot disk", p.Slug, p.DisplayName())
	}
	return fmt.Sprintf("Storage profile %q (%s) does not allow boot disks", p.Slug, p.DisplayName())
}

// pveCapacityDisk returns the first attached disk (not unusedN, not a CD-ROM) of a VM config whose
// storage is capacity class, or nil. Such a VM cannot be snapshotted into an image (plan S18:
// no snapshots on capacity; protection there is vzdump/PBS).
func pveCapacityDisk(cfg pve.VMConfig, classOf func(storage string) string) *pve.VMDisk {
	disks := pve.ParseDisks(cfg)
	sort.Slice(disks, func(i, j int) bool { return disks[i].Slot < disks[j].Slot })
	for i, d := range disks {
		if d.CDROM || strings.HasPrefix(d.Slot, "unused") || d.Storage == "" {
			continue
		}
		if classOf(d.Storage) == pve.ClassCapacity {
			return &disks[i]
		}
	}
	return nil
}

// pveResolveBridge maps the wizard's first network interface (the row's external id, the vnet;
// the API also takes the cache id) to the project's vnet ("" = not one of its networks).
func (h *Handler) pveResolveBridge(ctx context.Context, es *externalservice.ExternalService, proj *Project, data map[string]any) string {
	var netID string
	if ifs, ok := data["networkInterfaces"].([]any); ok && len(ifs) > 0 {
		if m, ok := ifs[0].(map[string]any); ok {
			netID = strAny(m["uuid"])
		}
	}
	if netID == "" {
		if ids, ok := data["networkIds"].([]any); ok && len(ids) > 0 {
			netID = strAny(ids[0])
		}
	}
	if netID == "" {
		return ""
	}
	cr, err := h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, netID)
	if err != nil || cr == nil {
		cr, err = h.cloud.FindByID(ctx, netID)
	}
	if err != nil || cr == nil || cr.ProjectID != proj.ID || cr.Type != cloud.TypeNetwork {
		return ""
	}
	if net, ok := cr.Data["network"].(map[string]any); ok {
		return strAny(net["vnet"])
	}
	return ""
}

// pveWantedSubnet is the subnet the wizard picked for the first NIC ("" = the network's first one).
func pveWantedSubnet(data map[string]any) string {
	if ifs, ok := data["networkInterfaces"].([]any); ok && len(ifs) > 0 {
		if m, ok := ifs[0].(map[string]any); ok {
			return strAny(m["subnet"])
		}
	}
	return ""
}

// pveKeypairPublicKey resolves a user's stored keypair public key by name.
func (h *Handler) pveKeypairPublicKey(ctx context.Context, svcID string, u *user.User, name string) string {
	uid := u.ID
	if uid == "" {
		uid = u.Sub
	}
	cr, err := h.cloud.FindByServiceIDAndExternalID(ctx, svcID, name+"_"+uid)
	if err != nil || cr == nil || cr.Type != cloud.TypeKeypair {
		return ""
	}
	if kp, ok := cr.Data["keypair"].(map[string]any); ok {
		return strAny(kp["public_key"])
	}
	return ""
}

// pveCreateKeypair is platform-only: store (or generate) an SSH keypair as a
// user-scoped cache row. A generated private key is returned ONCE via
// ephemeralData (never persisted).
func (h *Handler) pveCreateKeypair(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, data map[string]any) {
	name := strAny(data["name"])
	if name == "" {
		h.fail(w, httpx.BadRequest("name is required"))
		return
	}
	uid := u.ID
	if uid == "" {
		uid = u.Sub
	}
	pubKey := strings.TrimSpace(strAny(data["publicKey"]))
	var privPEM string
	if pubKey == "" {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			h.fail(w, err)
			return
		}
		sshPub, err := ssh.NewPublicKey(pub)
		if err != nil {
			h.fail(w, err)
			return
		}
		pubKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
		block, err := ssh.MarshalPrivateKey(priv, "")
		if err != nil {
			h.fail(w, err)
			return
		}
		privPEM = string(pem.EncodeToMemory(block))
	} else if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pubKey)); err != nil {
		h.fail(w, httpx.BadRequest("publicKey is not a valid SSH public key"))
		return
	}
	now := time.Now().UTC()
	cr := &cloud.CloudResource{
		ServiceID: es.ID, Region: h.pveRegion(proj, es), Type: cloud.TypeKeypair,
		UserID: uid, ExternalID: name + "_" + uid,
		Data: map[string]any{"keypair": map[string]any{
			"name": name, "public_key": pubKey, "fingerprint": sshFingerprint(pubKey),
		}},
		CreatedAt: &now, UpdatedAt: &now,
	}
	saved, err := h.cloud.Insert(r.Context(), cr)
	if err != nil {
		h.fail(w, err)
		return
	}
	if privPEM != "" {
		saved.EphemeralData = map[string]any{"privateKey": privPEM}
	}
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_CREATE", "", saved)
	httpx.OK(w, *saved)
}

// --- delete ---

// pveDeleteResource deletes one cached resource on a proxmox provider.
func (h *Handler) pveDeleteResource(ctx context.Context, es *externalservice.ExternalService, proj *Project, cr *cloud.CloudResource, deleteVolumes bool) error {
	ctx = context.WithoutCancel(ctx) // see pveCreate
	switch cr.Type {
	case cloud.TypeServer:
		if h.pveFor == nil {
			return fmt.Errorf("proxmox service not configured")
		}
		ps, err := h.pveFor(es)
		if err != nil {
			return err
		}
		// A backup goes with its server (pve/backup.go). If one cannot, the server stays and can be retried.
		if store := es.PVEBackupStorage(); store != "" {
			if err := ps.DeleteServerBackups(ctx, proj.ID, cr.ExternalID, store); err != nil {
				return fmt.Errorf("delete the server's backups: %w", err)
			}
		}
		// Volumes outlive the server unless the caller says otherwise. If moving one fails the server stays,
		// and its data with it.
		if !deleteVolumes {
			// A replicated disk cannot leave its VM, and neither may HA restart the VM meanwhile.
			if err := ps.DisableProtection(ctx, proj.ID, cr.ExternalID); err != nil {
				return fmt.Errorf("disable protection: %w", err)
			}
			if err := ps.KeepServerVolumes(ctx, proj.ID, cr.ExternalID); err != nil {
				return fmt.Errorf("keep the server's volumes: %w", err)
			}
		}
		if err := ps.DeleteServer(ctx, proj.ID, cr.ExternalID); err != nil {
			return err
		}
		// The VM and its public NIC are gone: its addresses go back to the pool. A
		// failure only leaks the address (teardown frees it), so it never fails the
		// delete of a VM that is already destroyed.
		if h.publicIPs != nil {
			if err := h.publicIPs.ReleaseServer(ctx, es.ID, cr.ExternalID); err != nil {
				slog.Error("pve delete: release public IPs", "project", proj.ID, "server", cr.ExternalID, "err", err)
			}
		}
		h.pveReleaseSeed(ctx, ps, es.ID, cr.ExternalID)
		h.pveArchiveServerVolumes(ctx, es, proj, cr.ExternalID)
		h.pveSyncVolumes(ctx, es, proj) // kept volumes have new ids on the holder
		return nil
	case cloud.TypeImage:
		// A project template is a qemu template VM in the pool — same destroy path.
		if h.pveFor == nil {
			return fmt.Errorf("proxmox service not configured")
		}
		ps, err := h.pveFor(es)
		if err != nil {
			return err
		}
		return ps.DeleteServer(ctx, proj.ID, cr.ExternalID)
	case cloud.TypeFloatingIP:
		// The row is derived from the allocation: deleting it detaches the NIC behind it. A VM
		// already gone just gives the address back.
		ps, err := h.pveFor(es)
		if err != nil {
			return err
		}
		if h.publicIPs == nil {
			return fmt.Errorf("public IP service not configured")
		}
		fip, _ := cr.Data["floatingIp"].(map[string]any)
		serverID := strAny(fip["server_id"])
		vm, err := ps.ResolveVM(ctx, proj.ID, serverID)
		if err != nil {
			return err
		}
		if vm == nil {
			return h.publicIPs.ReleaseServer(ctx, es.ID, serverID)
		}
		if err := h.pveDetachPublicNIC(ctx, ps, es, proj, vm, serverID); err != nil {
			return err
		}
		h.pveReseed(ctx, ps, es, proj.ID, serverID, vm.Node)
		return nil
	case cloud.TypeKeypair:
		return nil // platform-only row; the archive below is the whole delete
	case cloud.TypeNetwork:
		return errPVENetworkDelete // routed through pveDeleteNetwork (needs the lock + guard)
	case cloud.TypeSecurityGroup:
		ps, err := h.pveFor(es)
		if err != nil {
			return err
		}
		if sg, _ := cr.Data["securityGroup"].(map[string]any); strAny(sg["name"]) == pve.DefaultSecurityGroupName {
			return httpx.NewError(http.StatusConflict, http.StatusConflict, "The default security group cannot be deleted")
		}
		inUse, err := ps.SecurityGroupInUse(ctx, proj.ID, cr.ExternalID)
		if err != nil {
			return err
		}
		if inUse != "" {
			return httpx.NewError(http.StatusConflict, http.StatusConflict, "Security group is in use by server "+inUse)
		}
		return ps.DeleteSecurityGroup(ctx, proj.ID, cr.ExternalID)
	case cloud.TypeVolume:
		// Only a detached disk (unusedN) is deleted on its own; an attached one is 409.
		ps, err := h.pveFor(es)
		if err != nil {
			return err
		}
		vol, _ := cr.Data["volume"].(map[string]any)
		if err := ps.DeleteVolume(ctx, proj.ID, intAny(vol["vmid"]), cr.ExternalID); err != nil {
			return pveVolumeErr(err)
		}
		return nil
	default:
		return fmt.Errorf("resource type %q is not supported on a Proxmox provider", cr.Type)
	}
}

// pveArchiveServerVolumes drops the VOLUME rows of a destroyed server: its disks either went with it or moved to a
// holder under a new volid, and billing of the old rows must stop now rather than at the next sync.
func (h *Handler) pveArchiveServerVolumes(ctx context.Context, es *externalservice.ExternalService, proj *Project, serverExtID string) {
	rows, err := h.cloud.FindByServiceProjectAndType(ctx, es.ID, proj.ID, cloud.TypeVolume)
	if err != nil {
		slog.Error("pve delete: list volumes", "project", proj.ID, "server", serverExtID, "err", err)
		return
	}
	now := time.Now().UTC()
	for i := range rows {
		vol, _ := rows[i].Data["volume"].(map[string]any)
		if strconv.Itoa(intAny(vol["vmid"])) != serverExtID {
			continue
		}
		if err := h.cloud.DeleteAndArchive(ctx, &rows[i], now); err != nil {
			slog.Error("pve delete: archive volume", "project", proj.ID, "volume", rows[i].ExternalID, "err", err)
		}
	}
}

// --- actions ---

// pveAction handles POST /cloud/{id}/action for proxmox resources. Returns true
// when the action was handled (the caller falls through to 400 otherwise).
func (h *Handler) pveAction(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, cr *cloud.CloudResource, action string, data map[string]any) bool {
	r = r.WithContext(context.WithoutCancel(r.Context())) // see pveCreate
	ctx := r.Context()
	if cr.Type == cloud.TypeServer && h.pveProtectionAction(w, r, u, proj, es, cr, action) {
		return true
	}
	if action == "INVOKEACTION" {
		action = strings.ToUpper(strAny(data["action"]))
	}
	ps, err := h.pveFor(es)
	if err != nil || ps == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "proxmox client not ready")
		return true
	}
	if h.pvePublicNetworkAction(w, r, u, proj, es, cr, action, data) {
		return true
	}
	if h.pveVolumeAction(w, r, u, proj, es, cr, action, data) {
		return true
	}
	if h.pveSecurityGroupAction(w, r, u, proj, es, cr, action, data) {
		return true
	}
	if h.pveSubnetAction(w, r, u, proj, es, cr, action, data) {
		return true
	}
	if h.pveSnapshotAction(w, r, u, proj, es, cr, action, data) {
		return true
	}
	if h.pveBackupAction(w, r, u, proj, es, cr, action, data) {
		return true
	}
	switch action {
	case "START", "STOP", "SHUTOFF", "SHUTDOWN", "REBOOT", "SOFTREBOOT", "REBOOT_SOFT", "HARDREBOOT", "REBOOT_HARD":
		if err := ps.PowerAction(ctx, proj.ID, cr.ExternalID, action); err != nil {
			h.fail(w, pveVolumeErr(err))
			return true
		}
		h.pveRefreshServerRow(ctx, ps, proj, cr)
		h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
		httpx.OK(w, map[string]any{"result": *cr})
		return true

	case "RESIZE":
		fl, ferr := h.pveFlavorOf(ctx, strAny(data["flavorId"]))
		if ferr != nil {
			h.fail(w, ferr)
			return true
		}
		if fl.GPUCount > 0 {
			if err := h.enforcePVEGPUQuota(ctx, proj, es, fl, cr); err != nil {
				h.fail(w, err)
				return true
			}
		}
		if err := h.enforcePVEQuota(ctx, proj, es, pveResizeDemand(cr, fl)); err != nil {
			h.fail(w, err)
			return true
		}
		if err := ps.ResizeServer(ctx, proj.ID, cr.ExternalID, pve.ServerSpec{
			Slug: fl.Slug, VCPUs: fl.VCPUs, RAMMb: fl.RAMMb, DiskGb: fl.DiskGb,
			StorageProfile: fl.StorageProfile, GPUModel: fl.GPUModel, GPUCount: fl.GPUCount,
		}); err != nil {
			h.fail(w, pveVolumeErr(err))
			return true
		}
		h.pveRefreshServerRow(ctx, ps, proj, cr)
		h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
		httpx.OK(w, map[string]any{"result": *cr})
		return true

	case "SET_PASSWORD":
		password := strAny(data["password"])
		if password == "" {
			h.fail(w, httpx.BadRequest("password is required"))
			return true
		}
		username := strAny(data["username"])
		if username == "" {
			username = pve.DefaultUserOf(pve.ServerImageSlug(cr.Data))
		}
		if err := ps.SetPassword(ctx, proj.ID, cr.ExternalID, username, password); err != nil {
			h.fail(w, pveVolumeErr(err))
			return true
		}
		// A later rebuild of a seeded server sets the password from its seed: only the user the seed holds it for.
		if h.seeds != nil {
			if seed, err := h.seeds.ForServer(ctx, es.ID, proj.ID, cr.ExternalID); err == nil && seed != nil && seed.PasswordHash != "" &&
				username == cmp.Or(seed.LoginUser, pve.DefaultUserOf(pve.ServerImageSlug(cr.Data))) {
				if b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost); err == nil {
					seed.PasswordHash = string(b)
					if err := h.seeds.put(ctx, seed); err != nil {
						slog.Error("seed password", "server", cr.ExternalID, "err", err)
					}
				}
			}
		}
		h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
		httpx.OK(w, map[string]any{"result": map[string]any{"username": username}})
		return true

	case "REBUILD":
		tpl, herr := h.pveResolveTemplate(ctx, es, proj, strAny(data["imageId"]))
		if herr != nil {
			h.fail(w, herr)
			return true
		}
		vm, verr := ps.ResolveVM(ctx, proj.ID, cr.ExternalID)
		if verr != nil || vm == nil {
			h.fail(w, httpx.NotFound("Server not found"))
			return true
		}
		if tpl.MinDiskGb > 0 && int64(tpl.MinDiskGb) > vm.MaxDisk>>30 {
			h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, fmt.Sprintf("Image %q needs a disk of at least %d GB", tpl.Name, tpl.MinDiskGb)))
			return true
		}
		srcNode, srcID := tpl.Node, tpl.VMID
		if id := tpl.OnNode(vm.Node); id != 0 {
			srcNode, srcID = vm.Node, id
		}
		req := pve.RebuildReq{ProjectID: proj.ID, ServerID: cr.ExternalID, TemplateVMID: srcID, TemplateNode: srcNode, ImageSlug: tpl.Slug}
		// Delivered again with a new instance-id: the new disk's cloud-init runs everything once.
		if h.seeds != nil {
			if seed, err := h.seeds.ForServer(ctx, es.ID, proj.ID, cr.ExternalID); err == nil && seed != nil {
				req.Reseed = func(ctx context.Context, node string, _ int) error {
					iid, err := randomHex(8)
					if err != nil {
						return err
					}
					seed.InstanceID, seed.Node = "iid-"+iid, node
					return h.pveWriteSeed(ctx, ps, es, seed, "")
				}
			}
		}
		if err := ps.RebuildServer(ctx, req); err != nil {
			h.fail(w, pveVolumeErr(err))
			return true
		}
		h.pveRefreshServerRow(ctx, ps, proj, cr)
		h.pveSyncVolumes(ctx, es, proj)
		h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
		httpx.OK(w, map[string]any{"result": *cr})
		return true

	case "RENAME":
		name := strAny(data["name"])
		if herr := vmNameErr(name); herr != nil {
			h.fail(w, herr)
			return true
		}
		if err := ps.RenameServer(ctx, proj.ID, cr.ExternalID, name); err != nil {
			h.fail(w, pveVolumeErr(err))
			return true
		}
		if srv, ok := cr.Data["server"].(map[string]any); ok {
			srv["name"] = name
		}
		if saved, err := h.cloud.Insert(ctx, cr); err == nil {
			cr = saved
		}
		h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
		httpx.OK(w, map[string]any{"result": *cr})
		return true

	case "REMOTECONTROL", "SERIAL_CONSOLE":
		backendURL, ticketUser, ticket, cerr := ps.Console(ctx, proj.ID, cr.ExternalID, action == "SERIAL_CONSOLE")
		if cerr != nil {
			h.fail(w, pveVolumeErr(cerr))
			return true
		}
		kind := "novnc"
		if action == "SERIAL_CONSOLE" {
			kind = "serial"
		}
		// The PVE VNC ticket doubles as the RFB password — the FE passes it as the
		// noVNC credential (`password` in the result).
		h.emitPVEConsole(w, r, u, proj, cr, es.ID, cr.ExternalID, action, backendURL, kind, ticketUser, ticket)
		return true

	case "SHOW_CONSOLE_OUTPUT":
		// PVE has no console-log API; empty output is the honest graceful answer.
		httpx.OK(w, map[string]any{"result": ""})
		return true

	case "LIST_EVENTS":
		httpx.OK(w, map[string]any{"result": h.pveServerEvents(ctx, es.ID, proj.ID, cr)})
		return true
	}
	return false
}

// pveServerEvents renders a server's recorded Proxmox tasks in the events-tab shape. Only tasks since
// the row was created: vmids get reused, and the older tasks are another VM's, maybe another tenant's.
func (h *Handler) pveServerEvents(ctx context.Context, serviceID, projectID string, cr *cloud.CloudResource) []map[string]any {
	out := []map[string]any{}
	var since time.Time
	if cr.CreatedAt != nil {
		since = *cr.CreatedAt
	}
	events, err := h.cloud.TaskEventsForVM(ctx, serviceID, projectID, cr.ExternalID, since, 200)
	if err != nil {
		slog.Error("pve list events", "server", cr.ExternalID, "err", err)
		return out
	}
	for _, e := range events {
		out = append(out, map[string]any{
			"date": e.StartedAt, "action": e.Type, "message": e.Status, "requestId": e.ID, "userId": e.User,
		})
	}
	return out
}

// pveRefreshServerRow re-reads live VM state into the cached row (best-effort).
func (h *Handler) pveRefreshServerRow(ctx context.Context, ps *pve.Service, proj *Project, cr *cloud.CloudResource) {
	vm, err := ps.ResolveVM(ctx, proj.ID, cr.ExternalID)
	if err != nil || vm == nil || vm.NodeDown { // a down node's row has nothing to refresh from
		return
	}
	cr.Data = pve.ServerData(*vm, ps.ServerAddresses(ctx, *vm), nil)
	ps.StampProtection(ctx, *vm, cr.Data)
	ps.StampSnapshots(ctx, *vm, cr.Data)
	if saved, err := h.cloud.Insert(ctx, cr); err == nil {
		*cr = *saved
	}
}

func (h *Handler) pveFlavorOf(ctx context.Context, flavorID string) (*pveflavor.Flavor, *httpx.HTTPError) {
	if h.pveFlavors == nil {
		return nil, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "flavor catalog not configured")
	}
	fl, err := h.pveFlavors.Get(ctx, flavorID)
	if err != nil || fl == nil || !fl.Active {
		return nil, httpx.BadRequest("Unknown flavor")
	}
	return fl, nil
}

// pveResizeDemand is what a resize to fl adds: the vcpus/ram flavor delta, plus
// the boot-disk growth (ResizeServer grows the boot disk to fl.DiskGb; the cached
// flavor.disk is the live boot-disk size). Shrinks count as 0 — the gate skips them.
func pveResizeDemand(cr *cloud.CloudResource, fl *pveflavor.Flavor) pveQuotaDemand {
	var oldVCPUs, oldRAM, oldDisk int
	if srv, ok := cr.Data["server"].(map[string]any); ok {
		if flv, ok := srv["flavor"].(map[string]any); ok {
			oldVCPUs, oldRAM, oldDisk = intAny(flv["vcpus"]), intAny(flv["ram"]), intAny(flv["disk"])
		}
	}
	return pveQuotaDemand{VCPUs: fl.VCPUs - oldVCPUs, RAMMb: fl.RAMMb - oldRAM, DiskGb: fl.DiskGb - oldDisk}
}

// enforcePVEGPUQuota gates GPU flavors against the project's quota.gpu (pveQuotaLimits:
// project quota, else the provider default; catalog flavors carry the GPU demand
// directly). replaced (resize) frees its own devices first. No limit for the model =
// unlimited; a usage read error fails closed.
func (h *Handler) enforcePVEGPUQuota(ctx context.Context, proj *Project, es *externalservice.ExternalService, fl *pveflavor.Flavor, replaced *cloud.CloudResource) *httpx.HTTPError {
	model := cloud.NormalizeGPUAlias(fl.GPUModel)
	limit, limited := gpuLimitFor(pveQuotaLimits(proj, es), model)
	if !limited {
		return nil
	}
	usage, err := h.cloud.GPUUsageByProject(ctx, proj.ID)
	if err != nil {
		return errQuotaUsageUnavailable()
	}
	used := usage[model]
	if replaced != nil {
		if srv, ok := replaced.Data["server"].(map[string]any); ok {
			if flv, ok := srv["flavor"].(map[string]any); ok {
				if m, n := cloud.GPUFromFlavor(flv["extra_specs"]); n > 0 && cloud.NormalizeGPUAlias(m) == model {
					used -= n
					if used < 0 {
						used = 0
					}
				}
			}
		}
	}
	if used+fl.GPUCount > limit {
		return httpx.NewError(http.StatusConflict, http.StatusConflict,
			fmt.Sprintf("GPU quota exceeded for %s: %d in use, %d requested, limit %d", model, used, fl.GPUCount, limit))
	}
	return nil
}

// --- bulk (catalog) actions ---

// pveBulkAction serves the wizard catalog feeds for a proxmox provider. Returns
// true when handled.
func (h *Handler) pveBulkAction(w http.ResponseWriter, r *http.Request, proj *Project, es *externalservice.ExternalService, action string) bool {
	ctx := r.Context()
	region := h.pveRegion(proj, es)
	switch action {
	case "LIST_FLAVORS":
		var result []map[string]any = []map[string]any{}
		if h.pveFlavors != nil {
			if fls, err := h.pveFlavors.List(ctx, true); err == nil {
				for i := range fls {
					result = append(result, fls[i].LiveShape(es.ID, region))
				}
			}
		}
		httpx.OK(w, map[string]any{"result": result})
		return true

	case "PUBLIC_IMAGES", "PUBLIC_IMAGES_AS_CLOUD_RESOURCE":
		asResource := action == "PUBLIC_IMAGES_AS_CLOUD_RESOURCE"
		out := []any{}
		for _, t := range h.pveImageCatalog(ctx, es) {
			img := map[string]any{
				"id": t.Slug, "name": t.Name, "status": "active", "visibility": "public",
				"os_distro": t.OSDistro, "min_disk": t.MinDiskGb, "size": int64(t.MinDiskGb) << 30,
			}
			if asResource {
				out = append(out, cloud.CloudResource{
					Type: cloud.TypeImage, ExternalID: t.Slug, Region: region,
					ServiceID: es.ID, ProjectID: proj.ID, Data: map[string]any{"image": img},
				})
			} else {
				out = append(out, img)
			}
		}
		httpx.OK(w, map[string]any{"result": out})
		return true

	case "LIST_VOLUME_TYPES":
		out := []map[string]any{}
		for slug, prof := range es.PVEStorageProfiles() {
			out = append(out, map[string]any{
				"name":        slug,
				"displayName": prof.DisplayName(),
				"extra_specs": map[string]any{
					"shared": prof.Shared, "snapshots": prof.Snapshots, "thin": prof.Thin,
					"liveMigrate": prof.LiveMigrate, "replication": prof.Replication,
					"tier": prof.Tier, "class": prof.Class, "bootDisk": prof.BootDisk(), "dbaas": prof.DBaaS(),
				},
			})
		}
		httpx.OK(w, map[string]any{"result": out})
		return true
	}
	return false
}

// sshFingerprint renders the SHA256 fingerprint of an authorized_keys line
// (display-only; blank on a parse failure).
func sshFingerprint(pubKey string) string {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pubKey))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(pk)
}

// emitPVEConsole mints the console token for a PVE vncwebsocket/termproxy backend.
// The proxy dials it with the provider's API-token header + pinned TLS; the response carries the VNC ticket as
// `password` — noVNC must present it as the RFB credential.
func (h *Handler) emitPVEConsole(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, cr *cloud.CloudResource, svcID, externalID, action, backendURL, kind, ticketUser, password string) {
	if h.downloads == nil {
		h.fail(w, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "console not configured"))
		return
	}
	tok, err := h.downloads.CreateWithTTL(r.Context(), &cloud.CloudDownload{
		Type: cloud.DownloadTypeConsole, ServiceID: svcID, ProjectID: proj.ID, ExternalID: externalID,
		Metadata: map[string]string{"consoleUrl": backendURL},
	}, cloud.ConsoleTokenTTL)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
	proto := "vnc"
	if kind == "serial" {
		proto = "serial"
	}
	httpx.OK(w, map[string]any{"result": map[string]any{
		"protocol": proto,
		"type":     kind,
		"url":      h.consoleWebsocketURL(tok.ID),
		"user":     ticketUser,
		"password": password,
	}})
}

// errPVENetworkDelete signals cloudDelete to route through pveDeleteNetwork.
var errPVENetworkDelete = fmt.Errorf("pve network delete needs the sdn lock")

// pveCreateVolume adds a data disk to a server, or with no serverId a standalone volume (on a holder VM, see
// pve/holder.go). Body: {serverId?, availabilityZone?, sizeGiB, type?} — serverId is the SERVER cache id or vmid;
// availabilityZone the node for a standalone volume; type a storage-profile slug.
func (h *Handler) pveCreateVolume(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, data map[string]any) {
	ctx := r.Context()
	sizeGiB := intAny(data["sizeGiB"])
	if sizeGiB <= 0 {
		sizeGiB = intAny(data["size"])
	}
	if sizeGiB <= 0 {
		h.fail(w, httpx.BadRequest("sizeGiB is required"))
		return
	}
	serverRef := strAny(data["serverId"])
	// Accept the cache id or the vmid.
	serverExt := serverRef
	if cr, err := h.cloud.FindByID(ctx, serverRef); err == nil && cr != nil && cr.Type == cloud.TypeServer && cr.ProjectID == proj.ID {
		serverExt = cr.ExternalID
	}
	if err := h.enforcePVEQuota(ctx, proj, es, pveQuotaDemand{DiskGb: sizeGiB}); err != nil {
		h.fail(w, err)
		return
	}
	profiles := es.PVEStorageProfiles()
	slug := strAny(data["type"])
	if slug == "" {
		slug = es.PVEDefaultStorageProfile()
	}
	profile, ok := profiles[slug]
	if !ok {
		h.fail(w, httpx.BadRequest("Unknown volume type"))
		return
	}
	ps, ok2 := h.pveService(w, es)
	if !ok2 {
		return
	}
	if serverRef == "" {
		node, storage, err := ps.PlaceVolume(ctx, profile.StorageFor, strAny(data["availabilityZone"]), int64(sizeGiB)<<30, es.PVEStorageHeadroomPct())
		if err == nil {
			err = ps.CreateDetachedVolume(ctx, proj.ID, node, storage, sizeGiB, es.PVEStorageHeadroomPct())
		}
		if err != nil {
			h.fail(w, pveVolumeErr(err))
			return
		}
		h.pveSyncVolumes(ctx, es, proj)
		h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_CREATE", "", &cloud.CloudResource{Type: cloud.TypeVolume, ProjectID: proj.ID})
		httpx.OK(w, map[string]any{"result": map[string]any{"node": node}})
		return
	}
	vm, verr := ps.ResolveVM(ctx, proj.ID, serverExt)
	if verr != nil || vm == nil {
		h.fail(w, httpx.NotFound("Server not found"))
		return
	}
	storage := profile.StorageFor(vm.Node)
	if storage == "" {
		h.fail(w, httpx.BadRequest(fmt.Sprintf("Volume type %q is not available on the server's node", slug)))
		return
	}
	if _, err := ps.AddVolume(ctx, proj.ID, serverExt, sizeGiB, storage, es.PVEStorageHeadroomPct()); err != nil {
		if errors.Is(err, pve.ErrStorageFull) {
			h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, fmt.Sprintf("Not enough free space for a %d GB volume on the server's storage", sizeGiB)))
			return
		}
		h.fail(w, pveVolumeErr(err))
		return
	}
	// The volid is named server-side; the volume sync provider picks the new disk
	// up as its own VOLUME row. Reconcile it now so the FE sees it immediately.
	h.pveSyncVolumes(ctx, es, proj)
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_CREATE", "", &cloud.CloudResource{Type: cloud.TypeVolume, ProjectID: proj.ID})
	httpx.OK(w, map[string]any{"result": map[string]any{}})
}

// pveSyncVolumes runs one inline VOLUME reconcile (best-effort, post-mutation).
func (h *Handler) pveSyncVolumes(ctx context.Context, es *externalservice.ExternalService, proj *Project) {
	ps, err := h.pveFor(es)
	if err != nil {
		return
	}
	prov := ps.VolumeSyncProvider(h.pveRegion(proj, es), proj.ID, es.PVEVolumeProfileFunc())
	if rows, err := prov.ListReadable(ctx); err == nil {
		h.pveUpsertRows(ctx, es, rows)
	}
}

// pveFollowRetypedVolume points the row at the disk's new volid. A failure only costs what the retype
// cost before: the sync finds a new disk, and the bill shows two.
func (h *Handler) pveFollowRetypedVolume(ctx context.Context, ps *pve.Service, es *externalservice.ExternalService, proj *Project, cr *cloud.CloudResource, vmid int, slot string) {
	rows, err := ps.VolumeSyncProvider(h.pveRegion(proj, es), proj.ID, es.PVEVolumeProfileFunc()).ListReadable(ctx)
	if err == nil {
		for i := range rows {
			vol, _ := rows[i].Data["volume"].(map[string]any)
			if intAny(vol["vmid"]) == vmid && strAny(vol["slot"]) == slot {
				err = h.cloud.ChangeExternalID(ctx, cr, rows[i].ExternalID)
				if err == nil {
					return
				}
				break
			}
		}
	}
	slog.Error("retype: keep the volume's row", "project", proj.ID, "volume", cr.ExternalID, "err", err)
}

// computeBackend is what a tenant reads when the compute backend fails.
const computeBackend = "compute backend"

// pveVolumeErr maps a pve server or volume error: a refusal the caller can fix (pve.ConflictError) is
// 409 with its message, a VM locked by a running task too, anything else is PVE failing (502, cause in the log: the raw
// text names the API's address, and a proxied call names the node's).
// vmNameErr refuses a name PVE would refuse, with what to fix: PVE's own 400 would reach the tenant as a backend failure.
func vmNameErr(name string) *httpx.HTTPError {
	if name == "" {
		return httpx.BadRequest("name is required")
	}
	if !pve.ValidVMName(name) {
		return httpx.BadRequest("The name must be a DNS name: letters, digits and hyphens, with dots between parts. Each part starts and ends with a letter or digit")
	}
	return nil
}

func pveVolumeErr(err error) *httpx.HTTPError {
	var ce *pve.ConflictError
	if errors.As(err, &ce) {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, ce.Msg)
	}
	if pve.IsNodeUnreachable(err) {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, "The host of this server does not answer. Try again when it is back")
	}
	if msg := err.Error(); strings.Contains(msg, "can't lock file") || strings.Contains(msg, "VM is locked") {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, "The server is busy with another operation. Try again in a moment")
	}
	if errors.Is(err, pve.ErrStorageFull) {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, "Not enough free space on this storage")
	}
	return upstreamErr(computeBackend, err)
}

// sgRuleErr: the rule's fields come from the tenant and PVE is what validates them, so its 400 names what to fix and
// holds no address.
func sgRuleErr(err error) *httpx.HTTPError {
	var ae *pve.APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest || len(ae.Errors) == 0 {
		return pveVolumeErr(err)
	}
	parts := make([]string, 0, len(ae.Errors))
	for k, v := range ae.Errors {
		parts = append(parts, k+": "+strings.TrimSpace(v))
	}
	sort.Strings(parts)
	return httpx.BadRequest("Invalid firewall rule. " + strings.Join(parts, "; "))
}

// pveVolumeAction handles EXTEND / ATTACH / DETACH / RETYPE / LIST_ATTACH_TARGETS on a
// VOLUME row. Returns handled.
func (h *Handler) pveVolumeAction(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, cr *cloud.CloudResource, action string, data map[string]any) bool {
	if cr.Type != cloud.TypeVolume {
		return false
	}
	ctx := r.Context()
	ps, err := h.pveFor(es)
	if err != nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "proxmox client not ready")
		return true
	}
	vol, _ := cr.Data["volume"].(map[string]any)
	vmid := intAny(vol["vmid"])
	if vmid == 0 {
		h.fail(w, httpx.BadRequest("volume row carries no server"))
		return true
	}
	var aerr error
	renamed := false
	switch action {
	case "EXTEND":
		size := intAny(data["size"])
		if size <= 0 {
			size = intAny(data["sizeGiB"])
		}
		if size <= intAny(vol["size"]) {
			h.fail(w, httpx.BadRequest("New size must be larger (Proxmox cannot shrink a disk)"))
			return true
		}
		if e := h.enforcePVEQuota(ctx, proj, es, pveQuotaDemand{DiskGb: size - intAny(vol["size"])}); e != nil {
			h.fail(w, e)
			return true
		}
		aerr = ps.ExtendVolume(ctx, proj.ID, vmid, cr.ExternalID, size)
	case "DETACH":
		aerr = ps.DetachVolume(ctx, proj.ID, vmid, cr.ExternalID)
	case "ATTACH":
		target := strAny(data["serverId"])
		if target != "" {
			if scr, err := h.cloud.FindByID(ctx, target); err == nil && scr != nil && scr.Type == cloud.TypeServer && scr.ProjectID == proj.ID {
				target = scr.ExternalID
			}
		}
		aerr = ps.AttachVolume(ctx, proj.ID, vmid, cr.ExternalID, target)
		if aerr == nil && target != "" && target != strconv.Itoa(vmid) {
			// Moving a disk to another server renames it: the sync below adds the new row.
			_ = h.cloud.DeleteAndArchive(ctx, cr, time.Now().UTC())
			renamed = true
		}
	case "LIST_ATTACH_TARGETS":
		vms, err := ps.AttachTargets(ctx, proj.ID, vmid, cr.ExternalID)
		if err != nil {
			h.fail(w, pveVolumeErr(err))
			return true
		}
		out := []map[string]any{}
		for _, vm := range vms {
			ext := strconv.Itoa(vm.VMID)
			id := ""
			if scr, _ := h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, ext); scr != nil {
				id = scr.ID
			}
			out = append(out, map[string]any{"id": id, "externalId": ext, "name": vm.Name, "node": vm.Node})
		}
		httpx.OK(w, map[string]any{"result": out})
		return true
	case "RETYPE":
		slug := strAny(data["newType"])
		if slug == "" {
			slug = strAny(data["type"])
		}
		profile, ok := es.PVEStorageProfiles()[slug]
		if !ok {
			h.fail(w, httpx.BadRequest("Unknown volume type"))
			return true
		}
		if vol["bootable"] == true && !profile.BootDisk() {
			h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, pveNoBootDiskMsg(profile)))
			return true
		}
		vm, verr := ps.ResolveVM(ctx, proj.ID, strconv.Itoa(vmid))
		if verr != nil || vm == nil {
			h.fail(w, httpx.NotFound("Server not found"))
			return true
		}
		storage := profile.StorageFor(vm.Node)
		if storage == "" {
			h.fail(w, httpx.BadRequest(fmt.Sprintf("Volume type %q is not available on the volume's node", slug)))
			return true
		}
		slot := strAny(vol["slot"])
		if aerr = ps.RetypeVolume(ctx, proj.ID, vmid, cr.ExternalID, storage, es.PVEStorageHeadroomPct()); aerr == nil {
			// The move gives the disk a new volid. It is still this disk: the row follows it and keeps its
			// id, which is what the bill and its history are keyed on. The sync below fills in the rest.
			h.pveFollowRetypedVolume(ctx, ps, es, proj, cr, vmid, slot)
		}
	default:
		return false
	}
	reconcile := func() error {
		prov := ps.VolumeSyncProvider(h.pveRegion(proj, es), proj.ID, es.PVEVolumeProfileFunc())
		_, err := providers.Reconcile(ctx, prov, h.cloud, es.ID, time.Now().UTC())
		if err != nil {
			slog.Warn("volume action: refresh rows", "project", proj.ID, "err", err)
		}
		return err
	}
	if aerr != nil {
		// A failed cross-node attach can leave the disk on a holder under a new name, so the old row must go too.
		_ = reconcile()
		h.fail(w, pveVolumeErr(aerr))
		return true
	}
	// A sync during a move can have recorded the disk under its name on the carrier, which only a reconcile removes.
	if !renamed || reconcile() != nil {
		h.pveSyncVolumes(ctx, es, proj)
	}
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
	httpx.OK(w, map[string]any{"result": map[string]any{}})
	return true
}

// pveCreateSecurityGroup creates a firewall group (SECURITY_GROUP).
func (h *Handler) pveCreateSecurityGroup(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, data map[string]any) {
	name := strAny(data["name"])
	if name == "" {
		h.fail(w, httpx.BadRequest("name is required"))
		return
	}
	if strings.ContainsAny(name, ":;") {
		h.fail(w, httpx.BadRequest("name must not contain ':' or ';'"))
		return
	}
	ps, ok := h.pveService(w, es)
	if !ok {
		return
	}
	ctx := r.Context()
	id, err := ps.CreateSecurityGroup(ctx, proj.ID, name)
	if err != nil {
		h.fail(w, upstreamErr(computeBackend, err))
		return
	}
	now := time.Now().UTC()
	cr := &cloud.CloudResource{
		ServiceID: es.ID, Region: h.pveRegion(proj, es), ProjectID: proj.ID,
		Type: cloud.TypeSecurityGroup, ExternalID: id,
		Data: map[string]any{"securityGroup": map[string]any{
			"id": id, "name": name, "description": "", "security_group_rules": []any{},
		}},
		CreatedAt: &now, UpdatedAt: &now,
		Info: &cloud.Info{CreatedAt: &now, UpdatedAt: &now},
	}
	saved, ierr := h.cloud.Insert(ctx, cr)
	if ierr != nil {
		saved = cr
	}
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_CREATE", "", saved)
	httpx.OK(w, *saved)
}

// pveCreateImage snapshots a server into a project template (IMAGE).
// Body: {name, serverId} — the wizard's create{type:IMAGE, imageType:snapshot} shape.
func (h *Handler) pveCreateImage(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, data map[string]any) {
	if strAny(data["url"]) != "" {
		h.pveImportImage(w, r, u, proj, es, data)
		return
	}
	ctx := r.Context()
	name := strAny(data["name"])
	serverRef := strAny(data["serverId"])
	if name == "" || serverRef == "" {
		h.fail(w, httpx.BadRequest("name and serverId are required"))
		return
	}
	// An image is a VM too: its name meets the same rule at PVE.
	if herr := vmNameErr(name); herr != nil {
		h.fail(w, herr)
		return
	}
	serverExt := serverRef
	if cr, err := h.cloud.FindByID(ctx, serverRef); err == nil && cr != nil && cr.Type == cloud.TypeServer && cr.ProjectID == proj.ID {
		serverExt = cr.ExternalID
	}
	ps, ok := h.pveService(w, es)
	if !ok {
		return
	}
	vm, verr := ps.ResolveVM(ctx, proj.ID, serverExt)
	if verr != nil || vm == nil {
		h.fail(w, httpx.NotFound("Server not found"))
		return
	}
	cfg, gerr := ps.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	if gerr != nil {
		h.fail(w, upstreamErr(computeBackend, gerr))
		return
	}
	if d := pveCapacityDisk(cfg, es.PVEEffectiveStorageClass); d != nil {
		h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, fmt.Sprintf(
			"Snapshots are not available on HDD capacity storage: volume %s is on storage %s. Detach it first, or protect it with backups",
			d.Slot, d.Storage)))
		return
	}
	started := time.Now().UTC()
	vmid, cerr := ps.CreateImage(ctx, proj.ID, serverExt, name)
	if cerr != nil {
		h.fail(w, pveVolumeErr(cerr))
		return
	}
	// The source VM's disks are what a clone of the image starts from.
	h.pveSaveImageRow(w, r, u, proj, es, name, vmid, vm.MaxDisk, started)
}

// pveSaveImageRow writes the IMAGE row of a new template and answers with it. The sampler would find it within a minute.
func (h *Handler) pveSaveImageRow(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, name string, vmid int, sizeBytes int64, started time.Time) {
	ctx := r.Context()
	now := time.Now().UTC()
	cr := &cloud.CloudResource{
		ServiceID: es.ID, Region: h.pveRegion(proj, es), ProjectID: proj.ID,
		Type: cloud.TypeImage, ExternalID: strconv.Itoa(vmid),
		Data: map[string]any{"image": map[string]any{
			"id": strconv.Itoa(vmid), "name": name, "status": "active", "visibility": "private",
			"size": sizeBytes, "min_disk": (sizeBytes + 1<<30 - 1) >> 30,
		}},
		CreatedAt: &now, UpdatedAt: &now,
		Info: &cloud.Info{CreatedAt: &now, UpdatedAt: &now},
	}
	h.pveArchiveStaleRow(ctx, cr, started)
	saved, ierr := h.cloud.Insert(ctx, cr)
	if ierr != nil {
		saved = cr
	}
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_CREATE", "", saved)
	httpx.OK(w, *saved)
}

// pveSecurityGroupAction handles rule CRUD on a SECURITY_GROUP row and the
// server-side ADD/REMOVE_SECURITY_GROUP attach. Returns handled.
func (h *Handler) pveSecurityGroupAction(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, cr *cloud.CloudResource, action string, data map[string]any) bool {
	ctx := r.Context()
	ps, err := h.pveFor(es)
	if err != nil {
		return false
	}
	if cr.Type == cloud.TypeServer && action == "LIST_SECURITY_GROUPS" {
		ids, err := ps.VMSecurityGroups(ctx, proj.ID, cr.ExternalID)
		if err != nil {
			h.fail(w, upstreamErr(computeBackend, err))
			return true
		}
		rows := []cloud.CloudResource{}
		for _, id := range ids {
			if g, err := h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, id); err == nil && g != nil {
				rows = append(rows, *g)
			}
		}
		httpx.OK(w, map[string]any{"result": rows})
		return true
	}
	// Server-side attach/detach: the group is named in data.
	if cr.Type == cloud.TypeServer && (action == "ADD_SECURITY_GROUP" || action == "REMOVE_SECURITY_GROUP") {
		groupID := strAny(data["securityGroup"])
		if groupID == "" {
			groupID = strAny(data["name"])
		}
		if groupID == "" {
			groupID = strAny(data["securityGroupName"])
		}
		// Accept the cache id too.
		if g, err := h.cloud.FindByID(ctx, groupID); err == nil && g != nil && g.Type == cloud.TypeSecurityGroup && g.ProjectID == proj.ID {
			groupID = g.ExternalID
		}
		// The console names the group by its display name.
		ids, gerr := ps.ServerSecurityGroups(ctx, proj.ID, []string{groupID}, es.PVENetworkConfig().CIDRPool)
		var notFound *pve.SecurityGroupNotFoundError
		if errors.As(gerr, &notFound) {
			h.fail(w, httpx.BadRequest("Security group not found: "+notFound.Ref))
			return true
		}
		if gerr != nil {
			h.fail(w, upstreamErr(computeBackend, gerr))
			return true
		}
		groupID = ids[0]
		var aerr error
		if action == "ADD_SECURITY_GROUP" {
			aerr = ps.AttachSecurityGroup(ctx, proj.ID, cr.ExternalID, groupID)
		} else {
			aerr = ps.DetachSecurityGroup(ctx, proj.ID, cr.ExternalID, groupID)
		}
		if aerr != nil {
			h.fail(w, pveVolumeErr(aerr))
			return true
		}
		h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
		httpx.OK(w, map[string]any{"result": map[string]any{}})
		return true
	}
	if cr.Type != cloud.TypeSecurityGroup {
		return false
	}
	switch action {
	case "LIST_RULES":
		rules := []any{}
		if sg, ok := cr.Data["securityGroup"].(map[string]any); ok {
			if rs, ok := sg["security_group_rules"].([]any); ok {
				rules = rs
			}
		}
		httpx.OK(w, map[string]any{"result": rules})
		return true
	case "ADD_RULE":
		dir := strAny(data["direction"])
		ruleType := "in"
		if dir == "egress" {
			ruleType = "out"
		}
		rule := pve.FirewallRule{
			Type: ruleType, Action: "ACCEPT",
			Proto:   strAny(data["protocol"]),
			Source:  strAny(data["remoteIpPrefix"]),
			DPort:   portRange(data),
			Comment: strAny(data["description"]),
		}
		if err := ps.SGAddRule(ctx, proj.ID, cr.ExternalID, rule); err != nil {
			h.fail(w, sgRuleErr(err))
			return true
		}
		h.pveSyncSecurityGroups(ctx, es, proj)
		h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
		httpx.OK(w, map[string]any{"result": map[string]any{}})
		return true
	case "DELETE_RULE":
		pos := intAny(data["pos"])
		if pos == 0 {
			// The FE names rules "<group>-<pos>" (the sync id shape).
			if id := strAny(data["ruleId"]); id != "" {
				if i := strings.LastIndexByte(id, '-'); i >= 0 {
					pos, _ = strconv.Atoi(id[i+1:])
				}
			}
		}
		if err := ps.SGDeleteRule(ctx, proj.ID, cr.ExternalID, pos); err != nil {
			h.fail(w, pveVolumeErr(err))
			return true
		}
		h.pveSyncSecurityGroups(ctx, es, proj)
		h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
		httpx.OK(w, map[string]any{"result": map[string]any{}})
		return true
	}
	return false
}

// portRange renders the API's portRangeMin/Max into the PVE dport syntax.
func portRange(data map[string]any) string {
	lo, hi := intAny(data["portRangeMin"]), intAny(data["portRangeMax"])
	switch {
	case lo == 0 && hi == 0:
		return ""
	case hi == 0 || lo == hi:
		return strconv.Itoa(lo)
	default:
		return fmt.Sprintf("%d:%d", lo, hi)
	}
}

// pveSyncSecurityGroups reconciles the project's SG rows inline (best-effort).
func (h *Handler) pveSyncSecurityGroups(ctx context.Context, es *externalservice.ExternalService, proj *Project) {
	ps, err := h.pveFor(es)
	if err != nil {
		return
	}
	if rows, err := ps.SecurityGroupSyncProvider(h.pveRegion(proj, es), proj.ID).List(ctx); err == nil {
		h.pveUpsertRows(ctx, es, rows)
	}
}

// pveUpsertRows saves freshly listed rows: a new one is billed from now, an existing one only
// gets its data refreshed.
func (h *Handler) pveUpsertRows(ctx context.Context, es *externalservice.ExternalService, rows []cloud.CloudResource) {
	now := time.Now().UTC()
	for i := range rows {
		rows[i].ServiceID = es.ID
		if existing, _ := h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, rows[i].ExternalID); existing == nil {
			rows[i].CreatedAt, rows[i].UpdatedAt = &now, &now
			_, _ = h.cloud.Insert(ctx, &rows[i])
		} else {
			existing.Data = rows[i].Data
			existing.UpdatedAt = &now
			_, _ = h.cloud.Insert(ctx, existing)
		}
	}
}

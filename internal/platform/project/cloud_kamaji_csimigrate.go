package project

// cloud_kamaji_csimigrate.go moves a cluster made with the tenant-side proxmox-csi controller (csiSharedStores: its
// csi@pve!<id> token sits in the tenant) onto the CSI gateway: adopt the disks its PersistentVolumes name into the
// gateway's registry, switch the controller to the services cluster in one chart bump, and revoke the token.
// docs/managed-k8s.md, "Moving a cluster onto the CSI gateway".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/csigw"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// csiDriver is proxmox-csi-plugin's driver name: the PersistentVolumes it owns.
const csiDriver = "csi.proxmox.sinextra.dev"

// The tenant-side controller's config Secret, which holds the csi@pve!<id> token.
const tenantCSINamespace, tenantCSISecret = "csi-proxmox", "proxmox-csi-plugin"

// CSIMigrationPV is one PersistentVolume of the cluster and what the migration did, or would do, with its disk.
type CSIMigrationPV struct {
	PV        string `json:"pv"`
	VolID     string `json:"volid,omitempty"`
	Node      string `json:"node,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	// Existing: the registry already held the disk for this cluster (a migration run before).
	Existing bool   `json:"existing,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// CSIMigration is the report of MigrateClusterCSI.
type CSIMigration struct {
	ClusterID    string           `json:"clusterId"`
	DryRun       bool             `json:"dryRun"`
	ChartVersion string           `json:"chartVersion"`
	Adopted      []CSIMigrationPV `json:"adopted"`
	Refused      []CSIMigrationPV `json:"refused"`
	// Orphans are -pvc- disks on the cluster's stores that no PersistentVolume of the provider's clusters names, no
	// registry row holds and no VM has attached. Listed for the operator, never deleted.
	Orphans []string `json:"orphans"`
	// UnreadClusters are other clusters whose PersistentVolumes could not be read: any disk could be theirs.
	UnreadClusters []string `json:"unreadClusters,omitempty"`
	Switched       bool     `json:"switched"`
	TenantSecret   bool     `json:"tenantSecretDeleted"`
	TokenRevoked   bool     `json:"tokenRevoked"`
}

// csiPV is a PersistentVolume of proxmox-csi: its name and volumeHandle.
type csiPV struct{ Name, Handle string }

// csiDisk is a -pvc- disk as its store lists it, on the node it was listed on.
type csiDisk struct {
	VolID string
	Node  string
	Size  int64
}

// csiAdoptInput is everything planCSIAdoption decides on, read beforehand.
type csiAdoptInput struct {
	ClusterID string
	Region    string
	Storages  []string
	PVs       []csiPV
	Others    map[string][]csiPV // other clusters' PersistentVolumes, by cluster id
	Unread    []string           // other clusters whose PersistentVolumes could not be read
	Disks     map[string]csiDisk // key: csiKey(storage, name)
	Attached  map[string]int     // key → vmid of a VM that has the disk attached
	Workers   map[int]bool       // the cluster's workers
	Rows      map[string]string  // key → cluster id of a registry row (any state but deleted)
}

func csiKey(storage, name string) string { return storage + "/" + name }

var csiPVCDisk = regexp.MustCompile(`^vm-` + strconv.Itoa(csigw.ControllerVMID) + `-pvc-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}(\.raw|\.qcow2)?$`)

// parseCSIHandle reads a volumeHandle region/zone/storage/disk, where disk may carry a file store's "<vmid>/".
func parseCSIHandle(h string) (region, zone, storage, name string, ok bool) {
	parts := strings.SplitN(h, "/", 4)
	if len(parts) != 4 || parts[0] == "" || parts[2] == "" {
		return "", "", "", "", false
	}
	disk := parts[3]
	if dir, base, isFile := strings.Cut(disk, "/"); isFile {
		if dir != strconv.Itoa(csigw.ControllerVMID) || strings.Contains(base, "/") {
			return "", "", "", "", false
		}
		disk = base
	}
	return parts[0], parts[1], parts[2], disk, true
}

// planCSIAdoption decides, PV by PV, whether the cluster's disk may enter the registry. A disk is adopted when the
// cluster's own PV is the only one naming it, PVE has it, nothing but the cluster's workers has it attached, and no
// registry row holds it for anyone else. Nothing is adopted while another cluster's PVs are unknown: the disk could be
// theirs, named here by a crafted PV, and attached here through the old token. Everything else is refused with its
// reason, for the operator to decide.
func planCSIAdoption(in csiAdoptInput) (adopt, refuse []CSIMigrationPV, orphans []string) {
	claims := map[string][]string{} // key → clusters naming it
	note := func(cluster string, pvs []csiPV) {
		for _, p := range pvs {
			if _, _, st, name, ok := parseCSIHandle(p.Handle); ok {
				k := csiKey(st, name)
				if !slices.Contains(claims[k], cluster) {
					claims[k] = append(claims[k], cluster)
				}
			}
		}
	}
	note(in.ClusterID, in.PVs)
	for id, pvs := range in.Others {
		note(id, pvs)
	}
	seen := map[string]bool{}
	for _, p := range in.PVs {
		r := CSIMigrationPV{PV: p.Name}
		region, _, st, name, ok := parseCSIHandle(p.Handle)
		k := csiKey(st, name)
		d, onPVE := in.Disks[k]
		var others []string
		for _, c := range claims[k] {
			if c != in.ClusterID {
				others = append(others, c)
			}
		}
		vmid, attached := in.Attached[k]
		switch {
		case !ok:
			r.Reason = "volume handle " + p.Handle + " is not region/zone/storage/disk"
		case region != in.Region:
			r.Reason = fmt.Sprintf("region %s is not the cluster's (%s)", region, in.Region)
		case !slices.Contains(in.Storages, st):
			r.Reason = "storage " + st + " is not one of the cluster's"
		case !csiPVCDisk.MatchString(name):
			r.Reason = "disk " + name + " is not a name proxmox-csi gives a volume"
		case seen[k]:
			continue // a second PV of the same cluster on one disk: the first decides
		case in.Rows[k] == in.ClusterID:
			r.VolID, r.Node, r.SizeBytes, r.Existing = d.VolID, d.Node, d.Size, true
		case in.Rows[k] != "":
			r.Reason = "the registry holds the disk for cluster " + in.Rows[k]
		case !onPVE:
			r.Reason = "disk " + st + ":" + name + " is not on PVE"
		case len(others) > 0:
			sort.Strings(others)
			r.Reason = "the disk is also named by a PersistentVolume of " + strings.Join(others, ", ")
		case attached && !in.Workers[vmid]:
			r.Reason = fmt.Sprintf("the disk is attached to VM %d, which is not a worker of this cluster", vmid)
		case len(in.Unread) > 0:
			r.Reason = "the PersistentVolumes of " + strings.Join(in.Unread, ", ") + " could not be read"
		default:
			r.VolID, r.Node, r.SizeBytes = d.VolID, d.Node, d.Size
		}
		seen[k] = true
		if r.Reason != "" {
			r.VolID = st + ":" + name
			refuse = append(refuse, r)
			continue
		}
		adopt = append(adopt, r)
	}
	for k, d := range in.Disks {
		_, held := in.Rows[k]
		_, attached := in.Attached[k]
		if len(claims[k]) == 0 && !held && !attached {
			orphans = append(orphans, d.VolID)
		}
	}
	sort.Strings(orphans)
	return adopt, refuse, orphans
}

// MigrateClusterCSI moves one cluster of the kamaji provider es onto the CSI gateway, or with dryRun only reports what
// it would do. Refused PersistentVolumes stop the migration unless force is set: their pods could not attach afterwards.
// A cluster already on the gateway gets its leftovers taken away again (tenant Secret, token), nothing else.
func (h *Handler) MigrateClusterCSI(ctx context.Context, es *externalservice.ExternalService, ks *kamaji.Service, clusterID string, dryRun, force bool) (*CSIMigration, error) {
	ctx = context.WithoutCancel(ctx)
	cfg := ks.Config()
	rep := &CSIMigration{ClusterID: clusterID, DryRun: dryRun, ChartVersion: cfg.ChartVersion, Adopted: []CSIMigrationPV{},
		Refused: []CSIMigrationPV{}, Orphans: []string{}}
	info, err := ks.ClusterCSI(ctx, clusterID)
	if err != nil {
		return nil, httpx.NotFound(err.Error())
	}
	conflict := func(msg string, a ...any) error {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, fmt.Sprintf(msg, a...))
	}
	switch {
	case h.csiGW == nil || h.csiGW.Endpoint.URL == "":
		return nil, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable,
			"The portal has no CSI gateway URL (PROXCLOUD_CSI_GATEWAY_URL)")
	case len(info.Classes) == 0:
		return nil, conflict("Cluster %s has no proxmox-csi StorageClass: nothing to migrate", clusterID)
	case cfg.Defaults.Shared != nil:
		return nil, conflict("The provider runs sharedWorkers: the CSI gateway needs the project's own workers")
	case !kamaji.ChartHasGatewayCSI(cfg.ChartVersion):
		return nil, conflict("The provider pins cluster chart %s; the CSI gateway needs %s or later", cfg.ChartVersion, kamaji.GatewayChartVersion)
	}
	proj, err := h.svc.GetProjectByID(ctx, info.ProjectID)
	if err != nil {
		return nil, err
	}
	compute, err := h.kamajiCompute(ctx, proj, cfg.Defaults)
	if err != nil {
		return nil, err
	}
	if _, ok := compute.PVECSIGatewayConfig(); !ok {
		return nil, conflict("The Proxmox provider %s has no CSI gateway token", compute.Name)
	}
	if info.Pool != compute.PoolFor(proj.ID) {
		return nil, conflict("Cluster %s has its workers in %s, not the project's pool", clusterID, info.Pool)
	}
	ps, err := h.pveFor(compute)
	if err != nil {
		return nil, err
	}
	kc, err := ks.TenantKubeconfig(ctx, info.ProjectID, clusterID)
	if err != nil {
		return nil, upstreamErr("Kubernetes backend", err)
	}
	tenant, err := kamajik8s.New(string(kc))
	if err != nil {
		return nil, err
	}
	// The disks are listed with the gateway's token: PVE shows a vm-9999-… disk only to a token with Datastore.Allocate
	// on its store (there is no VM 9999 to hold VM.Audit on), which the portal's token does not have.
	gw, err := h.csiGW.GW(ctx, compute.ID)
	if err != nil {
		return nil, upstreamErr(computeBackend, err)
	}
	in, err := h.csiAdoptInput(ctx, ks, ps.C, gw, tenant, info, clusterID, compute.ID)
	if err != nil {
		return nil, err
	}
	adopt, refuse, orphans := planCSIAdoption(*in)
	rep.UnreadClusters = in.Unread
	rep.Refused = append(rep.Refused, refuse...)
	rep.Orphans = append(rep.Orphans, orphans...)
	if dryRun {
		rep.Adopted = append(rep.Adopted, adopt...)
		return rep, nil
	}
	if len(refuse) > 0 && !force {
		rep.Adopted = append(rep.Adopted, adopt...)
		return rep, conflict("%d PersistentVolumes of cluster %s cannot be adopted; resolve them or migrate with force", len(refuse), clusterID)
	}
	now := time.Now().UTC()
	for _, a := range adopt {
		if !a.Existing {
			st, name, _ := strings.Cut(a.VolID, ":")
			name = name[strings.LastIndex(name, "/")+1:]
			v := &csigw.Volume{ID: csigw.VolumeID(compute.ID, st, name), ClusterID: clusterID, ProjectID: proj.ID,
				ComputeServiceID: compute.ID, Storage: st, Node: a.Node, VolID: a.VolID, SizeBytes: a.SizeBytes,
				State: csigw.StateReady, CreatedAt: &now}
			if err := h.csiGW.Reg.InsertVolume(ctx, v); errors.Is(err, csigw.ErrExists) {
				// Taken between the read and now: someone else's unless it is ours.
				if cur, gerr := h.csiGW.Reg.Volume(ctx, v.ID); gerr != nil || cur == nil || cur.ClusterID != clusterID {
					a.Reason = "the registry took the disk for another cluster during the migration"
					rep.Refused = append(rep.Refused, a)
					continue
				}
			} else if err != nil {
				return rep, err
			}
		}
		rep.Adopted = append(rep.Adopted, a)
	}
	if !info.Gateway {
		cred := csigw.Credential{ID: clusterID, ProjectID: proj.ID, ComputeServiceID: compute.ID, Region: info.Region,
			Pool: info.Pool, Storages: classStoragesOf(info.Classes)}
		secret, err := h.csiGW.Reg.ControllerSecret(ctx, cred, h.csiGW.Endpoint)
		if err != nil {
			return rep, err
		}
		if err := ks.SwitchCSIGateway(ctx, proj.ID, clusterID, cfg.ChartVersion, secret, kamajiCSINamespace); err != nil {
			return rep, upstreamErr("Kubernetes backend", err)
		}
	}
	rep.Switched = true
	// The resource set no longer carries the old config, so the Secret it put in the tenant stays until deleted here.
	if err := tenant.DeleteSecret(ctx, tenantCSINamespace, tenantCSISecret); err != nil {
		return rep, upstreamErr("tenant cluster", err)
	}
	rep.TenantSecret = true
	if err := ps.C.RevokeClusterCSIToken(ctx, clusterID); err != nil {
		return rep, upstreamErr(computeBackend, err)
	}
	rep.TokenRevoked = true
	return rep, nil
}

func classStoragesOf(classes []kamaji.StorageClass) []string {
	var out []string
	for _, c := range classes {
		if !slices.Contains(out, c.Storage) {
			out = append(out, c.Storage)
		}
	}
	return out
}

func pvsOf(list []map[string]any) []csiPV {
	var out []csiPV
	for _, pv := range list {
		spec, _ := pv["spec"].(map[string]any)
		c, _ := spec["csi"].(map[string]any)
		if c == nil || c["driver"] != csiDriver {
			continue
		}
		md, _ := pv["metadata"].(map[string]any)
		name, _ := md["name"].(string)
		handle, _ := c["volumeHandle"].(string)
		out = append(out, csiPV{Name: name, Handle: handle})
	}
	return out
}

// csiAdoptInput reads what planCSIAdoption needs: the PersistentVolumes of this cluster and of every other cluster of
// the provider, the -pvc- disks on the cluster's stores (gw: the gateway's client), which VM holds each one and the
// cluster's workers (cl: the portal's, which reads pools), and the registry.
// ponytail: other clusters are read one by one through their admin kubeconfig, and every VM config once; a provider
// with hundreds of clusters wants a cached claim index instead.
func (h *Handler) csiAdoptInput(ctx context.Context, ks *kamaji.Service, cl, gw *pve.Client, tenant *kamajik8s.Client, info *kamaji.ClusterCSI, clusterID, computeID string) (*csiAdoptInput, error) {
	in := &csiAdoptInput{ClusterID: clusterID, Region: info.Region, Storages: classStoragesOf(info.Classes),
		Others: map[string][]csiPV{}, Disks: map[string]csiDisk{}, Attached: map[string]int{}, Workers: map[int]bool{},
		Rows: map[string]string{}}
	list, err := tenant.ListPersistentVolumes(ctx)
	if err != nil {
		return nil, upstreamErr("tenant cluster", err)
	}
	in.PVs = pvsOf(list)
	pins, err := ks.ListClusterPins(ctx)
	if err != nil {
		return nil, upstreamErr("Kubernetes backend", err)
	}
	for _, p := range pins {
		if p.ID == clusterID {
			continue
		}
		pvs, err := func() ([]csiPV, error) {
			kc, err := ks.TenantKubeconfig(ctx, p.ProjectID, p.ID)
			if err != nil {
				return nil, err
			}
			c, err := kamajik8s.New(string(kc))
			if err != nil {
				return nil, err
			}
			l, err := c.ListPersistentVolumes(ctx)
			return pvsOf(l), err
		}()
		if err != nil {
			in.Unread = append(in.Unread, p.ID)
			continue
		}
		in.Others[p.ID] = pvs
	}
	var stores, vms []struct {
		Type, Node, Storage, Status, Pool, Tags string
		VMID                                    int
		Shared, Template                        int
	}
	for typ, dst := range map[string]any{"storage": &stores, "vm": &vms} {
		raw, err := cl.Raw(ctx, http.MethodGet, "/cluster/resources", url.Values{"type": {typ}})
		if err != nil {
			return nil, upstreamErr(computeBackend, err)
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return nil, err
		}
	}
	listed := map[string]bool{}
	for _, s := range stores {
		if !slices.Contains(in.Storages, s.Storage) || s.Status != "available" || (s.Shared == 1 && listed[s.Storage]) {
			continue
		}
		listed[s.Storage] = true
		content, err := gw.StorageContents(ctx, s.Node, s.Storage, "images", 0)
		if err != nil {
			return nil, upstreamErr(computeBackend, err)
		}
		for _, c := range content {
			_, disk, _ := strings.Cut(c.VolID, ":")
			name := disk[strings.LastIndex(disk, "/")+1:]
			if csiPVCDisk.MatchString(name) {
				in.Disks[csiKey(s.Storage, name)] = csiDisk{VolID: c.VolID, Node: s.Node, Size: c.Size}
			}
		}
	}
	tag := "pc-k8s-" + clusterID
	for _, v := range vms {
		if v.Type != "qemu" || v.Template == 1 {
			continue
		}
		if v.Pool == info.Pool && slices.Contains(strings.FieldsFunc(v.Tags, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }), tag) {
			in.Workers[v.VMID] = true
		}
		raw, err := cl.Raw(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/qemu/%d/config", v.Node, v.VMID), nil)
		if err != nil {
			if pve.IsNotFound(err) {
				continue // gone since the listing
			}
			return nil, upstreamErr(computeBackend, err)
		}
		var cfg map[string]any
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		for _, val := range cfg {
			s, _ := val.(string)
			file, _, _ := strings.Cut(s, ",")
			st, disk, ok := strings.Cut(strings.TrimPrefix(file, "file="), ":")
			if !ok {
				continue
			}
			if k := csiKey(st, disk[strings.LastIndex(disk, "/")+1:]); slices.Contains(in.Storages, st) {
				in.Attached[k] = v.VMID
			}
		}
	}
	for _, st := range in.Storages {
		rows, err := h.csiGW.Reg.StorageVolumes(ctx, computeID, st)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			in.Rows[strings.TrimPrefix(r.ID, computeID+"/")] = r.ClusterID // the id is <service>/<storage>/<name>
		}
	}
	return in, nil
}

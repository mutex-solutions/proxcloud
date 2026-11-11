package pve

// capability.go — storage capability detection (S2/S19 API part). Detect reads the cluster
// storage definitions (GET /storage) plus the per-node status rows (GET /cluster/resources
// type=storage) and derives, per storage, what it can honestly do and a default class from a
// static type table. The installer's fio benchmark only ever SUGGESTS a class and an admin
// override always wins; both live in the provider config (externalservice), not here.

import (
	"context"
	"strings"
)

// Storage tiers: Full = Ceph, Standard = local ZFS/LVM + NAS.
const (
	TierFull     = "full"
	TierStandard = "standard"
)

// Storage classes (fio buckets: < 1 ms, 1–5 ms, > 5 ms p99 sync write).
const (
	ClassPerformance = "performance"
	ClassStandard    = "standard"
	ClassCapacity    = "capacity"
)

// ValidClass reports whether c is one of the three storage classes.
func ValidClass(c string) bool {
	return c == ClassPerformance || c == ClassStandard || c == ClassCapacity
}

// StorageCapability is what one storage can do, as detected. Profiles may only claim a subset.
type StorageCapability struct {
	Storage     string   `json:"storage"`
	Type        string   `json:"type"`
	Shared      bool     `json:"shared"`
	Content     []string `json:"content"`
	Snapshots   bool     `json:"snapshots"`
	Thin        bool     `json:"thin"`
	LiveMigrate bool     `json:"liveMigrate"`
	Replication bool     `json:"replication"`
	Tier        string   `json:"tier"`
	Class       string   `json:"class"` // detected default class (type table)
	// Nodes is every node PVE reports the storage on; Active = enabled and available there.
	Nodes map[string]StorageNodeState `json:"nodes"`
}

// StorageNodeState is one node's live view of a storage.
type StorageNodeState struct {
	Active bool  `json:"active"`
	Total  int64 `json:"total"`
	Avail  int64 `json:"avail"`
}

// HasContent reports whether the storage accepts a content type ("images", "iso", …).
func (s StorageCapability) HasContent(ct string) bool {
	for _, c := range s.Content {
		if c == ct {
			return true
		}
	}
	return false
}

// inherentlyShared are the plugin types PVE treats as shared whatever storage.cfg says
// (pve-storage Plugin.pm @SHARED_STORAGE; parse_config sets shared=1 for them, glusterfs
// kept for PVE 8). Inferring it here is a no-op when GET /storage already returns shared=1.
var inherentlyShared = map[string]bool{
	"rbd": true, "cephfs": true, "nfs": true, "cifs": true, "iscsi": true,
	"iscsidirect": true, "zfs": true, "pbs": true, "glusterfs": true, "drbd": true,
}

// classify derives the capability flags, tier and default class of one storage definition
// from the static type table (plan §3 capability matrix / S19 table). Unknown types get
// nothing claimed and class capacity: an admin override is the way up.
func classify(d StorageDef) StorageCapability {
	shared := d.Shared == 1 || inherentlyShared[d.Type]
	c := StorageCapability{
		Storage: d.Storage, Type: d.Type, Shared: shared, Content: splitList(d.Content),
		LiveMigrate: shared, Replication: d.Type == "zfspool",
		Tier: TierStandard, Class: ClassCapacity, Nodes: map[string]StorageNodeState{},
	}
	switch d.Type {
	case "zfspool":
		c.Class, c.Snapshots, c.Thin = ClassPerformance, true, d.Sparse == 1
	case "lvmthin":
		c.Class, c.Snapshots, c.Thin = ClassPerformance, true, true
	case "lvm":
		// Thick LVM: local disk → performance; shared (iSCSI/FC LUN, PVE 9) → standard.
		// PVE 9 volume-chain snapshots on shared LVM are a tech preview: not claimed.
		c.Class = ClassPerformance
		if shared {
			c.Class = ClassStandard
		}
	case "rbd":
		c.Tier, c.Class, c.Snapshots, c.Thin = TierFull, ClassPerformance, true, true
	case "cephfs":
		c.Tier, c.Class, c.Snapshots = TierFull, ClassStandard, true
	case "nfs", "cifs":
		// Snapshots are not offered on file storage by design (qcow2 internal snapshots block the VM).
		c.Class, c.Thin = ClassCapacity, true
	case "dir":
		c.Class, c.Thin = ClassCapacity, true
	case "iscsi", "iscsidirect":
		c.Class = ClassStandard
	case "zfs": // ZFS over iSCSI
		c.Class, c.Snapshots, c.Thin = ClassStandard, true, d.Sparse == 1
	}
	return c
}

// rowState is one GET /cluster/resources?type=storage row as a node state (avail clamped at 0).
// Lab-verified (PVE 9.2): an unmounted storage reports status "unknown"; a disabled one has no
// row at all, so it never reaches this.
func rowState(r ClusterResource) StorageNodeState {
	avail := r.MaxDisk - r.Disk
	if avail < 0 {
		avail = 0
	}
	return StorageNodeState{Active: r.Status == "available", Total: r.MaxDisk, Avail: avail}
}

// StorageHasRoom reports whether a storage can take diskBytes more on a node and still keep
// headroomPct % of its capacity free. An inactive storage never has room. Thin storages are
// checked against the full requested size: the disk may fill up to it.
func StorageHasRoom(st StorageNodeState, diskBytes int64, headroomPct float64) bool {
	return st.Active && float64(st.Avail-diskBytes) >= float64(st.Total)*headroomPct/100
}

// TemplateShared reports whether every volume of a template sits on shared storage, the first
// half of PVE's cross-node clone rule (qemu-server check_storage_access_clone: disks, EFI/TPM
// state and cloud-init / ISO drives count, a physical "cdrom" and unusedN do not). The other
// half, a shared clone target storage, is the caller's (pvePlacement). caps is DetectStorage's
// output; an unknown storage or a raw device path counts as local.
func TemplateShared(cfg VMConfig, caps map[string]StorageCapability) bool {
	for key := range cfg {
		if p, ok := diskSlotPrefix(key); (!ok || p == "unused") && key != "efidisk0" && key != "tpmstate0" {
			continue
		}
		vol, _, _ := strings.Cut(cfg.Str(key), ",")
		if vol == "" || vol == "none" || vol == "cdrom" {
			continue
		}
		sid, _, ok := strings.Cut(vol, ":")
		if !ok || !caps[sid].Shared {
			return false
		}
	}
	return true
}

// DetectStorage reads every cluster storage and returns its detected capabilities keyed by
// storage id. A storage disabled cluster-wide (disable=1) or outside its `nodes` restriction is
// never active on a node.
func (c *Client) DetectStorage(ctx context.Context) (map[string]StorageCapability, error) {
	defs, err := c.ListStorage(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := c.ClusterResources(ctx, "storage")
	if err != nil {
		return nil, err
	}
	out := map[string]StorageCapability{}
	restrict := map[string]map[string]bool{}
	disabled := map[string]bool{}
	for _, d := range defs {
		out[d.Storage] = classify(d)
		disabled[d.Storage] = d.Disable == 1
		if ns := splitList(d.Nodes); len(ns) > 0 {
			restrict[d.Storage] = map[string]bool{}
			for _, n := range ns {
				restrict[d.Storage][n] = true
			}
		}
	}
	for _, r := range rows {
		cp, ok := out[r.Storage]
		if r.Type != "storage" || !ok {
			continue
		}
		if rn := restrict[r.Storage]; rn != nil && !rn[r.Node] {
			continue
		}
		st := rowState(r)
		st.Active = st.Active && !disabled[r.Storage]
		cp.Nodes[r.Node] = st
	}
	return out, nil
}

func splitList(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

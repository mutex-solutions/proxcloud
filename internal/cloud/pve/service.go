package pve

// service.go — the provider-level service over one PVE cluster (dbaas.Service
// precedent): pool bootstrap/teardown + the shared derivations and mappings the
// sync providers and the write path both use. Dependency arrow stays pve→cloud.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Service wraps a Client with the per-provider identity (serviceID) and the
// pool-prefix tenancy derivation.
type Service struct {
	C          *Client
	ServiceID  string
	PoolPrefix string
	// HolderLock serialises holder use per project across API replicas (ShedLock.Wait). Nil = no lock: tools and tests.
	HolderLock func(ctx context.Context, name string, atMostFor, wait time.Duration) (release func(), acquired bool, err error)
}

// NewService builds the Service for one provider doc.
func NewService(cfg Config, serviceID, poolPrefix string) (*Service, error) {
	c, err := New(cfg)
	if err != nil {
		return nil, err
	}
	if poolPrefix == "" {
		poolPrefix = "pc-"
	}
	return &Service{C: c, ServiceID: serviceID, PoolPrefix: poolPrefix}, nil
}

// PoolFor mirrors externalservice.PoolFor — same prefix, same derivation.
func (s *Service) PoolFor(projectID string) string { return s.PoolPrefix + projectID }

// EnsureProjectPool creates the project's pool (idempotent).
func (s *Service) EnsureProjectPool(ctx context.Context, projectID string) (string, error) {
	pool := s.PoolFor(projectID)
	if err := s.C.CreatePool(ctx, pool, "proxcloud project "+projectID); err != nil {
		return "", err
	}
	return pool, nil
}

// TeardownProjectPool destroys every VM still in the project's pool, then the pool.
// Fail-closed: the first VM that refuses to die aborts the teardown so the caller
// reports incomplete deprovisioning (ceph teardown discipline).
func (s *Service) TeardownProjectPool(ctx context.Context, pool string) error {
	info, err := s.C.GetPool(ctx, pool)
	if err != nil {
		return fmt.Errorf("pve teardown: read pool %s: %w", pool, err)
	}
	if info == nil {
		return nil // already gone
	}
	for _, m := range info.Members {
		if m.Type != "qemu" {
			continue
		}
		// Stop hard (a running VM refuses destroy), then delete. Best-effort stop:
		// a stopped VM's stop task fails harmlessly.
		if upid, err := s.C.StopVM(ctx, m.Node, m.VMID); err == nil {
			_ = s.C.WaitTask(ctx, upid)
		}
		upid, err := s.C.DeleteVM(ctx, m.Node, m.VMID)
		if err != nil {
			return fmt.Errorf("pve teardown: delete vm %d: %w", m.VMID, err)
		}
		if err := s.C.WaitTask(ctx, upid); err != nil {
			return fmt.Errorf("pve teardown: delete vm %d: %w", m.VMID, err)
		}
	}
	return s.C.DeletePool(ctx, pool)
}

// --- shared VM mappings (sync + write read-back use the SAME shapes) ---

// Tag vocabulary. PVE tags are lowercase [a-z0-9_-]; semicolon-separated on the wire.
// The flavor and image slugs a VM was created from ride its tags so the sync can
// rebuild the billing-critical flavor block without any platform DB lookup.
const (
	TagManaged     = "proxcloud"
	tagFlavorPref  = "fl-"
	tagImagePref   = "img-"
	tagProfilePref = "sp-" // storage profile slug (Phase 5 billing attr)
	// tagSpreadPref carries a spread group: PVE's own affinity rules are HA-only and strict.
	tagSpreadPref  = "grp-"
	tagCreatedPref = "pc-c-"
)

// JoinTags renders VM tags for create.
func JoinTags(tags ...string) string { return strings.Join(tags, ";") }

// TagValue extracts "<pref><slug>" → slug from a semicolon-separated tag string.
func TagValue(tags, pref string) string {
	for _, t := range strings.Split(tags, ";") {
		if strings.HasPrefix(t, pref) {
			return strings.TrimPrefix(t, pref)
		}
	}
	return ""
}

// SpreadTag is the VM tag of a spread group.
func SpreadTag(group string) string { return tagSpreadPref + group }

// SpreadGroupSlug normalizes a spread group name into a tag-safe slug ("" = not usable).
func SpreadGroupSlug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	if s == "" || len(s) > 40 {
		return ""
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return ""
		}
	}
	return s
}

func FlavorTag(slug string) string  { return tagFlavorPref + slug }
func ImageTag(slug string) string   { return tagImagePref + slug }
func ProfileTag(slug string) string { return tagProfilePref + slug }

// CreatedTag is a VM's identity stamp (pc-c-<unix>), written by the portal at create. Together with
// the vmid it tells two VMs apart: PVE hands out the lowest free vmid again, and without the stamp a
// new VM inherits the old VM's cache row and bill item.
func CreatedTag(unix int64) string { return tagCreatedPref + strconv.FormatInt(unix, 10) }

// Identity values on SERVER and VOLUME data: "c-<unix>" from the stamp, "m-<ctime>" from the `meta`
// config key for a VM with no stamp. ctime is the weaker of the two: a clone keeps its source's
// (verified on PVE 9.2), so clones of one template look alike and only a VM created from scratch
// has its own. "" = unknown.
const (
	identityStamp = "c-"
	identityMeta  = "m-"
)

// taggedIdentity reads the stamp, "" when there is none (or the VM is mid-clone and still carries
// its source's tags).
func taggedIdentity(tags, lock string) string {
	if v := TagValue(tags, tagCreatedPref); v != "" && lock != "clone" && lock != "create" {
		return identityStamp + v
	}
	return ""
}

// metaIdentity reads the ctime PVE keeps in a VM's `meta` config key.
func metaIdentity(cfg VMConfig) string {
	for _, kv := range splitCommaKV(cfg.Str("meta")) {
		if kv[0] == "ctime" {
			return identityMeta + kv[1]
		}
	}
	return ""
}

// MapVMStatus maps PVE state → the API's status vocabulary the FE and
// billing eligibility understand (ACTIVE/SHUTOFF/BUILD/ERROR).
func MapVMStatus(status, lock string) string {
	switch lock {
	case "clone", "create":
		return "BUILD"
	case "migrate":
		return "MIGRATING"
	}
	switch status {
	case "running":
		return "ACTIVE"
	case "stopped":
		return "SHUTOFF"
	default:
		return strings.ToUpper(status)
	}
}

// ServerData builds the canonical data.server payload (the billing + FE contract,
// read by internal/cloud/billingresource/server.go). All values are
// round-trip-stable JSON scalars.
//
// volumeBacked is always true for PVE servers: every disk bills as a VOLUME row
// (root_disk_gb → 0 in the instance billing resource), so disk pricing follows the
// storage profile instead of the flavor.
func ServerData(r ClusterResource, addresses map[string]any, gpuSpecs map[string]any) map[string]any {
	flavorSlug := TagValue(r.Tags, tagFlavorPref)
	imageSlug := TagValue(r.Tags, tagImagePref)
	if addresses == nil {
		addresses = map[string]any{}
	}
	extraSpecs := gpuSpecs
	if extraSpecs == nil {
		extraSpecs = GPUSpecsFromTags(r.Tags)
	}
	if extraSpecs == nil {
		extraSpecs = map[string]any{}
	}
	server := map[string]any{
		"spreadGroup":      TagValue(r.Tags, tagSpreadPref),
		"name":             r.Name,
		"host":             r.Node,
		"status":           MapVMStatus(r.Status, r.Lock),
		"availabilityZone": r.Node,
		"addresses":        addresses,
		"flavor": map[string]any{
			"id":            flavorSlug,
			"name":          flavorSlug,
			"original_name": flavorSlug,
			"vcpus":         int(r.MaxCPU),
			"ram":           r.MaxMem / (1 << 20),
			"disk":          r.MaxDisk / (1 << 30),
			"extra_specs":   extraSpecs,
		},
	}
	if imageSlug != "" {
		server["image"] = map[string]any{"id": imageSlug}
	}
	if lb := LBOfTags(r.Tags); lb != "" {
		server["loadBalancerId"] = lb
	}
	data := map[string]any{
		"flavorName":   flavorSlug,
		"volumeBacked": true,
		"server":       server,
	}
	if id := taggedIdentity(r.Tags, r.Lock); id != "" {
		data[dataIdentity] = id
	}
	return data
}

// dataIdentity is the identity key on SERVER and VOLUME data ("" or absent = unknown).
const dataIdentity = "identity"

// AgentAddresses converts guest-agent NICs into the API's addresses shape:
// {"private": [{"addr": ..., "version": 4}]}. Loopback and link-local are dropped.
//
// ⚠ The return type is map[string]any over []any — NOT map[string][]map[string]any.
// A typed nested collection is not reflect-convertible to map[string]any, so the
// sync's keyed compare falls through to DeepEqual(typed, json-decoded) and reports
// a change on EVERY pass (live-caught: run-sync churned one SERVER row forever).
// Everything a provider caches must be JSON-round-trip-stable: strings, numbers,
// bools, map[string]any, []any — nothing else.
func AgentAddresses(ifaces []AgentInterface) map[string]any {
	var addrs []any
	for _, ni := range ifaces {
		if ni.Name == "lo" {
			continue
		}
		for _, ip := range ni.IPAddresses {
			if strings.HasPrefix(ip.Address, "127.") || strings.HasPrefix(ip.Address, "fe80") || ip.Address == "::1" {
				continue
			}
			ver := 4
			if ip.Type == "ipv6" {
				ver = 6
			}
			addrs = append(addrs, map[string]any{
				"addr":    ip.Address,
				"version": ver,
			})
		}
	}
	if len(addrs) == 0 {
		return map[string]any{}
	}
	return map[string]any{"private": addrs}
}

// VMDisk is one parsed disk entry of a VM config.
type VMDisk struct {
	Slot    string // "scsi0", "virtio1", "unused0", …
	VolID   string // "local-zfs:vm-101-disk-0"
	Storage string // "local-zfs"
	SizeGiB int
	CDROM   bool
	Boot    bool // slot is the boot disk (scsi0 by convention)
}

// diskSlots are the config key prefixes that carry disks.
var diskSlots = []string{"scsi", "virtio", "sata", "ide", "unused"}

// ParseDisks extracts disk entries from a VM config. efidisk/tpmstate are skipped
// (they are boot plumbing, cents-scale storage — not customer volumes).
func ParseDisks(cfg VMConfig) []VMDisk {
	var out []VMDisk
	for key := range cfg {
		pref, ok := diskSlotPrefix(key)
		if !ok {
			continue
		}
		raw := cfg.Str(key)
		if raw == "" {
			continue
		}
		d := VMDisk{Slot: key, Boot: key == "scsi0" || key == "virtio0"}
		for i, part := range strings.Split(raw, ",") {
			if i == 0 {
				d.VolID = part
				if j := strings.IndexByte(part, ':'); j > 0 {
					d.Storage = part[:j]
				}
				continue
			}
			kv := strings.SplitN(part, "=", 2)
			if len(kv) != 2 {
				continue
			}
			switch kv[0] {
			case "size":
				d.SizeGiB = sizeToGiB(kv[1])
			case "media":
				d.CDROM = kv[1] == "cdrom"
			}
		}
		if d.CDROM || d.VolID == "" || d.VolID == "none" {
			continue
		}
		_ = pref
		out = append(out, d)
	}
	return out
}

func diskSlotPrefix(key string) (string, bool) {
	for _, p := range diskSlots {
		if strings.HasPrefix(key, p) && len(key) > len(p) {
			if _, err := strconv.Atoi(key[len(p):]); err == nil {
				return p, true
			}
		}
	}
	return "", false
}

// sizeToGiB parses PVE size syntax ("32G", "8192M", "1T", bare bytes) to whole GiB
// (rounded up — a 1500M disk bills as 2 GiB, never 1).
func sizeToGiB(s string) int {
	if s == "" {
		return 0
	}
	mult := int64(1)
	last := s[len(s)-1]
	num := s
	switch last {
	case 'T':
		mult = 1 << 40
		num = s[:len(s)-1]
	case 'G':
		mult = 1 << 30
		num = s[:len(s)-1]
	case 'M':
		mult = 1 << 20
		num = s[:len(s)-1]
	case 'K':
		mult = 1 << 10
		num = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return 0
	}
	bytes := n * mult
	gib := bytes / (1 << 30)
	if bytes%(1<<30) != 0 {
		gib++
	}
	return int(gib)
}

// nowUTC is the single time source (test seam).
var nowUTC = func() time.Time { return time.Now().UTC() }

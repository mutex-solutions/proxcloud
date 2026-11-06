package pve

// gpu.go — GPU passthrough via cluster PCI resource mappings.
//
// Operators map GPUs in Proxmox (Datacenter → Resource Mappings → PCI) in one of
// two shapes, and BOTH are supported here:
//
//	A) one mapping per model, one map entry per physical card
//	   gpu-p40: [node=a,path=…18:00.0], [node=a,path=…3b:00.0], [node=b,path=…]
//	B) one mapping per card (the common homelab shape)
//	   gpu1: [node=a,path=…18:00.0]   gpu2: [node=a,path=…3b:00.0]   …
//
// The platform maps mapping-id → customer-facing alias in config.gpus:
//
//	"gpus": {"gpu1": {"alias": "nvidia-p40"}, "gpu2": {"alias": "nvidia-p40"}, …}
//
// so several mappings can share one alias. Availability is therefore computed per
// MAPPING, not per alias: a mapping already referenced by any VM's hostpciN is
// taken (PVE itself refuses to start a second VM on the same device). Counting
// live references — instead of platform tags — also sees VMs created outside
// proxcloud, so the scheduler never hands out a card an ops VM already holds.
//
// A created VM still carries a `g-<alias>x<n>` tag: the sync rebuilds
// extra_specs from it, which is what makes GPU billing + the GPU quota gate work
// unchanged (cloud.GPUFromFlavor).

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const tagGPUPref = "g-"

// GPUTag renders the GPU demand tag ("g-nvidia-p40x2").
func GPUTag(alias string, count int) string {
	return tagGPUPref + alias + "x" + strconv.Itoa(count)
}

// gpuFromTags parses the GPU tag back ("" when absent).
func gpuFromTags(tags string) (alias string, count int) {
	v := TagValue(tags, tagGPUPref)
	if v == "" {
		return "", 0
	}
	i := strings.LastIndexByte(v, 'x')
	if i <= 0 {
		return "", 0
	}
	n, err := strconv.Atoi(v[i+1:])
	if err != nil || n <= 0 {
		return "", 0
	}
	return v[:i], n
}

// GPUSpecsFromTags renders the flavor extra_specs shape for a VM's tags —
// what cloud.GPUFromFlavor already parses (billing + quota reuse).
func GPUSpecsFromTags(tags string) map[string]any {
	alias, count := gpuFromTags(tags)
	if alias == "" {
		return nil
	}
	return map[string]any{"pci_passthrough:alias": fmt.Sprintf("%s:%d", alias, count)}
}

// GPUSpecsFromConfig counts a VM's GPUs from its hostpciN mapping= entries, which is what the VM
// actually holds (the tag says what it was created with). aliasOf names a mapping's GPU model; a
// hostpci entry on a mapping it does not know (a NIC, an HBA) is not a GPU. nil when there are none.
func GPUSpecsFromConfig(cfg VMConfig, aliasOf func(mappingID string) string) map[string]any {
	if aliasOf == nil {
		return nil
	}
	counts := map[string]int{}
	for key := range cfg {
		if !strings.HasPrefix(key, "hostpci") {
			continue
		}
		for _, kv := range splitCommaKV(cfg.Str(key)) {
			if kv[0] == "mapping" {
				if alias := aliasOf(kv[1]); alias != "" {
					counts[alias]++
				}
			}
		}
	}
	if len(counts) == 0 {
		return nil
	}
	parts := make([]string, 0, len(counts))
	for alias, n := range counts {
		parts = append(parts, fmt.Sprintf("%s:%d", alias, n))
	}
	sort.Strings(parts) // the cached payload is compared whole: keep the order stable
	return map[string]any{"pci_passthrough:alias": strings.Join(parts, ",")}
}

// GPUSlot is one assignable passthrough device: a mapping id plus the node its
// entry lives on. A mapping holding entries on several nodes yields one slot per
// node (only the local one is usable by a VM placed there).
type GPUSlot struct {
	MappingID string
	Node      string
}

// GPUInventory is the per-alias view the scheduler and the capacity reads share.
type GPUInventory struct {
	Alias string
	All   []GPUSlot // every mapped device carrying this alias
	Free  []GPUSlot // those not referenced by any VM right now
}

// FreeByNode groups the free slots per node.
func (inv GPUInventory) FreeByNode() map[string][]GPUSlot {
	out := map[string][]GPUSlot{}
	for _, s := range inv.Free {
		out[s.Node] = append(out[s.Node], s)
	}
	return out
}

// FreeCountByNode is the scheduler's SchedReq.GPUFree input.
func (inv GPUInventory) FreeCountByNode() map[string]int {
	out := map[string]int{}
	for _, s := range inv.Free {
		out[s.Node]++
	}
	return out
}

// MappingsInUse returns the set of mapping ids referenced by any VM's hostpciN
// (cluster-wide, templates excluded — a template is never running). Reads each
// VM's config; fine at homelab/rack scale, and the only truthful source since
// PVE has no "who holds this mapping" API.
//
// ponytail: O(VMs) config reads per scheduling decision. Cache it per request if
// a cluster ever has hundreds of VMs.
func (s *Service) MappingsInUse(ctx context.Context) (map[string]bool, error) {
	rows, err := s.C.ClusterResources(ctx, "vm")
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, r := range rows {
		if r.Type != "qemu" || r.Template == 1 {
			continue
		}
		cfg, err := s.C.GetVMConfig(ctx, r.Node, r.VMID)
		if err != nil {
			// A VM that vanished mid-walk is not holding anything; anything else
			// would silently over-promise capacity, so surface it.
			if IsNotFound(err) {
				continue
			}
			return nil, err
		}
		for key := range cfg {
			if !strings.HasPrefix(key, "hostpci") {
				continue
			}
			for _, kv := range splitCommaKV(cfg.Str(key)) {
				if kv[0] == "mapping" && kv[1] != "" {
					used[kv[1]] = true
				}
			}
		}
	}
	return used, nil
}

// GPUInventoryFor builds the availability view for one alias. aliasMappings are
// the mapping ids configured under that alias (config.gpus).
func (s *Service) GPUInventoryFor(ctx context.Context, alias string, aliasMappings []string) (*GPUInventory, error) {
	if len(aliasMappings) == 0 {
		return &GPUInventory{Alias: alias}, nil
	}
	want := map[string]bool{}
	for _, id := range aliasMappings {
		want[id] = true
	}
	maps, err := s.C.ListPCIMappings(ctx)
	if err != nil {
		return nil, err
	}
	inv := &GPUInventory{Alias: alias}
	for _, m := range maps {
		if !want[m.ID] {
			continue
		}
		for _, entry := range m.Map {
			if node := MapEntryNode(entry); node != "" {
				inv.All = append(inv.All, GPUSlot{MappingID: m.ID, Node: node})
			}
		}
	}
	used, err := s.MappingsInUse(ctx)
	if err != nil {
		return nil, err
	}
	for _, slot := range inv.All {
		if !used[slot.MappingID] {
			inv.Free = append(inv.Free, slot)
		}
	}
	return inv, nil
}

// PickGPUSlots reserves n distinct free mappings on one node. Distinct mappings
// matter: two hostpci entries pointing at the SAME mapping would resolve to the
// same physical device and the VM would fail to start.
func (inv GPUInventory) PickGPUSlots(node string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, n)
	for _, s := range inv.Free {
		if s.Node != node || seen[s.MappingID] {
			continue
		}
		seen[s.MappingID] = true
		out = append(out, s.MappingID)
		if len(out) == n {
			return out, nil
		}
	}
	return nil, fmt.Errorf("pve gpu: node %s has %d free %s device(s), %d requested", node, len(out), inv.Alias, n)
}

// GPUCreateParams renders the create/config params for a GPU VM: q35 machine +
// one hostpciN per reserved mapping.
func GPUCreateParams(mappingIDs []string) map[string]string {
	params := map[string]string{"machine": "q35"}
	for i, id := range mappingIDs {
		params["hostpci"+strconv.Itoa(i)] = "mapping=" + id + ",pcie=1"
	}
	return params
}

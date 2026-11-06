package pve

// scheduler.go — node placement, a pure function over one /cluster/resources +
// /nodes snapshot (+ the storage-profile state per node). Filters: node online, allowed
// (template/profile locality), free GPU slots, CPU capacity under the overcommit ceiling,
// RAM for the flavor, free space on the profile's storage. Rank: most free RAM wins. One
// node = degenerate correct.

import (
	"fmt"
	"sort"
)

// SchedReq is one placement request.
type SchedReq struct {
	VCPUs         int
	RAMMb         int64
	OvercommitCPU float64 // ≤0 → 4
	// AllowedNodes restricts placement (storage-profile locality / GPU hosts).
	// nil = all nodes.
	AllowedNodes map[string]bool
	// GPUFree maps node → free passthrough slots for the requested GPU model.
	// nil = no GPU demand.
	GPUFree map[string]int
	GPUs    int
	// Storage maps node → live state of the storage the disk would land on there (the
	// profile's storage). A node missing from it is skipped. nil = no free-space filter.
	Storage map[string]StorageNodeState
	// DiskBytes must fit on Storage while HeadroomPct % of its capacity stays free.
	DiskBytes   int64
	HeadroomPct float64
}

// PickNode chooses the target node. vms is the cluster-wide qemu rows (ALL pools —
// capacity is global); nodes the /nodes list.
func PickNode(nodes []Node, vms []ClusterResource, req SchedReq) (string, error) {
	oc := req.OvercommitCPU
	if oc <= 0 {
		oc = 4
	}
	// Committed resources per node: Σ cores and Σ mem of every non-template VM.
	// Stopped VMs still count — they can start any time and PVE reserves nothing.
	usedCPU := map[string]float64{}
	usedMem := map[string]int64{}
	for _, r := range vms {
		if r.Type != "qemu" || r.Template == 1 || isHolder(r.Tags) {
			continue
		}
		usedCPU[r.Node] += r.MaxCPU
		usedMem[r.Node] += r.MaxMem
	}
	type cand struct {
		name    string
		freeMem int64
	}
	var cands []cand
	for _, n := range nodes {
		if n.Status != "online" {
			continue
		}
		if req.AllowedNodes != nil && !req.AllowedNodes[n.Node] {
			continue
		}
		if req.GPUs > 0 && req.GPUFree[n.Node] < req.GPUs {
			continue
		}
		if req.Storage != nil && !StorageHasRoom(req.Storage[n.Node], req.DiskBytes, req.HeadroomPct) {
			continue
		}
		if usedCPU[n.Node]+float64(req.VCPUs) > float64(n.MaxCPU)*oc {
			continue
		}
		freeMem := n.MaxMem - usedMem[n.Node]
		if freeMem < req.RAMMb*(1<<20) {
			continue
		}
		cands = append(cands, cand{n.Node, freeMem})
	}
	if len(cands) == 0 {
		return "", fmt.Errorf("no node has capacity for %d vCPU / %d MB / %d GiB disk", req.VCPUs, req.RAMMb, req.DiskBytes>>30)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].freeMem > cands[j].freeMem })
	return cands[0].name, nil
}

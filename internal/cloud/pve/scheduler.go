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
	// ReserveMem is memory a node keeps free for the protected VMs that fail over to it. nil = none.
	ReserveMem map[string]int64
	// SpreadGroup keeps the group's members (VMs of SpreadPool carrying its tag) apart: among the
	// nodes that fit, those with the fewest members win. SpreadStrict refuses a node that has one.
	// A preference, not a guarantee: nothing moves a member later (PVE's own rules are HA-only).
	SpreadPool   string
	SpreadGroup  string
	SpreadStrict bool
	// Members adds to the spread count of a node: replicas of a group that are decided and have no VM yet, which no
	// tag can show. With it the fewest-members rank works without a SpreadGroup.
	Members map[string]int
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
	members := map[string]int{}
	for node, n := range req.Members {
		members[node] = n
	}
	for _, r := range vms {
		if r.Type != "qemu" || r.Template == 1 || isHolder(r.Tags) {
			continue
		}
		usedCPU[r.Node] += r.MaxCPU
		usedMem[r.Node] += r.MaxMem
		if req.SpreadGroup != "" && r.Pool == req.SpreadPool && TagValue(r.Tags, tagSpreadPref) == req.SpreadGroup {
			members[r.Node]++
		}
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
		if req.SpreadStrict && members[n.Node] > 0 {
			continue
		}
		if req.Storage != nil && !StorageHasRoom(req.Storage[n.Node], req.DiskBytes, req.HeadroomPct) {
			continue
		}
		if usedCPU[n.Node]+float64(req.VCPUs) > float64(n.MaxCPU)*oc {
			continue
		}
		freeMem := n.MaxMem - usedMem[n.Node] - req.ReserveMem[n.Node]
		if freeMem < req.RAMMb*(1<<20) {
			continue
		}
		cands = append(cands, cand{n.Node, freeMem})
	}
	if len(cands) == 0 {
		return "", fmt.Errorf("no node has capacity for %d vCPU / %d MB / %d GiB disk", req.VCPUs, req.RAMMb, req.DiskBytes>>30)
	}
	sort.Slice(cands, func(i, j int) bool {
		if mi, mj := members[cands[i].name], members[cands[j].name]; mi != mj {
			return mi < mj
		}
		if cands[i].freeMem != cands[j].freeMem {
			return cands[i].freeMem > cands[j].freeMem
		}
		return cands[i].name < cands[j].name // equal nodes: the same pick every time
	})
	return cands[0].name, nil
}

// PlaceReplicas places add more replicas of one group, one PickNode at a time, on top of the have it already runs.
// Each pick counts at once: it becomes a VM of req's size on that node and takes req.DiskBytes from the node's storage
// (from every node's when the storage is shared, which is one pool seen from each node), so the next pick, and the
// next group placed on the returned vms, sees it. req.Storage is updated in place. It returns the group's new
// placement, the vms with the picks added, and how many of add fit; it stops at the first that does not.
func PlaceReplicas(nodes []Node, vms []ClusterResource, req SchedReq, have map[string]int, add int, sharedStorage bool) (map[string]int, []ClusterResource, int) {
	out := map[string]int{}
	for node, n := range have {
		out[node] = n
	}
	fit := 0
	for ; fit < add; fit++ {
		req.Members = out
		node, err := PickNode(nodes, vms, req)
		if err != nil {
			break
		}
		out[node]++
		vms = append(vms, ClusterResource{Type: "qemu", Node: node, MaxCPU: float64(req.VCPUs), MaxMem: req.RAMMb << 20})
		for n, st := range req.Storage {
			if n == node || sharedStorage {
				st.Avail -= req.DiskBytes
				req.Storage[n] = st
			}
		}
	}
	return out, vms, fit
}

// RemoveReplicas takes n replicas off a group's placement. Each one leaves the node whose MachineDeployment has the
// most replicas that are not ready (a stuck machine goes before a healthy one), then the node with the most replicas
// of the group (so a node is emptied last, and with it the volumes that live only there), then the one with the
// least free memory, then by name. unready and freeMem may be nil: that criterion is then skipped.
func RemoveReplicas(have map[string]int, n int, unready map[string]int, freeMem map[string]int64) map[string]int {
	out, left := map[string]int{}, map[string]int{}
	for node, c := range have {
		out[node] = c
		left[node] = unready[node]
	}
	for ; n > 0; n-- {
		pick := ""
		for node, c := range out {
			if c <= 0 {
				continue
			}
			better := pick == "" || left[node] > left[pick] ||
				(left[node] == left[pick] && (c > out[pick] ||
					(c == out[pick] && (freeMem[node] < freeMem[pick] || (freeMem[node] == freeMem[pick] && node < pick)))))
			if better {
				pick = node
			}
		}
		if pick == "" {
			break
		}
		out[pick]--
		if left[pick] > 0 {
			left[pick]--
		}
	}
	return out
}

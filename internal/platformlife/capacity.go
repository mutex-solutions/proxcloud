package platformlife

// capacity.go — the capacity row of the preflight: whether each Proxmox node has the memory the settings place on it,
// counted as CAPMOX counts before a clone.
//
// CAPMOX v0.9.2 schedules every machine whose template names allowedNodes, which the cluster chart does for the
// control plane and every node group (internal/service/scheduler/vmscheduler.go). A node's reservable memory is its
// total less the configured memory of every VM that is not a template and every container on it, running or not, as
// the CAPI token lists them (pkg/proxmox/goproxmox/api_client.go, GetReservableMemoryBytes). A machine that asks for
// more gets VMProvisionFailed, and CAPMOX never tries that machine again. A rolling update makes one machine more
// before it takes one away: one for the control plane, one for each group on each of its nodes.
//
// The row fails on the machines and the control plane's one more: a control plane update that cannot place its new
// machine has nothing that replaces it. The groups' spare machines are counted too and only warn: an upgrade of this
// release refuses a run that would replace machines (unexpected_rollout), so the room is needed when an operator
// changes a group's size or image, not at the install, and failing on it would refuse the smallest preset on a node
// that runs it.
//
// It reads with the CAPI token, so it sees what CAPMOX sees, no more: a VM the token may not audit is not in either
// count, and the node's real free memory can be less. Not verified on a lab.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// PlannedMemoryMiB is the memory the settings place on each node. need: the seed, the control plane machines and one
// more for their rolling update, and each group's machines (split over its nodes as the chart splits machineCount, the
// first nodes taking the remainder). spare: one more machine of each group on each of its nodes, on top of need.
func (in *Installation) PlannedMemoryMiB() (need, spare map[string]int) {
	need, spare = map[string]int{}, map[string]int{}
	s := in.Spec
	if s.Seed.Mode == SeedVM {
		need[s.Seed.Node] += s.Seed.MemoryMiB
	}
	for _, c := range s.Clusters {
		need[c.ControlPlane.Node] += (c.ControlPlane.Count() + 1) * c.ControlPlane.MemoryMiB
		for _, g := range c.NodeGroups {
			for i, node := range g.Nodes {
				n := g.Count / len(g.Nodes)
				if i < g.Count%len(g.Nodes) {
					n++
				}
				need[node] += n * g.MemoryMiB
				spare[node] += g.MemoryMiB
			}
		}
	}
	delete(need, "")
	delete(spare, "")
	return need, spare
}

// capacity is the capacity row. Before the first install a node without room for the machines and the control
// plane's one more fails: the install would stop at a machine CAPMOX refuses, or build a control plane whose first
// update does. A node that holds those and not the groups' spare machines warns. After the install the row warns for
// what a run already placed: the machines run, and the row must not stop the apply that repairs them. It fails for a
// control plane the settings make larger than the one a run built (controlPlaneGrowth) on a node without the room:
// those machines are not placed yet, and the apply would stop at one CAPMOX refuses. A read that fails is a warning.
func (p Preflight) capacity(ctx context.Context) Check {
	in := p.Installation
	warn := func(msg string) Check { return Check{ID: "capacity", Status: CheckWarn, Message: msg} }
	nodes, err := p.CAPI.Nodes(ctx)
	if err != nil {
		return warn("Could not read the nodes' memory with " + pve.CAPITokenID + ", so whether the machines fit is not known: " + err.Error())
	}
	vms, err := p.CAPI.ClusterResources(ctx, "vm")
	if err != nil {
		return warn("Could not read the VMs with " + pve.CAPITokenID + ", so whether the machines fit is not known: " + err.Error())
	}
	// What others take: this installation's own VMs are part of the plan.
	taken := map[string]int64{}
	for _, r := range vms {
		own := slices.Contains(strings.FieldsFunc(r.Tags, func(c rune) bool { return c == ';' || c == ',' || c == ' ' }), "pc-platform-"+in.ID)
		if (r.Type == "qemu" || r.Type == "lxc") && r.Template != 1 && !own {
			taken[r.Node] += r.MaxMem
		}
	}
	planned, spare := in.PlannedMemoryMiB()
	grows := map[string]string{} // node -> the control plane that grows there
	for i, c := range in.Spec.Clusters {
		if from, to := in.controlPlaneGrowth(i); to > 0 {
			grows[c.ControlPlane.Node] = fmt.Sprintf(" The settings take the control plane of %s from %d to %d machines there: an apply stops at this row until they fit.", in.Status.Clusters[i].Name, from, to)
		}
	}
	var short, tight, unknown, fits []string
	grown := false // a node is short where a control plane grows
	for _, node := range slices.Sorted(maps.Keys(planned)) {
		i := slices.IndexFunc(nodes, func(n pve.Node) bool { return n.Node == node })
		if i < 0 || nodes[i].MaxMem == 0 {
			unknown = append(unknown, fmt.Sprintf("Node %s did not report its memory to %s, so whether the %s the settings place there fit is not known.", node, pve.CAPITokenID, gib(planned[node])))
			continue
		}
		room := max(nodes[i].MaxMem-taken[node], 0) >> 20
		if int64(planned[node]) > room {
			short = append(short, fmt.Sprintf("Node %s: the settings place %s of memory there, and %s can be reserved (%s in the node, less %s configured for the other VMs and containers on it).",
				node, gib(planned[node]), gib(int(room)), gib(int(nodes[i].MaxMem>>20)), gib(int(taken[node]>>20)))+grows[node])
			grown = grown || grows[node] != ""
			continue
		}
		if int64(planned[node]+spare[node]) > room {
			tight = append(tight, fmt.Sprintf("Node %s holds the machines (%s of %s), and not the spare machine of every group on it as well (%s more): a change that replaces a group's machines there needs that room, or the new machine is refused and not tried again.",
				node, gib(planned[node]), gib(int(room)), gib(spare[node])))
			continue
		}
		fits = append(fits, fmt.Sprintf("%s: %s of %s", node, gib(planned[node]+spare[node]), gib(int(room))))
	}
	const how = " The count is the one made before each clone: the configured memory of every VM and container on the node, running or not, as " + pve.CAPITokenID +
		" lists them; a VM that token cannot see is not counted. The row fails on each machine of the settings and the one more a rolling update of the control plane makes; " +
		"the one more of each group on each of its nodes is counted too and only warns."
	if len(short) == 0 && len(tight)+len(unknown) > 0 {
		return warn(strings.Join(append(tight, unknown...), "\n") + how)
	}
	if len(short) == 0 {
		return Check{ID: "capacity", Status: CheckOK, Message: "Every node has the memory for its machines and their spares during an update (" + strings.Join(fits, "; ") + ")." + how}
	}
	msg := strings.Join(append(append(short, tight...), unknown...), "\n") + "\nA machine that does not fit is refused and not tried again, at the install or at the first update of the control plane. " +
		"Use smaller or fewer machines, fewer control plane machines, another node, or free memory on the node (a stopped VM counts with its configured memory)." + how
	if in.Installed() && !grown {
		return warn(msg)
	}
	return Check{ID: "capacity", Status: CheckFail, Message: msg}
}

// GrowthBlocking is why an apply of an installed platform cannot add the machines its plan lists: the capacity row's
// message when it fails, which after an install it does only for growth. nil when they fit, when the nodes cannot be
// read or without the CAPI token: stage 1 of the apply runs the same row and decides.
func (p Preflight) GrowthBlocking(ctx context.Context) []string {
	if p.CAPI == nil {
		return nil
	}
	if c := p.capacity(ctx); c.Status == CheckFail {
		return []string{c.Message}
	}
	return nil
}

// gib writes MiB as GiB, with one decimal when it is not whole.
func gib(mib int) string {
	if mib%1024 == 0 {
		return fmt.Sprintf("%d GiB", mib/1024)
	}
	return fmt.Sprintf("%.1f GiB", float64(mib)/1024)
}

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
// machine has nothing that replaces it. The groups' spare machines are counted too and only warn: an upgrade that
// replaces the machines takes one unit at a time and decides per unit, when its plan is read, whether the node has
// room for one more machine of that unit or the machine is deleted first (rollRoom, which reads the same numbers), so
// the sum of all spares is never needed at once, and failing on it would refuse the smallest preset on a node that
// runs it.
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
	mem, what, err := readMemory(ctx, p.CAPI, in.ID)
	if err != nil {
		return warn("Could not read " + what + " with " + pve.CAPITokenID + ", so whether the machines fit is not known: " + err.Error())
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
		m := mem[node]
		if m.total == 0 {
			unknown = append(unknown, fmt.Sprintf("Node %s did not report its memory to %s, so whether the %s the settings place there fit is not known.", node, pve.CAPITokenID, gib(planned[node])))
			continue
		}
		room := max(m.total-m.others, 0) >> 20
		if int64(planned[node]) > room {
			short = append(short, fmt.Sprintf("Node %s: the settings place %s of memory there, and %s can be reserved (%s in the node, less %s configured for the other VMs and containers on it).",
				node, gib(planned[node]), gib(int(room)), gib(int(m.total>>20)), gib(int(m.others>>20)))+grows[node])
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

// nodeMemory is one node's memory in bytes as CAPMOX counts it before a clone: the node's total, what the VMs and
// containers of others are configured with (running or not, templates left out), and what this installation's own are.
type nodeMemory struct{ total, others, own int64 }

// free is what a clone may still ask for, in MiB.
func (m nodeMemory) free() int { return int(max(m.total-m.others-m.own, 0) >> 20) }

// readMemory is the one read the capacity row and the roll share, with the CAPI token. what names the read that failed.
func readMemory(ctx context.Context, capi *pve.Client, id string) (mem map[string]nodeMemory, what string, err error) {
	nodes, err := capi.Nodes(ctx)
	if err != nil {
		return nil, "the nodes' memory", err
	}
	vms, err := capi.ClusterResources(ctx, "vm")
	if err != nil {
		return nil, "the VMs", err
	}
	mem = map[string]nodeMemory{}
	for _, n := range nodes {
		mem[n.Node] = nodeMemory{total: n.MaxMem}
	}
	for _, r := range vms {
		if (r.Type != "qemu" && r.Type != "lxc") || r.Template == 1 {
			continue
		}
		m := mem[r.Node]
		if slices.Contains(strings.FieldsFunc(r.Tags, func(c rune) bool { return c == ';' || c == ',' || c == ' ' }), "pc-platform-"+id) {
			m.own += r.MaxMem
		} else {
			m.others += r.MaxMem
		}
		mem[r.Node] = m
	}
	return mem, "", nil
}

// rollRoom decides, for a roll of the cluster's machines, where the one more machine a replacement makes first has
// room. Units are replaced one at a time, so each needs one spare of its own size and no more: the sum the capacity
// row warns about is never needed at once. short is the refusal for the control plane, which always makes one more
// machine first (a control plane of one cannot delete its only etcd member first); deleteFirst are the units
// ("group/node") whose node has no room, which then replace a machine by deleting it first. The memory is CAPMOX's
// count (readMemory, own machines included: they run); the boot store must also hold one more full disk. A store
// the token cannot read is not counted.
func rollRoom(ctx context.Context, capi *pve.Client, in *Installation) (short string, deleteFirst []string, err error) {
	if capi == nil {
		return "", nil, fmt.Errorf("the token of Cluster API is not stored")
	}
	mem, what, err := readMemory(ctx, capi, in.ID)
	if err != nil {
		return "", nil, fmt.Errorf("could not read %s with %s: %w", what, pve.CAPITokenID, err)
	}
	disk := map[string]int64{} // node -> free bytes on the boot store, when it could be read
	for _, u := range rollUnits(in.Spec.Clusters[0]) {
		if u.Machines == 0 {
			continue
		}
		if _, ok := disk[u.Node]; !ok {
			disk[u.Node] = -1
			if stores, err := capi.NodeStorages(ctx, u.Node); err == nil {
				for _, s := range stores {
					if s.Storage == in.Spec.Storage.Boot {
						disk[u.Node] = s.Avail
					}
				}
			}
		}
		var why string
		switch m := mem[u.Node]; {
		case m.total == 0:
			why = fmt.Sprintf("node %s did not report its memory to %s", u.Node, pve.CAPITokenID)
		case m.free() < u.MemoryMiB:
			why = fmt.Sprintf("node %s can reserve %s more (%s in the node, less %s configured for this platform's machines and %s for the other VMs and containers), and one more machine of %s needs %s",
				u.Node, gib(m.free()), gib(int(m.total>>20)), gib(int(m.own>>20)), gib(int(m.others>>20)), u.Name, gib(u.MemoryMiB))
		case disk[u.Node] >= 0 && disk[u.Node] < int64(u.DiskGiB)<<30:
			why = fmt.Sprintf("store %s on node %s has %d GiB free, and one more machine of %s needs a disk of %d GiB", in.Spec.Storage.Boot, u.Node, disk[u.Node]>>30, u.Name, u.DiskGiB)
		default:
			continue
		}
		if u.Key == "" {
			short = "The control plane is replaced by making one more machine first, and " + why + ". Free memory or space on the node (a stopped VM counts " +
				"with its configured memory), then read the plan again. The control plane is never replaced by deleting its machine first."
		} else {
			deleteFirst = append(deleteFirst, u.Key)
		}
	}
	return short, deleteFirst, nil
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

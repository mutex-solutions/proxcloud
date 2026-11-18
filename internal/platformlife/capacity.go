package platformlife

// capacity.go — the capacity row of the preflight: whether each Proxmox node has the memory the settings place on it,
// counted as CAPMOX counts before a clone.
//
// CAPMOX v0.9.2 schedules every machine whose template names allowedNodes, which the cluster chart does for the
// control plane and every node group (internal/service/scheduler/vmscheduler.go). A node's reservable memory is its
// total less the configured memory of every VM that is not a template and every container on it, running or not, as
// the CAPI token lists them (pkg/proxmox/goproxmox/api_client.go, GetReservableMemoryBytes). A machine that asks for
// more gets VMProvisionFailed, and CAPMOX never tries that machine again. A rolling update makes one machine more
// before it takes one away: one for the control plane, one for each group on each of its nodes. So a node that holds
// exactly the machines of the settings installs, and then fails its first update for good. The row therefore counts
// the machines and those spares.
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

// PlannedMemoryMiB is the memory the settings place on each node: the seed, the control plane machines and one more
// for their rolling update, and each group's machines (split over its nodes as the chart splits machineCount, the
// first nodes taking the remainder) with one more per node.
func (in *Installation) PlannedMemoryMiB() map[string]int {
	out := map[string]int{}
	s := in.Spec
	if s.Seed.Mode == SeedVM {
		out[s.Seed.Node] += s.Seed.MemoryMiB
	}
	for _, c := range s.Clusters {
		out[c.ControlPlane.Node] += (c.ControlPlane.Count() + 1) * c.ControlPlane.MemoryMiB
		for _, g := range c.NodeGroups {
			for i, node := range g.Nodes {
				n := g.Count / len(g.Nodes)
				if i < g.Count%len(g.Nodes) {
					n++
				}
				out[node] += (n + 1) * g.MemoryMiB
			}
		}
	}
	delete(out, "")
	return out
}

// capacity is the capacity row. Before the first install a node without room fails: the install would stop at a
// machine CAPMOX refuses, or build a platform whose first update does. After it a warning: the machines run, and the
// row must not stop the apply that repairs them. A read that fails is a warning too.
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
	planned := in.PlannedMemoryMiB()
	var short, unknown, fits []string
	for _, node := range slices.Sorted(maps.Keys(planned)) {
		i := slices.IndexFunc(nodes, func(n pve.Node) bool { return n.Node == node })
		if i < 0 || nodes[i].MaxMem == 0 {
			unknown = append(unknown, fmt.Sprintf("Node %s did not report its memory to %s, so whether the %s the settings place there fit is not known.", node, pve.CAPITokenID, gib(planned[node])))
			continue
		}
		room := max(nodes[i].MaxMem-taken[node], 0) >> 20
		if int64(planned[node]) > room {
			short = append(short, fmt.Sprintf("Node %s: the settings place %s of memory there, and %s can be reserved (%s in the node, less %s configured for the other VMs and containers on it).",
				node, gib(planned[node]), gib(int(room)), gib(int(nodes[i].MaxMem>>20)), gib(int(taken[node]>>20))))
			continue
		}
		fits = append(fits, fmt.Sprintf("%s: %s of %s", node, gib(planned[node]), gib(int(room))))
	}
	const how = " The count is the one made before each clone: the configured memory of every VM and container on the node, running or not, as " + pve.CAPITokenID +
		" lists them; a VM that token cannot see is not counted. It holds each machine of the settings and the spare ones a rolling update makes: one for the control plane, one for each group on each of its nodes."
	if len(short) == 0 && len(unknown) > 0 {
		return warn(strings.Join(unknown, "\n") + how)
	}
	if len(short) == 0 {
		return Check{ID: "capacity", Status: CheckOK, Message: "Every node has the memory for its machines and their spares during an update (" + strings.Join(fits, "; ") + ")." + how}
	}
	msg := strings.Join(append(short, unknown...), "\n") + "\nA machine that does not fit is refused and not tried again, at the install or at the first update that replaces machines. " +
		"Use smaller or fewer machines, fewer control plane machines, another node, or free memory on the node (a stopped VM counts with its configured memory)." + how
	if in.Installed() {
		return warn(msg)
	}
	return Check{ID: "capacity", Status: CheckFail, Message: msg}
}

// gib writes MiB as GiB, with one decimal when it is not whole.
func gib(mib int) string {
	if mib%1024 == 0 {
		return fmt.Sprintf("%d GiB", mib/1024)
	}
	return fmt.Sprintf("%.1f GiB", float64(mib)/1024)
}

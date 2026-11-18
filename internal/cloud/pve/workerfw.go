package pve

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// The VM firewall of a tenant Kubernetes cluster's workers (docs/managed-k8s.md, "Worker firewall"). CAPMOX clones
// the workers and has no field for a NIC's firewall flag, so the portal puts the firewall on after the clone, as it
// does for the sandbox workers (sandboxfw.go). Unlike those, a tenant worker keeps its egress open and its rules are
// an ordinary security group of the project, named after the cluster, which the tenant may edit.
//
// That group is also the switch: a cluster whose group exists is protected, and the provider sync brings every
// running worker of it behind the firewall. No group, nothing is touched.

// WorkerTagPrefix + the cluster id is the tag every worker VM of a cluster carries (the chart's machineTags).
const WorkerTagPrefix = "pc-k8s-"

// workerGroupPrefix + the cluster id is the display name of a cluster's security group. Tenants cannot create or
// delete a group under it.
const workerGroupPrefix = "k8s-"

// workerNIC is the one NIC the chart gives a worker. Any other NIC gets no IP filter set, which PVE reads as an
// empty one: it sends nothing.
const workerNIC = "net0"

// WorkerGroupName is the display name of a cluster's security group.
func WorkerGroupName(clusterID string) string { return workerGroupPrefix + clusterID }

// IsWorkerGroupName reports a security group name reserved for a cluster's workers.
func IsWorkerGroupName(name string) bool { return strings.HasPrefix(name, workerGroupPrefix) }

// IsWorkerVM reports a VM that CAPMOX made as a worker of some cluster.
func IsWorkerVM(tags string) bool {
	return slices.ContainsFunc(strings.Split(tags, ";"), func(t string) bool { return strings.HasPrefix(t, WorkerTagPrefix) })
}

// errWorkerGroups refuses a security group change on a worker row: its one group is the cluster's.
func errWorkerGroups() error {
	return &ConflictError{Msg: "This server is a worker node of a Kubernetes cluster. Its firewall is the cluster's security group: change the rules of that group instead"}
}

// workerFirewallOptions: default-deny ingress, open egress, anti-spoofing. No DHCP: CAPMOX gives static addresses.
var workerFirewallOptions = map[string]string{
	"enable": "1", "ipfilter": "1", "macfilter": "1", "dhcp": "0",
	"policy_in": "DROP", "policy_out": "ACCEPT",
}

// WorkerFirewall is what the workers of one cluster may be reached from.
type WorkerFirewall struct {
	ProjectID string
	ClusterID string
	Workers   []string // the worker addresses the cluster reserved on its vnet
	// LoadBalancers are the cluster's MetalLB addresses: any worker may answer for them (layer 2).
	LoadBalancers []string
	CIDRPool      string // the provider's tenant pool; other tenants' part of it is dropped by the isolation rules
}

// hostCIDRs covers IPv4 host addresses ("/32" allowed) with the fewest CIDRs. A rule's source takes a list of
// CIDRs, not a list of ranges.
func hostCIDRs(addrs []string) ([]string, error) {
	as := make([]netip.Addr, 0, len(addrs))
	for _, raw := range addrs {
		a, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimSpace(raw), "/32"))
		if err != nil || !a.Is4() {
			return nil, fmt.Errorf("pve worker firewall: invalid IPv4 address %q", raw)
		}
		as = append(as, a)
	}
	slices.SortFunc(as, func(a, b netip.Addr) int { return a.Compare(b) })
	as = slices.Compact(as)
	var out []string
	for i := 0; i < len(as); {
		j := i
		for j+1 < len(as) && as[j].Next() == as[j+1] {
			j++
		}
		out = append(out, cidrList(rangeCIDRs(as[i], as[j]))...)
		i = j + 1
	}
	return out, nil
}

// rules is the content of a new cluster group. Every rule is an inbound ACCEPT; the worker's policy drops the rest.
// Nothing comes in from the control plane: the kubelet and the konnectivity agent open their connections to it.
func (f WorkerFirewall) rules() ([]FirewallRule, error) {
	workers, err := hostCIDRs(f.Workers)
	if err != nil {
		return nil, err
	}
	lbs, err := hostCIDRs(f.LoadBalancers)
	if err != nil {
		return nil, err
	}
	if len(workers) == 0 {
		return nil, errors.New("pve worker firewall: the cluster has no worker addresses")
	}
	from := strings.Join(workers, ",")
	in := func(proto, source, dest, dport, comment string) FirewallRule {
		return FirewallRule{Type: "in", Action: "ACCEPT", Proto: proto, Source: source, Dest: dest, DPort: dport, Enable: 1, Comment: comment}
	}
	rules := []FirewallRule{
		in("udp", from, "", "8472", "workers: Cilium VXLAN (pod network)"),
		in("tcp", from, "", "4240", "workers: Cilium health"),
		in("icmp", from, "", "", "workers: Cilium health, ping"),
		in("tcp", from, "", "10250", "workers: kubelet (logs and exec through konnectivity, metrics)"),
		in("tcp", from, "", "7946", "workers: MetalLB memberlist"),
		in("udp", from, "", "7946", "workers: MetalLB memberlist"),
	}
	if len(lbs) > 0 {
		rules = append(rules, in("", "", strings.Join(lbs, ","), "", "LoadBalancer Services (MetalLB addresses)"))
	}
	if f.CIDRPool != "" {
		rules = append(rules,
			in("tcp", f.CIDRPool, "", "30000:32767", "NodePort Services, from the project's networks"),
			in("udp", f.CIDRPool, "", "30000:32767", "NodePort Services, from the project's networks"),
		)
	}
	return rules, nil
}

// WorkerGroup is the id of a cluster's security group, "" when the cluster has none (its workers are not protected).
func (s *Service) WorkerGroup(ctx context.Context, projectID, clusterID string) (string, error) {
	groups, err := s.C.ListFirewallGroups(ctx)
	if err != nil {
		return "", err
	}
	for _, g := range groups {
		if pid, name, ok := aliasProject(g.Comment); ok && pid == projectID && name == WorkerGroupName(clusterID) {
			return g.Group, nil
		}
	}
	return "", nil
}

// EnsureWorkerGroup returns the cluster's group, creating it with f's rules when missing. An existing group is left
// as it is: the tenant may have edited it.
func (s *Service) EnsureWorkerGroup(ctx context.Context, f WorkerFirewall) (string, error) {
	rules, err := f.rules()
	if err != nil {
		return "", err
	}
	if id, err := s.WorkerGroup(ctx, f.ProjectID, f.ClusterID); err != nil || id != "" {
		return id, err
	}
	id, err := s.CreateSecurityGroup(ctx, f.ProjectID, WorkerGroupName(f.ClusterID))
	if err != nil {
		return "", err
	}
	for i := len(rules) - 1; i >= 0; i-- { // each new rule lands on top
		if err := s.C.AddFirewallRule(ctx, id, rules[i]); err != nil {
			_ = s.DeleteSecurityGroup(context.WithoutCancel(ctx), f.ProjectID, id) // a half-built group would cut the workers
			return "", err
		}
	}
	return id, nil
}

// DeleteWorkerGroup removes a cluster's group once its workers are gone (the cluster's release). No group: nothing.
func (s *Service) DeleteWorkerGroup(ctx context.Context, projectID, clusterID string) error {
	id, err := s.WorkerGroup(ctx, projectID, clusterID)
	if err != nil || id == "" {
		return err
	}
	return s.DeleteSecurityGroup(ctx, projectID, id)
}

// WorkerState is one worker VM of a cluster and whether its firewall is in place.
type WorkerState struct {
	VMID    int    `json:"vmid"`
	Name    string `json:"name"`
	Node    string `json:"node"`
	Status  string `json:"status"`
	Secured bool   `json:"secured"`
	// Changed: this call wrote something on the VM.
	Changed bool `json:"changed,omitempty"`
}

func workersOf(vms []ClusterResource, pool, clusterID string) []ClusterResource {
	var out []ClusterResource
	for _, vm := range vms {
		if vm.Type == "qemu" && vm.Template == 0 && vm.Pool == pool && HasTag(vm.Tags, WorkerTagPrefix+clusterID) {
			out = append(out, vm)
		}
	}
	slices.SortFunc(out, func(a, b ClusterResource) int { return a.VMID - b.VMID })
	return out
}

// ClusterWorkers lists a cluster's worker VMs and, for a cluster with a group, whether each one's firewall reads as
// SecureClusterWorkers leaves it. It writes nothing. addrs are the cluster's reserved addresses.
func (s *Service) ClusterWorkers(ctx context.Context, projectID, clusterID, group string, addrs []string) ([]WorkerState, error) {
	return s.clusterWorkers(ctx, projectID, clusterID, group, addrs, false)
}

// SecureClusterWorkers puts every running worker of the cluster behind the VM firewall with group as its rules and
// addrs (the cluster's worker and load balancer addresses) as the only sources it may send from. A worker that is
// not running is left for a later pass: CAPMOX rewrites a new clone's NIC until its first start.
func (s *Service) SecureClusterWorkers(ctx context.Context, projectID, clusterID, group string, addrs []string) ([]WorkerState, error) {
	return s.clusterWorkers(ctx, projectID, clusterID, group, addrs, true)
}

func (s *Service) clusterWorkers(ctx context.Context, projectID, clusterID, group string, addrs []string, apply bool) ([]WorkerState, error) {
	vms, err := s.C.ClusterResources(ctx, "vm")
	if err != nil {
		return nil, err
	}
	return s.secureWorkers(ctx, workersOf(vms, s.PoolFor(projectID), clusterID), group, addrs, apply)
}

func (s *Service) secureWorkers(ctx context.Context, workers []ClusterResource, group string, addrs []string, apply bool) ([]WorkerState, error) {
	out := make([]WorkerState, 0, len(workers))
	var errs []error
	for _, vm := range workers {
		st := WorkerState{VMID: vm.VMID, Name: vm.Name, Node: vm.Node, Status: vm.Status}
		if group != "" && vm.Status == "running" && !vm.NodeDown {
			changed, err := s.secureWorker(ctx, vm.Node, vm.VMID, group, addrs, apply)
			if err != nil {
				errs = append(errs, fmt.Errorf("pve worker firewall: VM %d (%s): %w", vm.VMID, vm.Name, err))
			}
			st.Changed = changed && apply
			st.Secured = err == nil && (apply || !changed)
		}
		out = append(out, st)
	}
	return out, errors.Join(errs...)
}

// secureWorker checks one worker against what it must carry and, with apply, writes what differs: the IP filter, the
// group reference, the options, then firewall=1 on every NIC. Until that flag is on PVE applies none of the rest, so
// it goes last and a running worker is never filtered by a half-written firewall. changed reports a difference.
func (s *Service) secureWorker(ctx context.Context, node string, vmid int, group string, addrs []string, apply bool) (changed bool, err error) {
	if len(addrs) == 0 {
		return false, errors.New("no addresses for the IP filter: an empty filter would cut the worker off")
	}
	c := s.C
	cfg, err := c.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return false, err
	}
	want := make([]IPSet, 0, len(addrs))
	for _, a := range addrs {
		want = append(want, IPSet{CIDR: a})
	}
	sets, err := c.ListVMIPSets(ctx, node, vmid)
	if err != nil {
		return false, err
	}
	filtered := false
	if slices.ContainsFunc(sets, func(x IPSet) bool { return x.Name == "ipfilter-"+workerNIC }) {
		have, err := c.ListVMIPSetEntries(ctx, node, vmid, "ipfilter-"+workerNIC)
		if err != nil {
			return false, err
		}
		filtered = sameEntries(have, want)
	}
	if !filtered {
		changed = true
		if apply {
			if err := s.SetIPFilter(ctx, node, vmid, workerNIC, addrs); err != nil {
				return changed, err
			}
		}
	}
	rules, err := c.ListVMFirewallRules(ctx, node, vmid)
	if err != nil {
		return changed, err
	}
	if !slices.ContainsFunc(rules, func(r FirewallRule) bool { return r.Type == "group" && r.Action == group && r.Enable == 1 }) {
		changed = true
		if apply {
			if err := c.AddVMGroupRule(ctx, node, vmid, group); err != nil {
				return changed, err
			}
		}
	}
	opts, err := c.GetVMFirewallOptions(ctx, node, vmid)
	if err != nil {
		return changed, err
	}
	for k, v := range workerFirewallOptions {
		have, ok := opts[k]
		if !ok {
			have = vmFirewallDefaults[k]
		}
		if fmt.Sprint(have) != v {
			changed = true
			if apply {
				if err := c.SetVMFirewallOptions(ctx, node, vmid, workerFirewallOptions); err != nil {
					return changed, err
				}
			}
			break
		}
	}
	if params := nicFirewallParams(cfg); len(params) > 0 {
		changed = true
		if apply {
			upid, err := c.SetVMConfig(ctx, node, vmid, params)
			if err == nil {
				err = c.WaitTask(ctx, upid)
			}
			if err != nil {
				return changed, err
			}
		}
	}
	return changed, nil
}

// UnsecureClusterWorkers takes the firewall off every worker of the cluster and deletes its group: the way back
// from SecureClusterWorkers. The VM firewall is switched off first, which stops all filtering at once; the NIC flag
// and the IP filter set stay and do nothing without it. A worker that cannot be changed keeps the group, and with it
// the cluster stays protected.
func (s *Service) UnsecureClusterWorkers(ctx context.Context, projectID, clusterID string) error {
	group, err := s.WorkerGroup(ctx, projectID, clusterID)
	if err != nil || group == "" {
		return err
	}
	vms, err := s.C.ClusterResources(ctx, "vm")
	if err != nil {
		return err
	}
	for _, vm := range workersOf(vms, s.PoolFor(projectID), clusterID) {
		if err := s.C.SetVMFirewallOptions(ctx, vm.Node, vm.VMID, map[string]string{"enable": "0"}); err != nil {
			return fmt.Errorf("pve worker firewall: VM %d (%s): %w", vm.VMID, vm.Name, err)
		}
		for { // one reference at a time: each delete changes the digest
			rules, err := s.C.ListVMFirewallRules(ctx, vm.Node, vm.VMID)
			if err != nil {
				return fmt.Errorf("pve worker firewall: VM %d (%s): %w", vm.VMID, vm.Name, err)
			}
			i := slices.IndexFunc(rules, func(r FirewallRule) bool { return r.Type == "group" && r.Action == group })
			if i < 0 {
				break
			}
			if err := s.C.DeleteVMFirewallRule(ctx, vm.Node, vm.VMID, rules[i].Pos, rules[i].Digest); err != nil {
				return fmt.Errorf("pve worker firewall: VM %d (%s): %w", vm.VMID, vm.Name, err)
			}
		}
	}
	return s.DeleteSecurityGroup(ctx, projectID, group)
}

// ReconcileWorkerFirewalls is the provider sync's pass: every cluster that has a group gets its running workers
// behind the firewall, so a worker added or replaced after the cluster was protected is covered within one sync.
// addrsOf gives a cluster's reserved addresses; none (a cluster of another portal, or one being released) skips it.
func (s *Service) ReconcileWorkerFirewalls(ctx context.Context, addrsOf func(ctx context.Context, projectID, clusterID string) ([]string, error)) error {
	groups, err := s.C.ListFirewallGroups(ctx)
	if err != nil {
		return err
	}
	var vms []ClusterResource
	var errs []error
	for _, g := range groups {
		pid, name, ok := aliasProject(g.Comment)
		clusterID, isWorker := strings.CutPrefix(name, workerGroupPrefix)
		if !ok || !isWorker {
			continue
		}
		addrs, err := addrsOf(ctx, pid, clusterID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(addrs) == 0 {
			continue
		}
		if vms == nil {
			if vms, err = s.C.ClusterResources(ctx, "vm"); err != nil {
				return err
			}
		}
		if _, err := s.secureWorkers(ctx, workersOf(vms, s.PoolFor(pid), clusterID), g.Group, addrs, true); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

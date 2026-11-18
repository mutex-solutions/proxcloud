package pve

// firewallsvc.go — SECURITY_GROUP over cluster firewall groups. Group ids are
// opaque ("g"+random base36 — PVE caps group names hard) and ownership rides the
// COMMENT ("st:<projectID>:<display name>", the vnet-alias discipline) because
// groups have no pool scoping. VM attach = a per-VM firewall rule referencing the
// group + the VM firewall switched on.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// CreateSecurityGroup creates a project-owned firewall group; returns its id.
func (s *Service) CreateSecurityGroup(ctx context.Context, projectID, name string) (string, error) {
	groups, err := s.C.ListFirewallGroups(ctx)
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, g := range groups {
		taken[g.Group] = true
	}
	id := ""
	for range 8 {
		cand := "g" + randBase36(7)
		if !taken[cand] {
			id = cand
			break
		}
	}
	if id == "" {
		return "", fmt.Errorf("pve: could not allocate a group id")
	}
	if err := s.C.CreateFirewallGroup(ctx, id, OwnershipAlias(projectID, name)); err != nil {
		return "", err
	}
	return id, nil
}

// ownedGroup verifies a group's ownership comment; nil = absent.
func (s *Service) ownedGroup(ctx context.Context, projectID, groupID string) (*FirewallGroup, error) {
	groups, err := s.C.ListFirewallGroups(ctx)
	if err != nil {
		return nil, err
	}
	for i := range groups {
		if groups[i].Group == groupID {
			if pid, _, ok := aliasProject(groups[i].Comment); !ok || pid != projectID {
				return nil, fmt.Errorf("pve: group %s is not owned by project %s", groupID, projectID)
			}
			return &groups[i], nil
		}
	}
	return nil, nil
}

// DeleteSecurityGroup removes an owned group (idempotent on absent).
// PVE refuses to delete a NON-EMPTY group ("Security group 'x' is not empty"),
// so the rules go first — top-down, since deleting a rule renumbers the ones
// below it.
func (s *Service) DeleteSecurityGroup(ctx context.Context, projectID, groupID string) error {
	g, err := s.ownedGroup(ctx, projectID, groupID)
	if err != nil {
		return err
	}
	if g == nil {
		return nil
	}
	rules, err := s.C.ListFirewallRules(ctx, groupID)
	if err != nil {
		return err
	}
	for i := len(rules) - 1; i >= 0; i-- {
		if err := s.C.DeleteFirewallRule(ctx, groupID, rules[i].Pos, ""); err != nil {
			return err
		}
	}
	return s.C.DeleteFirewallGroup(ctx, groupID)
}

// DeleteProjectSecurityGroups removes every group the project owns (teardown,
// once its VMs are gone).
func (s *Service) DeleteProjectSecurityGroups(ctx context.Context, projectID string) error {
	groups, err := s.C.ListFirewallGroups(ctx)
	if err != nil {
		return err
	}
	for _, g := range groups {
		if pid, _, ok := aliasProject(g.Comment); ok && pid == projectID {
			if err := s.DeleteSecurityGroup(ctx, projectID, g.Group); err != nil {
				return err
			}
		}
	}
	return nil
}

// SGAddRule appends a rule to an owned group.
func (s *Service) SGAddRule(ctx context.Context, projectID, groupID string, r FirewallRule) error {
	g, err := s.ownedGroup(ctx, projectID, groupID)
	if err != nil {
		return err
	}
	if g == nil {
		return fmt.Errorf("pve: group %s not found", groupID)
	}
	return s.C.AddFirewallRule(ctx, groupID, r)
}

// SecurityGroupRuleID is the id a rule is listed and deleted by: "<group>-<pos>-<fingerprint of its content>". A
// position alone names whatever rule sits there now (a new rule lands at 0 and shifts the rest), so an id read from
// an older list stops matching once the rule moved or changed.
func SecurityGroupRuleID(group string, r FirewallRule) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{r.Type, r.Action, r.Proto, r.Source, r.Dest, r.DPort, r.SPort, r.Comment}, "\x00")))
	return group + "-" + strconv.Itoa(r.Pos) + "-" + hex.EncodeToString(sum[:4])
}

// sgRuleChanged is the refusal of a delete whose rule is not in the group as the caller listed it.
func sgRuleChanged() error {
	return &ConflictError{Msg: "The rules of this security group have changed. Refresh the list and try again"}
}

// SGDeleteRule removes the rule of an owned group that is listed under ruleID now; a ConflictError when none is.
func (s *Service) SGDeleteRule(ctx context.Context, projectID, groupID, ruleID string) error {
	g, err := s.ownedGroup(ctx, projectID, groupID)
	if err != nil {
		return err
	}
	if g == nil {
		return fmt.Errorf("pve: group %s not found", groupID)
	}
	rules, err := s.C.ListFirewallRules(ctx, groupID)
	if err != nil {
		return err
	}
	for _, r := range rules {
		if SecurityGroupRuleID(groupID, r) != ruleID {
			continue
		}
		// The digest closes the gap between this read and the delete. PVE's refusal text is from its source
		// (assert_if_modified), not from a lab run.
		err := s.C.DeleteFirewallRule(ctx, groupID, r.Pos, r.Digest)
		if err != nil && strings.Contains(err.Error(), "detected modified configuration") {
			return sgRuleChanged()
		}
		return err
	}
	return sgRuleChanged()
}

// sameGroupRule: two rules say the same thing, wherever they sit in the group (every field SecurityGroupRuleID
// fingerprints).
func sameGroupRule(a, b FirewallRule) bool {
	a.Pos, a.Digest, a.Enable = 0, "", 0
	b.Pos, b.Digest, b.Enable = 0, "", 0
	return a == b
}

// apiDPort is a destination port as the API writes one: none, "n" or "n:m".
var apiDPort = regexp.MustCompile(`^(\d+(:\d+)?)?$`)

// madeOutsideAPI: the rule says something a rule of the API cannot (the API writes ACCEPT rules with a protocol,
// one port or range, and a remote on the far side). An operator made it in Proxmox, and an edit through the API
// would drop that part: a DROP becomes an ACCEPT, "80,443" becomes every port.
func madeOutsideAPI(r FirewallRule) bool {
	wrongSide := (r.Type == "in" && r.Dest != "") || (r.Type == "out" && r.Source != "")
	return (r.Type != "in" && r.Type != "out") || r.Action != "ACCEPT" || r.SPort != "" || r.Macro != "" ||
		r.ICMPType != "" || wrongSide || !apiDPort.MatchString(r.DPort)
}

// SGReplaceRule puts r where the rule listed under ruleID is: the edit of a rule. PVE has no call that swaps a
// group rule for another with a check on what it replaces, so this adds the new rule and then deletes the old one.
// In that order there is no moment without either: deleting first would cut the traffic the rule allows, and
// lose the rule if the add were refused. A group that changed between the two steps keeps both rules, and the
// ConflictError says so.
func (s *Service) SGReplaceRule(ctx context.Context, projectID, groupID, ruleID string, r FirewallRule) error {
	g, err := s.ownedGroup(ctx, projectID, groupID)
	if err != nil {
		return err
	}
	if g == nil {
		return fmt.Errorf("pve: group %s not found", groupID)
	}
	rules, err := s.C.ListFirewallRules(ctx, groupID)
	if err != nil {
		return err
	}
	var old *FirewallRule
	for i := range rules {
		if SecurityGroupRuleID(groupID, rules[i]) == ruleID {
			old = &rules[i]
		}
	}
	if old == nil {
		return sgRuleChanged()
	}
	if madeOutsideAPI(*old) {
		return &ConflictError{Msg: "This rule was made outside ProxCloud and holds settings an edit here would lose. Delete it and add a new rule instead"}
	}
	if sameGroupRule(*old, r) {
		return nil
	}
	if err := s.C.AddFirewallRule(ctx, groupID, r); err != nil {
		return err
	}
	kept := func(why string) error {
		return &ConflictError{Msg: "The new rule was added, but the old one is still there: " + why + ". Refresh the list and delete the old rule"}
	}
	// The add moved the old rule, and its digest is of the list before the add: read again.
	if rules, err = s.C.ListFirewallRules(ctx, groupID); err != nil {
		return kept("the old rule could not be removed")
	}
	for _, cur := range rules {
		if !sameGroupRule(cur, *old) {
			continue
		}
		if err := s.C.DeleteFirewallRule(ctx, groupID, cur.Pos, cur.Digest); err != nil {
			if strings.Contains(err.Error(), "detected modified configuration") {
				break
			}
			return kept("the old rule could not be removed")
		}
		return nil
	}
	return kept("the rules of this security group changed meanwhile")
}

// AttachSecurityGroup references an owned group from a pool VM's firewall and
// switches the VM firewall + its NICs' firewall flag on.
func (s *Service) AttachSecurityGroup(ctx context.Context, projectID, serverExtID, groupID string) error {
	g, err := s.ownedGroup(ctx, projectID, groupID)
	if err != nil {
		return err
	}
	if g == nil {
		return fmt.Errorf("pve: group %s not found", groupID)
	}
	if _, name, _ := aliasProject(g.Comment); IsWorkerGroupName(name) {
		return errWorkerGroupOnServer(name)
	}
	vm, err := s.ResolveVM(ctx, projectID, serverExtID)
	if err != nil {
		return err
	}
	if vm == nil {
		return fmt.Errorf("pve: server %s not found", serverExtID)
	}
	// A worker's firewall is the platform's: another group would open it, or stay behind on a VM CAPMOX replaces.
	if IsWorkerVM(vm.Tags) {
		return errWorkerGroups()
	}
	if err := s.C.AddVMGroupRule(ctx, vm.Node, vm.VMID, groupID); err != nil {
		return err
	}
	if err := s.C.SetVMFirewallOptions(ctx, vm.Node, vm.VMID, map[string]string{"enable": "1"}); err != nil {
		return err
	}
	// NICs must carry firewall=1 or the group filters nothing (honesty over silence).
	cfg, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	if err != nil {
		return err
	}
	if params := nicFirewallParams(cfg); len(params) > 0 {
		up, err := s.C.SetVMConfig(ctx, vm.Node, vm.VMID, params)
		if err != nil {
			return err
		}
		return s.C.WaitTask(ctx, up)
	}
	return nil
}

// nicKeys returns the VM's netN config keys.
func nicKeys(cfg VMConfig) []string {
	var out []string
	for key := range cfg {
		if strings.HasPrefix(key, "net") {
			if _, err := strconv.Atoi(key[3:]); err == nil {
				out = append(out, key)
			}
		}
	}
	return out
}

// nicWithFirewall sets firewall=1 on a netN value, replacing any firewall=0 (an
// appended duplicate key is rejected by PVE).
func nicWithFirewall(val string) string {
	var out []string
	for _, p := range strings.Split(val, ",") {
		if !strings.HasPrefix(p, "firewall=") {
			out = append(out, p)
		}
	}
	return strings.Join(append(out, "firewall=1"), ",")
}

// nicFirewallParams is the config update that puts firewall=1 on every NIC
// lacking it (empty = nothing to change).
func nicFirewallParams(cfg VMConfig) map[string]string {
	params := map[string]string{}
	for _, key := range nicKeys(cfg) {
		val := cfg.Str(key)
		if val != "" && !strings.Contains(","+val+",", ",firewall=1,") {
			params[key] = nicWithFirewall(val)
		}
	}
	return params
}

// extraNICKeys lists a fresh clone's NICs beyond net0, with their ipconfigN, for
// deletion: create only plugs net0 (a public NIC comes later, through the IPAM).
// A clone inherits the source's NICs AND its firewall ipsets, so a project
// template made from a VM with a public NIC would hand every new server that
// VM's bridge plus an ipfilter allowing its public address — still usable after
// the address is released and reallocated to another tenant.
// Lab-verified (PVE 9.2): qm clone copies the source's firewall config, options, rules and ipsets.
func extraNICKeys(cfg VMConfig) []string {
	var out []string
	for _, key := range nicKeys(cfg) {
		if key == "net0" {
			continue
		}
		out = append(out, key)
		if ip := "ipconfig" + key[3:]; cfg.Str(ip) != "" {
			out = append(out, ip)
		}
	}
	slices.Sort(out)
	return out
}

// nicBridge extracts bridge=<id> from a netN value.
func nicBridge(val string) string {
	for _, p := range strings.Split(val, ",") {
		if b, ok := strings.CutPrefix(p, "bridge="); ok {
			return b
		}
	}
	return ""
}

// vmFirewallOptions is what every platform VM runs with: anti-spoof (ipfilter +
// macfilter); ingress closed except what a security group opens (default-deny
// ingress); egress open (open egress) — so
// group "out" rules restrict nothing; DHCP allowed so the SDN dnsmasq lease
// survives the DROP policy. dhcp=1 alone is not enough: proxmox-firewall checks the
// ipfilter set first, so SecureVM adds 0.0.0.0 to it for DISCOVER (lab-verified).
var vmFirewallOptions = map[string]string{
	"enable": "1", "ipfilter": "1", "macfilter": "1", "dhcp": "1",
	"policy_in": "DROP", "policy_out": "ACCEPT",
}

// SecureVM switches the VM firewall on with anti-spoofing. Every NIC on an SDN
// vnet gets ipfilter-netN = that vnet's subnets. A NIC anywhere else (a template
// or public bridge) gets no set, and PVE treats ipfilter=1 as an EMPTY set per
// NIC, so its IPv4 egress is dropped (lab-verified): fail-closed until the public-IP
// IPAM feeds SetIPFilter the allocated address.
// ponytail: on a DHCP zone the set is the vnet subnet, not the exact lease (PVE's IPAM
// assigns it, not us), so a VM can still spoof a sibling inside its own project subnet;
// a zone without DHCP pins the allocated /32 instead (CreateServer).
func (s *Service) SecureVM(ctx context.Context, node string, vmid int) error {
	cfg, err := s.C.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return err
	}
	vnets, err := s.C.ListSDNVnets(ctx)
	if err != nil {
		return err
	}
	zones, err := s.C.ListSDNZones(ctx)
	if err != nil {
		return err
	}
	dhcpZone := map[string]bool{}
	for _, z := range zones {
		dhcpZone[z.Zone] = z.DHCP != ""
	}
	vnetZone := map[string]string{}
	for _, v := range vnets {
		vnetZone[v.Vnet] = v.Zone
	}
	for _, key := range nicKeys(cfg) {
		br := nicBridge(cfg.Str(key))
		zone, isVnet := vnetZone[br]
		if !isVnet {
			continue
		}
		subs, err := s.C.ListSDNSubnets(ctx, br)
		if err != nil {
			return err
		}
		var cidrs []string
		for _, sub := range subs {
			if sub.CIDR != "" {
				cidrs = append(cidrs, sub.CIDR)
			}
		}
		if dhcpZone[zone] {
			cidrs = append(cidrs, "0.0.0.0")
		}
		if err := s.SetIPFilter(ctx, node, vmid, key, cidrs); err != nil {
			return err
		}
	}
	return s.C.SetVMFirewallOptions(ctx, node, vmid, vmFirewallOptions)
}

// SetIPFilter makes the VM's ipfilter-<nic> set hold exactly cidrs (no-op when
// it already does). A changed set is dropped and rebuilt: for that instant the
// NIC is fail-closed, never open.
func (s *Service) SetIPFilter(ctx context.Context, node string, vmid int, nic string, cidrs []string) error {
	name := "ipfilter-" + nic
	sets, err := s.C.ListVMIPSets(ctx, node, vmid)
	if err != nil {
		return err
	}
	for _, set := range sets {
		if set.Name != name {
			continue
		}
		entries, err := s.C.ListVMIPSetEntries(ctx, node, vmid, name)
		if err != nil {
			return err
		}
		have := make([]string, 0, len(entries))
		for _, e := range entries {
			have = append(have, e.CIDR)
		}
		want := make([]string, 0, len(cidrs))
		for _, c := range cidrs {
			want = append(want, strings.TrimSuffix(c, "/32")) // PVE stores a host without /32
		}
		slices.Sort(have)
		slices.Sort(want)
		if slices.Equal(have, want) {
			return nil
		}
		if err := s.C.DeleteVMIPSet(ctx, node, vmid, name); err != nil {
			return err
		}
		break
	}
	if err := s.C.CreateVMIPSet(ctx, node, vmid, name, "proxcloud anti-spoof"); err != nil {
		return err
	}
	for _, c := range cidrs {
		if err := s.C.AddVMIPSetEntry(ctx, node, vmid, name, c); err != nil {
			return err
		}
	}
	return nil
}

// DefaultSecurityGroupName is the per-project group attached when a server is
// created without an explicit choice.
const DefaultSecurityGroupName = "default"

// SecurityGroupNotFoundError marks a create-time group ref the project doesn't own.
type SecurityGroupNotFoundError struct{ Ref string }

func (e *SecurityGroupNotFoundError) Error() string { return "security group not found: " + e.Ref }

// defaultGroupRules is the default group (default-deny ingress, open egress):
// ingress from the provider CIDR pool. The cluster isolation rules already drop
// every other tenant's pool traffic, so what is left is the project's own
// networks. No SSH/ICMP from outside.
// ponytail: a pool-sourced packet spoofed onto a PUBLIC bridge also matches;
// its replies route via the vnet so TCP cannot complete. Upgrade: a per-project
// cluster IPSet of the project's subnets as the source.
func defaultGroupRules(cidrPool string) []FirewallRule {
	if cidrPool == "" {
		return nil // no pool → ingress stays fully closed
	}
	return []FirewallRule{{Type: "in", Action: "ACCEPT", Source: cidrPool, Comment: "intra-project (other tenants dropped by the isolation rules)"}}
}

// EnsureDefaultSecurityGroup returns the project's default group, creating it
// with defaultGroupRules when missing. An existing group is left untouched (the
// user may have edited it).
func (s *Service) EnsureDefaultSecurityGroup(ctx context.Context, projectID, cidrPool string) (string, error) {
	groups, err := s.C.ListFirewallGroups(ctx)
	if err != nil {
		return "", err
	}
	for _, g := range groups {
		if pid, name, ok := aliasProject(g.Comment); ok && pid == projectID && name == DefaultSecurityGroupName {
			return g.Group, nil
		}
	}
	id, err := s.CreateSecurityGroup(ctx, projectID, DefaultSecurityGroupName)
	if err != nil {
		return "", err
	}
	for _, r := range defaultGroupRules(cidrPool) {
		if err := s.C.AddFirewallRule(ctx, id, r); err != nil {
			_ = s.DeleteSecurityGroup(ctx, projectID, id) // never leave a half-built default behind
			return "", err
		}
	}
	return id, nil
}

// ServerSecurityGroups resolves a server create's group refs (ids or display
// names) to owned group ids; no refs → the project's default group.
func (s *Service) ServerSecurityGroups(ctx context.Context, projectID string, refs []string, cidrPool string) ([]string, error) {
	if len(refs) == 0 {
		id, err := s.EnsureDefaultSecurityGroup(ctx, projectID, cidrPool)
		if err != nil {
			return nil, err
		}
		return []string{id}, nil
	}
	groups, err := s.C.ListFirewallGroups(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ref := range refs {
		found := ""
		for _, g := range groups {
			pid, name, ok := aliasProject(g.Comment)
			if ok && pid == projectID && (g.Group == ref || name == ref) {
				found = g.Group
				break
			}
		}
		if found == "" {
			return nil, &SecurityGroupNotFoundError{Ref: ref}
		}
		if !slices.Contains(out, found) {
			out = append(out, found)
		}
	}
	return out, nil
}

// DetachSecurityGroup removes the VM firewall rule referencing the group.
func (s *Service) DetachSecurityGroup(ctx context.Context, projectID, serverExtID, groupID string) error {
	vm, err := s.ResolveVM(ctx, projectID, serverExtID)
	if err != nil {
		return err
	}
	if vm == nil {
		return fmt.Errorf("pve: server %s not found", serverExtID)
	}
	// A worker keeps its cluster's group. Any other group on it is from before attaching was refused, and may go.
	if IsWorkerVM(vm.Tags) {
		g, err := s.ownedGroup(ctx, projectID, groupID)
		if err != nil {
			return err
		}
		if g == nil {
			return errWorkerGroups()
		}
		if _, name, _ := aliasProject(g.Comment); IsWorkerGroupName(name) {
			return errWorkerGroups()
		}
	}
	base := qemuPath(vm.Node, vm.VMID) + "/firewall"
	var rules []FirewallRule
	if err := s.C.get(ctx, base+"/rules", nil, &rules); err != nil {
		return err
	}
	// Delete top-down so positions stay valid.
	for i := len(rules) - 1; i >= 0; i-- {
		if rules[i].Type == "group" && rules[i].Action == groupID {
			if err := s.C.delete(ctx, base+"/rules/"+strconv.Itoa(rules[i].Pos), nil, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// VMSecurityGroups lists the group ids a pool VM's firewall references.
func (s *Service) VMSecurityGroups(ctx context.Context, projectID, serverExtID string) ([]string, error) {
	vm, err := s.ResolveVM(ctx, projectID, serverExtID)
	if err != nil {
		return nil, err
	}
	if vm == nil {
		return nil, fmt.Errorf("pve: server %s not found", serverExtID)
	}
	var rules []FirewallRule
	if err := s.C.get(ctx, vmFirewallPath(vm.Node, vm.VMID)+"/rules", nil, &rules); err != nil {
		return nil, err
	}
	out := []string{}
	for _, r := range rules {
		if r.Type == "group" {
			out = append(out, r.Action)
		}
	}
	return out, nil
}

// SecurityGroupInUse names a pool VM whose firewall references the group ("" = none). PVE
// deletes a referenced group without complaint and leaves a dangling rule on the VM.
func (s *Service) SecurityGroupInUse(ctx context.Context, projectID, groupID string) (string, error) {
	return s.groupUser(ctx, projectID, groupID, "")
}

// groupUser is SecurityGroupInUse. With exceptCluster it is the platform asking before it deletes that cluster's
// group: the cluster's workers do not count, and a VM that cannot be read does not hold the delete back (attaching
// such a group to a server is refused, so only a group older than that refusal can be on one).
func (s *Service) groupUser(ctx context.Context, projectID, groupID, exceptCluster string) (string, error) {
	vms, templates, unread, err := s.poolVMs(ctx, projectID)
	if err != nil {
		return "", err
	}
	if len(unread) > 0 && exceptCluster == "" {
		return "", errUnreadVMs()
	}
	for _, vm := range append(vms, templates...) {
		if exceptCluster != "" && HasTag(vm.Tags, WorkerTagPrefix+exceptCluster) {
			continue
		}
		var rules []FirewallRule
		if err := s.C.get(ctx, vmFirewallPath(vm.Node, vm.VMID)+"/rules", nil, &rules); err != nil {
			return "", err
		}
		for _, r := range rules {
			if r.Type == "group" && r.Action == groupID {
				return vm.Name, nil
			}
		}
	}
	return "", nil
}

// clearVMRules removes every rule of a VM's firewall (bottom-up: positions stay valid).
func (s *Service) clearVMRules(ctx context.Context, node string, vmid int) error {
	base := vmFirewallPath(node, vmid) + "/rules"
	var rules []FirewallRule
	if err := s.C.get(ctx, base, nil, &rules); err != nil {
		return err
	}
	for i := len(rules) - 1; i >= 0; i-- {
		if err := s.C.delete(ctx, base+"/"+strconv.Itoa(rules[i].Pos), nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// --- SECURITY_GROUP sync provider ---

type SecurityGroupSyncProvider struct {
	svc       *Service
	region    string
	projectID string
}

func (s *Service) SecurityGroupSyncProvider(region, projectID string) *SecurityGroupSyncProvider {
	return &SecurityGroupSyncProvider{svc: s, region: region, projectID: projectID}
}

func (p *SecurityGroupSyncProvider) Type() string          { return cloud.TypeSecurityGroup }
func (p *SecurityGroupSyncProvider) ProjectID() string     { return p.projectID }
func (p *SecurityGroupSyncProvider) CompareKeys() []string { return []string{} }

func (p *SecurityGroupSyncProvider) List(ctx context.Context) ([]cloud.CloudResource, error) {
	groups, err := p.svc.C.ListFirewallGroups(ctx)
	if err != nil {
		return nil, err
	}
	now := nowUTC()
	var out []cloud.CloudResource
	for _, g := range groups {
		pid, name, ok := aliasProject(g.Comment)
		if !ok || pid != p.projectID {
			continue
		}
		rules := []any{}
		if rs, err := p.svc.C.ListFirewallRules(ctx, g.Group); err == nil {
			for _, r := range rs {
				// The remote of an inbound rule is where traffic comes from, of an outbound rule where it goes.
				remote := r.Source
				if r.Type == "out" {
					remote = r.Dest
				}
				rules = append(rules, map[string]any{
					"id":               SecurityGroupRuleID(g.Group, r),
					"direction":        map[string]string{"in": "ingress", "out": "egress"}[r.Type],
					"protocol":         r.Proto,
					"port_range":       r.DPort,
					"remote_ip_prefix": remote,
					"description":      r.Comment,
					"pos":              r.Pos,
				})
			}
		}
		out = append(out, cloud.CloudResource{
			ExternalID: g.Group,
			ProjectID:  p.projectID,
			Region:     p.region,
			Type:       cloud.TypeSecurityGroup,
			Data: map[string]any{
				"securityGroup": map[string]any{
					"id": g.Group, "name": name, "description": "",
					"security_group_rules": rules,
				},
			},
			Info: &cloud.Info{UpdatedAt: &now},
		})
	}
	return out, nil
}

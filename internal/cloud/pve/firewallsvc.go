package pve

// firewallsvc.go — SECURITY_GROUP over cluster firewall groups. Group ids are
// opaque ("g"+random base36 — PVE caps group names hard) and ownership rides the
// COMMENT ("st:<projectID>:<display name>", the vnet-alias discipline) because
// groups have no pool scoping. VM attach = a per-VM firewall rule referencing the
// group + the VM firewall switched on.

import (
	"context"
	"fmt"
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
		if err := s.C.DeleteFirewallRule(ctx, groupID, rules[i].Pos); err != nil {
			return err
		}
	}
	return s.C.DeleteFirewallGroup(ctx, groupID)
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

// SGDeleteRule removes the rule at pos of an owned group.
func (s *Service) SGDeleteRule(ctx context.Context, projectID, groupID string, pos int) error {
	g, err := s.ownedGroup(ctx, projectID, groupID)
	if err != nil {
		return err
	}
	if g == nil {
		return fmt.Errorf("pve: group %s not found", groupID)
	}
	return s.C.DeleteFirewallRule(ctx, groupID, pos)
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
	vm, err := s.ResolveVM(ctx, projectID, serverExtID)
	if err != nil {
		return err
	}
	if vm == nil {
		return fmt.Errorf("pve: server %s not found", serverExtID)
	}
	base := qemuPath(vm.Node, vm.VMID) + "/firewall"
	form := formVals{"type": "group", "action": groupID, "enable": "1"}.values()
	if err := s.C.post(ctx, base+"/rules", form, nil); err != nil {
		return err
	}
	if err := s.C.put(ctx, base+"/options", formVals{"enable": "1"}.values(), nil); err != nil {
		return err
	}
	// NICs must carry firewall=1 or the group filters nothing (honesty over silence).
	cfg, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	if err != nil {
		return err
	}
	params := map[string]string{}
	for key := range cfg {
		if strings.HasPrefix(key, "net") {
			if _, err := strconv.Atoi(key[3:]); err != nil {
				continue
			}
			val := cfg.Str(key)
			if val != "" && !strings.Contains(val, "firewall=1") {
				params[key] = val + ",firewall=1"
			}
		}
	}
	if len(params) > 0 {
		up, err := s.C.SetVMConfig(ctx, vm.Node, vm.VMID, params)
		if err != nil {
			return err
		}
		return s.C.WaitTask(ctx, up)
	}
	return nil
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
				rules = append(rules, map[string]any{
					"id":               g.Group + "-" + strconv.Itoa(r.Pos),
					"direction":        map[string]string{"in": "ingress", "out": "egress"}[r.Type],
					"protocol":         r.Proto,
					"port_range":       r.DPort,
					"remote_ip_prefix": r.Source,
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

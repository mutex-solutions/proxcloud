package pve

import (
	"context"
	"net/url"
	"slices"
)

// Cluster firewall security groups (GET /cluster/firewall/groups). No pool scoping
// exists — tenancy is the platform's per-project name prefix + DB ownership rows.

// FirewallGroup is one row of GET /cluster/firewall/groups.
type FirewallGroup struct {
	Group   string `json:"group"`
	Comment string `json:"comment"`
	Digest  string `json:"digest"`
}

func (c *Client) ListFirewallGroups(ctx context.Context) ([]FirewallGroup, error) {
	var out []FirewallGroup
	if err := c.get(ctx, "/cluster/firewall/groups", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateFirewallGroup creates a security group (synchronous).
func (c *Client) CreateFirewallGroup(ctx context.Context, group, comment string) error {
	form := formVals{"group": group, "comment": comment}.values()
	return c.post(ctx, "/cluster/firewall/groups", form, nil)
}

func (c *Client) DeleteFirewallGroup(ctx context.Context, group string) error {
	err := c.delete(ctx, "/cluster/firewall/groups/"+url.PathEscape(group), nil, nil)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}

// FirewallRule is one rule of a group (GET /cluster/firewall/groups/{group}).
type FirewallRule struct {
	Pos     int    `json:"pos"`
	Type    string `json:"type"`   // "in" | "out" | "group"
	Action  string `json:"action"` // "ACCEPT" | "DROP" | "REJECT"
	Proto   string `json:"proto"`
	Source  string `json:"source"`
	Dest    string `json:"dest"`
	DPort   string `json:"dport"`
	SPort   string `json:"sport"`
	Enable  int    `json:"enable"`
	Comment string `json:"comment"`
}

func (c *Client) ListFirewallRules(ctx context.Context, group string) ([]FirewallRule, error) {
	var out []FirewallRule
	if err := c.get(ctx, "/cluster/firewall/groups/"+url.PathEscape(group), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AddFirewallRule appends a rule to a group.
func (c *Client) AddFirewallRule(ctx context.Context, group string, r FirewallRule) error {
	return c.post(ctx, "/cluster/firewall/groups/"+url.PathEscape(group), ruleForm(r), nil)
}

// ruleForm is the create-rule body shared by group and vnet rules.
func ruleForm(r FirewallRule) url.Values {
	return formVals{
		"type":    r.Type,
		"action":  r.Action,
		"proto":   r.Proto,
		"source":  r.Source,
		"dest":    r.Dest,
		"dport":   r.DPort,
		"sport":   r.SPort,
		"comment": r.Comment,
		"enable":  "1",
	}.values()
}

// DeleteFirewallRule removes the rule at pos.
func (c *Client) DeleteFirewallRule(ctx context.Context, group string, pos int) error {
	return c.delete(ctx, "/cluster/firewall/groups/"+url.PathEscape(group)+"/"+itoa0(pos), nil, nil)
}

// itoa0 is itoa that keeps "0" (rule positions are 0-based).
func itoa0(n int) string {
	if n == 0 {
		return "0"
	}
	return itoa(n)
}

// --- SDN vnet firewall (GET/POST /cluster/sdn/vnets/{vnet}/firewall/rules, PUT .../options) ---
// Vnet rules are FORWARD-only and see every packet crossing the vnet bridge,
// including VM <-> gateway (host) traffic. They are NOT SDN-staged: they take
// effect immediately, no PUT /cluster/sdn needed.
// The stock iptables pve-firewall IGNORES vnet rules (pve-firewall docs): every
// node must run the nftables proxmox-firewall (host.fw `nftables: 1`) with the
// datacenter firewall enabled — test-connection checks both.

func vnetFirewallPath(vnet string) string {
	return "/cluster/sdn/vnets/" + url.PathEscape(vnet) + "/firewall"
}

func (c *Client) ListVnetFirewallRules(ctx context.Context, vnet string) ([]FirewallRule, error) {
	var out []FirewallRule
	if err := c.get(ctx, vnetFirewallPath(vnet)+"/rules", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AddVnetFirewallRule creates a rule. PVE inserts new rules at the TOP (pos 0).
// TODO(verify-lab): confirm the prepend on PVE 9 (RulesBase create_rule unshift).
func (c *Client) AddVnetFirewallRule(ctx context.Context, vnet string, r FirewallRule) error {
	return c.post(ctx, vnetFirewallPath(vnet)+"/rules", ruleForm(r), nil)
}

func (c *Client) DeleteVnetFirewallRule(ctx context.Context, vnet string, pos int) error {
	return c.delete(ctx, vnetFirewallPath(vnet)+"/rules/"+itoa0(pos), nil, nil)
}

// EnableVnetFirewall switches the vnet's rules on (options enable defaults to 0).
// policy_forward is left at its ACCEPT default so SNAT egress keeps working.
func (c *Client) EnableVnetFirewall(ctx context.Context, vnet string) error {
	return c.put(ctx, vnetFirewallPath(vnet)+"/options", formVals{"enable": "1"}.values(), nil)
}

// --- datacenter + VM firewall (/cluster/firewall/options, /nodes/{node}/qemu/{vmid}/firewall) ---

// DatacenterFirewallEnabled reads the cluster-wide master switch. Off (the PVE
// default) means no group, VM or vnet rule is enforced anywhere.
func (c *Client) DatacenterFirewallEnabled(ctx context.Context) (bool, error) {
	var out struct {
		Enable int `json:"enable"`
	}
	if err := c.get(ctx, "/cluster/firewall/options", nil, &out); err != nil {
		return false, err
	}
	return out.Enable >= 1, nil
}

// NodesWithoutNFTables lists the online nodes whose host firewall is not the
// nftables proxmox-firewall (host.fw `nftables: 1`): the stock iptables
// pve-firewall ignores every vnet rule, so tenant isolation would silently be off
// there. Offline nodes cannot be asked and are skipped.
// TODO(verify-lab): GET /nodes/{node}/firewall/options reports nftables as 0/1
// on PVE 9 (absent = 0).
func (c *Client) NodesWithoutNFTables(ctx context.Context) ([]string, error) {
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range nodes {
		if n.Status != "online" {
			continue
		}
		var opts struct {
			NFTables int `json:"nftables"`
		}
		if err := c.get(ctx, "/nodes/"+url.PathEscape(n.Node)+"/firewall/options", nil, &opts); err != nil {
			return nil, err
		}
		if opts.NFTables < 1 {
			out = append(out, n.Node)
		}
	}
	slices.Sort(out)
	return out, nil
}

func vmFirewallPath(node string, vmid int) string { return qemuPath(node, vmid) + "/firewall" }

// SetVMFirewallOptions PUTs VM firewall options (synchronous).
func (c *Client) SetVMFirewallOptions(ctx context.Context, node string, vmid int, opts map[string]string) error {
	return c.put(ctx, vmFirewallPath(node, vmid)+"/options", formVals(opts).values(), nil)
}

// AddVMGroupRule references a security group from the VM's rules.
func (c *Client) AddVMGroupRule(ctx context.Context, node string, vmid int, group string) error {
	form := formVals{"type": "group", "action": group, "enable": "1"}.values()
	return c.post(ctx, vmFirewallPath(node, vmid)+"/rules", form, nil)
}

// IPSet is one row of GET .../firewall/ipset (or one entry of a set: CIDR).
type IPSet struct {
	Name string `json:"name"`
	CIDR string `json:"cidr"`
}

func (c *Client) ListVMIPSets(ctx context.Context, node string, vmid int) ([]IPSet, error) {
	var out []IPSet
	if err := c.get(ctx, vmFirewallPath(node, vmid)+"/ipset", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) ListVMIPSetEntries(ctx context.Context, node string, vmid int, name string) ([]IPSet, error) {
	var out []IPSet
	if err := c.get(ctx, vmFirewallPath(node, vmid)+"/ipset/"+url.PathEscape(name), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) CreateVMIPSet(ctx context.Context, node string, vmid int, name, comment string) error {
	form := formVals{"name": name, "comment": comment}.values()
	return c.post(ctx, vmFirewallPath(node, vmid)+"/ipset", form, nil)
}

// DeleteVMIPSet drops a set with its entries (force) — entry deletes would need
// the CIDR's "/" in the path.
func (c *Client) DeleteVMIPSet(ctx context.Context, node string, vmid int, name string) error {
	return c.delete(ctx, vmFirewallPath(node, vmid)+"/ipset/"+url.PathEscape(name), formVals{"force": "1"}.values(), nil)
}

func (c *Client) AddVMIPSetEntry(ctx context.Context, node string, vmid int, name, cidr string) error {
	return c.post(ctx, vmFirewallPath(node, vmid)+"/ipset/"+url.PathEscape(name), formVals{"cidr": cidr}.values(), nil)
}

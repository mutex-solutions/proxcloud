package pve

import (
	"context"
	"net/url"
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
	form := formVals{
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
	return c.post(ctx, "/cluster/firewall/groups/"+url.PathEscape(group), form, nil)
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

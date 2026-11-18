package pve

import (
	"context"
	"strings"
)

// apiAccessComment marks the cluster firewall rule that lets a managed-services installation's platform network reach
// the Proxmox API. Its own marker, not isolationComment: no isolation rebuild or project sync reads or deletes it.
const apiAccessComment = "proxcloud:pve-api:"

// APIAccessComment is the comment of installationID's rule.
func APIAccessComment(installationID string) string { return apiAccessComment + installationID }

// APIAccessRule is the rule EnsureAPIAccess keeps: tcp from sources (addresses and CIDRs) to dest (an address; "" is
// any address of a node, for an API URL that names a host) on port.
func APIAccessRule(installationID string, sources []string, dest, port string) FirewallRule {
	return FirewallRule{Type: "in", Action: "ACCEPT", Proto: "tcp", Source: strings.Join(sources, ","), Dest: dest, DPort: port,
		Comment: APIAccessComment(installationID), Enable: 1}
}

// EnsureAPIAccess keeps exactly one rule of installationID in the cluster firewall: the one APIAccessRule names. A
// rule with the marker that differs (the settings changed) is replaced, the new one first. It reports whether it
// wrote anything. The rule is written whether or not the datacenter firewall is on: while it is off the rule does
// nothing, and the day it is turned on the platform keeps its path to the API.
// It needs Sys.Modify on /, which the portal token holds for the isolation rules (docs/proxmox-permissions.md).
func (s *Service) EnsureAPIAccess(ctx context.Context, installationID string, sources []string, dest, port string) (bool, error) {
	want := APIAccessRule(installationID, sources, dest, port)
	rules, err := s.C.ListClusterFirewallRules(ctx)
	if err != nil {
		return false, err
	}
	var have bool
	var stale []FirewallRule
	for _, r := range rules {
		switch {
		case r.Comment != want.Comment:
		case !have && sameLiveRule(r, want):
			have = true
		default:
			stale = append(stale, r)
		}
	}
	if !have {
		if err := s.C.AddClusterFirewallRule(ctx, want); err != nil {
			return false, err
		}
	}
	for _, r := range stale {
		if err := s.deleteClusterRule(ctx, r); err != nil {
			return true, err
		}
	}
	return !have || len(stale) > 0, nil
}

// RemoveAPIAccess deletes every rule of installationID and answers how many it deleted.
func (s *Service) RemoveAPIAccess(ctx context.Context, installationID string) (int, error) {
	rules, err := s.C.ListClusterFirewallRules(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rules {
		if r.Comment != APIAccessComment(installationID) {
			continue
		}
		if err := s.deleteClusterRule(ctx, r); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// HasAPIAccess reports whether the cluster firewall holds the rule APIAccessRule names, enabled.
func (s *Service) HasAPIAccess(ctx context.Context, installationID string, sources []string, dest, port string) (bool, error) {
	rules, err := s.C.ListClusterFirewallRules(ctx)
	if err != nil {
		return false, err
	}
	want := APIAccessRule(installationID, sources, dest, port)
	for _, r := range rules {
		if r.Comment == want.Comment && sameLiveRule(r, want) {
			return true, nil
		}
	}
	return false, nil
}

package pveincident

import (
	"errors"
	"sort"
	"time"
)

var (
	ErrInvalid   = errors.New("invalid incident observation")
	ErrThrottled = errors.New("incident observation interval not elapsed")
	ErrNotFound  = errors.New("incident not found")
)

func validID(s string) bool {
	if len(s) == 0 || len(s) > 160 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
func validTrust(t Trust) bool {
	return t == Verified || t == Untrusted || t == Unreachable || t == Malformed
}
func validKind(k string) bool {
	return k == KindVisibility || k == KindProvider || k == KindNode || k == KindProtection
}
func subject(p ProtectionFact) string { return p.ServerID + ":" + p.InstanceID }

// classify never treats a missing object as healthy/offline. Only explicit facts
// can progress node/protection evidence. All reasons are fixed codes, never upstream strings.
func classify(x SnapshotResult, now time.Time) ([]observation, error) {
	tr := x.Trust
	if !validTrust(tr.Provider) || !validTrust(tr.Nodes) || !validTrust(tr.VMs) || !validTrust(tr.Protection) || len(x.Nodes) > 4096 || len(x.Protection) > 10000 || len(x.Links) > 10000 {
		return nil, ErrInvalid
	}
	provider := observation{KindProvider, "provider", "provider_verified", 1, tr.Provider}
	if tr.Provider == Unreachable {
		provider.health, provider.reason = -1, "provider_unreachable"
	} else if tr.Provider != Verified {
		provider.health, provider.reason = 0, "provider_visibility_unknown"
	}
	visibility := observation{KindVisibility, "provider", "visibility_verified", 1, Verified}
	for _, t := range []Trust{tr.Provider, tr.Nodes, tr.VMs, tr.Protection} {
		if t == Untrusted || t == Malformed {
			visibility.health, visibility.reason, visibility.quality = -1, "visibility_untrusted", t
			break
		}
		if t == Unreachable {
			visibility.health, visibility.reason, visibility.quality = 0, "visibility_unreachable", t
		}
	}
	out := []observation{provider, visibility}
	seen := map[string]bool{}
	for _, n := range x.Nodes {
		if !validID(n.NodeID) || seen["node:"+n.NodeID] {
			return nil, ErrInvalid
		}
		seen["node:"+n.NodeID] = true
		o := observation{KindNode, n.NodeID, "node_unknown", 0, tr.Nodes}
		if tr.Nodes == Verified {
			switch n.Status {
			case Online:
				o.health, o.reason = 1, "node_online"
			case Offline:
				o.health, o.reason = -1, "node_observed_offline"
			case NodeUnknown:
				o.quality = Unreachable
			default:
				o.quality, o.reason = Malformed, "node_malformed"
			}
		}
		out = append(out, o)
	}
	for _, p := range x.Protection {
		if !validID(p.ServerID) || !validID(p.InstanceID) || seen["protection:"+subject(p)] {
			return nil, ErrInvalid
		}
		seen["protection:"+subject(p)] = true
		o := observation{KindProtection, subject(p), "protection_unknown", 0, tr.Protection}
		if tr.Protection == Verified && tr.VMs == Verified {
			switch {
			case p.FailCount < 0 || p.LastSync.After(now):
				o.quality, o.reason = Malformed, "replication_malformed"
			case !p.Enabled:
				o.health, o.reason = 1, "protection_disabled"
			case p.Mode == "shared":
				o.health, o.reason = 1, "shared_ha_present"
				if !p.HA {
					o.health, o.reason = -1, "shared_ha_missing"
				}
			case p.Mode == "replicated":
				switch {
				case p.LastSync.IsZero():
					o.reason = "initial_syncing"
				case p.FailCount > 0:
					o.health, o.reason = -1, "replication_failed"
				case !p.HA:
					o.health, o.reason = -1, "replicated_ha_missing"
				case now.Sub(p.LastSync) > 30*time.Minute:
					o.health, o.reason = -1, "replication_stale"
				default:
					o.health, o.reason = 1, "protection_active"
				}
			default:
				o.quality, o.reason = Malformed, "protection_malformed"
			}
		}
		out = append(out, o)
	}
	seen = map[string]bool{}
	for _, l := range x.Links {
		if !validID(l.ServerID) || !validID(l.ProjectID) || !validID(l.InstanceID) || !validID(l.NodeID) || seen[l.ServerID] {
			return nil, ErrInvalid
		}
		seen[l.ServerID] = true
	}
	sort.Slice(out, func(i, j int) bool { return out[i].kind+out[i].subject < out[j].kind+out[j].subject })
	return out, nil
}

package platformlife

// network.go — the platform network of mode routed (spec.network.mode): a vnet ProxCloud makes in a simple SDN zone,
// with one subnet whose gateway the node holds. A tenant's packet to the platform is then routed inside the node and
// never translated on the way: the zone's SNAT rule matches only what leaves through the node's uplink
// (docs/managed-services.md, "The path from tenants to the platform"). Nothing here was run on a lab.
//
// SNAT is on for the subnet: it is the platform machines' way out (image pulls) without a route on the site's router.
// What comes in from outside the node (the ProxCloud host to the seed and the API address) still needs the route
// "platform network via the node's address", which no check can read before the seed boots.

import (
	"context"
	"fmt"
	"slices"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// CodePlatformNetwork: the routed platform network could not be made or removed, or its vnet name is taken.
const CodePlatformNetwork = "platform_network_refused"

// networkAlias marks the vnet of a routed platform network as the installation's own.
func networkAlias(id string) string { return "proxcloud-platform:" + id }

// sdnLocked runs fn under the site's SDN apply lock (Runner.SDNLock; none in tests). busy: another change holds it.
func (r *Runner) sdnLocked(ctx context.Context, fn func() error) (busy bool, err error) {
	if r.SDNLock == nil {
		return false, fn()
	}
	release, ok, err := r.SDNLock(ctx)
	if err != nil || !ok {
		return !ok, err
	}
	defer release()
	return false, fn()
}

// ensureNetwork is the first step of stage 3 for a routed network: the zone (limited to the one node), the vnet and
// the subnet, applied. It writes nothing when all three exist and none of them is pending, and never touches a vnet
// that is not this installation's. An SDN apply is cluster-wide: it also applies what anyone else has staged, as a
// tenant network's apply does.
func (x *acq) ensureNetwork(ctx context.Context) error {
	n := x.in.Spec.Network
	if !n.Routed() {
		return nil
	}
	c := x.portal
	vnets, err := c.ListSDNVnets(ctx)
	if err != nil {
		return classify(CodePlatformNetwork, err)
	}
	vi := slices.IndexFunc(vnets, func(v pve.SDNVnet) bool { return v.Vnet == n.Bridge })
	if vi >= 0 && vnets[vi].Alias != networkAlias(x.in.ID) {
		return permanent(CodePlatformNetwork, "Proxmox already has a vnet %s (zone %s, alias %q) that this installation did not make: name another vnet in the settings, or use mode bridge to install on it.",
			n.Bridge, vnets[vi].Zone, vnets[vi].Alias)
	}
	zones, err := c.ListSDNZones(ctx)
	if err != nil {
		return classify(CodePlatformNetwork, err)
	}
	zi := slices.IndexFunc(zones, func(z pve.SDNZone) bool { return z.Zone == RoutedZone })
	if zi >= 0 && zones[zi].Type != "simple" {
		return permanent(CodePlatformNetwork, "The SDN zone %s exists and is a %s zone: a routed platform network needs it as a simple zone. Remove or rename that zone.", RoutedZone, zones[zi].Type)
	}
	// Pending: one of the installation's own objects is staged and not applied (a run that stopped before its apply).
	// What someone else staged is not a reason to apply.
	pending := zi >= 0 && zones[zi].State != "" || vi >= 0 && vnets[vi].State != ""
	var subnet bool
	if vi >= 0 {
		subs, err := c.ListSDNSubnets(ctx, n.Bridge)
		if err != nil {
			return classify(CodePlatformNetwork, err)
		}
		si := slices.IndexFunc(subs, func(s pve.SDNSubnet) bool { return s.CIDR == n.Prefix && s.Gateway == n.Gateway })
		subnet = si >= 0
		pending = pending || subnet && subs[si].State != ""
		if !subnet && len(subs) > 0 {
			return permanent(CodePlatformNetwork, "The vnet %s has a subnet that is not %s with gateway %s: delete it in Proxmox (Datacenter, SDN), then Retry.", n.Bridge, n.Prefix, n.Gateway)
		}
	}
	if zi >= 0 && vi >= 0 && subnet && !pending {
		return nil
	}
	if err := x.check(ctx); err != nil {
		return err
	}
	busy, err := x.r.sdnLocked(ctx, func() error {
		if zi < 0 {
			if err := c.CreateSDNZone(ctx, RoutedZone, "simple", map[string]string{"ipam": "pve", "nodes": x.in.Spec.Seed.Node}); err != nil {
				return fmt.Errorf("zone %s: %w", RoutedZone, err)
			}
		}
		if vi < 0 {
			if err := c.CreateSDNVnet(ctx, n.Bridge, RoutedZone, networkAlias(x.in.ID), 0); err != nil {
				return fmt.Errorf("vnet %s: %w", n.Bridge, err)
			}
		}
		if !subnet {
			if err := c.CreateSDNSubnet(ctx, n.Bridge, pve.SubnetOpts{CIDR: n.Prefix, Gateway: n.Gateway, SNAT: true}); err != nil {
				return fmt.Errorf("subnet %s: %w", n.Prefix, err)
			}
		}
		return (&pve.Service{C: c}).ApplyAndWait(ctx)
	})
	if busy {
		return waiting(CodePlatformNetwork, "Another network change is being applied on this site; the platform network is made after it.")
	}
	if err != nil {
		return classify(CodePlatformNetwork, err)
	}
	return nil
}

// removeNetwork deletes the vnet and subnet of a routed platform network, and the zone when no vnet is left in it. It
// answers the item a Remove shows. A vnet that is not this installation's stays.
func (x *acq) removeNetwork(ctx context.Context) (*StageItem, error) {
	n := x.in.Spec.Network
	if !n.Routed() {
		return nil, nil
	}
	item := func(state, msg string) *StageItem { return &StageItem{ID: "network", State: state, Message: msg} }
	c := x.portal
	vnets, err := c.ListSDNVnets(ctx)
	if err != nil {
		return nil, classify(CodePlatformNetwork, err)
	}
	vi := slices.IndexFunc(vnets, func(v pve.SDNVnet) bool { return v.Vnet == n.Bridge })
	if vi < 0 {
		return item("gone", fmt.Sprintf("Proxmox has no vnet %s.", n.Bridge)), nil
	}
	if vnets[vi].Alias != networkAlias(x.in.ID) {
		return item("left", fmt.Sprintf("The vnet %s is not this installation's (alias %q): not deleted.", n.Bridge, vnets[vi].Alias)), nil
	}
	zone := vnets[vi].Zone
	last := !slices.ContainsFunc(vnets, func(v pve.SDNVnet) bool { return v.Zone == zone && v.Vnet != n.Bridge })
	busy, err := x.r.sdnLocked(ctx, func() error {
		subs, err := c.ListSDNSubnets(ctx, n.Bridge)
		if err != nil {
			return err
		}
		for _, s := range subs {
			if err := c.DeleteSDNSubnet(ctx, n.Bridge, s.Subnet); err != nil {
				return err
			}
		}
		if err := c.DeleteSDNVnet(ctx, n.Bridge); err != nil {
			return err
		}
		if last && zone == RoutedZone {
			if err := c.DeleteSDNZone(ctx, zone); err != nil {
				return err
			}
		}
		return (&pve.Service{C: c}).ApplyAndWait(ctx)
	})
	if busy {
		return nil, waiting(CodePlatformNetwork, "Another network change is being applied on this site; the platform network is removed after it.")
	}
	if err != nil {
		return nil, classify(CodePlatformNetwork, err)
	}
	return item("gone", fmt.Sprintf("The vnet %s and its subnet %s were deleted, and SDN was applied.", n.Bridge, n.Prefix)), nil
}

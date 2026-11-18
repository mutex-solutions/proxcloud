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
	"strings"

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

// ifaceTaken refuses a vnet name that an interface of a node already holds (vmbr1, bond0, eno1: the vnet id's pattern
// admits them). An SDN apply would write a second stanza for that interface, which can detach a live bridge from its
// uplink. Whether Proxmox itself refuses such a vnet id is not known (no lab test), so nothing here depends on it.
func ifaceTaken(ctx context.Context, c *pve.Client, vnet string) (string, error) {
	nodes, err := c.NodesWithIface(ctx, vnet)
	if err != nil || len(nodes) == 0 {
		return "", err
	}
	return fmt.Sprintf("The name %s is an interface of %s: a vnet of that name would take the interface over. Name a vnet no node has (pcplat0), or use mode bridge to install on %s.",
		vnet, strings.Join(nodes, ", "), vnet), nil
}

// ensureNetwork is the first step of stage 3 for a routed network: the zone (limited to the one node), the vnet and
// the subnet, applied. It writes nothing when all three exist and are applied, and never touches a vnet that is not
// this installation's. An SDN apply is cluster-wide: it also applies what anyone else has staged, as a tenant
// network's apply does.
//
// Applied means: the seed's node lists the vnet among its bridges, which is what the seed's NIC needs. The lists of
// zones and vnets are no proof: Proxmox reports an object's pending state only when asked with pending=1 (not checked
// on a lab), so a run that stopped between the create and the apply would look finished. When the node's bridges
// cannot say (the read fails, or the token does not see the vnet there) SDN is applied once more, which changes
// nothing that is applied already.
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
	// A vnet this installation made under another name (the settings were renamed after a failed install) holds the
	// subnet: a second vnet could not have it.
	if oi := slices.IndexFunc(vnets, func(v pve.SDNVnet) bool { return v.Alias == networkAlias(x.in.ID) && v.Vnet != n.Bridge }); oi >= 0 {
		return permanent(CodePlatformNetwork, "This installation already made the vnet %s, and the settings now name %s: name %s again, or Remove the installation, which deletes that vnet.",
			vnets[oi].Vnet, n.Bridge, vnets[oi].Vnet)
	}
	if vi < 0 {
		taken, err := ifaceTaken(ctx, c, n.Bridge)
		if err != nil {
			return classify(CodePlatformNetwork, err)
		}
		if taken != "" {
			return permanent(CodePlatformNetwork, "%s", taken)
		}
	}
	zones, err := c.ListSDNZones(ctx)
	if err != nil {
		return classify(CodePlatformNetwork, err)
	}
	zi := slices.IndexFunc(zones, func(z pve.SDNZone) bool { return z.Zone == RoutedZone })
	if zi >= 0 && zones[zi].Type != "simple" {
		return permanent(CodePlatformNetwork, "The SDN zone %s exists and is a %s zone: a routed platform network needs it as a simple zone. Remove or rename that zone.", RoutedZone, zones[zi].Type)
	}
	var subnet, applied bool
	if vi >= 0 {
		subs, err := c.ListSDNSubnets(ctx, n.Bridge)
		if err != nil {
			return classify(CodePlatformNetwork, err)
		}
		si := slices.IndexFunc(subs, func(s pve.SDNSubnet) bool { return s.CIDR == n.Prefix && s.Gateway == n.Gateway })
		subnet = si >= 0
		if !subnet && len(subs) > 0 {
			return permanent(CodePlatformNetwork, "The vnet %s has a subnet that is not %s with gateway %s: delete it in Proxmox (Datacenter, SDN), then Retry.", n.Bridge, n.Prefix, n.Gateway)
		}
	}
	if zi >= 0 && vi >= 0 && subnet {
		bridges, _ := c.NodeBridges(ctx)
		applied = slices.ContainsFunc(bridges, func(b pve.NodeBridge) bool { return b.Node == x.in.Spec.Seed.Node && b.Iface == n.Bridge })
	}
	if applied {
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

// removeNetwork deletes every vnet that carries this installation's alias, with its subnets, and the zone pcplat when
// no other vnet is left in it. It looks for the alias whatever the settings say now: a draft whose install failed
// after stage 3 may have been renamed or switched to mode bridge since, and its vnet must not stay. It answers the
// item a Remove shows; nil when the network is not routed and nothing of this installation's is found.
func (x *acq) removeNetwork(ctx context.Context) (*StageItem, error) {
	n := x.in.Spec.Network
	item := func(state, msg string) *StageItem { return &StageItem{ID: "network", State: state, Message: msg} }
	c := x.portal
	vnets, err := c.ListSDNVnets(ctx)
	if err != nil {
		if !n.Routed() {
			return nil, nil // a bridge network made no vnet; a site without SDN rights for this read removes as before
		}
		return nil, classify(CodePlatformNetwork, err)
	}
	var mine []string
	for _, v := range vnets {
		if v.Alias == networkAlias(x.in.ID) {
			mine = append(mine, v.Vnet)
		}
	}
	if len(mine) == 0 {
		if !n.Routed() {
			return nil, nil
		}
		if vi := slices.IndexFunc(vnets, func(v pve.SDNVnet) bool { return v.Vnet == n.Bridge }); vi >= 0 {
			return item("left", fmt.Sprintf("The vnet %s is not this installation's (alias %q): not deleted.", n.Bridge, vnets[vi].Alias)), nil
		}
		return item("gone", fmt.Sprintf("Proxmox has no vnet %s.", n.Bridge)), nil
	}
	// The zone goes when it is the one ProxCloud made and no vnet but this installation's is in it.
	dropZone := !slices.ContainsFunc(vnets, func(v pve.SDNVnet) bool { return v.Zone == RoutedZone && !slices.Contains(mine, v.Vnet) }) &&
		slices.ContainsFunc(vnets, func(v pve.SDNVnet) bool { return v.Zone == RoutedZone && slices.Contains(mine, v.Vnet) })
	busy, err := x.r.sdnLocked(ctx, func() error {
		for _, vnet := range mine {
			subs, err := c.ListSDNSubnets(ctx, vnet)
			if err != nil {
				return err
			}
			for _, s := range subs {
				if err := c.DeleteSDNSubnet(ctx, vnet, s.Subnet); err != nil {
					return err
				}
			}
			if err := c.DeleteSDNVnet(ctx, vnet); err != nil {
				return err
			}
		}
		if dropZone {
			if err := c.DeleteSDNZone(ctx, RoutedZone); err != nil {
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
	return item("gone", fmt.Sprintf("The vnet %s and its subnets were deleted, and SDN was applied.", strings.Join(mine, ", "))), nil
}

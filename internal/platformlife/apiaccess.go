package platformlife

// apiaccess.go — the one cluster firewall rule of an installation: the seed and the platform machines open the
// Proxmox API (CAPMOX from the seed, proxmox-csi and the cloud controller from the node range). With the datacenter
// firewall on, a node drops what does not come from its own network; an operator added the rule by hand on the first
// production install. It is needed in mode routed as well: the node holds the gateway of the vnet, but Proxmox's
// default management set is the network of the node's own address only (read in pve-firewall's documentation, not
// tried on a lab).

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// APIAccess is what the rule names: as sources the seed's address and every cluster's node range as CIDRs (never the
// MetalLB range: nothing there calls Proxmox), as destination the host of spec.pveApiUrl when it is an address ("" for
// a name, which the firewall cannot hold), and the URL's port.
//
// The sources are whole node ranges because every machine of a cluster is a platform machine today. A sandbox node
// group on the platform cluster (the shared sandbox layout, a later release) must not reach the Proxmox API: it will
// take its addresses from an optional range of its own on the node group, and that range is then left out of these
// sources and fenced as a sandbox network, not as a platform one (Unfenced). An optional field, so a saved
// installation reads as before.
func (in *Installation) APIAccess() (sources []string, dest, port string) {
	prefix, _ := netip.ParsePrefix(in.Spec.Network.Prefix)
	if a, err := netip.ParseAddr(in.Spec.Seed.Address); err == nil {
		sources = append(sources, netip.PrefixFrom(a, 32).String())
	}
	for _, c := range in.Spec.Clusters {
		if r, ok := parseRange("", c.NodeRange, prefix, func(string, ...any) {}); ok {
			for _, p := range rangeCIDRs(r.first, r.last) {
				sources = append(sources, p.String())
			}
		}
	}
	port = "443"
	if u, err := url.Parse(in.Spec.PVEAPIURL); err == nil {
		if a, err := netip.ParseAddr(u.Hostname()); err == nil && a.Is4() {
			dest = a.String()
		}
		if u.Port() != "" {
			port = u.Port()
		}
	}
	return sources, dest, port
}

// ensureAPIAccess writes the rule, or replaces one written for other settings. Every install and apply comes here,
// so a rule deleted by hand comes back.
func (x *acq) ensureAPIAccess(ctx context.Context) error {
	sources, dest, port := x.in.APIAccess()
	if len(sources) == 0 {
		return nil
	}
	changed, err := (&pve.Service{C: x.portal}).EnsureAPIAccess(ctx, x.in.ID, sources, dest, port)
	if err != nil {
		return classify(CodePlatformNetwork, fmt.Errorf("the cluster firewall rule that lets the platform network reach the Proxmox API: %w", err))
	}
	if !changed {
		return nil
	}
	to := dest
	if to == "" {
		to = "the nodes"
	}
	return x.note(ctx, StageItem{ID: "pve-api-rule", State: "written", Message: fmt.Sprintf("Cluster firewall rule %q: ACCEPT tcp %s from %s to %s.",
		pve.APIAccessComment(x.in.ID), port, strings.Join(sources, ", "), to)})
}

// removeAPIAccess deletes the rule and answers the item a Remove shows (nil when there was none).
func (x *acq) removeAPIAccess(ctx context.Context) (*StageItem, error) {
	n, err := (&pve.Service{C: x.portal}).RemoveAPIAccess(ctx, x.in.ID)
	if err != nil {
		return nil, classify(CodePlatformNetwork, fmt.Errorf("the cluster firewall rule %q: %w", pve.APIAccessComment(x.in.ID), err))
	}
	if n == 0 {
		return nil, nil
	}
	return &StageItem{ID: "pve-api-rule", State: "gone", Message: fmt.Sprintf("The cluster firewall rule %q was deleted.", pve.APIAccessComment(x.in.ID))}, nil
}

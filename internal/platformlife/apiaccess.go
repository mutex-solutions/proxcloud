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
// The sources are whole node ranges because every machine in a cluster's node range is a platform machine. The
// sandbox machines (spec.sandbox, sandbox.go) must not reach the Proxmox API: they take their addresses from a range of
// their own, spec.sandbox.nodeRange, which Validate keeps apart from every cluster's node range. That range is in
// neither these sources nor Unfenced: the provider fences it as a sandbox network, not as a platform one.
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

// ListAPIAccess adds the rule to the plan of an installed platform when the cluster firewall does not hold it as
// APIAccess names it: the next apply writes it in stage 3 (ensureAPIAccess), and so does an upgrade run, so both lists
// name it. A platform installed before v0.9.0 never had the rule. Nothing is added when the rule is in place, when the
// settings name no source, or when the rules cannot be read (the Check row pve-api-rule says so, and the run decides).
func (p *Plan) ListAPIAccess(ctx context.Context, in *Installation, portal *pve.Service) {
	sources, dest, port := in.APIAccess()
	if !in.Installed() || len(sources) == 0 {
		return
	}
	if has, err := portal.HasAPIAccess(ctx, in.ID, sources, dest, port); err != nil || has {
		return
	}
	to := dest
	if to == "" {
		to = "the nodes"
	}
	ch := PlanChange{Name: "pve-api-rule", To: "ACCEPT tcp " + port + " from " + strings.Join(sources, ", ") + " to " + to + ", comment " + pve.APIAccessComment(in.ID),
		Class: ClassSite, Stage: StageSeedVM,
		Impact: "The run writes one rule into the cluster firewall of Proxmox: ACCEPT from the platform's addresses (the seed and the node range) to the port of the Proxmox API. " +
			"A rule with the same comment that differs is replaced by it. Nothing else in the firewall is touched, and no machine or pod restarts."}
	p.Changes = append(p.Changes, ch)
	for _, up := range []*Upgrade{p.Upgrade, p.Replacement} {
		if up != nil {
			up.Changes = append(up.Changes, ch)
		}
	}
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

package admin

// platform_site.go — what a save of a managed-services installation writes into its provider's network config, so that
// no operator has to: the installation's own addresses as blocked networks, and "dhcp": false on a default tenant zone
// that does not exist yet. Both were hand steps of the first production install (2026-11-17).

import (
	"context"
	"errors"
	"slices"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platformlife"
)

// siteChanges is what one save changed on the provider; the console shows it. Problem says what could not be done.
type siteChanges struct {
	BlockedCIDRs []string `json:"blockedCidrs,omitempty"` // added to config.network.blockedCidrs
	// BlockedCIDRsRemoved were added by an earlier save and are needed no longer: taken off blockedCidrs.
	BlockedCIDRsRemoved []string `json:"blockedCidrsRemoved,omitempty"`
	ZoneWithoutDHCP     string   `json:"zoneWithoutDhcp,omitempty"` // the zone whose config now says "dhcp": false
	Problem             string   `json:"problem,omitempty"`
}

// platformSiteDefaults brings the provider of in up to the installation; nil when there was nothing to change.
//
// Blocked networks: the seed's address, the API address and the node range, as the exact CIDRs no entry of
// blockedCidrs covers yet (Installation.Unfenced), never the MetalLB range, which tenants must reach. What a save
// added is recorded beside the list, in config.network.platformBlockedCidrs (one installation per provider, and one
// write for both, so the record cannot disagree with the list). The next save takes off the list exactly the recorded
// entries the settings no longer need (a mistyped prefix must not keep tenants from a LAN range for good) and never
// an entry it did not add. The next provider sync writes the cluster firewall DROPs; a narrower set it writes only on
// a provider with a project (pve.ReconcileProviderIsolation refuses otherwise, and the Check row isolation then says
// which rules to delete by hand). A Remove takes nothing off: the installation stays, its settings still name these
// addresses and the next install needs them fenced, and on a provider with no project the sync would refuse the
// narrower set on every cycle.
//
// Tenant zone: tenant Kubernetes clusters need networks without DHCP. When the default zone is a simple zone whose
// config says nothing about DHCP and Proxmox has no such zone yet, its config gets "dhcp": false, so the zone is
// created without (pve.EnsureZone). A zone that exists is never changed, and a config that says "dhcp": true stays.
// When the zones cannot be read nothing is written, and the Check row tenant-network says what is left to do.
func (h *Handler) platformSiteDefaults(ctx context.Context, in *platformlife.Installation, es *externalservice.ExternalService) *siteChanges {
	doc, err := h.repo.FindDoc(ctx, externalServiceCollection, in.ServiceID)
	if err != nil || doc == nil {
		return &siteChanges{Problem: "The provider could not be read, so its blocked networks were not filled: save the settings again."}
	}
	cfg := ensureConfig(doc)
	network := ensureMap(cfg, "network")
	n := es.PVENetworkConfig()
	out := &siteChanges{}
	recorded := false // the record of what the installation added changed, with or without the list

	if blocked, own, added, removed := fence(n.BlockedCIDRs, n.PlatformCIDRs, in.Unfenced); len(added)+len(removed) > 0 || !slices.Equal(own, n.PlatformCIDRs) {
		network["blockedCidrs"], network["platformBlockedCidrs"] = blocked, own
		if len(own) == 0 {
			delete(network, "platformBlockedCidrs")
		}
		out.BlockedCIDRs, out.BlockedCIDRsRemoved, recorded = added, removed, true
	}

	// Only a zone the config holds: nothing is added to a provider that names no zone.
	if z, ok := n.Zones[n.DefaultZone]; ok && z.Type() == "simple" && !z.NoDHCP && !hasKey(ensureMap(ensureMap(network, "zones"), n.DefaultZone), "dhcp") {
		raw := ensureMap(ensureMap(network, "zones"), n.DefaultZone)
		switch exists, err := h.zoneExists(ctx, es, n.DefaultZone); {
		case err != nil:
			out.Problem = "The SDN zones could not be read (" + err.Error() + "), so the default tenant zone " + n.DefaultZone +
				" was left as it is: when the zone does not exist yet, save the settings again, or set \"dhcp\": false on it under Cloud providers."
		case !exists:
			raw["dhcp"], out.ZoneWithoutDHCP = false, n.DefaultZone
		}
	}

	if !recorded && out.ZoneWithoutDHCP == "" {
		if out.Problem == "" {
			return nil
		}
		return out
	}
	if _, err := h.repo.SetFields(ctx, externalServiceCollection, in.ServiceID, pgdoc.M{"config": cfg}); err != nil {
		return &siteChanges{Problem: "The provider's network settings could not be saved (" + err.Error() + "): save the settings again."}
	}
	return out
}

// fence works out the provider's blocked networks for an installation: blocked is the list as stored, own the entries
// an earlier save added for the installation, unfenced what the installation still needs given a list (Installation.
// Unfenced). It answers the new list (the order kept), the new record, and what was added to and removed from the list.
// An entry of own that the operator took off the list by hand is not put back unless it is needed.
func fence(blocked, own []string, unfenced func([]string) []string) (next, record, added, removed []string) {
	others := slices.DeleteFunc(slices.Clone(blocked), func(c string) bool { return slices.Contains(own, c) })
	record = unfenced(others)
	for _, c := range blocked {
		if slices.Contains(others, c) || slices.Contains(record, c) {
			next = append(next, c)
		} else {
			removed = append(removed, c)
		}
	}
	for _, c := range record {
		if !slices.Contains(blocked, c) {
			next, added = append(next, c), append(added, c)
		}
	}
	return next, record, added, removed
}

func hasKey(m pgdoc.M, k string) bool { _, ok := m[k]; return ok }

// zoneExists reports whether Proxmox has the SDN zone.
func (h *Handler) zoneExists(ctx context.Context, es *externalservice.ExternalService, zone string) (bool, error) {
	if h.pveFor == nil {
		return false, errors.New("Proxmox access is not configured")
	}
	ps, err := h.pveFor(es)
	if err != nil {
		return false, err
	}
	zones, err := ps.C.ListSDNZones(ctx)
	if err != nil {
		return false, err
	}
	for _, z := range zones {
		if z.Zone == zone {
			return true, nil
		}
	}
	return false, nil
}

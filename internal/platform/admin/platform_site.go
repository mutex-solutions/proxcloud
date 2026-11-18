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
	BlockedCIDRs    []string `json:"blockedCidrs,omitempty"`    // added to config.network.blockedCidrs
	ZoneWithoutDHCP string   `json:"zoneWithoutDhcp,omitempty"` // the zone whose config now says "dhcp": false
	Problem         string   `json:"problem,omitempty"`
}

// platformSiteDefaults brings the provider of in up to the installation; nil when there was nothing to change.
//
// Blocked networks: the seed's address, the API address and the node range, as the exact CIDRs no entry of
// blockedCidrs covers yet (Installation.Unfenced), never the MetalLB range, which tenants must reach. Entries are only
// added: one a changed setting no longer needs stays, since nothing says who else relies on it. The next provider
// sync writes the cluster firewall DROPs for them.
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

	if add := in.Unfenced(n.BlockedCIDRs); len(add) > 0 {
		network["blockedCidrs"], out.BlockedCIDRs = append(slices.Clone(n.BlockedCIDRs), add...), add
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

	if len(out.BlockedCIDRs) == 0 && out.ZoneWithoutDHCP == "" {
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

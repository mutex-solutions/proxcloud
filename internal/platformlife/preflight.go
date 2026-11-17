package platformlife

// preflight.go — POST …/preflight and, later, a run's first stage (design §7, §8.1 to §8.5): read-only checks of the
// flags, the two platform tokens, the portal token's fence and mint grant, the stores, the image placement and the
// platform network against layer 0. Every row has P2's shape, {id, status, message}; a row that cannot be read fails.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
)

// Check statuses, as the Proxmox connection test's rows.
const (
	CheckOK   = "ok"
	CheckWarn = "warn"
	CheckFail = "fail"
)

// Check is one row of the preflight. The ids are part of the API (docs/api.md): flags, spec, network, isolation,
// tenant-network (a warning or ok, never a failure; absent when the provider names no zone), credentials,
// machines (a warning or ok, never a failure; absent while the installation has no VM), privileges:capi, privileges:csi, privileges:kubemint, mint-scope, pool-fence, token-scope, iso-store-visibility,
// boot-iso-grant, boot-iso, placement, images. A run's first stage fails preflight_failed with the failed rows as its items, or
// isolation_not_enforced when isolation is the only one.
type Check struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// Preflight is what one preflight reads.
type Preflight struct {
	Installation      *Installation
	Site              Site
	Allow             []string     // the provider's sandbox endpoints (es.PVESandboxEndpoints), part of the isolation rules
	ProviderImport    string       // the provider's importStorage, whose own ACL row needs the mint role too (pve.ManagedACLs)
	Portal            *pve.Service // the portal token
	ProviderDisabled  bool         // the provider's status is DISABLED: a provider sync skips it, so none writes its isolation rules
	CAPI, CSI         *pve.Client  // the platform tokens; nil when not stored
	TemplateUpdatesVM bool         // PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED
	GitHub            bool         // a GitHub integration is saved
	// Blocked reports a digest on the template update blocklist (templateupdates.Receipts.Blocked).
	Blocked func(ctx context.Context, digest string) (bool, error)
}

// providerDisabled is what the isolation row adds for a disabled provider: its messages send the operator to a
// provider sync, and the sync leaves a disabled provider out without a log line.
const providerDisabled = "the provider is disabled, and a provider sync skips a disabled provider; enable it under Cloud providers, then run a provider sync"

// SiteOf is the site of a Proxmox provider, without the live management networks.
func SiteOf(es *externalservice.ExternalService) Site {
	n := es.PVENetworkConfig()
	site := Site{CIDRPool: n.CIDRPool, Blocked: n.BlockedCIDRs, Sandbox: n.SandboxCIDRs}
	for _, p := range n.PublicNetworks {
		site.Public = append(site.Public, p.CIDR)
	}
	if z, ok := n.Zones[n.DefaultZone]; ok {
		site.DefaultZone, site.DefaultZoneKind, site.DefaultZoneNoDHCP = n.DefaultZone, z.Type(), z.NoDHCP
	}
	return site
}

// TokenClient is the provider's PVE connection with tok in place of the portal token (as PVECSIGatewayConfig does).
func TokenClient(es *externalservice.ExternalService, tok *TokenCredential) (*pve.Client, error) {
	if tok == nil {
		return nil, nil
	}
	cfg := es.PVEConfig()
	cfg.TokenID, cfg.Secret = tok.TokenID, tok.Secret
	cfg.SeedInventoryTokenID, cfg.SeedInventorySecret = "", ""
	return pve.New(cfg)
}

// OK reports whether no check failed.
func OK(checks []Check) bool {
	return !slices.ContainsFunc(checks, func(c Check) bool { return c.Status == CheckFail })
}

// Run runs every check it can and returns one row each.
func (p Preflight) Run(ctx context.Context) []Check {
	in := p.Installation
	s := in.Spec
	var out []Check
	row := func(id string, problems []string, passed string) {
		if len(problems) == 0 {
			out = append(out, Check{ID: id, Status: CheckOK, Message: passed})
			return
		}
		out = append(out, Check{ID: id, Status: CheckFail, Message: strings.Join(problems, "\n")})
	}
	readErr := func(what string, err error) []string { return []string{"Could not read " + what + ": " + err.Error()} }

	var flags []string
	if !p.TemplateUpdatesVM {
		flags = append(flags, "PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED is off: the platform images are imported through Template updates (proxcloud-up.sh install --managed-services).")
	}
	if !p.GitHub {
		flags = append(flags, "No GitHub integration is saved: Template updates finds the platform images through it.")
	}
	row("flags", flags, "Managed services and VM template updates are on, and a GitHub integration is saved.")

	var spec []string
	if err := in.Validate(); err != nil {
		spec = strings.Split(err.Error(), "\n")
	}
	row("spec", spec, "The installation is complete and consistent.")

	mgmt, complete, err := p.Portal.C.ManagementCIDRs(ctx)
	site := p.Site
	site.Mgmt = mgmt
	var unread []string
	switch {
	case err != nil:
		unread = readErr("the nodes' networks", err)
	case !complete:
		unread = []string{"A Proxmox node did not report its networks, so the platform network cannot be checked against them."}
	}
	if unread != nil {
		row("network", unread, "")
		row("isolation", unread, "")
	} else {
		row("network", in.SiteProblems(site), "The platform network is apart from every tenant, sandbox and public network, and tenants can reach the MetalLB range.")
		// Layer 0 drops tenant traffic to the management and blocked networks only.
		if unf := in.Unfenced(append(slices.Clone(site.Blocked), mgmt...)); len(unf) > 0 {
			row("isolation", []string{"Tenants could reach the seed, the API VIP or the node range: add " + strings.Join(unf, ", ") +
				" (or one network that covers them) to the provider's blockedCidrs, then run a provider sync so the cluster firewall drops them (with this installation saved it needs no project)."}, "")
		} else {
			switch err := p.Portal.CheckIsolation(ctx, site.CIDRPool, site.Blocked, site.Sandbox, p.Allow...); {
			case errors.Is(err, pve.ErrIsolationPending) && p.ProviderDisabled:
				row("isolation", nil, "The platform addresses are blocked networks. No cluster firewall rule is written yet: "+providerDisabled+".")
			case errors.Is(err, pve.ErrIsolationPending):
				row("isolation", nil, "The platform addresses are blocked networks; the cluster firewall rules are written by the next provider sync (with this installation saved it needs no project), or with the first tenant network.")
			case err != nil && p.ProviderDisabled:
				row("isolation", []string{"No provider sync writes or repairs these rules: " + providerDisabled + ". " + err.Error()}, "")
			case err != nil:
				row("isolation", []string{err.Error()}, "")
			default:
				row("isolation", nil, "The cluster firewall drops tenant traffic to the seed, the API VIP and the node range.")
			}
		}
	}

	// A warning, never a failure: the platform installs and databases run on any zone. But a tenant cluster's nodes get
	// fixed addresses, and a cluster create on a network whose zone has DHCP is refused. The create reads the live
	// zone (pve.VnetStaticPoolFor) and an existing zone is never changed (pve.EnsureZone), so this row reads it too;
	// a zone not made yet gets DHCP when its kind is simple and its config does not say dhcp: false (pve.ZoneDHCP).
	// Said here so the operator reads it before a tenant does.
	if site.DefaultZone != "" {
		const remedy = " A tenant Kubernetes cluster needs a network without DHCP, so a cluster create on a network of this zone is refused " +
			"(400, \"Worker nodes need a network without DHCP\") until the default zone is an EVPN or a VLAN zone, or a simple zone without DHCP " +
			"(config.network.zones and defaultZone, under Cloud providers; \"dhcp\": false on a simple zone that does not exist yet makes it without). A Kubernetes provider with shared workers (cluster.shared) uses no tenant network and is not affected. " +
			"The install and managed databases do not need it."
		zones, err := p.Portal.C.ListSDNZones(ctx)
		i := slices.IndexFunc(zones, func(z pve.SDNZone) bool { return z.Zone == site.DefaultZone })
		switch {
		case err != nil:
			out = append(out, Check{ID: "tenant-network", Status: CheckWarn, Message: "Could not read the SDN zones, so whether the provider's default zone " +
				site.DefaultZone + " has DHCP is not known: " + err.Error() + "." + remedy})
		case i >= 0 && zones[i].DHCP != "":
			out = append(out, Check{ID: "tenant-network", Status: CheckWarn, Message: "The provider's default zone " + site.DefaultZone + " has DHCP in Proxmox." + remedy})
		case i < 0 && site.DefaultZoneKind == "simple" && !site.DefaultZoneNoDHCP:
			out = append(out, Check{ID: "tenant-network", Status: CheckWarn, Message: "The provider's default zone " + site.DefaultZone +
				" is a simple zone that does not exist yet, and ProxCloud creates a simple zone with DHCP unless its config says \"dhcp\": false." + remedy})
		case i >= 0:
			row("tenant-network", nil, "The provider's default zone "+site.DefaultZone+" has no DHCP in Proxmox: a tenant cluster can use its networks.")
		default:
			row("tenant-network", nil, "The provider's default zone "+site.DefaultZone+" ("+site.DefaultZoneKind+") does not exist yet and is created without DHCP: a tenant cluster can use its networks.")
		}
	}

	var creds []string
	if p.CAPI == nil {
		creds = append(creds, "The token "+pve.CAPITokenID+" is not stored: paste its secret (PUT …/credentials).")
	}
	if p.CSI == nil {
		creds = append(creds, "The token "+pve.CSITokenID+" is not stored: paste its secret (PUT …/credentials).")
	}
	row("credentials", creds, "Both platform tokens are stored.")

	// A warning, never a failure, and absent before the first machine exists: after a node reboot Proxmox starts only
	// the VMs with onboot, and a stopped machine is started in Proxmox, not by a run.
	if p.CAPI != nil {
		vms, err := platformVMs(ctx, p.CAPI, in.ID)
		var bad []string
		for _, vm := range vms {
			if !vm.running {
				bad = append(bad, fmt.Sprintf("VM %d (%s) on %s is stopped: start it in Proxmox, or as root on %s: qm start %d", vm.vmid, vm.name, vm.node, vm.node, vm.vmid))
			}
			if !vm.onboot {
				bad = append(bad, fmt.Sprintf("VM %d (%s) on %s does not start when its node boots (onboot is off): the next Apply sets it, or as root on %s: qm set %d --onboot 1",
					vm.vmid, vm.name, vm.node, vm.node, vm.vmid))
			}
		}
		switch {
		case err != nil:
			out = append(out, Check{ID: "machines", Status: CheckWarn, Message: "Could not read the platform's VMs with " + pve.CAPITokenID + ", so whether they run is not known: " + err.Error()})
		case len(bad) > 0:
			out = append(out, Check{ID: "machines", Status: CheckWarn, Message: strings.Join(bad, "\n")})
		case len(vms) > 0:
			row("machines", nil, fmt.Sprintf("The platform's %d VMs run and start when their node boots.", len(vms)))
		}
	}

	privs := func(id string, c *pve.Client, who string, checks []pve.PrivCheck) {
		if c == nil {
			return
		}
		missing, err := c.MissingPrivileges(ctx, checks)
		if err != nil {
			row(id, readErr("the permissions of "+who, err), "")
			return
		}
		for i := range missing {
			missing[i] = who + " lacks " + missing[i] + " (run the managed-services script again: GET /api/v1/admin/setup/proxmox-script?managedServices=true&bootStorage=" + s.Storage.Boot + ", or step 2 of the Managed services page)"
		}
		row(id, missing, who+" holds what it needs.")
	}
	privs("privileges:capi", p.CAPI, pve.CAPITokenID, pve.CAPIPrivileges(s.Storage.Boot, s.Storage.ISO))
	privs("privileges:csi", p.CSI, pve.CSITokenID, pve.CSIPrivileges(s.Storage.Boot))
	privs("privileges:kubemint", p.Portal.C, "the portal token", pve.KubeMintPrivileges(p.ProviderImport))

	// The mint grant reaches the users of pve.KubeMintGroup alone. On /access/groups itself (where sites set up before
	// the group hold it) PVE lets the token create an API token for every user of the cluster, root@pam included.
	// And only them: a service user outside the group fails every cluster create and release.
	if held, err := p.Portal.C.HeldPrivileges(ctx, pve.KubeMintForbidden); err != nil {
		row("mint-scope", readErr("the permissions of the portal token", err), "")
	} else if len(held) > 0 {
		row("mint-scope", []string{"the portal token holds " + strings.Join(held, "; ") + ": " + pve.KubeMintNarrowing}, "")
	} else if outside, err := p.Portal.C.KubeMintGroupProblems(ctx); err != nil {
		row("mint-scope", readErr("the users the portal token creates tokens for", err), "")
	} else {
		row("mint-scope", outside, "The portal token may create tokens for the users of the group "+pve.KubeMintGroup+" alone, and both are in it.")
	}

	// The fence: the portal token holds nothing on either platform pool, the CSI token nothing on the seed's.
	var fence []string
	for _, f := range []struct {
		c    *pve.Client
		who  string
		pool string
	}{{p.Portal.C, "the portal token", pve.SystemPool}, {p.Portal.C, "the portal token", pve.SeedPool}, {p.CSI, pve.CSITokenID, pve.SeedPool}} {
		if f.c == nil {
			continue
		}
		have, err := f.c.Permissions(ctx, "/pool/"+f.pool)
		if err != nil {
			fence = append(fence, readErr("the permissions of "+f.who, err)...)
		} else if len(have) > 0 {
			fence = append(fence, fmt.Sprintf("%s holds %d privileges on /pool/%s: it must hold none there (NoAccess)", f.who, len(have), f.pool))
		}
	}
	row("pool-fence", fence, "Neither the portal token nor "+pve.CSITokenID+" reaches the seed's pool, and the portal token does not reach "+pve.SystemPool+".")

	// The rows behind a grant that must go, as far as the portal token may read them (an unreadable list names none).
	acls, _ := p.Portal.C.ListACL(ctx)

	// The platform tokens hold rows on the platform pools and on single stores. A grant on /pool, /storage or /vms
	// (an older setup wrote some, and the script removes no row) would hand whoever reads the token's Secret on the
	// platform cluster every tenant VM and every disk.
	var scope []string
	for _, tk := range []struct {
		c   *pve.Client
		who string
	}{{p.CAPI, pve.CAPITokenID}, {p.CSI, pve.CSITokenID}} {
		if tk.c == nil {
			continue
		}
		for _, path := range []string{"/pool", "/storage", "/vms"} {
			have, err := tk.c.Permissions(ctx, path)
			if err != nil {
				scope = append(scope, readErr("the permissions of "+tk.who, err)...)
			} else if len(have) > 0 {
				scope = append(scope, fmt.Sprintf("%s holds %d privileges on %s, which reaches every tenant's VMs or disks: it must hold none there. Remove the row that grants them, %s. Its rows on the platform pools and stores stay",
					tk.who, len(have), path, pve.TokenACLRemoval(acls, tk.who, path)))
			}
		}
	}
	if p.CAPI != nil || p.CSI != nil {
		row("token-scope", scope, "Neither platform token holds anything on /pool, /storage or /vms as a whole.")
	}

	defs, err := p.Portal.C.ListStorage(ctx)
	if p.CAPI != nil {
		var vis []string
		if err != nil {
			vis = readErr("the storages", err)
		} else if msg, seen, err := p.CAPI.ISOVisibilityProblem(ctx, defs, s.Storage.ISO); err != nil {
			vis = readErr("the ISO stores "+pve.CAPITokenID+" sees", err)
		} else if msg != "" {
			// A row is to remove only for a store the token sees and has no use for. Its rows on the ISO and the boot
			// store are the script's: privileges:capi needs them.
			var others []string
			for _, st := range seen {
				if st != s.Storage.ISO && st != s.Storage.Boot {
					others = append(others, "/storage/"+st)
				}
			}
			if len(others) > 0 {
				msg += ". To take the others from " + pve.CAPITokenID + ", " + pve.TokenACLRemoval(acls, pve.CAPITokenID, others...) +
					". Then check again: privileges:capi must stay ok"
			}
			if s.Storage.Boot != s.Storage.ISO && slices.Contains(seen, s.Storage.Boot) {
				msg += ". The boot store " + s.Storage.Boot + " takes ISO images and the token needs its row there: do not remove it, name another boot store or take iso out of its content (boot-iso)"
			}
			vis = []string{msg}
		}
		row("iso-store-visibility", vis, pve.CAPITokenID+" sees one ISO store, "+s.Storage.ISO+".")
	}
	var grant []string
	if err := p.Portal.C.CheckBootISOGrant(ctx, s.Storage.ISO); err != nil {
		grant = []string{err.Error()}
	}
	row("boot-iso-grant", grant, "capmox@pve may upload and delete bootstrap ISOs on "+s.Storage.ISO+".")

	caps, err := p.Portal.C.DetectStorage(ctx)
	if err != nil {
		row("boot-iso", readErr("the storages", err), "")
		row("placement", readErr("the storages", err), "")
	} else {
		var iso []string
		if msg := pve.BootISOProblem(caps, s.Storage.ISO, s.Storage.Boot); msg != "" {
			iso = []string{msg}
		}
		row("boot-iso", iso, s.Storage.ISO+" is node-local, takes ISOs, and is the only ISO store of the boot storage.")
		opts := templateupdates.PlacementOptions(caps, s.Storage.Import)
		var nodes, place []string
		if s.Seed.Mode == SeedVM {
			nodes = append(nodes, s.Seed.Node)
		}
		if s.NodeImage.Mode == ImageBOM {
			for _, c := range s.Clusters {
				nodes = append(nodes, c.ControlPlane.Node)
				for _, g := range c.NodeGroups {
					nodes = append(nodes, g.Nodes...)
				}
			}
		}
		for _, n := range slices.Compact(slices.Sorted(slices.Values(nodes))) {
			if !templateupdates.PlacementAllowed(opts, n, s.Storage.Import, s.Storage.Boot) {
				place = append(place, fmt.Sprintf("Node %s needs %s active with import content and %s with images content.", n, s.Storage.Import, s.Storage.Boot))
			}
		}
		row("placement", place, "Every node an image goes to has the import and the boot store.")
	}

	if p.Blocked != nil {
		var blocked []string
		if bom, err := Current(); err != nil {
			blocked = []string{err.Error()}
		} else {
			for _, im := range []struct {
				want bool
				what string
				d    string
			}{{s.Seed.Mode == SeedVM, "the seed image", bom.Seed.Image.Digest}, {s.NodeImage.Mode == ImageBOM, "the node image", bom.Kubernetes.NodeImage.Digest}} {
				if !im.want {
					continue
				}
				if b, err := p.Blocked(ctx, im.d); err != nil {
					blocked = append(blocked, readErr("the template update blocklist", err)...)
				} else if b {
					blocked = append(blocked, im.what+" "+im.d+" is on the blocklist: install a newer ProxCloud release")
				}
			}
		}
		row("images", blocked, "Neither BOM image is on the blocklist.")
	}
	return out
}

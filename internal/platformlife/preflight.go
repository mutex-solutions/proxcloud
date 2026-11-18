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

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
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

// Check is one row of the preflight. The ids are part of the API (docs/api.md): flags, spec, network, bridge (a failure before the
// first install, a warning after it), isolation,
// tenant-network and tenant-path (a warning or ok, never a failure; absent when the provider names no zone), csi-vmid (a warning or ok, never a failure), credentials,
// machines (a warning or ok, never a failure; absent while the installation has no VM), capacity (a failure before the first install, a warning after it or when it cannot be read; absent while the CAPI token is not stored), privileges:capi, privileges:csi, privileges:kubemint, mint-scope, pool-fence, token-scope, iso-store-visibility,
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
	MintStores        []string     // the stores a tenant cluster's token gets a row on (MintScopeOf)
	MintZones         []string     // the zones of the tenants' networks (MintScopeOf)
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
		site.DefaultZoneExitNodes = z.ExitNodes
	}
	return site
}

// MintScopeOf names what pve.KubeMintPrivileges reads on a Proxmox provider beside the parents: the store of every
// storage profile that can hold a boot disk, on every node (a tenant cluster boots from one of them, by the node its
// image is on: kamajiStorage), the ISO store, the import store, and every zone the provider names. Sorted, each once.
func MintScopeOf(es *externalservice.ExternalService) (stores, zones []string) {
	stores = []string{es.PVEBootISOStorage(), es.PVEImportStorage()}
	for _, p := range es.PVEStorageProfiles() {
		if p.BootDisk() {
			for _, s := range p.Storages {
				stores = append(stores, s)
			}
		}
	}
	for z := range es.PVENetworkConfig().Zones {
		zones = append(zones, z)
	}
	slices.Sort(stores)
	slices.Sort(zones)
	stores = slices.DeleteFunc(slices.Compact(stores), func(s string) bool { return s == "" })
	return stores, zones
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
	// What the record alone cannot say: whether the cluster chart of this installation's BOM tags a NIC.
	if bom, err := bomOf(in); err == nil && s.Network.VLAN > 0 && !kamaji.ChartHasVLAN(ClusterChartVersion(bom)) {
		spec = append(spec, fmt.Sprintf("spec.network.vlan %d: the platform's BOM (release %s) names cluster chart %s, which cannot tag a NIC; a VLAN tag needs chart %s or newer, so use a bridge or vnet that carries the VLAN untagged",
			s.Network.VLAN, bom.Release, ClusterChartVersion(bom), kamaji.VLANChartVersion))
	}
	if bom, err := bomOf(in); err == nil {
		for _, c := range s.Clusters {
			if n := c.ControlPlane.Count(); n > 1 && !kamaji.ChartHasControlPlaneReplicas(ClusterChartVersion(bom)) {
				spec = append(spec, fmt.Sprintf("spec.clusters: %d control plane machines: the platform's BOM (release %s) names cluster chart %s, which builds one; more need chart %s or newer, so keep 1 until the platform is on a release that has it",
					n, bom.Release, ClusterChartVersion(bom), kamaji.ControlPlaneReplicasChartVersion))
			}
		}
	}
	row("spec", spec, "The installation is complete and consistent.")

	mgmt, complete, err := p.Portal.C.ManagementCIDRs(ctx)
	site := p.Site
	site.Mgmt = mgmt
	if s.Network.Routed() && err == nil && complete {
		// A routed network is one the node itself routes: it must not be a network the node is already in.
		var all bool
		if site.NodeAddrs, all, err = p.Portal.C.NodeAddresses(ctx); err == nil {
			complete = all
		}
	}
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
		passed := "The platform network is apart from every tenant, sandbox and public network, and tenants can reach the MetalLB range."
		if s.Network.Routed() {
			passed += " It is routed by the node " + s.Seed.Node + ": everything outside that node (the ProxCloud host, your workstation) needs the route \"" +
				s.Network.Prefix + " via the address of " + s.Seed.Node + "\" to reach the seed and the API address. No check reads that route."
		}
		row("network", in.SiteProblems(site), passed)
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

	out = append(out, p.bridge(ctx))

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
		out = append(out, p.tenantPath(ctx, site, zones, err))
	}

	// A warning, never a failure: see pve.CSIVMIDCheck.
	if msg, free, _ := p.Portal.C.CSIVMIDCheck(ctx); free {
		row("csi-vmid", nil, msg)
	} else {
		out = append(out, Check{ID: "csi-vmid", Status: CheckWarn, Message: msg})
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
			if vm.err != nil {
				bad = append(bad, fmt.Sprintf("VM %d on %s could not be read with %s, so whether it runs and starts when its node boots is not known: %v", vm.vmid, vm.node, pve.CAPITokenID, vm.err))
				continue
			}
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

	if p.CAPI != nil {
		out = append(out, p.capacity(ctx))
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
	capi := pve.CAPIPrivileges(s.Storage.Boot, s.Storage.ISO)
	// The path Proxmox checks for a NIC of this network, not /sdn/zones alone: a row of the token's own below it
	// replaces what /sdn/zones passes down.
	if path := p.nicPath(ctx); path != "" {
		capi = append(capi, pve.PrivCheck{Path: path, Privs: []string{"SDN.Use"}})
	}
	privs("privileges:capi", p.CAPI, pve.CAPITokenID, capi)
	privs("privileges:csi", p.CSI, pve.CSITokenID, pve.CSIPrivileges(s.Storage.Boot))
	// The paths a mint writes, not their parents alone: a store or zone with a row of its own reads complete on the
	// parent and refuses the cluster token's row (production, 2026-11-17: every tenant cluster create failed on it).
	privs("privileges:kubemint", p.Portal.C, "the portal token", pve.KubeMintPrivileges(p.MintStores, p.MintZones))
	// Once for the row, when a line names a single store or zone: why the parent's grant does not reach it, and which
	// of these lines the script does not fix.
	if c := &out[len(out)-1]; c.ID == "privileges:kubemint" && c.Status == CheckFail && (strings.Contains(c.Message, " on /storage/") || strings.Contains(c.Message, " on /sdn/zones/")) {
		c.Message += "\n" + pve.KubeMintShadowed
	}

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
		if bom, err := bomOf(p.Installation); err != nil {
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
					blocked = append(blocked, im.what+" "+im.d+" is on the blocklist: "+blockedAdvice(p.Installation))
				}
			}
		}
		row("images", blocked, "Neither BOM image is on the blocklist.")
	}
	return out
}

// nicPath is the ACL path Proxmox checks SDN.Use on when a NIC is put on the platform network, where that is not
// /sdn/zones/<zone>/<bridge> of an existing bridge (which the row read before this field existed does not name): the
// vnet of a routed network, and the tag of a VLAN (/sdn/zones/localnetwork/<bridge>/<tag>, or the vnet's zone when
// the bridge is a vnet; a vnet list that cannot be read counts as none). "" otherwise.
func (p Preflight) nicPath(ctx context.Context) string {
	n := p.Installation.Spec.Network
	switch {
	case n.Routed():
		return "/sdn/zones/" + RoutedZone + "/" + n.Bridge
	case n.VLAN > 0:
		zone := "localnetwork"
		vnets, _ := p.Portal.C.ListSDNVnets(ctx)
		if i := slices.IndexFunc(vnets, func(v pve.SDNVnet) bool { return v.Vnet == n.Bridge }); i >= 0 {
			zone = vnets[i].Zone
		}
		return fmt.Sprintf("/sdn/zones/%s/%s/%d", zone, n.Bridge, n.VLAN)
	}
	return ""
}

// bridge is the bridge row. Mode bridge: every node a machine is placed on has the bridge (a Linux or OVS bridge, or
// an SDN vnet). Mode routed: the vnet's name is free, or the vnet is this installation's. Before the first install a
// miss fails; after it the network is fixed and the machines run on it, so a miss can only be a read that went wrong:
// a warning, so that no apply of an installed platform stops on it.
func (p Preflight) bridge(ctx context.Context) Check {
	in := p.Installation
	n := in.Spec.Network
	miss := func(msg string) Check {
		if in.Installed() {
			return Check{ID: "bridge", Status: CheckWarn, Message: msg}
		}
		return Check{ID: "bridge", Status: CheckFail, Message: msg}
	}
	ok := func(msg string) Check { return Check{ID: "bridge", Status: CheckOK, Message: msg} }
	if n.Routed() {
		vnets, err := p.Portal.C.ListSDNVnets(ctx)
		if err != nil {
			return miss("Could not read the SDN vnets: " + err.Error())
		}
		i := slices.IndexFunc(vnets, func(v pve.SDNVnet) bool { return v.Vnet == n.Bridge })
		switch {
		case i < 0:
			if taken, err := ifaceTaken(ctx, p.Portal.C, n.Bridge); err != nil {
				return miss("Could not read the nodes' interfaces, so whether the name " + n.Bridge + " is free is not known: " + err.Error())
			} else if taken != "" {
				return miss(taken)
			}
			return ok("No vnet " + n.Bridge + " exists yet: the install makes it in the simple zone " + RoutedZone + " on " + in.Spec.Seed.Node + ", with the subnet " + n.Prefix +
				", the gateway " + n.Gateway + " on the node and SNAT, and applies SDN.")
		case vnets[i].Alias != networkAlias(in.ID):
			return miss("Proxmox already has a vnet " + n.Bridge + " (zone " + vnets[i].Zone + ") that this installation did not make: name another vnet, or use mode bridge to install on it.")
		}
		return ok("The vnet " + n.Bridge + " of zone " + vnets[i].Zone + " is this installation's.")
	}
	nodes := []string{in.Spec.Seed.Node}
	for _, c := range in.Spec.Clusters {
		nodes = append(nodes, c.ControlPlane.Node)
		for _, g := range c.NodeGroups {
			nodes = append(nodes, g.Nodes...)
		}
	}
	nodes = slices.DeleteFunc(slices.Compact(slices.Sorted(slices.Values(nodes))), func(s string) bool { return s == "" })
	bridges, err := p.Portal.C.NodeBridges(ctx)
	if err != nil {
		return miss("Could not read the nodes' bridges: " + err.Error())
	}
	without := slices.DeleteFunc(slices.Clone(nodes), func(node string) bool {
		return slices.ContainsFunc(bridges, func(b pve.NodeBridge) bool { return b.Node == node && b.Iface == n.Bridge })
	})
	if len(without) > 0 {
		return miss("No bridge or vnet " + n.Bridge + " was found on " + strings.Join(without, ", ") +
			" (or the node did not report its networks): the settings place machines there. Correct the bridge or the nodes; an SDN vnet must be applied (Datacenter, SDN, Apply).")
	}
	msg := "The bridge " + n.Bridge + " exists on " + strings.Join(nodes, ", ") + "."
	if n.VLAN > 0 {
		msg += fmt.Sprintf(" Every NIC on it is tagged with VLAN %d: the site's switches and router must carry and route that VLAN, which no check reads.", n.VLAN)
	}
	return ok(msg)
}

// tenantPathGuide is where the tenant-path row sends the operator.
const tenantPathGuide = `docs/managed-services.md, "The path from tenants to the platform"`

// tenantPath is the tenant-path row: a warning or ok, never a failure, and no row when the installation names no
// platform machine. Tenants reach the platform network (the API address of their cluster, a managed database) from
// their own networks. When a Proxmox node translates that traffic (SNAT: a simple zone on the node of the tenant's VM,
// an EVPN zone on its exit nodes) and the translated packet enters the same node again through the platform bridge,
// the node's bridge connection tracking drops a TCP connection: a second flow with the reply tuple of the first
// (production acceptance of v0.8.0, 2026-11-18; stated from the kernel source, not reproduced on a lab). It does not
// happen when the node itself routes the platform network (it holds the gateway: the packet never leaves the node),
// when no node translates (a VLAN zone), or when the platform machines are on other nodes.
//
// The row reads the layout only: the default zone as Proxmox has it (as the provider's config names it when it does
// not exist yet), the nodes of the platform machines, and whether such a node holds the platform gateway on the
// platform bridge (a vnet of a simple zone with that gateway on a subnet, or the node's own interface). It opens no
// connection, so it cannot prove the path, and it does not see an address added on a node at runtime.
func (p Preflight) tenantPath(ctx context.Context, site Site, zones []pve.SDNZone, zonesErr error) Check {
	const limit = " This row reads the configuration only: it opens no connection, and it does not see an address or a rule added on a node by hand. " +
		"\"Test the tenant path\" opens one from a tenant network, once the platform is installed (POST …/tenant-path-probe)."
	n := p.Installation.Spec.Network
	ok := func(msg string) Check { return Check{ID: "tenant-path", Status: CheckOK, Message: msg + limit} }
	unknown := func(what string, err error) Check {
		return Check{ID: "tenant-path", Status: CheckWarn, Message: "Could not read " + what + ", so whether tenants can reach the platform network " + n.Prefix +
			" is not known: " + err.Error() + ". A node that translates tenant traffic (SNAT) and hosts a platform machine must route the platform network itself, or tenant Kubernetes workers never join and tenants cannot reach managed databases: " +
			tenantPathGuide + "."}
	}
	var plat []string
	for _, c := range p.Installation.Spec.Clusters {
		plat = append(plat, c.ControlPlane.Node)
		for _, g := range c.NodeGroups {
			plat = append(plat, g.Nodes...)
		}
	}
	plat = slices.DeleteFunc(slices.Compact(slices.Sorted(slices.Values(plat))), func(s string) bool { return s == "" })
	if zonesErr != nil {
		return unknown("the SDN zones", zonesErr)
	}
	nodeList := func(s string) []string {
		return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
	}
	zoneType := func(zone string) string {
		if i := slices.IndexFunc(zones, func(z pve.SDNZone) bool { return z.Zone == zone }); i >= 0 {
			return zones[i].Type
		}
		return ""
	}
	kind, exit := site.DefaultZoneKind, site.DefaultZoneExitNodes
	var only []string // simple: the nodes the zone is limited to; none = every node
	if i := slices.IndexFunc(zones, func(z pve.SDNZone) bool { return z.Zone == site.DefaultZone }); i >= 0 {
		kind, only, exit = zones[i].Type, nodeList(zones[i].Nodes), nodeList(zones[i].ExitNodes)
	}
	zone := "The provider's default zone " + site.DefaultZone
	var how string
	translates := func(string) bool { return true }
	switch {
	case kind == "simple":
		how = zone + " is a simple zone: Proxmox translates (SNAT) the traffic of a tenant network with outbound internet on the node its VM runs on"
		if len(only) > 0 {
			how += ", and the zone is limited to " + strings.Join(only, ", ")
			translates = func(node string) bool { return slices.Contains(only, node) }
		}
	case kind == "evpn" && len(exit) > 0:
		how = zone + " is an EVPN zone: its exit nodes (" + strings.Join(exit, ", ") + ") translate (SNAT) the traffic of a tenant network with outbound internet"
		translates = func(node string) bool { return slices.Contains(exit, node) }
	default:
		return ok(zone + " (" + kind + ") has no Proxmox node that translates tenant traffic: what routes tenant networks carries their traffic to the platform network " + n.Prefix + " unchanged.")
	}
	shared := slices.DeleteFunc(slices.Clone(plat), func(node string) bool { return !translates(node) })
	if len(shared) == 0 {
		return ok(how + ". The platform machines are on other nodes (" + strings.Join(plat, ", ") + "), so a translated packet does not enter the node that translated it.")
	}

	vnets, err := p.Portal.C.ListSDNVnets(ctx)
	if err != nil {
		return unknown("the SDN vnets", err)
	}
	// A routed network that the install has yet to make: the node will hold its gateway.
	if n.Routed() && !slices.ContainsFunc(vnets, func(v pve.SDNVnet) bool { return v.Vnet == n.Bridge }) {
		return ok("The platform network " + n.Prefix + " is of mode routed: the install makes it as a vnet whose gateway " + n.Gateway + " the node " +
			strings.Join(shared, ", ") + " holds, so tenant traffic to it never leaves the node. " + how + ".")
	}
	routed := "The platform network " + n.Prefix + " is routed by the node itself (it holds the gateway " + n.Gateway + " on " + n.Bridge + "), so tenant traffic to it never leaves the node. " + how + "."
	var bridge string // what the platform bridge is, when the node does not route it
	if i := slices.IndexFunc(vnets, func(v pve.SDNVnet) bool { return v.Vnet == n.Bridge }); i >= 0 {
		switch zt := zoneType(vnets[i].Zone); zt {
		case "simple":
			subs, err := p.Portal.C.ListSDNSubnets(ctx, n.Bridge)
			if err != nil {
				return unknown("the subnets of the vnet "+n.Bridge, err)
			}
			if slices.ContainsFunc(subs, func(s pve.SDNSubnet) bool { return s.Gateway == n.Gateway }) {
				return ok(routed)
			}
			bridge = "a vnet of the simple zone " + vnets[i].Zone + " with no subnet whose gateway is " + n.Gateway
		case "vlan":
			bridge = "a vnet of the VLAN zone " + vnets[i].Zone + ", on which Proxmox puts no address (a gateway the node holds on a VLAN interface of its own is not read)"
		default:
			bridge = "a vnet of the zone " + vnets[i].Zone + ", which this row cannot tell to be routed by the node"
		}
	} else {
		var without []string
		for _, node := range shared {
			addr, err := p.Portal.C.IfaceAddress(ctx, node, n.Bridge)
			if err != nil {
				return unknown("the networks of node "+node, err)
			}
			if addr != n.Gateway {
				without = append(without, node)
			}
		}
		if len(without) == 0 {
			return ok(routed)
		}
		shared = without
		bridge = "no vnet of a simple zone, and the node's saved network configuration does not hold the gateway " + n.Gateway + " on it"
	}
	nodes := strings.Join(shared, ", ")
	return Check{ID: "tenant-path", Status: CheckWarn, Message: "Tenants may not reach the platform network " + n.Prefix + " from " + nodes + ". " + how +
		". Platform machines are placed on " + nodes + " too, and this row finds nothing that makes the node itself route the platform network: its bridge " + n.Bridge + " is " + bridge +
		". A tenant's TCP connection to a platform address on the same node then leaves the node translated, comes back into it through " + n.Bridge +
		", and the node's connection tracking drops it. Tenant Kubernetes workers then never join their cluster (they stay \"not joined\") and tenants cannot reach managed databases. " +
		"The install and the platform's own machines are not affected.\n" +
		"Two layouts are supported. A new one-node site: make the platform network one the node routes (a vnet of a simple zone whose subnet has the platform gateway). " +
		"An existing site: give the node a leg on the platform bridge, by hand as root. Both, with what to check first, are in " + tenantPathGuide + "." + limit}
}
